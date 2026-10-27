package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ChristopherDavenport/agenttool"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// FactsMethod is the MCP method that carries the facts claim
// ([agenttool.Factual]) and the replay claim ([agenttool.Replayable])
// of a server's tools, a model response's calls together. mcpserver
// answers it and documents the wire; its FactsMethod is this string
// byte for byte. It is a method rather than a reserved tool name, so it
// cannot reach the model's tool list.
const FactsMethod = "execution/facts"

// FactsCapability is the experimental server capability that says the
// server answers [FactsMethod]. A server that does not advertise it is
// never asked, and its tools make no claim.
const FactsCapability = "io.github.christopherdavenport.agenttool/facts"

// ToolMetaKey is the _meta key of a listed tool under which mcpserver
// puts the tool's properties MCP has no field for; see [ToolMeta].
const ToolMetaKey = "io.github.christopherdavenport.agenttool/tool"

// ToolMeta is what a server says about one of its tools under
// [ToolMetaKey]: whether it makes the facts claim and the replay claim,
// and whether it is read-only, sequential, a barrier, and the shared state it
// names. They are the server's statement about the tool, not the hints
// of its MCP annotations, and they are as trustworthy as the server.
type ToolMeta struct {
	// Facts says the tool makes the facts claim, so its calls are worth
	// a [FactsMethod] request; one that does not is its own one fact.
	Facts bool
	// Replay says the server answers the tool's replay claim.
	Replay bool
	// ReadOnly says the tool does not modify its environment.
	ReadOnly bool
	// Sequential says the tool must not run alongside other tools.
	Sequential bool
	// Barrier says each call of the tool runs alone in its place in the
	// batch ([agenttool.Barrier]). A server sends sequential with it, for
	// clients that predate barriers; a barrier is read as a barrier and
	// not as sequential.
	Barrier bool
	// Resource names the shared state the tool touches, "" for none.
	Resource string
	// Flow is the tool's flow ([agenttool.Flow]), nil when the server
	// states none. It is taken as stated only under [WithClaims]; see
	// [Remote.Tools] for how it is read otherwise.
	Flow *agenttool.Flow
}

// ToolMetaOf reads a listed tool's [ToolMeta], reporting false when it
// carries none, as a tool from a server other than mcpserver does. A
// field of the wrong type reads as unset.
func ToolMetaOf(t *sdk.Tool) (ToolMeta, bool) {
	if t == nil {
		return ToolMeta{}, false
	}
	m, ok := t.Meta[ToolMetaKey].(map[string]any)
	if !ok {
		return ToolMeta{}, false
	}
	flag := func(k string) bool { b, _ := m[k].(bool); return b }
	res, _ := m["resource"].(string)
	meta := ToolMeta{Facts: flag("facts"), Replay: flag("replay"), ReadOnly: flag("readOnly"), Sequential: flag("sequential"), Barrier: flag("barrier"), Resource: res, Flow: flowMeta(m["flow"])}
	if meta.Barrier {
		// sequential beside barrier is for clients that predate barriers.
		meta.Sequential = false
	}
	return meta, true
}

// flowMeta reads a flow from a tool's _meta, nil when there is none or
// it is not an object. A member of the wrong type reads as unset, and a
// label that is not a string as one this package does not know, which
// [agenttool.FlowOf] reads as untrusted.
func flowMeta(v any) *agenttool.Flow {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	strs := func(k string) []string {
		list, _ := m[k].([]any)
		var out []string
		for _, e := range list {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	f := &agenttool.Flow{To: strs("to"), Carries: strs("carries")}
	f.Sends, _ = m["sends"].(bool)
	list, _ := m["reads"].([]any)
	for _, e := range list {
		s, _ := e.(string)
		f.Reads = append(f.Reads, agenttool.Label(s))
	}
	return f
}

// flowOf is a listed tool's flow as this side reads it. MCP's open-world
// hint, true or absent, as MCP defaults it whether or not the tool
// carries an annotations block, is untrusted content and a call that
// sends; a hint of false says nothing, since a closed world is a
// loosening claim. The server's own flow is taken as stated when the
// host takes its claims. Otherwise it can only add: a flow that says a
// tool reads a label or sends makes the tool's flow the open-world
// reading with those labels added, whatever its hint said, and one that
// adds nothing leaves the tool as its hint left it. An untrusted server
// can so never state less than the open-world reading, make an unstated
// flow a stated one that reads nothing, or take a label away.
func flowOf(t *sdk.Tool, meta ToolMeta, trusted bool) (agenttool.Flow, bool) {
	openWorld := agenttool.Flow{Reads: []agenttool.Label{agenttool.LabelUntrusted}, Sends: true}
	stated := t.Annotations == nil || t.Annotations.OpenWorldHint == nil || *t.Annotations.OpenWorldHint
	switch {
	case meta.Flow == nil:
		if stated {
			return openWorld, true
		}
		return agenttool.Flow{}, false
	case trusted:
		return *meta.Flow, true
	case len(meta.Flow.Reads) == 0 && !meta.Flow.Sends:
		if stated {
			return openWorld, true
		}
		return agenttool.Flow{}, false
	}
	flow := openWorld
	for _, l := range meta.Flow.Reads {
		if !flow.ReadsLabel(l) {
			flow.Reads = append(flow.Reads, l)
		}
	}
	return flow, true
}

// FactsCall is one call [Remote.Facts] is asked about: a tool of the
// remote by its local name, prefix included, and the call's arguments.
type FactsCall struct {
	Name string
	Args json.RawMessage
}

// FactsAnswer is the answer for one call of [Remote.Facts], and is what
// [agenttool.FactsOf] and [agenttool.ReplayOf] report for that call on
// the remote's tool of that name.
type FactsAnswer struct {
	// Claimed says the tool makes the facts claim. When it does not,
	// Facts is the zero value, which reads as the call itself.
	Claimed bool
	// Facts is the tool's claim; see [agenttool.Facts].
	Facts agenttool.Facts
	// Err is the claim's error: a call nothing can be said about, which
	// a policy must not allow. A tool the remote does not have is one.
	Err error
	// Replay is whether the call may run again. It is
	// [agenttool.ReplayKeyed] never, since the idempotency key does not
	// cross MCP, and [agenttool.ReplayUnknown] for a tool the server does
	// not mark as answering.
	Replay agenttool.Replay
}

// factsCall is one call on the wire; see mcpserver.FactsMethod.
type factsCall struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

type factsParams struct {
	sdk.ParamsBase
	Calls []factsCall `json:"calls"`
}

type factCall struct {
	Tool string          `json:"tool"`
	Args json.RawMessage `json:"args,omitempty"`
	Text string          `json:"text,omitempty"`
}

type factsClaim struct {
	Calls   []factCall      `json:"calls"`
	Rewrite json.RawMessage `json:"rewrite,omitempty"`
}

type factsAnswer struct {
	Facts  *factsClaim `json:"facts,omitempty"`
	Hint   bool        `json:"hint,omitempty"`
	Error  string      `json:"error,omitempty"`
	Replay string      `json:"replay"`
}

type factsResult struct {
	sdk.ResultBase
	Results []factsAnswer `json:"results"`
}

// claim reads one answer as the claim a tool makes, its presence
// included: facts or an error is a claim, neither is none. Facts are
// hints when the server marked them so, or when hint says the host does
// not take the server's claims, and a hint has no rewrite.
func (a factsAnswer) claim(hint bool) (agenttool.Facts, bool, error) {
	switch {
	case a.Error != "":
		return agenttool.Facts{}, true, errors.New(a.Error)
	case a.Facts == nil:
		return agenttool.Facts{}, false, nil
	}
	f := agenttool.Facts{Hint: hint || a.Hint}
	if !f.Hint {
		f.Rewrite = rawOrNil(a.Facts.Rewrite)
	}
	if a.Facts.Calls != nil {
		f.Calls = make([]agenttool.FactCall, len(a.Facts.Calls))
		for i, c := range a.Facts.Calls {
			f.Calls[i] = agenttool.FactCall{Tool: c.Tool, Args: rawOrNil(c.Args), Text: c.Text}
		}
	}
	return f, true, nil
}

// replay reads one answer's replay. Keyed is unknown here, since this
// client sends no idempotency key for the server's tool to deduplicate
// on, and a value this package does not know is unknown too.
func (a factsAnswer) replay() agenttool.Replay {
	if a.Replay == agenttool.ReplaySafe.String() {
		return agenttool.ReplaySafe
	}
	return agenttool.ReplayUnknown
}

// rawOrNil is nil for absent or null JSON.
func rawOrNil(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return raw
}

// Facts asks the server about several calls in one request, as
// [FactsMethod] takes a model response's calls together, and answers
// each as [agenttool.FactsOf] and [agenttool.ReplayOf] would on the
// remote's tool, so a harness deciding a response's calls pays one
// round trip for all of them. It reads the current snapshot of the
// tools: a call naming no tool of the remote is answered with an error,
// and a call of a tool that claims neither facts nor replay is answered
// as making no claim without being sent. Without [WithClaims], or for a
// server that does not advertise [FactsCapability], nothing is sent and
// every answer is no claim. The error is the request's, the end of ctx or of the remote
// included; each call's own is in its answer. With [WithReconnect] an
// ended session is restarted first, and the calls are answered against
// the restarted server's tools; a request the end of its session cut is
// made once more.
func (s *Remote) Facts(ctx context.Context, calls ...FactsCall) ([]FactsAnswer, error) {
	for try := 0; ; try++ {
		out, again, err := s.facts(ctx, calls, try == 0)
		if !again {
			return out, err
		}
	}
}

// facts is one [Remote.Facts] on the session as it is. It reports again
// when the request was cut by the end of the session and may be made
// once more, which it may only when first is set.
func (s *Remote) facts(ctx context.Context, calls []FactsCall, first bool) (_ []FactsAnswer, again bool, _ error) {
	if _, err := s.live(ctx); err != nil {
		return nil, false, fmt.Errorf("mcp: %s: %w", FactsMethod, err)
	}
	out := make([]FactsAnswer, len(calls))
	s.mu.RLock()
	l, entries := s.link, s.claims
	s.mu.RUnlock()
	var ask []factsCall
	var at []int
	var metas []ToolMeta
	for i, c := range calls {
		e, ok := entries[c.Name]
		if !ok {
			out[i] = FactsAnswer{Claimed: true, Err: fmt.Errorf("mcp: no tool %q", c.Name)}
			continue
		}
		if !s.factsOn(l) || (!e.meta.Facts && !(e.meta.Replay && s.claimsOn(l))) {
			continue
		}
		ask = append(ask, factsCall{Name: e.remote, Arguments: c.Args})
		at = append(at, i)
		metas = append(metas, e.meta)
	}
	if len(ask) == 0 {
		return out, false, nil
	}
	got, err := s.askFacts(ctx, l, ask)
	if err != nil {
		return nil, first && s.resend(ctx, l, err), err
	}
	for j, i := range at {
		out[i] = answerFor(got[j], metas[j], s.claimsOn(l))
	}
	return out, false, nil
}

// answerFor reads a server's answer as what the local tool, built from
// meta, reports: a claim it does not make reads as none. Without
// trusted the facts are hints and the replay is not taken.
func answerFor(a factsAnswer, meta ToolMeta, trusted bool) FactsAnswer {
	var ans FactsAnswer
	if meta.Facts {
		ans.Facts, ans.Claimed, ans.Err = a.claim(!trusted)
	}
	if meta.Replay && trusted {
		ans.Replay = a.replay()
	}
	return ans
}

// claimsOn reports whether the tools listed on l carry the server's
// claims: the host passed [WithClaims] and the server advertises
// [FactsCapability].
func (s *Remote) claimsOn(l *link) bool { return s.opts.claims && l.factsCap }

// factsOn reports whether the tools listed on l carry the server's
// facts, as claims under [WithClaims] or as hints under
// [WithFactsHints], on a server that advertises [FactsCapability].
func (s *Remote) factsOn(l *link) bool { return (s.opts.claims || s.opts.hints) && l.factsCap }

// askOne asks the server about one call of the tool ref. With
// [WithReconnect] it is asked on the restarted server only when that
// still lists the tool as ref has it, and a request the end of its
// session cut is made once more.
func (s *Remote) askOne(ctx context.Context, ref toolRef, args json.RawMessage) (factsAnswer, error) {
	for try := 0; ; try++ {
		l, err := s.sendOn(ctx, ref)
		if err != nil {
			return factsAnswer{}, fmt.Errorf("mcp: %s: %w", FactsMethod, err)
		}
		got, err := s.askFacts(ctx, l, []factsCall{{Name: ref.remote, Arguments: args}})
		if err != nil {
			if try == 0 && s.resend(ctx, l, err) {
				continue
			}
			return factsAnswer{}, err
		}
		return got[0], nil
	}
}

// askFacts sends one [FactsMethod] request on l under ctx and returns
// one answer per call. [Remote.Close] ends it with [ErrClosed].
func (s *Remote) askFacts(ctx context.Context, l *link, calls []factsCall) ([]factsAnswer, error) {
	for i := range calls {
		if len(calls[i].Arguments) == 0 {
			calls[i].Arguments = json.RawMessage("{}")
		}
	}
	ctx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	id := s.callSeq.Add(1)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, fmt.Errorf("mcp: %s: %w", FactsMethod, ErrClosed)
	}
	s.asking[id] = stop
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.asking, id)
		s.mu.Unlock()
	}()
	res, err := sdk.CallCustomMethod[*factsParams, *factsResult](ctx, l.session, FactsMethod, &factsParams{Calls: calls})
	if err != nil {
		if cause := context.Cause(ctx); errors.Is(cause, ErrClosed) {
			err = cause
		}
		return nil, fmt.Errorf("mcp: %s: %w", FactsMethod, err)
	}
	if res == nil || len(res.Results) != len(calls) {
		n := 0
		if res != nil {
			n = len(res.Results)
		}
		return nil, fmt.Errorf("mcp: %s: %d answers for %d calls", FactsMethod, n, len(calls))
	}
	return res.Results, nil
}

// claimOptions are the options that give the tool ref, listed on l, the
// server's claims, as meta marks them, when the remote carries them.
func (s *Remote) claimOptions(l *link, ref toolRef, meta ToolMeta) []agenttool.Option {
	if !s.factsOn(l) {
		return nil
	}
	trusted := s.claimsOn(l)
	var opts []agenttool.Option
	if meta.Facts {
		opts = append(opts, agenttool.WithFacts(func(ctx context.Context, args json.RawMessage) (agenttool.Facts, error) {
			a, err := s.askOne(ctx, ref, args)
			if err != nil {
				return agenttool.Facts{}, err
			}
			f, _, err := a.claim(!trusted)
			return f, err
		}))
	}
	if meta.Replay && trusted {
		opts = append(opts, agenttool.WithReplay(func(ctx context.Context, args json.RawMessage) agenttool.Replay {
			a, err := s.askOne(ctx, ref, args)
			if err != nil {
				return agenttool.ReplayUnknown
			}
			return a.replay()
		}))
	}
	return opts
}
