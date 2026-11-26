package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agenttool/mcpclient"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// principalHeader is how the end-to-end test says who it is: a stand-in
// for the verified token an embedding application reads.
const principalHeader = "X-Test-Principal"

type headerAdder struct {
	next http.RoundTripper
	key  string
	val  string
}

func (h headerAdder) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set(h.key, h.val)
	return h.next.RoundTrip(r)
}

// TestEventsWebhookClientServer holds go:events:webhook:end-to-end,
// events:webhook:mcp-name-missing-rejected and
// events:webhook:mcp-name-mismatch-rejected: mcpclient subscribes over
// Streamable HTTP with the routing headers, the server verifies the
// callback by a challenge the client's receiver echoes, delivers events
// signed with the real guarded client to a trusted loopback origin, the
// receiver verifies and forwards them with their cursors, a callback that
// does not know the secret fails the subscribe with -32027, a
// termination is forwarded, and an unsubscribe ends delivery.
func TestEventsWebhookClientServer(t *testing.T) {
	secrets := mcpclient.NewMemorySecrets()
	var mu sync.Mutex
	var got []mcpclient.WebhookMessage
	receiver := httptest.NewServer(mcpclient.NewWebhookReceiver(mcpclient.WebhookReceiverOptions{
		Secrets: secrets,
		Handle: func(_ context.Context, m mcpclient.WebhookMessage) error {
			mu.Lock()
			got = append(got, m)
			mu.Unlock()
			return nil
		},
	}))
	t.Cleanup(receiver.Close)
	messages := func() []mcpclient.WebhookMessage {
		mu.Lock()
		defer mu.Unlock()
		return append([]mcpclient.WebhookMessage(nil), got...)
	}

	server := sdk.NewServer(&sdk.Implementation{Name: "events", Version: "1"}, nil)
	ev, err := AddEvents(server, EventsOptions{Webhook: &WebhookOptions{
		Principal: func(_ context.Context, extra *sdk.RequestExtra, _ *sdk.ServerSession) string {
			if extra == nil || extra.Header == nil {
				return ""
			}
			return extra.Header.Get(principalHeader)
		},
		TrustedOrigins: []string{receiver.URL},
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ev.Close)
	et, err := ev.Register(EventDef{
		Name: "deploy", EmitOnly: true, PayloadSchema: json.RawMessage(countSchema),
		InputSchema: json.RawMessage(`{"type":"object","properties":{"region":{"type":"string","x-mcp-header":"Region"}},"required":["region"]}`),
		Match: func(_ EventContext, e Event, args json.RawMessage) bool {
			var a struct{ Region string }
			_ = json.Unmarshal(args, &a)
			return e.Meta["region"] == a.Region
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, &sdk.StreamableHTTPOptions{Stateless: true}))
	t.Cleanup(hs.Close)

	rec := &statusRecorder{next: headerAdder{next: http.DefaultTransport, key: principalHeader, val: "alice"}}
	connect := func(rt http.RoundTripper) *mcpclient.Remote {
		s, err := mcpclient.Connect(context.Background(), &sdk.StreamableClientTransport{Endpoint: hs.URL, HTTPClient: &http.Client{Transport: mcpclient.EventHeaders(rt)}}, mcpclient.WithEvents())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	}
	s := connect(rec)
	ctx := context.Background()
	if v := s.Session().InitializeResult().ProtocolVersion; v < "2026-07-28" {
		t.Skipf("the SDK negotiated %s, which has no routing headers", v)
	}
	descs, err := s.ListEvents(ctx)
	if err != nil || len(descs) != 1 || !descs[0].Webhooks() || !descs[0].Polls() {
		t.Fatalf("list = %+v, %v", descs, err)
	}

	secret, err := mcpclient.NewWebhookSecret()
	if err != nil {
		t.Fatal(err)
	}
	args := json.RawMessage(`{"region":"eu"}`)
	url := receiver.URL + "/hooks/a"
	p := mcpclient.SubscribeParams{Name: "deploy", Arguments: args, URL: url, Secret: secret, TTL: 10 * time.Minute}

	// A callback that does not know the secret fails the challenge, and
	// the error says only so.
	bad := p
	bad.URL = receiver.URL + "/hooks/unknown-secret"
	_, err = s.SubscribeEvents(ctx, bad)
	var ee *mcpclient.EventError
	if !errors.As(err, &ee) || ee.Code != mcpclient.EventsCodeCallbackEndpoint || !strings.Contains(string(ee.Data), "http_4xx") {
		t.Fatalf("subscribe with an unknown secret = %v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Error("the error holds the secret")
	}
	secrets.Add(secret)
	res, err := s.SubscribeEvents(ctx, p)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if res.ID == "" || res.RefreshBefore == nil || res.Cursor == nil {
		t.Fatalf("result %+v", res)
	}
	if ss, ok := secrets.SecretsFor(res.ID); !ok || len(ss) != 1 {
		t.Errorf("the receiver did not bind the id the challenge carried: %v %v", ss, ok)
	}

	emitIn(t, et, "eu", "e1")
	emitIn(t, et, "us", "elsewhere")
	emitIn(t, et, "eu", "e2")
	waitFor(t, func() bool { return len(messages()) == 2 })
	byID := map[string]mcpclient.WebhookMessage{}
	for _, m := range messages() {
		byID[m.Event.EventID] = m
		if m.Kind != mcpclient.WebhookEvent || m.SubscriptionID != res.ID || m.Cursor == nil || m.Event.Name != "deploy" || string(m.Event.Data) != `{"n":1}` {
			t.Errorf("message %+v", m)
		}
	}
	if byID["e1"].MessageID == "" || byID["e1"].MessageID == byID["e2"].MessageID {
		t.Error("message ids")
	}

	// A refresh with the same key answers with status and cursor.
	again, err := s.SubscribeEvents(ctx, mcpclient.SubscribeParams{Name: "deploy", Arguments: args, URL: url, Secret: secret, Cursor: res.Cursor})
	if err != nil || again.ID != res.ID || again.DeliveryStatus == nil || !again.DeliveryStatus.Active {
		t.Errorf("refresh %+v, %v", again, err)
	}

	// The routing headers apply to subscribe, and to unsubscribe.
	for name, tamper := range map[string]func(*http.Request){
		"events:webhook:mcp-name-mismatch-rejected": func(r *http.Request) { r.Header.Set("Mcp-Name", "other") },
		"events:webhook:mcp-name-missing-rejected":  func(r *http.Request) { r.Header.Del("Mcp-Name") },
	} {
		rec.tamper = tamper
		_, err := s.SubscribeEvents(ctx, p)
		if !errors.As(err, &ee) || ee.Code != mcpclient.EventsCodeHeaderMismatch {
			t.Errorf("%s: subscribe = %v, want -32020", name, err)
		}
		err = s.UnsubscribeEvents(ctx, "deploy", args, url)
		if !errors.As(err, &ee) || ee.Code != mcpclient.EventsCodeHeaderMismatch {
			t.Errorf("%s: unsubscribe = %v, want -32020", name, err)
		}
	}
	rec.tamper = nil

	// Another principal is another subscription, and cannot end alice's.
	bob := connect(headerAdder{next: http.DefaultTransport, key: principalHeader, val: "bob"})
	if err := bob.UnsubscribeEvents(ctx, "deploy", args, url); !errors.As(err, &ee) || ee.Code != mcpclient.EventsCodeNotFound {
		t.Errorf("bob's unsubscribe = %v", err)
	}
	nobody := connect(http.DefaultTransport)
	if _, err := nobody.SubscribeEvents(ctx, p); !errors.As(err, &ee) || ee.Code != mcpclient.EventsCodeForbidden {
		t.Errorf("an unauthenticated subscribe = %v", err)
	}

	// A termination reaches the receiver.
	if _, err := ev.Register(EventDef{Name: "deploy", EmitOnly: true, PayloadSchema: json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"},"x":{"type":"string"}}}`),
		InputSchema: json.RawMessage(`{"type":"object","properties":{"region":{"type":"string","x-mcp-header":"Region"}},"required":["region"]}`)}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return len(messages()) == 3 })
	if last := messages()[2]; last.Kind != mcpclient.WebhookTerminated || last.Error == nil || last.Error.Code != mcpclient.EventsCodeUnsupported {
		t.Errorf("terminated %+v", last)
	}
	if err := s.UnsubscribeEvents(ctx, "deploy", args, url); !errors.As(err, &ee) || ee.Code != mcpclient.EventsCodeNotFound {
		t.Errorf("unsubscribe after termination = %v", err)
	}
}

// TestEventsWebhookUnsubscribeStopsDelivery holds go:events:webhook:end-to-end:
// after an unsubscribe the receiver hears nothing more.
func TestEventsWebhookUnsubscribeStopsDelivery(t *testing.T) {
	secrets := mcpclient.NewMemorySecrets()
	var mu sync.Mutex
	n := 0
	receiver := httptest.NewServer(mcpclient.NewWebhookReceiver(mcpclient.WebhookReceiverOptions{
		Secrets: secrets,
		Handle:  func(context.Context, mcpclient.WebhookMessage) error { mu.Lock(); n++; mu.Unlock(); return nil },
	}))
	t.Cleanup(receiver.Close)
	count := func() int { mu.Lock(); defer mu.Unlock(); return n }

	server := sdk.NewServer(&sdk.Implementation{Name: "events", Version: "1"}, nil)
	ev, err := AddEvents(server, EventsOptions{Webhook: &WebhookOptions{
		Principal:      func(context.Context, *sdk.RequestExtra, *sdk.ServerSession) string { return "alice" },
		TrustedOrigins: []string{receiver.URL},
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ev.Close)
	et, err := ev.Register(EventDef{Name: "tick", EmitOnly: true, PayloadSchema: json.RawMessage(countSchema)})
	if err != nil {
		t.Fatal(err)
	}
	ct, st := sdk.NewInMemoryTransports()
	if _, err := server.Connect(context.Background(), st, nil); err != nil {
		t.Fatal(err)
	}
	s, err := mcpclient.Connect(context.Background(), ct, mcpclient.WithEvents())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	secret, _ := mcpclient.NewWebhookSecret()
	secrets.Add(secret)
	ctx := context.Background()
	if _, err := s.SubscribeEvents(ctx, mcpclient.SubscribeParams{Name: "tick", URL: receiver.URL + "/h", Secret: secret}); err != nil {
		t.Fatal(err)
	}
	emit(t, et, "t1", 1)
	waitFor(t, func() bool { return count() == 1 })
	if err := s.UnsubscribeEvents(ctx, "tick", nil, receiver.URL+"/h"); err != nil {
		t.Fatal(err)
	}
	emit(t, et, "t2", 2)
	time.Sleep(100 * time.Millisecond)
	if count() != 1 {
		t.Errorf("%d deliveries, want 1: one after the unsubscribe", count())
	}
}
