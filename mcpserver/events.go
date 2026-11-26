package mcpserver

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"sync"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// EventsExtension is the extension id under which a server that serves
// MCP Events (SEP-3415) says so, in capabilities.extensions, at
// initialize and in discovery. Its value is an empty object, since this
// package does not send events/list_changed. It is
// mcpclient.EventsExtension byte for byte, and a test holds the two
// together.
//
// Events are experimental: the extension is defined for protocol
// revision 2026-07-28 and later, its error codes are provisional, and
// this package serves its poll mode alone. What it does not serve, a
// stream, a webhook, is not offered in any event's delivery list and is
// answered with [EventsCodeUnsupported].
const EventsExtension = "io.modelcontextprotocol/events"

// The methods of the extension. A server of this package answers all of
// them: events/list and events/poll as the SEP says, and the others
// with [EventsCodeUnsupported], since it offers no stream and no
// webhook. They are mcpclient's byte for byte.
const (
	EventsListMethod        = "events/list"
	EventsPollMethod        = "events/poll"
	EventsStreamMethod      = "events/stream"
	EventsSubscribeMethod   = "events/subscribe"
	EventsUnsubscribeMethod = "events/unsubscribe"
)

// The codes events errors carry. The SEP names them provisional, so
// they are constants to change in one place; -32602 and -32020 are the
// core protocol's. They are mcpclient's byte for byte.
const (
	EventsCodeInvalidParams     = -32602
	EventsCodeNotFound          = -32023
	EventsCodeForbidden         = -32024
	EventsCodeResourceExhausted = -32025
	EventsCodeUnsupported       = -32026
	EventsCodeCallbackEndpoint  = -32027
	EventsCodeHeaderMismatch    = -32020
)

// The delivery modes of an event type. A type's [EventDef.Delivery]
// lists the modes it is served by; only [DeliveryPoll] is served yet.
const (
	DeliveryPoll    = "poll"
	DeliveryPush    = "push"
	DeliveryWebhook = "webhook"
)

// EventsError is an error with a code of the events extension, which a
// [Check] hook or an [EventDef.OnSubscribe] hook returns to answer a
// request with that code and not an internal error, and which poll
// answers as the JSON-RPC error it is:
//
//	{"code": -32023, "message": "unknown event", "data": {"kind": "event"}}
type EventsError struct {
	Code    int
	Message string
	// Data is the error's data, or nil for none.
	Data map[string]any
}

func (e *EventsError) Error() string { return fmt.Sprintf("events: %s (%d)", e.Message, e.Code) }

// wire is e as the SDK sends it.
func (e *EventsError) wire() error {
	var data json.RawMessage
	if e.Data != nil {
		data, _ = json.Marshal(e.Data)
	}
	return &wireError{Code: int64(e.Code), Message: e.Message, Data: data}
}

func invalidParams(format string, args ...any) *EventsError {
	return &EventsError{Code: EventsCodeInvalidParams, Message: fmt.Sprintf(format, args...)}
}

func notFoundEvent(name string) *EventsError {
	return &EventsError{Code: EventsCodeNotFound, Message: fmt.Sprintf("unknown event %q", name), Data: map[string]any{"kind": "event"}}
}

func unsupportedDelivery(mode string) *EventsError {
	return &EventsError{Code: EventsCodeUnsupported, Message: fmt.Sprintf("delivery mode %q is not supported", mode), Data: map[string]any{"feature": "deliveryMode", "value": mode}}
}

// Event is one event as an event type's hooks and [EventType.Emit] take
// it. Data is JSON that conforms to the type's payload schema.
type Event struct {
	// ID is the event's stable id, which a client deduplicates on.
	// Where the upstream has one, use it, so the same event reached by
	// two routes is one event. Emit makes one up when it is empty; a
	// [Check] result without one is dropped.
	ID string
	// Time is when the event occurred; the zero time is now.
	Time time.Time
	Data json.RawMessage
	// Meta is the event's _meta, which the payload schema does not
	// govern.
	Meta map[string]any
}

// EventContext says whom a hook runs for.
type EventContext struct {
	// Name is the event type.
	Name string
	// Principal is who the request came from, as [EventsOptions.Principal]
	// said, or "" for no one in particular.
	Principal string
	// Subscription is the id of the poll lease the request belongs to.
	Subscription string
	// Mode is the delivery mode, [DeliveryPoll].
	Mode string
}

// CheckContext is an [EventContext] with the limits of one check.
type CheckContext struct {
	EventContext
	// MaxEvents is how many events the client asked for, or the
	// server's default; a check that returns more than it can say so
	// with HasMore.
	MaxEvents int
	// MaxAgeMs is how far back the client wants events to start, in
	// milliseconds, 0 for no limit. The server drops older events from
	// a result, so a check may use it to avoid fetching them.
	MaxAgeMs int64
}

// CheckResult is what a [Check] returns.
type CheckResult struct {
	// Events are the events since the cursor, oldest first.
	Events []Event
	// Cursor is the position after Events, which the client sends back.
	// Nil says the type does not replay: the client then always polls
	// with no cursor, and each check returns what is new since the last.
	Cursor *string
	// HasMore says more events are ready, so the client polls again at
	// once.
	HasMore bool
	// Truncated says the check started later than the cursor it was
	// given, because the upstream no longer had the events between.
	Truncated bool
	// NextPollMs is how long the client should wait before polling, in
	// milliseconds; 0 is the server's default.
	NextPollMs int64
}

// Check is the function of an event type that pulls events from their
// upstream. It is called with the cursor the client sent, nil for the
// first poll, where it returns the current position and no events, the
// cursor the client then polls from. The cursor is the author's: an
// upstream position such as a history id, opaque to the client.
type Check func(ctx context.Context, c CheckContext, args json.RawMessage, cursor *string) (CheckResult, error)

// EventDef defines one event type: what it is called, what a client
// subscribes with, what its events carry, how it is served and where its
// events come from.
//
// A type is of one of two kinds. With a Check, the author pulls events
// from an upstream and owns the cursor. With EmitOnly, the author pushes
// them with [EventType.Emit] and the server keeps them in a bounded
// in-memory ring, whose cursors read "e<epoch>.<n>": a cursor from
// another process, or from before the ring's oldest event, reads as a
// gap, answered with truncated and a fresh cursor, so events emitted
// while no client polled and a restart lost are lost.
type EventDef struct {
	// Name identifies the type, and is what a client lists and polls.
	Name        string
	Description string
	// InputSchema is the JSON Schema of the arguments a client
	// subscribes with; the default is any object. A property may carry
	// "x-mcp-header": "Name", which has a client over Streamable HTTP
	// send that argument as an Mcp-Param-Name header, and has this
	// server refuse a request whose header and argument differ. Avoid
	// naming a property that is sensitive: intermediaries read headers.
	InputSchema json.RawMessage
	// PayloadSchema is the JSON Schema of an event's data, whose root
	// is an object. It is required.
	PayloadSchema json.RawMessage
	// Meta is the descriptor's _meta.
	Meta map[string]any
	// Delivery lists the modes the type is served by; the default is
	// poll, with webhook added for an emit-only type when the webhook
	// mode is on. An emit-only type may leave poll out and offer webhook
	// alone; events/poll then answers it with [EventsCodeUnsupported]. A
	// type with a Check needs poll. Push is not served, so it is a
	// registration error.
	Delivery []string

	// Check pulls events from the upstream. Exactly one of Check and
	// EmitOnly is set.
	Check Check
	// EmitOnly makes the type one the author emits.
	EmitOnly bool

	// Match decides whether an emitted event reaches the subscription
	// with these arguments; absent, every one does. Only an emit-only
	// type has it. It runs for each event as poll reads it, and for each
	// webhook subscription on the goroutine that emits, so it should be
	// cheap and must not hold on to ev.
	Match func(c EventContext, ev Event, args json.RawMessage) bool
	// Transform reshapes an emitted event for the subscription it goes
	// to; absent, it goes as emitted. Its result is checked against the
	// payload schema again, and an event whose transform fails or whose
	// result does not conform is skipped and reported to
	// [EventsOptions.OnError]. Only an emit-only type has it.
	Transform func(c EventContext, ev Event, args json.RawMessage) (Event, error)

	// OnSubscribe is called when a poll lease is first seen, which is
	// the nearest poll has to a subscription, and may refuse it by
	// returning an error, which a request then fails with. Poll has no
	// goodbye, so a lease lapses when its client stops polling for
	// [EventsOptions.LeaseTTL]; a server restart forgets leases, so
	// OnSubscribe is called again for a lease a client kept. Make it
	// idempotent.
	OnSubscribe func(ctx context.Context, c EventContext, args json.RawMessage) error
	// OnUnsubscribe is called when a lease lapses or is ended, the
	// counterpart of OnSubscribe.
	OnUnsubscribe func(ctx context.Context, c EventContext, args json.RawMessage) error
}

// EventsOptions are the settings of the events a server serves. The
// zero value is usable.
type EventsOptions struct {
	// Now is the clock; nil is [time.Now]. A test sets it to move time.
	Now func() time.Time
	// Principal says who a request came from, for the poll lease a
	// client holds (two principals polling the same event with the same
	// arguments are two leases) and for the hooks. extra is nil where
	// the transport has no request, as on stdio. Nil is no one, so
	// every request is one principal. This package does no
	// authentication: a Principal reads what the embedding
	// application's HTTP middleware verified, such as extra.TokenInfo.
	Principal func(extra *sdk.RequestExtra) string
	// LeaseTTL is how long a poll lease lasts after the last poll;
	// the default is 5 minutes. It also bounds nextPollMs, which is at
	// most half of it, so a client polling as told renews its lease.
	LeaseTTL time.Duration
	// DefaultNextPoll is the nextPollMs of a result that has none;
	// the default is 30 seconds.
	DefaultNextPoll time.Duration
	// DefaultMaxEvents is how many events a poll returns when the
	// client names no limit; the default is 100.
	DefaultMaxEvents int
	// MaxSubscriptions bounds the leases held at once, refused beyond
	// with [EventsCodeResourceExhausted]; the default is 1024.
	MaxSubscriptions int
	// ListPageSize is how many event types an events/list page holds;
	// the default is 100. A catalog beyond it is paged with an opaque
	// nextCursor, as tools/list is.
	ListPageSize int
	// Buffer bounds the ring of an emit-only type.
	Buffer EmitBuffer
	// Webhook turns on the webhook delivery mode, events/subscribe and
	// events/unsubscribe; nil leaves it off, and both are then answered
	// with [EventsCodeUnsupported]. See [WebhookOptions].
	Webhook *WebhookOptions
	// OnError is told of what the server skips or cannot do for an
	// author's sake: an event whose payload does not conform, a hook
	// that failed. Nil logs it with [slog.Default].
	OnError func(error)

	// epoch fixes the epoch of an emit-only type's cursors, which is
	// otherwise random for each process, so a test can write them.
	epoch string
}

// EmitBuffer bounds the in-memory ring of an emit-only event type, by
// age and by count; an event beyond either is dropped, and a cursor
// before the oldest kept reads as a gap.
type EmitBuffer struct {
	// MaxAge is how long an event is kept; the default is 10 minutes.
	MaxAge time.Duration
	// MaxEvents is how many events are kept; the default is 10000.
	MaxEvents int
}

const (
	defaultLeaseTTL        = 5 * time.Minute
	defaultNextPoll        = 30 * time.Second
	defaultMaxEvents       = 100
	defaultListPage        = 100
	defaultMaxSubscription = 1024
	defaultBufferAge       = 10 * time.Minute
	defaultBufferEvents    = 10000
)

func (o EventsOptions) withDefaults() EventsOptions {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.LeaseTTL <= 0 {
		o.LeaseTTL = defaultLeaseTTL
	}
	if o.DefaultNextPoll <= 0 {
		o.DefaultNextPoll = defaultNextPoll
	}
	if o.DefaultMaxEvents <= 0 {
		o.DefaultMaxEvents = defaultMaxEvents
	}
	if o.ListPageSize <= 0 {
		o.ListPageSize = defaultListPage
	}
	if o.MaxSubscriptions <= 0 {
		o.MaxSubscriptions = defaultMaxSubscription
	}
	if o.Buffer.MaxAge <= 0 {
		o.Buffer.MaxAge = defaultBufferAge
	}
	if o.Buffer.MaxEvents <= 0 {
		o.Buffer.MaxEvents = defaultBufferEvents
	}
	if o.OnError == nil {
		o.OnError = func(err error) { slog.Default().Warn("mcpserver: events", "error", err) }
	}
	if o.epoch == "" {
		var b [4]byte
		_, _ = rand.Read(b[:])
		o.epoch = hex.EncodeToString(b[:])
	}
	return o
}

// Events is the events a server serves, the registry of its event types
// and their poll leases.
type Events struct {
	opts EventsOptions

	mu    sync.Mutex
	types map[string]*EventType

	leases leaseTable
	// hooks is the webhook delivery, nil when it is off.
	hooks *webhookEngine
}

// EventType is a registered event type, the handle of its author.
type EventType struct {
	events *Events
	def    EventDef
	bind   []headerBinding
	input  *schemaCheck
	output *schemaCheck
	ring   *ring // emit-only types
}

// ErrNoEmit is what [EventType.Emit] returns for a type that has a
// Check, whose events come from the check.
var ErrNoEmit = errors.New("mcpserver: events: the type is not emit-only")

// ErrEventExists is what [Events.Register] returns for a type whose name
// and schemas are the registered type's, which would change nothing and
// whose hooks would be dropped on the floor. A type with other schemas
// replaces the registered one.
var ErrEventExists = errors.New("mcpserver: events: the type is registered with these schemas")

// AddEvents has s serve MCP Events: the extension is advertised at
// initialize and in discovery, events/list and events/poll are answered,
// and events/stream, events/subscribe and events/unsubscribe are
// answered with [EventsCodeUnsupported]. Event types are added to the
// result with [Events.Register]; until one is, events/list is empty.
// Call it once for a server.
//
// Requests to an events method over Streamable HTTP are checked against
// their Mcp-Name and Mcp-Param-* headers, which the extension requires a
// client of protocol 2026-07-28 to send, and refused with
// [EventsCodeHeaderMismatch] when one differs from the request's
// params, as the SEP says. The SDK sends a refusal found while handling
// a request as an HTTP 200 whose body is the error, where the SEP says
// 400; wrap the HTTP handler with [Events.HTTPHandler] to send the 400.
//
// This is experimental: the SEP's wire may change under it.
func AddEvents(s *sdk.Server, o EventsOptions) (*Events, error) {
	e := &Events{opts: o.withDefaults(), types: map[string]*EventType{}}
	e.leases = leaseTable{byKey: map[string]*lease{}}
	if o.Webhook != nil {
		e.opts.Webhook = new(WebhookOptions)
		*e.opts.Webhook = o.Webhook.withDefaults()
		var err error
		if e.hooks, err = newWebhookEngine(e, *e.opts.Webhook); err != nil {
			return nil, err
		}
	}
	for _, m := range []string{EventsListMethod, EventsPollMethod, EventsStreamMethod, EventsSubscribeMethod, EventsUnsubscribeMethod} {
		var err error
		switch m {
		case EventsListMethod:
			err = sdk.AddReceivingCustomMethod(s, m, e.handleList)
		case EventsPollMethod:
			err = sdk.AddReceivingCustomMethod(s, m, e.handlePoll)
		case EventsStreamMethod:
			err = sdk.AddReceivingCustomMethod(s, m, e.handleUnsupported(DeliveryPush))
		case EventsSubscribeMethod:
			err = sdk.AddReceivingCustomMethod(s, m, e.handleSubscribe)
		default:
			err = sdk.AddReceivingCustomMethod(s, m, e.handleUnsubscribe)
		}
		if err != nil {
			return nil, fmt.Errorf("mcpserver: %w", err)
		}
	}
	s.AddReceivingMiddleware(e.middleware)
	return e, nil
}

// Register adds an event type, or replaces the type of its name when its
// schemas differ, which ends the poll leases of the old type: a client's
// next poll of it is answered with [EventsCodeUnsupported], data
// feature "payloadSchema" or "inputSchema" and reason "schema_changed",
// to list again. A change that only adds to a schema still ends them;
// say so in the type's name by adding a new type beside the old when
// clients must not notice.
//
// The definition is refused when it has no name, or both or neither of
// Check and EmitOnly, a Match or Transform on a type with a Check, a
// schema that does not compile or a payload schema whose root is not an
// object, a Check type whose delivery list lacks poll
// (an emit-only type may offer webhook alone, and a poll of it is answered
// with [EventsCodeUnsupported]) or has a mode that is not served yet, or an x-mcp-header annotation that breaks the rules of HTTP
// header names and values.
func (e *Events) Register(def EventDef) (*EventType, error) {
	t, err := e.newType(def)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	old := e.types[def.Name]
	if old != nil {
		if sameSchema(old.def.InputSchema, t.def.InputSchema) && sameSchema(old.def.PayloadSchema, t.def.PayloadSchema) {
			e.mu.Unlock()
			return nil, fmt.Errorf("%w: %q", ErrEventExists, def.Name)
		}
		// A new buffer, since the old events do not conform: the
		// sequence goes on so old cursors read as a gap.
		if old.ring != nil && t.ring != nil {
			t.ring.continueFrom(old.ring)
		}
	}
	e.types[def.Name] = t
	e.mu.Unlock()
	if old != nil {
		feature := "inputSchema"
		if !sameSchema(old.def.PayloadSchema, t.def.PayloadSchema) {
			feature = "payloadSchema"
		}
		cause := &EventsError{
			Code: EventsCodeUnsupported, Message: fmt.Sprintf("event %q changed its %s", def.Name, feature),
			Data: map[string]any{"feature": feature, "reason": "schema_changed"},
		}
		e.leases.terminate(e, def.Name, cause)
		if e.hooks != nil {
			e.hooks.terminate(def.Name, cause)
		}
	}
	return t, nil
}

func (e *Events) newType(def EventDef) (*EventType, error) {
	switch {
	case def.Name == "":
		return nil, errors.New("mcpserver: events: a type needs a name")
	case def.Check == nil && !def.EmitOnly:
		return nil, fmt.Errorf("mcpserver: events: type %q has neither a Check nor EmitOnly", def.Name)
	case def.Check != nil && def.EmitOnly:
		return nil, fmt.Errorf("mcpserver: events: type %q has both a Check and EmitOnly", def.Name)
	case def.Check != nil && (def.Match != nil || def.Transform != nil):
		return nil, fmt.Errorf("mcpserver: events: type %q has a Check, whose events are the author's to select, and a Match or Transform", def.Name)
	}
	if len(def.Delivery) == 0 {
		def.Delivery = []string{DeliveryPoll}
		if e.hooks != nil && def.EmitOnly {
			def.Delivery = append(def.Delivery, DeliveryWebhook)
		}
	} else {
		def.Delivery = slices.Clone(def.Delivery)
		for _, m := range def.Delivery {
			switch m {
			case DeliveryPoll:
			case DeliveryPush:
				return nil, fmt.Errorf("mcpserver: events: type %q: delivery %q is not served yet", def.Name, m)
			case DeliveryWebhook:
				if e.hooks == nil {
					return nil, fmt.Errorf("mcpserver: events: type %q: delivery %q needs EventsOptions.Webhook", def.Name, m)
				}
				if !def.EmitOnly {
					return nil, fmt.Errorf("mcpserver: events: type %q: delivery %q is served for emit-only types, which this package keeps a replayable position for", def.Name, m)
				}
			default:
				return nil, fmt.Errorf("mcpserver: events: type %q: unknown delivery %q", def.Name, m)
			}
		}
		if !slices.Contains(def.Delivery, DeliveryPoll) && !def.EmitOnly {
			return nil, fmt.Errorf("mcpserver: events: type %q has a Check, which only poll runs, and its delivery list lacks poll", def.Name)
		}
	}
	if len(def.InputSchema) == 0 {
		def.InputSchema = json.RawMessage(`{"type":"object"}`)
	}
	if len(def.PayloadSchema) == 0 {
		return nil, fmt.Errorf("mcpserver: events: type %q needs a payload schema", def.Name)
	}
	if !objectRoot(def.PayloadSchema) {
		return nil, fmt.Errorf("mcpserver: events: type %q: the payload schema's root must be an object", def.Name)
	}
	in, err := newSchemaCheck(def.InputSchema)
	if err != nil {
		return nil, fmt.Errorf("mcpserver: events: type %q: input schema: %w", def.Name, err)
	}
	out, err := newSchemaCheck(def.PayloadSchema)
	if err != nil {
		return nil, fmt.Errorf("mcpserver: events: type %q: payload schema: %w", def.Name, err)
	}
	bind, err := scanHeaderBindings(def.InputSchema)
	if err != nil {
		return nil, fmt.Errorf("mcpserver: events: type %q: %w", def.Name, err)
	}
	t := &EventType{events: e, def: def, bind: bind, input: in, output: out}
	if def.EmitOnly {
		t.ring = newRing(e.opts.epoch, e.opts.Now, e.opts.Buffer)
	}
	return t, nil
}

// Name is the type's name.
func (t *EventType) Name() string { return t.def.Name }

// Emit adds an event of an emit-only type, which a poll of the type then
// returns to each subscription its Match accepts. The event's data is
// checked against the payload schema, and an event that does not
// conform is refused with the error, since the author can fix that and
// a client could not. A missing ID is made up and a missing Time is
// now. Emit never waits on a client.
//
// An event emitted reaches the ring of this process alone: a poll that
// lands on another replica reads a gap.
func (t *EventType) Emit(ev Event) error {
	if t.ring == nil {
		return ErrNoEmit
	}
	rec, err := t.record(ev, true)
	if err != nil {
		return err
	}
	if !t.events.live(t) {
		return fmt.Errorf("mcpserver: events: type %q is not registered any more", t.def.Name)
	}
	t.ring.append(rec)
	return nil
}

// Remove unregisters the type and ends its poll leases. A client's next
// poll of it is answered with [EventsCodeNotFound], data kind "event",
// to list again; a poll that never held a lease is answered the same
// way.
func (t *EventType) Remove() {
	e := t.events
	e.mu.Lock()
	if e.types[t.def.Name] == t {
		delete(e.types, t.def.Name)
	}
	e.mu.Unlock()
	e.leases.drop(e, t.def.Name)
	if e.hooks != nil {
		e.hooks.terminate(t.def.Name, notFoundEvent(t.def.Name))
	}
}

// live says whether t is the type registered under its name.
func (e *Events) live(t *EventType) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.types[t.def.Name] == t
}

func (e *Events) lookup(name string) *EventType {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.types[name]
}

// Close ends every poll lease, calling the types' OnUnsubscribe, which a
// server that is shutting down does so a hook sees the end, and stops
// webhook delivery: timers are cancelled, requests in flight are cut off
// and the goroutines are waited for, so nothing runs after it returns.
// Webhook subscriptions stay in the store, which clients refresh. Types
// and emitted events are untouched.
func (e *Events) Close() {
	e.leases.dropAll(e)
	if e.hooks != nil {
		e.hooks.close()
	}
}

// record turns ev into the form the server keeps, checked against the
// payload schema.
func (t *EventType) record(ev Event, generate bool) (eventRecord, error) {
	if ev.ID == "" {
		if !generate {
			return eventRecord{}, errors.New("the event has no id")
		}
		var b [16]byte
		_, _ = rand.Read(b[:])
		ev.ID = "evt_" + hex.EncodeToString(b[:])
	}
	if ev.Time.IsZero() {
		ev.Time = t.events.opts.Now()
	}
	if len(ev.Data) == 0 {
		return eventRecord{}, fmt.Errorf("event %q has no data", ev.ID)
	}
	if err := t.output.validate(ev.Data); err != nil {
		return eventRecord{}, fmt.Errorf("event %q does not conform to the payload schema of %q: %w", ev.ID, t.def.Name, err)
	}
	return eventRecord{ID: ev.ID, Time: ev.Time.UTC(), Data: bytes.Clone(ev.Data), Meta: ev.Meta}, nil
}

// eventRecord is an event as kept and served.
type eventRecord struct {
	ID   string
	Time time.Time
	Data json.RawMessage
	Meta map[string]any
}

func (r eventRecord) event() Event {
	return Event(r)
}

// sameSchema says whether two schemas are the same JSON, whatever the
// spacing and key order.
func sameSchema(a, b json.RawMessage) bool {
	ca, errA := canonicalJSON(a)
	cb, errB := canonicalJSON(b)
	if errA != nil || errB != nil {
		return bytes.Equal(a, b)
	}
	return bytes.Equal(ca, cb)
}

// canonicalJSON is raw with every object's keys sorted and no spacing,
// numbers as written.
func canonicalJSON(raw json.RawMessage) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

// sortedTypes lists the registered types by name.
func (e *Events) sortedTypes() []*EventType {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]*EventType, 0, len(e.types))
	for _, t := range e.types {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].def.Name < out[j].def.Name })
	return out
}
