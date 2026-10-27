package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agenttool"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// restarter stands for a server process the remote can start again: each
// session runs on a server from serve, numbered from 0 for the first,
// over a pipe the test can cut, as the process crashing or its
// connection dropping would.
type restarter struct {
	t     *testing.T
	serve func(n int) *sdk.Server

	mu    sync.Mutex
	pipes []net.Conn // the server's end of each session's pipe

	dials atomic.Int32  // calls of dial
	fail  atomic.Int32  // dials still to fail
	gate  chan struct{} // when set, dial waits for it to close

	// closeErr, when set, is what closing the client's end of each
	// session returns, as the transport of a server whose host is gone
	// reports when its process is closed.
	closeErr error
}

// failingClose is a transport whose connection closes with err.
type failingClose struct {
	sdk.Transport
	err error
}

func (f failingClose) Connect(ctx context.Context) (sdk.Connection, error) {
	c, err := f.Transport.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return failingConn{c, f.err}, nil
}

type failingConn struct {
	sdk.Connection
	err error
}

func (c failingConn) Close() error {
	_ = c.Connection.Close()
	return c.err
}

// open starts the next server and returns the client's transport to it.
func (r *restarter) open(ctx context.Context) (sdk.Transport, error) {
	c, sv := net.Pipe()
	r.mu.Lock()
	n := len(r.pipes)
	r.pipes = append(r.pipes, sv)
	r.mu.Unlock()
	if _, err := r.serve(n).Connect(ctx, &sdk.IOTransport{Reader: sv, Writer: sv}, nil); err != nil {
		return nil, err
	}
	var tr sdk.Transport = &sdk.IOTransport{Reader: c, Writer: c}
	if r.closeErr != nil {
		tr = failingClose{tr, r.closeErr}
	}
	return tr, nil
}

// dial is the remote's WithReconnect.
func (r *restarter) dial(ctx context.Context) (sdk.Transport, error) {
	r.dials.Add(1)
	if r.gate != nil {
		select {
		case <-r.gate:
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		}
	}
	if r.fail.Load() > 0 {
		r.fail.Add(-1)
		return nil, errors.New("the executor is unreachable")
	}
	return r.open(ctx)
}

// kill cuts the latest session's pipe.
func (r *restarter) kill() {
	r.mu.Lock()
	defer r.mu.Unlock()
	_ = r.pipes[len(r.pipes)-1].Close()
}

// connect opens the first session and returns the remote, with
// WithReconnect on unless off is set.
func (r *restarter) connect(off bool, opts ...Option) *Remote {
	r.t.Helper()
	ctx := context.Background()
	tr, err := r.open(ctx)
	if err != nil {
		r.t.Fatal(err)
	}
	if !off {
		opts = append(opts, WithReconnect(r.dial))
	}
	s, err := Connect(ctx, tr, opts...)
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { _ = s.Close() })
	return s
}

// drop cuts the session and waits for the remote to see it end.
func (r *restarter) drop(s *Remote) {
	r.t.Helper()
	l := s.current()
	r.kill()
	select {
	case <-l.ended:
	case <-time.After(5 * time.Second):
		r.t.Fatal("the remote did not see its session end")
	}
}

// hooked collects what WithReconnected reports.
type hooked struct {
	mu    sync.Mutex
	infos []ReconnectInfo
}

func (h *hooked) option() Option {
	return WithReconnected(func(i ReconnectInfo) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.infos = append(h.infos, i)
	})
}

func (h *hooked) got() []ReconnectInfo {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.infos)
}

func execute(tl agenttool.Tool, args string) (agenttool.Result, error) {
	return tl.Execute(context.Background(), agenttool.Call{ID: "c", Args: json.RawMessage(args)})
}

// After the session ends, the next request restarts the server first
// and then succeeds, whichever request it is, and the hook hears of it
// once.
func TestReconnectRestartsOnTheNextRequest(t *testing.T) {
	for _, tc := range []struct {
		name string
		do   func(*Remote) error
	}{
		{name: "a call", do: func(s *Remote) error {
			res, err := execute(lookup(t, s, "upper"), `{"text":"hi"}`)
			if err == nil && res.Output.Text != "HI" {
				t.Errorf("result %q, want HI", res.Output.Text)
			}
			return err
		}},
		{name: "a refresh", do: func(s *Remote) error { return s.Refresh(context.Background()) }},
		{name: "a facts read", do: func(s *Remote) error {
			_, err := s.Facts(context.Background(), FactsCall{Name: "upper"})
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &restarter{t: t, serve: func(int) *sdk.Server { return newServer(t) }}
			var h hooked
			s := r.connect(false, h.option())
			before := names(s.Tools())
			r.drop(s)
			if err := tc.do(s); err != nil {
				t.Fatalf("after the restart: %v", err)
			}
			if n := r.dials.Load(); n != 1 {
				t.Errorf("dialed %d times, want 1", n)
			}
			infos := h.got()
			if len(infos) != 1 {
				t.Fatalf("the hook fired %d times, want once", len(infos))
			}
			i := infos[0]
			if i.Err != nil || i.Changed || i.Server.Name != "fixture" || !slices.Equal(i.After, i.Before) || len(i.After) != len(before) {
				t.Errorf("hook got %+v", i)
			}
			if s.current().isEnded() || s.current().gen != 1 {
				t.Error("the remote is not on the restarted session")
			}
			if got := names(s.Tools()); !slices.Equal(got, before) {
				t.Errorf("tools %v, want %v", got, before)
			}
		})
	}
}

// A call in flight when its session ends returns an error saying it may
// have run, and the server sees it once: it is not sent again on the
// restarted session.
func TestReconnectDoesNotResendACutCall(t *testing.T) {
	var ran atomic.Int32
	started := make(chan struct{}, 2)
	r := &restarter{t: t}
	r.serve = func(int) *sdk.Server {
		s := newServer(t)
		s.AddTool(&sdk.Tool{Name: "deploy", InputSchema: json.RawMessage(`{"type":"object"}`)},
			func(ctx context.Context, _ *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
				ran.Add(1)
				started <- struct{}{}
				<-ctx.Done()
				return nil, ctx.Err()
			})
		return s
	}
	s := r.connect(false)
	deploy := lookup(t, s, "deploy")
	done := make(chan error, 1)
	go func() {
		_, err := execute(deploy, `{}`)
		done <- err
	}()
	<-started
	r.kill()
	var err error
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the cut call did not return")
	}
	if !errors.Is(err, ErrSessionEnded) || !strings.Contains(err.Error(), "may or may not have run") {
		t.Errorf("the cut call returned %v; want ErrSessionEnded saying it may have run", err)
	}
	// The next call restarts the server, and the cut one stays unsent.
	if _, err := execute(lookup(t, s, "upper"), `{"text":"x"}`); err != nil {
		t.Fatal(err)
	}
	if n := r.dials.Load(); n != 1 {
		t.Errorf("dialed %d times, want 1", n)
	}
	select {
	case <-started:
		t.Error("the cut call was sent again")
	case <-time.After(50 * time.Millisecond):
	}
	if n := ran.Load(); n != 1 {
		t.Errorf("the server ran the call %d times, want once", n)
	}
}

// A facts read the end of its session cuts is made once more on the
// restarted session, since it has no side effects.
func TestReconnectRetriesACutFactsRead(t *testing.T) {
	args := json.RawMessage(`{"command":"ls"}`)
	for _, tc := range []struct {
		name string
		ask  func(*testing.T, *Remote) error
	}{
		{name: "Facts", ask: func(t *testing.T, s *Remote) error {
			got, err := s.Facts(context.Background(), FactsCall{Name: "sh", Args: args})
			if err == nil && (!got[0].Claimed || len(got[0].Facts.Calls) != 1) {
				t.Errorf("answer %+v", got[0])
			}
			return err
		}},
		{name: "the tool's claim", ask: func(t *testing.T, s *Remote) error {
			f, claims, err := agenttool.FactsOf(context.Background(), lookup(t, s, "sh"), args)
			if err == nil && (!claims || len(f.Calls) != 1) {
				t.Errorf("facts %+v, claims %v", f, claims)
			}
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var asked atomic.Int32
			r := &restarter{t: t}
			r.serve = func(n int) *sdk.Server {
				s := factsServer(t, `{"facts":{"calls":[{"tool":"read","args":{"path":"a"}}]},"replay":"safe"}`, false)
				s.AddReceivingMiddleware(func(next sdk.MethodHandler) sdk.MethodHandler {
					return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
						if method == FactsMethod {
							asked.Add(1)
							if n == 0 {
								// The first server dies with the question.
								r.kill()
								<-ctx.Done()
								return nil, ctx.Err()
							}
						}
						return next(ctx, method, req)
					}
				})
				return s
			}
			s := r.connect(false, WithClaims())
			if err := tc.ask(t, s); err != nil {
				t.Fatalf("the read was not made again: %v", err)
			}
			if n := asked.Load(); n != 2 {
				t.Errorf("asked %d times, want twice: cut once, then again", n)
			}
			if n := r.dials.Load(); n != 1 {
				t.Errorf("dialed %d times, want 1", n)
			}
		})
	}
}

// changing serves, before the restart, upper, keep, alter and claimy,
// and after it keep unchanged, alter with another schema, claimy no
// longer claiming, and fresh; upper is gone. Each server counts the
// calls it ran on ran.
func changing(t *testing.T, ran *sync.Map) func(int) *sdk.Server {
	tool := func(s *sdk.Server, name, schema string, meta sdk.Meta) {
		s.AddTool(&sdk.Tool{Name: name, InputSchema: json.RawMessage(schema), Meta: meta},
			func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
				n, _ := ran.LoadOrStore(name, new(atomic.Int32))
				n.(*atomic.Int32).Add(1)
				return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: name}}}, nil
			})
	}
	claiming := sdk.Meta{ToolMetaKey: map[string]any{"facts": true}}
	return func(n int) *sdk.Server {
		s := sdk.NewServer(&sdk.Implementation{Name: "changing", Version: "1"}, &sdk.ServerOptions{
			Capabilities: &sdk.ServerCapabilities{Experimental: map[string]any{FactsCapability: map[string]any{}}},
		})
		err := sdk.AddReceivingCustomMethod(s, FactsMethod, func(_ context.Context, _ *sdk.ServerSession, p *wireParams) (*wireResult, error) {
			res := &wireResult{}
			for range p.Calls {
				res.Results = append(res.Results, json.RawMessage(`{"facts":{"calls":[]}}`))
			}
			return res, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		tool(s, "keep", `{"type":"object"}`, nil)
		if n == 0 {
			tool(s, "upper", `{"type":"object"}`, nil)
			tool(s, "alter", `{"type":"object"}`, nil)
			tool(s, "claimy", `{"type":"object"}`, claiming)
		} else {
			tool(s, "alter", `{"type":"object","properties":{"force":{"type":"boolean"}}}`, nil)
			tool(s, "claimy", `{"type":"object"}`, nil)
			tool(s, "fresh", `{"type":"object"}`, nil)
		}
		return s
	}
}

// A restarted server whose tools changed is taken as a refresh: Tools
// shows its list. A call decided on a tool before the restart is sent
// only when the server still lists that tool as it was.
func TestReconnectWithChangedTools(t *testing.T) {
	var ran sync.Map
	r := &restarter{t: t, serve: changing(t, &ran)}
	var h hooked
	s := r.connect(false, WithClaims(), h.option())
	old := map[string]agenttool.Tool{}
	for _, tl := range s.Tools() {
		old[tl.Name()] = tl
	}
	r.drop(s)
	// keep triggers the restart, and is sent: it is listed as it was.
	if res, err := execute(old["keep"], `{}`); err != nil || res.Output.Text != "keep" {
		t.Fatalf("keep: %+v, %v", res, err)
	}
	if got, want := slices.Sorted(slices.Values(names(s.Tools()))), []string{"alter", "claimy", "fresh", "keep"}; !slices.Equal(got, want) {
		t.Errorf("tools after the restart %v, want %v", got, want)
	}
	for _, name := range []string{"upper", "alter", "claimy"} {
		t.Run(name, func(t *testing.T) {
			_, err := execute(old[name], `{}`)
			if !errors.Is(err, ErrToolChanged) {
				t.Errorf("err = %v; want ErrToolChanged", err)
			}
			if _, ok := ran.Load(name); ok {
				t.Errorf("%s was sent", name)
			}
		})
	}
	// The old tool's claim is not asked of a tool that changed either.
	if _, _, err := agenttool.FactsOf(context.Background(), old["claimy"], json.RawMessage(`{}`)); !errors.Is(err, ErrToolChanged) {
		t.Errorf("the old claim answered %v; want ErrToolChanged", err)
	}
	// The tool as it is now runs.
	if _, err := execute(lookup(t, s, "alter"), `{}`); err != nil {
		t.Errorf("the restarted alter: %v", err)
	}
	infos := h.got()
	if len(infos) != 1 || !infos[0].Changed || !slices.Equal(infos[0].Before, []string{"alter", "claimy", "keep", "upper"}) || !slices.Equal(infos[0].After, []string{"alter", "claimy", "fresh", "keep"}) {
		t.Errorf("hook got %+v", infos)
	}
}

// Requests that find the session ended together share one dial, and a
// dial that fails fails them; the next request dials again.
func TestReconnectSharesOneDial(t *testing.T) {
	r := &restarter{t: t, serve: func(int) *sdk.Server { return newServer(t) }, gate: make(chan struct{})}
	var h hooked
	s := r.connect(false, h.option())
	upper := lookup(t, s, "upper")
	r.drop(s)
	r.fail.Store(1)

	const n = 5
	errs := make(chan error, n)
	for range n {
		go func() {
			_, err := execute(upper, `{"text":"x"}`)
			errs <- err
		}()
	}
	waitFor(t, func() []struct{} { return make([]struct{}, r.dials.Load()) }, 1)
	time.Sleep(20 * time.Millisecond)
	close(r.gate)
	for range n {
		if err := <-errs; !errors.Is(err, ErrSessionEnded) || !strings.Contains(err.Error(), "the executor is unreachable") {
			t.Errorf("a call during the failed dial: %v; want ErrSessionEnded with the dial's error", err)
		}
	}
	if got := r.dials.Load(); got != 1 {
		t.Errorf("dialed %d times, want 1 shared", got)
	}
	if infos := h.got(); len(infos) != 1 || infos[0].Err == nil {
		t.Errorf("hook got %+v; want the one failed attempt", infos)
	}
	if res, err := execute(upper, `{"text":"x"}`); err != nil || res.Output.Text != "X" {
		t.Fatalf("the next call: %+v, %v", res, err)
	}
	if got := r.dials.Load(); got != 2 {
		t.Errorf("dialed %d times, want 2: the next call dials again", got)
	}
	if infos := h.got(); len(infos) != 2 || infos[1].Err != nil {
		t.Errorf("hook got %+v; want a failure then a success", infos)
	}
}

// Requests waiting on the attempt are bounded by it, not by their own
// contexts alone: an attempt that does not finish in time fails.
func TestReconnectTimesOut(t *testing.T) {
	r := &restarter{t: t, serve: func(int) *sdk.Server { return newServer(t) }, gate: make(chan struct{})}
	s := r.connect(false)
	s.reconnectTimeout = 50 * time.Millisecond
	r.drop(s)
	_, err := execute(lookup(t, s, "upper"), `{"text":"x"}`)
	if !errors.Is(err, ErrSessionEnded) || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("err = %v; want ErrSessionEnded, timed out", err)
	}
}

// Close during or after a drop dials nothing more, and ends a request
// waiting on an attempt at once.
func TestReconnectClose(t *testing.T) {
	t.Run("after the drop", func(t *testing.T) {
		r := &restarter{t: t, serve: func(int) *sdk.Server { return newServer(t) }}
		var h hooked
		s := r.connect(false, h.option())
		upper := lookup(t, s, "upper")
		r.drop(s)
		_ = s.Close()
		if _, err := execute(upper, `{"text":"x"}`); !errors.Is(err, ErrClosed) {
			t.Errorf("a call after Close: %v; want ErrClosed", err)
		}
		if err := s.Refresh(context.Background()); !errors.Is(err, ErrClosed) {
			t.Errorf("a refresh after Close: %v; want ErrClosed", err)
		}
		if n := r.dials.Load(); n != 0 {
			t.Errorf("dialed %d times after Close", n)
		}
		if len(h.got()) != 0 {
			t.Error("the hook fired")
		}
	})
	t.Run("during the attempt", func(t *testing.T) {
		r := &restarter{t: t, serve: func(int) *sdk.Server { return newServer(t) }, gate: make(chan struct{})}
		var h hooked
		s := r.connect(false, h.option())
		upper := lookup(t, s, "upper")
		r.drop(s)
		done := make(chan error, 1)
		go func() {
			_, err := execute(upper, `{"text":"x"}`)
			done <- err
		}()
		waitFor(t, func() []struct{} { return make([]struct{}, r.dials.Load()) }, 1)
		_ = s.Close()
		select {
		case err := <-done:
			if !errors.Is(err, ErrClosed) {
				t.Errorf("the waiting call: %v; want ErrClosed", err)
			}
		case <-time.After(time.Second):
			t.Fatal("the waiting call did not end with Close")
		}
		if _, err := execute(upper, `{"text":"x"}`); !errors.Is(err, ErrClosed) {
			t.Errorf("a call after Close: %v; want ErrClosed", err)
		}
		if n := r.dials.Load(); n != 1 {
			t.Errorf("dialed %d times, want only the one Close stopped", n)
		}
		time.Sleep(20 * time.Millisecond)
		if len(h.got()) != 0 {
			t.Error("the hook fired for an attempt Close stopped")
		}
	})
}

// Close returns nil for a session that ended and could not be
// restarted, every time, though closing its transport fails: the
// failed restart was reported, and the restart's release of the old
// session and Close's close of it are one close (#88). Close of a live
// session still returns its error.
func TestReconnectCloseAfterAFailedRestart(t *testing.T) {
	gone := errors.New("dial tcp: connection refused")
	t.Run("ended", func(t *testing.T) {
		for i := range 20 {
			r := &restarter{t: t, serve: func(int) *sdk.Server { return newServer(t) }, closeErr: gone}
			var h hooked
			s := r.connect(false, h.option())
			r.drop(s)
			r.fail.Store(1)
			if _, err := execute(lookup(t, s, "upper"), `{"text":"x"}`); !errors.Is(err, ErrSessionEnded) {
				t.Fatalf("run %d: the call after the drop: %v; want ErrSessionEnded", i, err)
			}
			if infos := h.got(); len(infos) != 1 || infos[0].Err == nil {
				t.Fatalf("run %d: hook got %+v; want the one failed attempt", i, infos)
			}
			if err := s.Close(); err != nil {
				t.Fatalf("run %d: Close after the failed restart: %v; want nil", i, err)
			}
		}
	})
	t.Run("ended, nothing restarted", func(t *testing.T) {
		r := &restarter{t: t, serve: func(int) *sdk.Server { return newServer(t) }, closeErr: gone}
		s := r.connect(false)
		r.drop(s)
		if err := s.Close(); err != nil {
			t.Errorf("Close after the drop: %v; want nil", err)
		}
		if n := r.dials.Load(); n != 0 {
			t.Errorf("dialed %d times", n)
		}
	})
	t.Run("live", func(t *testing.T) {
		r := &restarter{t: t, serve: func(int) *sdk.Server { return newServer(t) }, closeErr: gone}
		s := r.connect(false)
		if err := s.Close(); !errors.Is(err, gone) {
			t.Errorf("Close of a live session: %v; want its close error %v", err, gone)
		}
	})
}

// Without WithReconnect a session that ends stays ended: later calls
// fail as they always did, and the snapshot stays.
func TestWithoutReconnectTheSessionStaysEnded(t *testing.T) {
	r := &restarter{t: t, serve: func(int) *sdk.Server { return newServer(t) }}
	s := r.connect(true)
	before := names(s.Tools())
	r.drop(s)
	_, err := execute(lookup(t, s, "upper"), `{"text":"x"}`)
	if err == nil || errors.Is(err, ErrSessionEnded) || !errors.Is(err, sdk.ErrConnectionClosed) {
		t.Errorf("err = %v; want the SDK's connection closed, as before", err)
	}
	if err := s.Refresh(context.Background()); err == nil {
		t.Error("a refresh on the ended session succeeded")
	}
	if got := names(s.Tools()); !slices.Equal(got, before) {
		t.Errorf("tools %v, want the old snapshot %v", got, before)
	}
	if n := r.dials.Load(); n != 0 {
		t.Errorf("dialed %d times", n)
	}
}

// The restarted session is the same client's: elicitation and what the
// host registered with WithClientSetup work on it.
func TestReconnectKeepsTheClientsHandlers(t *testing.T) {
	r := &restarter{t: t, serve: func(int) *sdk.Server {
		s := eliciting(nil)
		if err := sdk.AddReceivingCustomMethod(s, startMethod,
			func(_ context.Context, _ *sdk.ServerSession, p *startParams) (*startResult, error) {
				return &startResult{ID: "proc-" + p.Command}, nil
			}); err != nil {
			t.Fatal(err)
		}
		return s
	}}
	s := r.connect(false, WithElicitation(), WithClientSetup(func(c *sdk.Client) error {
		return sdk.AddSendingCustomMethod[*startParams, *startResult](c, startMethod)
	}))
	first := s.Session()
	r.drop(s)
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.Session() == first {
		t.Fatal("Session is the ended one")
	}
	ctx := agenttool.ContextWithElicitor(context.Background(), func(context.Context, agenttool.Elicitation) (agenttool.Answer, error) {
		return agenttool.Answer{Action: agenttool.ActionAccept, Content: json.RawMessage(`{"confirm":true}`)}, nil
	})
	res, err := lookup(t, s, "ask").Execute(ctx, agenttool.Call{ID: "c", Args: json.RawMessage(`{}`)})
	if err != nil || res.Output.Text != `accept {"confirm":true}` {
		t.Errorf("elicitation on the restarted session: %q, %v", res.Output.Text, err)
	}
	got, err := sdk.CallCustomMethod[*startParams, *startResult](context.Background(), s.Session(), startMethod, &startParams{Command: "gopls"})
	if err != nil || got.ID != "proc-gopls" {
		t.Errorf("custom method on the restarted session: %+v, %v", got, err)
	}
}

// holding is a remote whose session can be left refusing requests
// because its connection closed while it has not yet reported its end:
// its server's hold tool notifies progress before it answers, and the
// client's progress handler waits for release, and a session does not
// end while a handler of the client's runs.
type holding struct {
	r       *restarter
	s       *Remote
	h       hooked
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newHolding(t *testing.T) *holding {
	t.Helper()
	hd := &holding{entered: make(chan struct{}, 1), release: make(chan struct{})}
	hd.r = &restarter{t: t, serve: func(int) *sdk.Server {
		s := newServer(t)
		s.AddTool(&sdk.Tool{Name: "hold", InputSchema: json.RawMessage(`{"type":"object"}`)},
			func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
				err := req.Session.NotifyProgress(ctx, &sdk.ProgressNotificationParams{ProgressToken: "hold", Progress: 1})
				return &sdk.CallToolResult{}, err
			})
		return s
	}}
	hd.s = hd.r.connect(false, hd.h.option(), WithNotificationGrace(0), WithClientOptions(sdk.ClientOptions{
		ProgressNotificationHandler: func(context.Context, *sdk.ProgressNotificationClientRequest) {
			select {
			case hd.entered <- struct{}{}:
			default:
			}
			<-hd.release
		},
	}))
	// Registered after connect's Close, so it runs first: closing a
	// session waits for its handlers.
	t.Cleanup(hd.free)
	return hd
}

// free lets the client's handler return, and so the session end.
func (hd *holding) free() { hd.once.Do(func() { close(hd.release) }) }

// closeHeld leaves the session with the client's handler running and
// its connection closed, and returns it once it refuses requests with
// sdk.ErrConnectionClosed, not yet ended.
func (hd *holding) closeHeld(t *testing.T) *link {
	t.Helper()
	if _, err := execute(lookup(t, hd.s, "hold"), `{}`); err != nil {
		t.Fatal(err)
	}
	select {
	case <-hd.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the client's progress handler did not run")
	}
	l := hd.s.current()
	hd.r.kill()
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := l.session.Ping(context.Background(), nil)
		if errors.Is(err, sdk.ErrConnectionClosed) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the session does not refuse requests after its connection closed: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
	if l.isEnded() {
		t.Fatal("the session ended while the client's handler was running")
	}
	return l
}

// A call cut because its session's connection closed returns once the
// session has ended, so the very next call restarts the server and runs,
// rather than being sent on the dead session and cut as well (#90). The
// session ends 50 ms after the connection closed, when the client's
// handler returns; the cut call waits for it.
func TestReconnectRestartsOnTheCallAfterACut(t *testing.T) {
	const runs = 20
	failed := 0
	for i := range runs {
		hd := newHolding(t)
		old := hd.closeHeld(t)
		time.AfterFunc(50*time.Millisecond, hd.free)
		_, err := execute(lookup(t, hd.s, "upper"), `{"text":"x"}`)
		if !errors.Is(err, ErrSessionEnded) || !errors.Is(err, sdk.ErrConnectionClosed) {
			t.Fatalf("run %d: the first call returned %v; want it cut by the closed connection", i, err)
		}
		if !old.isEnded() {
			t.Errorf("run %d: the cut call returned before its session ended", i)
		}
		res, err := execute(lookup(t, hd.s, "upper"), `{"text":"x"}`)
		if err != nil || res.Output.Text != "X" {
			t.Logf("run %d: the call after the cut: %q, %v; want it to restart the server and run", i, res.Output.Text, err)
			failed++
			continue
		}
		if n := hd.r.dials.Load(); n != 1 {
			t.Errorf("run %d: dialed %d times, want 1", i, n)
		}
		if infos := hd.h.got(); len(infos) != 1 || infos[0].Err != nil {
			t.Errorf("run %d: hook got %+v; want one restart", i, infos)
		}
	}
	if failed > 0 {
		t.Errorf("in %d of %d runs the call after a cut was cut too", failed, runs)
	}
}

// A refresh whose listing the closed connection cuts is made once more
// on the restarted server, not on the dead session.
func TestReconnectRefreshAfterACut(t *testing.T) {
	const runs = 20
	failed := 0
	for i := range runs {
		hd := newHolding(t)
		hd.closeHeld(t)
		time.AfterFunc(50*time.Millisecond, hd.free)
		if err := hd.s.Refresh(context.Background()); err != nil {
			t.Logf("run %d: the refresh: %v; want it made again on the restarted server", i, err)
			failed++
			continue
		}
		if n := hd.r.dials.Load(); n != 1 {
			t.Errorf("run %d: dialed %d times, want 1", i, n)
		}
	}
	if failed > 0 {
		t.Errorf("in %d of %d runs the refresh was cut twice", failed, runs)
	}
}

// Calls the closed connection cuts together all wait for the session's
// end on the one channel and return; then the next call restarts the
// server once.
func TestReconnectConcurrentCutsWaitForTheEnd(t *testing.T) {
	hd := newHolding(t)
	old := hd.closeHeld(t)
	upper := lookup(t, hd.s, "upper")
	const n = 5
	type outcome struct {
		err   error
		ended bool
	}
	out := make(chan outcome, n)
	for range n {
		go func() {
			_, err := execute(upper, `{"text":"x"}`)
			out <- outcome{err, old.isEnded()}
		}()
	}
	time.AfterFunc(50*time.Millisecond, hd.free)
	for range n {
		select {
		case o := <-out:
			if !errors.Is(o.err, ErrSessionEnded) {
				t.Errorf("a cut call returned %v; want ErrSessionEnded", o.err)
			}
			if !o.ended {
				t.Error("a cut call returned before its session ended")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the cut calls did not all return")
		}
	}
	if res, err := execute(upper, `{"text":"x"}`); err != nil || res.Output.Text != "X" {
		t.Fatalf("the next call: %q, %v", res.Output.Text, err)
	}
	if got := hd.r.dials.Load(); got != 1 {
		t.Errorf("dialed %d times, want 1", got)
	}
}

// The wait for the end is bounded: a session that does not end within
// endGrace leaves the call cut all the same, and a caller whose context
// ends during the wait gets its call back at once, cut.
func TestReconnectCutWaitIsBounded(t *testing.T) {
	t.Run("the session does not end", func(t *testing.T) {
		hd := newHolding(t)
		hd.closeHeld(t)
		start := time.Now()
		_, err := execute(lookup(t, hd.s, "upper"), `{"text":"x"}`)
		took := time.Since(start)
		if !errors.Is(err, ErrSessionEnded) {
			t.Errorf("the call returned %v; want ErrSessionEnded", err)
		}
		if took < endGrace || took > endGrace+2*time.Second {
			t.Errorf("the call took %v; want about endGrace, %v", took, endGrace)
		}
	})
	t.Run("the caller's context ends", func(t *testing.T) {
		hd := newHolding(t)
		hd.closeHeld(t)
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err := lookup(t, hd.s, "upper").Execute(ctx, agenttool.Call{ID: "c", Args: json.RawMessage(`{"text":"x"}`)})
		took := time.Since(start)
		if !errors.Is(err, ErrSessionEnded) {
			t.Errorf("the call returned %v; want ErrSessionEnded", err)
		}
		if took >= endGrace {
			t.Errorf("the call took %v; want it back when its context ended, before endGrace", took)
		}
	})
}
