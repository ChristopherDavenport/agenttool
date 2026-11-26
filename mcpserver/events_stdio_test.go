package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agenttool/mcpclient"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// The stdio tests run a real subprocess: the test binary itself, which
// TestMain turns into an events server on stdin and stdout when
// stdioChildEnv is set. The parent drives it with mcpclient over an
// sdk.CommandTransport, the way an embedder starts a stdio MCP server.

const (
	stdioChildEnv     = "AGENTTOOL_EVENTS_STDIO_CHILD"
	stdioOriginEnv    = "AGENTTOOL_EVENTS_STDIO_ORIGIN"
	stdioPrincipalEnv = "AGENTTOOL_EVENTS_STDIO_PRINCIPAL"
	stdioMarkerEnv    = "AGENTTOOL_EVENTS_STDIO_MARKER"
)

func TestMain(m *testing.M) {
	if os.Getenv(stdioChildEnv) != "" {
		os.Exit(runStdioChild())
	}
	os.Exit(m.Run())
}

type stdioEmitArgs struct {
	Region string `json:"region"`
	ID     string `json:"id"`
}

// runStdioChild serves an emit-only "deploy" type and an "emit" tool that
// emits one event, until stdin closes. It then closes the Events and, when
// asked, records in a marker file whether its goroutines came back to
// what they were before the server started.
func runStdioChild() int {
	before := runtime.NumGoroutine()
	server := sdk.NewServer(&sdk.Implementation{Name: "events-stdio", Version: "1"}, nil)
	// A short nextPollMs so the client's loop polls again promptly.
	opts := EventsOptions{DefaultNextPoll: 20 * time.Millisecond}
	if origin := os.Getenv(stdioOriginEnv); origin != "" || os.Getenv(stdioPrincipalEnv) != "" {
		principal := os.Getenv(stdioPrincipalEnv)
		w := &WebhookOptions{Principal: func(context.Context, *sdk.RequestExtra, *sdk.ServerSession) string { return principal }}
		if origin != "" {
			w.TrustedOrigins = []string{origin}
		}
		opts.Webhook = w
	}
	ev, err := AddEvents(server, opts)
	if err != nil {
		return 2
	}
	et, err := ev.Register(EventDef{
		Name: "deploy", Description: "a deploy finished", EmitOnly: true,
		InputSchema:   json.RawMessage(`{"type":"object","properties":{"region":{"type":"string","x-mcp-header":"Region"}},"required":["region"]}`),
		PayloadSchema: json.RawMessage(countSchema),
		Match: func(_ EventContext, e Event, args json.RawMessage) bool {
			var a struct{ Region string }
			_ = json.Unmarshal(args, &a)
			return e.Meta["region"] == a.Region
		},
	})
	if err != nil {
		return 2
	}
	sdk.AddTool(server, &sdk.Tool{Name: "emit", Description: "emit a deploy event"},
		func(_ context.Context, _ *sdk.CallToolRequest, in stdioEmitArgs) (*sdk.CallToolResult, any, error) {
			if err := et.Emit(Event{ID: in.ID, Data: json.RawMessage(`{"n":1}`), Meta: map[string]any{"region": in.Region}}); err != nil {
				return nil, nil, err
			}
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "ok"}}}, nil, nil
		})
	runErr := server.Run(context.Background(), &sdk.StdioTransport{})
	ev.Close()
	if marker := os.Getenv(stdioMarkerEnv); marker != "" {
		verdict := "ok"
		deadline := time.Now().Add(2 * time.Second)
		// The runtime's own helpers may outlive the server by a moment.
		for runtime.NumGoroutine() > before {
			if time.Now().After(deadline) {
				verdict = "leak"
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		_ = os.WriteFile(marker, []byte(verdict), 0o600)
	}
	if runErr != nil {
		return 1
	}
	return 0
}

// stdioChild is a running subprocess and the client connected to it.
type stdioChild struct {
	cmd    *exec.Cmd
	remote *mcpclient.Remote
	marker string
}

// startStdioChild starts the server subprocess and connects to it with
// the events extension on.
func startStdioChild(t *testing.T, env ...string) *stdioChild {
	t.Helper()
	c := &stdioChild{marker: t.TempDir() + "/marker"}
	c.cmd = exec.Command(os.Args[0])
	c.cmd.Env = append(append(os.Environ(), stdioChildEnv+"=1", stdioMarkerEnv+"="+c.marker), env...)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	r, err := mcpclient.Connect(ctx, &sdk.CommandTransport{Command: c.cmd}, mcpclient.WithEvents())
	if err != nil {
		t.Fatalf("connect to the subprocess: %v", err)
	}
	c.remote = r
	t.Cleanup(func() { _ = c.stop() })
	return c
}

// stop closes the client, which ends the child, and returns when the
// process has exited.
func (c *stdioChild) stop() error {
	if c.remote == nil {
		return nil
	}
	err := c.remote.Close()
	c.remote = nil
	return err
}

// emit has the child emit one deploy event.
func (c *stdioChild) emit(t *testing.T, region, id string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := c.remote.Session().CallTool(ctx, &sdk.CallToolParams{Name: "emit", Arguments: map[string]any{"region": region, "id": id}})
	if err != nil || res.IsError {
		t.Fatalf("emit %s: %v %+v", id, err, res)
	}
}

// TestEventsStdioPoll holds go:events:stdio:poll: over a real stdio
// subprocess the extension is advertised, events/list names the type,
// and a poll bootstraps, then returns an emitted event with a cursor. The
// requests carry no HTTP headers, so the Mcp-Name check does not refuse.
func TestEventsStdioPoll(t *testing.T) {
	c := startStdioChild(t)
	ctx := context.Background()
	if !c.remote.EventsOffered() {
		t.Fatal("the extension was not advertised over stdio")
	}
	list, err := c.remote.ListEvents(ctx)
	if err != nil || len(list) != 1 || list[0].Name != "deploy" || !list[0].Polls() {
		t.Fatalf("list = %+v, %v", list, err)
	}
	args := json.RawMessage(`{"region":"eu"}`)
	boot, err := c.remote.PollEvents(ctx, mcpclient.PollParams{Name: "deploy", Arguments: args})
	if err != nil || boot.Cursor == nil || len(boot.Events) != 0 {
		t.Fatalf("bootstrap = %+v, %v", boot, err)
	}
	c.emit(t, "eu", "a")
	c.emit(t, "us", "elsewhere")
	res, err := c.remote.PollEvents(ctx, mcpclient.PollParams{Name: "deploy", Arguments: args, Cursor: boot.Cursor})
	if err != nil || res.Truncated || len(res.Events) != 1 || res.Events[0].EventID != "a" || res.Cursor == nil || *res.Cursor == *boot.Cursor {
		t.Fatalf("poll = %+v, %v", res, err)
	}
	again, err := c.remote.PollEvents(ctx, mcpclient.PollParams{Name: "deploy", Arguments: args, Cursor: res.Cursor})
	if err != nil || len(again.Events) != 0 {
		t.Fatalf("poll after the last = %+v, %v", again, err)
	}
}

// TestEventsStdioRunEvents holds go:events:stdio:run: the poll loop over
// a real subprocess delivers each event once.
func TestEventsStdioRunEvents(t *testing.T) {
	c := startStdioChild(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cursors := mcpclient.NewMemoryCursors()
	loop := stdioLoop(cursors)
	got := make(chan string, 16)
	done := make(chan error, 1)
	go func() {
		done <- c.remote.RunEvents(ctx, loop, func(_ context.Context, ev mcpclient.EventOccurrence) error {
			got <- ev.EventID
			return nil
		})
	}()
	stdioWaitCursor(t, cursors, loop)
	c.emit(t, "eu", "e1")
	c.emit(t, "eu", "e2")
	for _, want := range []string{"e1", "e2"} {
		select {
		case id := <-got:
			if id != want {
				t.Fatalf("got %s, want %s", id, want)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("no %s", want)
		}
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("RunEvents = %v", err)
	}
	select {
	case id := <-got:
		t.Errorf("%s delivered twice", id)
	default:
	}
}

func stdioLoop(cursors mcpclient.CursorStore) mcpclient.EventLoop {
	return mcpclient.EventLoop{Name: "deploy", Arguments: json.RawMessage(`{"region":"eu"}`), Cursors: cursors, Floor: 5 * time.Millisecond}
}

func stdioWaitCursor(t *testing.T, cursors mcpclient.CursorStore, loop mcpclient.EventLoop) {
	t.Helper()
	waitFor(t, func() bool {
		k, _ := cursors.Load(context.Background(), mcpclient.EventKey(loop.Name, loop.Arguments))
		return k != nil
	})
}

// TestEventsStdioRestart holds go:events:stdio:restart: after the server
// process is replaced, its cursors belong to another epoch, and a poll
// with the old one is answered as a gap with a fresh cursor, not an error;
// a loop run with the saved cursor reports the gap, keeps going and
// delivers what the new process emits.
func TestEventsStdioRestart(t *testing.T) {
	first := startStdioChild(t)
	ctx := context.Background()
	args := json.RawMessage(`{"region":"eu"}`)
	boot, err := first.remote.PollEvents(ctx, mcpclient.PollParams{Name: "deploy", Arguments: args})
	if err != nil || boot.Cursor == nil {
		t.Fatalf("bootstrap: %+v, %v", boot, err)
	}
	first.emit(t, "eu", "old")
	res, err := first.remote.PollEvents(ctx, mcpclient.PollParams{Name: "deploy", Arguments: args, Cursor: boot.Cursor})
	if err != nil || len(res.Events) != 1 {
		t.Fatalf("poll: %+v, %v", res, err)
	}
	old := *res.Cursor
	if err := first.stop(); err != nil {
		t.Logf("close of the first client: %v", err)
	}

	second := startStdioChild(t)
	gap, err := second.remote.PollEvents(ctx, mcpclient.PollParams{Name: "deploy", Arguments: args, Cursor: &old})
	if err != nil {
		t.Fatalf("poll with an old-epoch cursor = %v, want a gap", err)
	}
	if !gap.Truncated || gap.Cursor == nil || *gap.Cursor == old || len(gap.Events) != 0 {
		t.Fatalf("gap = %+v", gap)
	}

	cursors := mcpclient.NewMemoryCursors()
	loop := stdioLoop(cursors)
	if err := cursors.Save(ctx, mcpclient.EventKey(loop.Name, loop.Arguments), old); err != nil {
		t.Fatal(err)
	}
	gaps := make(chan struct{}, 4)
	loop.OnGap = func(context.Context) { gaps <- struct{}{} }
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	got := make(chan string, 4)
	done := make(chan error, 1)
	go func() {
		done <- second.remote.RunEvents(runCtx, loop, func(_ context.Context, ev mcpclient.EventOccurrence) error {
			got <- ev.EventID
			return nil
		})
	}()
	select {
	case <-gaps:
	case <-time.After(10 * time.Second):
		t.Fatal("the loop did not report the gap")
	}
	// The gap poll stored a fresh cursor of this process; wait for it.
	waitFor(t, func() bool {
		k, _ := cursors.Load(ctx, mcpclient.EventKey(loop.Name, loop.Arguments))
		return k != nil && *k != old
	})
	second.emit(t, "eu", "new")
	select {
	case id := <-got:
		if id != "new" {
			t.Fatalf("got %s after the restart", id)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the loop did not continue after the restart")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("RunEvents = %v", err)
	}
}

// TestEventsStdioShutdown holds go:events:stdio:shutdown: closing the
// client ends the child, which exits cleanly with no goroutine left
// behind by its leases or rings once Events.Close has run.
func TestEventsStdioShutdown(t *testing.T) {
	c := startStdioChild(t)
	ctx := context.Background()
	// Hold a lease so there is something for Close to end.
	if _, err := c.remote.PollEvents(ctx, mcpclient.PollParams{Name: "deploy", Arguments: json.RawMessage(`{"region":"eu"}`)}); err != nil {
		t.Fatal(err)
	}
	cmd := c.cmd
	if err := c.stop(); err != nil {
		t.Logf("close: %v", err)
	}
	if cmd.ProcessState == nil {
		t.Fatal("the child has not been waited for after Close")
	}
	if cmd.ProcessState.ExitCode() != 0 {
		t.Errorf("the child exited with %v", cmd.ProcessState)
	}
	b, err := os.ReadFile(c.marker)
	if err != nil {
		t.Fatalf("the child wrote no marker, so it did not shut down in order: %v", err)
	}
	if string(b) != "ok" {
		t.Errorf("the child's goroutines after shutdown: %s", b)
	}
	if err := cmd.Process.Signal(os.Signal(nil)); err == nil {
		t.Error("the child process is still there")
	}
}

// TestEventsStdioWebhook holds go:events:stdio:webhook: a subscribe over
// stdio, where there is no authentication, is refused with -32024 unless
// the embedder's Principal names someone; with one, and a trusted local
// callback, the subscription is verified, and an emitted event is POSTed
// to the receiver, which verifies its signature.
func TestEventsStdioWebhook(t *testing.T) {
	secrets := mcpclient.NewMemorySecrets()
	var mu sync.Mutex
	var msgs []mcpclient.WebhookMessage
	receiver := httptest.NewServer(mcpclient.NewWebhookReceiver(mcpclient.WebhookReceiverOptions{
		Secrets: secrets,
		Handle: func(_ context.Context, m mcpclient.WebhookMessage) error {
			mu.Lock()
			msgs = append(msgs, m)
			mu.Unlock()
			return nil
		},
	}))
	t.Cleanup(receiver.Close)
	secret, err := mcpclient.NewWebhookSecret()
	if err != nil {
		t.Fatal(err)
	}
	secrets.Add(secret)
	ctx := context.Background()
	p := mcpclient.SubscribeParams{Name: "deploy", Arguments: json.RawMessage(`{"region":"eu"}`), URL: receiver.URL + "/hook", Secret: secret, TTL: 10 * time.Minute}

	nobody := startStdioChild(t, stdioOriginEnv+"="+receiver.URL)
	var ee *mcpclient.EventError
	if _, err := nobody.remote.SubscribeEvents(ctx, p); !errors.As(err, &ee) || ee.Code != mcpclient.EventsCodeForbidden {
		t.Errorf("subscribe without a principal = %v, want -32024", err)
	}

	c := startStdioChild(t, stdioOriginEnv+"="+receiver.URL, stdioPrincipalEnv+"=local")
	res, err := c.remote.SubscribeEvents(ctx, p)
	if err != nil || res.ID == "" {
		t.Fatalf("subscribe with a principal: %+v, %v", res, err)
	}
	c.emit(t, "eu", "w1")
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(msgs) == 1 })
	mu.Lock()
	m := msgs[0]
	mu.Unlock()
	if m.Kind != mcpclient.WebhookEvent || m.SubscriptionID != res.ID || m.Event.EventID != "w1" || !strings.HasPrefix(m.Event.Name, "deploy") {
		t.Errorf("message %+v", m)
	}
}
