package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Requirement ids of this file: events:webhook:<rule> are the rules the
// conformance scenarios events-webhook and events-webhook-delivery
// (modelcontextprotocol/conformance#521, #541) grade, named as the
// sep-3415-<rule> ids there; go:events:webhook:<rule> are this
// package's own.

var messageIDForm = regexp.MustCompile(`^msg_[A-Za-z0-9_-]+$`)

// TestEventsWebhookSignatureVector holds events:webhook:delivery-signature-formula
// against the test vector of the Standard Webhooks specification, so an
// off-the-shelf verifier accepts what this package signs.
func TestEventsWebhookSignatureVector(t *testing.T) {
	key, err := parseWebhookSecret("whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw")
	if err != nil {
		t.Fatal(err)
	}
	got := signWebhook([][]byte{key}, "msg_p5jXN8AQM9LWM0D4loKWxJek", 1614265330, []byte(`{"test": 2432232314}`))
	if want := "v1,g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE="; got != want {
		t.Errorf("signature %q, want %q", got, want)
	}
	two := signWebhook([][]byte{key, key}, "i", 1, []byte("b"))
	if parts := strings.Fields(two); len(parts) != 2 || parts[0] != parts[1] || !strings.HasPrefix(parts[0], "v1,") {
		t.Errorf("two keys sign as %q", two)
	}
}

// TestEventsWebhookSecretForm holds events:webhook:subscribe-secret-format.
func TestEventsWebhookSecretForm(t *testing.T) {
	bytesOf := func(n int) string { return webhookSecretPrefix + base64Std(make([]byte, n)) }
	tests := []struct {
		name   string
		secret string
		ok     bool
	}{
		{"24 bytes", bytesOf(24), true},
		{"64 bytes", bytesOf(64), true},
		{"unpadded", strings.TrimRight(bytesOf(25), "="), true},
		{"23 bytes", bytesOf(23), false},
		{"65 bytes", bytesOf(65), false},
		{"no prefix", base64Std(make([]byte, 32)), false},
		{"wrong prefix", "whsek_" + base64Std(make([]byte, 32)), false},
		{"not base64", "whsec_!!!!", false},
		{"empty", "", false},
		{"prefix alone", "whsec_", false},
	}
	for _, tc := range tests {
		_, err := parseWebhookSecret(tc.secret)
		if (err == nil) != tc.ok {
			t.Errorf("%s: error %v, ok %v", tc.name, err, tc.ok)
		}
		if err != nil && tc.secret != "" && strings.Contains(err.Error(), tc.secret) {
			t.Errorf("%s: the error holds the secret", tc.name)
		}
	}
}

// TestEventsWebhookSubscribeAndDeliver holds
// events:webhook:subscribe-response-cursor,
// events:webhook:subscribe-refresh-before-present,
// events:webhook:ttl-omitted-means-default,
// events:webhook:subscribe-id-derived,
// events:webhook:verification-challenge-echo,
// events:webhook:verification-required-before-delivery,
// events:webhook:delivery-post-json,
// events:webhook:delivery-standard-webhooks-headers,
// events:webhook:delivery-signature-formula,
// events:webhook:delivery-subscription-id-header,
// events:webhook:delivery-webhook-id-per-message,
// events:webhook:envelope-signed-like-deliveries,
// events:webhook:envelope-webhook-id-format and
// events:webhook:envelope-type-discriminator: a subscription is made
// once its callback echoes a signed challenge, and an emitted event is
// delivered, signed, with the watermark.
func TestEventsWebhookSubscribeAndDeliver(t *testing.T) {
	wh := newWebhookHarness(t, nil)
	et := wh.emitter("count", nil)

	list := wh.listDelivery(t, "count")
	if list != "poll,webhook" {
		t.Errorf("delivery = %s, want poll,webhook", list)
	}
	emit(t, et, "before", 0)

	res := wh.subscribe(subParams("count", hookURL, secretA, ""))
	if res.ResultType != "complete" || !strings.HasPrefix(res.ID, "sub_") || res.ID != subscriptionID("alice", hookURL, "count", json.RawMessage(`{}`)) {
		t.Errorf("result %+v", res)
	}
	if res.RefreshBefore == nil || *res.RefreshBefore != wh.clock.Now().Add(time.Hour).Format(time.RFC3339) {
		t.Errorf("refreshBefore %v, want an hour from now", res.RefreshBefore)
	}
	if res.Cursor == nil || *res.Cursor != "eabcd1234.1" || res.Truncated || res.DeliveryStatus != nil {
		t.Errorf("cursor %v truncated %v status %v", res.Cursor, res.Truncated, res.DeliveryStatus)
	}

	// The handshake: one signed verification envelope, the new id in its
	// header, and no event delivered for what was emitted before.
	vs := wh.verifications()
	if len(vs) != 1 {
		t.Fatalf("%d verifications, want 1", len(vs))
	}
	v := vs[0]
	if !v.verifies(secretA) || v.Header.Get("X-MCP-Subscription-Id") != res.ID || !regexp.MustCompile(`^msg_verification_[A-Za-z0-9_-]{20,}$`).MatchString(v.Header.Get("webhook-id")) {
		t.Errorf("verification headers %v", v.Header)
	}
	var vb struct{ Type, Challenge string }
	if err := json.Unmarshal(v.Body, &vb); err != nil || vb.Type != "verification" || len(vb.Challenge) < 32 {
		t.Errorf("verification body %s", v.Body)
	}
	wh.settle()
	if n := len(wh.deliveries()); n != 0 {
		t.Fatalf("%d deliveries from before the subscription", n)
	}

	emit(t, et, "evt-1", 1)
	emit(t, et, "evt-2", 2)
	wh.settle()
	ds := wh.deliveries()
	if len(ds) != 2 {
		t.Fatalf("%d deliveries, want 2", len(ds))
	}
	ids := map[string]bool{}
	for _, d := range ds {
		h := d.Header
		if h.Get("Content-Type") != "application/json" || h.Get("X-MCP-Subscription-Id") != res.ID {
			t.Errorf("headers %v", h)
		}
		id := h.Get("webhook-id")
		if !messageIDForm.MatchString(id) || strings.Contains(id, d.eventID()) || ids[id] {
			t.Errorf("webhook-id %q of event %s", id, d.eventID())
		}
		ids[id] = true
		if ts, _ := strconv.ParseInt(h.Get("webhook-timestamp"), 10, 64); ts != wh.clock.Now().Unix() {
			t.Errorf("webhook-timestamp %q", h.Get("webhook-timestamp"))
		}
		if !d.verifies(secretA) || !strings.HasPrefix(h.Get("webhook-signature"), "v1,") {
			t.Errorf("signature %q does not verify", h.Get("webhook-signature"))
		}
		if d.typ() != "" {
			t.Errorf("an event has the type %q", d.typ())
		}
	}
	var body struct {
		EventID   string          `json:"eventId"`
		Name      string          `json:"name"`
		Timestamp string          `json:"timestamp"`
		Data      json.RawMessage `json:"data"`
		Cursor    string          `json:"cursor"`
	}
	first, second := ds[0], ds[1]
	if first.eventID() != "evt-1" {
		first, second = second, first
	}
	if err := json.Unmarshal(first.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body.EventID != "evt-1" || body.Name != "count" || string(body.Data) != `{"n":1}` || body.Cursor != "eabcd1234.2" {
		t.Errorf("body %s", first.Body)
	}
	if _, err := time.Parse(time.RFC3339Nano, body.Timestamp); err != nil {
		t.Errorf("timestamp %q", body.Timestamp)
	}
	// Both are delivered at once. The second's cursor is the watermark
	// before the first when that was not yet acknowledged, and its own
	// position when it was: never ahead of an event not acknowledged.
	if c := second.cursor(); c != "eabcd1234.1" && c != "eabcd1234.3" {
		t.Errorf("second cursor %s", c)
	}
}

func (wh *webhookHarness) listDelivery(t *testing.T, name string) string {
	t.Helper()
	raw, err := wh.call(EventsListMethod, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Events []struct {
			Name     string   `json:"name"`
			Delivery []string `json:"delivery"`
		} `json:"events"`
	}
	if jerr := json.Unmarshal(raw, &out); jerr != nil {
		t.Fatal(jerr)
	}
	for _, e := range out.Events {
		if e.Name == name {
			return strings.Join(e.Delivery, ",")
		}
	}
	return ""
}

// TestEventsWebhookSubscribeParams holds events:webhook:subscribe-url-https-required,
// events:webhook:subscribe-url-non-https-rejected,
// events:webhook:subscribe-secret-required,
// events:webhook:subscribe-secret-rejected,
// events:webhook:subscribe-webhook-only,
// events:webhook:ssrf-validate-callback-url and events:error-codes: what
// events/subscribe refuses, and with which code.
func TestEventsWebhookSubscribeParams(t *testing.T) {
	wh := newWebhookHarness(t, nil)
	wh.emitter("count", nil)
	wh.emitter("pollonly", func(d *EventDef) { d.Delivery = []string{"poll"} })
	wh.emitter("withargs", func(d *EventDef) {
		d.InputSchema = json.RawMessage(`{"type":"object","properties":{"k":{"type":"string"}},"required":["k"],"additionalProperties":false}`)
	})
	bad := func(url string) string { return subParams("count", url, secretA, "") }
	tests := []struct {
		name   string
		params string
		code   int
	}{
		{"http", bad("http://hooks.example.com/x"), EventsCodeInvalidParams},
		{"no scheme", bad("hooks.example.com/x"), EventsCodeInvalidParams},
		{"relative", bad("/hook"), EventsCodeInvalidParams},
		{"ftp", bad("ftp://hooks.example.com/x"), EventsCodeInvalidParams},
		{"javascript", bad("javascript:alert(1)"), EventsCodeInvalidParams},
		{"userinfo", bad("https://user:pw@hooks.example.com/x"), EventsCodeInvalidParams},
		{"user only", bad("https://user@hooks.example.com/x"), EventsCodeInvalidParams},
		{"no host", bad("https:///x"), EventsCodeInvalidParams},
		{"bad port", bad("https://hooks.example.com:99999/x"), EventsCodeInvalidParams},
		{"port zero", bad("https://hooks.example.com:0/x"), EventsCodeInvalidParams},
		{"too long", bad("https://hooks.example.com/" + strings.Repeat("a", 2048)), EventsCodeInvalidParams},
		{"loopback literal", bad("https://127.0.0.1/x"), EventsCodeInvalidParams},
		{"loopback with port", bad("https://127.0.0.1:8443/x"), EventsCodeInvalidParams},
		{"ipv6 loopback", bad("https://[::1]/x"), EventsCodeInvalidParams},
		{"ipv6 unique local", bad("https://[fd00::1]:8443/x"), EventsCodeInvalidParams},
		{"ipv6 link local zone", bad("https://[fe80::1%25eth0]/x"), EventsCodeInvalidParams},
		{"ipv4 mapped loopback", bad("https://[::ffff:127.0.0.1]/x"), EventsCodeInvalidParams},
		{"metadata", bad("https://169.254.169.254/latest"), EventsCodeInvalidParams},
		{"private", bad("https://10.1.2.3/x"), EventsCodeInvalidParams},
		{"cgnat", bad("https://100.64.0.1/x"), EventsCodeInvalidParams},
		{"zero", bad("https://0.0.0.0/x"), EventsCodeInvalidParams},
		{"no secret", fmt.Sprintf(`{"name":"count","delivery":{"mode":"webhook","url":%q}}`, hookURL), EventsCodeInvalidParams},
		{"secret 23 bytes", subParams("count", hookURL, webhookSecretPrefix+base64Std(make([]byte, 23)), ""), EventsCodeInvalidParams},
		{"secret no prefix", subParams("count", hookURL, base64Std(make([]byte, 32)), ""), EventsCodeInvalidParams},
		{"no delivery", `{"name":"count"}`, EventsCodeInvalidParams},
		{"delivery not an object", `{"name":"count","delivery":"x"}`, EventsCodeInvalidParams},
		{"no mode", fmt.Sprintf(`{"name":"count","delivery":{"url":%q,"secret":%q}}`, hookURL, secretA), EventsCodeInvalidParams},
		{"mode push", fmt.Sprintf(`{"name":"count","delivery":{"mode":"push","url":%q,"secret":%q}}`, hookURL, secretA), EventsCodeUnsupported},
		{"mode poll", fmt.Sprintf(`{"name":"count","delivery":{"mode":"poll","url":%q,"secret":%q}}`, hookURL, secretA), EventsCodeUnsupported},
		{"no name", subParams("", hookURL, secretA, ""), EventsCodeInvalidParams},
		{"unknown event", subParams("nope", hookURL, secretA, ""), EventsCodeNotFound},
		{"poll-only event", subParams("pollonly", hookURL, secretA, ""), EventsCodeUnsupported},
		{"arguments fail the schema", subParams("withargs", hookURL, secretA, ""), EventsCodeInvalidParams},
		{"cursor not a string", subParams("count", hookURL, secretA, `"cursor":5`), EventsCodeInvalidParams},
		{"maxAgeMs negative", subParams("count", hookURL, secretA, `"cursor":"x","maxAgeMs":-1`), EventsCodeInvalidParams},
		{"ttlMs a string", subParams("count", hookURL, secretA, `"ttlMs":"1h"`), EventsCodeInvalidParams},
		{"ttlMs fractional", subParams("count", hookURL, secretA, `"ttlMs":1.5`), EventsCodeInvalidParams},
	}
	for _, tc := range tests {
		err := wh.subscribeErr(tc.params)
		if err.Code != int64(tc.code) {
			t.Errorf("%s: code %d (%s), want %d", tc.name, err.Code, err.Message, tc.code)
		}
		if strings.Contains(err.Message, secretA) || strings.Contains(string(err.Data), secretA) {
			t.Errorf("%s: the error holds the secret", tc.name)
		}
	}
	if n := len(wh.all()); n != 0 {
		t.Errorf("%d requests were made for refused subscriptions", n)
	}
	// Unsubscribe applies the same URL rules to what it is given: a
	// refusal is a not found, since no such subscription can exist.
	if _, err := wh.call(EventsUnsubscribeMethod, `{"name":"count"}`); err == nil || err.Code != EventsCodeInvalidParams {
		t.Errorf("unsubscribe without a delivery = %v", err)
	}
	for _, e := range wh.reported() {
		if strings.Contains(e.Error(), secretA) {
			t.Errorf("reported %v", e)
		}
	}
}

// TestEventsWebhookNoPrincipal holds events:webhook:subscribe-cross-tenant-isolation
// in part: with no principal, subscribe and unsubscribe are refused with
// forbidden, whatever else is wrong with them, and nothing is sent. A
// server without a Principal at all serves no webhook.
func TestEventsWebhookNoPrincipal(t *testing.T) {
	wh := newWebhookHarness(t, nil)
	wh.emitter("count", nil)
	wh.setPrincipal("")
	for _, params := range []string{subParams("count", hookURL, secretA, ""), `{"name":"count"}`, `{}`, subParams("nope", "http://x", "bad", "")} {
		if err := wh.subscribeErr(params); err.Code != EventsCodeForbidden {
			t.Errorf("subscribe %s = %d, want forbidden", params, err.Code)
		}
	}
	if _, err := wh.call(EventsUnsubscribeMethod, `{"name":"count","delivery":{"url":"`+hookURL+`"}}`); err == nil || err.Code != EventsCodeForbidden {
		t.Errorf("unsubscribe = %v, want forbidden", err)
	}
	if n := len(wh.all()); n != 0 {
		t.Errorf("%d requests", n)
	}

	// No Principal function at all.
	h2 := newEventsHarness(t, func(o *EventsOptions) {
		o.Webhook = &WebhookOptions{send: func(context.Context, webhookRequest) (webhookResponse, error) {
			return webhookResponse{Status: 200}, nil
		}}
	})
	t.Cleanup(h2.events.Close)
	h2.emitter("count", nil)
	if _, err := h2.call(EventsSubscribeMethod, subParams("count", hookURL, secretA, "")); err == nil || err.Code != EventsCodeForbidden {
		t.Errorf("subscribe without a Principal = %v, want forbidden", err)
	}
}

// TestEventsWebhookTTL holds events:webhook:ttl-omitted-means-default,
// events:webhook:ttl-null-only-when-requested,
// events:webhook:ttl-refresh-before-lte-suggestion and
// events:webhook:ttl-no-rejection-path: every grant is finite, a short
// suggestion is raised to the floor, a long one lowered to the cap.
func TestEventsWebhookTTL(t *testing.T) {
	wh := newWebhookHarness(t, func(o *EventsOptions) { o.Webhook.TTL = 2 * time.Hour })
	wh.emitter("count", nil)
	tests := []struct {
		name  string
		extra string
		want  time.Duration
	}{
		{"omitted", ``, 2 * time.Hour},
		{"null requests no expiry and is granted a finite one", `"ttlMs":null`, 2 * time.Hour},
		{"suggestion under the cap", `"ttlMs":600000`, 10 * time.Minute},
		{"suggestion over the cap", `"ttlMs":99999999999`, 2 * time.Hour},
		{"suggestion under the floor", `"ttlMs":1000`, time.Minute},
		{"zero", `"ttlMs":0`, time.Minute},
		{"exactly the floor", `"ttlMs":60000`, time.Minute},
	}
	for i, tc := range tests {
		url := fmt.Sprintf("https://hooks.example.com/ttl/%d", i)
		res := wh.subscribe(subParams("count", url, secretA, tc.extra))
		if res.RefreshBefore == nil {
			t.Errorf("%s: refreshBefore is null, which a client may not be granted unless it asked, and then not by this server", tc.name)
			continue
		}
		got, err := time.Parse(time.RFC3339, *res.RefreshBefore)
		if err != nil || got.Sub(wh.clock.Now()) != tc.want {
			t.Errorf("%s: refreshBefore %s, want %v from now", tc.name, *res.RefreshBefore, tc.want)
		}
	}
	// A cap under the floor caps the floor.
	short := newWebhookHarness(t, func(o *EventsOptions) { o.Webhook.TTL = 10 * time.Second })
	short.emitter("count", nil)
	res := short.subscribe(subParams("count", hookURL, secretA, `"ttlMs":1000`))
	if got, _ := time.Parse(time.RFC3339, *res.RefreshBefore); got.Sub(short.clock.Now()) > 10*time.Second {
		t.Errorf("a 10 s cap granted %v", got.Sub(short.clock.Now()))
	}
}

// TestEventsWebhookIdentity holds events:webhook:subscribe-key-composition,
// events:webhook:subscribe-key-immutable,
// events:webhook:subscribe-id-derived,
// events:webhook:subscribe-id-not-an-input,
// events:webhook:subscribe-idempotent-upsert,
// events:webhook:subscribe-cross-tenant-isolation,
// events:webhook:unsubscribe-by-key,
// events:webhook:unsubscribe-empty-result and
// events:webhook:unsubscribe-unknown-not-found: the key is (principal,
// url, name, arguments), arguments compared as canonical JSON, and
// nothing else addresses a subscription.
func TestEventsWebhookIdentity(t *testing.T) {
	wh := newWebhookHarness(t, nil)
	wh.emitter("count", func(d *EventDef) {
		d.InputSchema = json.RawMessage(`{"type":"object","properties":{"a":{"type":"integer"},"b":{"type":"integer"}}}`)
	})
	wh.emitter("other", nil)
	with := func(name, url, args string) string {
		return fmt.Sprintf(`{"name":%q,"arguments":%s,"delivery":{"mode":"webhook","url":%q,"secret":%q}}`, name, args, url, secretA)
	}
	a := wh.subscribe(with("count", hookURL, `{"a":1,"b":2}`))
	again := wh.subscribe(with("count", hookURL, `{ "b": 2, "a": 1 }`))
	if a.ID != again.ID {
		t.Errorf("the same arguments in another order are %s and %s", a.ID, again.ID)
	}
	if again.DeliveryStatus == nil {
		t.Error("a refresh has no deliveryStatus")
	}
	if wh.events.hooks.count() != 1 {
		t.Errorf("%d subscriptions, want 1", wh.events.hooks.count())
	}
	ids := map[string]string{a.ID: "base"}
	for name, params := range map[string]string{
		"other arguments": with("count", hookURL, `{"a":2,"b":2}`),
		"other url":       with("count", "https://hooks.example.com/mcp/b", `{"a":1,"b":2}`),
		"other name":      with("other", hookURL, `{}`),
		"no arguments":    with("count", hookURL, `{}`),
	} {
		r := wh.subscribe(params)
		if prior, dup := ids[r.ID]; dup {
			t.Errorf("%s has the id of %s", name, prior)
		}
		ids[r.ID] = name
	}
	wh.setPrincipal("bob")
	b := wh.subscribe(with("count", hookURL, `{"a":1,"b":2}`))
	if b.ID == a.ID || b.DeliveryStatus != nil {
		t.Errorf("bob's subscription is alice's: %+v", b)
	}
	// Bob cannot end alice's subscription; each ends their own.
	wh.setPrincipal("carol")
	if _, err := wh.call(EventsUnsubscribeMethod, `{"name":"count","arguments":{"a":1,"b":2},"delivery":{"url":"`+hookURL+`"}}`); err == nil || err.Code != EventsCodeNotFound || !strings.Contains(string(err.Data), `"subscription"`) {
		t.Errorf("carol's unsubscribe = %v", err)
	}
	wh.setPrincipal("bob")
	raw, err := wh.call(EventsUnsubscribeMethod, `{"name":"count","arguments":{"a":1,"b":2},"delivery":{"url":"`+hookURL+`"}}`)
	if err != nil || withoutInfo(t, raw) != `{"resultType":"complete"}` {
		t.Errorf("bob's unsubscribe = %s, %v", raw, err)
	}
	if _, err := wh.call(EventsUnsubscribeMethod, `{"name":"count","arguments":{"a":1,"b":2},"delivery":{"url":"`+hookURL+`"}}`); err == nil || err.Code != EventsCodeNotFound {
		t.Errorf("a second unsubscribe = %v", err)
	}
	wh.setPrincipal("alice")
	if wh.events.hooks.count() != 5 {
		t.Errorf("%d subscriptions, want alice's 5", wh.events.hooks.count())
	}
	// The id is not an input.
	if _, err := wh.call(EventsUnsubscribeMethod, `{"name":"count","id":"`+a.ID+`"}`); err == nil || err.Code != EventsCodeInvalidParams {
		t.Errorf("unsubscribe by id = %v", err)
	}
	if _, err := wh.call(EventsUnsubscribeMethod, `{"name":"count","id":"`+a.ID+`","delivery":{"url":"https://hooks.example.com/nowhere"}}`); err == nil || err.Code != EventsCodeNotFound {
		t.Errorf("unsubscribe by id and another url = %v", err)
	}
	if wh.events.hooks.count() != 5 {
		t.Error("an unsubscribe by id ended a subscription")
	}
	// An unknown event is a not found of kind event.
	if _, err := wh.call(EventsUnsubscribeMethod, `{"name":"nope","delivery":{"url":"`+hookURL+`"}}`); err == nil || err.Code != EventsCodeNotFound || !strings.Contains(string(err.Data), `"event"`) {
		t.Errorf("unsubscribe of an unknown event = %v", err)
	}
	// Alice's own unsubscribe ends delivery.
	if _, err := wh.call(EventsUnsubscribeMethod, `{"name":"count","arguments":{"a":1,"b":2},"delivery":{"url":"`+hookURL+`"}}`); err != nil {
		t.Fatal(err)
	}
	wh.reset()
	emit(t, wh.events.lookup("count"), "x", 1)
	wh.settle()
	if got := len(wh.deliveries()); got != 3 {
		t.Errorf("%d deliveries, want the 3 subscriptions of count that are left", got)
	}
}

// TestEventsWebhookRefresh holds events:webhook:subscribe-idempotent-upsert,
// events:webhook:ttl-refresh-before-lte-suggestion,
// events:webhook:subscribe-response-cursor and go:events:webhook:no-reverify-on-refresh: a refresh renews the grant,
// answers with the delivery status and the watermark, and does not
// verify the callback again.
func TestEventsWebhookRefresh(t *testing.T) {
	wh := newWebhookHarness(t, nil)
	et := wh.emitter("count", nil)
	first := wh.subscribe(subParams("count", hookURL, secretA, `"ttlMs":600000`))
	wh.advance(5 * time.Minute)
	emit(t, et, "e1", 1)
	wh.settle()
	second := wh.subscribe(subParams("count", hookURL, secretA, `"ttlMs":600000`))
	if second.ID != first.ID {
		t.Fatal("the id changed")
	}
	got, _ := time.Parse(time.RFC3339, *second.RefreshBefore)
	if got.Sub(wh.clock.Now()) != 10*time.Minute {
		t.Errorf("refreshBefore %v from now", got.Sub(wh.clock.Now()))
	}
	if len(wh.verifications()) != 1 {
		t.Errorf("%d verifications after a refresh, want 1", len(wh.verifications()))
	}
	st := second.DeliveryStatus
	if st == nil || !st.Active || st.LastDeliveryAt == nil || st.LastError != nil || st.FailedSince != nil {
		t.Errorf("deliveryStatus %+v", st)
	}
	if second.Cursor == nil || *second.Cursor != "eabcd1234.1" {
		t.Errorf("cursor %v, want the watermark after the delivered event", second.Cursor)
	}
	// The grant lasts: still delivering 9 minutes on, not after 11.
	wh.advance(9 * time.Minute)
	wh.reset()
	emit(t, et, "e2", 2)
	wh.settle()
	if len(wh.deliveries()) != 1 {
		t.Error("no delivery within the grant")
	}
	wh.advance(2 * time.Minute)
	wh.reset()
	emit(t, et, "e3", 3)
	wh.settle()
	if len(wh.deliveries()) != 0 {
		t.Error("a delivery after the grant ended")
	}
}

// TestEventsWebhookRotation holds events:webhook:delivery-dual-sign-on-rotation:
// a refresh with a new secret replaces it, deliveries are signed with
// both for the grace window, and then with the new one alone.
func TestEventsWebhookRotation(t *testing.T) {
	wh := newWebhookHarness(t, nil)
	et := wh.emitter("count", nil)
	wh.subscribe(subParams("count", hookURL, secretA, ""))
	wh.subscribe(subParams("count", hookURL, secretB, ""))
	emit(t, et, "e1", 1)
	wh.settle()
	d := wh.deliveries()[0]
	if !d.verifies(secretA) || !d.verifies(secretB) || len(strings.Fields(d.Header.Get("webhook-signature"))) != 2 {
		t.Errorf("during the grace window: %q", d.Header.Get("webhook-signature"))
	}
	wh.advance(rotationGrace + time.Second)
	wh.reset()
	emit(t, et, "e2", 2)
	wh.settle()
	d = wh.deliveries()[0]
	if d.verifies(secretA) || !d.verifies(secretB) || len(strings.Fields(d.Header.Get("webhook-signature"))) != 1 {
		t.Errorf("after the grace window: %q", d.Header.Get("webhook-signature"))
	}
	// Refreshing with the same secret again does not bring the old one back.
	wh.subscribe(subParams("count", hookURL, secretB, ""))
	wh.reset()
	emit(t, et, "e3", 3)
	wh.settle()
	if d = wh.deliveries()[0]; d.verifies(secretA) {
		t.Error("the replaced secret signs again")
	}
}

// TestEventsWebhookExpiry holds events:webhook:ttl-refresh-before-lte-suggestion
// and go:events:webhook:expiry: a subscription whose grant ends is
// removed by its timer, once, with OnUnsubscribe, and a subscribe after
// that is a new one.
func TestEventsWebhookExpiry(t *testing.T) {
	var subs, unsubs int
	wh := newWebhookHarness(t, nil)
	et := wh.emitter("count", func(d *EventDef) {
		d.OnSubscribe = func(_ context.Context, c EventContext, _ json.RawMessage) error {
			if c.Mode != DeliveryWebhook || c.Principal != "alice" || !strings.HasPrefix(c.Subscription, "sub_") {
				t.Errorf("OnSubscribe context %+v", c)
			}
			subs++
			return nil
		}
		d.OnUnsubscribe = func(context.Context, EventContext, json.RawMessage) error { unsubs++; return nil }
	})
	wh.subscribe(subParams("count", hookURL, secretA, `"ttlMs":120000`))
	if subs != 1 || unsubs != 0 {
		t.Fatalf("subs %d unsubs %d", subs, unsubs)
	}
	wh.advance(119 * time.Second)
	if unsubs != 0 || wh.events.hooks.count() != 1 {
		t.Fatal("expired early")
	}
	wh.advance(2 * time.Second)
	if unsubs != 1 || wh.events.hooks.count() != 0 {
		t.Fatalf("after the grant: unsubs %d, held %d", unsubs, wh.events.hooks.count())
	}
	wh.reset()
	emit(t, et, "e", 1)
	wh.settle()
	if len(wh.all()) != 0 {
		t.Error("a request after expiry")
	}
	res := wh.subscribe(subParams("count", hookURL, secretA, `"ttlMs":120000`))
	if res.DeliveryStatus != nil || subs != 2 {
		t.Errorf("a subscribe after expiry is a refresh: %+v, subs %d", res, subs)
	}
	// A subscribe after the grant, before the timer fired, is also new.
	wh.timers.mu.Lock()
	for _, tm := range wh.timers.items {
		tm.stopped = true
	}
	wh.timers.mu.Unlock()
	wh.clock.Advance(3 * time.Minute)
	res = wh.subscribe(subParams("count", hookURL, secretA, `"ttlMs":120000`))
	if res.DeliveryStatus != nil || subs != 3 || unsubs != 2 {
		t.Errorf("a lazily expired subscription: %+v, subs %d unsubs %d", res, subs, unsubs)
	}
}

// TestEventsWebhookHooks holds go:events:webhook:hooks: OnSubscribe
// refusing a subscription, capacity checked before it, Match and
// Transform applied for the subscription's arguments, and poll served
// beside webhook from the same ring.
func TestEventsWebhookHooks(t *testing.T) {
	var hooked int
	wh := newWebhookHarness(t, func(o *EventsOptions) { o.Webhook.MaxSubscriptions = 2 })
	et := wh.emitter("region", func(d *EventDef) {
		d.InputSchema = json.RawMessage(`{"type":"object","properties":{"r":{"type":"string"}}}`)
		d.OnSubscribe = func(_ context.Context, c EventContext, args json.RawMessage) error {
			hooked++
			if strings.Contains(string(args), "forbidden") {
				return &EventsError{Code: EventsCodeForbidden, Message: "no"}
			}
			if strings.Contains(string(args), "boom") {
				return errors.New("secret internal detail")
			}
			return nil
		}
		d.Match = func(_ EventContext, ev Event, args json.RawMessage) bool {
			var a struct{ R string }
			_ = json.Unmarshal(args, &a)
			return a.R == "" || strings.HasPrefix(ev.ID, a.R)
		}
		d.Transform = func(_ EventContext, ev Event, _ json.RawMessage) (Event, error) {
			ev.Data = json.RawMessage(`{"n":99}`)
			return ev, nil
		}
	})
	region := func(r, url string) string {
		return fmt.Sprintf(`{"name":"region","arguments":{"r":%q},"delivery":{"mode":"webhook","url":%q,"secret":%q}}`, r, url, secretA)
	}
	if err := wh.subscribeErr(region("forbidden", hookURL)); err.Code != EventsCodeForbidden {
		t.Errorf("OnSubscribe refusal = %d", err.Code)
	}
	if err := wh.subscribeErr(region("boom", hookURL)); err.Code != -32603 || strings.Contains(err.Message, "secret internal") {
		t.Errorf("OnSubscribe failure = %d %s", err.Code, err.Message)
	}
	if wh.events.hooks.count() != 0 {
		t.Fatal("a refused subscription is held")
	}
	wh.subscribe(region("us", hookURL))
	wh.subscribe(region("eu", "https://hooks.example.com/mcp/b"))
	before := hooked
	if err := wh.subscribeErr(region("ap", "https://hooks.example.com/mcp/c")); err.Code != EventsCodeResourceExhausted || !strings.Contains(string(err.Data), `"subscriptions"`) {
		t.Errorf("past capacity = %d %s", err.Code, err.Data)
	}
	if hooked != before {
		t.Error("OnSubscribe ran for a subscription refused for capacity")
	}
	if err := et.Emit(Event{ID: "us-1", Data: json.RawMessage(`{"n":1}`)}); err != nil {
		t.Fatal(err)
	}
	if err := et.Emit(Event{ID: "eu-1", Data: json.RawMessage(`{"n":2}`)}); err != nil {
		t.Fatal(err)
	}
	wh.settle()
	byURL := map[string][]string{}
	for _, d := range wh.deliveries() {
		byURL[d.URL] = append(byURL[d.URL], d.eventID())
		if !strings.Contains(string(d.Body), `"data":{"n":99}`) {
			t.Errorf("the transform did not apply: %s", d.Body)
		}
	}
	if fmt.Sprint(byURL[hookURL]) != "[us-1]" || fmt.Sprint(byURL["https://hooks.example.com/mcp/b"]) != "[eu-1]" {
		t.Errorf("deliveries %v", byURL)
	}
	// Poll sees both events, from the same ring.
	got := wh.poll(`{"name":"region","arguments":{},"cursor":"eabcd1234.0"}`)
	if ids(got) != "us-1,eu-1" {
		t.Errorf("poll saw %s", ids(got))
	}
}

// TestEventsWebhookStreamStillUnsupported holds
// events:webhook:error-unsupported: with webhook on, events/stream is
// still answered with the unsupported error naming push, and an event
// lists no push.
func TestEventsWebhookStreamStillUnsupported(t *testing.T) {
	wh := newWebhookHarness(t, nil)
	wh.emitter("count", nil)
	_, err := wh.call(EventsStreamMethod, `{"name":"count"}`)
	if err == nil || err.Code != EventsCodeUnsupported || normalise(t, err.Data) != `{"feature":"deliveryMode","value":"push"}` {
		t.Errorf("events/stream = %v", err)
	}
	if got := wh.listDelivery(t, "count"); strings.Contains(got, "push") {
		t.Errorf("delivery %s", got)
	}
	if _, rerr := wh.events.Register(EventDef{Name: "p", PayloadSchema: json.RawMessage(countSchema), EmitOnly: true, Delivery: []string{"poll", "push"}}); rerr == nil {
		t.Error("a type that lists push was registered")
	}
}

// TestEventsWebhookOnlyType holds go:events:register:rules: an emit-only
// type may offer webhook alone, its ring takes events, and a poll of it
// is answered with the unsupported error for delivery mode poll.
func TestEventsWebhookOnlyType(t *testing.T) {
	wh := newWebhookHarness(t, nil)
	typ, err := wh.events.Register(EventDef{Name: "hook", PayloadSchema: json.RawMessage(countSchema), EmitOnly: true, Delivery: []string{"webhook"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := typ.Emit(Event{Data: json.RawMessage(`{"n":1}`)}); err != nil {
		t.Fatal(err)
	}
	_, perr := wh.call(EventsPollMethod, `{"name":"hook","cursor":null}`)
	if perr == nil || perr.Code != EventsCodeUnsupported {
		t.Fatalf("poll = %v, want unsupported", perr)
	}
	if got := normalise(t, perr.Data); got != `{"feature":"deliveryMode","value":"poll"}` {
		t.Errorf("data %s", got)
	}
}

// TestEventsWebhookRegisterRules holds go:events:register:rules for
// webhook: a webhook delivery needs the option and an emit-only type, and
// the default list of a type gains webhook when the option is on.
func TestEventsWebhookRegisterRules(t *testing.T) {
	wh := newWebhookHarness(t, nil)
	check := func(context.Context, CheckContext, json.RawMessage, *string) (CheckResult, error) {
		return CheckResult{}, nil
	}
	payload := json.RawMessage(countSchema)
	tests := []struct {
		name string
		def  EventDef
		want string
		list string
	}{
		{"emit-only default", EventDef{Name: "a", PayloadSchema: payload, EmitOnly: true}, "", "poll,webhook"},
		{"emit-only poll alone", EventDef{Name: "b", PayloadSchema: payload, EmitOnly: true, Delivery: []string{"poll"}}, "", "poll"},
		{"emit-only both", EventDef{Name: "c", PayloadSchema: payload, EmitOnly: true, Delivery: []string{"poll", "webhook"}}, "", "poll,webhook"},
		{"check default", EventDef{Name: "d", PayloadSchema: payload, Check: check}, "", "poll"},
		{"check with webhook", EventDef{Name: "e", PayloadSchema: payload, Check: check, Delivery: []string{"poll", "webhook"}}, "emit-only", ""},
		{"webhook alone", EventDef{Name: "f", PayloadSchema: payload, EmitOnly: true, Delivery: []string{"webhook"}}, "", "webhook"},
		{"check without poll", EventDef{Name: "g", PayloadSchema: payload, Check: check, Delivery: []string{}}, "", "poll"},
		{"check webhook alone", EventDef{Name: "h", PayloadSchema: payload, Check: check, Delivery: []string{"webhook"}}, "emit-only", ""},
	}
	for _, tc := range tests {
		_, err := wh.events.Register(tc.def)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: %v", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: %v, want %q", tc.name, err, tc.want)
		case tc.want == "":
			if got := wh.listDelivery(t, tc.def.Name); got != tc.list {
				t.Errorf("%s: delivery %s, want %s", tc.name, got, tc.list)
			}
		}
	}
	// Bad trusted origins are an error at AddEvents.
	if _, err := newGuard(&WebhookOptions{TrustedOrigins: []string{"nope"}}); err == nil {
		t.Error("a bad trusted origin was accepted")
	}
}
