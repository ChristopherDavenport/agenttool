package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agenttool"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// slowServer has a tool that never answers until its request is
// cancelled, reporting whether it was, and one that reports progress
// every tick for n ticks before answering.
func slowServer(cancelled chan<- struct{}) *sdk.Server {
	server := sdk.NewServer(&sdk.Implementation{Name: "slow", Version: "1"}, nil)
	server.AddTool(&sdk.Tool{Name: "hang", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(ctx context.Context, _ *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			<-ctx.Done()
			if cancelled != nil {
				cancelled <- struct{}{}
			}
			return nil, ctx.Err()
		})
	server.AddTool(&sdk.Tool{Name: "busy", InputSchema: json.RawMessage(`{"type":"object","properties":{"ticks":{"type":"integer"}}}`)},
		func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			var a struct{ Ticks int }
			_ = json.Unmarshal(req.Params.Arguments, &a)
			token := req.Params.GetProgressToken()
			for i := range a.Ticks {
				select {
				case <-time.After(20 * time.Millisecond):
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				if token != nil {
					_ = req.Session.NotifyProgress(ctx, &sdk.ProgressNotificationParams{ProgressToken: token, Progress: float64(i + 1)})
				}
			}
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "done"}}}, nil
		})
	return server
}

// TestCallTimeoutIdle: a call that goes quiet past its idle limit is
// stopped, its request cancelled, and returns an error naming the
// limit (#96).
func TestCallTimeoutIdle(t *testing.T) {
	cancelled := make(chan struct{}, 1)
	s := connect(t, slowServer(cancelled), WithCallTimeout(100*time.Millisecond, 0))
	start := time.Now()
	_, err := lookup(t, s, "hang").Execute(context.Background(), agenttool.Call{ID: "c", Args: json.RawMessage(`{}`)})
	var te *TimeoutError
	if !errors.As(err, &te) || !te.Idle || te.Limit != 100*time.Millisecond {
		t.Fatalf("err = %v, want an idle TimeoutError", err)
	}
	if !strings.Contains(err.Error(), "100ms") || !strings.Contains(err.Error(), "may or may not have taken effect") {
		t.Errorf("err = %q, want the limit named and the outcome said to be unknown", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("returned after %s", d)
	}
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Error("the server's tool was not cancelled")
	}
}

// TestCallTimeoutProgressResetsIdle: progress restarts the idle clock,
// whether or not the caller takes updates, and the maximum holds
// whatever progress says.
func TestCallTimeoutProgressResetsIdle(t *testing.T) {
	s := connect(t, slowServer(nil), WithCallTimeout(150*time.Millisecond, 0))
	// Ten ticks of 20 ms is 200 ms, past the idle limit, with progress
	// every tick.
	res, err := lookup(t, s, "busy").Execute(context.Background(), agenttool.Call{ID: "c", Args: json.RawMessage(`{"ticks":10}`)})
	if err != nil || res.Output.Text != "done" {
		t.Fatalf("busy = %+v, %v; want done, progress keeping it alive", res.Output, err)
	}

	s = connect(t, slowServer(nil), WithCallTimeout(150*time.Millisecond, 100*time.Millisecond))
	_, err = lookup(t, s, "busy").Execute(context.Background(), agenttool.Call{ID: "c", Args: json.RawMessage(`{"ticks":10}`)})
	var te *TimeoutError
	if !errors.As(err, &te) || te.Idle || te.Limit != 100*time.Millisecond {
		t.Fatalf("err = %v, want the maximum to hold through progress", err)
	}
}

// TestCallTimeoutPerTool: a tool's own limit wins over the server's,
// and zero lifts it.
func TestCallTimeoutPerTool(t *testing.T) {
	s := connect(t, slowServer(nil), WithPrefix("s"), WithCallTimeout(50*time.Millisecond, 0), WithCallTimeout(0, 0, "busy"))
	res, err := lookup(t, s, "s__busy").Execute(context.Background(), agenttool.Call{ID: "c", Args: json.RawMessage(`{"ticks":5}`)})
	if err != nil || res.Output.Text != "done" {
		t.Fatalf("busy = %+v, %v; want no limit on it", res.Output, err)
	}
	_, err = lookup(t, s, "s__hang").Execute(context.Background(), agenttool.Call{ID: "c", Args: json.RawMessage(`{}`)})
	var te *TimeoutError
	if !errors.As(err, &te) {
		t.Fatalf("hang: err = %v, want the server's limit", err)
	}
}

// TestCallTimeoutPausedForTheUser: the time a question waits on the
// user counts toward neither limit.
func TestCallTimeoutPausedForTheUser(t *testing.T) {
	for name, opts := range map[string][]Option{"current": {WithElicitation()}, "older": {WithElicitation(), older}} {
		t.Run(name, func(t *testing.T) {
			s := connect(t, eliciting(nil), append(opts, WithCallTimeout(150*time.Millisecond, 250*time.Millisecond))...)
			ctx := agenttool.ContextWithElicitor(context.Background(), func(context.Context, agenttool.Elicitation) (agenttool.Answer, error) {
				time.Sleep(500 * time.Millisecond)
				return agenttool.Answer{Action: agenttool.ActionAccept, Content: json.RawMessage(`{"confirm":true}`)}, nil
			})
			res, err := lookup(t, s, "ask").Execute(ctx, agenttool.Call{ID: "c", Args: json.RawMessage(`{}`)})
			if err != nil || res.Output.Text != `accept {"confirm":true}` {
				t.Fatalf("ask = %+v, %v; want the answer, the user's wait not counted", res.Output, err)
			}
		})
	}
}

func TestLimiterStaleTimer(t *testing.T) {
	stopped := make(chan error, 1)
	l := startLimiter("t", callTimeout{idle: 30 * time.Millisecond}, func(err error) { stopped <- err })
	for range 10 {
		time.Sleep(10 * time.Millisecond)
		l.progress()
	}
	select {
	case err := <-stopped:
		t.Fatalf("stopped by %v while progress kept coming", err)
	default:
	}
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("not stopped once progress stopped")
	}
	l.end()
}
