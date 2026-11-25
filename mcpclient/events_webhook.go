package mcpclient

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// NewWebhookSecret makes a signing secret for a webhook subscription:
// "whsec_" and the base64 of 32 random bytes, which is what the SEP asks
// of a client. The server never makes one, and a secret that is not made
// from a CSPRNG weakens every delivery, so make them here and keep them
// for the life of the subscription.
func NewWebhookSecret() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("mcp: events: making a webhook secret: %w", err)
	}
	return webhookSecretPrefix + base64.StdEncoding.EncodeToString(b[:]), nil
}

const webhookSecretPrefix = "whsec_"

// SubscribeParams are what an events/subscribe asks for. The key of a
// subscription is the server's principal, the URL, Name and Arguments:
// change one and it is another subscription, to end and make anew.
type SubscribeParams struct {
	// Name and Arguments are the event type and its arguments, a JSON
	// object conforming to its input schema, or nil.
	Name      string
	Arguments json.RawMessage
	// URL is where the server POSTs events: https, and reachable from
	// the server. Secret is the whsec_ secret its deliveries are signed
	// with, from [NewWebhookSecret]; sending a new one on a refresh
	// replaces it, and the server signs with both for a short while.
	URL    string
	Secret string
	// Cursor is the last cursor the client kept, nil for from now. Pass
	// the last one on every refresh: the server treats it as a no-op for
	// a live subscription and as the replay point for a lapsed one.
	Cursor *string
	// MaxAge, when positive, has a replay start no earlier than that
	// long ago.
	MaxAge time.Duration
	// TTL, when positive, is the lifetime suggested; the server decides,
	// and its answer is the result's RefreshBefore. NoExpiry asks for a
	// subscription that does not expire instead, which a server may
	// refuse by granting a finite one; the two are exclusive.
	TTL      time.Duration
	NoExpiry bool
}

// DeliveryStatus is what a refresh says of how delivery is going.
type DeliveryStatus struct {
	// Active is false when the server suspended delivery after repeated
	// failures, which the refresh that returned this has just ended.
	Active bool
	// LastDeliveryAt is when an event was last acknowledged, zero for
	// never.
	LastDeliveryAt time.Time
	// LastError is one of connection_refused, timeout, tls_error,
	// http_4xx, http_5xx, challenge_failed, or "".
	LastError string
	// FailedSince is when failures began, zero when there are none.
	FailedSince time.Time
}

// SubscribeResult is what an events/subscribe answered.
type SubscribeResult struct {
	// ID is the subscription's id, which arrives in the
	// X-MCP-Subscription-Id header of each delivery.
	ID string
	// RefreshBefore is when the grant ends and the client must subscribe
	// again, the same key, to keep it; nil is a subscription without
	// expiry.
	RefreshBefore *time.Time
	// Cursor is a position safe to keep: everything at or before it was
	// acknowledged or given up on. Nil for a type that does not replay.
	Cursor *string
	// Truncated says delivery started later than the cursor sent, or
	// that an event was given up on since: events may be missing, and
	// Cursor is the position to keep.
	Truncated bool
	// DeliveryStatus is set on a refresh, and may be nil.
	DeliveryStatus *DeliveryStatus
}

type deliveryWire struct {
	Mode   string `json:"mode,omitempty"`
	URL    string `json:"url"`
	Secret string `json:"secret,omitempty"`
}

type subscribeEventsParams struct {
	sdk.ParamsBase
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Delivery  deliveryWire    `json:"delivery"`
	Cursor    *string         `json:"cursor"`
	MaxAgeMs  *int64          `json:"maxAgeMs,omitempty"`
	TTLMs     json.RawMessage `json:"ttlMs,omitempty"`
}

type subscribeEventsResult struct {
	sdk.ResultBase
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

type unsubscribeEventsParams struct {
	sdk.ParamsBase
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Delivery  deliveryWire    `json:"delivery"`
}

type emptyEventsResult struct{ sdk.ResultBase }

// SubscribeEvents makes or refreshes a webhook subscription. The server
// refuses unless the caller is authenticated, and verifies that the URL
// wants deliveries before it answers, by posting it a signed challenge
// the receiver must echo, so the receiver must be up, and know the
// Secret, first; see [NewWebhookReceiver]. A refusal is an [EventError].
// A server that refuses the request for its headers is asked again once
// after the event types are listed again, as the SEP says.
func (s *Remote) SubscribeEvents(ctx context.Context, p SubscribeParams) (*SubscribeResult, error) {
	cs, err := s.eventsSession()
	if err != nil {
		return nil, err
	}
	if p.TTL > 0 && p.NoExpiry {
		return nil, errors.New("mcp: events: TTL and NoExpiry are exclusive")
	}
	if _, ok := s.schemas.get(p.Name); !ok {
		if _, err := s.ListEvents(ctx); err != nil {
			return nil, err
		}
	}
	res, err := s.subscribeOnce(ctx, cs, p)
	var ee *EventError
	if errors.As(err, &ee) && ee.Code == EventsCodeHeaderMismatch {
		if _, lerr := s.ListEvents(ctx); lerr == nil {
			res, err = s.subscribeOnce(ctx, cs, p)
		}
	}
	return res, err
}

func (s *Remote) subscribeOnce(ctx context.Context, cs *sdk.ClientSession, p SubscribeParams) (*SubscribeResult, error) {
	wire := &subscribeEventsParams{
		Name: p.Name, Arguments: p.Arguments, Cursor: p.Cursor,
		Delivery: deliveryWire{Mode: "webhook", URL: p.URL, Secret: p.Secret},
	}
	if p.MaxAge > 0 {
		ms := p.MaxAge.Milliseconds()
		wire.MaxAgeMs = &ms
	}
	switch {
	case p.NoExpiry:
		wire.TTLMs = json.RawMessage("null")
	case p.TTL > 0:
		wire.TTLMs = json.RawMessage(fmt.Sprint(p.TTL.Milliseconds()))
	}
	d, _ := s.schemas.get(p.Name)
	ctx = withEventHeaders(ctx, p.Name, d.InputSchema, p.Arguments)
	res, err := sdk.CallCustomMethod[*subscribeEventsParams, *subscribeEventsResult](ctx, cs, EventsSubscribeMethod, wire)
	if err != nil {
		return nil, eventErr(err)
	}
	out := &SubscribeResult{ID: res.ID, Cursor: res.Cursor, Truncated: res.Truncated}
	if res.RefreshBefore != nil {
		t, err := time.Parse(time.RFC3339Nano, *res.RefreshBefore)
		if err != nil {
			return nil, fmt.Errorf("mcp: events: refreshBefore %q: %w", *res.RefreshBefore, err)
		}
		out.RefreshBefore = &t
	}
	if st := res.DeliveryStatus; st != nil {
		ds := &DeliveryStatus{Active: st.Active}
		if st.LastDeliveryAt != nil {
			ds.LastDeliveryAt, _ = time.Parse(time.RFC3339Nano, *st.LastDeliveryAt)
		}
		if st.LastError != nil {
			ds.LastError = *st.LastError
		}
		if st.FailedSince != nil {
			ds.FailedSince, _ = time.Parse(time.RFC3339Nano, *st.FailedSince)
		}
		out.DeliveryStatus = ds
	}
	return out, nil
}

// UnsubscribeEvents ends a webhook subscription by its key, which is the
// event, its arguments and the URL; the id is not an input. A server
// that has no such subscription answers [EventsCodeNotFound], which is
// not a problem to a caller that wanted it gone.
func (s *Remote) UnsubscribeEvents(ctx context.Context, name string, args json.RawMessage, url string) error {
	cs, err := s.eventsSession()
	if err != nil {
		return err
	}
	if _, ok := s.schemas.get(name); !ok {
		if _, err := s.ListEvents(ctx); err != nil {
			return err
		}
	}
	d, _ := s.schemas.get(name)
	ctx = withEventHeaders(ctx, name, d.InputSchema, args)
	_, err = sdk.CallCustomMethod[*unsubscribeEventsParams, *emptyEventsResult](ctx, cs, EventsUnsubscribeMethod, &unsubscribeEventsParams{
		Name: name, Arguments: args, Delivery: deliveryWire{URL: url},
	})
	return eventErr(err)
}

// Subscription is a webhook subscription to keep alive with
// [Remote.KeepSubscribed].
type Subscription struct {
	// Params are what each subscribe sends. Their Cursor is the first
	// one's, and Cursor below is the later ones'.
	Params SubscribeParams
	// Cursor returns the last cursor the client kept, which each refresh
	// sends; nil, or a nil result, sends the cursor the last answer
	// carried, which is the server's watermark.
	Cursor func() *string
	// OnResult is called with each successful subscribe, the first
	// included, so a client can read the Cursor and the DeliveryStatus.
	OnResult func(*SubscribeResult)
	// OnError is called with each failed subscribe that is retried. A
	// failure that is not retried ends [Remote.KeepSubscribed].
	OnError func(error)
	// Refresh is the share of the grant that has passed when the
	// subscription is refreshed, drawn between Refresh and Refresh+0.3 so
	// clients do not refresh together; the default is 0.5, and it is
	// kept at 0.6 or less, so the draw stays under 0.9.
	Refresh float64
	// Retry is the first wait after a failure, doubled up to a minute;
	// the default is 5 seconds.
	Retry time.Duration
	// Idle is how long to wait when the grant is without expiry, which
	// the SEP still asks a client to refresh now and then; the default
	// is an hour.
	Idle time.Duration
	// Jitter returns a number in [0, 1); the default is crypto/rand.
	Jitter func() float64
	// Now and Sleep are the clock and the wait, which a test sets.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
}

// KeepSubscribed subscribes and keeps the subscription alive: it
// refreshes before the grant ends, with the last cursor, until ctx ends.
// A failure that may pass, a network error or a server that is busy, is
// retried with a growing wait, and reported to OnError. One that will
// not, [EventsCodeForbidden], [EventsCodeInvalidParams],
// [EventsCodeNotFound], [EventsCodeUnsupported] or
// [EventsCodeCallbackEndpoint] or a missing extension, ends the loop with
// that error: the event type may be gone, so list again and subscribe
// anew, or the caller may not subscribe at all. KeepSubscribed does not
// unsubscribe when ctx ends; call [Remote.UnsubscribeEvents].
func (s *Remote) KeepSubscribed(ctx context.Context, sub Subscription) error {
	now := sub.Now
	if now == nil {
		now = time.Now
	}
	sleep := sub.Sleep
	if sleep == nil {
		sleep = sleepTimer
	}
	jitter := sub.Jitter
	if jitter == nil {
		jitter = cryptoJitter
	}
	share := sub.Refresh
	if share <= 0 {
		share = 0.5
	}
	share = math.Min(share, 0.6)
	retry := sub.Retry
	if retry <= 0 {
		retry = 5 * time.Second
	}
	idle := sub.Idle
	if idle <= 0 {
		idle = time.Hour
	}
	params := sub.Params
	wait := retry
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		res, err := s.SubscribeEvents(ctx, params)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			var ee *EventError
			if errors.As(err, &ee) {
				switch ee.Code {
				case EventsCodeForbidden, EventsCodeInvalidParams, EventsCodeNotFound, EventsCodeUnsupported, EventsCodeCallbackEndpoint:
					return err
				}
			}
			if errors.Is(err, ErrEventsNotOffered) {
				return err
			}
			if sub.OnError != nil {
				sub.OnError(err)
			}
			if err := sleep(ctx, wait); err != nil {
				return err
			}
			wait = min(wait*2, time.Minute)
			continue
		}
		wait = retry
		if sub.OnResult != nil {
			sub.OnResult(res)
		}
		params.Cursor = res.Cursor
		if sub.Cursor != nil {
			if c := sub.Cursor(); c != nil {
				params.Cursor = c
			}
		}
		d := idle
		if res.RefreshBefore != nil {
			remaining := res.RefreshBefore.Sub(now())
			d = time.Duration(float64(remaining) * (share + 0.3*jitter()))
			d = max(d, time.Second)
			if d > remaining && remaining > 0 {
				d = remaining
			}
		}
		if err := sleep(ctx, d); err != nil {
			return err
		}
		// The cursor of the client is read when the wait ends: the
		// receiver has been keeping it meanwhile.
		if sub.Cursor != nil {
			if c := sub.Cursor(); c != nil {
				params.Cursor = c
			}
		}
	}
}

func cryptoJitter() float64 {
	n, err := rand.Int(rand.Reader, big.NewInt(1<<53))
	if err != nil {
		return 0.5
	}
	return float64(n.Int64()) / float64(1<<53)
}
