package mcpserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// eventsParams are the params of every events method. The fields not
// every method has are raw, so this package checks their types itself
// and answers a wrong one with [EventsCodeInvalidParams].
type eventsParams struct {
	sdk.ParamsBase
	Name        string          `json:"name,omitempty"`
	Arguments   json.RawMessage `json:"arguments,omitempty"`
	Cursor      json.RawMessage `json:"cursor,omitempty"`
	MaxAgeMs    json.RawMessage `json:"maxAgeMs,omitempty"`
	MaxEvents   json.RawMessage `json:"maxEvents,omitempty"`
	HeartbeatMs json.RawMessage `json:"heartbeatMs,omitempty"`
	TTLMs       json.RawMessage `json:"ttlMs,omitempty"`
	Delivery    json.RawMessage `json:"delivery,omitempty"`
}

type listEventsResult struct {
	sdk.ResultBase
	ResultType string            `json:"resultType"`
	Events     []eventDescriptor `json:"events"`
	NextCursor string            `json:"nextCursor,omitempty"`
}

type pollEventsResult struct {
	sdk.ResultBase
	ResultType string            `json:"resultType"`
	Events     []eventOccurrence `json:"events"`
	Cursor     *string           `json:"cursor"`
	Truncated  bool              `json:"truncated"`
	HasMore    bool              `json:"hasMore"`
	NextPollMs int64             `json:"nextPollMs"`
}

type emptyEventsResult struct {
	sdk.ResultBase
	ResultType string `json:"resultType"`
}

// middleware checks the routing headers of an events request, hands the
// handler what the request carried, which a handler does not see, and
// advertises the extension.
func (e *Events) middleware(next sdk.MethodHandler) sdk.MethodHandler {
	return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
		if strings.HasPrefix(method, "events/") {
			if r, ok := req.(*sdk.ServerRequest[*eventsParams]); ok {
				var h http.Header
				if r.Extra != nil {
					h = r.Extra.Header
				}
				if err := e.checkHeaders(method, r.Params, h); err != nil {
					return nil, err.wire()
				}
				ri := &requestInfo{extra: r.Extra}
				if e.opts.Principal != nil {
					ri.principal = e.opts.Principal(r.Extra)
				}
				ctx = context.WithValue(ctx, requestInfoKey{}, ri)
			}
		}
		res, err := next(ctx, method, req)
		if err == nil {
			switch r := res.(type) {
			case *sdk.InitializeResult:
				advertiseEvents(r.Capabilities)
			case *sdk.DiscoverResult:
				advertiseEvents(r.Capabilities)
			}
		}
		return res, err
	}
}

// advertiseEvents adds [EventsExtension] to caps, which the SDK builds
// afresh for each answer.
func advertiseEvents(caps *sdk.ServerCapabilities) {
	if caps == nil {
		return
	}
	if caps.Extensions == nil {
		caps.Extensions = map[string]any{}
	}
	caps.Extensions[EventsExtension] = map[string]any{}
}

func (e *Events) handleList(_ context.Context, _ *sdk.ServerSession, p *eventsParams) (*listEventsResult, error) {
	// One page: a cursor is one this server never gave.
	if p != nil && len(p.Cursor) > 0 && string(p.Cursor) != "null" {
		return nil, invalidParams("unknown cursor").wire()
	}
	res := &listEventsResult{ResultType: "complete", Events: []eventDescriptor{}}
	for _, t := range e.sortedTypes() {
		res.Events = append(res.Events, t.descriptor())
	}
	return res, nil
}

func (e *Events) handleUnsupported(mode string) func(context.Context, *sdk.ServerSession, *eventsParams) (*emptyEventsResult, error) {
	return func(context.Context, *sdk.ServerSession, *eventsParams) (*emptyEventsResult, error) {
		return nil, unsupportedDelivery(mode).wire()
	}
}

func (e *Events) handlePoll(ctx context.Context, _ *sdk.ServerSession, p *eventsParams) (*pollEventsResult, error) {
	res, err := e.poll(ctx, p)
	if err != nil {
		var ee *EventsError
		if !errors.As(err, &ee) {
			e.opts.OnError(err)
			ee = &EventsError{Code: -32603, Message: "the event could not be polled"}
		}
		return nil, ee.wire()
	}
	return res, nil
}

// poll answers one events/poll.
func (e *Events) poll(ctx context.Context, p *eventsParams) (*pollEventsResult, error) {
	if p == nil || p.Name == "" {
		return nil, invalidParams("events/poll needs a name")
	}
	t := e.lookup(p.Name)
	if t == nil {
		return nil, notFoundEvent(p.Name)
	}
	hasPoll := false
	for _, m := range t.def.Delivery {
		hasPoll = hasPoll || m == DeliveryPoll
	}
	if !hasPoll {
		return nil, unsupportedDelivery(DeliveryPoll)
	}
	args := p.Arguments
	if len(args) == 0 || string(args) == "null" {
		args = json.RawMessage("{}")
	}
	if err := t.input.validate(args); err != nil {
		return nil, invalidParams("arguments of %q: %v", p.Name, err)
	}
	call := pollCall{maxEvents: e.opts.DefaultMaxEvents}
	switch {
	case len(p.Cursor) == 0 || string(p.Cursor) == "null":
	default:
		var c string
		if json.Unmarshal(p.Cursor, &c) != nil {
			return nil, invalidParams("cursor must be a string or null")
		}
		call.cursor = &c
	}
	if n, ok, err := wholeNumber(p.MaxAgeMs, "maxAgeMs", 0); err != nil {
		return nil, err
	} else if ok {
		call.maxAgeMs = n
	}
	if n, ok, err := wholeNumber(p.MaxEvents, "maxEvents", 1); err != nil {
		return nil, err
	} else if ok {
		call.maxEvents = int(min(n, int64(max(1000, e.opts.DefaultMaxEvents))))
	}

	ri, _ := ctx.Value(requestInfoKey{}).(*requestInfo)
	ec := EventContext{Name: t.def.Name, Mode: DeliveryPoll}
	if ri != nil {
		ec.Principal = ri.principal
	}
	l, err := e.leases.acquire(ctx, e, t, ec, args)
	if err != nil {
		return nil, err
	}
	ec.Subscription = l.id
	call.newLease = l.fresh

	b, err := t.read(ctx, ec, args, call)
	if err != nil {
		return nil, err
	}
	res := &pollEventsResult{
		ResultType: "complete", Events: make([]eventOccurrence, 0, len(b.events)),
		Cursor: b.cursor, Truncated: b.truncated, HasMore: b.hasMore, NextPollMs: b.nextPollMs,
	}
	for _, ev := range b.events {
		res.Events = append(res.Events, occurrence(t.def.Name, ev))
	}
	return res, nil
}

// wholeNumber reads an optional non-negative whole number of at least
// min.
func wholeNumber(raw json.RawMessage, name string, min int64) (int64, bool, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, false, nil
	}
	var f float64
	if json.Unmarshal(raw, &f) != nil || f != math.Trunc(f) || f < float64(min) || f > maxSafeInt {
		return 0, false, invalidParams("%s must be a whole number of at least %d", name, min)
	}
	return int64(f), true, nil
}

// pollCall is what one poll asks of an event type.
type pollCall struct {
	cursor    *string
	maxEvents int
	maxAgeMs  int64
	// newLease says this is the first poll of the lease.
	newLease bool
}

// batch is what one read of an event type found, whatever route the
// read is for: a poll result, or the frames of a stream.
type batch struct {
	events     []Event
	cursor     *string
	truncated  bool
	hasMore    bool
	nextPollMs int64
}

// read finds the events a subscription with args is to see after
// call.cursor.
func (t *EventType) read(ctx context.Context, ec EventContext, args json.RawMessage, call pollCall) (batch, error) {
	e := t.events
	now := e.opts.Now()
	var floor time.Time
	if call.cursor != nil && call.maxAgeMs > 0 {
		floor = now.Add(-time.Duration(call.maxAgeMs) * time.Millisecond)
	}
	if t.ring != nil {
		r := t.ring.read(call.cursor, call.maxEvents, floor, func(rec eventRecord) (Event, bool) {
			return t.choose(ec, rec, args)
		})
		cur := r.cursor
		return batch{events: r.events, cursor: &cur, truncated: r.truncated, hasMore: r.hasMore, nextPollMs: e.nextPollMs(0)}, nil
	}
	res, err := t.def.Check(ctx, CheckContext{EventContext: ec, MaxEvents: call.maxEvents, MaxAgeMs: call.maxAgeMs}, args, call.cursor)
	if err != nil {
		return batch{}, err
	}
	next := e.nextPollMs(res.NextPollMs)
	if call.cursor == nil && (call.newLease || res.Cursor != nil) {
		// From now: the events the check found are before the client's
		// start.
		return batch{cursor: res.Cursor, nextPollMs: next}, nil
	}
	b := batch{cursor: res.Cursor, truncated: res.Truncated, hasMore: res.HasMore, nextPollMs: next}
	if res.Cursor == nil {
		b.truncated = false
	}
	for _, ev := range res.Events {
		rec, err := t.record(ev, false)
		if err != nil {
			e.opts.OnError(fmt.Errorf("mcpserver: events: %q: dropped: %w", t.def.Name, err))
			continue
		}
		if !floor.IsZero() && res.Cursor != nil && rec.Time.Before(floor) {
			b.truncated = true
			continue
		}
		b.events = append(b.events, rec.event())
	}
	return b, nil
}

// choose applies Match and Transform to an emitted event.
func (t *EventType) choose(ec EventContext, rec eventRecord, args json.RawMessage) (Event, bool) {
	ev := rec.event()
	if t.def.Match != nil && !t.def.Match(ec, ev, args) {
		return Event{}, false
	}
	if t.def.Transform == nil {
		return ev, true
	}
	out, err := t.def.Transform(ec, ev, args)
	if err == nil {
		if out.ID == "" {
			out.ID = ev.ID
		}
		if out.Time.IsZero() {
			out.Time = ev.Time
		}
		var r eventRecord
		if r, err = t.record(out, false); err == nil {
			return r.event(), true
		}
	}
	t.events.opts.OnError(fmt.Errorf("mcpserver: events: %q: event %q skipped: %w", t.def.Name, ev.ID, err))
	return Event{}, false
}

// nextPollMs is the wait the server asks, in milliseconds: want, or the
// default, and at most half the lease, so a client that waits as told
// holds its lease.
func (e *Events) nextPollMs(want int64) int64 {
	if want <= 0 {
		want = int64((e.opts.DefaultNextPoll + time.Millisecond - 1) / time.Millisecond)
	}
	if limit := int64(e.opts.LeaseTTL / 2 / time.Millisecond); want > limit {
		want = limit
	}
	if want < 1 {
		want = 1
	}
	return want
}

// leaseTable holds the poll leases, which stand in for the subscriptions
// poll does not have: a client that polls is subscribed until it stops.
// The table is soft state: a restart forgets it, and the next poll of a
// lease a client kept is a first poll.
type leaseTable struct {
	mu    sync.Mutex
	byKey map[string]*lease
	seq   uint64
}

type lease struct {
	key     string
	id      string
	name    string
	args    json.RawMessage
	ec      EventContext
	unsub   func(context.Context, EventContext, json.RawMessage) error
	expires time.Time
	// fresh is true for the first poll alone.
	fresh      bool
	terminated *EventsError
	ready      chan struct{}
	startErr   error
}

// leaseKey is the (principal, event, canonical arguments) a lease is
// kept under.
func leaseKey(principal, name string, args json.RawMessage) string {
	c, err := canonicalJSON(args)
	if err != nil {
		c = args
	}
	sum := sha256.Sum256(bytes.Join([][]byte{[]byte(principal), []byte(name), c}, []byte{0}))
	return hex.EncodeToString(sum[:])
}

// acquire finds or makes the lease for a poll, renews it, and calls
// OnSubscribe the first time. It reaps lapsed leases first.
func (lt *leaseTable) acquire(ctx context.Context, e *Events, t *EventType, ec EventContext, args json.RawMessage) (*lease, error) {
	now := e.opts.Now()
	key := leaseKey(ec.Principal, ec.Name, args)

	lt.mu.Lock()
	var lapsed []*lease
	for k, l := range lt.byKey {
		select {
		case <-l.ready:
		default:
			continue
		}
		if !l.expires.After(now) && k != key {
			lapsed = append(lapsed, l)
			delete(lt.byKey, k)
		}
	}
	l, ok := lt.byKey[key]
	if ok {
		select {
		case <-l.ready:
			if l.terminated != nil {
				delete(lt.byKey, key)
				lapsed = append(lapsed, l)
				terminated := l.terminated
				lt.mu.Unlock()
				lt.unsubscribe(e, lapsed)
				return nil, terminated
			}
			if !l.expires.After(now) {
				// Lapsed, and polled again: a lease anew.
				delete(lt.byKey, key)
				lapsed = append(lapsed, l)
				ok = false
			}
		default:
		}
	}
	if ok {
		l.expires = now.Add(e.opts.LeaseTTL)
		lt.mu.Unlock()
		lt.unsubscribe(e, lapsed)
		select {
		case <-l.ready:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if l.startErr != nil {
			return nil, l.startErr
		}
		c := *l
		c.fresh = false
		return &c, nil
	}
	if len(lt.byKey) >= e.opts.MaxSubscriptions {
		lt.mu.Unlock()
		lt.unsubscribe(e, lapsed)
		return nil, &EventsError{Code: EventsCodeResourceExhausted, Message: "too many subscriptions", Data: map[string]any{"limit": "subscriptions", "max": e.opts.MaxSubscriptions}}
	}
	lt.seq++
	ec.Subscription = fmt.Sprintf("poll_%d", lt.seq)
	l = &lease{
		key: key, id: ec.Subscription, name: ec.Name, args: args, ec: ec, unsub: t.def.OnUnsubscribe,
		expires: now.Add(e.opts.LeaseTTL), fresh: true, ready: make(chan struct{}),
	}
	lt.byKey[key] = l
	lt.mu.Unlock()
	lt.unsubscribe(e, lapsed)

	var err error
	if t.def.OnSubscribe != nil {
		err = t.def.OnSubscribe(ctx, ec, args)
	}
	if err != nil {
		var ee *EventsError
		if !errors.As(err, &ee) {
			e.opts.OnError(fmt.Errorf("mcpserver: events: %q: OnSubscribe: %w", ec.Name, err))
			ee = &EventsError{Code: -32603, Message: "the subscription could not be made"}
		}
		l.startErr = ee
		lt.mu.Lock()
		if lt.byKey[key] == l {
			delete(lt.byKey, key)
		}
		lt.mu.Unlock()
		close(l.ready)
		return nil, ee
	}
	close(l.ready)
	c := *l
	return &c, nil
}

// unsubscribe calls the OnUnsubscribe of leases that have ended.
func (lt *leaseTable) unsubscribe(e *Events, ended []*lease) {
	for _, l := range ended {
		if l.unsub == nil {
			continue
		}
		if err := l.unsub(context.Background(), l.ec, l.args); err != nil {
			e.opts.OnError(fmt.Errorf("mcpserver: events: %q: OnUnsubscribe: %w", l.name, err))
		}
	}
}

// terminate marks the leases of name ended with err, which the next poll
// of each is answered with once.
func (lt *leaseTable) terminate(_ *Events, name string, err *EventsError) {
	lt.mu.Lock()
	defer lt.mu.Unlock()
	for _, l := range lt.byKey {
		if l.name == name {
			l.terminated = err
		}
	}
}

// drop ends the leases of name at once.
func (lt *leaseTable) drop(e *Events, name string) {
	lt.mu.Lock()
	var ended []*lease
	for k, l := range lt.byKey {
		if l.name == name {
			ended = append(ended, l)
			delete(lt.byKey, k)
		}
	}
	lt.mu.Unlock()
	lt.unsubscribe(e, ended)
}

func (lt *leaseTable) dropAll(e *Events) {
	lt.mu.Lock()
	var ended []*lease
	for k, l := range lt.byKey {
		ended = append(ended, l)
		delete(lt.byKey, k)
	}
	lt.mu.Unlock()
	lt.unsubscribe(e, ended)
}

// leaseCount is how many leases are held, for a test.
func (lt *leaseTable) leaseCount() int {
	lt.mu.Lock()
	defer lt.mu.Unlock()
	return len(lt.byKey)
}
