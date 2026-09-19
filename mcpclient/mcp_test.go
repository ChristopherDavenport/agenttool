package mcpclient

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
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

func connect(t *testing.T, server *sdk.Server, opts ...Option) *Server {
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

func lookup(t *testing.T, s *Server, name string) agenttool.Tool {
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

func TestPrefixAndSequential(t *testing.T) {
	cases := []struct {
		name       string
		opts       []Option
		wantName   string
		sequential bool
	}{
		{"no prefix", nil, "upper", false},
		{"prefix", []Option{WithPrefix("fs")}, "fs__upper", false},
		{"sequential by remote name", []Option{WithPrefix("fs"), WithSequential("upper")}, "fs__upper", true},
		{"sequential by local name", []Option{WithPrefix("fs"), WithSequential("fs__upper")}, "fs__upper", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := connect(t, newServer(t), tc.opts...)
			tl := lookup(t, s, tc.wantName)
			if agenttool.IsSequential(tl) != tc.sequential {
				t.Errorf("sequential = %v, want %v", agenttool.IsSequential(tl), tc.sequential)
			}
			if agenttool.IsSequential(lookup(t, s, s.Name("image"))) {
				t.Error("other tools should not be sequential")
			}
		})
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
	var updates []Progress
	res, err := lookup(t, s, "progress").Execute(context.Background(), agenttool.Call{ID: "c", Args: json.RawMessage(`{}`), OnUpdate: func(r agenttool.Result) {
		mu.Lock()
		defer mu.Unlock()
		updates = append(updates, r.Details.(Progress))
		if r.Output.Text != "step" {
			t.Errorf("update text = %q", r.Output.Text)
		}
	}})
	if err != nil || res.Output.Text != "done" {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	// The SDK dispatches notifications asynchronously, so the last update
	// may land after the result.
	got := waitFor(t, func() []Progress {
		mu.Lock()
		defer mu.Unlock()
		return append([]Progress(nil), updates...)
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

func TestResultEdgeCases(t *testing.T) {
	if _, err := Result(nil); err == nil {
		t.Error("nil result should error")
	}
	if _, err := Result(&sdk.CallToolResult{IsError: true}); err == nil || err.Error() != "tool call failed" {
		t.Errorf("empty error result = %v", err)
	}
	res, err := Result(&sdk.CallToolResult{Content: []sdk.Content{&sdk.ImageContent{Data: []byte{1}}}})
	if err != nil || res.Output.Parts[0].(*openresponses.InputImage).ImageURL != "data:application/octet-stream;base64,AQ==" {
		t.Errorf("image without mime = %+v, %v", res, err)
	}
}
