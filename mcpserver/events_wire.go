package mcpserver

import (
	"encoding/json"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
)

// wireError is the JSON-RPC error the SDK sends.
type wireError = jsonrpc.Error

// subscriptionIDKey is the _meta key that ties a frame of a stream to
// the events/stream request it belongs to, whose value is that
// request's JSON-RPC id.
const subscriptionIDKey = "io.modelcontextprotocol/subscriptionId"

// The notifications of a stream, which the SEP defines and this package
// does not send yet: go-sdk v1.8.0 has no custom notifications, so push
// waits for one. The frame types are here so the format is settled, and
// the golden tests hold it.
const (
	notifyActive     = "notifications/events/active"
	notifyEvent      = "notifications/events/event"
	notifyHeartbeat  = "notifications/events/heartbeat"
	notifyError      = "notifications/events/error"
	notifyTerminated = "notifications/events/terminated"
)

// eventDescriptor is an event type as events/list says it.
type eventDescriptor struct {
	Name          string          `json:"name"`
	Description   string          `json:"description"`
	Delivery      []string        `json:"delivery"`
	InputSchema   json.RawMessage `json:"inputSchema"`
	PayloadSchema json.RawMessage `json:"payloadSchema"`
	Meta          map[string]any  `json:"_meta,omitempty"`
}

func (t *EventType) descriptor() eventDescriptor {
	return eventDescriptor{
		Name: t.def.Name, Description: t.def.Description, Delivery: t.def.Delivery,
		InputSchema: t.def.InputSchema, PayloadSchema: t.def.PayloadSchema, Meta: t.def.Meta,
	}
}

// eventOccurrence is an event as a poll result carries it. Its cursor is
// the result's, so it has none.
type eventOccurrence struct {
	EventID   string          `json:"eventId"`
	Name      string          `json:"name"`
	Timestamp string          `json:"timestamp"`
	Data      json.RawMessage `json:"data"`
	Meta      map[string]any  `json:"_meta,omitempty"`
}

func occurrence(name string, ev Event) eventOccurrence {
	return eventOccurrence{EventID: ev.ID, Name: name, Timestamp: ev.Time.UTC().Format(time.RFC3339Nano), Data: ev.Data, Meta: ev.Meta}
}

// frame is a notification of a stream.
type frame struct {
	Method string
	Params any
}

func (f frame) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  any    `json:"params"`
	}{"2.0", f.Method, f.Params})
}

type activeParams struct {
	Cursor      *string        `json:"cursor"`
	Truncated   bool           `json:"truncated"`
	HeartbeatMs int64          `json:"heartbeatMs"`
	Meta        map[string]any `json:"_meta"`
}

// eventParams is an occurrence with its cursor, which a stream's frame
// has where a poll result does not; null for a type that does not replay.
type eventParams struct {
	EventID   string          `json:"eventId"`
	Name      string          `json:"name"`
	Timestamp string          `json:"timestamp"`
	Data      json.RawMessage `json:"data"`
	Cursor    *string         `json:"cursor"`
	Meta      map[string]any  `json:"_meta"`
}

type heartbeatParams struct {
	Cursor *string        `json:"cursor"`
	Meta   map[string]any `json:"_meta"`
}

type errorParams struct {
	Error *wireError     `json:"error"`
	Meta  map[string]any `json:"_meta"`
}

// streamSink is where a subscription's frames go: the one seam between
// what the events core knows, which events there are, where a cursor is,
// who is to see what, and how they travel. Poll does not use it, since a
// poll is a result; a stream would implement it over a notification
// channel and a webhook over HTTP requests, and the core would feed them
// the same way, with [feedSink]. A sink is for one subscription, and its
// methods are called in order, never at once.
type streamSink interface {
	// active says the subscription is live from cursor, truncated when
	// that is later than the cursor asked for. It is the first call, and
	// is made again, with truncated, after a gap.
	active(cursor *string, truncated bool, heartbeatMs int64) error
	// event delivers one event, and cursor is the position after it.
	event(name string, ev Event, cursor *string) error
	// heartbeat says the subscription is quiet at cursor.
	heartbeat(cursor *string) error
	// failure reports an error the subscription survives.
	failure(err *EventsError) error
	// terminated ends the subscription with err.
	terminated(err *EventsError) error
	// finish ends the request, after terminated or when the server
	// closes the stream.
	finish() error
}

// frameSink is a [streamSink] that writes the SEP's frames, as JSON
// messages, to write. id is the JSON-RPC id of the events/stream request.
type frameSink struct {
	id    any
	write func(json.RawMessage) error
}

func (s frameSink) meta(extra map[string]any) map[string]any {
	m := map[string]any{}
	for k, v := range extra {
		m[k] = v
	}
	m[subscriptionIDKey] = s.id
	return m
}

func (s frameSink) send(method string, params any) error {
	b, err := json.Marshal(frame{Method: method, Params: params})
	if err != nil {
		return err
	}
	return s.write(b)
}

func (s frameSink) active(cursor *string, truncated bool, heartbeatMs int64) error {
	return s.send(notifyActive, activeParams{Cursor: cursor, Truncated: truncated, HeartbeatMs: heartbeatMs, Meta: s.meta(nil)})
}

func (s frameSink) event(name string, ev Event, cursor *string) error {
	return s.send(notifyEvent, eventParams{
		EventID: ev.ID, Name: name, Timestamp: ev.Time.UTC().Format(time.RFC3339Nano),
		Data: ev.Data, Cursor: cursor, Meta: s.meta(ev.Meta),
	})
}

func (s frameSink) heartbeat(cursor *string) error {
	return s.send(notifyHeartbeat, heartbeatParams{Cursor: cursor, Meta: s.meta(nil)})
}

func (s frameSink) failure(err *EventsError) error {
	return s.send(notifyError, errorParams{Error: err.wireValue(), Meta: s.meta(nil)})
}

func (s frameSink) terminated(err *EventsError) error {
	return s.send(notifyTerminated, errorParams{Error: err.wireValue(), Meta: s.meta(nil)})
}

func (s frameSink) finish() error {
	b, err := json.Marshal(struct {
		JSONRPC string `json:"jsonrpc"`
		ID      any    `json:"id"`
		Result  any    `json:"result"`
	}{"2.0", s.id, map[string]any{"resultType": "complete", "_meta": s.meta(nil)}})
	if err != nil {
		return err
	}
	return s.write(b)
}

// wireValue is e as the error object of a frame.
func (e *EventsError) wireValue() *wireError {
	w, _ := e.wire().(*wireError)
	return w
}

// feedSink hands one batch of a subscription to sink: when start, the
// active frame first, then each event with the cursor after it. The
// cursors of a batch stay at its start until its last event, so a cursor
// a client persists never runs ahead of an event it has not been sent;
// the last carries the batch's. It is how a stream would drain what poll
// reads, and is held by a test with a recording sink.
func feedSink(sink streamSink, name string, start bool, from *string, heartbeatMs int64, b batch) error {
	if start || b.truncated {
		if err := sink.active(b.cursor, b.truncated, heartbeatMs); err != nil {
			return err
		}
	}
	for i, ev := range b.events {
		cur := from
		if i == len(b.events)-1 {
			cur = b.cursor
		}
		if err := sink.event(name, ev, cur); err != nil {
			return err
		}
	}
	return nil
}
