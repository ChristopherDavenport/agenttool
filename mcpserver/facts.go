package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

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
//	{"facts": true, "replay": true, "readOnly": true, "sequential": true, "barrier": true, "resource": "shell:session"}
//
// A field that would be false or empty is left out, and a tool that has
// none of them carries no entry. facts says the tool makes the facts
// claim ([agenttool.IsFactual]), so a client that trusts the server
// asks [FactsMethod] about its calls and skips the request for one that
// does not; replay says the tool answers [agenttool.Replayable], which every tool [agenttool.New]
// and [agenttool.NewFunc] build does, so it says the server will answer
// and not that the answer is other than unknown. readOnly is the tool's
// [agenttool.Annotations] ReadOnly, sequential [agenttool.IsSequential]
// and resource [agenttool.ResourceOf]. barrier is [agenttool.IsBarrier],
// and comes with sequential: a client that predates barriers reads the
// tool as sequential and runs its batch one call at a time, which is
// stricter, never looser; a client that reads barrier takes the tool as
// a barrier alone. It is mcpclient.ToolMetaKey byte for byte.
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
	Facts *factsClaim `json:"facts,omitempty"`
	// Hint says the facts are hints, from a source the serving host
	// does not trust; see [agenttool.Facts.Hint].
	Hint   bool   `json:"hint,omitempty"`
	Error  string `json:"error,omitempty"`
	Replay string `json:"replay"`
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
	switch {
	case agenttool.IsSequential(tl):
		m["sequential"] = true
	case agenttool.IsBarrier(tl):
		// A client older than barriers reads sequential and runs the
		// batch one call at a time, as it did before: safe, never
		// looser. A client that reads barrier takes the tool as a
		// barrier and not as sequential.
		m["barrier"] = true
		m["sequential"] = true
	}
	if r := agenttool.ResourceOf(tl); r != "" {
		m["resource"] = r
	}
	if f, ok := agenttool.FlowOf(tl); ok {
		reads := make([]string, len(f.Reads))
		for i, l := range f.Reads {
			reads[i] = string(l)
		}
		flow := map[string]any{"reads": reads, "sends": f.Sends}
		if len(f.To) > 0 {
			flow["to"] = f.To
		}
		if len(f.Carries) > 0 {
			flow["carries"] = f.Carries
		}
		m["flow"] = flow
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
func serveFacts(s *sdk.Server, tools []agenttool.Tool, l *listing) error {
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
				case *sdk.ListToolsResult:
					var session *sdk.ServerSession
					if lr, ok := req.(*sdk.ListToolsRequest); ok {
						session = lr.Session
					}
					l.current(session, r)
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
		a.Hint = f.Hint
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

// listing keeps the definitions one [Options.AddTools] served in step
// with its tools, whose properties a stand-in such as [agenttool.Follow]
// takes from whichever tool is in force. The SDK keeps the definition
// a tool was added with, so a listing would otherwise state the facts
// mark, sequential, resource and flow the tool had then: a tool that
// gained a facts claim would be listed without it, and a client that
// takes the server's claims would decide its calls by name.
//
// Two things keep the listing right. Each tools/list result has the
// definitions of the tools this layer serves read again from the tools,
// so whatever a client lists is the tool in force; reading them at list
// time needs nothing from the tool, where a hook that told the server
// of a change would need every stand-in to carry one. And each call of
// a tool compares its definition with the one last added to the server
// and, when it changed, adds it again, which has the SDK send every
// client a tool-list-changed notification, since a client lists only
// when it connects or is told to.
//
// A client session that listed a tool and calls it after it changed
// decided the call on what it listed, which may lack the claim the tool
// now makes, so the call is refused and the client told to list again.
// The listing each session was last given is kept per session, so one
// client's re-listing never stands for another's; a session that never
// listed, as over stateless HTTP, where a session lasts one request, has
// nothing to be stale against and is let through.
type listing struct {
	server *sdk.Server
	byName map[string]agenttool.Tool

	mu sync.Mutex
	// added is each tool's definition as last added to the server, as
	// JSON, and handlers the handler it was added with.
	added    map[string]string
	handlers map[string]sdk.ToolHandler
	// listed is, per client session, each tool's definition as that
	// session was last listed it, dropped when the session ends.
	listed map[*sdk.ServerSession]map[string]string
}

func newListing(s *sdk.Server, tools []agenttool.Tool) *listing {
	l := &listing{server: s, byName: make(map[string]agenttool.Tool, len(tools)), added: map[string]string{}, handlers: map[string]sdk.ToolHandler{}, listed: map[*sdk.ServerSession]map[string]string{}}
	for _, tl := range tools {
		l.byName[tl.Name()] = tl
	}
	return l
}

// add adds tl to the server with h, which first checks whether tl's
// definition has changed since.
func (l *listing) add(tl agenttool.Tool, h sdk.ToolHandler) {
	name := tl.Name()
	checked := func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		key := l.check(name)
		if req.Params.RequestState == "" && l.stale(req.Session, name, key) {
			// A call that resumes one already running was let in when
			// it began.
			return errorResult(fmt.Errorf("tool %q changed since this client listed it, so the call was decided on the old listing and was not run; list the tools again and decide the call anew", name)), nil
		}
		return h(ctx, req)
	}
	def := Definition(tl)
	l.mu.Lock()
	l.added[name], l.handlers[name] = definitionKey(def), checked
	l.mu.Unlock()
	l.server.AddTool(def, checked)
}

// check adds the tool name again when its definition is not the one
// last added, so the SDK tells clients the list changed. Only the
// handler the server routes the name's calls to calls it, which is the
// one added last, so a layer whose tool another AddTools replaced never
// adds its own back.
// It returns the definition's key as it is now.
func (l *listing) check(name string) string {
	def := Definition(l.byName[name])
	key := definitionKey(def)
	l.mu.Lock()
	if l.added[name] == key {
		l.mu.Unlock()
		return key
	}
	l.added[name] = key
	h := l.handlers[name]
	l.mu.Unlock()
	l.server.AddTool(def, h)
	return key
}

// stale reports whether session was last listed the tool name with a
// definition other than key. A session that never listed it is not.
func (l *listing) stale(session *sdk.ServerSession, name, key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	was, ok := l.listed[session][name]
	return ok && was != key
}

// record keeps what session was listed, and forgets the session when it
// ends.
func (l *listing) record(session *sdk.ServerSession, keys map[string]string) {
	if session == nil || len(keys) == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	seen, ok := l.listed[session]
	if !ok {
		seen = map[string]string{}
		l.listed[session] = seen
		go func() {
			_ = session.Wait()
			l.mu.Lock()
			delete(l.listed, session)
			l.mu.Unlock()
		}()
	}
	for name, key := range keys {
		seen[name] = key
	}
}

// current puts in r the definitions of the tools this layer serves as
// they are now. The SDK's tools are copied rather than changed, since
// it lists the same values again; what [Definition] does not set, an
// output schema or icons, is kept. A layer added later wraps this one,
// so it rewrites last and a name added twice is listed as added last.
// What session is listed is recorded for [listing.stale].
func (l *listing) current(session *sdk.ServerSession, r *sdk.ListToolsResult) {
	keys := map[string]string{}
	defer func() { l.record(session, keys) }()
	for i, t := range r.Tools {
		if t == nil {
			continue
		}
		tl, ok := l.byName[t.Name]
		if !ok {
			continue
		}
		def := Definition(tl)
		keys[t.Name] = definitionKey(def)
		c := *t
		c.Title, c.Description, c.InputSchema, c.Annotations, c.Meta = def.Title, def.Description, def.InputSchema, def.Annotations, def.Meta
		r.Tools[i] = &c
	}
}

// definitionKey is a definition as JSON, for telling whether one
// changed; a definition that does not encode is never equal to one.
func definitionKey(def *sdk.Tool) string {
	data, err := json.Marshal(def)
	if err != nil {
		return "unencodable: " + err.Error()
	}
	return string(data)
}
