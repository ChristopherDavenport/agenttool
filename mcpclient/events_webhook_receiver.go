package mcpclient

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// WebhookKind says what a [WebhookMessage] is.
type WebhookKind string

const (
	// WebhookEvent is an event.
	WebhookEvent WebhookKind = "event"
	// WebhookGap says events may have been lost: Cursor is the position
	// to keep, as a poll's truncated says.
	WebhookGap WebhookKind = "gap"
	// WebhookTerminated says the subscription is over, with Error as
	// the reason: [EventsCodeForbidden] to stop, [EventsCodeNotFound]
	// or [EventsCodeUnsupported] to list again and subscribe anew.
	WebhookTerminated WebhookKind = "terminated"
)

// WebhookMessage is a verified delivery, as a receiver hands it on.
type WebhookMessage struct {
	Kind WebhookKind
	// SubscriptionID is the X-MCP-Subscription-Id of the delivery, and
	// MessageID its webhook-id.
	SubscriptionID string
	MessageID      string
	// Event is set for a [WebhookEvent].
	Event *EventOccurrence
	// Cursor is the position to keep: the watermark of an event, the
	// fresh cursor of a gap. Nil for a type that does not replay. The
	// consumer must keep it, with the event: cursor recovery depends on
	// it.
	Cursor *string
	// Error is set for a [WebhookTerminated].
	Error *EventError
}

// WebhookSecrets says which secrets a receiver verifies with. A
// subscription's secret is the client's, and the server's id for the
// subscription is not known until subscribe answers, so the first
// message, the verification challenge, carries an id the receiver has
// not been told: it is verified against the secrets that no id has been
// bound to, and the id is bound to the one that verifies.
type WebhookSecrets interface {
	// SecretsFor returns the secrets registered for a subscription id,
	// and false when the id is not known.
	SecretsFor(id string) ([]string, bool)
	// Unbound returns the secrets registered with no id yet.
	Unbound() []string
	// Bind records that the subscription id is the one of secret.
	Bind(id, secret string)
}

// MemorySecrets is a [WebhookSecrets] in memory.
type MemorySecrets struct {
	mu      sync.Mutex
	byID    map[string][]string
	unbound []string
}

// NewMemorySecrets returns an empty [MemorySecrets].
func NewMemorySecrets() *MemorySecrets { return &MemorySecrets{byID: map[string][]string{}} }

// Add registers a secret before its subscription is made, so the
// challenge can be answered.
func (m *MemorySecrets) Add(secret string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.unbound {
		if s == secret {
			return
		}
	}
	m.unbound = append(m.unbound, secret)
}

// Remove forgets a secret and the subscriptions bound to it.
func (m *MemorySecrets) Remove(secret string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.unbound = removeString(m.unbound, secret)
	for id, ss := range m.byID {
		if ss = removeString(ss, secret); len(ss) == 0 {
			delete(m.byID, id)
		} else {
			m.byID[id] = ss
		}
	}
}

func removeString(ss []string, s string) []string {
	out := ss[:0:0]
	for _, v := range ss {
		if v != s {
			out = append(out, v)
		}
	}
	return out
}

// SecretsFor implements [WebhookSecrets].
func (m *MemorySecrets) SecretsFor(id string) ([]string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ss, ok := m.byID[id]
	return append([]string(nil), ss...), ok
}

// Unbound implements [WebhookSecrets].
func (m *MemorySecrets) Unbound() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.unbound...)
}

// Bind implements [WebhookSecrets]: the secret is bound to id, and a
// secret may be bound to several ids, since one URL and secret can serve
// several subscriptions.
func (m *MemorySecrets) Bind(id, secret string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.byID[id] {
		if s == secret {
			return
		}
	}
	m.byID[id] = append(m.byID[id], secret)
}

// WebhookReceiverOptions are the settings of a [NewWebhookReceiver].
type WebhookReceiverOptions struct {
	// Secrets says which secrets to verify with; required.
	Secrets WebhookSecrets
	// Handle is called with each verified, fresh delivery that is an
	// event, a gap or a termination, before the receiver answers 2xx: an
	// answer says the message was accepted, so Handle should keep it
	// durably, or queue it, and return. An error is answered with 500, so
	// the server retries. It is required.
	Handle func(ctx context.Context, m WebhookMessage) error
	// Now is the clock; nil is [time.Now].
	Now func() time.Time
	// Tolerance is how far a webhook-timestamp may be from now, either
	// way; the default is 5 minutes, which is what the SEP says.
	Tolerance time.Duration
	// MaxBody is the largest body read; the default is 256 KiB, the
	// SEP's size for a delivery, and a larger one is answered with 413.
	MaxBody int64
	// Seen bounds how many webhook ids, and how many event ids of each of
	// 1024 subscriptions, are remembered for deduplication; the default
	// is 4096.
	Seen int
}

const (
	defaultWebhookTolerance = 5 * time.Minute
	defaultWebhookBody      = 256 << 10
	maxWebhookSubs          = 1024
)

type webhookReceiver struct {
	o WebhookReceiverOptions

	mu       sync.Mutex
	ids      *seenSet
	inflight map[string]bool
	events   map[string]*seenSet
}

// NewWebhookReceiver returns the [http.Handler] of a webhook callback.
// It answers POST alone, and for each delivery:
//
//   - reads the raw body and verifies webhook-signature, Standard
//     Webhooks v1 with any of a space-separated list, against the raw
//     bytes, with the secrets of the X-MCP-Subscription-Id, before it
//     parses anything; a failure is 401;
//   - refuses a webhook-timestamp more than five minutes from now, 400;
//   - answers the verification challenge by echoing it, and binds the
//     id of the challenge to the secret that verified it;
//   - answers 503, which the server retries, for a delivery for an id it
//     has not been told of, since the subscribe answer may not have
//     reached it yet;
//   - drops a webhook-id it has handled, answering 200, and an event id
//     it has handled for the subscription;
//   - hands an event, a gap or a termination to Handle, and answers 200
//     once Handle returns nil.
//
// What it does not do is authenticate the sender any other way: the
// signature is the only authentication the extension has. Put it behind
// TLS and give it a path of its own.
func NewWebhookReceiver(o WebhookReceiverOptions) http.Handler {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Tolerance <= 0 {
		o.Tolerance = defaultWebhookTolerance
	}
	if o.MaxBody <= 0 {
		o.MaxBody = defaultWebhookBody
	}
	if o.Seen <= 0 {
		o.Seen = defaultEventSeen
	}
	return &webhookReceiver{o: o, ids: newSeenSet(o.Seen), inflight: map[string]bool{}, events: map[string]*seenSet{}}
}

func (r *webhookReceiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	msgID := req.Header.Get("webhook-id")
	tsHeader := req.Header.Get("webhook-timestamp")
	sigHeader := req.Header.Get("webhook-signature")
	subID := req.Header.Get("X-MCP-Subscription-Id")
	if msgID == "" || tsHeader == "" || sigHeader == "" || subID == "" {
		http.Error(w, "missing webhook headers", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, r.o.MaxBody+1))
	if err != nil {
		http.Error(w, "unreadable body", http.StatusBadRequest)
		return
	}
	if int64(len(body)) > r.o.MaxBody {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}
	ts, err := strconv.ParseInt(tsHeader, 10, 64)
	if err != nil {
		http.Error(w, "bad webhook-timestamp", http.StatusBadRequest)
		return
	}
	if d := r.o.Now().Sub(time.Unix(ts, 0)); d > r.o.Tolerance || d < -r.o.Tolerance {
		http.Error(w, "webhook-timestamp is not fresh", http.StatusBadRequest)
		return
	}

	// The signature, before the body is read as anything.
	known, secrets := r.candidates(subID)
	secret, ok := verifyWebhook(secrets, msgID, tsHeader, sigHeader, body)
	if !ok {
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}
	var head struct {
		Type      string          `json:"type"`
		Challenge string          `json:"challenge"`
		Error     *wireError      `json:"error"`
		Cursor    *string         `json:"cursor"`
		EventID   string          `json:"eventId"`
		Name      string          `json:"name"`
		Timestamp string          `json:"timestamp"`
		Data      json.RawMessage `json:"data"`
		Meta      map[string]any  `json:"_meta"`
	}
	if err := json.Unmarshal(body, &head); err != nil {
		http.Error(w, "the body is not JSON", http.StatusBadRequest)
		return
	}
	if head.Type == "verification" {
		// Consumed here, not forwarded; the id is the server's and new.
		if r.o.Secrets != nil {
			r.o.Secrets.Bind(subID, secret)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"challenge": head.Challenge})
		return
	}
	if !known {
		// Verified against a secret nobody has bound: the subscribe
		// answer may not have arrived yet. The server retries.
		http.Error(w, "unknown subscription", http.StatusServiceUnavailable)
		return
	}

	msg := WebhookMessage{SubscriptionID: subID, MessageID: msgID, Cursor: head.Cursor}
	switch head.Type {
	case "":
		ts, _ := time.Parse(time.RFC3339Nano, head.Timestamp)
		msg.Kind = WebhookEvent
		msg.Event = &EventOccurrence{EventID: head.EventID, Name: head.Name, Timestamp: ts, Data: head.Data, Meta: head.Meta}
	case "gap":
		msg.Kind = WebhookGap
	case "terminated":
		msg.Kind = WebhookTerminated
		msg.Error = &EventError{Code: -32603, Message: "terminated"}
		if e := head.Error; e != nil {
			msg.Error = &EventError{Code: int(e.Code), Message: e.Message, Data: e.Data}
		}
	default:
		// A control envelope this client does not know: acknowledged, so
		// the server does not retry what a newer SEP revision added.
		w.WriteHeader(http.StatusOK)
		return
	}

	key := subID + "\x00" + msgID
	r.mu.Lock()
	if r.inflight[key] {
		r.mu.Unlock()
		http.Error(w, "in progress", http.StatusServiceUnavailable)
		return
	}
	if r.ids.has(key) {
		r.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		return
	}
	dupEvent := msg.Kind == WebhookEvent && r.eventSeen(subID).has(head.EventID)
	if dupEvent {
		r.ids.add(key)
		r.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		return
	}
	r.inflight[key] = true
	r.mu.Unlock()

	err = r.o.Handle(req.Context(), msg)

	r.mu.Lock()
	delete(r.inflight, key)
	if err == nil {
		r.ids.add(key)
		if msg.Kind == WebhookEvent {
			r.eventSeen(subID).add(head.EventID)
		}
	}
	r.mu.Unlock()
	if err != nil {
		http.Error(w, "the delivery was not accepted", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// wireError is an error object as a terminated envelope carries it.
type wireError struct {
	Code    int64           `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// candidates are the secrets to verify a delivery for id with, and
// whether the id is known. An unknown id is verified against the secrets
// with no id yet, which is how the first challenge is.
func (r *webhookReceiver) candidates(id string) (bool, []string) {
	if r.o.Secrets == nil {
		return false, nil
	}
	if ss, ok := r.o.Secrets.SecretsFor(id); ok {
		return true, ss
	}
	return false, r.o.Secrets.Unbound()
}

// eventSeen is the event ids seen of a subscription; the caller holds mu.
func (r *webhookReceiver) eventSeen(id string) *seenSet {
	s := r.events[id]
	if s == nil {
		if len(r.events) >= maxWebhookSubs {
			for k := range r.events {
				delete(r.events, k)
				break
			}
		}
		s = newSeenSet(r.o.Seen)
		r.events[id] = s
	}
	return s
}

// verifyWebhook checks a webhook-signature header against the raw body:
// HMAC-SHA256 over "id.timestamp.body" with the key of each secret, a
// match with any "v1," signature of the header, and returns the secret
// that verified. The comparison is constant time.
func verifyWebhook(secrets []string, id, ts, header string, body []byte) (string, bool) {
	var sigs [][]byte
	for _, f := range strings.Fields(header) {
		if enc, ok := strings.CutPrefix(f, "v1,"); ok {
			if b, err := base64.StdEncoding.DecodeString(enc); err == nil {
				sigs = append(sigs, b)
			}
		}
	}
	if len(sigs) == 0 {
		return "", false
	}
	found := ""
	ok := false
	for _, s := range secrets {
		key, err := decodeSecret(s)
		if err != nil {
			continue
		}
		m := hmac.New(sha256.New, key)
		m.Write([]byte(id + "." + ts + "."))
		m.Write(body)
		sum := m.Sum(nil)
		for _, sig := range sigs {
			if hmac.Equal(sig, sum) && !ok {
				found, ok = s, true
			}
		}
	}
	return found, ok
}

func decodeSecret(s string) ([]byte, error) {
	enc, ok := strings.CutPrefix(s, webhookSecretPrefix)
	if !ok {
		return nil, errors.New("a webhook secret starts with whsec_")
	}
	key, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return nil, fmt.Errorf("a webhook secret is base64: %w", err)
	}
	return key, nil
}
