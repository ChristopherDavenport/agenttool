package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/ChristopherDavenport/agenttool"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// eliciting is a server whose "ask" tool asks the user to confirm a
// deletion mid-call and reports what came back, the way a server that
// guards a destructive step does. It asks the 2026-07-28 way, by
// returning the question as the call's result; the SDK's server turns
// that into a request of its own for an older client. When gate is set,
// every call waits on it before asking, so calls are held in flight
// together.
func eliciting(gate *sync.WaitGroup) *sdk.Server {
	s := sdk.NewServer(&sdk.Implementation{Name: "asker", Version: "1"}, nil)
	s.AddTool(&sdk.Tool{Name: "ask", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			answer, ok := req.Params.InputResponses["confirm"].(*sdk.ElicitResult)
			if !ok {
				if gate != nil {
					gate.Done()
					gate.Wait()
				}
				return &sdk.CallToolResult{InputRequests: sdk.InputRequestMap{"confirm": &sdk.ElicitParams{
					Message:         "delete the branch?",
					RequestedSchema: json.RawMessage(`{"type":"object","properties":{"confirm":{"type":"boolean"}},"required":["confirm"]}`),
				}}}, nil
			}
			content, _ := json.Marshal(answer.Content)
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: answer.Action + " " + string(content)}}}, nil
		})
	return s
}

// older is the client option that negotiates the protocol before
// 2026-07-28, under which the server sends its question on its own.
var older Option = func(o *options) { o.protocolVersion = "2025-11-25" }

func TestElicitationReachesTheCallsElicitor(t *testing.T) {
	s := connect(t, eliciting(nil), WithElicitation())
	var got agenttool.Elicitation
	var callID string
	ctx := agenttool.ContextWithElicitor(context.Background(), func(ctx context.Context, q agenttool.Elicitation) (agenttool.Answer, error) {
		got = q
		if c, ok := agenttool.CallFrom(ctx); ok {
			callID = c.ID
		}
		return agenttool.Answer{Action: agenttool.ActionAccept, Content: json.RawMessage(`{"confirm":true}`)}, nil
	})
	res, err := lookup(t, s, "ask").Execute(ctx, agenttool.Call{ID: "call_1", Args: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Output.Text != `accept {"confirm":true}` {
		t.Errorf("the server saw %q; want the user's answer", res.Output.Text)
	}
	if got.Message != "delete the branch?" || !strings.Contains(string(got.Schema), `"confirm"`) || got.URL != "" {
		t.Errorf("question = %+v", got)
	}
	if callID != "call_1" {
		t.Errorf("the elicitor saw call %q; want the call that asked, so the record can file it there", callID)
	}
}

func TestElicitationNobodyToAsk(t *testing.T) {
	// A call whose context carries no elicitor: the server hears cancel,
	// which says nobody chose, rather than decline, which says someone did.
	s := connect(t, eliciting(nil), WithElicitation())
	res, err := lookup(t, s, "ask").Execute(context.Background(), agenttool.Call{ID: "c", Args: json.RawMessage(`{}`)})
	if err != nil || !strings.HasPrefix(res.Output.Text, "cancel") {
		t.Errorf("res = %q, err = %v; want cancel", res.Output.Text, err)
	}
}

func TestElicitationOffByDefault(t *testing.T) {
	// Without the option the client offers no elicitation, so a server
	// that asks anyway is refused by the SDK, exactly as before.
	s := connect(t, eliciting(nil))
	ctx := agenttool.ContextWithElicitor(context.Background(), func(context.Context, agenttool.Elicitation) (agenttool.Answer, error) {
		t.Error("the elicitor was called without WithElicitation")
		return agenttool.Answer{}, nil
	})
	_, err := lookup(t, s, "ask").Execute(ctx, agenttool.Call{ID: "c", Args: json.RawMessage(`{}`)})
	if err == nil {
		t.Error("the server's elicitation should fail without the capability")
	}
	if caps := s.Session().InitializeResult(); caps == nil {
		t.Fatal("no initialize result")
	}
}

// askTwice runs two calls of ask at once, held in flight together, each
// under its own call ID, and returns what the server saw for each.
func askTwice(t *testing.T, s *Remote, gate *sync.WaitGroup, ctx context.Context) []string {
	t.Helper()
	ask := lookup(t, s, "ask")
	var wg sync.WaitGroup
	outs := make([]string, 2)
	for i := range outs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := ask.Execute(ctx, agenttool.Call{ID: "call_" + string(rune('a'+i)), Args: json.RawMessage(`{}`)})
			if err != nil {
				t.Error(err)
			}
			outs[i] = res.Output.Text
		}()
	}
	wg.Wait()
	return outs
}

// From 2026-07-28 the question comes back with its call, so two calls in
// flight are each asked, and each under its own ID.
func TestElicitationTwoCallsEachAskedUnderTheirOwnCall(t *testing.T) {
	var gate sync.WaitGroup
	gate.Add(2)
	s := connect(t, eliciting(&gate), WithElicitation())
	var mu sync.Mutex
	asked := map[string]bool{}
	ctx := agenttool.ContextWithElicitor(context.Background(), func(ctx context.Context, q agenttool.Elicitation) (agenttool.Answer, error) {
		c, _ := agenttool.CallFrom(ctx)
		mu.Lock()
		asked[c.ID] = true
		mu.Unlock()
		return agenttool.Answer{Action: agenttool.ActionDecline}, nil
	})
	for i, out := range askTwice(t, s, &gate, ctx) {
		if !strings.HasPrefix(out, "decline") {
			t.Errorf("call %d: server saw %q; want the user's decline", i, out)
		}
	}
	if !asked["call_a"] || !asked["call_b"] {
		t.Errorf("asked under %v; want each call under its own ID", asked)
	}
}

// Before 2026-07-28 the question names no call. With one call in flight
// it is that call's; with two, nobody is asked.
func TestElicitationOlderServer(t *testing.T) {
	t.Run("one call in flight is asked", func(t *testing.T) {
		s := connect(t, eliciting(nil), WithElicitation(), older)
		var callID string
		ctx := agenttool.ContextWithElicitor(context.Background(), func(ctx context.Context, q agenttool.Elicitation) (agenttool.Answer, error) {
			c, _ := agenttool.CallFrom(ctx)
			callID = c.ID
			return agenttool.Answer{Action: agenttool.ActionAccept, Content: json.RawMessage(`{"confirm":true}`)}, nil
		})
		res, err := lookup(t, s, "ask").Execute(ctx, agenttool.Call{ID: "call_1", Args: json.RawMessage(`{}`)})
		if err != nil || res.Output.Text != `accept {"confirm":true}` || callID != "call_1" {
			t.Errorf("res = %q, err = %v, asked under %q", res.Output.Text, err, callID)
		}
	})
	t.Run("two calls in flight ask nobody", func(t *testing.T) {
		var gate sync.WaitGroup
		gate.Add(2)
		s := connect(t, eliciting(&gate), WithElicitation(), older)
		ctx := agenttool.ContextWithElicitor(context.Background(), func(context.Context, agenttool.Elicitation) (agenttool.Answer, error) {
			t.Error("a question that cannot be told apart from its sibling was put to the user")
			return agenttool.Answer{Action: agenttool.ActionAccept}, nil
		})
		for i, out := range askTwice(t, s, &gate, ctx) {
			if !strings.HasPrefix(out, "cancel") {
				t.Errorf("call %d: server saw %q; want cancel", i, out)
			}
		}
	})
}

func TestElicitationAnswerErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		ans agenttool.Answer
		err error
	}{
		"harness failed to ask": {err: errors.New("no terminal")},
		"unknown action":        {ans: agenttool.Answer{Action: "maybe"}},
		"content not an object": {ans: agenttool.Answer{Action: agenttool.ActionAccept, Content: json.RawMessage(`[1]`)}},
	} {
		t.Run(name, func(t *testing.T) {
			s := connect(t, eliciting(nil), WithElicitation())
			ctx := agenttool.ContextWithElicitor(context.Background(), func(context.Context, agenttool.Elicitation) (agenttool.Answer, error) {
				return tc.ans, tc.err
			})
			_, err := lookup(t, s, "ask").Execute(ctx, agenttool.Call{ID: "c", Args: json.RawMessage(`{}`)})
			if err == nil {
				t.Error("the server should see the elicitation fail, not an answer")
			}
		})
	}
}

func TestElicitationHandlerFromClientOptionsWins(t *testing.T) {
	s := connect(t, eliciting(nil), WithElicitation(), WithClientOptions(sdk.ClientOptions{
		ElicitationHandler: func(context.Context, *sdk.ElicitRequest) (*sdk.ElicitResult, error) {
			return &sdk.ElicitResult{Action: "decline"}, nil
		},
	}))
	ctx := agenttool.ContextWithElicitor(context.Background(), func(context.Context, agenttool.Elicitation) (agenttool.Answer, error) {
		t.Error("the product's own handler should answer")
		return agenttool.Answer{}, nil
	})
	res, err := lookup(t, s, "ask").Execute(ctx, agenttool.Call{ID: "c", Args: json.RawMessage(`{}`)})
	if err != nil || !strings.HasPrefix(res.Output.Text, "decline") {
		t.Errorf("res = %q, err = %v; want the product handler's decline", res.Output.Text, err)
	}
}
