package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agenttool/mcpclient"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// claimer is a set of tools that make claims and count what they are
// asked, failing the test if anything runs one.
type claimer struct {
	t        *testing.T
	executed atomic.Int64
	asked    atomic.Int64
}

func (c *claimer) never(context.Context, agenttool.Call) (agenttool.Result, error) {
	c.executed.Add(1)
	c.t.Error("a tool ran")
	return agenttool.Text("ran"), nil
}

func (c *claimer) tool(name string, opts ...agenttool.Option) agenttool.Tool {
	return agenttool.NewFunc(name, name, json.RawMessage(`{"type":"object"}`), c.never, opts...)
}

// facts claims fn and counts each claim asked.
func (c *claimer) facts(fn func(ctx context.Context, args json.RawMessage) (agenttool.Facts, error)) agenttool.Option {
	return agenttool.WithFacts(func(ctx context.Context, args json.RawMessage) (agenttool.Facts, error) {
		c.asked.Add(1)
		return fn(ctx, args)
	})
}

func replay(r agenttool.Replay) agenttool.Option {
	return agenttool.WithReplay(func(context.Context, json.RawMessage) agenttool.Replay { return r })
}

// shell claims a read and a write and stamps its arguments, as a shell
// whose command is "cat .env > out/x" would.
func (c *claimer) shell() agenttool.Tool {
	return c.tool("sh", c.facts(func(_ context.Context, args json.RawMessage) (agenttool.Facts, error) {
		var a struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return agenttool.Facts{}, err
		}
		return agenttool.Facts{
			Calls: []agenttool.FactCall{
				{Tool: "read", Args: json.RawMessage(`{"path":".env"}`), Text: "read .env"},
				{Tool: "write", Args: json.RawMessage(`{"path":"out/x"}`)},
				{Args: args},
			},
			Rewrite: json.RawMessage(`{"command":` + strconv.Quote(a.Command) + `,"stamp":"abc"}`),
		}, nil
	}), replay(agenttool.ReplaySafe))
}

// bare is a tool that declares nothing at all, not even replay, which
// every tool agenttool builds answers.
type bare struct{ name string }

func (b bare) Name() string                { return b.name }
func (b bare) Description() string         { return b.name }
func (b bare) Parameters() json.RawMessage { return nil }
func (b bare) Execute(context.Context, agenttool.Call) (agenttool.Result, error) {
	return agenttool.Text("ran"), nil
}

func (c *claimer) tools() []agenttool.Tool {
	return []agenttool.Tool{
		c.shell(),
		c.tool("nothing", c.facts(func(context.Context, json.RawMessage) (agenttool.Facts, error) {
			return agenttool.Facts{Calls: []agenttool.FactCall{}}, nil
		})),
		c.tool("itself", c.facts(func(context.Context, json.RawMessage) (agenttool.Facts, error) {
			return agenttool.Facts{}, nil
		})),
		c.tool("broken", c.facts(func(context.Context, json.RawMessage) (agenttool.Facts, error) {
			return agenttool.Facts{}, errors.New("cannot read the command")
		})),
		c.tool("session", c.facts(func(ctx context.Context, _ json.RawMessage) (agenttool.Facts, error) {
			if _, ok := SessionFrom(ctx); !ok {
				return agenttool.Facts{}, errors.New("no session")
			}
			return agenttool.Facts{}, nil
		})),
		c.tool("keyed", replay(agenttool.ReplayKeyed)),
		c.tool("plain"),
		bare{name: "bare"},
	}
}

// countFacts counts the execution/facts requests s receives.
func countFacts(s *sdk.Server) *atomic.Int64 {
	var n atomic.Int64
	s.AddReceivingMiddleware(func(next sdk.MethodHandler) sdk.MethodHandler {
		return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
			if method == FactsMethod {
				n.Add(1)
			}
			return next(ctx, method, req)
		}
	})
	return &n
}

func claimsOf(t *testing.T, s *mcpclient.Remote, name string, args string) (agenttool.Facts, bool, error) {
	t.Helper()
	tl, ok := agenttool.Set(s.Tools()).Lookup(name)
	if !ok {
		t.Fatalf("no tool %q", name)
	}
	return agenttool.FactsOf(context.Background(), tl, json.RawMessage(args))
}

func TestFactsRoundTrip(t *testing.T) {
	c := &claimer{t: t}
	s := roundTrip(t, newServer(t, "claims", c.tools()...), mcpclient.WithClaims())

	f, claims, err := claimsOf(t, s, "sh", `{"command":"cat .env > out/x"}`)
	if err != nil || !claims {
		t.Fatalf("sh: claims %v, err %v", claims, err)
	}
	if len(f.Calls) != 3 {
		t.Fatalf("sh: calls = %+v", f.Calls)
	}
	if f.Calls[0].Tool != "read" || string(f.Calls[0].Args) != `{"path":".env"}` || f.Calls[0].Text != "read .env" {
		t.Errorf("sh: first call = %+v", f.Calls[0])
	}
	if f.Calls[1].Tool != "write" || f.Calls[1].Text != "" {
		t.Errorf("sh: second call = %+v", f.Calls[1])
	}
	if f.Calls[2].Tool != "" || normalise(t, f.Calls[2].Args) != normalise(t, json.RawMessage(`{"command":"cat .env > out/x"}`)) {
		t.Errorf("sh: the call itself = %+v", f.Calls[2])
	}
	if normalise(t, f.Rewrite) != normalise(t, json.RawMessage(`{"command":"cat .env > out/x","stamp":"abc"}`)) {
		t.Errorf("sh: rewrite = %s", f.Rewrite)
	}

	f, claims, err = claimsOf(t, s, "nothing", `{}`)
	if err != nil || !claims || f.Calls == nil || len(f.Calls) != 0 {
		t.Errorf("nothing: %+v, claims %v, err %v; want empty, non-nil calls", f, claims, err)
	}
	f, claims, err = claimsOf(t, s, "itself", `{}`)
	if err != nil || !claims || f.Calls != nil || f.Rewrite != nil {
		t.Errorf("itself: %+v, claims %v, err %v; want nil calls and no rewrite", f, claims, err)
	}
	_, claims, err = claimsOf(t, s, "broken", `{}`)
	if !claims || err == nil || err.Error() != "cannot read the command" {
		t.Errorf("broken: claims %v, err %v", claims, err)
	}
	if _, _, err = claimsOf(t, s, "session", `{}`); err != nil {
		t.Errorf("session: the claim was asked without the session on its context: %v", err)
	}
	for _, name := range []string{"keyed", "plain", "bare"} {
		if f, claims, err := claimsOf(t, s, name, `{}`); claims || err != nil || f.Calls != nil {
			t.Errorf("%s: %+v, claims %v, err %v; want no claim", name, f, claims, err)
		}
	}
	if c.asked.Load() != 5 {
		t.Errorf("claims asked %d times, want 5", c.asked.Load())
	}
	if c.executed.Load() != 0 {
		t.Error("asking for facts ran a tool")
	}
}

func TestFactsReplayCarried(t *testing.T) {
	c := &claimer{t: t}
	s := roundTrip(t, newServer(t, "claims", c.tools()...), mcpclient.WithClaims())
	tools := agenttool.Set(s.Tools())
	for name, want := range map[string]agenttool.Replay{
		"sh":    agenttool.ReplaySafe,
		"keyed": agenttool.ReplayUnknown, // the key does not cross MCP
		"plain": agenttool.ReplayUnknown,
		"bare":  agenttool.ReplayUnknown,
	} {
		tl, _ := tools.Lookup(name)
		if got := agenttool.ReplayOf(context.Background(), tl, json.RawMessage(`{}`)); got != want {
			t.Errorf("%s: replay %v, want %v", name, got, want)
		}
	}
	if c.executed.Load() != 0 {
		t.Error("asking for replay ran a tool")
	}
}

func TestFactsBatch(t *testing.T) {
	c := &claimer{t: t}
	server := newServer(t, "claims", c.tools()...)
	requests := countFacts(server)
	s := roundTrip(t, server, mcpclient.WithPrefix("x"), mcpclient.WithClaims())
	ctx := context.Background()

	got, err := s.Facts(ctx,
		mcpclient.FactsCall{Name: "x__sh", Args: json.RawMessage(`{"command":"ls"}`)},
		mcpclient.FactsCall{Name: "x__bare"},
		mcpclient.FactsCall{Name: "x__broken"},
		mcpclient.FactsCall{Name: "sh"}, // not this remote's name for it
		mcpclient.FactsCall{Name: "x__nothing"},
		mcpclient.FactsCall{Name: "x__keyed"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if n := requests.Load(); n != 1 {
		t.Errorf("%d requests for one batch, want 1", n)
	}
	if a := got[0]; !a.Claimed || a.Err != nil || len(a.Facts.Calls) != 3 || a.Replay != agenttool.ReplaySafe {
		t.Errorf("sh: %+v", a)
	}
	if a := got[1]; a.Claimed || a.Err != nil || a.Replay != agenttool.ReplayUnknown {
		t.Errorf("bare: %+v", a)
	}
	if a := got[2]; !a.Claimed || a.Err == nil || a.Err.Error() != "cannot read the command" {
		t.Errorf("broken: %+v", a)
	}
	if a := got[3]; a.Err == nil {
		t.Errorf("a name that is not the remote's: %+v", a)
	}
	if a := got[4]; !a.Claimed || a.Facts.Calls == nil || len(a.Facts.Calls) != 0 {
		t.Errorf("nothing: %+v", a)
	}
	if a := got[5]; a.Claimed || a.Replay != agenttool.ReplayUnknown {
		t.Errorf("keyed: %+v", a)
	}

	// The batch answers as the tools do one at a time.
	for i, name := range []string{"x__sh", "x__bare", "x__broken", "", "x__nothing", "x__keyed"} {
		if name == "" {
			continue
		}
		tl, _ := agenttool.Set(s.Tools()).Lookup(name)
		args := json.RawMessage(`{}`)
		if name == "x__sh" {
			args = json.RawMessage(`{"command":"ls"}`)
		}
		f, claims, err := agenttool.FactsOf(ctx, tl, args)
		if claims != got[i].Claimed || (err == nil) != (got[i].Err == nil) || len(f.Calls) != len(got[i].Facts.Calls) || (f.Calls == nil) != (got[i].Facts.Calls == nil) {
			t.Errorf("%s: one at a time %+v %v %v, in the batch %+v", name, f, claims, err, got[i])
		}
		if r := agenttool.ReplayOf(ctx, tl, args); r != got[i].Replay {
			t.Errorf("%s: replay one at a time %v, in the batch %v", name, r, got[i].Replay)
		}
	}

	// A batch of tools that claim nothing is not sent.
	before := requests.Load()
	got, err = s.Facts(ctx, mcpclient.FactsCall{Name: "x__bare"}, mcpclient.FactsCall{Name: "x__bare"})
	if err != nil || len(got) != 2 || got[0].Claimed || got[1].Claimed {
		t.Errorf("bare batch: %+v, %v", got, err)
	}
	if requests.Load() != before {
		t.Error("a batch of tools that claim nothing was sent")
	}
	if c.executed.Load() != 0 {
		t.Error("asking for facts ran a tool")
	}
}

// TestFactsWithoutCapability: a server that marks its tools but does
// not answer the method is never asked, and its tools claim nothing.
func TestFactsWithoutCapability(t *testing.T) {
	c := &claimer{t: t}
	server := sdk.NewServer(&sdk.Implementation{Name: "old", Version: "1"}, nil)
	for _, tl := range c.tools() {
		h, err := Handler(tl)
		if err != nil {
			t.Fatal(err)
		}
		server.AddTool(Definition(tl), h)
	}
	requests := countFacts(server)
	s := roundTrip(t, server, mcpclient.WithClaims())
	tl, _ := agenttool.Set(s.Tools()).Lookup("sh")
	if agenttool.IsFactual(tl) {
		t.Error("a tool of a server without the capability claims facts")
	}
	if r := agenttool.ReplayOf(context.Background(), tl, json.RawMessage(`{}`)); r != agenttool.ReplayUnknown {
		t.Errorf("replay = %v, want unknown", r)
	}
	got, err := s.Facts(context.Background(), mcpclient.FactsCall{Name: "sh"}, mcpclient.FactsCall{Name: "broken"})
	if err != nil || got[0].Claimed || got[1].Claimed {
		t.Errorf("batch: %+v, %v", got, err)
	}
	if requests.Load() != 0 || c.asked.Load() != 0 {
		t.Error("a server without the capability was asked")
	}
}

// TestFactsIgnoredByDefault: a host takes a server's claims only by
// opting in, since a claim is what a policy decides on and a server
// that lied could steer it. Without WithClaims the server is never
// asked, even though it advertises the method, and its tools claim
// nothing; what the listing says of ordering still applies.
func TestFactsIgnoredByDefault(t *testing.T) {
	c := &claimer{t: t}
	server := newServer(t, "claims", append(c.tools(), c.tool("seq", agenttool.WithSequential()))...)
	requests := countFacts(server)
	s := roundTrip(t, server)
	set := agenttool.Set(s.Tools())
	for _, name := range []string{"sh", "nothing", "broken"} {
		tl, _ := set.Lookup(name)
		if agenttool.IsFactual(tl) {
			t.Errorf("%s claims facts without WithClaims", name)
		}
	}
	sh, _ := set.Lookup("sh")
	if r := agenttool.ReplayOf(context.Background(), sh, nil); r != agenttool.ReplayUnknown {
		t.Errorf("sh: replay %v without WithClaims", r)
	}
	if got, err := s.Facts(context.Background(), mcpclient.FactsCall{Name: "sh"}, mcpclient.FactsCall{Name: "broken"}); err != nil || got[0].Claimed || got[1].Claimed || got[0].Replay != agenttool.ReplayUnknown {
		t.Errorf("batch: %+v, %v", got, err)
	}
	if requests.Load() != 0 || c.asked.Load() != 0 {
		t.Error("the server was asked without WithClaims")
	}
	if seq, _ := set.Lookup("seq"); !agenttool.IsSequential(seq) {
		t.Error("sequential from the listing needs no opt-in, and was dropped")
	}
}

func TestFactsCancelled(t *testing.T) {
	started, stopped := make(chan struct{}), make(chan error, 1)
	slow := agenttool.NewFunc("slow", "slow", nil, func(context.Context, agenttool.Call) (agenttool.Result, error) {
		t.Error("a tool ran")
		return agenttool.Result{}, nil
	}, agenttool.WithFacts(func(ctx context.Context, _ json.RawMessage) (agenttool.Facts, error) {
		close(started)
		<-ctx.Done()
		stopped <- ctx.Err()
		return agenttool.Facts{}, ctx.Err()
	}))
	s := roundTrip(t, newServer(t, "slow", slow), mcpclient.WithClaims())
	tl, _ := agenttool.Set(s.Tools()).Lookup("slow")
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		cancel()
	}()
	_, _, err := agenttool.FactsOf(ctx, tl, json.RawMessage(`{}`))
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	select {
	case err := <-stopped:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("the claim stopped with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the server's claim was not cancelled")
	}
}

func TestFactsClosed(t *testing.T) {
	started := make(chan struct{})
	slow := agenttool.NewFunc("slow", "slow", nil, func(context.Context, agenttool.Call) (agenttool.Result, error) {
		return agenttool.Result{}, nil
	}, agenttool.WithFacts(func(ctx context.Context, _ json.RawMessage) (agenttool.Facts, error) {
		close(started)
		<-ctx.Done()
		return agenttool.Facts{}, ctx.Err()
	}))
	s := roundTrip(t, newServer(t, "slow", slow), mcpclient.WithClaims())
	tl, _ := agenttool.Set(s.Tools()).Lookup("slow")
	go func() {
		<-started
		_ = s.Close()
	}()
	_, _, err := agenttool.FactsOf(context.Background(), tl, json.RawMessage(`{}`))
	if !errors.Is(err, mcpclient.ErrClosed) {
		t.Errorf("err = %v, want ErrClosed", err)
	}
	if _, _, err := agenttool.FactsOf(context.Background(), tl, json.RawMessage(`{}`)); !errors.Is(err, mcpclient.ErrClosed) {
		t.Errorf("after Close: err = %v, want ErrClosed", err)
	}
}

func TestToolMetaListing(t *testing.T) {
	c := &claimer{t: t}
	tools := append(c.tools(),
		c.tool("seq", agenttool.WithSequential()),
		c.tool("res", agenttool.WithResource("shell:session")),
		c.tool("ro", agenttool.WithAnnotations(agenttool.Annotations{ReadOnly: true})),
	)
	cs := rawClient(t, newServer(t, "claims", tools...), nil, nil)
	listed := map[string]*sdk.Tool{}
	for tl, err := range cs.Tools(context.Background(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		listed[tl.Name] = tl
	}
	for name, want := range map[string]mcpclient.ToolMeta{
		"sh":   {Facts: true, Replay: true},
		"seq":  {Replay: true, Sequential: true},
		"res":  {Replay: true, Resource: "shell:session"},
		"ro":   {Replay: true, ReadOnly: true},
		"bare": {},
	} {
		got, ok := mcpclient.ToolMetaOf(listed[name])
		if ok != (want != mcpclient.ToolMeta{}) || got != want {
			t.Errorf("%s: meta %+v (%v), want %+v", name, got, ok, want)
		}
	}

	s := roundTrip(t, newServer(t, "claims", tools...))
	set := agenttool.Set(s.Tools())
	seq, _ := set.Lookup("seq")
	res, _ := set.Lookup("res")
	if !agenttool.IsSequential(seq) {
		t.Error("seq is not sequential on the client")
	}
	if got := agenttool.ResourceOf(res); got != "shell:session" {
		t.Errorf("res: resource %q", got)
	}

	// The host's option wins over the server's word.
	s = roundTrip(t, newServer(t, "claims", tools...), mcpclient.WithResource("container:1", "res"), mcpclient.WithResource("", "sh"))
	set = agenttool.Set(s.Tools())
	res, _ = set.Lookup("res")
	if got := agenttool.ResourceOf(res); got != "container:1" {
		t.Errorf("res with WithResource: resource %q", got)
	}
}

// TestFactsAcrossAddTools: tools added by two calls are both answered,
// a name added again by the later, and the capability is advertised
// on a server the caller built, whichever protocol the client speaks.
func TestFactsAcrossAddTools(t *testing.T) {
	c := &claimer{t: t}
	server := sdk.NewServer(&sdk.Implementation{Name: "mine", Version: "1"}, nil)
	if err := AddTools(server, c.shell(), c.tool("again", c.facts(func(context.Context, json.RawMessage) (agenttool.Facts, error) {
		return agenttool.Facts{}, errors.New("first")
	}))); err != nil {
		t.Fatal(err)
	}
	if err := AddTools(server, c.tool("again", c.facts(func(context.Context, json.RawMessage) (agenttool.Facts, error) {
		return agenttool.Facts{}, errors.New("second")
	}))); err != nil {
		t.Fatal(err)
	}
	s := roundTrip(t, server, mcpclient.WithClaims())
	got, err := s.Facts(context.Background(), mcpclient.FactsCall{Name: "sh", Args: json.RawMessage(`{"command":"ls"}`)}, mcpclient.FactsCall{Name: "again"})
	if err != nil {
		t.Fatal(err)
	}
	if !got[0].Claimed || len(got[0].Facts.Calls) != 3 {
		t.Errorf("sh, added first: %+v", got[0])
	}
	if got[1].Err == nil || got[1].Err.Error() != "second" {
		t.Errorf("again, added twice: %+v", got[1])
	}

	for _, version := range []string{"", "2025-11-25"} {
		var opts *sdk.ClientSessionOptions
		if version != "" {
			opts = &sdk.ClientSessionOptions{ProtocolVersion: version}
		}
		cs := rawClient(t, server, nil, opts)
		caps := cs.InitializeResult().Capabilities
		if _, ok := caps.Experimental[FactsCapability]; !ok {
			t.Errorf("protocol %q: capabilities %+v do not advertise facts", version, caps)
		}
	}
}

// rawFacts is the result of execution/facts with each answer as sent.
type rawFacts struct {
	sdk.ResultBase
	Results []json.RawMessage `json:"results"`
}

// TestFactsWire asks as another client would and reads the answers as
// sent: a name no tool has is an error of its own beside the others,
// nil calls are null and empty ones [], and keyed is sent as unknown.
func TestFactsWire(t *testing.T) {
	c := &claimer{t: t}
	server := newServer(t, "claims", c.tools()...)
	ct, st := sdk.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := server.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	client := sdk.NewClient(&sdk.Implementation{Name: "raw", Version: "1"}, nil)
	if err := sdk.AddSendingCustomMethod[*factsParams, *rawFacts](client, FactsMethod); err != nil {
		t.Fatal(err)
	}
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	res, err := sdk.CallCustomMethod[*factsParams, *rawFacts](ctx, cs, FactsMethod, &factsParams{Calls: []factsCall{
		{Name: "missing"}, {Name: "itself"}, {Name: "nothing"}, {Name: "keyed"}, {Name: "bare"}, {Name: "sh", Arguments: json.RawMessage(`{"command":"ls"}`)},
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		`{"error":"unknown tool \"missing\"","replay":"unknown"}`,
		`{"facts":{"calls":null},"replay":"unknown"}`,
		`{"facts":{"calls":[]},"replay":"unknown"}`,
		`{"replay":"unknown"}`,
		`{"replay":"unknown"}`,
		`{"facts":{"calls":[{"tool":"read","args":{"path":".env"},"text":"read .env"},{"tool":"write","args":{"path":"out/x"}},{"tool":"","args":{"command":"ls"}}],"rewrite":{"command":"ls","stamp":"abc"}},"replay":"safe"}`,
	}
	if len(res.Results) != len(want) {
		t.Fatalf("%d answers for %d calls", len(res.Results), len(want))
	}
	for i, w := range want {
		if got := normalise(t, res.Results[i]); got != normalise(t, json.RawMessage(w)) {
			t.Errorf("answer %d = %s\nwant %s", i, got, w)
		}
	}
	if c.executed.Load() != 0 {
		t.Error("asking for facts ran a tool")
	}
}

// TestFactsConstantsMatchClient holds this module's wire names to
// mcpclient's, reading them out of the sibling's source as
// TestRecordMetaKeyMatchesClient does.
func TestFactsConstantsMatchClient(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "mcpclient", "facts.go"))
	if err != nil {
		t.Skip("mcpclient is not beside this module here: " + err.Error())
	}
	f, err := parser.ParseFile(token.NewFileSet(), "facts.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	theirs := map[string]string{}
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, name := range vs.Names {
				if i < len(vs.Values) {
					if lit, ok := vs.Values[i].(*ast.BasicLit); ok {
						theirs[name.Name], _ = strconv.Unquote(lit.Value)
					}
				}
			}
		}
	}
	for name, ours := range map[string]string{"FactsMethod": FactsMethod, "FactsCapability": FactsCapability, "ToolMetaKey": ToolMetaKey} {
		if theirs[name] != ours {
			t.Errorf("%s: mcpserver %q, mcpclient %q", name, ours, theirs[name])
		}
	}
}
