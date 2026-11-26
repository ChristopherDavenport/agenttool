package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Requirement ids: events:<area>:<rule> are the SEP's rules, shared with
// other SDKs; go:events:<area>:<rule> are this package's own. Each test
// names the ids it holds in its doc comment, and
// TestEventsRequirementsCovered checks every id in eventsRequirements is
// named by one.

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock { return &fakeClock{t: time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// withoutInfo is raw without the serverInfo the SDK stamps on a result's _meta.
func withoutInfo(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if meta, ok := m["_meta"].(map[string]any); ok {
		delete(meta, "io.modelcontextprotocol/serverInfo")
		if len(meta) == 0 {
			delete(m, "_meta")
		}
	}
	out, _ := json.Marshal(m)
	return string(out)
}

// rawResult keeps a result as sent.
type rawResult struct {
	sdk.ResultBase
	raw json.RawMessage
}

func (r *rawResult) UnmarshalJSON(b []byte) error {
	r.raw = append(r.raw[:0], b...)
	return nil
}

const countSchema = `{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}`

type eventsHarness struct {
	t      *testing.T
	clock  *fakeClock
	events *Events
	cs     *sdk.ClientSession
	errMu  sync.Mutex
	errors []error
}

// reported is what the server told OnError so far.
func (h *eventsHarness) reported() []error {
	h.errMu.Lock()
	defer h.errMu.Unlock()
	return append([]error(nil), h.errors...)
}

func newEventsHarness(t *testing.T, mod func(*EventsOptions)) *eventsHarness {
	t.Helper()
	return newEventsHarnessWithClock(t, newClock(), mod)
}

func newEventsHarnessWithClock(t *testing.T, clock *fakeClock, mod func(*EventsOptions)) *eventsHarness {
	t.Helper()
	h := &eventsHarness{t: t, clock: clock}
	opts := EventsOptions{Now: h.clock.Now, epoch: "abcd1234", OnError: func(err error) { h.errMu.Lock(); h.errors = append(h.errors, err); h.errMu.Unlock() }}
	if mod != nil {
		mod(&opts)
	}
	server := sdk.NewServer(&sdk.Implementation{Name: "events", Version: "1"}, nil)
	ev, err := AddEvents(server, opts)
	if err != nil {
		t.Fatal(err)
	}
	h.events = ev
	ct, st := sdk.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := server.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	client := sdk.NewClient(&sdk.Implementation{Name: "raw", Version: "1"}, nil)
	for _, m := range []string{EventsListMethod, EventsPollMethod, EventsStreamMethod, EventsSubscribeMethod, EventsUnsubscribeMethod} {
		if err := sdk.AddSendingCustomMethod[*eventsParams, *rawResult](client, m); err != nil {
			t.Fatal(err)
		}
	}
	h.cs, err = client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.cs.Close() })
	return h
}

// call sends method with params, JSON, and returns the result as sent
// or the JSON-RPC error.
func (h *eventsHarness) call(method, params string) (json.RawMessage, *jsonrpc.Error) {
	h.t.Helper()
	var p eventsParams
	if err := json.Unmarshal([]byte(params), &p); err != nil {
		h.t.Fatal(err)
	}
	res, err := sdk.CallCustomMethod[*eventsParams, *rawResult](context.Background(), h.cs, method, &p)
	if err != nil {
		var we *jsonrpc.Error
		if !errors.As(err, &we) {
			h.t.Fatalf("%s: %v is not a JSON-RPC error", method, err)
		}
		return nil, we
	}
	return res.raw, nil
}

type pollOut struct {
	ResultType string `json:"resultType"`
	Events     []struct {
		EventID   string          `json:"eventId"`
		Name      string          `json:"name"`
		Timestamp string          `json:"timestamp"`
		Data      json.RawMessage `json:"data"`
		Cursor    *string         `json:"cursor"`
		Meta      map[string]any  `json:"_meta"`
	} `json:"events"`
	Cursor     *string `json:"cursor"`
	Truncated  bool    `json:"truncated"`
	HasMore    bool    `json:"hasMore"`
	NextPollMs int64   `json:"nextPollMs"`
}

func (h *eventsHarness) poll(params string) pollOut {
	h.t.Helper()
	raw, err := h.call(EventsPollMethod, params)
	if err != nil {
		h.t.Fatalf("poll %s: %v", params, err)
	}
	var out pollOut
	if e := json.Unmarshal(raw, &out); e != nil {
		h.t.Fatal(e)
	}
	return out
}

func (h *eventsHarness) pollErr(params string) *jsonrpc.Error {
	h.t.Helper()
	raw, err := h.call(EventsPollMethod, params)
	if err == nil {
		h.t.Fatalf("poll %s = %s, want an error", params, raw)
	}
	return err
}

func (h *eventsHarness) emitter(name string, mod func(*EventDef)) *EventType {
	h.t.Helper()
	def := EventDef{Name: name, Description: "counts", PayloadSchema: json.RawMessage(countSchema), EmitOnly: true}
	if mod != nil {
		mod(&def)
	}
	et, err := h.events.Register(def)
	if err != nil {
		h.t.Fatal(err)
	}
	return et
}

func emit(t *testing.T, et *EventType, id string, n int) {
	t.Helper()
	if err := et.Emit(Event{ID: id, Data: json.RawMessage(fmt.Sprintf(`{"n":%d}`, n))}); err != nil {
		t.Fatal(err)
	}
}

func str(s string) *string { return &s }

func ids(o pollOut) string {
	var s []string
	for _, e := range o.Events {
		s = append(s, e.EventID)
	}
	return strings.Join(s, ",")
}

func cur(o pollOut) string {
	if o.Cursor == nil {
		return "<null>"
	}
	return *o.Cursor
}

// eventsRequirements are the requirement ids the tests hold.
var eventsRequirements = []string{
	"events:capability:declared",
	"events:fallback:unsupported-modes",
	"events:list:descriptors",
	"events:list:delivery",
	"events:list:pagination",
	"events:poll:bootstrap",
	"events:poll:replay",
	"events:poll:has-more",
	"events:poll:truncated",
	"events:poll:errors",
	"events:poll:next-poll-ms",
	"events:poll:result-type",
	"events:cursor:absent-is-null",
	"events:cursor:max-age",
	"events:cursor:non-replay",
	"events:mcp-name",
	"events:mcp-param",
	"events:stream:frames",
	"events:error-codes",
	"go:events:register:rules",
	"go:events:register:schema-change",
	"go:events:ring:epoch",
	"go:events:ring:bounds",
	"go:events:lease:lifecycle",
	"go:events:lease:capacity",
	"go:events:hooks:match-transform",
	"go:events:sink:feed",
	"go:events:constants-match-client",
	"go:events:header-roundtrip",
	"go:events:poll-loop",
	"events:webhook:mcp-name-mismatch-rejected",
	"events:webhook:mcp-name-missing-rejected",
	"events:webhook:receiver-verify-signature",
	"events:webhook:receiver-timestamp-freshness",
	"events:webhook:receiver-dedup-webhook-id",
	"events:webhook:receiver-dedup-event-per-subscription",
	"events:webhook:receiver-forwards-control",
	"events:webhook:receiver-verification-echo",
	"events:webhook:receiver-unknown-id-retryable",
	"go:events:webhook:secret-made",
	"go:events:webhook:keep",
	"go:events:webhook:receiver-retry",
	"go:events:webhook:end-to-end",
	"events:webhook:delivery-410-non-retryable",
	"events:webhook:delivery-413-non-retryable",
	"events:webhook:delivery-body-size",
	"events:webhook:delivery-dual-sign-on-rotation",
	"events:webhook:delivery-post-json",
	"events:webhook:delivery-retries-bounded",
	"events:webhook:delivery-retry-regenerates-signature",
	"events:webhook:delivery-signature-formula",
	"events:webhook:delivery-standard-webhooks-headers",
	"events:webhook:delivery-status-last-error-category",
	"events:webhook:delivery-subscription-id-header",
	"events:webhook:delivery-webhook-id-per-message",
	"events:webhook:envelope-gap",
	"events:webhook:envelope-signed-like-deliveries",
	"events:webhook:envelope-terminated",
	"events:webhook:envelope-type-discriminator",
	"events:webhook:envelope-webhook-id-format",
	"events:webhook:error-callback-endpoint-error",
	"events:webhook:error-unsupported",
	"events:webhook:ssrf-no-redirects",
	"events:webhook:ssrf-reject-non-routable",
	"events:webhook:ssrf-validate-at-delivery-time",
	"events:webhook:ssrf-validate-callback-url",
	"events:webhook:subscribe-cross-tenant-isolation",
	"events:webhook:subscribe-id-derived",
	"events:webhook:subscribe-id-not-an-input",
	"events:webhook:subscribe-idempotent-upsert",
	"events:webhook:subscribe-key-composition",
	"events:webhook:subscribe-key-immutable",
	"events:webhook:subscribe-refresh-before-present",
	"events:webhook:subscribe-refresh-reactivates",
	"events:webhook:subscribe-response-cursor",
	"events:webhook:subscribe-response-truncated",
	"events:webhook:subscribe-secret-format",
	"events:webhook:subscribe-secret-rejected",
	"events:webhook:subscribe-secret-required",
	"events:webhook:subscribe-url-https-required",
	"events:webhook:subscribe-url-non-https-rejected",
	"events:webhook:subscribe-webhook-only",
	"events:webhook:ttl-no-rejection-path",
	"events:webhook:ttl-null-only-when-requested",
	"events:webhook:ttl-omitted-means-default",
	"events:webhook:ttl-refresh-before-lte-suggestion",
	"events:webhook:unsubscribe-by-key",
	"events:webhook:unsubscribe-empty-result",
	"events:webhook:unsubscribe-unknown-not-found",
	"events:webhook:verification-cached-per-principal-url",
	"events:webhook:verification-challenge-echo",
	"events:webhook:verification-failure-error",
	"events:webhook:verification-no-raw-endpoint-responses",
	"events:webhook:verification-required-before-delivery",
	"events:webhook:verification-uses-ssrf-hardened-path",
	"go:events:webhook:close",
	"go:events:webhook:concurrent-emit",
	"go:events:webhook:expiry",
	"go:events:webhook:hooks",
	"go:events:webhook:no-reverify-on-refresh",
	"go:events:webhook:replay",
	"go:events:webhook:send-limits",
	"go:events:webhook:store",
	"go:events:webhook:suspension",
	"go:events:webhook:trust-origin-at-runtime",
	"go:events:webhook:verification-failed-pair",
	"go:events:webhook:verification-host-limits",
	"go:events:webhook:watermark",
}

// eventsWebhookBaselined are the requirement ids of the webhook
// conformance scenarios this package does not meet, with the reason for
// each, as a baseline file would carry them. TestEventsRequirementsCovered
// checks that no test claims one, so a row that is implemented moves to
// eventsRequirements.
var eventsWebhookBaselined = map[string]string{
	"events:webhook:ttl-no-expiry-persisted":              "this package never grants a subscription that does not expire, which the SEP allows; a server that grants no expiry must persist it across restarts",
	"events:webhook:ttl-no-expiry-gc-terminated":          "no expiry is never granted, so there is no sustained-failure drop with a terminated envelope",
	"events:webhook:ttl-long-grant-retained":              "grants are at most WebhookOptions.TTL, an hour by default, held in the store the embedder chose; a long grant over the default map store would not survive a restart",
	"events:webhook:verification-persisted-for-no-expiry": "verification is soft state per process, and no expiry is never granted; a restart verifies again on the next subscribe",
	"events:webhook:server-identity-key-discovery":        "the optional v1a ed25519 signature and the Server Card signingKeys are not implemented; they depend on ext-server-card#55",
}

// TestEventsCapability holds events:capability:declared: the extension is
// advertised at initialize, with an empty settings object.
func TestEventsCapability(t *testing.T) {
	h := newEventsHarness(t, nil)
	got, ok := h.cs.InitializeResult().Capabilities.Extensions[EventsExtension]
	if !ok {
		t.Fatalf("extensions = %v, want %s", h.cs.InitializeResult().Capabilities.Extensions, EventsExtension)
	}
	if m, ok := got.(map[string]any); !ok || len(m) != 0 {
		t.Errorf("settings = %#v, want {}", got)
	}
}

// TestEventsList holds events:list:descriptors and events:list:delivery:
// the descriptors as sent, sorted by name, with resultType complete.
func TestEventsList(t *testing.T) {
	h := newEventsHarness(t, nil)
	raw, err := h.call(EventsListMethod, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := withoutInfo(t, raw), `{"events":[],"resultType":"complete"}`; got != want {
		t.Errorf("empty list = %s, want %s", got, want)
	}
	h.emitter("b.count", func(d *EventDef) {
		d.InputSchema = json.RawMessage(`{"type":"object","properties":{"region":{"type":"string","x-mcp-header":"Region"}}}`)
		d.Meta = map[string]any{"owner": "x"}
	})
	h.emitter("a.count", nil)
	raw, err = h.call(EventsListMethod, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"resultType":"complete","events":[
	  {"name":"a.count","description":"counts","delivery":["poll"],"inputSchema":{"type":"object"},"payloadSchema":` + countSchema + `},
	  {"name":"b.count","description":"counts","delivery":["poll"],"inputSchema":{"type":"object","properties":{"region":{"type":"string","x-mcp-header":"Region"}}},"payloadSchema":` + countSchema + `,"_meta":{"owner":"x"}}]}`
	if withoutInfo(t, raw) != normalise(t, json.RawMessage(want)) {
		t.Errorf("list = %s\nwant %s", withoutInfo(t, raw), normalise(t, json.RawMessage(want)))
	}
	if _, err := h.call(EventsListMethod, `{"cursor":"x"}`); err == nil || err.Code != EventsCodeInvalidParams {
		t.Errorf("list with a cursor = %v, want invalid params", err)
	}
}

// TestEventsListPagination holds events:list:pagination: a page of
// ListPageSize types, a nextCursor while more follow and none on the last
// page, a cursor that survives a type added or removed between pages, and
// a cursor this server never gave is invalid params.
func TestEventsListPagination(t *testing.T) {
	h := newEventsHarness(t, func(o *EventsOptions) { o.ListPageSize = 2 })
	var typeB *EventType
	for _, n := range []string{"e", "a", "d", "b", "c"} {
		if et := h.emitter(n, nil); n == "b" {
			typeB = et
		}
	}
	names := func(r listEventsResult) string {
		var out []string
		for _, d := range r.Events {
			out = append(out, d.Name)
		}
		return strings.Join(out, ",")
	}
	page := func(params string) listEventsResult {
		t.Helper()
		raw, err := h.call(EventsListMethod, params)
		if err != nil {
			t.Fatal(err)
		}
		var r listEventsResult
		if e := json.Unmarshal(raw, &r); e != nil {
			t.Fatal(e)
		}
		return r
	}
	p1 := page(`{}`)
	if names(p1) != "a,b" || p1.NextCursor == "" {
		t.Fatalf("page 1 = %s next %q", names(p1), p1.NextCursor)
	}
	// A type added before the cursor, and one removed, move nothing else.
	h.emitter("a0", nil)
	typeB.Remove()
	p2 := page(fmt.Sprintf(`{"cursor":%q}`, p1.NextCursor))
	if names(p2) != "c,d" || p2.NextCursor == "" || p2.NextCursor == p1.NextCursor {
		t.Fatalf("page 2 = %s next %q", names(p2), p2.NextCursor)
	}
	p3 := page(fmt.Sprintf(`{"cursor":%q}`, p2.NextCursor))
	if names(p3) != "e" || p3.NextCursor != "" {
		t.Fatalf("page 3 = %s next %q", names(p3), p3.NextCursor)
	}
	for _, bad := range []string{`{"cursor":"x"}`, `{"cursor":"bGlzdDo"}`, `{"cursor":5}`} {
		if _, err := h.call(EventsListMethod, bad); err == nil || err.Code != EventsCodeInvalidParams {
			t.Errorf("list %s = %v, want invalid params", bad, err)
		}
	}
}

// TestEventsPollEmitOnly holds events:poll:bootstrap, events:poll:replay,
// events:poll:has-more, events:poll:result-type and
// events:cursor:absent-is-null for an emit-only type.
func TestEventsPollEmitOnly(t *testing.T) {
	h := newEventsHarness(t, nil)
	et := h.emitter("count", nil)
	emit(t, et, "pre", 0) // before the client starts: not replayed

	first := h.poll(`{"name":"count"}`)
	if first.ResultType != "complete" || len(first.Events) != 0 || cur(first) != "eabcd1234.1" {
		t.Fatalf("bootstrap = %+v, want no events at eabcd1234.1", first)
	}
	if again := h.poll(`{"name":"count","cursor":null}`); cur(again) != "eabcd1234.1" || len(again.Events) != 0 {
		t.Errorf("explicit null cursor = %+v, want the same as absent", again)
	}
	for i := 1; i <= 5; i++ {
		emit(t, et, fmt.Sprintf("e%d", i), i)
	}
	steps := []struct {
		name, params string
		ids, cursor  string
		more         bool
	}{
		{"limited", `{"name":"count","cursor":"eabcd1234.1","maxEvents":2}`, "e1,e2", "eabcd1234.3", true},
		{"continued", `{"name":"count","cursor":"eabcd1234.3","maxEvents":2}`, "e3,e4", "eabcd1234.5", true},
		{"rest", `{"name":"count","cursor":"eabcd1234.5","maxEvents":2}`, "e5", "eabcd1234.6", false},
		{"all", `{"name":"count","cursor":"eabcd1234.1"}`, "e1,e2,e3,e4,e5", "eabcd1234.6", false},
		{"caught up", `{"name":"count","cursor":"eabcd1234.6"}`, "", "eabcd1234.6", false},
	}
	for _, s := range steps {
		got := h.poll(s.params)
		if ids(got) != s.ids || cur(got) != s.cursor || got.HasMore != s.more || got.Truncated {
			t.Errorf("%s: ids %q cursor %s hasMore %v truncated %v; want %q %s %v", s.name, ids(got), cur(got), got.HasMore, got.Truncated, s.ids, s.cursor, s.more)
		}
	}
	got := h.poll(`{"name":"count","cursor":"eabcd1234.5"}`)
	if len(got.Events) != 1 || got.Events[0].Name != "count" || got.Events[0].Timestamp != "2026-11-25T12:00:00Z" || normalise(t, got.Events[0].Data) != `{"n":5}` || got.Events[0].Cursor != nil {
		t.Errorf("occurrence = %+v, want name, timestamp and data, and no cursor of its own", got.Events)
	}
}

// TestEventsPollTruncated holds events:poll:truncated and
// events:cursor:max-age for the ring.
func TestEventsPollTruncated(t *testing.T) {
	h := newEventsHarness(t, func(o *EventsOptions) { o.Buffer = EmitBuffer{MaxEvents: 3, MaxAge: time.Hour} })
	et := h.emitter("count", nil)
	for i := 1; i <= 5; i++ {
		emit(t, et, fmt.Sprintf("e%d", i), i)
		h.clock.Advance(time.Minute)
	}
	// e1 and e2 were dropped by count.
	tests := []struct {
		name, params string
		ids, cursor  string
		truncated    bool
		more         bool
	}{
		{"behind the ring", `{"name":"count","cursor":"eabcd1234.0"}`, "", "eabcd1234.2", true, true},
		{"at the edge", `{"name":"count","cursor":"eabcd1234.2"}`, "e3,e4,e5", "eabcd1234.5", false, false},
		{"another process", `{"name":"count","cursor":"e00000000.4"}`, "", "eabcd1234.2", true, true},
		{"not ours", `{"name":"count","cursor":"junk"}`, "", "eabcd1234.2", true, true},
		{"ahead of us", `{"name":"count","cursor":"eabcd1234.99"}`, "", "eabcd1234.2", true, true},
		{"max age floor", `{"name":"count","cursor":"eabcd1234.2","maxAgeMs":150000}`, "e4,e5", "eabcd1234.5", true, false},
		{"max age with no cursor is ignored", `{"name":"count","maxAgeMs":1}`, "", "eabcd1234.5", false, false},
	}
	for _, tc := range tests {
		got := h.poll(tc.params)
		if ids(got) != tc.ids || cur(got) != tc.cursor || got.Truncated != tc.truncated || got.HasMore != tc.more {
			t.Errorf("%s: ids %q cursor %s truncated %v hasMore %v; want %q %s %v %v", tc.name, ids(got), cur(got), got.Truncated, got.HasMore, tc.ids, tc.cursor, tc.truncated, tc.more)
		}
	}
	// The gap is a result, never an error, and polling from its cursor goes on.
	got := h.poll(`{"name":"count","cursor":"eabcd1234.0"}`)
	if next := h.poll(fmt.Sprintf(`{"name":"count","cursor":%q}`, cur(got))); ids(next) != "e3,e4,e5" || next.Truncated {
		t.Errorf("after the gap = %+v, want the retained events", next)
	}
}

// TestEventsRingBounds holds go:events:ring:bounds and go:events:ring:epoch: the buffer drops by age and by count, and a
// replaced type keeps the sequence so old cursors read as gaps.
func TestEventsRingBounds(t *testing.T) {
	clock := newClock()
	r := newRing("ep", clock.Now, EmitBuffer{MaxAge: time.Minute, MaxEvents: 10})
	all := func(rec eventRecord) (Event, bool) { return rec.event(), true }
	for i := 0; i < 3; i++ {
		r.append(eventRecord{ID: fmt.Sprint(i)})
		clock.Advance(40 * time.Second)
	}
	// Sequence numbers 1, 2, 3 were appended 120s, 80s and 40s ago: the
	// first two are past a minute, so 1 and 2 are dropped.
	if g := r.read(str(r.format(0)), 10, time.Time{}, all); !g.truncated || g.cursor != r.format(2) || !g.hasMore {
		t.Errorf("a cursor before the dropped events = %+v, want a gap at the oldest position", g)
	}
	if g := r.read(str(r.format(1)), 10, time.Time{}, all); !g.truncated {
		t.Errorf("cursor 1 is before dropped event 2: %+v", g)
	}
	g := r.read(str(r.format(2)), 10, time.Time{}, all)
	if g.truncated || len(g.events) != 1 || g.events[0].ID != "2" || g.cursor != r.format(3) {
		t.Errorf("a cursor at the edge = %+v, want the newest event and no gap", g)
	}

	// watch signals appends, and stops.
	n := 0
	cancel := r.watch(func() { n++ })
	r.append(eventRecord{ID: "x"})
	cancel()
	r.append(eventRecord{ID: "y"})
	if n != 1 {
		t.Errorf("watcher called %d times, want 1", n)
	}

	// A new ring that continues an old one reads old cursors as gaps.
	next := newRing("zz", clock.Now, EmitBuffer{MaxAge: time.Hour, MaxEvents: 10})
	next.continueFrom(r)
	if g := next.read(str(r.format(r.last-1)), 10, time.Time{}, all); !g.truncated {
		t.Errorf("an old cursor on the continued ring = %+v, want a gap", g)
	}
	if g := next.read(str(r.format(r.last)), 10, time.Time{}, all); g.truncated || len(g.events) != 0 {
		t.Errorf("a cursor at the head on the continued ring = %+v, want none", g)
	}
}

// TestEventsPollErrors holds events:poll:errors and events:error-codes:
// plain JSON-RPC errors, with the codes and data of the SEP.
func TestEventsPollErrors(t *testing.T) {
	h := newEventsHarness(t, nil)
	h.emitter("count", func(d *EventDef) {
		d.InputSchema = json.RawMessage(`{"type":"object","properties":{"region":{"type":"string"}},"required":["region"],"additionalProperties":false}`)
	})
	tests := []struct {
		name, params string
		code         int64
		data         string
	}{
		{"unknown event", `{"name":"nope"}`, EventsCodeNotFound, `{"kind":"event"}`},
		{"no name", `{}`, EventsCodeInvalidParams, ""},
		{"missing argument", `{"name":"count"}`, EventsCodeInvalidParams, ""},
		{"wrong argument", `{"name":"count","arguments":{"region":1}}`, EventsCodeInvalidParams, ""},
		{"extra argument", `{"name":"count","arguments":{"region":"a","x":1}}`, EventsCodeInvalidParams, ""},
		{"bad cursor", `{"name":"count","arguments":{"region":"a"},"cursor":3}`, EventsCodeInvalidParams, ""},
		{"bad maxEvents", `{"name":"count","arguments":{"region":"a"},"maxEvents":0}`, EventsCodeInvalidParams, ""},
		{"bad maxAgeMs", `{"name":"count","arguments":{"region":"a"},"maxAgeMs":-1}`, EventsCodeInvalidParams, ""},
		{"fractional maxEvents", `{"name":"count","arguments":{"region":"a"},"maxEvents":1.5}`, EventsCodeInvalidParams, ""},
	}
	for _, tc := range tests {
		err := h.pollErr(tc.params)
		if err.Code != tc.code {
			t.Errorf("%s: code %d, want %d (%s)", tc.name, err.Code, tc.code, err.Message)
		}
		if tc.data != "" && normalise(t, err.Data) != tc.data {
			t.Errorf("%s: data %s, want %s", tc.name, err.Data, tc.data)
		}
	}
}

// TestEventsUnsupportedModes holds events:fallback:unsupported-modes:
// stream, subscribe and unsubscribe are answered with the SEP's
// unsupported error, naming the mode.
func TestEventsUnsupportedModes(t *testing.T) {
	h := newEventsHarness(t, nil)
	h.emitter("count", nil)
	for method, mode := range map[string]string{EventsStreamMethod: "push", EventsSubscribeMethod: "webhook", EventsUnsubscribeMethod: "webhook"} {
		_, err := h.call(method, `{"name":"count"}`)
		if err == nil || err.Code != EventsCodeUnsupported {
			t.Fatalf("%s = %v, want unsupported", method, err)
		}
		if normalise(t, err.Data) != fmt.Sprintf(`{"feature":"deliveryMode","value":%q}`, mode) {
			t.Errorf("%s: data %s", method, err.Data)
		}
	}
}

// checkFixture is a check-type event over a list of "upstream" events
// with integer positions, whose cursor is the position.
type checkFixture struct {
	mu       sync.Mutex
	upstream []Event
	replay   bool
	calls    []string
	more     bool
	trunc    bool
	nextMs   int64
	err      error
}

func (f *checkFixture) check(_ context.Context, c CheckContext, args json.RawMessage, cursor *string) (CheckResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cs := "nil"
	if cursor != nil {
		cs = *cursor
	}
	f.calls = append(f.calls, fmt.Sprintf("%s/%d/%d", cs, c.MaxEvents, c.MaxAgeMs))
	if f.err != nil {
		return CheckResult{}, f.err
	}
	pos := 0
	if cursor != nil {
		fmt.Sscan(*cursor, &pos)
	}
	if cursor == nil && f.replay {
		return CheckResult{Cursor: str(fmt.Sprint(len(f.upstream))), NextPollMs: f.nextMs}, nil
	}
	res := CheckResult{Events: f.upstream[pos:], HasMore: f.more, Truncated: f.trunc, NextPollMs: f.nextMs}
	if f.replay {
		res.Cursor = str(fmt.Sprint(len(f.upstream)))
	}
	if !f.replay {
		f.upstream = nil
	}
	return res, nil
}

func (f *checkFixture) add(id string, at time.Time) {
	f.mu.Lock()
	f.upstream = append(f.upstream, Event{ID: id, Time: at, Data: json.RawMessage(`{"n":1}`)})
	f.mu.Unlock()
}

func (h *eventsHarness) checker(f *checkFixture, mod func(*EventDef)) *EventType {
	h.t.Helper()
	def := EventDef{Name: "upstream", PayloadSchema: json.RawMessage(countSchema), Check: f.check}
	if mod != nil {
		mod(&def)
	}
	et, err := h.events.Register(def)
	if err != nil {
		h.t.Fatal(err)
	}
	return et
}

// TestEventsPollCheck holds events:poll:bootstrap and :replay for a
// check type: the first poll returns the check's position and none of
// its events, later ones replay from the cursor the author owns, and the
// limits reach the check.
func TestEventsPollCheck(t *testing.T) {
	h := newEventsHarness(t, nil)
	f := &checkFixture{replay: true}
	f.add("old", h.clock.Now())
	h.checker(f, nil)

	first := h.poll(`{"name":"upstream"}`)
	if len(first.Events) != 0 || cur(first) != "1" {
		t.Fatalf("bootstrap = %+v, want the check's position and no events", first)
	}
	f.add("a", h.clock.Now())
	f.add("b", h.clock.Now())
	got := h.poll(`{"name":"upstream","cursor":"1","maxEvents":7,"maxAgeMs":60000}`)
	if ids(got) != "a,b" || cur(got) != "3" || got.HasMore || got.Truncated {
		t.Errorf("replay = %+v", got)
	}
	if want := []string{"nil/100/0", "1/7/60000"}; strings.Join(f.calls, " ") != strings.Join(want, " ") {
		t.Errorf("checks = %v, want %v", f.calls, want)
	}

	// hasMore and truncated come from the check.
	f.more, f.trunc = true, true
	got = h.poll(`{"name":"upstream","cursor":"1"}`)
	if !got.HasMore || !got.Truncated {
		t.Errorf("flags = hasMore %v truncated %v, want both from the check", got.HasMore, got.Truncated)
	}
	f.more, f.trunc = false, false

	// maxAgeMs drops events before the floor and says truncated.
	f.add("stale", h.clock.Now().Add(-time.Hour))
	f.add("fresh", h.clock.Now())
	got = h.poll(`{"name":"upstream","cursor":"3","maxAgeMs":60000}`)
	if ids(got) != "fresh" || !got.Truncated {
		t.Errorf("max age = %+v, want fresh alone, truncated", got)
	}

	// An event with no id, or one that does not conform, is dropped and reported.
	f.mu.Lock()
	f.upstream = append(f.upstream, Event{Data: json.RawMessage(`{"n":1}`)}, Event{ID: "bad", Data: json.RawMessage(`{"n":"x"}`)}, Event{ID: "ok", Data: json.RawMessage(`{"n":1}`)})
	f.mu.Unlock()
	got = h.poll(`{"name":"upstream","cursor":"5"}`)
	if ids(got) != "ok" || len(h.errors) != 2 {
		t.Errorf("dropped = %+v, errors %v, want only ok and two reports", got, h.errors)
	}
}

// TestEventsPollNonReplay holds events:cursor:non-replay: a type whose
// check has no cursor answers null always, a first poll starts from now
// and later polls return what is new.
func TestEventsPollNonReplay(t *testing.T) {
	h := newEventsHarness(t, nil)
	f := &checkFixture{}
	f.add("before", h.clock.Now())
	h.checker(f, nil)
	first := h.poll(`{"name":"upstream"}`)
	if len(first.Events) != 0 || first.Cursor != nil {
		t.Fatalf("first = %+v, want no events and a null cursor", first)
	}
	f.add("a", h.clock.Now())
	f.trunc = true
	next := h.poll(`{"name":"upstream"}`)
	if ids(next) != "a" || next.Cursor != nil || next.Truncated {
		t.Errorf("next = %+v, want a, a null cursor and truncated false for a type that does not replay", next)
	}
}

// TestEventsNextPollMs holds events:poll:next-poll-ms: the check's wait
// or the default, at most half the lease.
func TestEventsNextPollMs(t *testing.T) {
	tests := []struct {
		name   string
		mod    func(*EventsOptions)
		checks int64
		want   int64
	}{
		{"default", nil, 0, 30000},
		{"from the check", nil, 2500, 2500},
		{"clamped to half the lease", nil, 10 * 60000, 150000},
		{"short lease", func(o *EventsOptions) { o.LeaseTTL = 10 * time.Second }, 0, 5000},
		{"configured default", func(o *EventsOptions) { o.DefaultNextPoll = 7 * time.Second }, 0, 7000},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newEventsHarness(t, tc.mod)
			f := &checkFixture{replay: true, nextMs: tc.checks}
			h.checker(f, nil)
			if got := h.poll(`{"name":"upstream"}`); got.NextPollMs != tc.want {
				t.Errorf("nextPollMs = %d, want %d", got.NextPollMs, tc.want)
			}
		})
	}
	h := newEventsHarness(t, nil)
	et := h.emitter("count", nil)
	emit(t, et, "x", 1)
	if got := h.poll(`{"name":"count"}`); got.NextPollMs != 30000 {
		t.Errorf("emit-only nextPollMs = %d, want 30000", got.NextPollMs)
	}
}

// TestEventsCheckErrors: an EventsError from a check is the error sent,
// another is reported and sent as an internal error without its text.
func TestEventsCheckErrors(t *testing.T) {
	h := newEventsHarness(t, nil)
	f := &checkFixture{replay: true, err: &EventsError{Code: EventsCodeForbidden, Message: "no", Data: map[string]any{"reason": "revoked"}}}
	h.checker(f, nil)
	if err := h.pollErr(`{"name":"upstream","cursor":"0"}`); err.Code != EventsCodeForbidden || normalise(t, err.Data) != `{"reason":"revoked"}` {
		t.Errorf("error = %+v", err)
	}
	f.err = errors.New("db password is hunter2")
	err := h.pollErr(`{"name":"upstream","cursor":"0"}`)
	if err.Code != -32603 || strings.Contains(err.Message, "hunter2") || len(h.errors) == 0 || !strings.Contains(h.errors[0].Error(), "hunter2") {
		t.Errorf("error = %+v, reported %v", err, h.errors)
	}
}

// TestEventsRegister holds go:events:register:rules.
func TestEventsRegister(t *testing.T) {
	h := newEventsHarness(t, nil)
	check := func(context.Context, CheckContext, json.RawMessage, *string) (CheckResult, error) {
		return CheckResult{}, nil
	}
	payload := json.RawMessage(countSchema)
	tests := []struct {
		name string
		def  EventDef
		want string // "" for accepted
	}{
		{"ok", EventDef{Name: "ok", PayloadSchema: payload, EmitOnly: true}, ""},
		{"check", EventDef{Name: "ok2", PayloadSchema: payload, Check: check}, ""},
		{"no name", EventDef{PayloadSchema: payload, EmitOnly: true}, "needs a name"},
		{"neither kind", EventDef{Name: "x", PayloadSchema: payload}, "neither"},
		{"both kinds", EventDef{Name: "x", PayloadSchema: payload, EmitOnly: true, Check: check}, "both"},
		{"match on check", EventDef{Name: "x", PayloadSchema: payload, Check: check, Match: func(EventContext, Event, json.RawMessage) bool { return true }}, "Match or Transform"},
		{"no payload schema", EventDef{Name: "x", EmitOnly: true}, "payload schema"},
		{"payload not an object", EventDef{Name: "x", PayloadSchema: json.RawMessage(`{"type":"string"}`), EmitOnly: true}, "root must be an object"},
		{"bad schema", EventDef{Name: "x", PayloadSchema: json.RawMessage(`{"type":"object","properties":3}`), EmitOnly: true}, "payload schema"},
		{"push", EventDef{Name: "x", PayloadSchema: payload, EmitOnly: true, Delivery: []string{"poll", "push"}}, "not served yet"},
		{"webhook only", EventDef{Name: "x", PayloadSchema: payload, EmitOnly: true, Delivery: []string{"poll", "webhook"}}, "needs EventsOptions.Webhook"},
		{"unknown mode", EventDef{Name: "x", PayloadSchema: payload, EmitOnly: true, Delivery: []string{"pigeon"}}, "unknown delivery"},
		{"header name", EventDef{Name: "x", PayloadSchema: payload, EmitOnly: true, InputSchema: json.RawMessage(`{"type":"object","properties":{"a":{"type":"string","x-mcp-header":"Bad Name"}}}`)}, "may not"},
		{"header type", EventDef{Name: "x", PayloadSchema: payload, EmitOnly: true, InputSchema: json.RawMessage(`{"type":"object","properties":{"a":{"type":"number","x-mcp-header":"A"}}}`)}, "string, integer or boolean"},
		{"header twice", EventDef{Name: "x", PayloadSchema: payload, EmitOnly: true, InputSchema: json.RawMessage(`{"type":"object","properties":{"a":{"type":"string","x-mcp-header":"A"},"b":{"type":"string","x-mcp-header":"a"}}}`)}, "twice"},
	}
	for _, tc := range tests {
		_, err := h.events.Register(tc.def)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: %v", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: error %v, want it to say %q", tc.name, err, tc.want)
		}
	}
	// The same name with the same schemas is refused.
	if _, err := h.events.Register(EventDef{Name: "ok", PayloadSchema: json.RawMessage(` { "required":["n"], "properties":{"n":{"type":"integer"}}, "type":"object" } `), EmitOnly: true}); !errors.Is(err, ErrEventExists) {
		t.Errorf("same schemas = %v, want ErrEventExists", err)
	}
	// Emit on a check type, and emit of a bad payload.
	et, _ := h.events.Register(EventDef{Name: "pulled", PayloadSchema: payload, Check: check})
	if err := et.Emit(Event{Data: json.RawMessage(`{"n":1}`)}); !errors.Is(err, ErrNoEmit) {
		t.Errorf("emit on a check type = %v", err)
	}
	em := h.events.lookup("ok")
	if err := em.Emit(Event{Data: json.RawMessage(`{"n":"x"}`)}); err == nil {
		t.Error("emit of a payload that does not conform was accepted")
	}
	if err := em.Emit(Event{}); err == nil {
		t.Error("emit with no data was accepted")
	}
	// A made-up id.
	if err := em.Emit(Event{Data: json.RawMessage(`{"n":1}`)}); err != nil {
		t.Fatal(err)
	}
	h.poll(`{"name":"ok"}`)
	got := h.poll(`{"name":"ok","cursor":"eabcd1234.0"}`)
	if len(got.Events) != 1 || !strings.HasPrefix(got.Events[0].EventID, "evt_") {
		t.Errorf("made-up id = %+v", got.Events)
	}
}

// TestEventsSchemaChange holds go:events:register:schema-change: a type
// registered again with other schemas ends the leases of the old with
// unsupported and schema_changed, once; a removed type is not found.
func TestEventsSchemaChange(t *testing.T) {
	h := newEventsHarness(t, nil)
	var unsub []string
	h.emitter("count", func(d *EventDef) {
		d.OnUnsubscribe = func(_ context.Context, c EventContext, _ json.RawMessage) error {
			unsub = append(unsub, c.Subscription)
			return nil
		}
	})
	h.poll(`{"name":"count"}`)
	et2, err := h.events.Register(EventDef{Name: "count", EmitOnly: true, PayloadSchema: json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"},"m":{"type":"integer"}}}`)})
	if err != nil {
		t.Fatal(err)
	}
	e := h.pollErr(`{"name":"count"}`)
	if e.Code != EventsCodeUnsupported || normalise(t, e.Data) != `{"feature":"payloadSchema","reason":"schema_changed"}` {
		t.Errorf("after a payload change = %+v", e)
	}
	if len(unsub) != 1 {
		t.Errorf("OnUnsubscribe called %d times, want 1", len(unsub))
	}
	if got := h.poll(`{"name":"count"}`); got.Cursor == nil {
		t.Errorf("the poll after the error starts a new lease: %+v", got)
	}
	// An input schema change.
	if _, err := h.events.Register(EventDef{Name: "count", EmitOnly: true, PayloadSchema: json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"},"m":{"type":"integer"}}}`), InputSchema: json.RawMessage(`{"type":"object","properties":{"r":{"type":"string"}}}`)}); err != nil {
		t.Fatal(err)
	}
	if e := h.pollErr(`{"name":"count"}`); normalise(t, e.Data) != `{"feature":"inputSchema","reason":"schema_changed"}` {
		t.Errorf("after an input change = %+v", e)
	}
	// Old cursors of the replaced buffer are gaps.
	if err := et2.Emit(Event{Data: json.RawMessage(`{"n":1}`)}); err == nil {
		t.Error("a replaced type's handle still emits")
	}

	// Remove.
	h.poll(`{"name":"count"}`)
	h.events.lookup("count").Remove()
	if e := h.pollErr(`{"name":"count"}`); e.Code != EventsCodeNotFound || normalise(t, e.Data) != `{"kind":"event"}` {
		t.Errorf("after Remove = %+v", e)
	}
	if n := h.events.leases.leaseCount(); n != 0 {
		t.Errorf("%d leases after Remove", n)
	}
}

// TestEventsLeases holds go:events:lease:lifecycle and go:events:lease:capacity:
// OnSubscribe on the first poll, a lease per principal and arguments
// however written, renewed by polling, ended by silence, and bounded.
func TestEventsLeases(t *testing.T) {
	var mu sync.Mutex
	var log []string
	note := func(s string) { mu.Lock(); log = append(log, s); mu.Unlock() }
	h := newEventsHarness(t, func(o *EventsOptions) {
		o.LeaseTTL = time.Minute
		o.MaxSubscriptions = 3
		o.Principal = func(extra *sdk.RequestExtra) string { return "anon" }
	})
	h.emitter("count", func(d *EventDef) {
		d.InputSchema = json.RawMessage(`{"type":"object","properties":{"a":{"type":"integer"},"b":{"type":"integer"}}}`)
		d.OnSubscribe = func(_ context.Context, c EventContext, args json.RawMessage) error {
			note("sub " + c.Principal + " " + string(args))
			if strings.Contains(string(args), `"a":99`) {
				return &EventsError{Code: EventsCodeForbidden, Message: "not for you"}
			}
			return nil
		}
		d.OnUnsubscribe = func(_ context.Context, c EventContext, args json.RawMessage) error {
			note("unsub " + string(args))
			return nil
		}
	})
	h.poll(`{"name":"count","arguments":{"a":1,"b":2}}`)
	h.poll(`{"name":"count","arguments":{"b":2,"a":1}}`) // the same subscription
	if len(log) != 1 {
		t.Fatalf("log = %v, want one subscribe for arguments in either order", log)
	}
	h.poll(`{"name":"count","arguments":{"a":2}}`)
	if len(log) != 2 {
		t.Fatalf("log = %v", log)
	}
	// Renewing keeps a lease: poll the first just before it lapses, twice.
	h.clock.Advance(50 * time.Second)
	h.poll(`{"name":"count","arguments":{"a":1,"b":2}}`)
	h.clock.Advance(50 * time.Second)
	h.poll(`{"name":"count","arguments":{"a":1,"b":2}}`)
	// The second was silent for 100s: it lapsed, and is reaped by the next poll.
	if len(log) != 3 || !strings.HasPrefix(log[2], "unsub") || !strings.Contains(log[2], `"a":2`) {
		t.Fatalf("log = %v, want the silent lease ended", log)
	}
	if n := h.events.leases.leaseCount(); n != 1 {
		t.Errorf("%d leases, want 1", n)
	}
	// A poll after a lease lapsed is a first poll again.
	h.clock.Advance(2 * time.Minute)
	h.poll(`{"name":"count","arguments":{"a":1,"b":2}}`)
	if got := log[len(log)-2:]; !strings.HasPrefix(got[0], "unsub") || !strings.HasPrefix(got[1], "sub") {
		t.Errorf("log tail = %v, want unsub then sub", got)
	}
	// A refused subscription is the error and leaves no lease.
	e := h.pollErr(`{"name":"count","arguments":{"a":99}}`)
	if e.Code != EventsCodeForbidden {
		t.Errorf("refused = %+v", e)
	}
	if n := h.events.leases.leaseCount(); n != 1 {
		t.Errorf("%d leases after a refusal, want 1", n)
	}
	// Capacity: three leases, and the fourth is exhausted before OnSubscribe.
	h.poll(`{"name":"count","arguments":{"a":5}}`)
	h.poll(`{"name":"count","arguments":{"a":6}}`)
	before := len(log)
	e = h.pollErr(`{"name":"count","arguments":{"a":7}}`)
	if e.Code != EventsCodeResourceExhausted || normalise(t, e.Data) != `{"limit":"subscriptions","max":3}` || len(log) != before {
		t.Errorf("over capacity = %+v, log grew %d", e, len(log)-before)
	}
	h.events.Close()
	if n := h.events.leases.leaseCount(); n != 0 {
		t.Errorf("%d leases after Close", n)
	}
}

// TestEventsMatchTransform holds go:events:hooks:match-transform: a match
// selects per subscription, a transform reshapes and is checked again,
// and the cursor passes what was not selected.
func TestEventsMatchTransform(t *testing.T) {
	h := newEventsHarness(t, nil)
	et := h.emitter("count", func(d *EventDef) {
		d.InputSchema = json.RawMessage(`{"type":"object","properties":{"min":{"type":"integer"}}}`)
		d.Match = func(_ EventContext, ev Event, args json.RawMessage) bool {
			var a struct{ Min int }
			var p struct{ N int }
			_ = json.Unmarshal(args, &a)
			_ = json.Unmarshal(ev.Data, &p)
			return p.N >= a.Min
		}
		d.Transform = func(_ EventContext, ev Event, _ json.RawMessage) (Event, error) {
			var p struct{ N int }
			_ = json.Unmarshal(ev.Data, &p)
			if p.N == 4 {
				return Event{Data: json.RawMessage(`{"n":"four"}`)}, nil // does not conform
			}
			if p.N == 5 {
				return Event{}, errors.New("no fives")
			}
			ev.Data = json.RawMessage(fmt.Sprintf(`{"n":%d}`, p.N*10))
			return ev, nil
		}
	})
	h.poll(`{"name":"count","arguments":{"min":2}}`)
	for i := 1; i <= 6; i++ {
		emit(t, et, fmt.Sprintf("e%d", i), i)
	}
	got := h.poll(`{"name":"count","arguments":{"min":2},"cursor":"eabcd1234.0"}`)
	if ids(got) != "e2,e3,e6" || cur(got) != "eabcd1234.6" {
		t.Errorf("selected = %q at %s, want e2,e3,e6 at the head", ids(got), cur(got))
	}
	if normalise(t, got.Events[0].Data) != `{"n":20}` {
		t.Errorf("transformed = %s", got.Events[0].Data)
	}
	if len(h.errors) != 2 {
		t.Errorf("reported %v, want the bad transform and the failed one", h.errors)
	}
	// A subscription with other arguments sees others.
	if all := h.poll(`{"name":"count","arguments":{"min":6},"cursor":"eabcd1234.0"}`); ids(all) != "e6" {
		t.Errorf("min 6 = %q", ids(all))
	}
}

// TestEventsInitialize holds the unsupported-offer path of the helper
// types: the error type's wire form.
func TestEventsErrorWire(t *testing.T) {
	e := &EventsError{Code: EventsCodeNotFound, Message: "unknown event", Data: map[string]any{"kind": "event"}}
	b, _ := json.Marshal(e.wireValue())
	if string(b) != `{"code":-32023,"message":"unknown event","data":{"kind":"event"}}` {
		t.Errorf("wire = %s", b)
	}
	for code, want := range map[int]int{EventsCodeInvalidParams: -32602, EventsCodeNotFound: -32023, EventsCodeForbidden: -32024, EventsCodeResourceExhausted: -32025, EventsCodeUnsupported: -32026, EventsCodeCallbackEndpoint: -32027, EventsCodeHeaderMismatch: -32020} {
		if code != want {
			t.Errorf("code %d, want %d", code, want)
		}
	}
	if sdk.CodeHeaderMismatch != EventsCodeHeaderMismatch {
		t.Errorf("SDK header mismatch code %d", sdk.CodeHeaderMismatch)
	}
}
