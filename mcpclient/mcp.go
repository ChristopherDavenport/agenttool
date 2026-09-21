// Package mcpclient wraps the tools of a remote MCP server as
// agenttool.Tool values: the consume side of MCP.
//
// The native contract is the centre and MCP is an edge: an MCP tool is a
// name, a description, an input schema and a call over a transport, which
// is a subset of agenttool.Tool, so this package is a mapping and nothing more.
// The official Go SDK owns transports and protocol negotiation. A harness
// that only writes Go tools never imports this package.
//
//	s, err := mcpclient.Connect(ctx, &sdk.CommandTransport{Command: cmd}, mcpclient.WithPrefix("fs"))
//	...
//	defer s.Close()
//	tools := s.Tools()
//
// A remote tool's annotations, the read-only and destructive hints MCP
// carries, reach the local tool through [agenttool.AnnotationsOf]; they
// are the server's word, so a policy may read them and must not trust
// them alone.
//
// Tools returns a snapshot. The remote subscribes to the server's
// tool-list-changed notification and refreshes it, so a loop that reads
// its tool list each turn should call Tools then rather than hold the
// slice. The refresh is a listing round trip, and until it lands the
// snapshot is the old list: [Remote.Await] blocks until every
// notification received so far is reflected, and a call that returns
// after a notification was received waits for that refresh before
// returning, so a tool that changes the tool list returns with the new
// list in place. A server need not notify before it answers, and the
// reference Go SDK does not, so a call whose list looks unchanged waits
// [DefaultNotificationGrace] for a notification before returning; see
// [WithNotificationGrace], which bounds or disables that wait. A
// notification that arrives after all of it is reflected after the next
// Await, and a consumer that must see a change now calls Refresh. A refresh that fails leaves the
// snapshot as it was and reports through [WithRefreshError], or the
// SDK logger when one is set; Await returns the same error until a
// refresh succeeds.
package mcpclient

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/openresponses"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// progressGrace is how long a call's progress token stays routable after
// the call returns.
const progressGrace = time.Second

// DefaultNotificationGrace is how long a call waits for a
// tool-list-changed notification the server has not sent yet; see
// [WithNotificationGrace]. The reference Go SDK arms the notification on
// a 10 ms timer after the change, so a tool that adds a tool returns
// before it is sent.
const DefaultNotificationGrace = 50 * time.Millisecond

// Option configures [Connect].
type Option func(*options)

type options struct {
	prefix         string
	sequential     map[string]bool
	resources      map[string]string
	grace          *time.Duration
	client         sdk.Implementation
	clientOpts     sdk.ClientOptions
	onRefreshError func(error)
}

// octetStream is the media type for bytes of unknown type.
const octetStream = "application/octet-stream"

// WithPrefix prefixes every tool name with prefix and a double
// underscore, so WithPrefix("fs") turns a remote "read" into "fs__read"
// and two servers with a "read" tool do not collide in one request.
func WithPrefix(prefix string) Option {
	return func(o *options) { o.prefix = prefix }
}

// WithSequential marks the named tools agenttool.Sequential. Names are
// matched before and after prefixing, so either form works. Remote tools
// are never sequential otherwise.
func WithSequential(names ...string) Option {
	return func(o *options) {
		if o.sequential == nil {
			o.sequential = make(map[string]bool, len(names))
		}
		for _, n := range names {
			o.sequential[n] = true
		}
	}
}

// WithResource names the shared state the named tools touch, so the
// executor runs two calls of them one after the other and the rest of
// the batch alongside; see [agenttool.Resource]. Names are matched
// before and after prefixing, so either form works, and several tools
// of one server named in one call share the state. It is what a remote
// shell or container session needs, since a remote tool names no
// resource of its own: MCP has no field for one.
//
//	mcpclient.WithResource("shell:session", "bash", "run_tests")
func WithResource(resource string, names ...string) Option {
	return func(o *options) {
		if o.resources == nil {
			o.resources = make(map[string]string, len(names))
		}
		for _, n := range names {
			o.resources[n] = resource
		}
	}
}

// WithNotificationGrace bounds how long a call waits, after its result
// has arrived, for a tool-list-changed notification the server may not
// have sent yet, before returning. The default is
// [DefaultNotificationGrace] and zero turns the wait off.
//
// A server does not have to notify before it answers, and the reference
// Go SDK does not: it arms the notification on a 10 ms timer, so a tool
// that adds a tool returns first and the notification follows, which
// left the new tool out of the list the loop read for its next turn. The
// grace is the cap and not the cost, since the wait ends as soon as the
// notification lands, but a call that changes nothing waits the whole
// window. It is skipped for a server that does not advertise
// tools.listChanged and for a tool the server annotated read-only,
// which cannot have changed the list without lying; a change missed
// that way is reflected at the next [Remote.Await] as before.
func WithNotificationGrace(d time.Duration) Option {
	return func(o *options) { o.grace = &d }
}

// WithClientInfo sets the implementation name and version the client
// announces during initialisation. The default is "agenttool".
func WithClientInfo(name, version string) Option {
	return func(o *options) { o.client = sdk.Implementation{Name: name, Version: version} }
}

// WithClientOptions sets SDK client options such as a logger. The tool
// list and progress notification handlers are installed by [Connect] and
// chained to any the caller set.
func WithClientOptions(opts sdk.ClientOptions) Option {
	return func(o *options) { o.clientOpts = opts }
}

// WithRefreshError sets the function called when the refresh that
// follows a tool-list-changed notification fails. The snapshot is left
// as it was. Without it the failure is logged through the SDK logger
// from [WithClientOptions] when one is set, and dropped otherwise.
func WithRefreshError(fn func(error)) Option {
	return func(o *options) { o.onRefreshError = fn }
}

// Remote is one connected MCP server: its session and the tools it
// offers. It is the consume-side handle; mcpserver.NewServer returns
// the SDK server for the serve side.
type Remote struct {
	session *sdk.ClientSession
	opts    options

	mu       sync.RWMutex
	tools    []agenttool.Tool
	progress map[string]func(agenttool.Result) // by progress token

	// grace bounds the post-call wait for a notification and
	// listChanged reports whether the server says it sends any. Both are
	// set before the remote is returned and read-only after.
	grace       time.Duration
	listChanged bool

	// notified is closed and replaced when a tool-list-changed
	// notification arrives, so a call can wait for one that has not come
	// yet. It is under mu.
	notified chan struct{}

	// seen counts tool-list-changed notifications as they arrive.
	// settled is the count the snapshot reflects and settledErr the
	// error of the refresh that settled it; settledCh is closed and
	// replaced whenever they change. All three are under mu. listing
	// serialises refreshes, so the count a refresh settles is the one it
	// read after acquiring it, which every earlier notification precedes.
	seen       atomic.Int64
	listing    sync.Mutex
	settled    int64
	settledErr error
	settledCh  chan struct{}

	token atomic.Int64
}

// Connect opens one session over t, lists its tools and returns the
// remote. The caller closes it. The context bounds the connection and
// the initial listing only.
func Connect(ctx context.Context, t sdk.Transport, opts ...Option) (*Remote, error) {
	o := options{client: sdk.Implementation{Name: "agenttool", Version: "0"}}
	for _, opt := range opts {
		opt(&o)
	}
	s := &Remote{opts: o, progress: make(map[string]func(agenttool.Result)), settledCh: make(chan struct{}), notified: make(chan struct{}), grace: DefaultNotificationGrace}
	if o.grace != nil {
		s.grace = *o.grace
	}

	clientOpts := o.clientOpts
	userToolsChanged := clientOpts.ToolListChangedHandler
	clientOpts.ToolListChangedHandler = func(ctx context.Context, req *sdk.ToolListChangedRequest) {
		if userToolsChanged != nil {
			userToolsChanged(ctx, req)
		}
		s.seen.Add(1)
		s.mu.Lock()
		close(s.notified)
		s.notified = make(chan struct{})
		s.mu.Unlock()
		// The handler runs on the session's receive path; listing needs
		// a round trip, so refresh off it.
		go func() {
			if err := s.refresh(context.Background(), false); err != nil {
				s.refreshFailed(err)
			}
		}()
	}
	userProgress := clientOpts.ProgressNotificationHandler
	clientOpts.ProgressNotificationHandler = func(ctx context.Context, req *sdk.ProgressNotificationClientRequest) {
		if userProgress != nil {
			userProgress(ctx, req)
		}
		s.onProgress(req.Params)
	}

	client := sdk.NewClient(&o.client, &clientOpts)
	session, err := client.Connect(ctx, t, nil)
	if err != nil {
		return nil, fmt.Errorf("mcp: connect: %w", err)
	}
	s.session = session
	if res := session.InitializeResult(); res != nil && res.Capabilities != nil && res.Capabilities.Tools != nil {
		s.listChanged = res.Capabilities.Tools.ListChanged
	}
	if err := s.Refresh(ctx); err != nil {
		_ = session.Close()
		return nil, err
	}
	return s, nil
}

// Session returns the underlying SDK session, for resources, prompts and
// anything else this package does not map.
func (s *Remote) Session() *sdk.ClientSession { return s.session }

// Tools returns a snapshot of the server's tools as of the last listing.
func (s *Remote) Tools() []agenttool.Tool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]agenttool.Tool(nil), s.tools...)
}

// refreshFailed reports a failed automatic refresh.
func (s *Remote) refreshFailed(err error) {
	switch {
	case s.opts.onRefreshError != nil:
		s.opts.onRefreshError(err)
	case s.opts.clientOpts.Logger != nil:
		s.opts.clientOpts.Logger.Error("mcpclient: refresh after tool-list-changed failed", "err", err)
	}
}

// Refresh lists the server's tools again and replaces the snapshot. It
// runs automatically on the tool-list-changed notification; see
// [WithRefreshError] for how a failure there is reported. Refreshes
// are serialised, so a Refresh that finds one in flight waits its turn.
func (s *Remote) Refresh(ctx context.Context) error {
	return s.refresh(ctx, true)
}

// refresh lists and replaces the snapshot, then marks the notifications
// received before the listing began as settled, with the listing's
// error. When force is false and a refresh that began after the last
// notification has already settled, there is nothing to do: several
// notifications in quick succession cost one listing.
func (s *Remote) refresh(ctx context.Context, force bool) error {
	s.listing.Lock()
	defer s.listing.Unlock()
	gen := s.seen.Load()
	if !force {
		s.mu.RLock()
		settled := s.settled
		s.mu.RUnlock()
		if settled >= gen {
			return nil
		}
	}
	tools, err := s.list(ctx)
	s.mu.Lock()
	if err == nil {
		s.tools = tools
	}
	s.settled = max(s.settled, gen)
	s.settledErr = err
	close(s.settledCh)
	s.settledCh = make(chan struct{})
	s.mu.Unlock()
	return err
}

func (s *Remote) list(ctx context.Context) ([]agenttool.Tool, error) {
	var tools []agenttool.Tool
	for t, err := range s.session.Tools(ctx, nil) {
		if err != nil {
			return nil, fmt.Errorf("mcp: list tools: %w", err)
		}
		rt, err := s.wrap(t)
		if err != nil {
			return nil, err
		}
		tools = append(tools, rt)
	}
	return tools, nil
}

// Await returns once the snapshot reflects every tool-list-changed
// notification received before the call, or when ctx ends. It returns
// nil when the refresh that settled them succeeded, the refresh's error
// when it failed, in which case the snapshot is the older list until a
// refresh succeeds, or ctx's error. It returns at once when no refresh
// is pending, so a loop can call it before each read of Tools.
func (s *Remote) Await(ctx context.Context) error {
	target := s.seen.Load()
	for {
		s.mu.RLock()
		settled, err, ch := s.settled, s.settledErr, s.settledCh
		s.mu.RUnlock()
		if settled >= target {
			return err
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Close closes the session.
func (s *Remote) Close() error {
	return s.session.Close()
}

// Name returns the local name of a remote tool under this server's
// prefix.
func (s *Remote) Name(remote string) string {
	if s.opts.prefix == "" {
		return remote
	}
	return s.opts.prefix + "__" + remote
}

func (s *Remote) wrap(t *sdk.Tool) (agenttool.Tool, error) {
	schema := json.RawMessage(`{"type":"object"}`)
	if t.InputSchema != nil {
		var err error
		if schema, err = json.Marshal(t.InputSchema); err != nil {
			return nil, fmt.Errorf("mcp: tool %q: input schema: %w", t.Name, err)
		}
	}
	remote := t.Name
	name := s.Name(remote)
	readOnly := false
	if a, ok := AnnotationsOf(t); ok {
		readOnly = a.ReadOnly
	}
	var opts []agenttool.Option
	if s.opts.sequential[remote] || s.opts.sequential[name] {
		opts = append(opts, agenttool.WithSequential())
	}
	if res, ok := s.opts.resources[remote]; ok {
		opts = append(opts, agenttool.WithResource(res))
	} else if res, ok := s.opts.resources[name]; ok {
		opts = append(opts, agenttool.WithResource(res))
	}
	if a, ok := AnnotationsOf(t); ok {
		opts = append(opts, agenttool.WithAnnotations(a))
	}
	return agenttool.NewFunc(name, t.Description, schema, func(ctx context.Context, call agenttool.Call) (agenttool.Result, error) {
		return s.call(ctx, remote, readOnly, call)
	}, opts...), nil
}

// AnnotationsOf maps a remote tool's annotations, reporting false when
// it carries none. The hints a server omits take MCP's defaults, so a
// tool that carries an annotations block without a destructive or
// open-world hint is destructive and open-world, and a tool with no
// block at all says nothing and is the zero [agenttool.Annotations].
// Title is the tool's own when it has one, since MCP prefers it for
// display, and the annotations' otherwise.
//
// The hints are the server's word and nothing more: a policy may use
// them to be stricter and must not use them alone to allow a call.
func AnnotationsOf(t *sdk.Tool) (agenttool.Annotations, bool) {
	if t == nil || t.Annotations == nil {
		return agenttool.Annotations{}, false
	}
	a := agenttool.Annotations{
		Title:       t.Annotations.Title,
		ReadOnly:    t.Annotations.ReadOnlyHint,
		Destructive: t.Annotations.DestructiveHint == nil || *t.Annotations.DestructiveHint,
		Idempotent:  t.Annotations.IdempotentHint,
		OpenWorld:   t.Annotations.OpenWorldHint == nil || *t.Annotations.OpenWorldHint,
	}
	if t.Title != "" {
		a.Title = t.Title
	}
	return a, true
}

// call invokes a remote tool and maps its result. When the call asked
// for updates a progress token is attached and notifications for it are
// routed to Call.OnUpdate with an agenttool.ProgressInfo as Details, so
// a tool served again by mcpserver forwards the same numbers. The SDK
// dispatches notifications on their own goroutines, so an update may be
// delivered concurrently with, or just after, the result; the batch
// executor in the tool package serialises them and drops anything after
// completion.
func (s *Remote) call(ctx context.Context, remote string, readOnly bool, call agenttool.Call) (agenttool.Result, error) {
	args := call.Args
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	params := &sdk.CallToolParams{Name: remote, Arguments: args}
	if call.OnUpdate != nil {
		token := "agenttool-" + strconv.FormatInt(s.token.Add(1), 10)
		params.Meta = sdk.Meta{"progressToken": token}
		s.mu.Lock()
		s.progress[token] = call.OnUpdate
		s.mu.Unlock()
		// Notifications are dispatched asynchronously and can trail the
		// result, so the token outlives the call briefly.
		defer time.AfterFunc(progressGrace, func() {
			s.mu.Lock()
			delete(s.progress, token)
			s.mu.Unlock()
		})
	}
	before := s.seen.Load()
	res, err := s.session.CallTool(ctx, params)
	if err != nil {
		return agenttool.Result{}, fmt.Errorf("mcp: call %q: %w", remote, err)
	}
	if s.seen.Load() == before && !readOnly {
		// The server may notify after it answers, and the reference
		// implementation does, so give the notification a moment to
		// arrive before deciding the list is unchanged.
		s.awaitNotification(ctx, before)
	}
	if s.seen.Load() != before {
		// The list changed while this call ran, most likely because of
		// it. Wait for the refresh so the caller's next Tools reads the
		// list the result belongs with. The result stands either way: a
		// refresh that fails is reported through refreshFailed, and one
		// that outlives ctx is reflected after the next Await.
		_ = s.Await(ctx)
	}
	return ResultOf(res)
}

// awaitNotification waits up to the grace for a tool-list-changed
// notification past target, and returns at once when the grace is off,
// when the server advertises no tool-list-changed notification, or when
// one has already arrived. It never fails a call: a notification that
// does not come in time is reflected at the next [Remote.Await].
func (s *Remote) awaitNotification(ctx context.Context, target int64) {
	if s.grace <= 0 || !s.listChanged {
		return
	}
	// The channel is taken before the counter is read, so a notification
	// that lands between the two closes the channel this waits on.
	s.mu.RLock()
	notified := s.notified
	s.mu.RUnlock()
	if s.seen.Load() > target {
		return
	}
	timer := time.NewTimer(s.grace)
	defer timer.Stop()
	select {
	case <-notified:
	case <-timer.C:
	case <-ctx.Done():
	}
}

// onProgress routes a progress notification to the call that asked for
// it.
func (s *Remote) onProgress(p *sdk.ProgressNotificationParams) {
	if p == nil {
		return
	}
	token, ok := p.ProgressToken.(string)
	if !ok {
		return
	}
	s.mu.RLock()
	fn, ok := s.progress[token]
	s.mu.RUnlock()
	if !ok {
		return
	}
	r := agenttool.Text(p.Message)
	r.Details = agenttool.ProgressInfo{Progress: p.Progress, Total: p.Total, Message: p.Message}
	fn(r)
}

// ResultOf maps an MCP call result to a tool result. Text-only content
// becomes Output.Text; image, audio and resource content becomes
// Output.Parts with the matching openresponses content types; structured
// content is appended as JSON text. An isError result becomes an error
// whose message is the text content, so the loop produces the error
// output the model sees. Details carries the SDK result verbatim.
func ResultOf(res *sdk.CallToolResult) (agenttool.Result, error) {
	if res == nil {
		return agenttool.Result{}, errors.New("mcp: empty result")
	}
	var parts openresponses.Contents
	var texts []string
	textOnly := true
	for _, c := range res.Content {
		switch v := c.(type) {
		case *sdk.TextContent:
			texts = append(texts, v.Text)
			parts = append(parts, &openresponses.Text{Text: v.Text})
		case *sdk.ImageContent:
			textOnly = false
			parts = append(parts, &openresponses.InputImage{ImageURL: dataURL(v.MIMEType, v.Data)})
		case *sdk.AudioContent:
			textOnly = false
			parts = append(parts, &openresponses.InputFile{Filename: audioName(v.MIMEType), FileData: base64.StdEncoding.EncodeToString(v.Data)})
		case *sdk.EmbeddedResource:
			if v.Resource == nil {
				continue
			}
			if len(v.Resource.Blob) > 0 {
				textOnly = false
				parts = append(parts, &openresponses.InputFile{Filename: v.Resource.URI, FileData: base64.StdEncoding.EncodeToString(v.Resource.Blob)})
				continue
			}
			texts = append(texts, v.Resource.Text)
			parts = append(parts, &openresponses.Text{Text: v.Resource.Text})
		case *sdk.ResourceLink:
			line := "resource: " + v.URI
			texts = append(texts, line)
			parts = append(parts, &openresponses.Text{Text: line})
		default:
			data, err := json.Marshal(c)
			if err != nil {
				continue
			}
			texts = append(texts, string(data))
			parts = append(parts, &openresponses.Text{Text: string(data)})
		}
	}
	if res.StructuredContent != nil {
		data, err := json.Marshal(res.StructuredContent)
		if err == nil {
			texts = append(texts, string(data))
			parts = append(parts, &openresponses.Text{Text: string(data)})
		}
	}
	text := strings.Join(texts, "\n")
	if res.IsError {
		if text == "" {
			text = "tool call failed"
		}
		return agenttool.Result{Details: res}, errors.New(text)
	}
	out := agenttool.Result{Details: res}
	if textOnly {
		out.Output.Text = text
	} else {
		out.Output.Parts = parts
	}
	return out, nil
}

// audioExt maps the audio media types MCP servers commonly send to a
// file extension. It is fixed rather than read from the host's MIME
// table so a part is named the same everywhere.
var audioExt = map[string]string{
	"audio/wav":   ".wav",
	"audio/x-wav": ".wav",
	"audio/wave":  ".wav",
	"audio/mpeg":  ".mp3",
	"audio/mp3":   ".mp3",
	"audio/mp4":   ".m4a",
	"audio/aac":   ".aac",
	"audio/ogg":   ".ogg",
	"audio/opus":  ".opus",
	"audio/flac":  ".flac",
	"audio/webm":  ".webm",
}

// audioName names an audio part after its media type, "audio.wav" for
// audio/wav, so the type survives in a part that has no field for it.
// An unknown or empty type gives "audio".
func audioName(mediaType string) string {
	base, _, _ := strings.Cut(mediaType, ";")
	return "audio" + audioExt[strings.ToLower(strings.TrimSpace(base))]
}

func dataURL(mediaType string, data []byte) string {
	if mediaType == "" {
		mediaType = octetStream
	}
	return "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(data)
}
