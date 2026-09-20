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
// Tools returns a snapshot. The server subscribes to the SDK's
// tool-list-changed notification and refreshes it, so a loop that reads
// its tool list each turn should call Tools then rather than hold the
// slice; agentturn's Config.ToolProvider is that hook.
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

// Option configures [Connect].
type Option func(*options)

type options struct {
	prefix     string
	sequential map[string]bool
	client     sdk.Implementation
	clientOpts sdk.ClientOptions
}

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

// Server is one connected MCP session and the tools it offers.
type Server struct {
	session *sdk.ClientSession
	opts    options

	mu    sync.RWMutex
	tools []agenttool.Tool

	token    atomic.Int64
	progress sync.Map // progress token (string) -> func(agenttool.Result)
}

// Connect opens one session over t, lists its tools and returns the
// server. The caller closes it. The context bounds the connection and
// the initial listing only.
func Connect(ctx context.Context, t sdk.Transport, opts ...Option) (*Server, error) {
	o := options{client: sdk.Implementation{Name: "agenttool", Version: "0"}}
	for _, opt := range opts {
		opt(&o)
	}
	s := &Server{opts: o}

	clientOpts := o.clientOpts
	userToolsChanged := clientOpts.ToolListChangedHandler
	clientOpts.ToolListChangedHandler = func(ctx context.Context, req *sdk.ToolListChangedRequest) {
		if userToolsChanged != nil {
			userToolsChanged(ctx, req)
		}
		// The handler runs on the session's receive path; listing needs
		// a round trip, so refresh off it.
		go func() { _ = s.Refresh(context.Background()) }()
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
	if err := s.Refresh(ctx); err != nil {
		_ = session.Close()
		return nil, err
	}
	return s, nil
}

// Session returns the underlying SDK session, for resources, prompts and
// anything else this package does not map.
func (s *Server) Session() *sdk.ClientSession { return s.session }

// Tools returns a snapshot of the server's tools as of the last listing.
func (s *Server) Tools() []agenttool.Tool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]agenttool.Tool(nil), s.tools...)
}

// Refresh lists the server's tools again and replaces the snapshot. It
// runs automatically on the tool-list-changed notification.
func (s *Server) Refresh(ctx context.Context) error {
	var tools []agenttool.Tool
	for t, err := range s.session.Tools(ctx, nil) {
		if err != nil {
			return fmt.Errorf("mcp: list tools: %w", err)
		}
		rt, err := s.wrap(t)
		if err != nil {
			return err
		}
		tools = append(tools, rt)
	}
	s.mu.Lock()
	s.tools = tools
	s.mu.Unlock()
	return nil
}

// Close closes the session.
func (s *Server) Close() error {
	return s.session.Close()
}

// Name returns the local name of a remote tool under this server's
// prefix.
func (s *Server) Name(remote string) string {
	if s.opts.prefix == "" {
		return remote
	}
	return s.opts.prefix + "__" + remote
}

func (s *Server) wrap(t *sdk.Tool) (agenttool.Tool, error) {
	schema, err := json.Marshal(t.InputSchema)
	if err != nil {
		return nil, fmt.Errorf("mcp: tool %q: input schema: %w", t.Name, err)
	}
	if t.InputSchema == nil {
		schema = json.RawMessage(`{"type":"object"}`)
	}
	remote := t.Name
	name := s.Name(remote)
	return &agenttool.Func{
		ToolName:        name,
		ToolDescription: t.Description,
		Schema:          schema,
		RunAlone:        s.opts.sequential[remote] || s.opts.sequential[name],
		Fn: func(ctx context.Context, call agenttool.Call) (agenttool.Result, error) {
			return s.call(ctx, remote, call)
		},
	}, nil
}

// call invokes a remote tool and maps its result. When the call asked
// for updates a progress token is attached and notifications for it are
// routed to Call.OnUpdate with an agenttool.ProgressInfo as Details, so
// a tool served again by mcpserver forwards the same numbers. The SDK
// dispatches notifications on their own goroutines, so an update may be
// delivered concurrently with, or just after, the result; the batch
// executor in the tool package serialises them and drops anything after
// completion.
func (s *Server) call(ctx context.Context, remote string, call agenttool.Call) (agenttool.Result, error) {
	args := call.Args
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	params := &sdk.CallToolParams{Name: remote, Arguments: args}
	if call.OnUpdate != nil {
		token := "agenttool-" + strconv.FormatInt(s.token.Add(1), 10)
		params.Meta = sdk.Meta{"progressToken": token}
		s.progress.Store(token, call.OnUpdate)
		// Notifications are dispatched asynchronously and can trail the
		// result, so the token outlives the call briefly.
		defer time.AfterFunc(progressGrace, func() { s.progress.Delete(token) })
	}
	res, err := s.session.CallTool(ctx, params)
	if err != nil {
		return agenttool.Result{}, err
	}
	return Result(res)
}

// onProgress routes a progress notification to the call that asked for
// it.
func (s *Server) onProgress(p *sdk.ProgressNotificationParams) {
	if p == nil {
		return
	}
	token, ok := p.ProgressToken.(string)
	if !ok {
		return
	}
	fn, ok := s.progress.Load(token)
	if !ok {
		return
	}
	r := agenttool.Text(p.Message)
	r.Details = agenttool.ProgressInfo{Progress: p.Progress, Total: p.Total, Message: p.Message}
	fn.(func(agenttool.Result))(r)
}

// Result maps an MCP call result to a tool result. Text-only content
// becomes Output.Text; image, audio and resource content becomes
// Output.Parts with the matching openresponses content types; structured
// content is appended as JSON text. An isError result becomes an error
// whose message is the text content, so the loop produces the error
// output the model sees. Details carries the SDK result verbatim.
func Result(res *sdk.CallToolResult) (agenttool.Result, error) {
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
			parts = append(parts, &openresponses.InputFile{Filename: "audio", FileData: base64.StdEncoding.EncodeToString(v.Data)})
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

func dataURL(mime string, data []byte) string {
	if mime == "" {
		mime = "application/octet-stream"
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
}
