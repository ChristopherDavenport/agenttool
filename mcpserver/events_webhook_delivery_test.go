package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// answer returns a respond func that answers events with status, and
// verifications with their echo.
func answer(status int) func(delivery) (webhookResponse, error) {
	return func(d delivery) (webhookResponse, error) {
		if d.typ() == "verification" {
			return echoOK(d)
		}
		return webhookResponse{Status: status, Body: []byte("LEAK-body-of-the-endpoint")}, nil
	}
}

// longTTL lets a test run for hours of fake time on one grant.
func longTTL(o *EventsOptions) { o.Webhook.TTL = 48 * time.Hour }

func hostURL(i int) string { return fmt.Sprintf("https://h%d.example.com/hook", i) }

// TestEventsWebhookVerificationFailures holds
// events:webhook:verification-failure-error,
// events:webhook:error-callback-endpoint-error,
// events:webhook:verification-no-raw-endpoint-responses and
// events:webhook:verification-required-before-delivery: a callback that
// does not echo the challenge fails the subscribe with -32027 and one of
// the categories, whatever it said, and nothing is delivered to it.
func TestEventsWebhookVerificationFailures(t *testing.T) {
	wh := newWebhookHarness(t, nil)
	et := wh.emitter("count", nil)
	body := func(status int, b string) func(delivery) (webhookResponse, error) {
		return func(delivery) (webhookResponse, error) { return webhookResponse{Status: status, Body: []byte(b)}, nil }
	}
	failure := func(cat string) func(delivery) (webhookResponse, error) {
		return func(delivery) (webhookResponse, error) {
			return webhookResponse{}, &sendError{Category: cat, Err: fmt.Errorf("LEAK-cause %s", cat)}
		}
	}
	tests := []struct {
		name    string
		respond func(delivery) (webhookResponse, error)
		reason  string
	}{
		{"refused", failure(failConnection), "connection_refused"},
		{"timeout", failure(failTimeout), "timeout"},
		{"tls", failure(failTLS), "tls_error"},
		{"404", body(404, "LEAK-not-found"), "http_4xx"},
		{"410", body(410, "LEAK"), "http_4xx"},
		{"302", body(302, "LEAK"), "http_4xx"},
		{"500", body(500, "LEAK-stack-trace"), "http_5xx"},
		{"503", body(503, "LEAK"), "http_5xx"},
		{"200 with no echo", body(200, ""), "challenge_failed"},
		{"200 with an empty object", body(200, "{}"), "challenge_failed"},
		{"200 with the wrong nonce", body(200, `{"challenge":"LEAK-guess"}`), "challenge_failed"},
		{"200 with a number", body(200, `{"challenge":5}`), "challenge_failed"},
		{"200 with text", body(200, "LEAK ok"), "challenge_failed"},
		{"200 with an array", body(200, `["LEAK"]`), "challenge_failed"},
		{"204", body(204, ""), "challenge_failed"},
	}
	for i, tc := range tests {
		wh.reset()
		wh.setRespond(tc.respond)
		err := wh.subscribeErr(subParams("count", hostURL(i), secretA, ""))
		if err.Code != EventsCodeCallbackEndpoint {
			t.Errorf("%s: code %d (%s)", tc.name, err.Code, err.Message)
			continue
		}
		var data struct{ Reason string }
		_ = json.Unmarshal(err.Data, &data)
		if data.Reason != tc.reason {
			t.Errorf("%s: reason %q, want %q", tc.name, data.Reason, tc.reason)
		}
		if all := err.Message + string(err.Data); strings.Contains(all, "LEAK") || strings.Contains(all, secretA) {
			t.Errorf("%s: the error repeats the endpoint or holds the secret: %s", tc.name, all)
		}
		if wh.events.hooks.count() != 0 {
			t.Errorf("%s: a subscription is held", tc.name)
		}
	}
	// Whatever the verification answered, events emitted since went nowhere.
	emit(t, et, "e1", 1)
	wh.setRespond(nil)
	wh.settle()
	for _, d := range wh.deliveries() {
		t.Errorf("a delivery %s to a callback that did not verify", d.Body)
	}
	for _, e := range wh.reported() {
		if strings.Contains(e.Error(), secretA) {
			t.Errorf("reported %v", e)
		}
	}
}

// TestEventsWebhookVerificationCache holds
// events:webhook:verification-cached-per-principal-url and go:events:webhook:verification-failed-pair: a verified (principal,
// URL) covers every subscription of that principal to it and no one
// else's, and a failed pair is not tried again for a minute.
func TestEventsWebhookVerificationCache(t *testing.T) {
	wh := newWebhookHarness(t, nil)
	wh.emitter("count", func(d *EventDef) {
		d.InputSchema = json.RawMessage(`{"type":"object","properties":{"k":{"type":"integer"}}}`)
	})
	withArgs := func(k int) string {
		return fmt.Sprintf(`{"name":"count","arguments":{"k":%d},"delivery":{"mode":"webhook","url":%q,"secret":%q}}`, k, hookURL, secretA)
	}
	wh.subscribe(withArgs(1))
	wh.subscribe(withArgs(2))
	wh.subscribe(withArgs(3))
	if n := len(wh.verifications()); n != 1 {
		t.Errorf("%d verifications for one principal and url, want 1", n)
	}
	wh.setPrincipal("bob")
	wh.subscribe(withArgs(1))
	if n := len(wh.verifications()); n != 2 {
		t.Errorf("%d verifications after another principal, want 2: one principal's never waives another's", n)
	}
	wh.setPrincipal("alice")
	// Ending the last subscription forgets the verification.
	for k := 1; k <= 3; k++ {
		if _, err := wh.call(EventsUnsubscribeMethod, fmt.Sprintf(`{"name":"count","arguments":{"k":%d},"delivery":{"url":%q}}`, k, hookURL)); err != nil {
			t.Fatal(err)
		}
	}
	wh.subscribe(withArgs(1))
	if n := len(wh.verifications()); n != 3 {
		t.Errorf("%d verifications after the last subscription ended, want 3", n)
	}

	// A failed pair.
	wh.setRespond(answer(500))
	wh.setRespond(func(d delivery) (webhookResponse, error) { return webhookResponse{Status: 500}, nil })
	wh.reset()
	bad := subParams("count", "https://down.example.com/x", secretA, "")
	first := wh.subscribeErr(bad)
	second := wh.subscribeErr(bad)
	if first.Code != EventsCodeCallbackEndpoint || second.Code != first.Code || string(second.Data) != string(first.Data) {
		t.Errorf("failures %v, %v", first, second)
	}
	if n := len(wh.verifications()); n != 1 {
		t.Errorf("%d verifications of a failed pair within a minute, want 1", n)
	}
	wh.advance(61 * time.Second)
	wh.subscribeErr(bad)
	if n := len(wh.verifications()); n != 2 {
		t.Errorf("%d verifications after the minute, want 2", n)
	}
}

// TestEventsWebhookVerificationHostLimits holds go:events:webhook:verification-host-limits: at most ten handshakes to
// a host in 60 seconds, one at a time, and callers of one pair share a
// handshake.
func TestEventsWebhookVerificationHostLimits(t *testing.T) {
	wh := newWebhookHarness(t, nil)
	wh.emitter("count", nil)
	for i := 0; i < verifyPerWindow; i++ {
		wh.subscribe(subParams("count", fmt.Sprintf("https://one.example.com/%d", i), secretA, ""))
	}
	err := wh.subscribeErr(subParams("count", "https://one.example.com/over", secretA, ""))
	var data struct {
		Limit        string
		RetryAfterMs int64
	}
	_ = json.Unmarshal(err.Data, &data)
	if err.Code != EventsCodeResourceExhausted || data.Limit != "verification" || data.RetryAfterMs <= 0 || data.RetryAfterMs > 60000 {
		t.Errorf("the eleventh = %d %s", err.Code, err.Data)
	}
	// Another host is not affected, and the first is free after the window.
	wh.subscribe(subParams("count", "https://two.example.com/0", secretA, ""))
	wh.advance(61 * time.Second)
	wh.subscribe(subParams("count", "https://one.example.com/over", secretA, ""))

	// One at a time per host: two callbacks of a host, the first slow.
	slow := newWebhookHarness(t, nil)
	slow.emitter("count", nil)
	release := make(chan struct{})
	var mu sync.Mutex
	inFlight, peak := 0, 0
	slow.gate = func(ctx context.Context, d delivery) {
		if d.typ() != "verification" {
			return
		}
		mu.Lock()
		inFlight++
		peak = max(peak, inFlight)
		mu.Unlock()
		select {
		case <-release:
		case <-ctx.Done():
		}
		mu.Lock()
		inFlight--
		mu.Unlock()
	}
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			slow.subscribe(subParams("count", fmt.Sprintf("https://slow.example.com/%d", i), secretA, ""))
		}()
	}
	waitFor(t, func() bool { return len(slow.verifications()) >= 1 })
	time.Sleep(20 * time.Millisecond)
	if n := len(slow.verifications()); n != 1 {
		t.Errorf("%d handshakes at once to one host, want 1", n)
	}
	for i := 0; i < 3; i++ {
		release <- struct{}{}
	}
	wg.Wait()
	if peak != 1 {
		t.Errorf("peak of %d handshakes at once", peak)
	}

	// Callers of one pair share a handshake.
	shared := newWebhookHarness(t, nil)
	shared.emitter("count", func(d *EventDef) {
		d.InputSchema = json.RawMessage(`{"type":"object","properties":{"k":{"type":"integer"}}}`)
	})
	hold := make(chan struct{})
	shared.gate = func(ctx context.Context, d delivery) {
		if d.typ() == "verification" {
			select {
			case <-hold:
			case <-ctx.Done():
			}
		}
	}
	for k := 0; k < 3; k++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			shared.subscribe(fmt.Sprintf(`{"name":"count","arguments":{"k":%d},"delivery":{"mode":"webhook","url":%q,"secret":%q}}`, k, hookURL, secretA))
		}()
	}
	waitFor(t, func() bool { return len(shared.verifications()) >= 1 })
	time.Sleep(20 * time.Millisecond)
	close(hold)
	wg.Wait()
	if n := len(shared.verifications()); n != 1 {
		t.Errorf("%d handshakes for three subscribes of one pair, want 1", n)
	}
	if shared.events.hooks.count() != 3 {
		t.Errorf("%d subscriptions held", shared.events.hooks.count())
	}
}

// TestEventsWebhookRetries holds events:webhook:delivery-retries-bounded,
// events:webhook:delivery-retry-regenerates-signature and
// events:webhook:delivery-webhook-id-per-message and
// events:webhook:envelope-gap: five attempts, at 5, 30,
// 120 and 300 seconds after each failure, the webhook-id kept and the
// timestamp and signature made again, then the event is given up on.
func TestEventsWebhookRetries(t *testing.T) {
	wh := newWebhookHarness(t, longTTL)
	et := wh.emitter("count", nil)
	wh.subscribe(subParams("count", hookURL, secretA, `"ttlMs":86400000`))
	wh.setRespond(answer(503))
	start := wh.clock.Now()
	emit(t, et, "e1", 1)
	wh.advance(time.Hour)
	ds := wh.deliveries()
	if len(ds) != maxAttempts {
		t.Fatalf("%d attempts, want %d", len(ds), maxAttempts)
	}
	var offsets []time.Duration
	for _, d := range ds {
		offsets = append(offsets, d.At.Sub(start))
	}
	want := []time.Duration{0, 5 * time.Second, 35 * time.Second, 155 * time.Second, 455 * time.Second}
	if fmt.Sprint(offsets) != fmt.Sprint(want) {
		t.Errorf("attempts at %v, want %v", offsets, want)
	}
	sigs := map[string]bool{}
	for i, d := range ds {
		if d.Header.Get("webhook-id") != ds[0].Header.Get("webhook-id") {
			t.Errorf("attempt %d changed the webhook-id", i)
		}
		if !d.verifies(secretA) {
			t.Errorf("attempt %d does not verify", i)
		}
		sigs[d.Header.Get("webhook-signature")] = true
	}
	if len(sigs) != maxAttempts {
		t.Errorf("%d distinct signatures over %d attempts: each carries its own timestamp", len(sigs), maxAttempts)
	}
	// Given up: nothing more, and the refresh says there was a gap.
	if n := len(wh.deliveries()); n != maxAttempts {
		t.Errorf("%d attempts after the hour", n)
	}
	wh.setRespond(nil)
	res := wh.subscribe(subParams("count", hookURL, secretA, `"ttlMs":86400000`))
	if !res.Truncated || res.DeliveryStatus == nil || res.DeliveryStatus.LastError == nil || *res.DeliveryStatus.LastError != "http_5xx" || res.DeliveryStatus.FailedSince == nil {
		t.Errorf("refresh after giving up: %+v", res)
	}
	if res.Cursor == nil || *res.Cursor != "eabcd1234.1" {
		t.Errorf("cursor %v: the abandoned event is behind it", res.Cursor)
	}
	// The next acknowledged delivery brings the gap envelope, signed like
	// the others, with the watermark.
	wh.reset()
	emit(t, et, "e2", 2)
	wh.settle()
	ds = wh.deliveries()
	if len(ds) != 2 {
		t.Fatalf("%d deliveries, want the event and the gap envelope", len(ds))
	}
	var gap delivery
	for _, d := range ds {
		if d.typ() == "gap" {
			gap = d
		}
	}
	if gap.Body == nil || !gap.verifies(secretA) || gap.Header.Get("X-MCP-Subscription-Id") != res.ID || !strings.HasPrefix(gap.Header.Get("webhook-id"), "msg_gap_") || !messageIDForm.MatchString(gap.Header.Get("webhook-id")) {
		t.Fatalf("gap %v", gap)
	}
	var g struct {
		Type   string `json:"type"`
		Cursor string `json:"cursor"`
	}
	_ = json.Unmarshal(gap.Body, &g)
	if g.Cursor != "eabcd1234.2" || strings.Contains(string(gap.Body), "eventId") {
		t.Errorf("gap body %s", gap.Body)
	}
	if again := wh.subscribe(subParams("count", hookURL, secretA, `"ttlMs":86400000`)); again.Truncated {
		t.Error("truncated after the gap was told")
	}
}

// TestEventsWebhookFinalStatuses holds events:webhook:delivery-410-non-retryable
// and events:webhook:delivery-413-non-retryable.
func TestEventsWebhookFinalStatuses(t *testing.T) {
	for _, status := range []int{410, 413} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			wh := newWebhookHarness(t, longTTL)
			et := wh.emitter("count", nil)
			wh.subscribe(subParams("count", hookURL, secretA, `"ttlMs":86400000`))
			wh.setRespond(answer(status))
			emit(t, et, "e1", 1)
			wh.advance(time.Hour)
			if n := len(wh.deliveries()); n != 1 {
				t.Fatalf("%d attempts after a %d, want 1", n, status)
			}
			wh.setRespond(nil)
			res := wh.subscribe(subParams("count", hookURL, secretA, `"ttlMs":86400000`))
			if res.DeliveryStatus == nil || !res.DeliveryStatus.Active || res.DeliveryStatus.LastError != nil {
				t.Errorf("a %d counts as a failure of the subscription: %+v", status, res.DeliveryStatus)
			}
			// 410 is the receiver's choice and no gap; 413 is one.
			if res.Truncated != (status == 413) {
				t.Errorf("truncated %v after a %d", res.Truncated, status)
			}
			emit(t, et, "e2", 2)
			wh.advance(time.Minute)
			gaps := 0
			for _, d := range wh.deliveries() {
				if d.typ() == "gap" {
					gaps++
				}
			}
			if gaps != map[int]int{410: 0, 413: 1}[status] {
				t.Errorf("%d gap envelopes after a %d", gaps, status)
			}
		})
	}
}

// TestEventsWebhookDeliveryStatus holds
// events:webhook:delivery-status-last-error-category: a refresh says how
// delivery is going, in categories alone.
func TestEventsWebhookDeliveryStatus(t *testing.T) {
	wh := newWebhookHarness(t, nil)
	et := wh.emitter("count", nil)
	wh.subscribe(subParams("count", hookURL, secretA, `"ttlMs":86400000`))
	tests := []struct {
		name    string
		respond func(delivery) (webhookResponse, error)
		want    string
	}{
		{"404", answer(404), "http_4xx"},
		{"301", answer(301), "http_4xx"},
		{"500", answer(500), "http_5xx"},
		{"refused", func(d delivery) (webhookResponse, error) {
			return webhookResponse{}, &sendError{Category: failConnection, Err: fmt.Errorf("LEAK")}
		}, "connection_refused"},
		{"timeout", func(d delivery) (webhookResponse, error) {
			return webhookResponse{}, &sendError{Category: failTimeout, Err: fmt.Errorf("LEAK")}
		}, "timeout"},
		{"tls", func(d delivery) (webhookResponse, error) {
			return webhookResponse{}, &sendError{Category: failTLS, Err: fmt.Errorf("LEAK")}
		}, "tls_error"},
	}
	for i, tc := range tests {
		wh.setRespond(tc.respond)
		emit(t, et, fmt.Sprintf("e%d", i), i)
		wh.settle()
		res := wh.subscribe(subParams("count", hookURL, secretA, `"ttlMs":86400000`))
		st := res.DeliveryStatus
		if st == nil || st.LastError == nil || *st.LastError != tc.want || st.FailedSince == nil {
			t.Errorf("%s: %+v", tc.name, st)
		}
		raw, _ := json.Marshal(res)
		if strings.Contains(string(raw), "LEAK") {
			t.Errorf("%s: %s", tc.name, raw)
		}
	}
	// Success clears the error and the failure time.
	wh.setRespond(nil)
	wh.advance(10 * time.Minute)
	res := wh.subscribe(subParams("count", hookURL, secretA, `"ttlMs":86400000`))
	if st := res.DeliveryStatus; st == nil || st.LastError != nil || st.FailedSince != nil || st.LastDeliveryAt == nil || !st.Active {
		t.Errorf("after recovery: %+v", st)
	}
}

// TestEventsWebhookWatermark holds go:events:webhook:watermark: the
// cursor of a delivery and of a refresh never passes an event not
// acknowledged, and an event that is the oldest carries its own.
func TestEventsWebhookWatermark(t *testing.T) {
	wh := newWebhookHarness(t, nil)
	et := wh.emitter("count", nil)
	first := wh.subscribe(subParams("count", hookURL, secretA, `"ttlMs":86400000`))
	if first.Cursor == nil || *first.Cursor != "eabcd1234.0" {
		t.Fatalf("cursor %v", first.Cursor)
	}
	var mu sync.Mutex
	failA := true
	wh.setRespond(func(d delivery) (webhookResponse, error) {
		mu.Lock()
		defer mu.Unlock()
		if d.typ() == "verification" {
			return echoOK(d)
		}
		if d.eventID() == "a" && failA {
			return webhookResponse{Status: 500}, nil
		}
		return webhookResponse{Status: 200}, nil
	})
	emit(t, et, "a", 1)
	wh.settle()
	emit(t, et, "b", 2)
	emit(t, et, "c", 3)
	wh.settle()
	cursors := map[string]string{}
	for _, d := range wh.deliveries() {
		if d.eventID() != "a" {
			cursors[d.eventID()] = d.cursor()
		}
	}
	// a is at position 1 and unresolved: b and c are acknowledged but the
	// cursor stays before a.
	if cursors["b"] != "eabcd1234.0" || cursors["c"] != "eabcd1234.0" {
		t.Errorf("cursors %v, want the watermark before a", cursors)
	}
	res := wh.subscribe(subParams("count", hookURL, secretA, `"ttlMs":86400000`))
	if *res.Cursor != "eabcd1234.0" || res.Truncated {
		t.Errorf("refresh while a is pending: %+v", res)
	}
	mu.Lock()
	failA = false
	mu.Unlock()
	wh.advance(5 * time.Second)
	res = wh.subscribe(subParams("count", hookURL, secretA, `"ttlMs":86400000`))
	if *res.Cursor != "eabcd1234.3" {
		t.Errorf("refresh after a was acknowledged: cursor %s", *res.Cursor)
	}
	wh.reset()
	emit(t, et, "d", 4)
	wh.settle()
	if c := wh.deliveries()[0].cursor(); c != "eabcd1234.4" {
		t.Errorf("an event with nothing before it carries its own position, not %s", c)
	}
}

// TestEventsWebhookReplay holds go:events:webhook:replay,
// events:webhook:subscribe-response-truncated and
// events:cursor:max-age for webhook: a cursor at subscribe replays what
// the ring holds after it, a stale one is a truncated, and a refresh with
// an old cursor while live delivers nothing twice.
func TestEventsWebhookReplay(t *testing.T) {
	wh := newWebhookHarness(t, func(o *EventsOptions) { o.Buffer.MaxEvents = 5 })
	et := wh.emitter("count", nil)
	for i := 1; i <= 3; i++ {
		emit(t, et, fmt.Sprintf("e%d", i), i)
	}
	res := wh.subscribe(subParams("count", hookURL, secretA, `"cursor":"eabcd1234.1"`))
	if res.Truncated {
		t.Error("truncated for a cursor the ring holds")
	}
	wh.settle()
	var got []string
	for _, d := range wh.deliveries() {
		got = append(got, d.eventID())
	}
	slices.Sort(got)
	if fmt.Sprint(got) != "[e2 e3]" {
		t.Errorf("replayed %v, want e2 and e3", got)
	}
	// A refresh with an older cursor is a no-op for a live subscription.
	wh.reset()
	wh.subscribe(subParams("count", hookURL, secretA, `"cursor":"eabcd1234.0"`))
	wh.settle()
	if n := len(wh.deliveries()); n != 0 {
		t.Errorf("%d deliveries from a refresh with an old cursor", n)
	}
	// A cursor the ring has moved past, and one from another process.
	for i := 4; i <= 12; i++ {
		emit(t, et, fmt.Sprintf("e%d", i), i)
	}
	wh.settle()
	for name, cursor := range map[string]string{"evicted": "eabcd1234.2", "foreign": "effffffff.3", "garbage": "nonsense"} {
		url := "https://hooks.example.com/" + name
		wh.reset()
		res := wh.subscribe(subParams("count", url, secretA, `"cursor":"`+cursor+`"`))
		if !res.Truncated {
			t.Errorf("%s: not truncated", name)
		}
		wh.settle()
		n := 0
		for _, d := range wh.deliveries() {
			if d.URL == url {
				n++
			}
		}
		if n != 5 {
			t.Errorf("%s: replayed %d events, want the 5 the ring holds", name, n)
		}
	}
	// maxAgeMs drops what is older than it.
	wh2 := newWebhookHarness(t, nil)
	et2 := wh2.emitter("count", nil)
	emit(t, et2, "old", 1)
	wh2.clock.Advance(10 * time.Minute)
	emit(t, et2, "new", 2)
	res = wh2.subscribe(subParams("count", hookURL, secretA, `"cursor":"eabcd1234.0","maxAgeMs":60000`))
	wh2.settle()
	if !res.Truncated || len(wh2.deliveries()) != 1 || wh2.deliveries()[0].eventID() != "new" {
		t.Errorf("maxAgeMs: truncated %v, deliveries %d", res.Truncated, len(wh2.deliveries()))
	}
	// maxAgeMs is ignored with no cursor.
	res = wh2.subscribe(subParams("count", "https://hooks.example.com/nocursor", secretA, `"maxAgeMs":60000`))
	if res.Truncated {
		t.Error("truncated with no cursor")
	}
}

// TestEventsWebhookSuspension holds go:events:webhook:suspension and
// events:webhook:subscribe-refresh-reactivates: more
// than 95% failures in at least 100 attempts in an hour suspends
// delivery, a refresh says so and resumes it.
func TestEventsWebhookSuspension(t *testing.T) {
	wh := newWebhookHarness(t, longTTL)
	et := wh.emitter("count", nil)
	wh.subscribe(subParams("count", hookURL, secretA, `"ttlMs":86400000`))
	wh.setRespond(answer(500))
	for i := 0; i < 25; i++ {
		emit(t, et, fmt.Sprintf("e%d", i), i)
	}
	wh.advance(30 * time.Minute)
	n := len(wh.deliveries())
	if n < suspendMinAttempts || n > suspendMinAttempts+maxInFlight {
		t.Fatalf("%d attempts before suspension, want about %d", n, suspendMinAttempts)
	}
	wh.advance(10 * time.Hour)
	if got := len(wh.deliveries()); got != n {
		t.Errorf("%d attempts while suspended, want none after %d", got-n, n)
	}
	wh.setRespond(nil)
	res := wh.subscribe(subParams("count", hookURL, secretA, `"ttlMs":86400000`))
	if st := res.DeliveryStatus; st == nil || st.Active || st.LastError == nil || *st.LastError != "http_5xx" {
		t.Errorf("the refresh that ends a suspension says so: %+v", st)
	}
	wh.settle()
	acked := map[string]bool{}
	for _, d := range wh.deliveries()[n:] {
		acked[d.eventID()] = true
	}
	if len(acked) == 0 {
		t.Error("nothing was retried after the refresh")
	}
	again := wh.subscribe(subParams("count", hookURL, secretA, `"ttlMs":86400000`))
	if st := again.DeliveryStatus; st == nil || !st.Active {
		t.Errorf("still suspended after a refresh: %+v", st)
	}
}

// TestEventsWebhookSuspensionRule holds go:events:webhook:suspension on
// the rule alone.
func TestEventsWebhookSuspensionRule(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		ok, bad   int
		age       time.Duration
		suspended bool
	}{
		{"99 failures", 0, 99, 0, false},
		{"100 failures", 0, 100, 0, true},
		{"95 percent exactly", 5, 95, 0, false},
		{"96 percent", 4, 96, 0, true},
		{"a brief blip among many", 900, 100, 0, false},
		{"failures older than the window", 0, 100, suspendWindow + time.Second, false},
	}
	for _, tc := range tests {
		tr := &tracked{w: &webhookEngine{}}
		for i := 0; i < tc.ok; i++ {
			tr.history = append(tr.history, attempt{at: now.Add(-tc.age), ok: true})
		}
		for i := 0; i < tc.bad-1; i++ {
			tr.history = append(tr.history, attempt{at: now.Add(-tc.age), ok: false})
		}
		// The failure that is just recorded is the last of bad.
		tr.recordLocked(now, false)
		if got := tr.checkSuspendLocked(now); got != tc.suspended {
			t.Errorf("%s: suspended %v, want %v", tc.name, got, tc.suspended)
		}
	}
}

// TestEventsWebhookTooLarge holds events:webhook:delivery-body-size: an
// event whose body is over 256 KiB is not sent, and is a gap.
func TestEventsWebhookTooLarge(t *testing.T) {
	wh := newWebhookHarness(t, nil)
	et, err := wh.events.Register(EventDef{Name: "blob", PayloadSchema: json.RawMessage(`{"type":"object","properties":{"s":{"type":"string"}}}`), EmitOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	wh.subscribe(strings.Replace(subParams("count", hookURL, secretA, ""), `"count"`, `"blob"`, 1))
	big := fmt.Sprintf(`{"s":%q}`, strings.Repeat("x", maxBodyBytes))
	if err := et.Emit(Event{ID: "big", Data: json.RawMessage(big)}); err != nil {
		t.Fatal(err)
	}
	if err := et.Emit(Event{ID: "small", Data: json.RawMessage(`{"s":"x"}`)}); err != nil {
		t.Fatal(err)
	}
	wh.settle()
	// The gap is told after the next delivery that is acknowledged.
	if err := et.Emit(Event{ID: "later", Data: json.RawMessage(`{"s":"y"}`)}); err != nil {
		t.Fatal(err)
	}
	wh.settle()
	for _, d := range wh.deliveries() {
		if len(d.Body) > maxBodyBytes {
			t.Errorf("a body of %d bytes was sent", len(d.Body))
		}
	}
	got := map[string]bool{}
	for _, d := range wh.deliveries() {
		got[d.eventID()+d.typ()] = true
	}
	if got["big"] || !got["small"] || !got["later"] || !got["gap"] {
		t.Errorf("deliveries %v: want the small event and a gap, not the big one", got)
	}
	if len(wh.reported()) == 0 {
		t.Error("the dropped event was not reported")
	}
}

// TestEventsWebhookTerminated holds events:webhook:envelope-terminated:
// a type that is replaced with other schemas or removed ends its
// subscriptions with a signed terminated envelope, which is retried, and
// a subscribe afterwards is a new subscription.
func TestEventsWebhookTerminated(t *testing.T) {
	var unsubs int
	wh := newWebhookHarness(t, nil)
	hook := func(d *EventDef) {
		d.OnUnsubscribe = func(context.Context, EventContext, json.RawMessage) error { unsubs++; return nil }
	}
	et := wh.emitter("count", hook)
	res := wh.subscribe(subParams("count", hookURL, secretA, ""))
	wh.reset()

	changed := `{"type":"object","properties":{"n":{"type":"integer"},"extra":{"type":"string"}},"required":["n"]}`
	if _, err := wh.events.Register(EventDef{Name: "count", PayloadSchema: json.RawMessage(changed), EmitOnly: true, OnUnsubscribe: hook0(&unsubs)}); err != nil {
		t.Fatal(err)
	}
	wh.settle()
	ds := wh.deliveries()
	if len(ds) != 1 || ds[0].typ() != "terminated" {
		t.Fatalf("deliveries %d", len(ds))
	}
	d := ds[0]
	if !d.verifies(secretA) || d.Header.Get("X-MCP-Subscription-Id") != res.ID || !strings.HasPrefix(d.Header.Get("webhook-id"), "msg_terminated_") || !messageIDForm.MatchString(d.Header.Get("webhook-id")) {
		t.Errorf("headers %v", d.Header)
	}
	var b struct {
		Type  string
		Error struct {
			Code    int
			Message string
			Data    map[string]any
		}
	}
	_ = json.Unmarshal(d.Body, &b)
	if b.Error.Code != EventsCodeUnsupported || b.Error.Data["reason"] != "schema_changed" || b.Error.Data["feature"] != "payloadSchema" {
		t.Errorf("terminated body %s", d.Body)
	}
	if wh.events.hooks.count() != 0 || unsubs != 1 {
		t.Errorf("held %d, OnUnsubscribe %d", wh.events.hooks.count(), unsubs)
	}
	// The old handle emits nowhere now.
	wh.reset()
	_ = et.Emit(Event{ID: "x", Data: json.RawMessage(`{"n":1}`)})
	wh.settle()
	if len(wh.all()) != 0 {
		t.Error("a delivery to a terminated subscription")
	}
	// Unsubscribing what is gone is a not found; subscribing is new.
	if _, err := wh.call(EventsUnsubscribeMethod, `{"name":"count","delivery":{"url":"`+hookURL+`"}}`); err == nil || err.Code != EventsCodeNotFound {
		t.Errorf("unsubscribe after termination = %v", err)
	}
	if again := wh.subscribe(subParams("count", hookURL, secretA, "")); again.DeliveryStatus != nil || again.ID != res.ID {
		t.Errorf("subscribe after termination %+v", again)
	}

	// Removal.
	wh.reset()
	wh.events.lookup("count").Remove()
	wh.settle()
	ds = wh.deliveries()
	if len(ds) != 1 || ds[0].typ() != "terminated" || !strings.Contains(string(ds[0].Body), fmt.Sprint(EventsCodeNotFound)) {
		t.Errorf("removal: %v", ds)
	}
	// A terminated envelope the endpoint does not take is retried, then dropped.
	wh2 := newWebhookHarness(t, nil)
	wh2.emitter("count", nil)
	wh2.subscribe(subParams("count", hookURL, secretA, ""))
	wh2.setRespond(answer(500))
	wh2.reset()
	wh2.events.lookup("count").Remove()
	wh2.advance(time.Hour)
	if n := len(wh2.deliveries()); n != maxAttempts {
		t.Errorf("%d attempts at a terminated envelope, want %d", n, maxAttempts)
	}
	wh2.events.hooks.mu.Lock()
	left := len(wh2.events.hooks.all)
	wh2.events.hooks.mu.Unlock()
	if left != 0 || wh2.timers.pending() != 0 {
		t.Errorf("after the envelope was given up: %d tracked, %d timers", left, wh2.timers.pending())
	}
}

// TestEventsWebhookEmbedderControls holds events:webhook:envelope-gap and
// events:webhook:envelope-terminated for the embedder's calls: GapWebhook
// sends one subscription a signed gap envelope with a fresh cursor and
// leaves it live, TerminateWebhook sends the given cause (or forbidden) and
// ends it, and both refuse an id that is not there.
func TestEventsWebhookEmbedderControls(t *testing.T) {
	var unsubs int
	wh := newWebhookHarness(t, nil)
	et := wh.emitter("count", func(d *EventDef) { d.OnUnsubscribe = hook0(&unsubs) })
	other := wh.subscribe(subParams("count", "https://hook.example.com/other", secretA, ""))
	res := wh.subscribe(subParams("count", hookURL, secretA, ""))
	wh.reset()

	if err := wh.events.GapWebhook("nope"); !errors.Is(err, ErrWebhookNotFound) {
		t.Errorf("gap of an unknown id = %v", err)
	}
	if err := wh.events.GapWebhook(res.ID); err != nil {
		t.Fatal(err)
	}
	wh.settle()
	ds := wh.deliveries()
	if len(ds) != 1 || ds[0].typ() != "gap" || !ds[0].verifies(secretA) || ds[0].Header.Get("X-MCP-Subscription-Id") != res.ID {
		t.Fatalf("after a gap: %v", ds)
	}
	var g struct{ Cursor string }
	_ = json.Unmarshal(ds[0].Body, &g)
	if g.Cursor != "eabcd1234.0" {
		t.Errorf("gap cursor %q", g.Cursor)
	}
	// Still live, and the other subscription was not told.
	wh.reset()
	emit(t, et, "x", 1)
	wh.settle()
	if n := len(wh.deliveries()); n != 2 {
		t.Errorf("%d deliveries after a gap, want the event to both", n)
	}

	wh.reset()
	cause := &EventsError{Code: EventsCodeNotFound, Message: "revoked", Data: map[string]any{"kind": "subscription"}}
	if err := wh.events.TerminateWebhook(res.ID, cause); err != nil {
		t.Fatal(err)
	}
	wh.settle()
	ds = wh.deliveries()
	if len(ds) != 1 || ds[0].typ() != "terminated" || !strings.Contains(string(ds[0].Body), "revoked") || ds[0].Header.Get("X-MCP-Subscription-Id") != res.ID {
		t.Fatalf("after terminate: %v", ds)
	}
	if unsubs != 1 || wh.events.hooks.count() != 1 {
		t.Errorf("OnUnsubscribe %d, held %d", unsubs, wh.events.hooks.count())
	}
	if err := wh.events.TerminateWebhook(res.ID, nil); !errors.Is(err, ErrWebhookNotFound) {
		t.Errorf("second terminate = %v", err)
	}
	if err := wh.events.GapWebhook(res.ID); !errors.Is(err, ErrWebhookNotFound) {
		t.Errorf("gap after terminate = %v", err)
	}
	// A nil cause is forbidden.
	wh.reset()
	if err := wh.events.TerminateWebhook(other.ID, nil); err != nil {
		t.Fatal(err)
	}
	wh.settle()
	if ds = wh.deliveries(); len(ds) != 1 || !strings.Contains(string(ds[0].Body), fmt.Sprint(EventsCodeForbidden)) {
		t.Errorf("default cause: %v", ds)
	}
	// Off without the webhook mode.
	h := newEventsHarness(t, nil)
	if h.events.TerminateWebhook("x", nil) == nil || h.events.GapWebhook("x") == nil {
		t.Error("controls worked with the webhook mode off")
	}
}

func hook0(n *int) func(context.Context, EventContext, json.RawMessage) error {
	return func(context.Context, EventContext, json.RawMessage) error { *n++; return nil }
}

// TestEventsWebhookClose holds go:events:webhook:close: Close cuts off
// requests in flight, cancels timers and waits for the goroutines, so
// none is left, and nothing is delivered after it.
func TestEventsWebhookClose(t *testing.T) {
	wh := newWebhookHarness(t, nil)
	et := wh.emitter("count", nil)
	wh.subscribe(subParams("count", hookURL, secretA, ""))
	wh.settle()
	before := runtime.NumGoroutine()

	// Some attempts block until cut off, some wait for a retry.
	wh.setRespond(answer(500))
	emit(t, et, "retrying", 1)
	wh.settle()
	blocked := make(chan struct{}, 4)
	wh.mu.Lock()
	wh.gate = func(ctx context.Context, d delivery) {
		blocked <- struct{}{}
		<-ctx.Done()
	}
	wh.mu.Unlock()
	for i := 0; i < 3; i++ {
		emit(t, et, fmt.Sprintf("stuck%d", i), i)
	}
	for i := 0; i < 3; i++ {
		<-blocked
	}
	if wh.timers.pending() == 0 {
		t.Fatal("no retry timer is waiting")
	}
	done := make(chan struct{})
	go func() { wh.events.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return with requests in flight")
	}
	if !wh.events.hooks.idleForTest() || wh.timers.pending() != 0 {
		t.Errorf("after Close: busy %d, timers %d", wh.events.hooks.busy.Load(), wh.timers.pending())
	}
	waitFor(t, func() bool { return runtime.NumGoroutine() <= before })

	wh.setRespond(nil)
	wh.mu.Lock()
	wh.gate = nil
	wh.mu.Unlock()
	wh.reset()
	emit(t, et, "after", 9)
	wh.advance(time.Hour)
	if len(wh.all()) != 0 {
		t.Error("a request after Close")
	}
	if err := wh.subscribeErr(subParams("count", "https://hooks.example.com/late", secretA, "")); err.Code != -32603 {
		t.Errorf("subscribe after Close = %d", err.Code)
	}
	wh.events.Close()
}

// TestEventsWebhookConcurrentEmit holds go:events:webhook:concurrent-emit:
// many emitters, every event delivered once, under the race detector.
func TestEventsWebhookConcurrentEmit(t *testing.T) {
	wh := newWebhookHarness(t, nil)
	et := wh.emitter("count", nil)
	wh.subscribe(subParams("count", hookURL, secretA, ""))
	wh.subscribe(subParams("count", "https://hooks.example.com/second", secretA, ""))
	const workers, each = 8, 25
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				_ = et.Emit(Event{ID: fmt.Sprintf("w%d-%d", w, i), Data: json.RawMessage(`{"n":1}`)})
			}
		}()
	}
	wg.Wait()
	wh.settle()
	seen := map[string]int{}
	for _, d := range wh.deliveries() {
		seen[d.URL+" "+d.eventID()]++
	}
	if len(seen) != 2*workers*each {
		t.Errorf("%d distinct deliveries, want %d", len(seen), 2*workers*each)
	}
	for k, n := range seen {
		if n != 1 {
			t.Errorf("%s delivered %d times", k, n)
		}
	}
	res := wh.subscribe(subParams("count", hookURL, secretA, ""))
	if res.Cursor == nil || *res.Cursor != fmt.Sprintf("eabcd1234.%d", workers*each) {
		t.Errorf("watermark %v after every delivery was acknowledged", res.Cursor)
	}
}

// TestEventsWebhookStore holds go:events:webhook:store: the default map
// store, a store shared by two servers, and the single winner of a delete.
func TestEventsWebhookStore(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryWebhookStore()
	rec := WebhookRecord{ID: "sub_1", Name: "n", Arguments: json.RawMessage(`{"a":1}`), URL: "u", Secret: "s"}
	if _, ok, _ := s.Get(ctx, "sub_1"); ok {
		t.Error("an empty store has a record")
	}
	if err := s.Put(ctx, rec); err != nil {
		t.Fatal(err)
	}
	rec.Name = "m"
	rec.ID = "sub_2"
	_ = s.Put(ctx, rec)
	got, ok, _ := s.Get(ctx, "sub_1")
	if !ok || got.Name != "n" {
		t.Errorf("get %+v", got)
	}
	got.Arguments[0] = 'X'
	if again, _, _ := s.Get(ctx, "sub_1"); string(again.Arguments) != `{"a":1}` {
		t.Error("a record read can change the stored one")
	}
	if l, _ := s.ListByName(ctx, "n"); len(l) != 1 || l[0].ID != "sub_1" {
		t.Errorf("list %v", l)
	}
	if ok, _ := s.Delete(ctx, "sub_1"); !ok {
		t.Error("delete of a record said false")
	}
	if ok, _ := s.Delete(ctx, "sub_1"); ok {
		t.Error("a second delete said true")
	}

	// Two servers on one store: the second recovers a subscription the
	// first made, verifies its callback itself, and a record deleted under
	// a live subscription ends it.
	shared := NewMemoryWebhookStore()
	one := newWebhookHarness(t, func(o *EventsOptions) { o.Webhook.Store = shared })
	one.emitter("count", nil)
	first := one.subscribe(subParams("count", hookURL, secretA, ""))
	two := newWebhookHarness(t, func(o *EventsOptions) { o.Webhook.Store = shared })
	et2 := two.emitter("count", nil)
	second := two.subscribe(subParams("count", hookURL, secretA, ""))
	if second.ID != first.ID || second.DeliveryStatus != nil {
		t.Errorf("recovered subscription %+v", second)
	}
	if len(two.verifications()) != 1 {
		t.Error("the second server did not verify the callback it took over")
	}
	emit(t, et2, "e", 1)
	two.settle()
	if len(two.deliveries()) != 1 {
		t.Error("the second server did not deliver")
	}
	if _, err := two.call(EventsUnsubscribeMethod, `{"name":"count","delivery":{"url":"`+hookURL+`"}}`); err != nil {
		t.Fatal(err)
	}
	two.reset()
	// The first still has it live; its next refresh finds the record gone
	// and starts afresh.
	third := one.subscribe(subParams("count", hookURL, secretA, ""))
	if third.DeliveryStatus != nil {
		t.Errorf("a refresh of a deleted subscription: %+v", third)
	}
	if one.events.hooks.count() != 1 {
		t.Errorf("%d held", one.events.hooks.count())
	}
}

// TestEventsWebhookExpiryRace holds go:events:webhook:store: of replicas
// that expire a record, the one whose delete finds it runs OnUnsubscribe.
func TestEventsWebhookExpiryRace(t *testing.T) {
	var unsubs int
	lost := &losingStore{WebhookStore: NewMemoryWebhookStore()}
	wh := newWebhookHarness(t, func(o *EventsOptions) { o.Webhook.Store = lost })
	wh.emitter("count", func(d *EventDef) {
		d.OnUnsubscribe = func(context.Context, EventContext, json.RawMessage) error { unsubs++; return nil }
	})
	wh.subscribe(subParams("count", hookURL, secretA, `"ttlMs":60000`))
	lost.lose.Store(true)
	wh.advance(2 * time.Minute)
	if unsubs != 0 {
		t.Errorf("OnUnsubscribe ran %d times for a delete another replica won", unsubs)
	}
	if wh.events.hooks.count() != 0 {
		t.Error("the subscription is still delivered")
	}
}
