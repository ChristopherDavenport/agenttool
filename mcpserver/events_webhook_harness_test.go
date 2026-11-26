package mcpserver

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeTimers is the AfterFunc of a test: timers fire when the test moves
// the clock, in order of their due times, and never on their own.
type fakeTimers struct {
	mu    sync.Mutex
	clock *fakeClock
	items []*fakeTimer
}

type fakeTimer struct {
	due     time.Time
	f       func()
	stopped bool
}

func (ft *fakeTimers) after(d time.Duration, f func()) func() bool {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	t := &fakeTimer{due: ft.clock.Now().Add(d), f: f}
	ft.items = append(ft.items, t)
	return func() bool {
		ft.mu.Lock()
		defer ft.mu.Unlock()
		was := !t.stopped
		t.stopped = true
		return was
	}
}

// next removes and returns the earliest timer due at or before limit.
func (ft *fakeTimers) next(limit time.Time) *fakeTimer {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	var best *fakeTimer
	bi := -1
	for i, t := range ft.items {
		if t.stopped || t.due.After(limit) {
			continue
		}
		if best == nil || t.due.Before(best.due) {
			best, bi = t, i
		}
	}
	if best != nil {
		ft.items = append(ft.items[:bi], ft.items[bi+1:]...)
		best.stopped = true
	}
	return best
}

// pending is how many timers have not fired or been stopped.
func (ft *fakeTimers) pending() int {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	n := 0
	for _, t := range ft.items {
		if !t.stopped {
			n++
		}
	}
	return n
}

// delivery is one request the fake sender saw.
type delivery struct {
	URL    string
	Header http.Header
	Body   []byte
	At     time.Time
}

func (d delivery) typ() string {
	var b struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(d.Body, &b)
	return b.Type
}

func (d delivery) eventID() string {
	var b struct {
		EventID string `json:"eventId"`
	}
	_ = json.Unmarshal(d.Body, &b)
	return b.EventID
}

func (d delivery) cursor() string {
	var b struct {
		Cursor *string `json:"cursor"`
	}
	_ = json.Unmarshal(d.Body, &b)
	if b.Cursor == nil {
		return "<null>"
	}
	return *b.Cursor
}

// verifies says whether the delivery's signature holds under a secret.
func (d delivery) verifies(secret string) bool {
	key, err := parseWebhookSecret(secret)
	if err != nil {
		return false
	}
	ts, _ := strconv.ParseInt(d.Header.Get("webhook-timestamp"), 10, 64)
	m := hmac.New(sha256.New, key)
	fmt.Fprintf(m, "%s.%d.", d.Header.Get("webhook-id"), ts)
	m.Write(d.Body)
	want := "v1," + base64.StdEncoding.EncodeToString(m.Sum(nil))
	for _, s := range strings.Fields(d.Header.Get("webhook-signature")) {
		if hmac.Equal([]byte(s), []byte(want)) {
			return true
		}
	}
	return false
}

// webhookHarness is an events harness with webhook delivery over a fake
// sender, fake time and a principal the test sets.
type webhookHarness struct {
	*eventsHarness
	timers *fakeTimers

	mu        sync.Mutex
	principal string
	sent      []delivery
	// respond answers a request; nil answers 200, and echoes the
	// challenge of a verification.
	respond func(d delivery) (webhookResponse, error)
	// gate, when set, is waited on before a request is answered.
	gate func(ctx context.Context, d delivery)
}

func newWebhookHarness(t *testing.T, mod func(*EventsOptions)) *webhookHarness {
	t.Helper()
	wh := &webhookHarness{principal: "alice"}
	clock := newClock()
	wh.timers = &fakeTimers{clock: clock}
	h := newEventsHarnessWithClock(t, clock, func(o *EventsOptions) {
		o.Webhook = &WebhookOptions{
			Principal: func(context.Context, *sdk.RequestExtra, *sdk.ServerSession) string {
				wh.mu.Lock()
				defer wh.mu.Unlock()
				return wh.principal
			},
			AfterFunc: wh.timers.after,
			send:      wh.send,
		}
		if mod != nil {
			mod(o)
		}
	})
	wh.eventsHarness = h
	t.Cleanup(h.events.Close)
	return wh
}

func (wh *webhookHarness) setPrincipal(p string) {
	wh.mu.Lock()
	wh.principal = p
	wh.mu.Unlock()
}

func (wh *webhookHarness) send(ctx context.Context, r webhookRequest) (webhookResponse, error) {
	d := delivery{URL: r.URL, Header: r.Header.Clone(), Body: append([]byte(nil), r.Body...), At: wh.clock.Now()}
	wh.mu.Lock()
	wh.sent = append(wh.sent, d)
	respond, gate := wh.respond, wh.gate
	wh.mu.Unlock()
	if gate != nil {
		gate(ctx, d)
	}
	if err := ctx.Err(); err != nil {
		return webhookResponse{}, &sendError{Category: failTimeout, Err: err}
	}
	if respond != nil {
		return respond(d)
	}
	return echoOK(d)
}

// echoOK answers 200, with the challenge echoed for a verification.
func echoOK(d delivery) (webhookResponse, error) {
	if d.typ() == "verification" {
		var b struct {
			Challenge string `json:"challenge"`
		}
		_ = json.Unmarshal(d.Body, &b)
		out, _ := json.Marshal(map[string]string{"challenge": b.Challenge})
		return webhookResponse{Status: 200, Body: out}, nil
	}
	return webhookResponse{Status: 200}, nil
}

func (wh *webhookHarness) setRespond(f func(d delivery) (webhookResponse, error)) {
	wh.mu.Lock()
	wh.respond = f
	wh.mu.Unlock()
}

// deliveries are the requests seen so far that are not verifications.
func (wh *webhookHarness) deliveries() []delivery {
	wh.mu.Lock()
	defer wh.mu.Unlock()
	var out []delivery
	for _, d := range wh.sent {
		if d.typ() != "verification" {
			out = append(out, d)
		}
	}
	return out
}

func (wh *webhookHarness) verifications() []delivery {
	wh.mu.Lock()
	defer wh.mu.Unlock()
	var out []delivery
	for _, d := range wh.sent {
		if d.typ() == "verification" {
			out = append(out, d)
		}
	}
	return out
}

func (wh *webhookHarness) all() []delivery {
	wh.mu.Lock()
	defer wh.mu.Unlock()
	return append([]delivery(nil), wh.sent...)
}

func (wh *webhookHarness) reset() {
	wh.mu.Lock()
	wh.sent = nil
	wh.mu.Unlock()
}

// settle waits until no delivery goroutine runs.
func (wh *webhookHarness) settle() {
	wh.t.Helper()
	waitFor(wh.t, wh.events.hooks.idleForTest)
}

// advance moves the clock by d, firing the timers due on the way, each at
// its time, and letting the deliveries they start finish before the next.
func (wh *webhookHarness) advance(d time.Duration) {
	wh.t.Helper()
	target := wh.clock.Now().Add(d)
	for {
		wh.settle()
		tm := wh.timers.next(target)
		if tm == nil {
			break
		}
		if tm.due.After(wh.clock.Now()) {
			wh.clock.Advance(tm.due.Sub(wh.clock.Now()))
		}
		tm.f()
	}
	wh.settle()
	if now := wh.clock.Now(); target.After(now) {
		wh.clock.Advance(target.Sub(now))
	}
}

// secretA and secretB are 32-byte secrets.
var (
	secretA = webhookSecretOf(1)
	secretB = webhookSecretOf(2)
)

func webhookSecretOf(n byte) string {
	b := make([]byte, 32)
	for i := range b {
		b[i] = n + byte(i)
	}
	return webhookSecretPrefix + base64.StdEncoding.EncodeToString(b)
}

const hookURL = "https://hooks.example.com/mcp/a"

// subParams is the params of an events/subscribe.
func subParams(name, url, secret string, extra string) string {
	if extra != "" {
		extra = "," + extra
	}
	return fmt.Sprintf(`{"name":%q,"arguments":{},"delivery":{"mode":"webhook","url":%q,"secret":%q}%s}`, name, url, secret, extra)
}

type subOut struct {
	ResultType     string  `json:"resultType"`
	ID             string  `json:"id"`
	RefreshBefore  *string `json:"refreshBefore"`
	Cursor         *string `json:"cursor"`
	Truncated      bool    `json:"truncated"`
	DeliveryStatus *struct {
		Active         bool    `json:"active"`
		LastDeliveryAt *string `json:"lastDeliveryAt"`
		LastError      *string `json:"lastError"`
		FailedSince    *string `json:"failedSince"`
	} `json:"deliveryStatus"`
}

func (wh *webhookHarness) subscribe(params string) subOut {
	wh.t.Helper()
	raw, err := wh.call(EventsSubscribeMethod, params)
	if err != nil {
		wh.t.Fatalf("events/subscribe: %v", err)
	}
	var out subOut
	if jerr := json.Unmarshal(raw, &out); jerr != nil {
		wh.t.Fatal(jerr)
	}
	return out
}

func (wh *webhookHarness) subscribeErr(params string) *jsonrpc.Error {
	wh.t.Helper()
	raw, err := wh.call(EventsSubscribeMethod, params)
	if err == nil {
		wh.t.Fatalf("events/subscribe was accepted: %s", raw)
	}
	return err
}

func base64Std(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// losingStore deletes as the wrapped store does but reports false while
// lose is set, as a replica that lost the race to delete would see.
type losingStore struct {
	WebhookStore
	lose atomic.Bool
}

func (s *losingStore) Delete(ctx context.Context, id string) (bool, error) {
	ok, err := s.WebhookStore.Delete(ctx, id)
	if s.lose.Load() {
		return false, err
	}
	return ok, err
}
