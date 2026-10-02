package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agenttool"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// addBusy gives s a "build" tool that runs for d unless its context ends
// first, closes started when a call reaches it, and reports on out
// whether it was cancelled or ran to the end. It stands for a served
// test suite the harness closes the server under.
func addBusy(s *sdk.Server, d time.Duration, started chan<- struct{}, out chan<- string) {
	var once sync.Once
	s.AddTool(&sdk.Tool{Name: "build", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(ctx context.Context, _ *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			once.Do(func() { close(started) })
			select {
			case <-ctx.Done():
				out <- "cancelled"
				return nil, ctx.Err()
			case <-time.After(d):
				out <- "ran to the end"
				return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "built"}}}, nil
			}
		})
}

// elicitation is the option set under test: Close's guarantees do not
// depend on whether the remote offers elicitation.
var elicitation = map[string][]Option{
	"without WithElicitation": nil,
	"with WithElicitation":    {WithElicitation()},
}

// A remote closed while a call is in flight ends the call at once with
// ErrClosed and cancels its request, whether or not the remote was
// dialed with WithElicitation (#67). Without it, Close used to wait for
// the call to finish, since the SDK's session close waits for its
// requests in flight, and the call then succeeded against a server the
// harness had closed. Over stateless HTTP the served tool is not stopped
// by the cancelled request, as WithElicitation's doc says, so only the
// in-memory case checks that the tool was.
func TestCloseEndsACallInFlight(t *testing.T) {
	for tname, dial := range transports {
		for oname, opts := range elicitation {
			t.Run(tname+"/"+oname, func(t *testing.T) {
				started, out := make(chan struct{}), make(chan string, 1)
				s := sdk.NewServer(&sdk.Implementation{Name: "busy", Version: "1"}, nil)
				addBusy(s, 3*time.Second, started, out)
				r := dial(t, s, opts...)
				build := lookup(t, r, "build")
				done := make(chan error, 1)
				go func() {
					_, err := build.Execute(context.Background(), agenttool.Call{ID: "c", Args: json.RawMessage(`{}`)})
					done <- err
				}()
				<-started
				at := time.Now()
				if err := r.Close(); err != nil {
					t.Errorf("close: %v", err)
				}
				if d := time.Since(at); d > time.Second {
					t.Errorf("Close took %v; want it at once, not after the call", d)
				}
				select {
				case err := <-done:
					if !errors.Is(err, ErrClosed) {
						t.Errorf("the call ended with %v; want ErrClosed", err)
					}
				case <-time.After(time.Second):
					t.Fatal("the call was still running a second after Close")
				}
				if tname == "in memory" {
					select {
					case got := <-out:
						if got != "cancelled" {
							t.Errorf("the served tool %s; want it cancelled with the request", got)
						}
					case <-time.After(time.Second):
						t.Error("the served tool was still running a second after Close")
					}
				}
				if _, err := build.Execute(context.Background(), agenttool.Call{ID: "d", Args: json.RawMessage(`{}`)}); !errors.Is(err, ErrClosed) {
					t.Errorf("a call after Close: err = %v; want ErrClosed", err)
				}
			})
		}
	}
}

// Close with several calls in flight and one call's question with the
// user ends every one of them with ErrClosed, stops the question with
// the same cause, and still tells the server nobody chose. It runs
// under -race with the calls starting, asking and being closed at once.
func TestCloseEndsEveryCallAndTheQuestion(t *testing.T) {
	const busy = 4
	for name, dial := range transports {
		t.Run(name, func(t *testing.T) {
			heard := make(chan string, 2)
			s := resumed(heard, false)
			started, out := make(chan struct{}), make(chan string, busy)
			var reached sync.WaitGroup
			reached.Add(busy)
			var once sync.Once
			s.AddTool(&sdk.Tool{Name: "build", InputSchema: json.RawMessage(`{"type":"object"}`)},
				func(ctx context.Context, _ *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
					once.Do(func() { close(started) })
					reached.Done()
					select {
					case <-ctx.Done():
						out <- "cancelled"
						return nil, ctx.Err()
					case <-time.After(3 * time.Second):
						out <- "ran to the end"
						return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "built"}}}, nil
					}
				})
			r := dial(t, s, WithElicitation())

			asked, cause := make(chan struct{}), make(chan error, 1)
			ctx := agenttool.ContextWithElicitor(context.Background(), func(ctx context.Context, _ agenttool.Elicitation) (agenttool.Answer, error) {
				close(asked)
				<-ctx.Done()
				cause <- context.Cause(ctx)
				return agenttool.Answer{}, ctx.Err()
			})
			build, rollout := lookup(t, r, "build"), lookup(t, r, "rollout")
			errs := make(chan error, busy+1)
			for i := range busy {
				go func() {
					_, err := build.Execute(context.Background(), agenttool.Call{ID: fmt.Sprint("b", i), Args: json.RawMessage(`{}`)})
					errs <- err
				}()
			}
			go func() {
				_, err := rollout.Execute(ctx, agenttool.Call{ID: "q", Args: json.RawMessage(`{}`)})
				errs <- err
			}()
			<-started
			reached.Wait()
			<-asked
			at := time.Now()
			if err := r.Close(); err != nil {
				t.Errorf("close: %v", err)
			}
			if d := time.Since(at); d > time.Second {
				t.Errorf("Close took %v; want it within the cancel's grace", d)
			}
			for range busy + 1 {
				select {
				case err := <-errs:
					if !errors.Is(err, ErrClosed) {
						t.Errorf("a call ended with %v; want ErrClosed", err)
					}
				case <-time.After(time.Second):
					t.Fatal("a call was still running a second after Close")
				}
			}
			if got := <-cause; !errors.Is(got, ErrClosed) {
				t.Errorf("the elicitor's context ended with %v; want ErrClosed", got)
			}
			hear(t, heard, "cancel")
			if name == "in memory" {
				for range busy {
					select {
					case got := <-out:
						if got != "cancelled" {
							t.Errorf("a served tool %s; want it cancelled with the request", got)
						}
					case <-time.After(time.Second):
						t.Fatal("a served tool was still running a second after Close")
					}
				}
			}
		})
	}
}
