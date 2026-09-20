package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agenttool/mcpclient"
	"github.com/ChristopherDavenport/openresponses"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type textArgs struct {
	Text string `json:"text" desc:"Text to transform"`
}

type stats struct {
	Length int  `json:"length"`
	Upper  bool `json:"upper"`
}

var fixtures = []agenttool.Tool{
	agenttool.New("upper", "uppercase text", func(_ context.Context, a textArgs) (string, error) { return strings.ToUpper(a.Text), nil }),
	agenttool.New("stats", "describe text", func(_ context.Context, a textArgs) (stats, error) {
		return stats{Length: len(a.Text), Upper: strings.ToUpper(a.Text) == a.Text}, nil
	}, agenttool.WithStrict()),
	agenttool.New("picture", "an image", func(context.Context, agenttool.NoArgs) (openresponses.Contents, error) {
		return openresponses.Contents{
			&openresponses.Text{Text: "caption"},
			&openresponses.InputImage{ImageURL: "data:image/png;base64,AQID"},
			&openresponses.InputFile{Filename: "notes.bin", FileData: "CQ=="},
		}, nil
	}),
	agenttool.New("fails", "always fails", func(context.Context, textArgs) (string, error) { return "", errors.New("disk on fire") }),
	agenttool.New("progress", "reports progress", func(ctx context.Context, _ agenttool.NoArgs) (string, error) {
		r := agenttool.Text("half way")
		r.Details = agenttool.ProgressInfo{Progress: 1, Total: 2}
		agenttool.Progress(ctx, r)
		agenttool.Progress(ctx, agenttool.Text("nearly"))
		return "done", nil
	}),
	&agenttool.Func{ToolName: "bare", ToolDescription: "no schema", Fn: func(context.Context, agenttool.Call) (agenttool.Result, error) { return agenttool.Text("bare"), nil }},
}

// roundTrip serves the fixtures over front/mcp and consumes them with
// tools/mcp over in-memory transports.
func roundTrip(t *testing.T, server *sdk.Server, opts ...mcpclient.Option) *mcpclient.Server {
	t.Helper()
	ct, st := sdk.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := server.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	s, err := mcpclient.Connect(ctx, ct, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func normalise(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("bad JSON %s: %v", raw, err)
	}
	out, _ := json.Marshal(v)
	return string(out)
}

func TestRoundTripDefinitions(t *testing.T) {
	s := roundTrip(t, NewServer("fixtures", "1", fixtures...))
	got := agenttool.Set(s.Tools())
	if len(got) != len(fixtures) {
		t.Fatalf("got %d tools, want %d", len(got), len(fixtures))
	}
	for _, local := range fixtures {
		t.Run(local.Name(), func(t *testing.T) {
			rt, ok := got.Lookup(local.Name())
			if !ok {
				t.Fatal("missing")
			}
			if rt.Description() != local.Description() {
				t.Errorf("description = %q, want %q", rt.Description(), local.Description())
			}
			want := local.Parameters()
			if len(want) == 0 {
				want = emptySchema
			}
			if normalise(t, rt.Parameters()) != normalise(t, want) {
				t.Errorf("schema = %s, want %s", rt.Parameters(), want)
			}
			if agenttool.IsStrict(rt) {
				t.Error("strict never survives the wire")
			}
		})
	}
}

func TestRoundTripExecute(t *testing.T) {
	s := roundTrip(t, NewServer("fixtures", "1", fixtures...))
	set := agenttool.Set(s.Tools())
	ctx := context.Background()
	cases := []struct {
		name     string
		tool     string
		args     string
		wantText string
		wantErr  string
		check    func(*testing.T, agenttool.Result)
	}{
		{name: "string", tool: "upper", args: `{"text":"abc"}`, wantText: "ABC"},
		{name: "struct as JSON", tool: "stats", args: `{"text":"ABC"}`, wantText: `{"length":3,"upper":true}`},
		{name: "no schema", tool: "bare", args: `{}`, wantText: "bare"},
		{name: "error", tool: "fails", args: `{"text":"x"}`, wantErr: "disk on fire"},
		{name: "bad arguments are a tool error", tool: "upper", args: `{"text":5}`, wantErr: "invalid arguments"},
		{name: "image and file", tool: "picture", args: `{}`, check: func(t *testing.T, r agenttool.Result) {
			if len(r.Output.Parts) != 3 {
				t.Fatalf("parts = %+v", r.Output.Parts)
			}
			if p, ok := r.Output.Parts[0].(*openresponses.Text); !ok || p.Text != "caption" {
				t.Errorf("part 0 = %+v", r.Output.Parts[0])
			}
			if p, ok := r.Output.Parts[1].(*openresponses.InputImage); !ok || p.ImageURL != "data:image/png;base64,AQID" {
				t.Errorf("part 1 = %+v", r.Output.Parts[1])
			}
			if p, ok := r.Output.Parts[2].(*openresponses.InputFile); !ok || p.FileData != "CQ==" || p.Filename != "file:///notes.bin" {
				t.Errorf("part 2 = %+v", r.Output.Parts[2])
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt, ok := set.Lookup(tc.tool)
			if !ok {
				t.Fatal("missing tool")
			}
			res, err := rt.Execute(ctx, agenttool.Call{ID: "c", Args: json.RawMessage(tc.args)})
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
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

func TestRoundTripProgress(t *testing.T) {
	s := roundTrip(t, NewServer("fixtures", "1", fixtures...))
	rt, _ := agenttool.Set(s.Tools()).Lookup("progress")
	var mu sync.Mutex
	var got []agenttool.ProgressInfo
	res, err := rt.Execute(context.Background(), agenttool.Call{Args: json.RawMessage(`{}`), OnUpdate: func(r agenttool.Result) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, r.Details.(agenttool.ProgressInfo))
	}})
	if err != nil || res.Output.Text != "done" {
		t.Fatalf("res = %+v err = %v", res, err)
	}
	// Notifications are dispatched asynchronously by the SDK and may land
	// after the result.
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("updates = %+v", got)
		}
		time.Sleep(2 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if got[0] != (agenttool.ProgressInfo{Progress: 1, Total: 2, Message: "half way"}) {
		t.Errorf("first update = %+v", got[0])
	}
	if got[1].Message != "nearly" || got[1].Progress != 2 {
		t.Errorf("second update = %+v", got[1])
	}
}

// TestProxiedProgress takes the second hop the README promises: a
// remote server's tool is consumed by mcpclient, served again by this
// package, and consumed once more. The numbers the origin reported must
// arrive intact rather than being replaced by the serving side's own
// counter.
func TestProxiedProgress(t *testing.T) {
	origin := sdk.NewServer(&sdk.Implementation{Name: "origin", Version: "1"}, nil)
	origin.AddTool(&sdk.Tool{Name: "count", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			if token := req.Params.GetProgressToken(); token != nil {
				for _, p := range []sdk.ProgressNotificationParams{
					{Progress: 10, Total: 40, Message: "a quarter"},
					{Progress: 40, Total: 40, Message: "all"},
				} {
					p.ProgressToken = token
					if err := req.Session.NotifyProgress(ctx, &p); err != nil {
						return nil, err
					}
				}
			}
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "counted"}}}, nil
		})
	first := roundTrip(t, origin, mcpclient.WithPrefix("origin"))
	proxy := NewServer("proxy", "1", first.Tools()...)
	second := roundTrip(t, proxy)

	rt, ok := agenttool.Set(second.Tools()).Lookup("origin__count")
	if !ok {
		t.Fatalf("proxied tool missing from %v", second.Tools())
	}
	var mu sync.Mutex
	var got []agenttool.ProgressInfo
	res, err := rt.Execute(context.Background(), agenttool.Call{Args: json.RawMessage(`{}`), OnUpdate: func(r agenttool.Result) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, r.Details.(agenttool.ProgressInfo))
	}})
	if err != nil || res.Output.Text != "counted" {
		t.Fatalf("res = %+v err = %v", res, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("updates = %+v", got)
		}
		time.Sleep(2 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []agenttool.ProgressInfo{
		{Progress: 10, Total: 40, Message: "a quarter"},
		{Progress: 40, Total: 40, Message: "all"},
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("update %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestContentOf(t *testing.T) {
	cases := []struct {
		name string
		out  openresponses.FunctionCallOutputData
		want string
	}{
		{"text", openresponses.FunctionCallOutputData{Text: "hi"}, `[{"type":"text","text":"hi"}]`},
		{"empty text", openresponses.FunctionCallOutputData{}, `[{"type":"text","text":""}]`},
		{"textual parts", openresponses.FunctionCallOutputData{Parts: openresponses.Contents{&openresponses.InputText{Text: "a"}, &openresponses.OutputText{Text: "b"}}},
			`[{"type":"text","text":"a"},{"type":"text","text":"b"}]`},
		{"image url becomes a link", openresponses.FunctionCallOutputData{Parts: openresponses.Contents{&openresponses.InputImage{ImageURL: "https://x/y.png"}}},
			`[{"type":"resource_link","uri":"https://x/y.png","name":"image"}]`},
		{"data url without mime", openresponses.FunctionCallOutputData{Parts: openresponses.Contents{&openresponses.InputImage{ImageURL: "data:;base64,AQ=="}}},
			`[{"type":"image","mimeType":"application/octet-stream","data":"AQ=="}]`},
		{"file url", openresponses.FunctionCallOutputData{Parts: openresponses.Contents{&openresponses.InputFile{Filename: "n", FileURL: "https://x/n"}}},
			`[{"type":"resource_link","uri":"https://x/n","name":"n"}]`},
		{"file name only", openresponses.FunctionCallOutputData{Parts: openresponses.Contents{&openresponses.InputFile{Filename: "n"}}},
			`[{"type":"text","text":"n"}]`},
		{"bad base64 falls back to text", openresponses.FunctionCallOutputData{Parts: openresponses.Contents{&openresponses.InputFile{FileData: "!!"}}},
			`[{"type":"text","text":"!!"}]`},
		{"unknown part as JSON", openresponses.FunctionCallOutputData{Parts: openresponses.Contents{&openresponses.Refusal{Refusal: "no"}}},
			`[{"type":"text","text":"{\"type\":\"refusal\",\"refusal\":\"no\"}"}]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.Marshal(ContentOf(tc.out))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Errorf("content = %s\n want %s", got, tc.want)
			}
		})
	}
}

func TestParseDataURL(t *testing.T) {
	cases := []struct {
		url  string
		mime string
		ok   bool
	}{
		{"data:image/png;base64,AQID", "image/png", true},
		{"data:;base64,AQID", "application/octet-stream", true},
		{"data:text/plain,hello", "", false},
		{"data:image/png;base64,***", "", false},
		{"https://example.com/a.png", "", false},
		{"data:nocomma", "", false},
	}
	for _, tc := range cases {
		mime, _, ok := parseDataURL(tc.url)
		if ok != tc.ok || mime != tc.mime {
			t.Errorf("parseDataURL(%q) = %q, %v; want %q, %v", tc.url, mime, ok, tc.mime, tc.ok)
		}
	}
}
