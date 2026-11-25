package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// What a subscription's delivery is held to. The retry schedule is the
// TypeScript reference's: five attempts, so four waits, which is under
// eight minutes in all; the suspension rule is the SEP's example.
const (
	maxAttempts        = 5
	maxInFlight        = 10
	maxHeld            = 10000
	maxBodyBytes       = 256 << 10
	suspendWindow      = 60 * time.Minute
	suspendMinAttempts = 100
	suspendPercent     = 95
	rotationGrace      = 300 * time.Second
	replayBatch        = 1000
)

var retryDelays = [maxAttempts - 1]time.Duration{5 * time.Second, 30 * time.Second, 120 * time.Second, 300 * time.Second}

const (
	kindEvent      = ""
	kindGap        = "gap"
	kindTerminated = "terminated"
)

// message is one POST to make, event or control, until it is
// acknowledged or given up on. Its webhook-id is made once, and kept by
// every retry.
type message struct {
	id   string
	kind string
	// seq is the position of an event in the ring, 0 for a control
	// envelope.
	seq uint64
	ev  Event
	// cause is the error of a terminated envelope.
	cause *EventsError

	attempts int
	running  bool
	// stop cancels the retry timer while the message waits for it.
	stop func() bool
}

func (m *message) waiting() bool { return m.stop != nil }

type attempt struct {
	at time.Time
	ok bool
}

// tracked is the delivery state of one webhook subscription in this
// process: the messages not yet resolved, the position it has read the
// ring to and the record of how its attempts went. A [WebhookStore] holds
// what must outlive the process; this is rebuilt from a refresh.
type tracked struct {
	w    *webhookEngine
	typ  *EventType
	pair string
	ec   EventContext
	args json.RawMessage
	url  string
	// trusted says the callback's origin is one the embedder listed.
	trusted bool

	// dirty and draining coalesce the signals of the ring: one drain
	// runs at a time, on the goroutine of an emitter, and reads all that
	// is new.
	dirty    atomic.Bool
	draining atomic.Bool

	mu          sync.Mutex
	unwatch     func()
	stopExpiry  func() bool
	secret      []byte
	prev        []byte
	prevUntil   time.Time
	pos         uint64
	msgs        []*message
	running     int
	gapPending  bool
	suspended   bool
	terminating bool
	ended       bool
	history     []attempt
	lastOK      time.Time
	lastErr     string
	failedSince time.Time
	overflowAt  time.Time
}

// keys are the secrets a message is signed with now: the current one and
// a replaced one still inside its grace.
func (t *tracked) keysLocked(now time.Time) [][]byte {
	keys := [][]byte{t.secret}
	if t.prev != nil && now.Before(t.prevUntil) {
		keys = append(keys, t.prev)
	}
	return keys
}

// setSecrets installs the record's secrets; the caller holds no lock.
func (t *tracked) setSecrets(secret, prev []byte, prevUntil time.Time) {
	t.mu.Lock()
	t.secret, t.prev, t.prevUntil = secret, prev, prevUntil
	t.mu.Unlock()
}

// ring is the type's ring, which an emit-only type has.
func (t *tracked) ring() *ring { return t.typ.ring }

// signal is what the ring calls after an event is appended. It reads
// the new events on this goroutine, unless another drain is on it, which
// then finds them.
func (t *tracked) signal() {
	t.dirty.Store(true)
	for t.draining.CompareAndSwap(false, true) {
		for t.dirty.Swap(false) {
			t.drain(nil, time.Time{}, false)
		}
		t.draining.Store(false)
		if !t.dirty.Load() {
			return
		}
	}
}

// drain reads the ring from the position the subscription has reached,
// or from, to the head, and queues what the subscription's Match and
// Transform let through. For the first read of a subscription, initial,
// a ring that has moved past the cursor is not a gap to send but the
// truncated its subscribe is answered with, which drain returns, and
// floor drops events older than it.
func (t *tracked) drain(from *string, floor time.Time, initial bool) (truncated bool) {
	r := t.ring()
	for {
		t.mu.Lock()
		if t.ended || t.terminating {
			t.mu.Unlock()
			return truncated
		}
		cur := from
		if cur == nil {
			c := r.format(t.pos)
			cur = &c
		}
		t.mu.Unlock()

		res := r.read(cur, replayBatch, floor, func(rec eventRecord) (Event, bool) {
			return t.typ.choose(t.ec, rec, t.args)
		})
		from = nil

		t.mu.Lock()
		if t.ended || t.terminating {
			t.mu.Unlock()
			return truncated
		}
		if pos, ok := r.parse(res.cursor); ok {
			t.pos = pos
		}
		for i, ev := range res.events {
			t.addLocked(&message{id: messageID(""), seq: res.seqs[i], ev: ev})
		}
		if res.truncated {
			if initial {
				truncated = true
			} else {
				t.gapPending = true
			}
		}
		t.pumpLocked()
		t.mu.Unlock()
		if !res.hasMore {
			return truncated
		}
	}
}

// addLocked queues m, dropping the oldest event that is waiting when
// the queue is full, which is a gap.
func (t *tracked) addLocked(m *message) {
	held := 0
	for _, o := range t.msgs {
		if o.kind == kindEvent {
			held++
		}
	}
	for held >= maxHeld {
		i := slices.IndexFunc(t.msgs, func(o *message) bool { return o.kind == kindEvent && !o.running })
		if i < 0 {
			break
		}
		t.resolveLocked(t.msgs[i])
		t.gapPending = true
		held--
		if now := t.w.now(); now.Sub(t.overflowAt) > time.Minute {
			t.overflowAt = now
			t.w.e.opts.OnError(fmt.Errorf("mcpserver: events: webhook %s holds %d messages and drops the oldest", t.ec.Subscription, maxHeld))
		}
	}
	t.msgs = append(t.msgs, m)
}

// pumpLocked starts the attempts that may start: messages that are not
// running and not waiting for a retry, at most maxInFlight at once, a
// gap envelope only when nothing else is in flight, nothing while
// delivery is suspended.
func (t *tracked) pumpLocked() {
	if t.ended || (t.suspended && !t.terminating) {
		return
	}
	for _, m := range t.msgs {
		if t.running >= maxInFlight {
			return
		}
		if m.running || m.waiting() {
			continue
		}
		if m.kind == kindGap && t.running > 0 {
			continue
		}
		m.running = true
		m.attempts++
		t.running++
		t.w.start(func() { t.attempt(m) })
	}
}

// watermarkLocked is the cursor every event at or before is resolved:
// just before the oldest event still in the queue, or at the position
// read when it is empty.
func (t *tracked) watermarkLocked() string {
	for _, m := range t.msgs {
		if m.kind == kindEvent {
			return t.ring().format(m.seq - 1)
		}
	}
	return t.ring().format(t.pos)
}

// eventBody is an event as a webhook delivers it.
type eventBody struct {
	EventID   string          `json:"eventId"`
	Name      string          `json:"name"`
	Timestamp string          `json:"timestamp"`
	Data      json.RawMessage `json:"data"`
	Cursor    *string         `json:"cursor"`
	Meta      map[string]any  `json:"_meta,omitempty"`
}

type gapBody struct {
	Type   string  `json:"type"`
	Cursor *string `json:"cursor"`
}

type terminatedBody struct {
	Type  string     `json:"type"`
	Error *wireError `json:"error"`
}

// bodyLocked is the JSON of m as it is sent now. An event carries its
// own position when it is the oldest unresolved one, and the watermark
// otherwise, so a cursor a client keeps never passes an event it has not
// been given.
func (t *tracked) bodyLocked(m *message) ([]byte, error) {
	switch m.kind {
	case kindGap:
		c := t.watermarkLocked()
		return json.Marshal(gapBody{Type: kindGap, Cursor: &c})
	case kindTerminated:
		return json.Marshal(terminatedBody{Type: kindTerminated, Error: m.cause.wireValue()})
	}
	c := t.watermarkLocked()
	if i := slices.IndexFunc(t.msgs, func(o *message) bool { return o.kind == kindEvent }); i >= 0 && t.msgs[i] == m {
		c = t.ring().format(m.seq)
	}
	return json.Marshal(eventBody{
		EventID: m.ev.ID, Name: t.ec.Name, Timestamp: m.ev.Time.UTC().Format(time.RFC3339Nano),
		Data: m.ev.Data, Cursor: &c, Meta: m.ev.Meta,
	})
}

// attempt makes one POST of m.
func (t *tracked) attempt(m *message) {
	t.mu.Lock()
	if t.ended {
		t.mu.Unlock()
		return
	}
	body, err := t.bodyLocked(m)
	if err == nil && len(body) > maxBodyBytes {
		err = fmt.Errorf("the body is %d bytes, over %d", len(body), maxBodyBytes)
	}
	if err != nil {
		t.running--
		m.running = false
		t.w.e.opts.OnError(fmt.Errorf("mcpserver: events: webhook %s: message dropped: %w", t.ec.Subscription, err))
		t.abandonLocked(m)
		t.pumpLocked()
		t.mu.Unlock()
		return
	}
	keys := t.keysLocked(t.w.now())
	t.mu.Unlock()

	resp, serr := t.w.post(t.w.ctx, t.url, t.ec.Subscription, m.id, body, keys, t.trusted)

	t.mu.Lock()
	defer t.mu.Unlock()
	t.running--
	m.running = false
	if t.ended {
		return
	}
	now := t.w.now()
	switch {
	case serr == nil && resp.Status >= 200 && resp.Status <= 299:
		t.recordLocked(now, true)
		t.lastOK, t.lastErr, t.failedSince = now, "", time.Time{}
		t.resolveLocked(m)
		switch m.kind {
		case kindTerminated:
			t.endLocked()
			return
		case kindGap:
		default:
			if t.gapPending && !slices.ContainsFunc(t.msgs, func(o *message) bool { return o.kind == kindGap }) {
				t.gapPending = false
				t.msgs = append(t.msgs, &message{id: messageID(kindGap), kind: kindGap})
			}
		}
	case serr == nil && resp.Status == 410:
		// The receiver does not want it, which is not a failure of
		// the subscription.
		t.resolveLocked(m)
	case serr == nil && resp.Status == 413:
		t.abandonLocked(m)
	default:
		cat := ""
		if serr != nil {
			cat = serr.Category
		} else {
			cat = statusCategory(resp.Status)
		}
		t.recordLocked(now, false)
		t.lastErr = cat
		if t.failedSince.IsZero() {
			t.failedSince = now
		}
		switch {
		case m.attempts >= maxAttempts:
			t.abandonLocked(m)
		case t.terminating:
			t.retryLocked(m)
		case t.checkSuspendLocked(now):
			// Parked until a refresh; the message keeps its attempts.
		default:
			t.retryLocked(m)
		}
	}
	t.pumpLocked()
}

// retryLocked has m wait for its next attempt.
func (t *tracked) retryLocked(m *message) {
	d := retryDelays[m.attempts-1]
	m.stop = t.w.afterFunc(d, func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		m.stop = nil
		t.pumpLocked()
	})
}

// resolveLocked removes m from the queue.
func (t *tracked) resolveLocked(m *message) {
	if m.stop != nil {
		m.stop()
		m.stop = nil
	}
	t.msgs = slices.DeleteFunc(t.msgs, func(o *message) bool { return o == m })
}

// abandonLocked gives up on m: an event not delivered is a gap, which
// the client is told of by a gap envelope after the next delivery that
// is acknowledged and by truncated on its next refresh.
func (t *tracked) abandonLocked(m *message) {
	t.resolveLocked(m)
	switch m.kind {
	case kindEvent, kindGap:
		t.gapPending = true
	case kindTerminated:
		t.endLocked()
	}
}

func (t *tracked) recordLocked(now time.Time, ok bool) {
	t.history = append(t.history, attempt{at: now, ok: ok})
	cut := 0
	for cut < len(t.history) && now.Sub(t.history[cut].at) > suspendWindow {
		cut++
	}
	t.history = t.history[cut:]
}

// checkSuspendLocked suspends delivery when more than 95% of at least
// 100 attempts in the last hour failed, and says whether it did. A
// brief outage does not suspend a healthy subscription, and a quiet one
// is not suspended on a handful of failures. The messages wait, with
// their timers stopped, for a refresh.
func (t *tracked) checkSuspendLocked(now time.Time) bool {
	if t.suspended {
		return true
	}
	if len(t.history) < suspendMinAttempts {
		return false
	}
	failed := 0
	for _, a := range t.history {
		if !a.ok {
			failed++
		}
	}
	if failed*100 <= suspendPercent*len(t.history) {
		return false
	}
	t.suspended = true
	for _, m := range t.msgs {
		if m.stop != nil {
			m.stop()
			m.stop = nil
		}
	}
	return true
}

// resume is what a refresh does to a subscription: delivery starts
// again, with the failures of the past forgotten.
func (t *tracked) resume() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.suspended {
		t.suspended = false
		t.history = nil
	}
	t.pumpLocked()
}

// deliveryStatus is what a refresh says of delivery.
type deliveryStatus struct {
	Active         bool    `json:"active"`
	LastDeliveryAt *string `json:"lastDeliveryAt"`
	LastError      *string `json:"lastError"`
	FailedSince    *string `json:"failedSince,omitempty"`
}

func (t *tracked) status() *deliveryStatus {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := &deliveryStatus{Active: !t.suspended}
	if !t.lastOK.IsZero() {
		v := t.lastOK.UTC().Format(time.RFC3339)
		s.LastDeliveryAt = &v
	}
	if t.lastErr != "" {
		v := t.lastErr
		s.LastError = &v
	}
	if !t.failedSince.IsZero() {
		v := t.failedSince.UTC().Format(time.RFC3339)
		s.FailedSince = &v
	}
	return s
}

// position is the watermark and whether a gap is waiting to be told.
func (t *tracked) position() (cursor string, gap bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	gap = t.gapPending || slices.ContainsFunc(t.msgs, func(o *message) bool { return o.kind == kindGap })
	return t.watermarkLocked(), gap
}

// terminate ends the subscription with a terminated envelope, which is
// retried like a delivery, and nothing after it.
func (t *tracked) terminate(cause *EventsError) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.ended || t.terminating {
		return
	}
	t.terminating = true
	if t.unwatch != nil {
		t.unwatch()
		t.unwatch = nil
	}
	if t.stopExpiry != nil {
		t.stopExpiry()
	}
	for _, m := range t.msgs {
		if m.stop != nil {
			m.stop()
			m.stop = nil
		}
	}
	// What is running finishes and is not retried: the queue is the
	// envelope alone.
	t.msgs = slices.DeleteFunc(t.msgs, func(m *message) bool { return !m.running })
	t.msgs = append(t.msgs, &message{id: messageID(kindTerminated), kind: kindTerminated, cause: cause})
	t.pumpLocked()
}

// end stops the subscription at once, with no envelope.
func (t *tracked) end() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.endLocked()
}

func (t *tracked) endLocked() {
	if t.ended {
		return
	}
	t.ended = true
	if t.unwatch != nil {
		t.unwatch()
		t.unwatch = nil
	}
	if t.stopExpiry != nil {
		t.stopExpiry()
		t.stopExpiry = nil
	}
	for _, m := range t.msgs {
		if m.stop != nil {
			m.stop()
		}
	}
	t.msgs = nil
	t.w.forget(t)
}

// unsubscribeHook calls the type's OnUnsubscribe.
func (t *tracked) unsubscribeHook() {
	if t.typ == nil || t.typ.def.OnUnsubscribe == nil {
		return
	}
	if err := t.typ.def.OnUnsubscribe(context.Background(), t.ec, t.args); err != nil {
		t.w.e.opts.OnError(fmt.Errorf("mcpserver: events: %q: OnUnsubscribe: %w", t.ec.Name, err))
	}
}
