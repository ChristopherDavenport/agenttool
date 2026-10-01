package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agenttool/mcpclient"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

var confirmSchema = json.RawMessage(`{"type":"object","properties":{"confirm":{"type":"boolean"}},"required":["confirm"]}`)

// asker is a tool that asks each question in turn through the elicitor
// on its context, as a tool guarding a destructive step does, and
// reports every answer and whether its call stayed the same throughout.
func asker(questions ...agenttool.Elicitation) agenttool.Tool {
	return agenttool.NewFunc("deploy", "asks before it deploys", nil, func(ctx context.Context, call agenttool.Call) (agenttool.Result, error) {
		ask, ok := agenttool.ElicitorFrom(ctx)
		if !ok {
			return agenttool.Text("nobody to ask"), nil
		}
		var out []string
		for _, q := range questions {
			ans, err := ask(ctx, q)
			if err != nil {
				return agenttool.Result{}, err
			}
			out = append(out, string(ans.Action)+" "+string(ans.Content))
		}
		if c, _ := agenttool.CallFrom(ctx); c.ID != call.ID {
			return agenttool.Result{}, errors.New("the call on the context changed between questions")
		}
		return agenttool.Text(strings.Join(out, "; ")), nil
	})
}

var confirm = agenttool.Elicitation{Message: "deploy to production?", Schema: confirmSchema}

// answering is a host elicitor that accepts every question with
// content, and records each question and the call it came under.
type answering struct {
	mu      sync.Mutex
	asked   []string
	callIDs []string
	content string
	action  agenttool.Action
}

func (a *answering) elicit(ctx context.Context, q agenttool.Elicitation) (agenttool.Answer, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.asked = append(a.asked, q.Message)
	c, _ := agenttool.CallFrom(ctx)
	a.callIDs = append(a.callIDs, c.ID)
	action := a.action
	if action == "" {
		action = agenttool.ActionAccept
	}
	if action != agenttool.ActionAccept {
		return agenttool.Answer{Action: action}, nil
	}
	return agenttool.Answer{Action: action, Content: json.RawMessage(a.content)}, nil
}

func callVia(t *testing.T, s *mcpclient.Remote, ctx context.Context) (agenttool.Result, error) {
	t.Helper()
	tl, ok := agenttool.Set(s.Tools()).Lookup("deploy")
	if !ok {
		t.Fatal("no deploy tool")
	}
	return tl.Execute(ctx, agenttool.Call{ID: "call_1", Args: json.RawMessage(`{}`)})
}

// TestElicitationRoundTrips: at 2026-07-28 each question goes back as
// the call's result and the answer comes with the next request, and
// the tool, which ran on across both, gets it as a plain return.
func TestElicitationRoundTrips(t *testing.T) {
	second := agenttool.Elicitation{Message: "and restart it?", Schema: confirmSchema}
	s := roundTrip(t, newServer(t, "deployer", asker(confirm, second)), mcpclient.WithElicitation())
	host := &answering{content: `{"confirm":true}`}
	res, err := callVia(t, s, agenttool.ContextWithElicitor(context.Background(), host.elicit))
	if err != nil {
		t.Fatal(err)
	}
	if want := `accept {"confirm":true}; accept {"confirm":true}`; res.Output.Text != want {
		t.Errorf("output = %q, want %q", res.Output.Text, want)
	}
	if strings.Join(host.asked, "|") != "deploy to production?|and restart it?" {
		t.Errorf("asked %q", host.asked)
	}
	for _, id := range host.callIDs {
		if id != "call_1" {
			t.Errorf("the host saw the question under call %q; want the call that asked", id)
		}
	}
}

// TestElicitationDeclined: a no reaches the tool as a no, with no
// content.
func TestElicitationDeclined(t *testing.T) {
	s := roundTrip(t, newServer(t, "deployer", asker(confirm)), mcpclient.WithElicitation())
	host := &answering{action: agenttool.ActionDecline}
	res, err := callVia(t, s, agenttool.ContextWithElicitor(context.Background(), host.elicit))
	if err != nil || res.Output.Text != "decline " {
		t.Errorf("res = %q, err = %v; want decline", res.Output.Text, err)
	}
}

// TestElicitationByURL: a question that sends the user to a page goes
// as a URL question, so the tool never sees what the user types there.
func TestElicitationByURL(t *testing.T) {
	q := agenttool.Elicitation{Message: "sign in", URL: "https://example.com/auth"}
	var got agenttool.Elicitation
	s := roundTrip(t, newServer(t, "deployer", asker(q)), mcpclient.WithElicitation())
	ctx := agenttool.ContextWithElicitor(context.Background(), func(_ context.Context, q agenttool.Elicitation) (agenttool.Answer, error) {
		got = q
		return agenttool.Answer{Action: agenttool.ActionAccept}, nil
	})
	res, err := callVia(t, s, ctx)
	if err != nil || res.Output.Text != "accept " {
		t.Errorf("res = %q, err = %v", res.Output.Text, err)
	}
	if got.URL != q.URL || got.Schema != nil {
		t.Errorf("the host was asked %+v", got)
	}
}

// TestElicitationTogether: two questions a tool asks at once are
// answered each by its own answer, whether they go back in one round
// or two.
func TestElicitationTogether(t *testing.T) {
	tool := agenttool.NewFunc("deploy", "asks two things at once", nil, func(ctx context.Context, _ agenttool.Call) (agenttool.Result, error) {
		ask, _ := agenttool.ElicitorFrom(ctx)
		regions := []string{"us", "eu"}
		answers := make([]string, len(regions))
		errs := make([]error, len(regions))
		var wg sync.WaitGroup
		for i, region := range regions {
			wg.Go(func() {
				ans, err := ask(ctx, agenttool.Elicitation{Message: region, Schema: json.RawMessage(`{"type":"object","properties":{"region":{"type":"string"}}}`)})
				answers[i], errs[i] = string(ans.Content), err
			})
		}
		wg.Wait()
		if err := errors.Join(errs...); err != nil {
			return agenttool.Result{}, err
		}
		return agenttool.Text(strings.Join(answers, " ")), nil
	})
	s := roundTrip(t, newServer(t, "deployer", tool), mcpclient.WithElicitation())
	ctx := agenttool.ContextWithElicitor(context.Background(), func(_ context.Context, q agenttool.Elicitation) (agenttool.Answer, error) {
		return agenttool.Answer{Action: agenttool.ActionAccept, Content: json.RawMessage(`{"region":"` + q.Message + `"}`)}, nil
	})
	res, err := callVia(t, s, ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Output.Text != `{"region":"us"} {"region":"eu"}` {
		t.Errorf("output = %q; each question should get its own answer", res.Output.Text)
	}
}

// TestElicitationOlderClient: a client before 2026-07-28 is asked with
// a request of its own while the call runs, as the SDK allows there.
func TestElicitationOlderClient(t *testing.T) {
	var asked string
	cs := rawClient(t, newServer(t, "deployer", asker(confirm)), &sdk.ClientOptions{
		ElicitationHandler: func(_ context.Context, req *sdk.ElicitRequest) (*sdk.ElicitResult, error) {
			asked = req.Params.Message
			return &sdk.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
		},
	}, &sdk.ClientSessionOptions{ProtocolVersion: "2025-11-25"})
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "deploy"})
	if err != nil {
		t.Fatal(err)
	}
	if got := text(res); got != `accept {"confirm":true}` || asked != confirm.Message {
		t.Errorf("output = %q, asked %q", got, asked)
	}
}

// TestElicitationNotOffered: a client that offers no elicitation is
// asked nothing, and the tool finds nobody to ask, as before.
func TestElicitationNotOffered(t *testing.T) {
	s := roundTrip(t, newServer(t, "deployer", asker(confirm)))
	res, err := callVia(t, s, context.Background())
	if err != nil || res.Output.Text != "nobody to ask" {
		t.Errorf("res = %q, err = %v", res.Output.Text, err)
	}
}

// TestElicitationModeNotOffered: a client that takes forms alone is
// not sent a URL question, and the tool hears cancel: nobody chose.
func TestElicitationModeNotOffered(t *testing.T) {
	q := agenttool.Elicitation{Message: "sign in", URL: "https://example.com/auth"}
	cs := rawClient(t, newServer(t, "deployer", asker(q)), &sdk.ClientOptions{
		ElicitationHandler: func(context.Context, *sdk.ElicitRequest) (*sdk.ElicitResult, error) {
			t.Error("the client was sent a question it does not take")
			return &sdk.ElicitResult{Action: "accept"}, nil
		},
	}, nil)
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "deploy"})
	if err != nil || text(res) != "cancel " {
		t.Errorf("res = %q, err = %v; want cancel", text(res), err)
	}
}

// manual connects a client at 2026-07-28 that answers questions itself,
// with the SDK's round trips off, and returns the input-required result
// of a first call.
func manual(t *testing.T, tl agenttool.Tool) (*sdk.ClientSession, *sdk.CallToolResult) {
	t.Helper()
	return manualWith(t, Options{}, tl)
}

// manualWith is manual serving tl with o.
func manualWith(t *testing.T, o Options, tl agenttool.Tool) (*sdk.ClientSession, *sdk.CallToolResult) {
	t.Helper()
	server, err := o.NewServer("deployer", "1", tl)
	if err != nil {
		t.Fatal(err)
	}
	cs := rawClient(t, server, &sdk.ClientOptions{
		ElicitationHandler: func(context.Context, *sdk.ElicitRequest) (*sdk.ElicitResult, error) {
			return nil, errors.New("unused")
		},
		MultiRoundTrip: &sdk.MultiRoundTripOptions{Disabled: true},
	}, nil)
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "deploy"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.NeedsInput() || len(res.InputRequests) != 1 || res.RequestState == "" {
		t.Fatalf("first result = %+v; want one question and a request state", res)
	}
	return cs, res
}

// only returns the ID of a result's one input request.
func only(res *sdk.CallToolResult) string {
	for id := range res.InputRequests {
		return id
	}
	return ""
}

// TestElicitationAnswers: the answers a client comes back with are
// checked, and a question it leaves out is answered cancel.
func TestElicitationAnswers(t *testing.T) {
	cases := []struct {
		name    string
		answer  sdk.InputResponse
		want    string
		wantErr string
	}{
		{name: "accepted", answer: &sdk.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, want: `accept {"confirm":true}`},
		{name: "left out", want: "cancel "},
		{name: "off the schema", answer: &sdk.ElicitResult{Action: "accept", Content: map[string]any{"confirm": "yes"}}, wantErr: "elicitation answer"},
		{name: "unknown action", answer: &sdk.ElicitResult{Action: "maybe"}, wantErr: `unknown action "maybe"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cs, first := manual(t, asker(confirm))
			responses := sdk.InputResponseMap{}
			if tc.answer != nil {
				responses[only(first)] = tc.answer
			}
			res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "deploy", InputResponses: responses, RequestState: first.RequestState})
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case tc.wantErr != "":
				if !res.IsError || !strings.Contains(text(res), tc.wantErr) {
					t.Errorf("res = %q, isError %v; want an error with %q", text(res), res.IsError, tc.wantErr)
				}
			case res.IsError || text(res) != tc.want:
				t.Errorf("res = %q, isError %v; want %q", text(res), res.IsError, tc.want)
			}
		})
	}
}

// TestElicitationStateAnswersOnce: a request state is spent by the
// answer that uses it, so a replayed or invented one reaches no call.
func TestElicitationStateAnswersOnce(t *testing.T) {
	cs, first := manual(t, asker(confirm))
	answer := sdk.InputResponseMap{only(first): &sdk.ElicitResult{Action: "decline"}}
	for i, state := range []string{first.RequestState, first.RequestState, "invented"} {
		res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "deploy", InputResponses: answer, RequestState: state})
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if res.IsError {
				t.Fatalf("the first answer failed: %q", text(res))
			}
			continue
		}
		if !res.IsError || !strings.Contains(text(res), "no longer waiting") {
			t.Errorf("answer %d: res = %q; want it refused", i, text(res))
		}
	}
}

// TestElicitationAbandoned: a call whose client never comes back with
// the answer is stopped, so its tool does not hold what it owns forever.
func TestElicitationAbandoned(t *testing.T) {
	tool, stopped := askAndWait()
	manualWith(t, Options{AbandonAfter: 20 * time.Millisecond}, tool)
	wantStopped(t, stopped, "the abandoned call was never stopped")
}

// askAndWait is a tool that asks once and reports the error its
// question ended with.
func askAndWait() (agenttool.Tool, chan error) {
	stopped := make(chan error, 1)
	return agenttool.NewFunc("deploy", "asks and waits", nil, func(ctx context.Context, _ agenttool.Call) (agenttool.Result, error) {
		ask, _ := agenttool.ElicitorFrom(ctx)
		_, err := ask(ctx, confirm)
		stopped <- err
		return agenttool.Result{}, err
	}), stopped
}

func wantStopped(t *testing.T, stopped chan error, msg string) {
	t.Helper()
	select {
	case err := <-stopped:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("the tool saw %v; want its context cancelled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal(msg)
	}
}

// TestElicitationSessionClosed: a call parked on a question is stopped
// when its session ends, rather than holding its tool until the
// abandon timer, since no answer can come back on it (#54).
func TestElicitationSessionClosed(t *testing.T) {
	tool, stopped := askAndWait()
	cs, _ := manual(t, tool)
	if err := cs.Close(); err != nil {
		t.Fatal(err)
	}
	wantStopped(t, stopped, "the call ran on after its session closed")
}

// TestElicitationLastRequest: the Go SDK's client makes ten requests of
// a call, so a tenth question is answered with an error at once rather
// than sent to a client that would give up on the call, and the tool
// returns its result on that last request (#54).
func TestElicitationLastRequest(t *testing.T) {
	tool := agenttool.NewFunc("deploy", "asks one step at a time", nil, func(ctx context.Context, _ agenttool.Call) (agenttool.Result, error) {
		ask, _ := agenttool.ElicitorFrom(ctx)
		var out []string
		for range 10 {
			ans, err := ask(ctx, confirm)
			if err != nil {
				out = append(out, "error: "+err.Error())
				break
			}
			out = append(out, string(ans.Action))
		}
		return agenttool.Text(strings.Join(out, "; ")), nil
	})
	s := roundTrip(t, newServer(t, "deployer", tool), mcpclient.WithElicitation())
	host := &answering{content: `{"confirm":true}`}
	res, err := callVia(t, s, agenttool.ContextWithElicitor(context.Background(), host.elicit))
	if err != nil {
		t.Fatal(err)
	}
	if n := len(host.asked); n != 9 {
		t.Errorf("the client was asked %d questions; want 9", n)
	}
	want := strings.Repeat("accept; ", 9) + "error: mcpserver: elicitation: the client makes 10 requests of a call at most"
	if !strings.HasPrefix(res.Output.Text, want) {
		t.Errorf("output = %q; want it to start %q", res.Output.Text, want)
	}
}

// TestElicitationStatelessHTTP: over stateless HTTP each request is a
// session of its own that ends with it, so the end of the session that
// started a call must not stop the call.
func TestElicitationStatelessHTTP(t *testing.T) {
	server := newServer(t, "deployer", asker(confirm))
	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, &sdk.StreamableHTTPOptions{Stateless: true})
	hs := httptest.NewServer(handler)
	t.Cleanup(hs.Close)
	s, err := mcpclient.Connect(context.Background(), &sdk.StreamableClientTransport{Endpoint: hs.URL}, mcpclient.WithElicitation())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	host := &answering{content: `{"confirm":true}`}
	res, err := callVia(t, s, agenttool.ContextWithElicitor(context.Background(), host.elicit))
	if err != nil {
		t.Fatal(err)
	}
	if want := `accept {"confirm":true}`; res.Output.Text != want {
		t.Errorf("output = %q, want %q", res.Output.Text, want)
	}
}

// TestElicitationCancelled: a call the client cancels while the tool
// runs stops the tool, although it runs detached from the request.
func TestElicitationCancelled(t *testing.T) {
	started, stopped := make(chan struct{}), make(chan struct{})
	tool := agenttool.NewFunc("deploy", "runs until stopped", nil, func(ctx context.Context, _ agenttool.Call) (agenttool.Result, error) {
		close(started)
		<-ctx.Done()
		close(stopped)
		return agenttool.Result{}, ctx.Err()
	})
	s := roundTrip(t, newServer(t, "deployer", tool), mcpclient.WithElicitation())
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		cancel()
	}()
	_, _ = callVia(t, s, ctx)
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("the tool ran on after its call was cancelled")
	}
}

// rawClient connects an SDK client to server in memory.
func rawClient(t *testing.T, server *sdk.Server, opts *sdk.ClientOptions, sessionOpts *sdk.ClientSessionOptions) *sdk.ClientSession {
	t.Helper()
	ct, st := sdk.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := server.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "raw", Version: "1"}, opts).Connect(ctx, ct, sessionOpts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// text joins a result's text content.
func text(res *sdk.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// TestElicitationEsc: a user who presses Esc on a served question ends
// the harness's call, and mcpclient answers the question cancel in one
// more request, so the tool hears that nobody chose at once rather than
// holding what it took until the abandon timer: with the session left
// open, and over stateless HTTP, where there is no session to end
// (#57).
func TestElicitationEsc(t *testing.T) {
	for name, connect := range map[string]func(*testing.T, *sdk.Server) *mcpclient.Remote{
		"session open": func(t *testing.T, server *sdk.Server) *mcpclient.Remote {
			return roundTrip(t, server, mcpclient.WithElicitation())
		},
		"stateless HTTP": func(t *testing.T, server *sdk.Server) *mcpclient.Remote {
			handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, &sdk.StreamableHTTPOptions{Stateless: true})
			hs := httptest.NewServer(handler)
			t.Cleanup(hs.Close)
			s, err := mcpclient.Connect(context.Background(), &sdk.StreamableClientTransport{Endpoint: hs.URL}, mcpclient.WithElicitation())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			return s
		},
	} {
		t.Run(name, func(t *testing.T) {
			heard := make(chan agenttool.Action, 1)
			tool := agenttool.NewFunc("deploy", "asks before it deploys", nil, func(ctx context.Context, _ agenttool.Call) (agenttool.Result, error) {
				ask, _ := agenttool.ElicitorFrom(ctx)
				ans, err := ask(ctx, confirm)
				if err != nil {
					return agenttool.Result{}, err
				}
				heard <- ans.Action
				return agenttool.Text(string(ans.Action)), nil
			})
			s := connect(t, newServer(t, "deployer", tool))
			asked := make(chan struct{})
			ctx, cancel := context.WithCancel(agenttool.ContextWithElicitor(context.Background(), func(ctx context.Context, _ agenttool.Elicitation) (agenttool.Answer, error) {
				close(asked)
				<-ctx.Done()
				return agenttool.Answer{}, ctx.Err()
			}))
			go func() {
				<-asked
				cancel()
			}()
			if _, err := callVia(t, s, ctx); !errors.Is(err, context.Canceled) {
				t.Errorf("err = %v; want the call's cancellation", err)
			}
			select {
			case a := <-heard:
				if a != agenttool.ActionCancel {
					t.Errorf("the tool heard %q; want cancel", a)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("the tool was still waiting after Esc; DefaultAbandonAfter is %v", DefaultAbandonAfter)
			}
		})
	}
}
