package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agenttool/mcpclient"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// eventsServer is a server with one emit-only type, "deploy", whose
// region argument is carried in a header.
func eventsServer(t *testing.T) (*sdk.Server, *EventType) {
	t.Helper()
	server := sdk.NewServer(&sdk.Implementation{Name: "events", Version: "1"}, nil)
	ev, err := AddEvents(server, EventsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	et, err := ev.Register(EventDef{
		Name: "deploy", Description: "a deploy finished", EmitOnly: true,
		InputSchema:   json.RawMessage(`{"type":"object","properties":{"region":{"type":"string","x-mcp-header":"Region"}},"required":["region"]}`),
		PayloadSchema: json.RawMessage(countSchema),
		Match: func(_ EventContext, ev Event, args json.RawMessage) bool {
			var a, d struct {
				Region string `json:"region"`
			}
			_ = json.Unmarshal(args, &a)
			if v, ok := ev.Meta["region"].(string); ok {
				d.Region = v
			}
			return a.Region == d.Region
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return server, et
}

func emitIn(t *testing.T, et *EventType, region, id string) {
	t.Helper()
	if err := et.Emit(Event{ID: id, Data: json.RawMessage(`{"n":1}`), Meta: map[string]any{"region": region}}); err != nil {
		t.Fatal(err)
	}
}

// TestEventsClientServer holds go:events:poll-loop end to end: the
// client lists what the server serves and its loop receives each emitted
// event once, in order, resuming from its stored cursor.
func TestEventsClientServer(t *testing.T) {
	server, et := eventsServer(t)
	s := roundTrip(t, server, mcpclient.WithEvents())
	if !s.EventsOffered() {
		t.Fatal("the server's extension was not seen")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	list, err := s.ListEvents(ctx)
	if err != nil || len(list) != 1 || list[0].Name != "deploy" || !list[0].Polls() {
		t.Fatalf("list = %+v, %v", list, err)
	}

	cursors := mcpclient.NewMemoryCursors()
	loop := mcpclient.EventLoop{Name: "deploy", Arguments: json.RawMessage(`{"region":"eu"}`), Cursors: cursors, Floor: time.Millisecond}
	var mu sync.Mutex
	var got []string
	handle := func(_ context.Context, ev mcpclient.EventOccurrence) error {
		mu.Lock()
		got = append(got, ev.EventID)
		mu.Unlock()
		return nil
	}
	wake := make(chan struct{}, 16)
	loop.Sleep = func(ctx context.Context, _ time.Duration) error {
		select {
		case <-wake:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	done := make(chan error, 1)
	go func() { done <- s.RunEvents(ctx, loop, handle) }()

	// The first poll starts from now: nothing emitted before it is seen.
	emitIn(t, et, "eu", "never") // may or may not precede the bootstrap
	waitFor(t, func() bool {
		k, _ := cursors.Load(ctx, mcpclient.EventKey("deploy", loop.Arguments))
		return k != nil
	})
	mu.Lock()
	got = nil
	mu.Unlock()
	for i := 1; i <= 3; i++ {
		emitIn(t, et, "eu", fmt.Sprintf("eu%d", i))
	}
	emitIn(t, et, "us", "us1") // another subscription's
	wake <- struct{}{}
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(got) >= 3 })
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("RunEvents = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(got, ",") != "eu1,eu2,eu3" {
		t.Errorf("received %v, want eu1,eu2,eu3 and not us1", got)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// statusRecorder is a round tripper that records the status of each
// response and can tamper with a request's headers.
type statusRecorder struct {
	next   http.RoundTripper
	tamper func(*http.Request)
	mu     sync.Mutex
	status []int
}

func (s *statusRecorder) RoundTrip(r *http.Request) (*http.Response, error) {
	if s.tamper != nil && r.Header.Get("Mcp-Name") != "" {
		r = r.Clone(r.Context())
		s.tamper(r)
	}
	resp, err := s.next.RoundTrip(r)
	if err == nil && r.Header.Get("Mcp-Name") != "" {
		s.mu.Lock()
		s.status = append(s.status, resp.StatusCode)
		s.mu.Unlock()
	}
	return resp, err
}

// TestEventsHTTPHeaders holds events:mcp-name, events:mcp-param and
// go:events:header-roundtrip over Streamable HTTP, protocol 2026-07-28:
// the headers a client sends are the ones the server accepts, and a
// request whose headers differ from its params is refused with -32020
// and data.mismatch.
func TestEventsHTTPHeaders(t *testing.T) {
	server, et := eventsServer(t)
	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, &sdk.StreamableHTTPOptions{Stateless: true})
	hs := httptest.NewServer(handler)
	t.Cleanup(hs.Close)
	rec := &statusRecorder{next: http.DefaultTransport}
	connect := func() *mcpclient.Remote {
		s, err := mcpclient.Connect(context.Background(), &sdk.StreamableClientTransport{Endpoint: hs.URL, HTTPClient: &http.Client{Transport: mcpclient.EventHeaders(rec)}}, mcpclient.WithEvents())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	}
	s := connect()
	ctx := context.Background()
	if v := s.Session().InitializeResult().ProtocolVersion; v < "2026-07-28" {
		t.Skipf("the SDK negotiated %s, which has no routing headers", v)
	}
	args := json.RawMessage(`{"region":"eu"}`)

	res, err := s.PollEvents(ctx, mcpclient.PollParams{Name: "deploy", Arguments: args})
	if err != nil || res.Cursor == nil {
		t.Fatalf("poll with the client's own headers: %+v, %v", res, err)
	}
	emitIn(t, et, "eu", "a")
	res, err = s.PollEvents(ctx, mcpclient.PollParams{Name: "deploy", Arguments: args, Cursor: res.Cursor})
	if err != nil || len(res.Events) != 1 {
		t.Fatalf("second poll: %+v, %v", res, err)
	}

	tests := []struct {
		name   string
		tamper func(*http.Request)
		header string
	}{
		{"name", func(r *http.Request) { r.Header.Set("Mcp-Name", "other") }, "Mcp-Name"},
		{"param", func(r *http.Request) { r.Header.Set("Mcp-Param-Region", "us") }, "Mcp-Param-Region"},
		{"param dropped", func(r *http.Request) { r.Header.Del("Mcp-Param-Region") }, "Mcp-Param-Region"},
	}
	for _, tc := range tests {
		rec.tamper = tc.tamper
		_, err := s.PollEvents(ctx, mcpclient.PollParams{Name: "deploy", Arguments: args})
		var ee *mcpclient.EventError
		if !errors.As(err, &ee) || ee.Code != mcpclient.EventsCodeHeaderMismatch {
			t.Errorf("%s: error %v, want -32020", tc.name, err)
			continue
		}
		var data struct {
			Mismatch struct {
				Header string `json:"header"`
			} `json:"mismatch"`
		}
		if json.Unmarshal(ee.Data, &data) != nil || data.Mismatch.Header != tc.header {
			t.Errorf("%s: data = %s, want mismatch of %s", tc.name, ee.Data, tc.header)
		}
	}
	rec.tamper = nil
	// The SDK answers a refusal found in a handler with HTTP 200 and the
	// error in the body; the SEP says 400. Recorded here so a change of
	// either is noticed.
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, st := range rec.status {
		if st != http.StatusOK {
			t.Logf("a response had status %d", st)
		}
	}
}

// TestEventsConstantsMatchClient holds go:events:constants-match-client:
// this module's wire names are mcpclient's, read out of the sibling's
// source as TestFactsConstantsMatchClient does.
func TestEventsConstantsMatchClient(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "mcpclient", "events.go"))
	if err != nil {
		t.Skip("mcpclient is not beside this module here: " + err.Error())
	}
	f, err := parser.ParseFile(token.NewFileSet(), "events.go", src, 0)
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
				if i >= len(vs.Values) {
					continue
				}
				if lit, ok := vs.Values[i].(*ast.BasicLit); ok {
					if lit.Kind == token.STRING {
						theirs[name.Name], _ = strconv.Unquote(lit.Value)
					} else {
						theirs[name.Name] = lit.Value
					}
				}
				if un, ok := vs.Values[i].(*ast.UnaryExpr); ok {
					if lit, ok := un.X.(*ast.BasicLit); ok {
						theirs[name.Name] = un.Op.String() + lit.Value
					}
				}
			}
		}
	}
	ours := map[string]string{
		"EventsExtension": EventsExtension, "EventsListMethod": EventsListMethod, "EventsPollMethod": EventsPollMethod,
		"EventsCodeInvalidParams": strconv.Itoa(EventsCodeInvalidParams), "EventsCodeNotFound": strconv.Itoa(EventsCodeNotFound),
		"EventsCodeForbidden": strconv.Itoa(EventsCodeForbidden), "EventsCodeResourceExhausted": strconv.Itoa(EventsCodeResourceExhausted),
		"EventsCodeUnsupported": strconv.Itoa(EventsCodeUnsupported), "EventsCodeCallbackEndpoint": strconv.Itoa(EventsCodeCallbackEndpoint),
		"EventsCodeHeaderMismatch": strconv.Itoa(EventsCodeHeaderMismatch),
	}
	for name, v := range ours {
		if theirs[name] != v {
			t.Errorf("%s: mcpserver %q, mcpclient %q", name, v, theirs[name])
		}
	}
}

// TestEventsRequirementsCovered checks that every requirement id of
// eventsRequirements is named by a test beside it, in either module.
func TestEventsRequirementsCovered(t *testing.T) {
	var text strings.Builder
	for _, dir := range []string{".", filepath.Join("..", "mcpclient")} {
		files, _ := filepath.Glob(filepath.Join(dir, "*_test.go"))
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			s := string(b)
			if i := strings.Index(s, "var eventsRequirements = []string{"); i >= 0 {
				j := strings.Index(s[i:], "\n}\n")
				s = s[:i] + s[i+j:]
			}
			text.WriteString(s)
		}
	}
	for _, id := range eventsRequirements {
		if !strings.Contains(text.String(), id) {
			t.Errorf("requirement %s is not held by any test", id)
		}
	}
}
