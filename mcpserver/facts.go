package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ChristopherDavenport/agenttool"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// FactsMethod is the MCP method that carries the facts claim
// ([agenttool.Factual]) and the replay claim ([agenttool.Replayable])
// of the tools a server serves. It is mcpclient.FactsMethod byte for
// byte, and a test holds the two together.
//
// It is a method of its own rather than a reserved tool name, since a
// tool could reach the model's tool list if a client failed to hide it
// and a method cannot. Its params are a list of calls, a model
// response's calls together, so a response of five calls costs one
// round trip rather than five:
//
//	{"calls": [{"name": "bash", "arguments": {"command": "cat .env > out/x"}}]}
//
// Its result has one answer per call, in the order asked:
//
//	{"results": [{"facts": {"calls": [{"tool": "read", "args": {"path": ".env"}},
//	                                  {"tool": "write", "args": {"path": "out/x"}}],
//	                        "rewrite": {...}},
//	              "replay": "unknown"}]}
//
// An answer carries facts when the tool makes the claim, with calls
// null for the call itself and an empty list for a call that amounts to
// nothing the tool can state, and rewrite absent for none; error in
// facts' place when the claim failed or no tool has the name; neither
// when the tool makes no claim. Replay is "safe" or "unknown", the
// tool's answer for the call, and is there for every tool the server
// serves. A tool's "keyed" answer is sent as "unknown", since MCP
// carries no idempotency key and a call this package serves has none to
// deduplicate on.
//
// Answering never acts: the handler asks each tool's claims and never
// runs Execute.
const FactsMethod = "execution/facts"

// FactsCapability is the experimental capability under which a server
// that answers [FactsMethod] says so, at initialize and in discovery, so
// that a client knows to ask. Its value is an empty object. It is
// mcpclient.FactsCapability byte for byte.
const FactsCapability = "io.github.christopherdavenport.agenttool/facts"

// ToolMetaKey is the _meta key of a served tool's definition under which
// the tool's properties cross in the listing, as facts about the tool
// rather than as MCP annotations, which are hints a policy must not use
// alone:
//
//	{"facts": true, "replay": true, "readOnly": true, "sequential": true, "resource": "shell:session"}
//
// A field that would be false or empty is left out, and a tool that has
// none of them carries no entry. facts says the tool makes the facts
// claim ([agenttool.IsFactual]), so a client that trusts the server
// asks [FactsMethod] about its calls and skips the request for one that
// does not; replay says the tool answers [agenttool.Replayable], which every tool [agenttool.New]
// and [agenttool.NewFunc] build does, so it says the server will answer
// and not that the answer is other than unknown. readOnly is the tool's
// [agenttool.Annotations] ReadOnly, sequential [agenttool.IsSequential]
// and resource [agenttool.ResourceOf]. It is mcpclient.ToolMetaKey byte
// for byte.
const ToolMetaKey = "io.github.christopherdavenport.agenttool/tool"

// factsCall is one call [FactsMethod] is asked about.
type factsCall struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// factsParams are the params of [FactsMethod].
type factsParams struct {
	sdk.ParamsBase
	Calls []factsCall `json:"calls"`
}

// factCall is an [agenttool.FactCall] on the wire.
type factCall struct {
	Tool string          `json:"tool"`
	Args json.RawMessage `json:"args,omitempty"`
	Text string          `json:"text,omitempty"`
}

// factsClaim is an [agenttool.Facts] on the wire. Calls has no
// omitempty, so nil, the call itself, is sent as null and an empty list
// as [], which a reader must not take for the call itself.
type factsClaim struct {
	Calls   []factCall      `json:"calls"`
	Rewrite json.RawMessage `json:"rewrite,omitempty"`
}

// factsAnswer is the answer for one call.
type factsAnswer struct {
	Facts  *factsClaim `json:"facts,omitempty"`
	Error  string      `json:"error,omitempty"`
	Replay string      `json:"replay"`
}

// factsResult is the result of [FactsMethod].
type factsResult struct {
	sdk.ResultBase
	Results []factsAnswer `json:"results"`
}

// toolMeta is the entry a tool's definition carries under [ToolMetaKey],
// nil when the tool has nothing to say.
func toolMeta(tl agenttool.Tool) map[string]any {
	m := map[string]any{}
	if agenttool.IsFactual(tl) {
		m["facts"] = true
	}
	if _, ok := tl.(agenttool.Replayable); ok {
		m["replay"] = true
	}
	if agenttool.AnnotationsOf(tl).ReadOnly {
		m["readOnly"] = true
	}
	if agenttool.IsSequential(tl) {
		m["sequential"] = true
	}
	if r := agenttool.ResourceOf(tl); r != "" {
		m["resource"] = r
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

// serveFacts has s answer [FactsMethod] for tools and advertise
// [FactsCapability]. Each call adds a layer of middleware that answers
// the calls naming its own tools and passes the rest inward, so tools
// added by several calls of [AddTools] are all answered, and a tool added
// again is answered by the layer that added it last, as the SDK serves
// the definition added last. The innermost handler answers a name no
// layer serves with an error.
func serveFacts(s *sdk.Server, tools []agenttool.Tool) error {
	err := sdk.AddReceivingCustomMethod(s, FactsMethod, func(_ context.Context, _ *sdk.ServerSession, p *factsParams) (*factsResult, error) {
		res := &factsResult{Results: make([]factsAnswer, len(p.Calls))}
		for i, c := range p.Calls {
			res.Results[i] = factsAnswer{Error: fmt.Sprintf("unknown tool %q", c.Name), Replay: agenttool.ReplayUnknown.String()}
		}
		return res, nil
	})
	if err != nil {
		return fmt.Errorf("mcpserver: %w", err)
	}
	byName := make(map[string]agenttool.Tool, len(tools))
	for _, tl := range tools {
		byName[tl.Name()] = tl
	}
	s.AddReceivingMiddleware(func(next sdk.MethodHandler) sdk.MethodHandler {
		return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
			if method == FactsMethod {
				if r, ok := req.(*sdk.ServerRequest[*factsParams]); ok {
					return answerFacts(ctx, next, method, r, byName)
				}
			}
			res, err := next(ctx, method, req)
			if err == nil {
				switch r := res.(type) {
				case *sdk.InitializeResult:
					advertise(r.Capabilities)
				case *sdk.DiscoverResult:
					advertise(r.Capabilities)
				}
			}
			return res, err
		}
	})
	return nil
}

// advertise adds [FactsCapability] to caps, which the SDK builds afresh
// for each answer.
func advertise(caps *sdk.ServerCapabilities) {
	if caps == nil {
		return
	}
	if caps.Experimental == nil {
		caps.Experimental = map[string]any{}
	}
	caps.Experimental[FactsCapability] = map[string]any{}
}

// answerFacts answers the calls of req that name a tool of byName and
// passes the others to next, in one request, merging the answers back in
// the order asked.
func answerFacts(ctx context.Context, next sdk.MethodHandler, method string, req *sdk.ServerRequest[*factsParams], byName map[string]agenttool.Tool) (sdk.Result, error) {
	var calls []factsCall
	if req.Params != nil {
		calls = req.Params.Calls
	}
	if req.Session != nil {
		ctx = ContextWithSession(ctx, req.Session)
	}
	res := &factsResult{Results: make([]factsAnswer, len(calls))}
	var rest []factsCall
	var restAt []int
	for i, c := range calls {
		tl, ok := byName[c.Name]
		if !ok {
			rest, restAt = append(rest, c), append(restAt, i)
			continue
		}
		res.Results[i] = answerCall(ctx, tl, c.Arguments)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	if len(rest) == 0 {
		return res, nil
	}
	inner := &sdk.ServerRequest[*factsParams]{Session: req.Session, Extra: req.Extra, Params: &factsParams{ParamsBase: paramsBase(req.Params), Calls: rest}}
	out, err := next(ctx, method, inner)
	if err != nil {
		return nil, err
	}
	got, ok := out.(*factsResult)
	if !ok {
		return nil, fmt.Errorf("mcpserver: %s: answered with %T", method, out)
	}
	if len(got.Results) != len(rest) {
		return nil, fmt.Errorf("mcpserver: %s: %d answers for %d calls", method, len(got.Results), len(rest))
	}
	for j, i := range restAt {
		res.Results[i] = got.Results[j]
	}
	return res, nil
}

// paramsBase carries the request's _meta inward.
func paramsBase(p *factsParams) sdk.ParamsBase {
	if p == nil {
		return sdk.ParamsBase{}
	}
	return p.ParamsBase
}

// answerCall asks tl's claims about one call. It never runs the tool.
func answerCall(ctx context.Context, tl agenttool.Tool, args json.RawMessage) factsAnswer {
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	a := factsAnswer{Replay: servedReplay(agenttool.ReplayOf(ctx, tl, args)).String()}
	f, claims, err := agenttool.FactsOf(ctx, tl, args)
	switch {
	case !claims:
	case err != nil:
		a.Error = err.Error()
	default:
		a.Facts = claimOf(f)
	}
	return a
}

// servedReplay is the replay a served call can claim: keyed needs the
// idempotency key, which MCP does not carry, so it reads as unknown.
func servedReplay(r agenttool.Replay) agenttool.Replay {
	if r == agenttool.ReplayKeyed {
		return agenttool.ReplayUnknown
	}
	return r
}

// claimOf puts f on the wire, keeping nil calls apart from empty ones.
func claimOf(f agenttool.Facts) *factsClaim {
	c := &factsClaim{Rewrite: f.Rewrite}
	if f.Calls != nil {
		c.Calls = make([]factCall, len(f.Calls))
		for i, fc := range f.Calls {
			c.Calls[i] = factCall{Tool: fc.Tool, Args: fc.Args, Text: fc.Text}
		}
	}
	return c
}
