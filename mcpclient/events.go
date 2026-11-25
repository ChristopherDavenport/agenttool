package mcpclient

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// EventsExtension is the extension id under which a server that serves
// MCP Events (SEP-3415) says so, in capabilities.extensions. It is
// mcpserver.EventsExtension byte for byte, and a test holds the two
// together.
//
// Events are experimental: the extension is defined for protocol
// revision 2026-07-28 and later, and its error codes are provisional.
// This package takes the poll mode alone: it lists events, polls them
// and runs a poll loop. A subscription by stream or webhook is not
// made, and an event type that does not list poll is not polled.
const EventsExtension = "io.modelcontextprotocol/events"

// The methods of the extension this package sends. They are
// mcpserver's byte for byte.
const (
	EventsListMethod = "events/list"
	EventsPollMethod = "events/poll"
)

// The codes events errors carry; see [EventError]. They are mcpserver's
// byte for byte, and provisional in the SEP, so they are constants to
// change in one place.
const (
	EventsCodeInvalidParams     = -32602
	EventsCodeNotFound          = -32023
	EventsCodeForbidden         = -32024
	EventsCodeResourceExhausted = -32025
	EventsCodeUnsupported       = -32026
	EventsCodeCallbackEndpoint  = -32027
	EventsCodeHeaderMismatch    = -32020
)

// ErrEventsNotOffered is returned for an events call to a server that
// did not advertise [EventsExtension]: the SEP forbids sending it
// events methods, and it would answer method not found.
var ErrEventsNotOffered = errors.New("mcp: the server does not offer the events extension")

// EventError is an error a server answered an events request with. A
// caller branches on Code: [EventsCodeNotFound] and
// [EventsCodeUnsupported] say the event or its schema is gone, so list
// again and subscribe anew, and [EventsCodeForbidden] says stop.
type EventError struct {
	Code    int
	Message string
	// Data is the error's data, or nil.
	Data json.RawMessage
}

func (e *EventError) Error() string {
	if len(e.Data) > 0 {
		return fmt.Sprintf("mcp: events: %s (%d) %s", e.Message, e.Code, e.Data)
	}
	return fmt.Sprintf("mcp: events: %s (%d)", e.Message, e.Code)
}

// eventErr maps an SDK error to an [EventError] when it is a JSON-RPC
// error, and returns it as it is otherwise.
func eventErr(err error) error {
	var we *jsonrpc.Error
	if errors.As(err, &we) {
		return &EventError{Code: int(we.Code), Message: we.Message, Data: we.Data}
	}
	return err
}

// WithEvents has [Connect] take MCP Events: the extension is offered in
// the client's capabilities and events/list and events/poll are
// registered, so [Remote.ListEvents], [Remote.PollEvents] and
// [Remote.RunEvents] work. Without it they return an error. A server
// that does not advertise the extension is not sent events requests.
//
// Over Streamable HTTP the extension asks a client to send an Mcp-Name
// header, and an Mcp-Param-* header for each argument an event type's
// input schema marks with x-mcp-header, with every events request. The
// SDK cannot be given per-request headers, so the HTTP client of the
// transport needs [EventHeaders] as its round tripper:
//
//	hc := &http.Client{Transport: mcpclient.EventHeaders(nil)}
//	t := &sdk.StreamableClientTransport{Endpoint: url, HTTPClient: hc}
//
// A server that checks them refuses a request without them.
func WithEvents() Option {
	return func(o *options) {
		o.events = true
		var caps sdk.ClientCapabilities
		if o.clientOpts.Capabilities != nil {
			caps = *o.clientOpts.Capabilities
		}
		exts := make(map[string]any, len(caps.Extensions)+1)
		for k, v := range caps.Extensions {
			exts[k] = v
		}
		caps.Extensions = exts
		caps.AddExtension(EventsExtension, nil)
		o.clientOpts.Capabilities = &caps
		o.setups = append(o.setups, func(c *sdk.Client) error {
			if err := sdk.AddSendingCustomMethod[*listEventsParams, *listEventsResult](c, EventsListMethod); err != nil {
				return err
			}
			return sdk.AddSendingCustomMethod[*pollEventsParams, *pollEventsResult](c, EventsPollMethod)
		})
	}
}

// EventDescriptor is an event type a server lists.
type EventDescriptor struct {
	Name        string
	Description string
	// Delivery is the modes the server serves it by: "poll", "push",
	// "webhook". Only "poll" is taken here.
	Delivery []string
	// InputSchema is the JSON Schema of the arguments a subscription
	// takes, and PayloadSchema that of an event's data.
	InputSchema   json.RawMessage
	PayloadSchema json.RawMessage
	Meta          map[string]any
}

// Polls says whether the type is served by poll.
func (d EventDescriptor) Polls() bool {
	for _, m := range d.Delivery {
		if m == "poll" {
			return true
		}
	}
	return false
}

// EventOccurrence is one event as a poll returns it.
type EventOccurrence struct {
	// EventID is the event's stable id, which the poll loop
	// deduplicates on.
	EventID string
	Name    string
	// Timestamp is when the event occurred; zero when the server's
	// was not a time.
	Timestamp time.Time
	Data      json.RawMessage
	Meta      map[string]any
}

// PollParams are what one poll asks for.
type PollParams struct {
	// Name is the event type, and Arguments the subscription's
	// arguments, a JSON object conforming to its input schema, or nil.
	Name      string
	Arguments json.RawMessage
	// Cursor is where the last poll left off; nil starts from now.
	Cursor *string
	// MaxAge, when positive, has a replay start no earlier than that
	// long ago; a cursor older than it reads as a gap.
	MaxAge time.Duration
	// MaxEvents, when positive, limits the batch; more are left for the
	// next poll, with HasMore.
	MaxEvents int
}

// PollResult is what one poll returned.
type PollResult struct {
	Events []EventOccurrence
	// Cursor is where to poll from next, nil for a type that does not
	// replay, which is then polled without one.
	Cursor *string
	// Truncated says the poll started later than the cursor, so events
	// between were lost; the Cursor is fresh and to be kept.
	Truncated bool
	// HasMore says more are ready: poll again at once.
	HasMore bool
	// NextPoll is how long the server asks the client to wait.
	NextPoll time.Duration
}

type listEventsParams struct {
	sdk.ParamsBase
	Cursor string `json:"cursor,omitempty"`
}

type wireDescriptor struct {
	Name          string          `json:"name"`
	Description   string          `json:"description"`
	Delivery      []string        `json:"delivery"`
	InputSchema   json.RawMessage `json:"inputSchema"`
	PayloadSchema json.RawMessage `json:"payloadSchema"`
	Meta          map[string]any  `json:"_meta"`
}

type listEventsResult struct {
	sdk.ResultBase
	Events     []wireDescriptor `json:"events"`
	NextCursor string           `json:"nextCursor"`
}

type pollEventsParams struct {
	sdk.ParamsBase
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Cursor    *string         `json:"cursor"`
	MaxAgeMs  *int64          `json:"maxAgeMs,omitempty"`
	MaxEvents *int            `json:"maxEvents,omitempty"`
}

type wireOccurrence struct {
	EventID   string          `json:"eventId"`
	Name      string          `json:"name"`
	Timestamp string          `json:"timestamp"`
	Data      json.RawMessage `json:"data"`
	Meta      map[string]any  `json:"_meta"`
}

type pollEventsResult struct {
	sdk.ResultBase
	Events     []wireOccurrence `json:"events"`
	Cursor     *string          `json:"cursor"`
	Truncated  bool             `json:"truncated"`
	HasMore    bool             `json:"hasMore"`
	NextPollMs int64            `json:"nextPollMs"`
}

// eventsOffered says whether the session's server advertised the
// extension.
func eventsOffered(s *sdk.ClientSession) bool {
	res := s.InitializeResult()
	if res == nil || res.Capabilities == nil {
		return false
	}
	_, ok := res.Capabilities.Extensions[EventsExtension]
	return ok
}

// EventsOffered says whether the server advertised MCP Events, so a host
// knows whether to list its events.
func (s *Remote) EventsOffered() bool { return eventsOffered(s.Session()) }

func (s *Remote) eventsSession() (*sdk.ClientSession, error) {
	if !s.opts.events {
		return nil, errors.New("mcp: events are not taken: connect with WithEvents")
	}
	cs := s.Session()
	if !eventsOffered(cs) {
		return nil, ErrEventsNotOffered
	}
	return cs, nil
}

// eventSchemas is what the remote knows of the servers' event types, for
// the headers of a poll.
type eventSchemas struct {
	mu    sync.Mutex
	known map[string]EventDescriptor
}

func (e *eventSchemas) get(name string) (EventDescriptor, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	d, ok := e.known[name]
	return d, ok
}

func (e *eventSchemas) set(ds []EventDescriptor) {
	m := make(map[string]EventDescriptor, len(ds))
	for _, d := range ds {
		m[d.Name] = d
	}
	e.mu.Lock()
	e.known = m
	e.mu.Unlock()
}

// ListEvents lists the event types the server serves, all pages. An
// event type whose x-mcp-header annotations break the transport's rules
// is left out, as the SEP says, since its requests could not be sent.
// The listing is kept for the headers of later polls.
func (s *Remote) ListEvents(ctx context.Context) ([]EventDescriptor, error) {
	cs, err := s.eventsSession()
	if err != nil {
		return nil, err
	}
	var out []EventDescriptor
	cursor := ""
	for page := 0; page < 1000; page++ {
		res, err := sdk.CallCustomMethod[*listEventsParams, *listEventsResult](ctx, cs, EventsListMethod, &listEventsParams{Cursor: cursor})
		if err != nil {
			return nil, eventErr(err)
		}
		for _, d := range res.Events {
			desc := EventDescriptor(d)
			if _, err := paramBindings(desc.InputSchema); err != nil {
				continue
			}
			out = append(out, desc)
		}
		if res.NextCursor == "" {
			s.schemas.set(out)
			return out, nil
		}
		cursor = res.NextCursor
	}
	return nil, errors.New("mcp: events: the server's event list does not end")
}

// PollEvents polls one event type once. A server that refuses the
// request for its headers is polled again once after the event types
// are listed again, as the SEP says.
func (s *Remote) PollEvents(ctx context.Context, p PollParams) (*PollResult, error) {
	cs, err := s.eventsSession()
	if err != nil {
		return nil, err
	}
	if _, ok := s.schemas.get(p.Name); !ok {
		if _, err := s.ListEvents(ctx); err != nil {
			return nil, err
		}
	}
	res, err := s.pollOnce(ctx, cs, p)
	var ee *EventError
	if errors.As(err, &ee) && ee.Code == EventsCodeHeaderMismatch {
		if _, lerr := s.ListEvents(ctx); lerr == nil {
			res, err = s.pollOnce(ctx, cs, p)
		}
	}
	return res, err
}

func (s *Remote) pollOnce(ctx context.Context, cs *sdk.ClientSession, p PollParams) (*PollResult, error) {
	wire := &pollEventsParams{Name: p.Name, Arguments: p.Arguments, Cursor: p.Cursor}
	if p.MaxAge > 0 {
		ms := p.MaxAge.Milliseconds()
		wire.MaxAgeMs = &ms
	}
	if p.MaxEvents > 0 {
		wire.MaxEvents = &p.MaxEvents
	}
	d, _ := s.schemas.get(p.Name)
	ctx = withEventHeaders(ctx, p.Name, d.InputSchema, p.Arguments)
	res, err := sdk.CallCustomMethod[*pollEventsParams, *pollEventsResult](ctx, cs, EventsPollMethod, wire)
	if err != nil {
		return nil, eventErr(err)
	}
	out := &PollResult{Cursor: res.Cursor, Truncated: res.Truncated, HasMore: res.HasMore, NextPoll: time.Duration(res.NextPollMs) * time.Millisecond}
	for _, w := range res.Events {
		ts, _ := time.Parse(time.RFC3339Nano, w.Timestamp)
		out.Events = append(out.Events, EventOccurrence{EventID: w.EventID, Name: w.Name, Timestamp: ts, Data: w.Data, Meta: w.Meta})
	}
	return out, nil
}

// CursorStore keeps the cursor of each subscription, so a loop that
// restarts resumes where it left off. The key is [EventKey]. A cursor
// the server no longer holds reads as a gap and is replaced, so a stale
// store costs events, never correctness.
type CursorStore interface {
	// Load returns the saved cursor, nil when there is none.
	Load(ctx context.Context, key string) (*string, error)
	// Save keeps the cursor, which is never nil.
	Save(ctx context.Context, key string, cursor string) error
}

type memoryCursors struct {
	mu sync.Mutex
	m  map[string]string
}

// NewMemoryCursors is a [CursorStore] that keeps cursors in memory, for
// the life of the process.
func NewMemoryCursors() CursorStore { return &memoryCursors{m: map[string]string{}} }

func (c *memoryCursors) Load(_ context.Context, key string) (*string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if v, ok := c.m[key]; ok {
		return &v, nil
	}
	return nil, nil
}

func (c *memoryCursors) Save(_ context.Context, key, cursor string) error {
	c.mu.Lock()
	c.m[key] = cursor
	c.mu.Unlock()
	return nil
}

// EventKey is the key a subscription's cursor is kept under: its event
// type and its arguments, however spelt.
func EventKey(name string, args json.RawMessage) string {
	if len(args) == 0 || string(args) == "null" {
		args = json.RawMessage("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return name + "\x00" + string(args)
	}
	c, _ := json.Marshal(v)
	return name + "\x00" + string(c)
}

// EventLoop is one subscription to poll, with how to poll it. The zero
// value of each field but Name is usable.
type EventLoop struct {
	// Name and Arguments are the subscription.
	Name      string
	Arguments json.RawMessage
	// Cursors keeps the cursor between polls and runs; the default is
	// in memory for this loop alone.
	Cursors CursorStore
	// Floor is the least the loop waits between polls that the server did
	// not say have more; the server's nextPollMs is a recommendation, and
	// a loop may poll slower but not faster than this. The default is 1
	// second.
	Floor time.Duration
	// MaxAge and MaxEvents are sent with each poll; see [PollParams].
	MaxAge    time.Duration
	MaxEvents int
	// OnGap is called when a poll says it started later than the
	// cursor, so events may have been lost, before the batch it came
	// with is handled. A host that needs them re-reads the state
	// through a tool. It is not called for a type that does not
	// replay.
	OnGap func(ctx context.Context)
	// Seen bounds how many event ids are remembered for deduplication;
	// the default is 4096.
	Seen int
	// Sleep waits d or until ctx ends, and is the loop's clock; the
	// default is a timer. A test sets it to run time.
	Sleep func(ctx context.Context, d time.Duration) error
}

const (
	defaultEventFloor = time.Second
	defaultEventSeen  = 4096
)

func sleepTimer(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// RunEvents polls the loop's subscription until ctx ends or something
// fails, and calls handle with each event, once: an event id seen
// before, which a poll after a gap or a second route can repeat, is
// skipped. The cursor is saved after each batch has been handled, so a
// handle that returns an error stops the loop with the batch's cursor
// unsaved and its events delivered again when the loop runs again, which
// is at-least-once. A batch with HasMore is followed by a poll at once;
// otherwise the loop waits the server's nextPollMs, at least the floor.
//
// It returns ctx's error when ctx ends, and otherwise the error that
// stopped it: an [EventError] a caller branches on, or what handle
// returned. It does not retry; a host that wants that runs it again,
// which resumes from the saved cursor.
func (s *Remote) RunEvents(ctx context.Context, l EventLoop, handle func(context.Context, EventOccurrence) error) error {
	if _, err := s.eventsSession(); err != nil {
		return err
	}
	if l.Cursors == nil {
		l.Cursors = NewMemoryCursors()
	}
	floor := l.Floor
	if floor <= 0 {
		floor = defaultEventFloor
	}
	sleep := l.Sleep
	if sleep == nil {
		sleep = sleepTimer
	}
	seen := newSeenSet(l.Seen)
	key := EventKey(l.Name, l.Arguments)
	cursor, err := l.Cursors.Load(ctx, key)
	if err != nil {
		return fmt.Errorf("mcp: events: loading the cursor: %w", err)
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		res, err := s.PollEvents(ctx, PollParams{Name: l.Name, Arguments: l.Arguments, Cursor: cursor, MaxAge: l.MaxAge, MaxEvents: l.MaxEvents})
		if err != nil {
			return err
		}
		if res.Truncated && res.Cursor != nil && l.OnGap != nil {
			l.OnGap(ctx)
		}
		for _, ev := range res.Events {
			if !seen.add(ev.EventID) {
				continue
			}
			if err := handle(ctx, ev); err != nil {
				return err
			}
		}
		// A type that does not replay answers a null cursor, and is
		// polled with none.
		cursor = res.Cursor
		if cursor != nil {
			if err := l.Cursors.Save(ctx, key, *cursor); err != nil {
				return fmt.Errorf("mcp: events: saving the cursor: %w", err)
			}
		}
		if res.HasMore {
			continue
		}
		if err := sleep(ctx, max(res.NextPoll, floor)); err != nil {
			return err
		}
	}
}

// seenSet remembers the last n event ids.
type seenSet struct {
	n     int
	ids   map[string]struct{}
	order []string
}

func newSeenSet(n int) *seenSet {
	if n <= 0 {
		n = defaultEventSeen
	}
	return &seenSet{n: n, ids: map[string]struct{}{}}
}

// add notes id and says whether it is new.
func (s *seenSet) add(id string) bool {
	if _, ok := s.ids[id]; ok {
		return false
	}
	s.ids[id] = struct{}{}
	s.order = append(s.order, id)
	if len(s.order) > s.n {
		delete(s.ids, s.order[0])
		s.order = s.order[1:]
	}
	return true
}

// Headers of an events request over Streamable HTTP.

type eventHeadersKey struct{}

// withEventHeaders puts on ctx the headers [EventHeaders] sends: the
// event name and the argument headers the input schema marks.
func withEventHeaders(ctx context.Context, name string, input, args json.RawMessage) context.Context {
	h := http.Header{}
	h.Set("Mcp-Name", encodeHeaderValue(name))
	if binds, err := paramBindings(input); err == nil && len(binds) > 0 {
		var m map[string]json.RawMessage
		_ = json.Unmarshal(args, &m)
		for _, b := range binds {
			raw, ok := lookupPath(m, b.path)
			if !ok || string(raw) == "null" {
				continue
			}
			if v, ok := headerText(raw); ok {
				h.Set("Mcp-Param-"+b.header, encodeHeaderValue(v))
			}
		}
	}
	return context.WithValue(ctx, eventHeadersKey{}, h)
}

// EventHeaders is an [http.RoundTripper] that adds to an events request
// the Mcp-Name and Mcp-Param-* headers the extension asks of a client
// over Streamable HTTP, which [Remote.PollEvents] puts on the request's
// context, and passes any other request on unchanged. next is the
// transport it wraps, [http.DefaultTransport] when nil.
func EventHeaders(next http.RoundTripper) http.RoundTripper {
	if next == nil {
		next = http.DefaultTransport
	}
	return eventHeaderTripper{next}
}

type eventHeaderTripper struct{ next http.RoundTripper }

func (t eventHeaderTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	h, _ := req.Context().Value(eventHeadersKey{}).(http.Header)
	if len(h) == 0 {
		return t.next.RoundTrip(req)
	}
	req = req.Clone(req.Context())
	for k, vs := range h {
		req.Header[k] = vs
	}
	return t.next.RoundTrip(req)
}

type paramBinding struct {
	path   []string
	header string
}

type schemaProp struct {
	Type       json.RawMessage       `json:"type"`
	Header     json.RawMessage       `json:"x-mcp-header"`
	Properties map[string]schemaProp `json:"properties"`
}

// paramBindings finds the x-mcp-header annotations of an input schema
// and fails when one breaks the transport's rules: a name of HTTP token
// characters, once, case aside, on a string, integer or boolean.
func paramBindings(schema json.RawMessage) ([]paramBinding, error) {
	if len(schema) == 0 {
		return nil, nil
	}
	var root schemaProp
	if err := json.Unmarshal(schema, &root); err != nil {
		return nil, err
	}
	var out []paramBinding
	seen := map[string]bool{}
	var walk func(map[string]schemaProp, []string) error
	walk = func(props map[string]schemaProp, prefix []string) error {
		names := make([]string, 0, len(props))
		for n := range props {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			p := props[n]
			path := append(append([]string(nil), prefix...), n)
			if len(p.Header) > 0 {
				var h string
				if json.Unmarshal(p.Header, &h) != nil || h == "" {
					return fmt.Errorf("x-mcp-header of %q is not a name", strings.Join(path, "."))
				}
				for _, c := range h {
					if !isTokenChar(c) {
						return fmt.Errorf("x-mcp-header %q has the character %q", h, c)
					}
				}
				var typ string
				_ = json.Unmarshal(p.Type, &typ)
				if typ != "string" && typ != "integer" && typ != "boolean" {
					return fmt.Errorf("x-mcp-header %q is on a %q", h, typ)
				}
				if seen[strings.ToLower(h)] {
					return fmt.Errorf("x-mcp-header %q is used twice", h)
				}
				seen[strings.ToLower(h)] = true
				out = append(out, paramBinding{path: path, header: h})
			}
			if err := walk(p.Properties, path); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(root.Properties, nil); err != nil {
		return nil, err
	}
	return out, nil
}

func isTokenChar(c rune) bool {
	switch {
	case c >= '0' && c <= '9', c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z':
		return true
	}
	return strings.ContainsRune("!#$%&'*+-.^_`|~", c)
}

func lookupPath(args map[string]json.RawMessage, path []string) (json.RawMessage, bool) {
	cur, ok := args[path[0]]
	if !ok {
		return nil, false
	}
	for _, part := range path[1:] {
		var obj map[string]json.RawMessage
		if json.Unmarshal(cur, &obj) != nil {
			return nil, false
		}
		if cur, ok = obj[part]; !ok {
			return nil, false
		}
	}
	return cur, true
}

// headerText is a JSON primitive as header text; false for one that is
// not a string, boolean or safe integer.
func headerText(raw json.RawMessage) (string, bool) {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return "", false
	}
	switch x := v.(type) {
	case string:
		return x, true
	case bool:
		return strconv.FormatBool(x), true
	case float64:
		if x != math.Trunc(x) || math.Abs(x) > 1<<53-1 {
			return "", false
		}
		return strconv.FormatInt(int64(x), 10), true
	}
	return "", false
}

// encodeHeaderValue is v as a header value: as it is when plain ASCII
// without edge whitespace, and Base64 between the transport's sentinels
// otherwise.
func encodeHeaderValue(v string) string {
	need := v != "" && (v[0] == ' ' || v[0] == '\t' || v[len(v)-1] == ' ' || v[len(v)-1] == '\t')
	for _, c := range v {
		if c < 0x20 || c > 0x7E {
			need = true
		}
	}
	if strings.HasPrefix(v, "=?base64?") && strings.HasSuffix(v, "?=") {
		need = true
	}
	if need {
		return "=?base64?" + base64.StdEncoding.EncodeToString([]byte(v)) + "?="
	}
	return v
}
