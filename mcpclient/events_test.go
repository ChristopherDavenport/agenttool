package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Requirement ids: events:<area>:<rule> are the SEP's rules, shared with
// other SDKs; go:events:<area>:<rule> are this package's own.

type fakeListParams struct {
	sdk.ParamsBase
	Cursor string `json:"cursor,omitempty"`
}

type fakeListResult struct {
	sdk.ResultBase
	ResultType string           `json:"resultType"`
	Events     []map[string]any `json:"events"`
	NextCursor string           `json:"nextCursor,omitempty"`
}

type fakePollParams struct {
	sdk.ParamsBase
	Raw map[string]json.RawMessage `json:"-"`
}

func (p *fakePollParams) UnmarshalJSON(b []byte) error { return json.Unmarshal(b, &p.Raw) }

type fakePollResult struct {
	sdk.ResultBase
	ResultType string           `json:"resultType"`
	Events     []map[string]any `json:"events"`
	Cursor     *string          `json:"cursor"`
	Truncated  bool             `json:"truncated"`
	HasMore    bool             `json:"hasMore"`
	NextPollMs int64            `json:"nextPollMs"`
}

// fakeEvents is a server that lists what it is given and answers each
// poll with the next script step.
type fakeEvents struct {
	mu     sync.Mutex
	pages  [][]map[string]any
	script []func(raw map[string]json.RawMessage) (*fakePollResult, error)
	polls  []map[string]json.RawMessage
	offer  bool
}

func (f *fakeEvents) server(t *testing.T) *sdk.Server {
	opts := &sdk.ServerOptions{}
	if f.offer {
		caps := &sdk.ServerCapabilities{}
		caps.AddExtension(EventsExtension, nil)
		opts.Capabilities = caps
	}
	s := sdk.NewServer(&sdk.Implementation{Name: "fake", Version: "1"}, opts)
	err := sdk.AddReceivingCustomMethod(s, EventsListMethod, func(_ context.Context, _ *sdk.ServerSession, p *fakeListParams) (*fakeListResult, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		i := 0
		if p.Cursor != "" {
			fmt.Sscan(p.Cursor, &i)
		}
		res := &fakeListResult{ResultType: "complete", Events: f.pages[i]}
		if i+1 < len(f.pages) {
			res.NextCursor = fmt.Sprint(i + 1)
		}
		return res, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	err = sdk.AddReceivingCustomMethod(s, EventsPollMethod, func(_ context.Context, _ *sdk.ServerSession, p *fakePollParams) (*fakePollResult, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.polls = append(f.polls, p.Raw)
		if len(f.script) == 0 {
			return nil, &jsonrpc.Error{Code: -32603, Message: "script ended"}
		}
		step := f.script[0]
		f.script = f.script[1:]
		return step(p.Raw)
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func desc(name string, delivery []string, input string) map[string]any {
	d := map[string]any{"name": name, "description": "d", "delivery": delivery, "payloadSchema": json.RawMessage(`{"type":"object"}`)}
	if input != "" {
		d["inputSchema"] = json.RawMessage(input)
	} else {
		d["inputSchema"] = json.RawMessage(`{"type":"object"}`)
	}
	return d
}

func evt(id string) map[string]any {
	return map[string]any{"eventId": id, "name": "n", "timestamp": "2026-11-25T12:00:00Z", "data": json.RawMessage(`{"id":"` + id + `"}`)}
}

func step(cursor string, hasMore bool, nextMs int64, events ...string) func(map[string]json.RawMessage) (*fakePollResult, error) {
	return func(map[string]json.RawMessage) (*fakePollResult, error) {
		r := &fakePollResult{ResultType: "complete", Events: []map[string]any{}, HasMore: hasMore, NextPollMs: nextMs}
		if cursor != "" {
			r.Cursor = &cursor
		}
		for _, e := range events {
			r.Events = append(r.Events, evt(e))
		}
		return r, nil
	}
}

func eventsRemote(t *testing.T, f *fakeEvents, opts ...Option) *Remote {
	f.offer = true
	return connect(t, f.server(t), append([]Option{WithEvents()}, opts...)...)
}

// TestEventsGating holds events:capability:gating: no events request
// goes to a server that did not advertise the extension, or from a
// client that did not take it.
func TestEventsGating(t *testing.T) {
	ctx := context.Background()
	f := &fakeEvents{pages: [][]map[string]any{{}}}
	f.offer = false
	s := connect(t, f.server(t), WithEvents())
	if s.EventsOffered() {
		t.Error("EventsOffered for a server that did not advertise")
	}
	if _, err := s.ListEvents(ctx); !errors.Is(err, ErrEventsNotOffered) {
		t.Errorf("ListEvents = %v, want ErrEventsNotOffered", err)
	}
	if _, err := s.PollEvents(ctx, PollParams{Name: "n"}); !errors.Is(err, ErrEventsNotOffered) {
		t.Errorf("PollEvents = %v, want ErrEventsNotOffered", err)
	}
	if err := s.RunEvents(ctx, EventLoop{Name: "n"}, nil); !errors.Is(err, ErrEventsNotOffered) {
		t.Errorf("RunEvents = %v, want ErrEventsNotOffered", err)
	}
	if len(f.polls) != 0 {
		t.Errorf("%d requests reached the server", len(f.polls))
	}

	f.offer = true
	plain := connect(t, f.server(t))
	if _, err := plain.ListEvents(ctx); err == nil || errors.Is(err, ErrEventsNotOffered) {
		t.Errorf("without WithEvents: %v, want an error saying so", err)
	}
	on := connect(t, f.server(t), WithEvents())
	if !on.EventsOffered() {
		t.Error("EventsOffered = false for a server that advertised")
	}
}

// TestEventsClientCapability: the client offers the extension, and keeps
// the capabilities the host set.
func TestEventsClientCapability(t *testing.T) {
	var o options
	custom := &sdk.ClientCapabilities{}
	custom.AddExtension("acme/x", nil)
	WithClientOptions(sdk.ClientOptions{Capabilities: custom})(&o)
	WithEvents()(&o)
	exts := o.clientOpts.Capabilities.Extensions
	if _, ok := exts[EventsExtension]; !ok {
		t.Errorf("extensions = %v, want %s", exts, EventsExtension)
	}
	if _, ok := exts["acme/x"]; !ok {
		t.Errorf("extensions = %v, want the host's kept", exts)
	}
	if _, ok := custom.Extensions[EventsExtension]; ok {
		t.Error("the host's capabilities were changed")
	}
}

// TestEventsList holds events:list:descriptors: all pages, descriptors
// read in full, and a type whose header annotations break the transport's
// rules left out.
func TestEventsList(t *testing.T) {
	f := &fakeEvents{pages: [][]map[string]any{
		{desc("a", []string{"poll"}, ""), desc("bad.name", []string{"poll"}, `{"type":"object","properties":{"x":{"type":"string","x-mcp-header":"Bad Name"}}}`)},
		{desc("b", []string{"push", "poll"}, `{"type":"object","properties":{"r":{"type":"string","x-mcp-header":"Region"}}}`), desc("c", []string{"webhook"}, ""), desc("num", []string{"poll"}, `{"type":"object","properties":{"x":{"type":"number","x-mcp-header":"X"}}}`)},
	}}
	s := eventsRemote(t, f)
	got, err := s.ListEvents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, d := range got {
		names = append(names, d.Name)
	}
	if strings.Join(names, ",") != "a,b,c" {
		t.Errorf("listed %v, want a,b,c with the two bad annotations left out", names)
	}
	if !got[1].Polls() || got[2].Polls() || got[0].Description != "d" || string(got[0].PayloadSchema) != `{"type":"object"}` {
		t.Errorf("descriptors = %+v", got)
	}
}

// TestEventsPollWire holds events:poll:bootstrap and events:cursor:absent-is-null
// from the sender's side: the cursor is always sent, null to start from
// now, and the limits only when set; the result is read in full.
func TestEventsPollWire(t *testing.T) {
	f := &fakeEvents{pages: [][]map[string]any{{desc("n", []string{"poll"}, "")}}}
	f.script = []func(map[string]json.RawMessage) (*fakePollResult, error){
		step("c1", false, 5000),
		func(map[string]json.RawMessage) (*fakePollResult, error) {
			c := "c2"
			return &fakePollResult{ResultType: "complete", Events: []map[string]any{evt("x")}, Cursor: &c, Truncated: true, HasMore: true, NextPollMs: 250}, nil
		},
	}
	s := eventsRemote(t, f)
	ctx := context.Background()
	res, err := s.PollEvents(ctx, PollParams{Name: "n"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Cursor == nil || *res.Cursor != "c1" || res.NextPoll != 5*time.Second || len(res.Events) != 0 {
		t.Errorf("first = %+v", res)
	}
	c := "c1"
	res, err = s.PollEvents(ctx, PollParams{Name: "n", Arguments: json.RawMessage(`{"a":1}`), Cursor: &c, MaxAge: 90 * time.Second, MaxEvents: 5})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated || !res.HasMore || res.NextPoll != 250*time.Millisecond || len(res.Events) != 1 {
		t.Fatalf("second = %+v", res)
	}
	ev := res.Events[0]
	if ev.EventID != "x" || ev.Name != "n" || !ev.Timestamp.Equal(time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)) || string(ev.Data) != `{"id":"x"}` {
		t.Errorf("event = %+v", ev)
	}
	if string(f.polls[0]["cursor"]) != "null" || f.polls[0]["maxAgeMs"] != nil || f.polls[0]["maxEvents"] != nil || f.polls[0]["arguments"] != nil {
		t.Errorf("first request = %v, want a null cursor and nothing else but the name", f.polls[0])
	}
	if string(f.polls[1]["cursor"]) != `"c1"` || string(f.polls[1]["maxAgeMs"]) != "90000" || string(f.polls[1]["maxEvents"]) != "5" || string(f.polls[1]["arguments"]) != `{"a":1}` {
		t.Errorf("second request = %v", f.polls[1])
	}
}

// TestEventsPollErrors holds events:poll:errors: a JSON-RPC error is an
// EventError a caller branches on.
func TestEventsPollErrors(t *testing.T) {
	f := &fakeEvents{pages: [][]map[string]any{{desc("n", []string{"poll"}, "")}}}
	f.script = []func(map[string]json.RawMessage) (*fakePollResult, error){
		func(map[string]json.RawMessage) (*fakePollResult, error) {
			return nil, &jsonrpc.Error{Code: EventsCodeNotFound, Message: "gone", Data: json.RawMessage(`{"kind":"event"}`)}
		},
	}
	s := eventsRemote(t, f)
	_, err := s.PollEvents(context.Background(), PollParams{Name: "n"})
	var ee *EventError
	if !errors.As(err, &ee) || ee.Code != EventsCodeNotFound || string(ee.Data) != `{"kind":"event"}` || !strings.Contains(ee.Error(), "gone") {
		t.Errorf("error = %#v", err)
	}
}

// TestEventsHeaderMismatchRelists: a server that refuses for headers is
// asked again after the event types are listed again.
func TestEventsHeaderMismatchRelists(t *testing.T) {
	f := &fakeEvents{pages: [][]map[string]any{{desc("n", []string{"poll"}, "")}}}
	f.script = []func(map[string]json.RawMessage) (*fakePollResult, error){
		func(map[string]json.RawMessage) (*fakePollResult, error) {
			return nil, &jsonrpc.Error{Code: EventsCodeHeaderMismatch, Message: "headers"}
		},
		step("c", false, 1000),
	}
	s := eventsRemote(t, f)
	res, err := s.PollEvents(context.Background(), PollParams{Name: "n"})
	if err != nil || res.Cursor == nil {
		t.Fatalf("poll = %+v, %v; want the retry's answer", res, err)
	}
	if len(f.polls) != 2 {
		t.Errorf("%d polls, want 2", len(f.polls))
	}
}

// loopHarness runs RunEvents against a script with a clock that
// records the waits and cancels the loop when the script ends.
type loopHarness struct {
	waits  []time.Duration
	got    []string
	cancel context.CancelFunc
	f      *fakeEvents
}

func (h *loopHarness) run(t *testing.T, l EventLoop, handle func(context.Context, EventOccurrence) error) error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	defer cancel()
	s := eventsRemote(t, h.f)
	l.Sleep = func(ctx context.Context, d time.Duration) error {
		h.waits = append(h.waits, d)
		h.f.mu.Lock()
		done := len(h.f.script) == 0
		h.f.mu.Unlock()
		if done {
			cancel()
		}
		return ctx.Err()
	}
	if handle == nil {
		handle = func(_ context.Context, ev EventOccurrence) error { h.got = append(h.got, ev.EventID); return nil }
	}
	return s.RunEvents(ctx, l, handle)
}

// TestEventsLoop holds go:events:poll-loop and events:client:dedupe: the
// loop dedups by event id, saves the cursor, polls at once on hasMore,
// waits nextPollMs but never less than the floor, and reports a gap.
func TestEventsLoop(t *testing.T) {
	f := &fakeEvents{pages: [][]map[string]any{{desc("n", []string{"poll"}, "")}}}
	f.script = []func(map[string]json.RawMessage) (*fakePollResult, error){
		step("c1", true, 9000, "a", "b"),  // hasMore: no wait
		step("c2", false, 9000, "b", "c"), // b is a repeat; waits 9s
		step("c3", false, 100),            // under the floor: waits 1s
		func(map[string]json.RawMessage) (*fakePollResult, error) {
			c := "c4"
			return &fakePollResult{ResultType: "complete", Events: []map[string]any{evt("d")}, Cursor: &c, Truncated: true, NextPollMs: 3000}, nil
		},
	}
	cursors := NewMemoryCursors()
	gaps := 0
	h := &loopHarness{f: f}
	err := h.run(t, EventLoop{Name: "n", Cursors: cursors, OnGap: func(context.Context) { gaps++ }}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunEvents = %v, want the context's error", err)
	}
	if strings.Join(h.got, ",") != "a,b,c,d" {
		t.Errorf("handled %v, want a,b,c,d once each", h.got)
	}
	if want := []time.Duration{9 * time.Second, time.Second, 3 * time.Second}; fmt.Sprint(h.waits) != fmt.Sprint(want) {
		t.Errorf("waits %v, want %v", h.waits, want)
	}
	if gaps != 1 {
		t.Errorf("OnGap called %d times, want 1", gaps)
	}
	got, _ := cursors.Load(context.Background(), EventKey("n", nil))
	if got == nil || *got != "c4" {
		t.Errorf("saved cursor = %v, want c4", got)
	}
	// Each poll was sent with the cursor the last returned.
	wants := []string{"null", `"c1"`, `"c2"`, `"c3"`}
	for i, w := range wants {
		if string(f.polls[i]["cursor"]) != w {
			t.Errorf("poll %d cursor = %s, want %s", i, f.polls[i]["cursor"], w)
		}
	}
}

// TestEventsLoopResumes: a loop that runs again starts from the cursor
// the store kept, and a type that does not replay is polled with none.
func TestEventsLoopResumes(t *testing.T) {
	f := &fakeEvents{pages: [][]map[string]any{{desc("n", []string{"poll"}, "")}}}
	f.script = []func(map[string]json.RawMessage) (*fakePollResult, error){step("kept", false, 0, "a")}
	cursors := NewMemoryCursors()
	h := &loopHarness{f: f}
	_ = h.run(t, EventLoop{Name: "n", Cursors: cursors}, nil)

	f.script = []func(map[string]json.RawMessage) (*fakePollResult, error){step("", false, 0, "b"), step("", false, 0, "c")}
	h2 := &loopHarness{f: f}
	_ = h2.run(t, EventLoop{Name: "n", Cursors: cursors}, nil)
	if string(f.polls[1]["cursor"]) != `"kept"` {
		t.Errorf("resumed with cursor %s, want \"kept\"", f.polls[1]["cursor"])
	}
	if string(f.polls[2]["cursor"]) != "null" {
		t.Errorf("a type with a null cursor is polled with %s, want null", f.polls[2]["cursor"])
	}
	// The store keeps the last real cursor: a null one is not saved.
	if got, _ := cursors.Load(context.Background(), EventKey("n", nil)); got == nil || *got != "kept" {
		t.Errorf("store = %v, want kept", got)
	}
}

// TestEventsLoopStops: an error from the handler stops the loop with the
// batch's cursor unsaved, so its events come again; a server's error
// ends it and is the loop's error.
func TestEventsLoopStops(t *testing.T) {
	f := &fakeEvents{pages: [][]map[string]any{{desc("n", []string{"poll"}, "")}}}
	f.script = []func(map[string]json.RawMessage) (*fakePollResult, error){step("c1", false, 0, "a", "b")}
	cursors := NewMemoryCursors()
	boom := errors.New("boom")
	h := &loopHarness{f: f}
	err := h.run(t, EventLoop{Name: "n", Cursors: cursors}, func(_ context.Context, ev EventOccurrence) error {
		if ev.EventID == "b" {
			return boom
		}
		return nil
	})
	if !errors.Is(err, boom) {
		t.Errorf("RunEvents = %v, want the handler's error", err)
	}
	if got, _ := cursors.Load(context.Background(), EventKey("n", nil)); got != nil {
		t.Errorf("cursor %v saved for a batch that was not handled", *got)
	}

	f.script = []func(map[string]json.RawMessage) (*fakePollResult, error){
		func(map[string]json.RawMessage) (*fakePollResult, error) {
			return nil, &jsonrpc.Error{Code: EventsCodeForbidden, Message: "revoked"}
		},
	}
	h2 := &loopHarness{f: f}
	var ee *EventError
	if err := h2.run(t, EventLoop{Name: "n"}, nil); !errors.As(err, &ee) || ee.Code != EventsCodeForbidden {
		t.Errorf("RunEvents = %v, want the forbidden EventError", err)
	}
}

// TestEventKey: arguments are the same subscription however spelt.
func TestEventKey(t *testing.T) {
	a := EventKey("n", json.RawMessage(`{"a":1,"b":[1,2],"c":{"y":1,"x":2}}`))
	b := EventKey("n", json.RawMessage(` { "c":{"x":2,"y":1}, "b":[1,2], "a":1 } `))
	if a != b {
		t.Errorf("%q != %q", a, b)
	}
	if EventKey("n", nil) != EventKey("n", json.RawMessage(`{}`)) || EventKey("n", nil) == EventKey("m", nil) {
		t.Error("nil arguments are {}, and the name is part of the key")
	}
}

func TestSeenSet(t *testing.T) {
	s := newSeenSet(2)
	for i, tc := range []struct {
		id  string
		new bool
	}{{"a", true}, {"a", false}, {"b", true}, {"c", true}, {"a", true}, {"c", false}} {
		if got := s.add(tc.id); got != tc.new {
			t.Errorf("step %d: add(%s) = %v, want %v", i, tc.id, got, tc.new)
		}
	}
}

// TestEventsHeaderTripper holds events:mcp-name and events:mcp-param from
// the sender's side: Mcp-Name and an Mcp-Param-* for each annotated
// argument present, Base64 where needed, and other requests untouched.
func TestEventsHeaderTripper(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = r.Header.Clone() }))
	defer srv.Close()
	hc := &http.Client{Transport: EventHeaders(nil)}
	input := json.RawMessage(`{"type":"object","properties":{
		"region":{"type":"string","x-mcp-header":"Region"},
		"tier":{"type":"integer","x-mcp-header":"Tier"},
		"live":{"type":"boolean","x-mcp-header":"Live"},
		"nested":{"type":"object","properties":{"zone":{"type":"string","x-mcp-header":"Zone"}}},
		"gone":{"type":"string","x-mcp-header":"Gone"},
		"nul":{"type":"string","x-mcp-header":"Nul"},
		"note":{"type":"string"}}}`)
	tests := []struct {
		name string
		args string
		want map[string]string
	}{
		{"plain", `{"region":"us","tier":2,"live":false,"nested":{"zone":"a"},"nul":null,"note":"x"}`, map[string]string{"Mcp-Name": "evt", "Mcp-Param-Region": "us", "Mcp-Param-Tier": "2", "Mcp-Param-Live": "false", "Mcp-Param-Zone": "a"}},
		{"non-ascii", `{"region":"zürich"}`, map[string]string{"Mcp-Name": "evt", "Mcp-Param-Region": "=?base64?esO8cmljaA==?="}},
		{"edge space", `{"region":" x"}`, map[string]string{"Mcp-Name": "evt", "Mcp-Param-Region": "=?base64?IHg=?="}},
		{"sentinel look-alike", `{"region":"=?base64?x?="}`, map[string]string{"Mcp-Name": "evt", "Mcp-Param-Region": "=?base64?PT9iYXNlNjQ/eD89?="}},
		{"no args", ``, map[string]string{"Mcp-Name": "evt"}},
		{"not a primitive", `{"region":{"a":1},"tier":1.5}`, map[string]string{"Mcp-Name": "evt"}},
	}
	for _, tc := range tests {
		ctx := withEventHeaders(context.Background(), "evt", input, json.RawMessage(tc.args))
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, nil)
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		for k, v := range tc.want {
			if got.Get(k) != v {
				t.Errorf("%s: %s = %q, want %q", tc.name, k, got.Get(k), v)
			}
		}
		for k := range got {
			if strings.HasPrefix(k, "Mcp-Param-") && tc.want[k] == "" {
				t.Errorf("%s: unexpected %s", tc.name, k)
			}
		}
	}
	// A request with nothing on its context is passed on as it is.
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got.Get("Mcp-Name") != "" {
		t.Errorf("Mcp-Name on a request that is not an events request: %q", got.Get("Mcp-Name"))
	}
}
