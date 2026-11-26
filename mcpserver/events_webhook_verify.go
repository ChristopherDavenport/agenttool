package mcpserver

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// The limits of the verification handshake, per destination host: one
// handshake at a time, at most ten in 60 seconds, at most ten waiting
// for their turn, and a pair that failed is not tried again for 60
// seconds. They are the bounds of what a client can aim at a victim by
// subscribing with its URL.
const (
	verifyPerWindow  = 10
	verifyWindow     = 60 * time.Second
	verifyMaxWaiting = 10
	verifyFailedFor  = 60 * time.Second
	verifyMaxFailed  = 10000
	verifyLingerFor  = 10 * time.Minute
)

// verifier remembers which (principal, URL) pairs have shown they want
// deliveries and runs the handshake that shows it: a signed
// "verification" envelope with a single-use nonce that the endpoint
// echoes in a 2xx body. It is soft state, as the subscriptions it
// covers are: a restart verifies again.
type verifier struct {
	mu       sync.Mutex
	verified map[string]*verifiedPair
	failed   map[string]failedPair
	inflight map[string]*handshake
	hosts    map[string]*hostGate
}

type verifiedPair struct {
	// refs is how many subscriptions the verification covers, and at is
	// when it was made: one that covers none is dropped after a while.
	refs int
	at   time.Time
}

type failedPair struct {
	until time.Time
	err   *EventsError
}

type handshake struct {
	done chan struct{}
	err  *EventsError
}

// hostGate limits the handshakes aimed at one host.
type hostGate struct {
	sem     chan struct{}
	waiting int
	starts  []time.Time
}

func newVerifier() *verifier {
	return &verifier{
		verified: map[string]*verifiedPair{}, failed: map[string]failedPair{},
		inflight: map[string]*handshake{}, hosts: map[string]*hostGate{},
	}
}

func pairKey(principal, url string) string { return principal + "\x00" + url }

func callbackError(reason string) *EventsError {
	return &EventsError{
		Code: EventsCodeCallbackEndpoint, Message: "the callback endpoint did not verify",
		Data: map[string]any{"reason": reason},
	}
}

// cover says one more subscription is covered by the pair's
// verification, and uncover that one fewer is; the pair is forgotten
// with its last.
func (v *verifier) cover(pair string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if p := v.verified[pair]; p != nil {
		p.refs++
	}
}

func (v *verifier) uncover(pair string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if p := v.verified[pair]; p != nil {
		if p.refs--; p.refs <= 0 {
			delete(v.verified, pair)
		}
	}
}

// verify makes sure the pair is verified, running the handshake for sub,
// signed with key, when it is not. Callers of the same pair share one
// handshake.
func (w *webhookEngine) verify(ctx context.Context, principal, callback, sub string, key []byte, trusted bool) *EventsError {
	pair := pairKey(principal, callback)
	v := w.verifier
	now := w.now()

	v.mu.Lock()
	for k, p := range v.verified {
		if p.refs <= 0 && now.Sub(p.at) > verifyLingerFor {
			delete(v.verified, k)
		}
	}
	if v.verified[pair] != nil {
		v.mu.Unlock()
		return nil
	}
	if f, ok := v.failed[pair]; ok {
		if now.Before(f.until) {
			v.mu.Unlock()
			return f.err
		}
		delete(v.failed, pair)
	}
	if h := v.inflight[pair]; h != nil {
		v.mu.Unlock()
		select {
		case <-h.done:
			return h.err
		case <-ctx.Done():
			return callbackError(failTimeout)
		}
	}
	h := &handshake{done: make(chan struct{})}
	v.inflight[pair] = h
	v.mu.Unlock()

	err := w.handshake(ctx, callback, sub, key, trusted)

	v.mu.Lock()
	delete(v.inflight, pair)
	h.err = err
	switch {
	case err == nil:
		v.verified[pair] = &verifiedPair{at: now}
	case err.Code == EventsCodeCallbackEndpoint:
		if len(v.failed) >= verifyMaxFailed {
			for k := range v.failed {
				delete(v.failed, k)
				break
			}
		}
		v.failed[pair] = failedPair{until: now.Add(verifyFailedFor), err: err}
	}
	v.mu.Unlock()
	close(h.done)
	return err
}

// handshake posts the challenge to callback, within the limits of its
// host, and checks the echo.
func (w *webhookEngine) handshake(ctx context.Context, callback, sub string, key []byte, trusted bool) *EventsError {
	u, err := url.Parse(callback)
	if err != nil {
		return invalidParams("delivery.url is not a URL")
	}
	gate, ee := w.enterHost(u.Host)
	if ee != nil {
		return ee
	}
	if ee := w.acquireHost(ctx, gate); ee != nil {
		return ee
	}
	defer func() { <-gate.sem }()

	var nonce [32]byte
	_, _ = rand.Read(nonce[:])
	challenge := base64.RawURLEncoding.EncodeToString(nonce[:])
	body, _ := json.Marshal(map[string]string{"type": "verification", "challenge": challenge})
	resp, serr := w.post(ctx, callback, sub, messageID("verification"), body, [][]byte{key}, trusted)
	if serr != nil {
		return callbackError(serr.Category)
	}
	if resp.Status < 200 || resp.Status > 299 {
		return callbackError(statusCategory(resp.Status))
	}
	var echo struct {
		Challenge string `json:"challenge"`
	}
	if json.Unmarshal(resp.Body, &echo) != nil || subtle.ConstantTimeCompare([]byte(echo.Challenge), []byte(challenge)) != 1 {
		return callbackError(failChallenge)
	}
	return nil
}

// enterHost counts a handshake against host, at most ten in the window,
// and returns the gate whose slot it is to take. The wait for the slot
// is in acquireHost.
func (w *webhookEngine) enterHost(host string) (*hostGate, *EventsError) {
	v := w.verifier
	now := w.now()
	v.mu.Lock()
	defer v.mu.Unlock()
	g := v.hosts[host]
	if g == nil {
		if len(v.hosts) >= verifyMaxFailed {
			for k, o := range v.hosts {
				if o.waiting == 0 && len(o.sem) == 0 {
					delete(v.hosts, k)
				}
			}
		}
		g = &hostGate{sem: make(chan struct{}, 1)}
		v.hosts[host] = g
	}
	live := g.starts[:0]
	for _, s := range g.starts {
		if now.Sub(s) < verifyWindow {
			live = append(live, s)
		}
	}
	g.starts = live
	exhausted := func(wait time.Duration) *EventsError {
		return &EventsError{
			Code: EventsCodeResourceExhausted, Message: "too many verifications for the host",
			Data: map[string]any{"limit": "verification", "retryAfterMs": wait.Milliseconds()},
		}
	}
	if len(g.starts) >= verifyPerWindow {
		return nil, exhausted(verifyWindow - now.Sub(g.starts[0]))
	}
	if g.waiting >= verifyMaxWaiting {
		return nil, exhausted(time.Second)
	}
	g.starts = append(g.starts, now)
	g.waiting++
	return g, nil
}

// acquireHost waits for the gate's one slot, which the caller releases
// by receiving from gate.sem once this returns nil.
func (w *webhookEngine) acquireHost(ctx context.Context, g *hostGate) *EventsError {
	select {
	case g.sem <- struct{}{}:
		w.verifier.mu.Lock()
		g.waiting--
		w.verifier.mu.Unlock()
		return nil
	case <-ctx.Done():
		w.verifier.mu.Lock()
		g.waiting--
		w.verifier.mu.Unlock()
		return callbackError(failTimeout)
	}
}

// post signs and sends one message to callback, and returns the answer,
// or the category it failed in.
func (w *webhookEngine) post(ctx context.Context, callback, sub, id string, body []byte, keys [][]byte, trusted bool) (webhookResponse, *sendError) {
	ts := w.now().Unix()
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("webhook-id", id)
	h.Set("webhook-timestamp", strconv.FormatInt(ts, 10))
	h.Set("webhook-signature", signWebhook(keys, id, ts, body))
	h.Set("X-MCP-Subscription-Id", sub)
	resp, err := w.send(ctx, webhookRequest{URL: callback, Header: h, Body: body, Trusted: trusted})
	if err != nil {
		var se *sendError
		if !errors.As(err, &se) {
			se = &sendError{Category: failConnection, Err: err}
		}
		return webhookResponse{}, se
	}
	return resp, nil
}
