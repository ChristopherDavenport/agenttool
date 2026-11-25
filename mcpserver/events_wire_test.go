package mcpserver

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestEventsFrames holds events:stream:frames: the frames of a stream as
// the SEP shows them, byte for byte, as a golden test so a sink added
// later needs no format decision.
func TestEventsFrames(t *testing.T) {
	var got []string
	sink := frameSink{id: 7, write: func(b json.RawMessage) error { got = append(got, string(b)); return nil }}
	at := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	steps := []func() error{
		func() error { return sink.active(str("e1a2b3c4.0"), false, 30000) },
		func() error {
			return sink.event("incident.created", Event{ID: "inc-1", Time: at, Data: json.RawMessage(`{"title":"Disk full"}`), Meta: map[string]any{"k": "v"}}, str("e1a2b3c4.1"))
		},
		func() error {
			return sink.event("pulse", Event{ID: "p", Time: at, Data: json.RawMessage(`{}`)}, nil)
		},
		func() error { return sink.heartbeat(str("e1a2b3c4.1")) },
		func() error { return sink.active(str("e1a2b3c4.9"), true, 30000) },
		func() error { return sink.failure(&EventsError{Code: -32603, Message: "check failed"}) },
		func() error {
			return sink.terminated(&EventsError{Code: EventsCodeForbidden, Message: "Forbidden", Data: map[string]any{"reason": "Access revoked"}})
		},
		sink.finish,
	}
	for i, s := range steps {
		if err := s(); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	const id = `"io.modelcontextprotocol/subscriptionId":7`
	want := []string{
		`{"jsonrpc":"2.0","method":"notifications/events/active","params":{"cursor":"e1a2b3c4.0","truncated":false,"heartbeatMs":30000,"_meta":{` + id + `}}}`,
		`{"jsonrpc":"2.0","method":"notifications/events/event","params":{"eventId":"inc-1","name":"incident.created","timestamp":"2026-11-25T12:00:00Z","data":{"title":"Disk full"},"cursor":"e1a2b3c4.1","_meta":{"k":"v",` + id + `}}}`,
		`{"jsonrpc":"2.0","method":"notifications/events/event","params":{"eventId":"p","name":"pulse","timestamp":"2026-11-25T12:00:00Z","data":{},"cursor":null,"_meta":{` + id + `}}}`,
		`{"jsonrpc":"2.0","method":"notifications/events/heartbeat","params":{"cursor":"e1a2b3c4.1","_meta":{` + id + `}}}`,
		`{"jsonrpc":"2.0","method":"notifications/events/active","params":{"cursor":"e1a2b3c4.9","truncated":true,"heartbeatMs":30000,"_meta":{` + id + `}}}`,
		`{"jsonrpc":"2.0","method":"notifications/events/error","params":{"error":{"code":-32603,"message":"check failed"},"_meta":{` + id + `}}}`,
		`{"jsonrpc":"2.0","method":"notifications/events/terminated","params":{"error":{"code":-32024,"message":"Forbidden","data":{"reason":"Access revoked"}},"_meta":{` + id + `}}}`,
		`{"jsonrpc":"2.0","id":7,"result":{"resultType":"complete","_meta":{` + id + `}}}`,
	}
	if len(got) != len(want) {
		t.Fatalf("%d frames, want %d", len(got), len(want))
	}
	for i := range want {
		if normalise(t, json.RawMessage(got[i])) != normalise(t, json.RawMessage(want[i])) {
			t.Errorf("frame %d = %s\nwant %s", i, got[i], want[i])
		}
	}
	// The method names are the SEP's.
	for m, w := range map[string]string{notifyActive: "notifications/events/active", notifyEvent: "notifications/events/event", notifyHeartbeat: "notifications/events/heartbeat", notifyError: "notifications/events/error", notifyTerminated: "notifications/events/terminated"} {
		if m != w {
			t.Errorf("method %q, want %q", m, w)
		}
	}
}

// recorder is a streamSink that records the calls made on it.
type recorder struct{ calls []string }

func (r *recorder) cur(c *string) string {
	if c == nil {
		return "null"
	}
	return *c
}
func (r *recorder) active(c *string, tr bool, hb int64) error {
	r.calls = append(r.calls, "active "+r.cur(c)+" "+map[bool]string{true: "truncated", false: "ok"}[tr])
	return nil
}
func (r *recorder) event(name string, ev Event, c *string) error {
	r.calls = append(r.calls, "event "+ev.ID+" "+r.cur(c))
	return nil
}
func (r *recorder) heartbeat(c *string) error {
	r.calls = append(r.calls, "heartbeat "+r.cur(c))
	return nil
}
func (r *recorder) failure(e *EventsError) error { r.calls = append(r.calls, "failure"); return nil }
func (r *recorder) terminated(e *EventsError) error {
	r.calls = append(r.calls, "terminated")
	return nil
}
func (r *recorder) finish() error { r.calls = append(r.calls, "finish"); return nil }

var _ streamSink = (*recorder)(nil)
var _ streamSink = frameSink{}

// TestEventsFeedSink holds go:events:sink:feed: the core feeds a sink
// from the same read poll uses, independent of how frames travel, and a
// consumer woken by the ring's watcher sees events as they are emitted.
func TestEventsFeedSink(t *testing.T) {
	h := newEventsHarness(t, nil)
	et := h.emitter("count", nil)
	ec := EventContext{Name: "count", Mode: "stream"}
	args := json.RawMessage(`{}`)
	ctx := t.Context()

	woke := make(chan struct{}, 8)
	cancel := et.ring.watch(func() { woke <- struct{}{} })
	defer cancel()

	rec := &recorder{}
	b, err := et.read(ctx, ec, args, pollCall{maxEvents: 10})
	if err != nil {
		t.Fatal(err)
	}
	if err := feedSink(rec, "count", true, nil, 30000, b); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b", "c"} {
		emit(t, et, id, 1)
		<-woke
	}
	b2, err := et.read(ctx, ec, args, pollCall{cursor: b.cursor, maxEvents: 10})
	if err != nil {
		t.Fatal(err)
	}
	if err := feedSink(rec, "count", false, b.cursor, 30000, b2); err != nil {
		t.Fatal(err)
	}
	// A gap mid-stream is a fresh active with truncated, and no events.
	b3, _ := et.read(ctx, ec, args, pollCall{cursor: str("e00000000.1"), maxEvents: 10})
	if err := feedSink(rec, "count", false, nil, 30000, b3); err != nil {
		t.Fatal(err)
	}
	want := []string{"active eabcd1234.0 ok", "event a eabcd1234.0", "event b eabcd1234.0", "event c eabcd1234.3", "active eabcd1234.0 truncated"}
	// The first read was a bootstrap at 0; the later events carry the
	// cursor of the batch start until the last.
	if strings.Join(rec.calls, "|") != strings.Join(want, "|") {
		t.Errorf("sink calls = %v\nwant %v", rec.calls, want)
	}
}

// TestEventsHeaders holds events:mcp-name and events:mcp-param: the
// routing headers are checked against the params, with the -32020 error
// and its data.mismatch, and only for clients of protocol 2026-07-28.
func TestEventsHeaders(t *testing.T) {
	h := newEventsHarness(t, nil)
	h.emitter("deploy", func(d *EventDef) {
		d.InputSchema = json.RawMessage(`{"type":"object","properties":{
			"region":{"type":"string","x-mcp-header":"Region"},
			"tier":{"type":"integer","x-mcp-header":"Tier"},
			"live":{"type":"boolean","x-mcp-header":"Live"},
			"nested":{"type":"object","properties":{"zone":{"type":"string","x-mcp-header":"Zone"}}},
			"note":{"type":"string"}}}`)
	})
	hd := func(kv ...string) http.Header {
		h := http.Header{}
		h.Set(protocolHeader, "2026-07-28")
		for i := 0; i < len(kv); i += 2 {
			h.Set(kv[i], kv[i+1])
		}
		return h
	}
	const args = `{"region":"us-west1","tier":2,"live":true,"nested":{"zone":"a"},"note":"free"}`
	good := []string{"Mcp-Name", "deploy", "Mcp-Param-Region", "us-west1", "Mcp-Param-Tier", "2", "Mcp-Param-Live", "true", "Mcp-Param-Zone", "a"}
	without := func(name string) []string {
		var out []string
		for i := 0; i < len(good); i += 2 {
			if !strings.EqualFold(good[i], name) {
				out = append(out, good[i], good[i+1])
			}
		}
		return out
	}
	with := func(name, v string) []string { return append(without(name), name, v) }
	tests := []struct {
		name   string
		method string
		params string
		header http.Header
		want   string // the mismatching header, "" for accepted
	}{
		{"all agree", EventsPollMethod, args, hd(good...), ""},
		{"every named method", EventsStreamMethod, args, hd(good...), ""},
		{"stdio has no headers", EventsPollMethod, args, nil, ""},
		{"older protocol", EventsPollMethod, args, http.Header{"Mcp-Name": {"wrong"}}, ""},
		{"list has no name", EventsListMethod, `{}`, hd(), ""},
		{"name missing", EventsPollMethod, args, hd(without("Mcp-Name")...), "Mcp-Name"},
		{"name differs", EventsSubscribeMethod, args, hd(with("Mcp-Name", "other")...), "Mcp-Name"},
		{"name base64", EventsPollMethod, args, hd(with("Mcp-Name", "=?base64?ZGVwbG95?=")...), ""},
		{"name bad base64", EventsPollMethod, args, hd(with("Mcp-Name", "=?base64?%%?=")...), "Mcp-Name"},
		{"param differs", EventsUnsubscribeMethod, args, hd(with("Mcp-Param-Region", "eu-west1")...), "Mcp-Param-Region"},
		{"param missing", EventsPollMethod, args, hd(without("Mcp-Param-Region")...), "Mcp-Param-Region"},
		{"integer differs", EventsPollMethod, args, hd(with("Mcp-Param-Tier", "3")...), "Mcp-Param-Tier"},
		{"integer spelt 2.0", EventsPollMethod, args, hd(with("Mcp-Param-Tier", "2.0")...), ""},
		{"boolean differs", EventsPollMethod, args, hd(with("Mcp-Param-Live", "false")...), "Mcp-Param-Live"},
		{"nested differs", EventsPollMethod, args, hd(with("Mcp-Param-Zone", "b")...), "Mcp-Param-Zone"},
		{"param for an absent argument", EventsPollMethod, `{"tier":2,"live":true,"nested":{"zone":"a"}}`, hd(good...), "Mcp-Param-Region"},
		{"param base64", EventsPollMethod, `{"region":"zürich","tier":2,"live":true,"nested":{"zone":"a"}}`, hd(with("Mcp-Param-Region", "=?base64?esO8cmljaA==?=")...), ""},
		{"param base64 wrong", EventsPollMethod, `{"region":"zürich","tier":2,"live":true,"nested":{"zone":"a"}}`, hd(with("Mcp-Param-Region", "=?base64?AAAA?=")...), "Mcp-Param-Region"},
		{"unannotated argument needs no header", EventsPollMethod, `{"note":"x","region":"r","tier":1,"live":false,"nested":{"zone":"a"}}`, hd("Mcp-Name", "deploy", "Mcp-Param-Region", "r", "Mcp-Param-Tier", "1", "Mcp-Param-Live", "false", "Mcp-Param-Zone", "a"), ""},
		{"unknown event is left to the handler", EventsPollMethod, `{"name":"nope"}`, hd("Mcp-Name", "nope"), ""},
	}
	for _, tc := range tests {
		var p eventsParams
		raw := tc.params
		if tc.method != EventsListMethod {
			raw = `{"name":` + nameOf(tc.params, tc.name) + `,"arguments":` + tc.params + `}`
			if strings.Contains(tc.params, `"name"`) {
				raw = tc.params
			}
		}
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		err := h.events.checkHeaders(tc.method, &p, tc.header)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: refused: %v", tc.name, err)
		case tc.want != "" && err == nil:
			t.Errorf("%s: accepted, want a mismatch of %s", tc.name, tc.want)
		case err != nil:
			if err.Code != EventsCodeHeaderMismatch {
				t.Errorf("%s: code %d, want -32020", tc.name, err.Code)
			}
			m, _ := err.Data["mismatch"].(map[string]any)
			if m["header"] != tc.want {
				t.Errorf("%s: mismatch = %v, want header %s", tc.name, err.Data, tc.want)
			}
			if _, ok := m["body"]; !ok {
				t.Errorf("%s: mismatch has no body: %v", tc.name, err.Data)
			}
		}
	}
}

// nameOf is the event name a header test's params go to.
func nameOf(_ string, test string) string {
	if strings.HasPrefix(test, "unknown") {
		return `"nope"`
	}
	return `"deploy"`
}
