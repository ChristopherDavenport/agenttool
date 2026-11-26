package mcpserver

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// WebhookOptions turn on the webhook delivery mode of MCP Events: a
// client subscribes with events/subscribe, naming a URL and a secret,
// and the server POSTs the events to the URL, signed as Standard
// Webhooks are. A server without them answers events/subscribe with
// [EventsCodeUnsupported] and lists no webhook delivery.
//
// This is code that makes requests to a URL a client chose. It resolves
// the URL's host at every send and refuses a host with any address that
// is not globally routable, dials the address it checked, follows no
// redirect, and delivers to nothing that has not echoed a challenge.
// What it does not do is authenticate: Principal is the embedder's, and
// webhook is refused for a request that has none.
type WebhookOptions struct {
	// Principal says who a request came from, for the subscription's
	// key: subscriptions of two principals are two subscriptions, and
	// one cannot unsubscribe or rotate the other's. It reads what the
	// embedding application verified, such as extra.TokenInfo, which is
	// nil where the transport has no request, as on stdio. An empty
	// result is no one, and events/subscribe and events/unsubscribe are
	// then refused with [EventsCodeForbidden], as the SEP says. There is
	// no default: this package does no authentication, so a server
	// serves no webhook until its embedder says who is calling.
	Principal func(ctx context.Context, extra *sdk.RequestExtra, session *sdk.ServerSession) string
	// Store holds the subscriptions; the default is a map in memory.
	Store WebhookStore
	// TrustedOrigins are origins, "scheme://host[:port]", whose
	// callbacks may use http and may be at a special-purpose address,
	// for a development server or a receiver on a private network. The
	// verification handshake, the signature and the refusal to follow a
	// redirect still apply to them.
	TrustedOrigins []string
	// Resolver looks up a callback's host; the default is the system
	// resolver. A deployment that resolves names its own way, or a test,
	// sets it. Its answers are checked like any others.
	Resolver func(ctx context.Context, host string) ([]netip.Addr, error)
	// TLSConfig is the base of the TLS configuration of a delivery, for
	// roots a private CA needs. Nil is the default configuration.
	TLSConfig *tls.Config
	// Timeout is how long a delivery attempt, a handshake or a lookup
	// may take; the default is 5 seconds.
	Timeout time.Duration
	// TTL is the longest lifetime a subscription is granted, and what
	// one is granted when the client names none; the default is an
	// hour. A client's suggestion below 60 seconds is raised to 60. This
	// package never grants a subscription that does not expire.
	TTL time.Duration
	// MaxSubscriptions bounds the subscriptions this process delivers
	// for, refused beyond with [EventsCodeResourceExhausted]; the
	// default is 1024.
	MaxSubscriptions int
	// AfterFunc schedules f after d and returns a func that cancels it,
	// reporting whether it did before f ran; nil is [time.AfterFunc].
	// Retries and expiry use it, so a test that sets it and Now moves
	// time itself.
	AfterFunc func(d time.Duration, f func()) (stop func() bool)

	// send replaces the guarded HTTP client, for a test.
	send webhookSender
}

const (
	defaultWebhookTTL = time.Hour
	ttlFloor          = 60 * time.Second
)

// webhookEngine is the webhook side of an [Events]: the store of
// subscriptions, the delivery state of those this process serves and the
// verification of callbacks.
type webhookEngine struct {
	e        *Events
	o        WebhookOptions
	store    WebhookStore
	guard    *guard
	send     webhookSender
	verifier *verifier
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	// busy counts the goroutines in flight, which a test waits out.
	busy atomic.Int64

	mu     sync.Mutex
	closed bool
	subs   map[string]*tracked
	all    map[*tracked]struct{}
	locks  map[string]*idLock
}

type idLock struct {
	mu sync.Mutex
	n  int
}

func newWebhookEngine(e *Events, o WebhookOptions) (*webhookEngine, error) {
	g, err := newGuard(&o)
	if err != nil {
		return nil, err
	}
	w := &webhookEngine{
		e: e, o: o, store: o.Store, guard: g, send: o.send, verifier: newVerifier(),
		subs: map[string]*tracked{}, all: map[*tracked]struct{}{}, locks: map[string]*idLock{},
	}
	if w.send == nil {
		w.send = g.send
	}
	w.ctx, w.cancel = context.WithCancel(context.Background())
	return w, nil
}

func (o WebhookOptions) withDefaults() WebhookOptions {
	if o.Store == nil {
		o.Store = NewMemoryWebhookStore()
	}
	if o.TTL <= 0 {
		o.TTL = defaultWebhookTTL
	}
	if o.MaxSubscriptions <= 0 {
		o.MaxSubscriptions = defaultMaxSubscription
	}
	o.TrustedOrigins = slices.Clone(o.TrustedOrigins)
	if o.AfterFunc == nil {
		o.AfterFunc = func(d time.Duration, f func()) func() bool {
			return time.AfterFunc(d, f).Stop
		}
	}
	return o
}

func (w *webhookEngine) now() time.Time { return w.e.opts.Now() }

func (w *webhookEngine) afterFunc(d time.Duration, f func()) func() bool {
	return w.o.AfterFunc(d, f)
}

// start runs f on a goroutine the engine waits for when it closes. A
// closed engine runs nothing.
func (w *webhookEngine) start(f func()) {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.wg.Add(1)
	w.busy.Add(1)
	w.mu.Unlock()
	go func() {
		defer w.wg.Done()
		defer w.busy.Add(-1)
		f()
	}()
}

// forget drops t from the engine's tables and uncovers its pair.
func (w *webhookEngine) forget(t *tracked) {
	w.mu.Lock()
	delete(w.all, t)
	if w.subs[t.ec.Subscription] == t {
		delete(w.subs, t.ec.Subscription)
	}
	w.mu.Unlock()
	w.verifier.uncover(t.pair)
}

func (w *webhookEngine) lockID(id string) func() {
	w.mu.Lock()
	l := w.locks[id]
	if l == nil {
		l = &idLock{}
		w.locks[id] = l
	}
	l.n++
	w.mu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		w.mu.Lock()
		if l.n--; l.n == 0 {
			delete(w.locks, id)
		}
		w.mu.Unlock()
	}
}

func (w *webhookEngine) tracked(id string) *tracked {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.subs[id]
}

// close stops every delivery: timers are cancelled, in-flight requests
// are cut off and the goroutines are waited for. Subscriptions stay in
// the store.
func (w *webhookEngine) close() {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.closed = true
	live := make([]*tracked, 0, len(w.all))
	for t := range w.all {
		live = append(live, t)
	}
	w.mu.Unlock()
	w.cancel()
	for _, t := range live {
		t.end()
	}
	w.wg.Wait()
}

// subscriptionID is the id of a subscription, "sub_" and 22 characters of
// the SHA-256 of the canonical JSON of its key. It is a handle for the
// receiver to route by, and not a capability: no method takes it.
func subscriptionID(principal, url, name string, args json.RawMessage) string {
	c, err := canonicalJSON(args)
	if err != nil {
		c = args
	}
	key, _ := json.Marshal([]json.RawMessage{mustJSON(principal), mustJSON(url), mustJSON(name), c})
	sum := sha256.Sum256(key)
	return "sub_" + base64.RawURLEncoding.EncodeToString(sum[:])[:22]
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// subscribeResult is the answer of events/subscribe.
type subscribeResult struct {
	sdk.ResultBase
	ResultType     string          `json:"resultType"`
	ID             string          `json:"id"`
	RefreshBefore  *string         `json:"refreshBefore"`
	Cursor         *string         `json:"cursor"`
	Truncated      bool            `json:"truncated"`
	DeliveryStatus *deliveryStatus `json:"deliveryStatus,omitempty"`
}

// deliveryParams are the delivery of an events/subscribe and an
// events/unsubscribe.
type deliveryParams struct {
	Mode   string `json:"mode"`
	URL    string `json:"url"`
	Secret string `json:"secret"`
}

func (w *webhookEngine) principal(ctx context.Context, ss *sdk.ServerSession) string {
	if w.o.Principal == nil {
		return ""
	}
	var extra *sdk.RequestExtra
	if ri, ok := ctx.Value(requestInfoKey{}).(*requestInfo); ok {
		extra = ri.extra
	}
	return w.o.Principal(ctx, extra, ss)
}

func forbiddenNoPrincipal(method string) *EventsError {
	return &EventsError{Code: EventsCodeForbidden, Message: method + " needs an authenticated principal", Data: map[string]any{"reason": "unauthenticated"}}
}

func (t *EventType) delivers(mode string) bool { return slices.Contains(t.def.Delivery, mode) }

func parseDelivery(raw json.RawMessage, needSecret bool) (deliveryParams, *EventsError) {
	var d deliveryParams
	if len(raw) == 0 || string(raw) == "null" {
		return d, invalidParams("delivery is required")
	}
	if json.Unmarshal(raw, &d) != nil {
		return d, invalidParams("delivery must be an object of strings")
	}
	if d.URL == "" {
		return d, invalidParams("delivery.url is required")
	}
	if needSecret && d.Secret == "" {
		return d, invalidParams("delivery.secret is required")
	}
	return d, nil
}

// grant is the lifetime granted for a suggestion: the server's TTL when
// there is none or it is null, and otherwise the suggestion between
// 60 seconds and the TTL.
func (w *webhookEngine) grant(raw json.RawMessage) (time.Duration, error) {
	n, ok, err := wholeNumber(raw, "ttlMs", 0)
	if err != nil {
		return 0, err
	}
	if !ok {
		return w.o.TTL, nil
	}
	return min(max(time.Duration(n)*time.Millisecond, min(ttlFloor, w.o.TTL)), w.o.TTL), nil
}

// subscribe answers one events/subscribe: it creates the subscription or
// refreshes the one with the same key.
func (w *webhookEngine) subscribe(ctx context.Context, ss *sdk.ServerSession, p *eventsParams) (*subscribeResult, error) {
	principal := w.principal(ctx, ss)
	if principal == "" {
		return nil, forbiddenNoPrincipal(EventsSubscribeMethod)
	}
	if p == nil || p.Name == "" {
		return nil, invalidParams("events/subscribe needs a name")
	}
	t := w.e.lookup(p.Name)
	if t == nil {
		return nil, notFoundEvent(p.Name)
	}
	d, ee := parseDelivery(p.Delivery, true)
	if ee != nil {
		return nil, ee
	}
	if d.Mode != DeliveryWebhook {
		if d.Mode == "" {
			return nil, invalidParams("delivery.mode is required")
		}
		return nil, unsupportedDelivery(d.Mode)
	}
	if !t.delivers(DeliveryWebhook) {
		return nil, unsupportedDelivery(DeliveryWebhook)
	}
	u, err := w.guard.checkURL(d.URL)
	if err != nil {
		return nil, invalidParams("%v", err)
	}
	key, err := parseWebhookSecret(d.Secret)
	if err != nil {
		return nil, invalidParams("%v", err)
	}
	args := p.Arguments
	if len(args) == 0 || string(args) == "null" {
		args = json.RawMessage("{}")
	}
	if err := t.input.validate(args); err != nil {
		return nil, invalidParams("arguments of %q: %v", p.Name, err)
	}
	var cursor *string
	if len(p.Cursor) > 0 && string(p.Cursor) != "null" {
		var c string
		if json.Unmarshal(p.Cursor, &c) != nil {
			return nil, invalidParams("cursor must be a string or null")
		}
		cursor = &c
	}
	var floor time.Time
	if n, ok, err := wholeNumber(p.MaxAgeMs, "maxAgeMs", 0); err != nil {
		return nil, err
	} else if ok && n > 0 && cursor != nil {
		floor = w.now().Add(-time.Duration(n) * time.Millisecond)
	}
	ttl, err := w.grant(p.TTLMs)
	if err != nil {
		return nil, err
	}

	id := subscriptionID(principal, d.URL, p.Name, args)
	defer w.lockID(id)()
	now := w.now()
	trusted := w.guard.isTrusted(u)

	rec, found, err := w.store.Get(ctx, id)
	if err != nil {
		return nil, w.internal("the subscription could not be read", err)
	}
	if found && !rec.Expires.After(now) {
		w.expire(rec)
		found = false
	}

	res := &subscribeResult{ResultType: "complete", ID: id}
	pair := pairKey(principal, d.URL)
	live := w.tracked(id)
	if found && live != nil {
		// A refresh. The secret is replaced, and the old one still
		// signs for a while, so a delivery in flight verifies either
		// way.
		if rec.Secret != d.Secret {
			rec.PreviousSecret, rec.PreviousExpires = rec.Secret, now.Add(rotationGrace)
			rec.Secret = d.Secret
		}
		res.DeliveryStatus = live.status()
		prev, prevUntil := previousKey(rec, now)
		live.setSecrets(key, prev, prevUntil)
		rec.Expires = now.Add(ttl)
		if err := w.store.Put(ctx, rec); err != nil {
			return nil, w.internal("the subscription could not be kept", err)
		}
		w.arm(live, rec.Expires)
		live.resume()
		c, gap := live.position()
		res.Cursor, res.Truncated = &c, gap
		ref := rec.Expires.UTC().Format(time.RFC3339)
		res.RefreshBefore = &ref
		return res, nil
	}

	// A subscription that is new here: made, or recovered from a store
	// this process did not write. Capacity comes before any hook.
	if live != nil {
		// The store lost the record, since another replica ended the
		// subscription: what this process holds of it is over.
		live.end()
	}
	if w.count() >= w.o.MaxSubscriptions {
		return nil, &EventsError{Code: EventsCodeResourceExhausted, Message: "too many subscriptions", Data: map[string]any{"limit": "subscriptions", "max": w.o.MaxSubscriptions}}
	}
	if ee := w.verify(ctx, principal, d.URL, id, key, trusted); ee != nil {
		return nil, ee
	}
	rec = WebhookRecord{
		ID: id, Principal: principal, URL: d.URL, Name: p.Name, Arguments: args,
		Secret: d.Secret, Created: now, Expires: now.Add(ttl),
	}
	ec := EventContext{Name: p.Name, Principal: principal, Subscription: id, Mode: DeliveryWebhook}
	if t.def.OnSubscribe != nil {
		if err := t.def.OnSubscribe(ctx, ec, args); err != nil {
			var ee *EventsError
			if !errors.As(err, &ee) {
				w.e.opts.OnError(fmt.Errorf("mcpserver: events: %q: OnSubscribe: %w", p.Name, err))
				ee = &EventsError{Code: -32603, Message: "the subscription could not be made"}
			}
			return nil, ee
		}
	}
	tr := &tracked{w: w, typ: t, pair: pair, ec: ec, args: args, url: d.URL, trusted: trusted, secret: key}
	if err := w.begin(tr, cursor, floor, res); err != nil {
		tr.unsubscribeHook()
		return nil, err
	}
	if err := w.store.Put(ctx, rec); err != nil {
		w.dropLocal(tr)
		tr.unsubscribeHook()
		return nil, w.internal("the subscription could not be kept", err)
	}
	w.arm(tr, rec.Expires)
	ref := rec.Expires.UTC().Format(time.RFC3339)
	res.RefreshBefore = &ref
	return res, nil
}

func previousKey(rec WebhookRecord, now time.Time) ([]byte, time.Time) {
	if rec.PreviousSecret == "" || !now.Before(rec.PreviousExpires) {
		return nil, time.Time{}
	}
	k, err := parseWebhookSecret(rec.PreviousSecret)
	if err != nil {
		return nil, time.Time{}
	}
	return k, rec.PreviousExpires
}

func (w *webhookEngine) internal(what string, err error) *EventsError {
	w.e.opts.OnError(fmt.Errorf("mcpserver: events: webhook: %s: %w", what, err))
	return &EventsError{Code: -32603, Message: what}
}

func (w *webhookEngine) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.subs)
}

// begin makes tr live: it takes its place, covers its pair, watches the
// ring and reads from the cursor, or from now when there is none.
func (w *webhookEngine) begin(tr *tracked, cursor *string, floor time.Time, res *subscribeResult) *EventsError {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return &EventsError{Code: -32603, Message: "the server is closing"}
	}
	w.subs[tr.ec.Subscription] = tr
	w.all[tr] = struct{}{}
	w.mu.Unlock()
	w.verifier.cover(tr.pair)

	r := tr.ring()
	tr.draining.Store(true)
	unwatch := r.watch(tr.signal)
	tr.mu.Lock()
	tr.unwatch = unwatch
	tr.mu.Unlock()
	if cursor == nil {
		head := r.read(nil, 0, time.Time{}, nil)
		tr.mu.Lock()
		tr.pos, _ = r.parse(head.cursor)
		tr.mu.Unlock()
	}
	res.Truncated = false
	if cursor != nil {
		res.Truncated = tr.drain(cursor, floor, true)
	}
	tr.draining.Store(false)
	tr.signal()
	c, _ := tr.position()
	res.Cursor = &c
	return nil
}

// dropLocal ends tr without a trace in the store.
func (w *webhookEngine) dropLocal(tr *tracked) { tr.end() }

// arm has the subscription looked at when its grant ends.
func (w *webhookEngine) arm(tr *tracked, expires time.Time) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if tr.ended {
		return
	}
	if tr.stopExpiry != nil {
		tr.stopExpiry()
	}
	id := tr.ec.Subscription
	tr.stopExpiry = w.afterFunc(max(expires.Sub(w.now()), 0), func() { w.expireByID(id) })
}

// expireByID is the timer of a grant: it re-reads the store, so a
// refresh that another replica made counts, and expires the record when
// the grant has ended.
func (w *webhookEngine) expireByID(id string) {
	w.mu.Lock()
	closed := w.closed
	w.mu.Unlock()
	if closed {
		return
	}
	defer w.lockID(id)()
	rec, ok, err := w.store.Get(w.ctx, id)
	if err != nil {
		w.e.opts.OnError(fmt.Errorf("mcpserver: events: webhook: expiry: %w", err))
		return
	}
	if !ok {
		if tr := w.tracked(id); tr != nil {
			tr.end()
		}
		return
	}
	if rec.Expires.After(w.now()) {
		if tr := w.tracked(id); tr != nil {
			w.arm(tr, rec.Expires)
		}
		return
	}
	w.expire(rec)
}

// expire removes a record whose grant has ended, ends its delivery and
// calls OnUnsubscribe, once even when replicas race: the one whose
// delete finds the record does.
func (w *webhookEngine) expire(rec WebhookRecord) {
	deleted, err := w.store.Delete(w.ctx, rec.ID)
	if err != nil {
		w.e.opts.OnError(fmt.Errorf("mcpserver: events: webhook: expiry: %w", err))
		return
	}
	tr := w.tracked(rec.ID)
	if tr != nil {
		tr.end()
	}
	if deleted {
		w.unsubscribeHook(rec, tr)
	}
}

func (w *webhookEngine) unsubscribeHook(rec WebhookRecord, tr *tracked) {
	if tr == nil {
		t := w.e.lookup(rec.Name)
		if t == nil {
			return
		}
		tr = &tracked{typ: t, w: w, args: rec.Arguments, ec: EventContext{Name: rec.Name, Principal: rec.Principal, Subscription: rec.ID, Mode: DeliveryWebhook}}
	}
	tr.unsubscribeHook()
}

// unsubscribe answers one events/unsubscribe.
func (w *webhookEngine) unsubscribe(ctx context.Context, ss *sdk.ServerSession, p *eventsParams) (*emptyEventsResult, error) {
	principal := w.principal(ctx, ss)
	if principal == "" {
		return nil, forbiddenNoPrincipal(EventsUnsubscribeMethod)
	}
	if p == nil || p.Name == "" {
		return nil, invalidParams("events/unsubscribe needs a name")
	}
	d, ee := parseDelivery(p.Delivery, false)
	if ee != nil {
		return nil, ee
	}
	args := p.Arguments
	if len(args) == 0 || string(args) == "null" {
		args = json.RawMessage("{}")
	}
	if !json.Valid(args) {
		return nil, invalidParams("arguments must be JSON")
	}
	id := subscriptionID(principal, d.URL, p.Name, args)
	defer w.lockID(id)()
	rec, ok, err := w.store.Get(ctx, id)
	if err != nil {
		return nil, w.internal("the subscription could not be read", err)
	}
	deleted := false
	if ok {
		if deleted, err = w.store.Delete(ctx, id); err != nil {
			return nil, w.internal("the subscription could not be removed", err)
		}
	}
	if !deleted || !rec.Expires.After(w.now()) {
		if tr := w.tracked(id); tr != nil {
			tr.end()
		}
		kind := "subscription"
		if w.e.lookup(p.Name) == nil {
			kind = "event"
		}
		return nil, &EventsError{Code: EventsCodeNotFound, Message: "no such subscription", Data: map[string]any{"kind": kind}}
	}
	tr := w.tracked(id)
	if tr != nil {
		tr.end()
	}
	w.unsubscribeHook(rec, tr)
	return &emptyEventsResult{ResultType: "complete"}, nil
}

// terminate ends the subscriptions to an event type with cause: each
// record is deleted, and the one that deleted it sends the client a
// terminated envelope, which is retried like a delivery.
func (w *webhookEngine) terminate(name string, cause *EventsError) {
	recs, err := w.store.ListByName(w.ctx, name)
	if err != nil {
		w.e.opts.OnError(fmt.Errorf("mcpserver: events: webhook: terminate %q: %w", name, err))
	}
	for _, rec := range recs {
		w.terminateOne(rec, cause)
	}
}

func (w *webhookEngine) terminateOne(rec WebhookRecord, cause *EventsError) {
	defer w.lockID(rec.ID)()
	deleted, err := w.store.Delete(w.ctx, rec.ID)
	if err != nil {
		w.e.opts.OnError(fmt.Errorf("mcpserver: events: webhook: terminate: %w", err))
		return
	}
	tr := w.tracked(rec.ID)
	if !deleted {
		if tr != nil {
			tr.end()
		}
		return
	}
	if tr == nil {
		// A subscription this process did not start, which a replica
		// that shares the store made: it can still send the envelope.
		key, kerr := parseWebhookSecret(rec.Secret)
		if kerr != nil {
			return
		}
		u, uerr := w.guard.checkURL(rec.URL)
		if uerr != nil {
			return
		}
		prev, prevUntil := previousKey(rec, w.now())
		tr = &tracked{
			w: w, typ: w.e.lookup(rec.Name), pair: pairKey(rec.Principal, rec.URL), args: rec.Arguments, url: rec.URL,
			trusted: w.guard.isTrusted(u), secret: key, prev: prev, prevUntil: prevUntil,
			ec: EventContext{Name: rec.Name, Principal: rec.Principal, Subscription: rec.ID, Mode: DeliveryWebhook},
		}
		w.mu.Lock()
		if w.closed {
			w.mu.Unlock()
			return
		}
		w.all[tr] = struct{}{}
		w.mu.Unlock()
		w.verifier.cover(tr.pair)
	}
	w.mu.Lock()
	if w.subs[rec.ID] == tr {
		delete(w.subs, rec.ID)
	}
	w.mu.Unlock()
	tr.terminate(cause)
	tr.unsubscribeHook()
}

// idleForTest says no delivery goroutine runs.
func (w *webhookEngine) idleForTest() bool { return w.busy.Load() == 0 }

// TrustWebhookOrigin adds origin, "scheme://host[:port]", to the
// [WebhookOptions.TrustedOrigins] of a server that is running, for an
// origin that is known only then, such as a receiver a test starts on a
// port the system picks. It is as the option says: callbacks under it
// may use http and a special-purpose address, and the handshake, the
// signature and the refusal of redirects still apply. A subscription
// already made keeps the trust it was made with. It fails for an origin
// that is not of that form, and where the webhook mode is off.
//
// This turns off the server's defence against a client that names a
// callback to reach what the server can reach, for that origin, and
// nothing takes the trust back. Call it only from code the embedder
// controls, with an origin the embedder chose, as a test does for a
// receiver on a port the system picked. Never call it with a value a
// client sent, and never from a tool, route or handler a client can
// reach.
func (e *Events) TrustWebhookOrigin(origin string) error {
	if e.hooks == nil {
		return errors.New("mcpserver: events: the webhook mode is off")
	}
	return e.hooks.guard.trust(origin)
}
