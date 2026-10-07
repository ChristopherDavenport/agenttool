package mcpserver

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/google/jsonschema-go/jsonschema"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// DefaultAbandonAfter is how long a call waits for the client to come
// back with the answers to its questions before it is stopped; see
// [Options.AbandonAfter]. The client comes back once the user has
// answered, so this is human time.
const DefaultAbandonAfter = 30 * time.Minute

// DefaultMaxRequests is how many requests the client makes of one call
// at most; see [Options.MaxRequests]. It is the Go SDK's limit: its
// client gives up on a call whose tenth request brings back a question
// rather than a result.
const DefaultMaxRequests = 10

// roundTripVersion is the protocol version from which a server asks by
// returning its questions as the call's result, and may no longer send
// elicitation/create while it serves a call.
const roundTripVersion = "2026-07-28"

// elicitation returns the elicitation a client offered, and false when
// it offered none, in which case a tool served to it asks whatever
// elicitor the host put on the context, or nobody.
func elicitation(session *sdk.ServerSession) (*sdk.ElicitationCapabilities, bool) {
	if session == nil {
		return nil, false
	}
	params := session.InitializeParams()
	if params == nil || params.Capabilities == nil || params.Capabilities.Elicitation == nil {
		return nil, false
	}
	return params.Capabilities.Elicitation, true
}

// roundTrips reports whether the client takes questions as a call's
// result, which is how a server must ask from 2026-07-28.
func roundTrips(session *sdk.ServerSession) bool {
	params := session.InitializeParams()
	return params != nil && params.ProtocolVersion >= roundTripVersion
}

// question is an elicitation as MCP carries it, with its form schema
// resolved so the answer can be checked against it.
type question struct {
	params *sdk.ElicitParams
	schema *jsonschema.Resolved
}

// noFields is the schema of a form that asks for nothing.
var noFields = json.RawMessage(`{"type":"object","properties":{}}`)

// questionOf maps a tool's elicitation to MCP's: a URL makes it a URL
// question, and anything else is a form.
func questionOf(q agenttool.Elicitation) (question, error) {
	p := &sdk.ElicitParams{Message: q.Message}
	if q.URL != "" {
		p.Mode, p.URL = "url", q.URL
		p.ElicitationID = randomID()
		return question{params: p}, nil
	}
	p.Mode = "form"
	if len(q.Schema) == 0 {
		// A question with no fields is a confirmation, which MCP asks as
		// a form with none. The schema is required in form mode, and a
		// client may refuse a form without one: Claude Code does.
		p.RequestedSchema = noFields
		return question{params: p}, nil
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(q.Schema, &schema); err != nil {
		return question{}, fmt.Errorf("mcpserver: elicitation schema: %w", err)
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		return question{}, fmt.Errorf("mcpserver: elicitation schema: %w", err)
	}
	p.RequestedSchema = q.Schema
	return question{params: p, schema: resolved}, nil
}

// offered reports whether the client takes a question of this mode. A
// client that names neither mode takes forms, as MCP says.
func offered(caps *sdk.ElicitationCapabilities, mode string) bool {
	if mode == "url" {
		return caps.URL != nil
	}
	return caps.Form != nil || caps.URL == nil
}

// answerOf maps the client's answer back, checking a form's content
// against the schema it was asked with: the client is meant to, and
// the tool should not have to trust that it did.
func answerOf(q question, res *sdk.ElicitResult) (agenttool.Answer, error) {
	ans := agenttool.Answer{Action: agenttool.Action(res.Action)}
	switch ans.Action {
	case agenttool.ActionAccept:
	case agenttool.ActionDecline, agenttool.ActionCancel:
		return ans, nil
	default:
		return agenttool.Answer{}, fmt.Errorf("mcpserver: elicitation answer: unknown action %q", res.Action)
	}
	if res.Content == nil {
		return ans, nil
	}
	if q.schema != nil {
		if err := q.schema.Validate(res.Content); err != nil {
			return agenttool.Answer{}, fmt.Errorf("mcpserver: elicitation answer: %w", err)
		}
	}
	content, err := json.Marshal(res.Content)
	if err != nil {
		return agenttool.Answer{}, fmt.Errorf("mcpserver: elicitation answer: %w", err)
	}
	ans.Content = content
	return ans, nil
}

// askDirectly is the elicitor for a client before 2026-07-28, which the
// server asks with a request of its own while the call runs. The SDK
// checks the answer against the schema.
func askDirectly(session *sdk.ServerSession, caps *sdk.ElicitationCapabilities) agenttool.Elicitor {
	return func(ctx context.Context, q agenttool.Elicitation) (agenttool.Answer, error) {
		mq, err := questionOf(q)
		if err != nil {
			return agenttool.Answer{}, err
		}
		if !offered(caps, mq.params.Mode) {
			return agenttool.Answer{Action: agenttool.ActionCancel}, nil
		}
		res, err := session.Elicit(ctx, mq.params)
		if err != nil {
			return agenttool.Answer{}, fmt.Errorf("mcpserver: elicitation: %w", err)
		}
		return answerOf(question{params: mq.params}, res)
	}
}

// A run is a call served to a client that takes questions as the
// call's result. MCP ends the request when the question goes back, and
// the client answers by calling again, so the tool runs on its own
// goroutine, detached from the request that started it, and is parked
// between requests under a request state the client echoes.
type run struct {
	session string
	caps    *sdk.ElicitationCapabilities
	cancel  context.CancelFunc

	// over is closed once nobody can collect the run's result: it went
	// back, or the run was stopped. It ends the run's watch on its
	// session.
	over     chan struct{}
	overOnce sync.Once

	// state is the request state the run is parked under, guarded by
	// the parked's mutex.
	state string

	// asks carries the tool's questions to whichever request is waiting
	// on the run; it is unbuffered, so a question asked between requests
	// waits for the next one.
	asks chan *ask

	// done is closed when the tool returns, with res and err set.
	done chan struct{}
	res  agenttool.Result
	err  error

	// pending is the round the client is answering, by the ID each
	// question went out under, and seq numbers those IDs. Only the
	// request that holds the run touches them.
	pending  map[string]*ask
	seq      int
	requests int
	timer    *time.Timer
}

// finish marks the run as one whose result nobody will collect.
func (r *run) finish() { r.overOnce.Do(func() { close(r.over) }) }

// An ask is one question waiting on its answer.
type ask struct {
	q     question
	reply chan reply
}

type reply struct {
	ans agenttool.Answer
	err error
}

// elicit is the run's elicitor: it hands the question to the request
// waiting on the run and waits for the answer that comes back with the
// next.
func (r *run) elicit(ctx context.Context, q agenttool.Elicitation) (agenttool.Answer, error) {
	mq, err := questionOf(q)
	if err != nil {
		return agenttool.Answer{}, err
	}
	if !offered(r.caps, mq.params.Mode) {
		return agenttool.Answer{Action: agenttool.ActionCancel}, nil
	}
	a := &ask{q: mq, reply: make(chan reply, 1)}
	select {
	case r.asks <- a:
	case <-ctx.Done():
		return agenttool.Answer{}, ctx.Err()
	}
	select {
	case rep := <-a.reply:
		return rep.ans, rep.err
	case <-ctx.Done():
		return agenttool.Answer{}, ctx.Err()
	}
}

// parked holds the runs of one tool whose questions are with a client,
// by the request state the client will echo.
type parked struct {
	abandonAfter time.Duration
	maxRequests  int

	mu   sync.Mutex
	runs map[string]*run
}

// park files r under a new request state and starts the clock on the
// client's answer.
func (p *parked) park(r *run) string {
	state := randomID()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.runs == nil {
		p.runs = make(map[string]*run)
	}
	p.runs[state], r.state = r, state
	r.timer = time.AfterFunc(p.abandonAfter, func() {
		if p.take(state, r.session) != nil {
			r.cancel()
			r.finish()
		}
	})
	return state
}

// drop stops r and removes it from the runs waiting for their client,
// if it is one of them.
func (p *parked) drop(r *run) {
	p.mu.Lock()
	if p.runs[r.state] == r {
		delete(p.runs, r.state)
		r.timer.Stop()
	}
	p.mu.Unlock()
	r.cancel()
	r.finish()
}

// take removes and returns the run filed under state, when there is one
// and it belongs to session. Each state answers once.
func (p *parked) take(state, session string) *run {
	p.mu.Lock()
	defer p.mu.Unlock()
	r, ok := p.runs[state]
	if !ok || r.session != session {
		return nil
	}
	delete(p.runs, state)
	r.timer.Stop()
	return r
}

// wait holds the request until the tool asks or returns. A question
// goes back as the call's result, with any others the tool has asked
// alongside it, and the run is parked for the answers; the result goes
// back as the call's. A request cancelled while it waits stops the tool.
//
// On the client's last request a question cannot go back, since the
// client would give up on the call instead of answering it, so it is
// answered with an error at once and the request waits on.
func (p *parked) wait(ctx context.Context, r *run) (*sdk.CallToolResult, error) {
	r.requests++
	for {
		select {
		case <-r.done:
			r.finish()
			return resultOf(r.res, r.err), nil
		case a := <-r.asks:
			asks := []*ask{a}
			for more := true; more; {
				select {
				case a := <-r.asks:
					asks = append(asks, a)
				default:
					more = false
				}
			}
			if p.maxRequests > 0 && r.requests >= p.maxRequests {
				err := fmt.Errorf("mcpserver: elicitation: the client makes %d requests of a call at most and this question would need one more; ask fewer questions, or ask them together", p.maxRequests)
				for _, a := range asks {
					a.reply <- reply{err: err}
				}
				continue
			}
			r.pending = make(map[string]*ask, len(asks))
			requests := make(sdk.InputRequestMap, len(asks))
			for _, a := range asks {
				r.seq++
				id := "elicit_" + strconv.Itoa(r.seq)
				r.pending[id], requests[id] = a, a.q.params
			}
			return &sdk.CallToolResult{InputRequests: requests, RequestState: p.park(r)}, nil
		case <-ctx.Done():
			r.cancel()
			r.finish()
			return nil, ctx.Err()
		}
	}
}

// watch stops r when its session ends, since no answer can come back
// on a session that has closed. A request-scoped session, stateless
// HTTP's, ends with each request while the client carries on, so a run
// started on one is left to the timer.
func (p *parked) watch(r *run, req *sdk.CallToolRequest) {
	if req.Session.ID() == "" && req.Extra != nil && req.Extra.Header != nil {
		return
	}
	end := ended(req.Session)
	go func() {
		select {
		case <-end:
			p.drop(r)
		case <-r.over:
		}
	}()
}

// sessionEnds closes a channel when each session a run is watching
// ends, with one goroutine per session however many runs it has.
var sessionEnds struct {
	mu sync.Mutex
	m  map[*sdk.ServerSession]chan struct{}
}

// ended returns a channel closed when session ends.
func ended(session *sdk.ServerSession) <-chan struct{} {
	sessionEnds.mu.Lock()
	defer sessionEnds.mu.Unlock()
	if ch, ok := sessionEnds.m[session]; ok {
		return ch
	}
	if sessionEnds.m == nil {
		sessionEnds.m = make(map[*sdk.ServerSession]chan struct{})
	}
	ch := make(chan struct{})
	sessionEnds.m[session] = ch
	go func() {
		_ = session.Wait()
		sessionEnds.mu.Lock()
		delete(sessionEnds.m, session)
		sessionEnds.mu.Unlock()
		close(ch)
	}()
	return ch
}

// resume takes the answers a client came back with to the run they
// belong to and waits on it again. A question the client left out is
// answered cancel, since nobody chose.
func (p *parked) resume(ctx context.Context, name string, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
	r := p.take(req.Params.RequestState, sessionID(req.Session))
	if r == nil {
		return errorResult(fmt.Errorf("mcpserver: tool %q: the call these answers belong to is no longer waiting for them; call the tool again", name)), nil
	}
	for id, a := range r.pending {
		var rep reply
		switch res := req.Params.InputResponses[id].(type) {
		case nil:
			rep.ans = agenttool.Answer{Action: agenttool.ActionCancel}
		case *sdk.ElicitResult:
			rep.ans, rep.err = answerOf(a.q, res)
		default:
			rep.err = fmt.Errorf("mcpserver: elicitation answer: the client answered with %T", res)
		}
		a.reply <- rep
	}
	r.pending = nil
	return p.wait(ctx, r)
}

// sessionID is the ID of the session a request came on, "" for none,
// which a parked run is bound to so that only its own client answers.
func sessionID(session *sdk.ServerSession) string {
	if session == nil {
		return ""
	}
	return session.ID()
}

// randomID is a random opaque string: the request state, which names a run
// only this process holds, and so carries nothing a client could forge
// or read, and a URL question's ID.
func randomID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%x", b)
}
