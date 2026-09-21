package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/openresponses"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const textSchema = `{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`

type textArgs struct {
	Text string `json:"text"`
}

// newServer builds an SDK server with one tool of every result shape.
func newServer(t *testing.T) *sdk.Server {
	t.Helper()
	s := sdk.NewServer(&sdk.Implementation{Name: "fixture", Version: "1"}, nil)
	s.AddTool(&sdk.Tool{Name: "upper", Description: "uppercase text", InputSchema: json.RawMessage(textSchema)},
		func(_ context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			var a textArgs
			if err := json.Unmarshal(req.Params.Arguments, &a); err != nil {
				return nil, err
			}
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: strings.ToUpper(a.Text)}}}, nil
		})
	s.AddTool(&sdk.Tool{Name: "two_texts", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "a"}, &sdk.TextContent{Text: "b"}}}, nil
		})
	s.AddTool(&sdk.Tool{Name: "image", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return &sdk.CallToolResult{Content: []sdk.Content{
				&sdk.TextContent{Text: "here"},
				&sdk.ImageContent{MIMEType: "image/png", Data: []byte{1, 2, 3}},
			}}, nil
		})
	s.AddTool(&sdk.Tool{Name: "resources", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return &sdk.CallToolResult{Content: []sdk.Content{
				&sdk.EmbeddedResource{Resource: &sdk.ResourceContents{URI: "file:///a.txt", Text: "text resource"}},
				&sdk.EmbeddedResource{Resource: &sdk.ResourceContents{URI: "file:///b.bin", Blob: []byte{9}}},
				&sdk.ResourceLink{URI: "file:///c"},
			}}, nil
		})
	s.AddTool(&sdk.Tool{Name: "fails", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return &sdk.CallToolResult{IsError: true, Content: []sdk.Content{&sdk.TextContent{Text: "disk on fire"}}}, nil
		})
	s.AddTool(&sdk.Tool{Name: "structured", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "summary"}}, StructuredContent: map[string]any{"n": 1}}, nil
		})
	s.AddTool(&sdk.Tool{Name: "progress", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			if token := req.Params.GetProgressToken(); token != nil {
				for i := 1; i <= 2; i++ {
					if err := req.Session.NotifyProgress(ctx, &sdk.ProgressNotificationParams{ProgressToken: token, Progress: float64(i), Total: 2, Message: "step"}); err != nil {
						return nil, err
					}
				}
			}
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "done"}}}, nil
		})
	return s
}

func connect(t *testing.T, server *sdk.Server, opts ...Option) *Remote {
	t.Helper()
	ct, st := sdk.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := server.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	s, err := Connect(ctx, ct, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func lookup(t *testing.T, s *Remote, name string) agenttool.Tool {
	t.Helper()
	tl, ok := agenttool.Set(s.Tools()).Lookup(name)
	if !ok {
		t.Fatalf("tool %q not found in %v", name, names(s.Tools()))
	}
	return tl
}

func names(tools []agenttool.Tool) []string {
	out := make([]string, 0, len(tools))
	for _, tl := range tools {
		out = append(out, tl.Name())
	}
	return out
}

func TestConnectListsTools(t *testing.T) {
	s := connect(t, newServer(t))
	upper := lookup(t, s, "upper")
	if upper.Description() != "uppercase text" {
		t.Errorf("description = %q", upper.Description())
	}
	var got, want any
	if err := json.Unmarshal(upper.Parameters(), &got); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal([]byte(textSchema), &want)
	if gs, ws := mustJSON(got), mustJSON(want); gs != ws {
		t.Errorf("schema = %s, want %s", gs, ws)
	}
	if agenttool.IsStrict(upper) || agenttool.IsSequential(upper) {
		t.Error("remote tools are neither strict nor sequential by default")
	}
	if s.Session() == nil {
		t.Error("no session")
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestPrefixSequentialAndResource(t *testing.T) {
	cases := []struct {
		name         string
		opts         []Option
		wantName     string
		sequential   bool
		wantResource string
	}{
		{name: "no prefix", wantName: "upper"},
		{name: "prefix", opts: []Option{WithPrefix("fs")}, wantName: "fs__upper"},
		{name: "sequential by remote name", opts: []Option{WithPrefix("fs"), WithSequential("upper")}, wantName: "fs__upper", sequential: true},
		{name: "sequential by local name", opts: []Option{WithPrefix("fs"), WithSequential("fs__upper")}, wantName: "fs__upper", sequential: true},
		{name: "resource by remote name", opts: []Option{WithPrefix("fs"), WithResource("shell:session", "upper")}, wantName: "fs__upper", wantResource: "shell:session"},
		{name: "resource by local name", opts: []Option{WithPrefix("fs"), WithResource("shell:session", "fs__upper")}, wantName: "fs__upper", wantResource: "shell:session"},
		{name: "sequential wins over resource", opts: []Option{WithSequential("upper"), WithResource("shell:session", "upper")}, wantName: "upper", sequential: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := connect(t, newServer(t), tc.opts...)
			tl := lookup(t, s, tc.wantName)
			if agenttool.IsSequential(tl) != tc.sequential {
				t.Errorf("sequential = %v, want %v", agenttool.IsSequential(tl), tc.sequential)
			}
			if got := agenttool.ResourceOf(tl); got != tc.wantResource {
				t.Errorf("resource = %q, want %q", got, tc.wantResource)
			}
			other := lookup(t, s, s.Name("image"))
			if agenttool.IsSequential(other) || agenttool.ResourceOf(other) != "" {
				t.Error("other tools should be neither sequential nor resourced")
			}
		})
	}
}

// TestResourceSharedByTwoTools: two remote tools named in one call
// share the state, so the executor never runs them together.
func TestResourceSharedByTwoTools(t *testing.T) {
	s := connect(t, newServer(t), WithResource("shell:session", "upper", "two_texts"))
	for _, name := range []string{"upper", "two_texts"} {
		if got := agenttool.ResourceOf(lookup(t, s, name)); got != "shell:session" {
			t.Errorf("%s resource = %q", name, got)
		}
	}
}

func TestExecuteMapsResults(t *testing.T) {
	s := connect(t, newServer(t))
	ctx := context.Background()
	cases := []struct {
		name     string
		tool     string
		args     string
		wantText string
		wantErr  string
		check    func(t *testing.T, r agenttool.Result)
	}{
		{name: "text", tool: "upper", args: `{"text":"abc"}`, wantText: "ABC"},
		{name: "empty args", tool: "two_texts", args: ``, wantText: "a\nb"},
		{name: "structured appended", tool: "structured", args: `{}`, wantText: "summary\n{\"n\":1}"},
		{name: "is_error", tool: "fails", args: `{}`, wantErr: "disk on fire"},
		{name: "image becomes parts", tool: "image", args: `{}`, check: func(t *testing.T, r agenttool.Result) {
			if r.Output.Text != "" || len(r.Output.Parts) != 2 {
				t.Fatalf("output = %+v", r.Output)
			}
			if txt, ok := r.Output.Parts[0].(*openresponses.Text); !ok || txt.Text != "here" {
				t.Errorf("part 0 = %+v", r.Output.Parts[0])
			}
			img, ok := r.Output.Parts[1].(*openresponses.InputImage)
			if !ok || img.ImageURL != "data:image/png;base64,AQID" {
				t.Errorf("part 1 = %+v", r.Output.Parts[1])
			}
		}},
		{name: "resources", tool: "resources", args: `{}`, check: func(t *testing.T, r agenttool.Result) {
			if len(r.Output.Parts) != 3 {
				t.Fatalf("parts = %+v", r.Output.Parts)
			}
			if txt, ok := r.Output.Parts[0].(*openresponses.Text); !ok || txt.Text != "text resource" {
				t.Errorf("part 0 = %+v", r.Output.Parts[0])
			}
			if f, ok := r.Output.Parts[1].(*openresponses.InputFile); !ok || f.Filename != "file:///b.bin" || f.FileData != "CQ==" {
				t.Errorf("part 1 = %+v", r.Output.Parts[1])
			}
			if txt, ok := r.Output.Parts[2].(*openresponses.Text); !ok || txt.Text != "resource: file:///c" {
				t.Errorf("part 2 = %+v", r.Output.Parts[2])
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := lookup(t, s, tc.tool).Execute(ctx, agenttool.Call{ID: "c", Args: json.RawMessage(tc.args)})
			if tc.wantErr != "" {
				if err == nil || err.Error() != tc.wantErr {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				if _, ok := res.Details.(*sdk.CallToolResult); !ok {
					t.Error("details should carry the SDK result")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.check != nil {
				tc.check(t, res)
				return
			}
			if res.Output.Text != tc.wantText {
				t.Errorf("text = %q, want %q", res.Output.Text, tc.wantText)
			}
		})
	}
}

func TestProgressForwarded(t *testing.T) {
	s := connect(t, newServer(t))
	var mu sync.Mutex
	var updates []agenttool.ProgressInfo
	res, err := lookup(t, s, "progress").Execute(context.Background(), agenttool.Call{ID: "c", Args: json.RawMessage(`{}`), OnUpdate: func(r agenttool.Result) {
		mu.Lock()
		defer mu.Unlock()
		updates = append(updates, r.Details.(agenttool.ProgressInfo))
		if r.Output.Text != "step" {
			t.Errorf("update text = %q", r.Output.Text)
		}
	}})
	if err != nil || res.Output.Text != "done" {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	// The SDK dispatches notifications asynchronously, so the last update
	// may land after the result.
	got := waitFor(t, func() []agenttool.ProgressInfo {
		mu.Lock()
		defer mu.Unlock()
		return append([]agenttool.ProgressInfo(nil), updates...)
	}, 2)
	if got[1].Progress != 2 || got[1].Total != 2 {
		t.Errorf("updates = %+v", got)
	}
	// Without OnUpdate no token is sent and the call still works.
	if _, err := lookup(t, s, "progress").Execute(context.Background(), agenttool.Call{Args: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
}

// waitFor polls get until it returns n items or a deadline passes.
func waitFor[T any](t *testing.T, get func() []T, n int) []T {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := get()
		if len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("got %d items, want %d: %+v", len(got), n, got)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestListChangedRefreshes(t *testing.T) {
	server := newServer(t)
	s := connect(t, server)
	before := len(s.Tools())
	server.AddTool(&sdk.Tool{Name: "late", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "late"}}}, nil
		})
	deadline := time.Now().Add(5 * time.Second)
	for len(s.Tools()) == before {
		if time.Now().After(deadline) {
			t.Fatalf("tools not refreshed: %v", names(s.Tools()))
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, ok := agenttool.Set(s.Tools()).Lookup("late"); !ok {
		t.Errorf("late tool missing: %v", names(s.Tools()))
	}
	server.RemoveTools("late")
	deadline = time.Now().Add(5 * time.Second)
	for len(s.Tools()) != before {
		if time.Now().After(deadline) {
			t.Fatalf("removal not reflected: %v", names(s.Tools()))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRefreshErrorReported(t *testing.T) {
	server := newServer(t)
	var failing atomic.Bool
	server.AddReceivingMiddleware(func(next sdk.MethodHandler) sdk.MethodHandler {
		return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
			if method == "tools/list" && failing.Load() {
				return nil, errors.New("listing broke")
			}
			return next(ctx, method, req)
		}
	})
	errs := make(chan error, 1)
	s := connect(t, server, WithRefreshError(func(err error) { errs <- err }))
	before := names(s.Tools())

	failing.Store(true)
	server.AddTool(&sdk.Tool{Name: "late", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return &sdk.CallToolResult{}, nil
		})
	select {
	case err := <-errs:
		if !strings.Contains(err.Error(), "listing broke") {
			t.Errorf("refresh err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("refresh error not reported")
	}
	if got := names(s.Tools()); strings.Join(got, ",") != strings.Join(before, ",") {
		t.Errorf("snapshot changed on a failed refresh: %v, was %v", got, before)
	}
	// Await reports the failure rather than blocking, until a refresh
	// succeeds.
	if err := s.Await(context.Background()); err == nil || !strings.Contains(err.Error(), "listing broke") {
		t.Errorf("Await after a failed refresh = %v", err)
	}
	failing.Store(false)
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Await(context.Background()); err != nil {
		t.Errorf("Await after a successful refresh = %v", err)
	}
	if _, ok := agenttool.Set(s.Tools()).Lookup("late"); !ok {
		t.Errorf("late tool missing after Refresh: %v", names(s.Tools()))
	}
}

// holdListing installs middleware that, while hold is set, blocks
// tools/list until release is closed, so a refresh can be observed in
// flight.
func holdListing(server *sdk.Server) (hold *atomic.Bool, release chan struct{}) {
	hold = new(atomic.Bool)
	release = make(chan struct{})
	server.AddReceivingMiddleware(func(next sdk.MethodHandler) sdk.MethodHandler {
		return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
			if method == "tools/list" && hold.Load() {
				select {
				case <-release:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			return next(ctx, method, req)
		}
	})
	return hold, release
}

func addLate(server *sdk.Server) {
	server.AddTool(&sdk.Tool{Name: "late", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "late"}}}, nil
		})
}

// waitSeen blocks until the remote has received n tool-list-changed
// notifications.
func waitSeen(t *testing.T, s *Remote, n int64) {
	t.Helper()
	waitFor(t, func() []struct{} { return make([]struct{}, s.seen.Load()) }, int(n))
}

func TestAwaitWaitsForRefresh(t *testing.T) {
	server := newServer(t)
	hold, release := holdListing(server)
	s := connect(t, server)
	if err := s.Await(context.Background()); err != nil {
		t.Fatalf("Await with nothing pending = %v", err)
	}

	hold.Store(true)
	addLate(server)
	waitSeen(t, s, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := s.Await(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Await while the listing is held = %v, want deadline", err)
	}
	if _, ok := agenttool.Set(s.Tools()).Lookup("late"); ok {
		t.Fatal("snapshot changed before the listing returned")
	}

	close(release)
	if err := s.Await(context.Background()); err != nil {
		t.Fatalf("Await after the listing = %v", err)
	}
	if _, ok := agenttool.Set(s.Tools()).Lookup("late"); !ok {
		t.Errorf("late tool missing after Await: %v", names(s.Tools()))
	}
}

func TestCallWaitsForRefresh(t *testing.T) {
	server := newServer(t)
	hold, release := holdListing(server)
	var remote atomic.Pointer[Remote]
	server.AddTool(&sdk.Tool{Name: "unlock", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(ctx context.Context, _ *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			server.AddTool(&sdk.Tool{Name: "secret", InputSchema: json.RawMessage(`{"type":"object"}`)},
				func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
					return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "shh"}}}, nil
				})
			// Return only once the client has the notification, so the
			// result follows it as it would from a server that notifies
			// synchronously.
			for remote.Load().seen.Load() == 0 {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(time.Millisecond):
				}
			}
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "unlocked"}}}, nil
		})
	s := connect(t, server)
	remote.Store(s)
	unlock := lookup(t, s, "unlock")

	hold.Store(true)
	type outcome struct {
		res agenttool.Result
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := unlock.Execute(context.Background(), agenttool.Call{ID: "c", Args: json.RawMessage(`{}`)})
		done <- outcome{res, err}
	}()
	waitSeen(t, s, 1)
	time.Sleep(20 * time.Millisecond)
	select {
	case o := <-done:
		t.Fatalf("call returned before the refresh: %+v", o)
	default:
	}
	if _, ok := agenttool.Set(s.Tools()).Lookup("secret"); ok {
		t.Fatal("snapshot changed before the listing returned")
	}

	close(release)
	select {
	case o := <-done:
		if o.err != nil || o.res.Output.Text != "unlocked" {
			t.Fatalf("res = %+v, err = %v", o.res, o.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("call did not return after the listing")
	}
	if _, ok := agenttool.Set(s.Tools()).Lookup("secret"); !ok {
		t.Errorf("secret tool missing when the call returned: %v", names(s.Tools()))
	}
}

func TestAudioName(t *testing.T) {
	cases := []struct {
		mediaType string
		want      string
	}{
		{"", "audio"},
		{"audio/x-no-such-type", "audio"},
		{"audio/wav", "audio.wav"},
		{"Audio/MPEG; codecs=x", "audio.mp3"},
	}
	for _, tc := range cases {
		if got := audioName(tc.mediaType); got != tc.want {
			t.Errorf("audioName(%q) = %q, want %q", tc.mediaType, got, tc.want)
		}
	}
	res, err := ResultOf(&sdk.CallToolResult{Content: []sdk.Content{&sdk.AudioContent{MIMEType: "audio/wav", Data: []byte{1}}}})
	if err != nil {
		t.Fatal(err)
	}
	if f, ok := res.Output.Parts[0].(*openresponses.InputFile); !ok || f.Filename != "audio.wav" || f.FileData != "AQ==" {
		t.Errorf("audio part = %+v", res.Output.Parts[0])
	}
}

func TestResultEdgeCases(t *testing.T) {
	if _, err := ResultOf(nil); err == nil {
		t.Error("nil result should error")
	}
	if _, err := ResultOf(&sdk.CallToolResult{IsError: true}); err == nil || err.Error() != "tool call failed" {
		t.Errorf("empty error result = %v", err)
	}
	res, err := ResultOf(&sdk.CallToolResult{Content: []sdk.Content{&sdk.ImageContent{Data: []byte{1}}}})
	if err != nil || res.Output.Parts[0].(*openresponses.InputImage).ImageURL != "data:application/octet-stream;base64,AQ==" {
		t.Errorf("image without mime = %+v, %v", res, err)
	}
}

func ptr[T any](v T) *T { return &v }

// TestAnnotationsMapped covers the hints a policy layer keys on,
// including the defaults MCP applies to a block that omits one.
func TestAnnotationsMapped(t *testing.T) {
	cases := []struct {
		name string
		tool *sdk.Tool
		want agenttool.Annotations
		ok   bool
	}{
		{
			name: "no annotations",
			tool: &sdk.Tool{Name: "plain", InputSchema: json.RawMessage(`{"type":"object"}`)},
		},
		{
			name: "read-only",
			tool: &sdk.Tool{Name: "search", InputSchema: json.RawMessage(`{"type":"object"}`),
				Annotations: &sdk.ToolAnnotations{Title: "Search issues", ReadOnlyHint: true, DestructiveHint: ptr(false), OpenWorldHint: ptr(false)}},
			want: agenttool.Annotations{Title: "Search issues", ReadOnly: true},
			ok:   true,
		},
		{
			name: "destructive and open world",
			tool: &sdk.Tool{Name: "delete", InputSchema: json.RawMessage(`{"type":"object"}`),
				Annotations: &sdk.ToolAnnotations{DestructiveHint: ptr(true), OpenWorldHint: ptr(true)}},
			want: agenttool.Annotations{Destructive: true, OpenWorld: true},
			ok:   true,
		},
		{
			name: "the omitted hints take MCP's defaults",
			tool: &sdk.Tool{Name: "write", InputSchema: json.RawMessage(`{"type":"object"}`),
				Annotations: &sdk.ToolAnnotations{IdempotentHint: true}},
			want: agenttool.Annotations{Destructive: true, Idempotent: true, OpenWorld: true},
			ok:   true,
		},
		{
			name: "the tool's own title wins",
			tool: &sdk.Tool{Name: "write", Title: "Write a file", InputSchema: json.RawMessage(`{"type":"object"}`),
				Annotations: &sdk.ToolAnnotations{Title: "older title", ReadOnlyHint: true, DestructiveHint: ptr(false), OpenWorldHint: ptr(false)}},
			want: agenttool.Annotations{Title: "Write a file", ReadOnly: true},
			ok:   true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := AnnotationsOf(tc.tool)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("AnnotationsOf = %+v %v, want %+v %v", got, ok, tc.want, tc.ok)
			}
			// The same hints reach the local tool the adapter builds.
			server := sdk.NewServer(&sdk.Implementation{Name: "annotated", Version: "1"}, nil)
			server.AddTool(tc.tool, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
				return &sdk.CallToolResult{}, nil
			})
			s := connect(t, server)
			if got := agenttool.AnnotationsOf(lookup(t, s, tc.tool.Name)); got != tc.want {
				t.Errorf("local tool's annotations = %+v, want %+v", got, tc.want)
			}
		})
	}
	if _, ok := AnnotationsOf(nil); ok {
		t.Error("a nil tool reported annotations")
	}
}

// TestCallSeesATooltAddedByItself is the reference SDK's own timing: a
// tool whose handler adds a tool returns before the notification is
// sent, because the server arms it on a 10 ms timer, so the call must
// leave a grace window for it or the new tool appears a turn late.
func TestCallSeesAToolAddedByItself(t *testing.T) {
	server := newServer(t)
	server.AddTool(&sdk.Tool{Name: "unlock", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(ctx context.Context, _ *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			server.AddTool(&sdk.Tool{Name: "secret", InputSchema: json.RawMessage(`{"type":"object"}`)},
				func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
					return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "shh"}}}, nil
				})
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "unlocked"}}}, nil
		})
	s := connect(t, server)
	res, err := lookup(t, s, "unlock").Execute(context.Background(), agenttool.Call{ID: "c", Args: json.RawMessage(`{}`)})
	if err != nil || res.Output.Text != "unlocked" {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	if _, ok := agenttool.Set(s.Tools()).Lookup("secret"); !ok {
		t.Errorf("the tool the call added is not in the list it returned with: %v", names(s.Tools()))
	}
}

func TestAwaitNotificationGrace(t *testing.T) {
	const grace = 100 * time.Millisecond
	cases := []struct {
		name        string
		grace       time.Duration
		listChanged bool
		notify      bool // a notification arrives while the wait is on
		wantWait    bool
	}{
		{name: "nothing comes", grace: grace, listChanged: true, wantWait: true},
		{name: "a notification ends the wait", grace: grace, listChanged: true, notify: true},
		{name: "the grace is off", grace: 0, listChanged: true},
		{name: "the server sends no notifications", grace: grace, listChanged: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Remote{notified: make(chan struct{}), grace: tc.grace, listChanged: tc.listChanged}
			if tc.notify {
				time.AfterFunc(5*time.Millisecond, func() {
					s.seen.Add(1)
					s.mu.Lock()
					close(s.notified)
					s.notified = make(chan struct{})
					s.mu.Unlock()
				})
			}
			start := time.Now()
			s.awaitNotification(context.Background(), 0)
			if waited := time.Since(start); (waited >= grace) != tc.wantWait {
				t.Errorf("waited %v, want the whole grace = %v", waited, tc.wantWait)
			}
		})
	}

	// A notification already counted needs no waiting at all.
	s := &Remote{notified: make(chan struct{}), grace: grace, listChanged: true}
	s.seen.Add(1)
	start := time.Now()
	s.awaitNotification(context.Background(), 0)
	if waited := time.Since(start); waited >= grace {
		t.Errorf("waited %v for a notification already seen", waited)
	}

	// A cancelled context ends the wait with the call, not the grace.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start = time.Now()
	(&Remote{notified: make(chan struct{}), grace: grace, listChanged: true}).awaitNotification(ctx, 0)
	if waited := time.Since(start); waited >= grace {
		t.Errorf("waited %v after the context ended", waited)
	}
}

// TestReadOnlyCallDoesNotWait: the wait is for a call that might have
// changed the list, so a tool the server marked read-only skips it and
// the forty read-only tools of a server cost nothing.
func TestReadOnlyCallDoesNotWait(t *testing.T) {
	const grace = 500 * time.Millisecond
	server := sdk.NewServer(&sdk.Implementation{Name: "annotated", Version: "1"}, nil)
	server.AddTool(&sdk.Tool{
		Name:        "search",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: ptr(false), OpenWorldHint: ptr(false)},
	}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "found"}}}, nil
	})
	server.AddTool(&sdk.Tool{Name: "write", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "written"}}}, nil
		})
	s := connect(t, server, WithNotificationGrace(grace))

	start := time.Now()
	if _, err := lookup(t, s, "search").Execute(context.Background(), agenttool.Call{ID: "c", Args: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if waited := time.Since(start); waited >= grace/2 {
		t.Errorf("a read-only call waited %v", waited)
	}

	// A tool that says nothing waits, because it may have changed the list.
	start = time.Now()
	if _, err := lookup(t, s, "write").Execute(context.Background(), agenttool.Call{ID: "c", Args: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if waited := time.Since(start); waited < grace/2 {
		t.Errorf("an unannotated call waited %v, want the grace", waited)
	}
}
