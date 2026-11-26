package mcpclient

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
)

// Requirement ids of this file: events:webhook:<rule> are the SEP's
// rules, named as in the server's tests; go:events:webhook:<rule> are
// this package's own.

// TestEventsWebhookSecretMade holds go:events:webhook:secret-made: a
// secret is whsec_ and 32 random bytes, and no two are alike.
func TestEventsWebhookSecretMade(t *testing.T) {
	a, err := NewWebhookSecret()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewWebhookSecret()
	key, derr := decodeSecret(a)
	if derr != nil || len(key) != 32 || a == b || !strings.HasPrefix(a, "whsec_") {
		t.Errorf("secret %q (%d bytes, %v), next %q", a, len(key), derr, b)
	}
}

func subResult(id, refresh, cursor string, extra func(*fakeSubResult)) func(map[string]json.RawMessage) (*fakeSubResult, error) {
	return func(map[string]json.RawMessage) (*fakeSubResult, error) {
		r := &fakeSubResult{ResultType: "complete", ID: id}
		if refresh != "" {
			r.RefreshBefore = &refresh
		}
		if cursor != "" {
			r.Cursor = &cursor
		}
		if extra != nil {
			extra(r)
		}
		return r, nil
	}
}

// TestEventsSubscribeWire holds events:webhook:subscribe-secret-required,
// events:webhook:ttl-omitted-means-default and
// events:webhook:ttl-null-only-when-requested on the client's side: what
// is sent, and how the answer is read.
func TestEventsSubscribeWire(t *testing.T) {
	ctx := context.Background()
	f := &fakeEvents{pages: [][]map[string]any{{desc("n", []string{"poll", "webhook"}, "")}}}
	f.subScript = []func(map[string]json.RawMessage) (*fakeSubResult, error){
		subResult("sub_1", "2026-11-25T13:00:00Z", "c1", func(r *fakeSubResult) {
			r.Truncated = true
			r.DeliveryStatus = map[string]any{"active": false, "lastDeliveryAt": "2026-11-25T12:30:00Z", "lastError": "http_5xx", "failedSince": "2026-11-25T12:40:00Z"}
		}),
		subResult("sub_1", "", "", nil),
		subResult("sub_1", "2026-11-25T13:00:00Z", "", nil),
		subResult("sub_1", "2026-11-25T13:00:00Z", "", nil),
	}
	s := eventsRemote(t, f)
	secret, _ := NewWebhookSecret()
	cursor := "c0"
	res, err := s.SubscribeEvents(ctx, SubscribeParams{Name: "n", Arguments: json.RawMessage(`{"a":1}`), URL: "https://h.example.com/x", Secret: secret, Cursor: &cursor, MaxAge: time.Minute, TTL: 10 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if res.ID != "sub_1" || res.RefreshBefore == nil || !res.RefreshBefore.Equal(time.Date(2026, 11, 25, 13, 0, 0, 0, time.UTC)) || res.Cursor == nil || *res.Cursor != "c1" || !res.Truncated {
		t.Errorf("result %+v", res)
	}
	st := res.DeliveryStatus
	if st == nil || st.Active || st.LastError != "http_5xx" || st.LastDeliveryAt.IsZero() || st.FailedSince.IsZero() {
		t.Errorf("delivery status %+v", st)
	}
	first := f.subs[0]
	if string(first["name"]) != `"n"` || string(first["arguments"]) != `{"a":1}` || string(first["cursor"]) != `"c0"` || string(first["maxAgeMs"]) != "60000" || string(first["ttlMs"]) != "600000" {
		t.Errorf("params %v", first)
	}
	var d map[string]string
	_ = json.Unmarshal(first["delivery"], &d)
	if d["mode"] != "webhook" || d["url"] != "https://h.example.com/x" || d["secret"] != secret {
		t.Errorf("delivery %v", d)
	}

	// No expiry is asked with a null; a grant of null is read as none.
	res, err = s.SubscribeEvents(ctx, SubscribeParams{Name: "n", URL: "https://h.example.com/x", Secret: secret, NoExpiry: true})
	if err != nil || res.RefreshBefore != nil || res.DeliveryStatus != nil {
		t.Errorf("no-expiry result %+v, %v", res, err)
	}
	if v, ok := f.subs[1]["ttlMs"]; !ok || string(v) != "null" {
		t.Errorf("ttlMs %q (present %v), want null", v, ok)
	}
	if string(f.subs[1]["cursor"]) != "null" {
		t.Errorf("cursor %s, want null", f.subs[1]["cursor"])
	}
	// No TTL: no ttlMs.
	if _, err = s.SubscribeEvents(ctx, SubscribeParams{Name: "n", URL: "https://h.example.com/x", Secret: secret}); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.subs[2]["ttlMs"]; ok {
		t.Error("a ttlMs was sent for none")
	}
	if _, err := s.SubscribeEvents(ctx, SubscribeParams{Name: "n", URL: "u", Secret: secret, NoExpiry: true, TTL: time.Minute}); err == nil {
		t.Error("TTL and NoExpiry together were accepted")
	}

	// Unsubscribe sends the key, no secret and no mode.
	if err := s.UnsubscribeEvents(ctx, "n", json.RawMessage(`{"a":1}`), "https://h.example.com/x"); err != nil {
		t.Fatal(err)
	}
	u := f.unsubs[0]
	if string(u["name"]) != `"n"` || string(u["arguments"]) != `{"a":1}` || strings.Contains(string(u["delivery"]), "secret") || strings.Contains(string(u["delivery"]), "mode") || !strings.Contains(string(u["delivery"]), "h.example.com") {
		t.Errorf("unsubscribe params %v", u)
	}
	f.unsubErr = &jsonrpc.Error{Code: EventsCodeNotFound, Message: "no such subscription"}
	var ee *EventError
	if err := s.UnsubscribeEvents(ctx, "n", nil, "u"); !errors.As(err, &ee) || ee.Code != EventsCodeNotFound {
		t.Errorf("unsubscribe of nothing = %v", err)
	}

	// Gating: without the extension, nothing is sent.
	off := &fakeEvents{pages: [][]map[string]any{{}}}
	off.offer = false
	s2 := connect(t, off.server(t), WithEvents())
	if _, err := s2.SubscribeEvents(ctx, SubscribeParams{Name: "n"}); !errors.Is(err, ErrEventsNotOffered) {
		t.Errorf("subscribe = %v", err)
	}
	if err := s2.UnsubscribeEvents(ctx, "n", nil, "u"); !errors.Is(err, ErrEventsNotOffered) {
		t.Errorf("unsubscribe = %v", err)
	}
	if len(off.subs)+len(off.unsubs) != 0 {
		t.Error("requests reached a server that did not offer the extension")
	}
}

// TestEventsSubscribeErrors: an error code comes back as an EventError
// the caller branches on, with the reason of a callback error in Data.
func TestEventsSubscribeErrors(t *testing.T) {
	f := &fakeEvents{pages: [][]map[string]any{{desc("n", []string{"webhook"}, "")}}}
	f.subScript = []func(map[string]json.RawMessage) (*fakeSubResult, error){
		func(map[string]json.RawMessage) (*fakeSubResult, error) {
			return nil, &jsonrpc.Error{Code: EventsCodeCallbackEndpoint, Message: "the callback endpoint did not verify", Data: json.RawMessage(`{"reason":"challenge_failed"}`)}
		},
	}
	s := eventsRemote(t, f)
	_, err := s.SubscribeEvents(context.Background(), SubscribeParams{Name: "n", URL: "https://x", Secret: "whsec_x"})
	var ee *EventError
	if !errors.As(err, &ee) || ee.Code != EventsCodeCallbackEndpoint || !strings.Contains(string(ee.Data), "challenge_failed") {
		t.Errorf("error %v", err)
	}
}

type keepHarness struct {
	waits []time.Duration
	f     *fakeEvents
	now   time.Time
	// stop is called when the script is used up.
	cancel context.CancelFunc
}

func (h *keepHarness) run(t *testing.T, sub Subscription) error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	defer cancel()
	s := eventsRemote(t, h.f)
	sub.Now = func() time.Time { return h.now }
	sub.Jitter = func() float64 { return 0.5 }
	sub.Sleep = func(ctx context.Context, d time.Duration) error {
		h.waits = append(h.waits, d)
		h.now = h.now.Add(d)
		h.f.mu.Lock()
		done := len(h.f.subScript) == 0
		h.f.mu.Unlock()
		if done {
			cancel()
		}
		return ctx.Err()
	}
	return s.KeepSubscribed(ctx, sub)
}

// TestEventsKeepSubscribed holds events:webhook:ttl-refresh-before-lte-suggestion
// and go:events:webhook:keep: the subscription is refreshed part of the
// way through each grant, with the jitter, the last cursor and the
// grant the server made, and a refusal that will not pass ends the loop.
func TestEventsKeepSubscribed(t *testing.T) {
	start := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) string { return start.Add(d).Format(time.RFC3339) }
	h := &keepHarness{now: start, f: &fakeEvents{pages: [][]map[string]any{{desc("n", []string{"webhook"}, "")}}}}
	h.f.subScript = []func(map[string]json.RawMessage) (*fakeSubResult, error){
		subResult("sub_1", at(time.Hour), "w1", nil),      // wait 0.65 of an hour
		subResult("sub_1", at(79*time.Minute), "w2", nil), // 40 minutes left at the next: 0.65 of it
	}
	var results []string
	var client *string
	cur := "client-1"
	client = &cur
	err := h.run(t, Subscription{
		Params:   SubscribeParams{Name: "n", URL: "https://h/x", Secret: "whsec_x", TTL: time.Hour},
		Cursor:   func() *string { return client },
		OnResult: func(r *SubscribeResult) { results = append(results, *r.Cursor) },
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error %v", err)
	}
	if fmt.Sprint(results) != "[w1 w2]" {
		t.Errorf("results %v", results)
	}
	want := []time.Duration{39 * time.Minute, 26 * time.Minute}
	if len(h.waits) != 2 || h.waits[0] != want[0] || h.waits[1] != want[1] {
		t.Errorf("waits %v, want %v", h.waits, want)
	}
	// The first subscribe has the params' cursor (none); the second, the
	// one the client kept.
	if string(h.f.subs[0]["cursor"]) != "null" || string(h.f.subs[1]["cursor"]) != `"client-1"` {
		t.Errorf("cursors %s, %s", h.f.subs[0]["cursor"], h.f.subs[1]["cursor"])
	}

	// With no cursor of the client's, the server's watermark is sent.
	h = &keepHarness{now: start, f: &fakeEvents{pages: [][]map[string]any{{desc("n", []string{"webhook"}, "")}}}}
	h.f.subScript = []func(map[string]json.RawMessage) (*fakeSubResult, error){
		subResult("sub_1", at(time.Hour), "w1", nil), subResult("sub_1", at(2*time.Hour), "w2", nil),
	}
	_ = h.run(t, Subscription{Params: SubscribeParams{Name: "n", URL: "u", Secret: "whsec_x"}})
	if string(h.f.subs[1]["cursor"]) != `"w1"` {
		t.Errorf("second cursor %s, want the watermark", h.f.subs[1]["cursor"])
	}

	// Errors that may pass are retried with a growing wait; others end it.
	h = &keepHarness{now: start, f: &fakeEvents{pages: [][]map[string]any{{desc("n", []string{"webhook"}, "")}}}}
	busy := func(map[string]json.RawMessage) (*fakeSubResult, error) {
		return nil, &jsonrpc.Error{Code: EventsCodeResourceExhausted, Message: "busy"}
	}
	h.f.subScript = []func(map[string]json.RawMessage) (*fakeSubResult, error){busy, busy, busy, subResult("sub_1", at(10*time.Hour), "w", nil)}
	var reported int
	_ = h.run(t, Subscription{Params: SubscribeParams{Name: "n", URL: "u", Secret: "whsec_x"}, OnError: func(error) { reported++ }, Retry: time.Second})
	if reported != 3 || len(h.waits) < 3 || h.waits[0] != time.Second || h.waits[1] != 2*time.Second || h.waits[2] != 4*time.Second {
		t.Errorf("reported %d, waits %v", reported, h.waits)
	}
	for _, code := range []int64{EventsCodeForbidden, EventsCodeInvalidParams, EventsCodeNotFound, EventsCodeUnsupported, EventsCodeCallbackEndpoint} {
		h = &keepHarness{now: start, f: &fakeEvents{pages: [][]map[string]any{{desc("n", []string{"webhook"}, "")}}}}
		h.f.subScript = []func(map[string]json.RawMessage) (*fakeSubResult, error){func(map[string]json.RawMessage) (*fakeSubResult, error) {
			return nil, &jsonrpc.Error{Code: code, Message: "no"}
		}}
		var ee *EventError
		if err := h.run(t, Subscription{Params: SubscribeParams{Name: "n", URL: "u", Secret: "whsec_x"}}); !errors.As(err, &ee) || ee.Code != int(code) || len(h.waits) != 0 {
			t.Errorf("code %d: %v, waits %v", code, err, h.waits)
		}
	}

	// A grant without expiry is still refreshed now and then.
	h = &keepHarness{now: start, f: &fakeEvents{pages: [][]map[string]any{{desc("n", []string{"webhook"}, "")}}}}
	h.f.subScript = []func(map[string]json.RawMessage) (*fakeSubResult, error){subResult("sub_1", "", "w", nil)}
	_ = h.run(t, Subscription{Params: SubscribeParams{Name: "n", URL: "u", Secret: "whsec_x"}, Idle: 2 * time.Hour})
	if len(h.waits) != 1 || h.waits[0] != 2*time.Hour {
		t.Errorf("idle waits %v", h.waits)
	}
	// A grant that is nearly over is refreshed at once, never negative.
	h = &keepHarness{now: start, f: &fakeEvents{pages: [][]map[string]any{{desc("n", []string{"webhook"}, "")}}}}
	h.f.subScript = []func(map[string]json.RawMessage) (*fakeSubResult, error){subResult("sub_1", at(-time.Minute), "w", nil)}
	_ = h.run(t, Subscription{Params: SubscribeParams{Name: "n", URL: "u", Secret: "whsec_x"}})
	if len(h.waits) != 1 || h.waits[0] < time.Second {
		t.Errorf("waits for a grant in the past %v", h.waits)
	}
	// A cancelled context ends it with the context's error.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := eventsRemote(t, &fakeEvents{pages: [][]map[string]any{{}}})
	if err := s.KeepSubscribed(ctx, Subscription{Params: SubscribeParams{Name: "n"}}); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
	if cryptoJitter() < 0 || cryptoJitter() >= 1 {
		t.Error("jitter out of range")
	}
}

// signer signs deliveries as a server would.
type signer struct {
	secret string
	id     string
	now    time.Time
}

func (s signer) post(t *testing.T, h http.Handler, msgID string, body []byte, mod func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	key, err := decodeSecret(s.secret)
	if err != nil {
		t.Fatal(err)
	}
	ts := strconv.FormatInt(s.now.Unix(), 10)
	m := hmac.New(sha256.New, key)
	m.Write([]byte(msgID + "." + ts + "."))
	m.Write(body)
	req := httptest.NewRequest(http.MethodPost, "/hook", bytes.NewReader(body))
	req.Header.Set("webhook-id", msgID)
	req.Header.Set("webhook-timestamp", ts)
	req.Header.Set("webhook-signature", "v1,"+base64.StdEncoding.EncodeToString(m.Sum(nil)))
	req.Header.Set("X-MCP-Subscription-Id", s.id)
	req.Header.Set("Content-Type", "application/json")
	if mod != nil {
		mod(req)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

type receiverFixture struct {
	h       http.Handler
	secrets *MemorySecrets
	now     time.Time
	mu      sync.Mutex
	got     []WebhookMessage
	fail    error
}

func newReceiver(t *testing.T, mod func(*WebhookReceiverOptions)) (*receiverFixture, string) {
	t.Helper()
	secret, err := NewWebhookSecret()
	if err != nil {
		t.Fatal(err)
	}
	rf := &receiverFixture{secrets: NewMemorySecrets(), now: time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)}
	rf.secrets.Add(secret)
	o := WebhookReceiverOptions{
		Secrets: rf.secrets, Now: func() time.Time { return rf.now },
		Handle: func(_ context.Context, m WebhookMessage) error {
			rf.mu.Lock()
			defer rf.mu.Unlock()
			if rf.fail != nil {
				return rf.fail
			}
			rf.got = append(rf.got, m)
			return nil
		},
	}
	if mod != nil {
		mod(&o)
	}
	rf.h = NewWebhookReceiver(o)
	return rf, secret
}

func (rf *receiverFixture) messages() []WebhookMessage {
	rf.mu.Lock()
	defer rf.mu.Unlock()
	return append([]WebhookMessage(nil), rf.got...)
}

const eventBody = `{"eventId":"evt_1","name":"n","timestamp":"2026-11-25T11:59:00Z","data":{"k":"v"},"cursor":"c9"}`

// bound answers the challenge so the subscription id is known.
func (rf *receiverFixture) bound(t *testing.T, secret, id string) signer {
	t.Helper()
	s := signer{secret: secret, id: id, now: rf.now}
	rec := s.post(t, rf.h, "msg_verification_x", []byte(`{"type":"verification","challenge":"abc"}`), nil)
	if rec.Code != 200 {
		t.Fatalf("verification = %d", rec.Code)
	}
	return s
}

// TestEventsWebhookReceiver holds events:webhook:receiver-verify-signature,
// events:webhook:receiver-timestamp-freshness,
// events:webhook:receiver-dedup-webhook-id,
// events:webhook:receiver-dedup-event-per-subscription,
// events:webhook:receiver-forwards-control,
// events:webhook:receiver-verification-echo and
// events:webhook:receiver-unknown-id-retryable: the receiver verifies the
// signature over the raw body before it acts, refuses stale messages,
// deduplicates, forwards events, gaps and terminations with their
// cursors, and answers the challenge.
func TestEventsWebhookReceiver(t *testing.T) {
	rf, secret := newReceiver(t, nil)
	s := signer{secret: secret, id: "sub_A", now: rf.now}

	// The challenge: echoed, not forwarded, and binds the id.
	rec := s.post(t, rf.h, "msg_verification_1", []byte(`{"type":"verification","challenge":"n0nce-_"}`), nil)
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"challenge":"n0nce-_"}` || rec.Header().Get("Content-Type") != "application/json" {
		t.Errorf("challenge = %d %q", rec.Code, rec.Body)
	}
	if len(rf.messages()) != 0 {
		t.Error("a verification was forwarded")
	}
	if ss, ok := rf.secrets.SecretsFor("sub_A"); !ok || len(ss) != 1 || ss[0] != secret {
		t.Errorf("secrets for the id: %v %v", ss, ok)
	}

	// An event.
	rec = s.post(t, rf.h, "msg_1", []byte(eventBody), nil)
	if rec.Code != 200 {
		t.Fatalf("event = %d %s", rec.Code, rec.Body)
	}
	m := rf.messages()
	if len(m) != 1 || m[0].Kind != WebhookEvent || m[0].SubscriptionID != "sub_A" || m[0].MessageID != "msg_1" || m[0].Cursor == nil || *m[0].Cursor != "c9" ||
		m[0].Event.EventID != "evt_1" || m[0].Event.Name != "n" || string(m[0].Event.Data) != `{"k":"v"}` || m[0].Event.Timestamp.IsZero() {
		t.Errorf("forwarded %+v", m)
	}

	// The same webhook-id again is acknowledged and not forwarded; the same
	// event under another webhook-id, too; another subscription's is not.
	if rec = s.post(t, rf.h, "msg_1", []byte(eventBody), nil); rec.Code != 200 || len(rf.messages()) != 1 {
		t.Errorf("repeat of a message: %d, %d forwarded", rec.Code, len(rf.messages()))
	}
	if rec = s.post(t, rf.h, "msg_2", []byte(eventBody), nil); rec.Code != 200 || len(rf.messages()) != 1 {
		t.Errorf("repeat of an event: %d, %d forwarded", rec.Code, len(rf.messages()))
	}
	other := rf.bound(t, secret, "sub_B")
	if rec = other.post(t, rf.h, "msg_3", []byte(eventBody), nil); rec.Code != 200 || len(rf.messages()) != 2 {
		t.Errorf("the same event on another subscription: %d, %d forwarded", rec.Code, len(rf.messages()))
	}

	// Control envelopes are forwarded with what they carry.
	if rec = s.post(t, rf.h, "msg_gap_1", []byte(`{"type":"gap","cursor":"fresh"}`), nil); rec.Code != 200 {
		t.Fatalf("gap = %d", rec.Code)
	}
	if rec = s.post(t, rf.h, "msg_terminated_1", []byte(`{"type":"terminated","error":{"code":-32026,"message":"changed","data":{"reason":"schema_changed"}}}`), nil); rec.Code != 200 {
		t.Fatalf("terminated = %d", rec.Code)
	}
	m = rf.messages()
	gap, term := m[2], m[3]
	if gap.Kind != WebhookGap || *gap.Cursor != "fresh" || gap.Event != nil {
		t.Errorf("gap %+v", gap)
	}
	if term.Kind != WebhookTerminated || term.Error == nil || term.Error.Code != EventsCodeUnsupported || !strings.Contains(string(term.Error.Data), "schema_changed") {
		t.Errorf("terminated %+v", term)
	}
	// An envelope type from a later revision is acknowledged and dropped.
	before := len(rf.messages())
	if rec = s.post(t, rf.h, "msg_new_1", []byte(`{"type":"future"}`), nil); rec.Code != 200 || len(rf.messages()) != before {
		t.Errorf("unknown envelope: %d, %d forwarded", rec.Code, len(rf.messages())-before)
	}
	// A null cursor, for a type that does not replay, is nil.
	if rec = s.post(t, rf.h, "msg_nc", []byte(`{"eventId":"evt_2","name":"n","timestamp":"2026-11-25T11:59:00Z","data":{},"cursor":null}`), nil); rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	if last := rf.messages()[len(rf.messages())-1]; last.Cursor != nil {
		t.Errorf("cursor %v", last.Cursor)
	}
}

// TestEventsWebhookReceiverRefusals holds
// events:webhook:receiver-verify-signature,
// events:webhook:receiver-timestamp-freshness and
// events:webhook:receiver-unknown-id-retryable: what the receiver turns
// away, and that it never reaches Handle.
func TestEventsWebhookReceiverRefusals(t *testing.T) {
	rf, secret := newReceiver(t, nil)
	s := rf.bound(t, secret, "sub_A")
	wrong, _ := NewWebhookSecret()
	body := []byte(eventBody)
	tests := []struct {
		name string
		run  func() *httptest.ResponseRecorder
		want int
	}{
		{"wrong secret", func() *httptest.ResponseRecorder {
			return signer{secret: wrong, id: "sub_A", now: rf.now}.post(t, rf.h, "m1", body, nil)
		}, 401},
		{"body changed", func() *httptest.ResponseRecorder {
			return s.post(t, rf.h, "m2", body, func(r *http.Request) {
				r.Body = http.NoBody
				r.Body = nopCloser(bytes.NewReader(append([]byte(" "), body...)))
			})
		}, 401},
		{"reserialized body", func() *httptest.ResponseRecorder {
			var v any
			_ = json.Unmarshal(body, &v)
			re, _ := json.Marshal(v)
			return s.post(t, rf.h, "m3", body, func(r *http.Request) { r.Body = nopCloser(bytes.NewReader(re)) })
		}, 401},
		{"id changed", func() *httptest.ResponseRecorder {
			return s.post(t, rf.h, "m4", body, func(r *http.Request) { r.Header.Set("webhook-id", "m4x") })
		}, 401},
		{"timestamp changed", func() *httptest.ResponseRecorder {
			return s.post(t, rf.h, "m5", body, func(r *http.Request) {
				r.Header.Set("webhook-timestamp", strconv.FormatInt(rf.now.Unix()+1, 10))
			})
		}, 401},
		{"no signature", func() *httptest.ResponseRecorder {
			return s.post(t, rf.h, "m6", body, func(r *http.Request) { r.Header.Del("webhook-signature") })
		}, 400},
		{"junk signature", func() *httptest.ResponseRecorder {
			return s.post(t, rf.h, "m7", body, func(r *http.Request) { r.Header.Set("webhook-signature", "v1,!!!") })
		}, 401},
		{"v1a alone", func() *httptest.ResponseRecorder {
			return s.post(t, rf.h, "m8", body, func(r *http.Request) {
				r.Header.Set("webhook-signature", strings.Replace(r.Header.Get("webhook-signature"), "v1,", "v1a,", 1))
			})
		}, 401},
		{"no id header", func() *httptest.ResponseRecorder {
			return s.post(t, rf.h, "m9", body, func(r *http.Request) { r.Header.Del("webhook-id") })
		}, 400},
		{"no subscription header", func() *httptest.ResponseRecorder {
			return s.post(t, rf.h, "m10", body, func(r *http.Request) { r.Header.Del("X-MCP-Subscription-Id") })
		}, 400},
		{"bad timestamp", func() *httptest.ResponseRecorder {
			return s.post(t, rf.h, "m11", body, func(r *http.Request) { r.Header.Set("webhook-timestamp", "yesterday") })
		}, 400},
		{"stale", func() *httptest.ResponseRecorder {
			return signer{secret: secret, id: "sub_A", now: rf.now.Add(-5*time.Minute - time.Second)}.post(t, rf.h, "m12", body, nil)
		}, 400},
		{"from the future", func() *httptest.ResponseRecorder {
			return signer{secret: secret, id: "sub_A", now: rf.now.Add(5*time.Minute + time.Second)}.post(t, rf.h, "m13", body, nil)
		}, 400},
		{"not json", func() *httptest.ResponseRecorder { return s.post(t, rf.h, "m14", []byte("not json"), nil) }, 400},
		{"too large", func() *httptest.ResponseRecorder {
			return s.post(t, rf.h, "m15", bytes.Repeat([]byte("x"), defaultWebhookBody+1), nil)
		}, 413},
		{"get", func() *httptest.ResponseRecorder {
			rec := httptest.NewRecorder()
			rf.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/hook", nil))
			return rec
		}, 405},
		{"id nobody told of", func() *httptest.ResponseRecorder {
			return signer{secret: secret, id: "sub_unknown", now: rf.now}.post(t, rf.h, "m16", body, nil)
		}, 503},
		{"unknown id and unknown secret", func() *httptest.ResponseRecorder {
			return signer{secret: wrong, id: "sub_unknown", now: rf.now}.post(t, rf.h, "m17", body, nil)
		}, 401},
		{"challenge with an unknown secret", func() *httptest.ResponseRecorder {
			return signer{secret: wrong, id: "sub_unknown", now: rf.now}.post(t, rf.h, "m18", []byte(`{"type":"verification","challenge":"x"}`), nil)
		}, 401},
	}
	for _, tc := range tests {
		if rec := tc.run(); rec.Code != tc.want {
			t.Errorf("%s: %d, want %d (%s)", tc.name, rec.Code, tc.want, strings.TrimSpace(rec.Body.String()))
		}
	}
	if n := len(rf.messages()); n != 0 {
		t.Errorf("%d messages reached Handle", n)
	}
	if _, ok := rf.secrets.SecretsFor("sub_unknown"); ok {
		t.Error("an id was bound by a challenge nobody signed")
	}
	// The tolerance is the SEP's five minutes, to the second.
	if rec := (signer{secret: secret, id: "sub_A", now: rf.now.Add(-5 * time.Minute)}).post(t, rf.h, "edge", body, nil); rec.Code != 200 {
		t.Errorf("exactly five minutes old = %d", rec.Code)
	}
}

type nopCloserT struct{ *bytes.Reader }

func (nopCloserT) Close() error { return nil }

func nopCloser(r *bytes.Reader) nopCloserT { return nopCloserT{r} }

// TestEventsWebhookReceiverRotation holds events:webhook:delivery-dual-sign-on-rotation
// on the receiver's side: a header with two signatures verifies if either
// does, and a delivery is verified with each secret of its subscription.
func TestEventsWebhookReceiverRotation(t *testing.T) {
	rf, oldSecret := newReceiver(t, nil)
	s := rf.bound(t, oldSecret, "sub_A")
	newSecret, _ := NewWebhookSecret()
	rf.secrets.Bind("sub_A", newSecret)

	body := []byte(eventBody)
	sign := func(secret, id string) string {
		key, _ := decodeSecret(secret)
		m := hmac.New(sha256.New, key)
		m.Write([]byte(id + "." + strconv.FormatInt(rf.now.Unix(), 10) + "."))
		m.Write(body)
		return "v1," + base64.StdEncoding.EncodeToString(m.Sum(nil))
	}
	for i, hdr := range []string{
		sign(oldSecret, "r1") + " " + sign(newSecret, "r1"),
		sign(newSecret, "r2"),
		"v1,AAAA " + sign(oldSecret, "r3"),
	} {
		id := fmt.Sprintf("r%d", i+1)
		rec := s.post(t, rf.h, id, append([]byte(nil), body...), func(r *http.Request) {
			r.Header.Set("webhook-signature", hdr)
			// A different event id each, so deduplication is not what is tested.
		})
		if rec.Code != 200 && rec.Code != 401 {
			t.Fatalf("%s: %d", id, rec.Code)
		}
		if rec.Code != 200 {
			t.Errorf("header %d: %d", i, rec.Code)
		}
	}
	// A signature of the wrong message among valid ones does not help.
	rec := s.post(t, rf.h, "r9", body, func(r *http.Request) { r.Header.Set("webhook-signature", "v1,AAAA "+sign(oldSecret, "other")) })
	if rec.Code != 401 {
		t.Errorf("a signature of another message = %d", rec.Code)
	}
}

// TestEventsWebhookReceiverHandle holds events:webhook:receiver-dedup-webhook-id
// and go:events:webhook:receiver-retry: a delivery Handle refuses is
// answered with 500 and not remembered, so the retry, with the same
// webhook-id, is handled; one in progress is not handled twice at once.
func TestEventsWebhookReceiverHandle(t *testing.T) {
	rf, secret := newReceiver(t, nil)
	s := rf.bound(t, secret, "sub_A")
	rf.fail = errors.New("disk full")
	rec := s.post(t, rf.h, "msg_1", []byte(eventBody), nil)
	if rec.Code != 500 || strings.Contains(rec.Body.String(), "disk full") {
		t.Errorf("refused delivery = %d %q", rec.Code, rec.Body)
	}
	rf.mu.Lock()
	rf.fail = nil
	rf.mu.Unlock()
	if rec = s.post(t, rf.h, "msg_1", []byte(eventBody), nil); rec.Code != 200 || len(rf.messages()) != 1 {
		t.Errorf("retry = %d, %d handled", rec.Code, len(rf.messages()))
	}

	// One in flight: the second copy is told to come back.
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	slow, secret2 := newReceiver(t, func(o *WebhookReceiverOptions) {
		o.Handle = func(context.Context, WebhookMessage) error {
			once.Do(func() { close(entered) })
			<-release
			return nil
		}
	})
	s2 := slow.bound(t, secret2, "sub_A")
	done := make(chan int)
	go func() { done <- s2.post(t, slow.h, "msg_1", []byte(eventBody), nil).Code }()
	<-entered
	if rec := s2.post(t, slow.h, "msg_1", []byte(eventBody), nil); rec.Code != 503 {
		t.Errorf("a copy in progress = %d", rec.Code)
	}
	close(release)
	if code := <-done; code != 200 {
		t.Errorf("the first = %d", code)
	}
}
