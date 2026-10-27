package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/openresponses"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// resourceServer offers two text resources, a blob and a template, a
// page of two at a time, and a tool that links to one.
func resourceServer(t *testing.T, tools ...string) *sdk.Server {
	t.Helper()
	server := sdk.NewServer(&sdk.Implementation{Name: "logs", Version: "1"}, &sdk.ServerOptions{PageSize: 2})
	text := func(uri, mime, body string) sdk.ResourceHandler {
		return func(context.Context, *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
			return &sdk.ReadResourceResult{Contents: []*sdk.ResourceContents{{URI: uri, MIMEType: mime, Text: body}}}, nil
		}
	}
	server.AddResource(&sdk.Resource{URI: "file:///big.log", Name: "big.log", Description: "the server log", MIMEType: "text/plain", Size: 4096}, text("file:///big.log", "text/plain", "line 1"))
	server.AddResource(&sdk.Resource{URI: "file:///notes.md", Name: "notes.md", MIMEType: "text/markdown"}, text("file:///notes.md", "text/markdown", "# notes"))
	server.AddResource(&sdk.Resource{URI: "file:///shot.png", Name: "shot.png", MIMEType: "image/png"},
		func(context.Context, *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
			return &sdk.ReadResourceResult{Contents: []*sdk.ResourceContents{{URI: "file:///shot.png", MIMEType: "image/png", Blob: []byte{1, 2, 3}}}}, nil
		})
	server.AddResourceTemplate(&sdk.ResourceTemplate{URITemplate: "file:///logs/{day}.log", Name: "day", Description: "one day's log"},
		func(context.Context, *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) { return nil, nil })
	server.AddTool(&sdk.Tool{Name: "search", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.ResourceLink{URI: "file:///big.log", Name: "big.log"}}}, nil
		})
	for _, name := range tools {
		server.AddTool(&sdk.Tool{Name: name, InputSchema: json.RawMessage(`{"type":"object"}`)},
			func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
				return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "the server's own"}}}, nil
			})
	}
	return server
}

// TestResourceTools: a server's resources as two tools, defined word
// for word as RFC 0001 states, that list them and read one, so the
// model can follow a link a tool returned (#94).
func TestResourceTools(t *testing.T) {
	s := connect(t, resourceServer(t), WithPrefix("logs"))
	tools := s.ResourceTools()
	if got := names(tools); strings.Join(got, ",") != "logs__list_resources,logs__read_resource" {
		t.Fatalf("resource tools = %v", got)
	}
	// The canonical definitions RFC 0001 gives, under the prefix.
	want := []string{
		`{"description":"List the resources this server offers, a page at a time, and the URI templates it reads others by. Read one with read_resource.","name":"logs__list_resources","parameters":{"properties":{"cursor":{"description":"The cursor the previous page ended with, to list the page after it.","type":"string"}},"required":[],"type":"object"},"type":"function"}`,
		`{"description":"Read a resource this server offers, by the URI list_resources or a resource_link gives, or one a URI template from list_resources makes.","name":"logs__read_resource","parameters":{"properties":{"uri":{"description":"The resource's URI.","type":"string"}},"required":["uri"],"type":"object"},"type":"function"}`,
	}
	for i, tl := range tools {
		got, err := json.Marshal(agenttool.Definition(tl))
		if err != nil {
			t.Fatal(err)
		}
		if normal(t, got) != normal(t, []byte(want[i])) {
			t.Errorf("definition %d = %s\n want %s", i, got, want[i])
		}
		if a := agenttool.AnnotationsOf(tl); a != resourceAnnotations || !a.ReadOnly || a.Destructive {
			t.Errorf("%s annotations = %+v", tl.Name(), a)
		}
	}

	ctx := context.Background()
	run := func(tl agenttool.Tool, args string) agenttool.Result {
		t.Helper()
		res, err := tl.Execute(ctx, agenttool.Call{ID: "c", Args: json.RawMessage(args)})
		if err != nil {
			t.Fatalf("%s %s: %v", tl.Name(), args, err)
		}
		return res
	}
	first := run(tools[0], `{}`)
	wantFirst := "resource_link file:///big.log: big.log, the server log, text/plain, 4096 bytes\n" +
		"resource_link file:///notes.md: notes.md, text/markdown\n" +
		"resource_template file:///logs/{day}.log: day, one day's log\n" +
		"next_cursor "
	if !strings.HasPrefix(first.Output.Text, wantFirst) {
		t.Fatalf("first page = %q, want it to begin %q", first.Output.Text, wantFirst)
	}
	cursor := strings.TrimPrefix(first.Output.Text, wantFirst)
	args, _ := json.Marshal(map[string]string{"cursor": cursor})
	if second := run(tools[0], string(args)); second.Output.Text != "resource_link file:///shot.png: shot.png, image/png" {
		t.Errorf("second page = %q", second.Output.Text)
	}

	if got := run(tools[1], `{"uri":"file:///big.log"}`); got.Output.Text != "resource file:///big.log (text/plain)\nline 1" {
		t.Errorf("read text = %q", got.Output.Text)
	}
	shot := run(tools[1], `{"uri":"file:///shot.png"}`)
	if len(shot.Output.Parts) != 1 {
		t.Fatalf("read blob = %+v", shot.Output)
	}
	if img, ok := shot.Output.Parts[0].(*openresponses.InputImage); !ok || img.ImageURL != "data:image/png;base64,AQID" {
		t.Errorf("read blob = %+v", shot.Output.Parts[0])
	}
	if _, err := tools[1].Execute(ctx, agenttool.Call{ID: "c", Args: json.RawMessage(`{}`)}); err == nil || !strings.HasPrefix(err.Error(), "invalid arguments:") {
		t.Errorf("read without uri = %v", err)
	}
}

// TestResourceToolsYieldToServerTools: a server tool of the same name
// wins, and a server without resources has no resource tools.
func TestResourceToolsYieldToServerTools(t *testing.T) {
	s := connect(t, resourceServer(t, "read_resource"))
	if got := names(s.ResourceTools()); strings.Join(got, ",") != "list_resources" {
		t.Errorf("resource tools = %v, want list_resources alone", got)
	}
	if res, err := lookup(t, s, "read_resource").Execute(context.Background(), agenttool.Call{ID: "c", Args: json.RawMessage(`{}`)}); err != nil || res.Output.Text != "the server's own" {
		t.Errorf("read_resource = %+v, %v; want the server's tool", res.Output, err)
	}
	plain := connect(t, sdk.NewServer(&sdk.Implementation{Name: "plain", Version: "1"}, nil))
	if tools := plain.ResourceTools(); tools != nil {
		t.Errorf("resource tools of a server without resources = %v", names(tools))
	}
}

// TestResourceDefinitionsMatchTheRFC holds the definitions to the ones
// RFC 0001 states word for word, so the two cannot drift apart. The RFC
// is outside the module, so the test is skipped where the module is
// built alone.
func TestResourceDefinitionsMatchTheRFC(t *testing.T) {
	rfc, err := os.ReadFile(filepath.Join("..", "docs", "rfcs", "0001-tool-contract.md"))
	if err != nil {
		t.Skipf("RFC 0001 not beside the module: %v", err)
	}
	_, section, _ := strings.Cut(string(rfc), "\n#### Resources\n")
	_, block, _ := strings.Cut(section, "```json\n")
	block, _, _ = strings.Cut(block, "```")
	lines := strings.Split(strings.TrimSpace(block), "\n")
	tools := connect(t, resourceServer(t)).ResourceTools()
	if len(lines) != len(tools) {
		t.Fatalf("the RFC defines %d resource tools, the binding offers %d", len(lines), len(tools))
	}
	for i, tl := range tools {
		got, err := json.Marshal(agenttool.Definition(tl))
		if err != nil {
			t.Fatal(err)
		}
		if normal(t, got) != normal(t, []byte(lines[i])) {
			t.Errorf("definition %d = %s\n   the RFC says %s", i, got, lines[i])
		}
	}
}

// templateServer offers one resource and answers resources/templates/list
// with answer, counting the requests.
func templateServer(n *atomic.Int64, answer func(cursor string) (*sdk.ListResourceTemplatesResult, error)) *sdk.Server {
	server := sdk.NewServer(&sdk.Implementation{Name: "tpl", Version: "1"}, nil)
	server.AddResource(&sdk.Resource{URI: "file:///a", Name: "a"}, func(context.Context, *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
		return &sdk.ReadResourceResult{}, nil
	})
	server.AddReceivingMiddleware(func(next sdk.MethodHandler) sdk.MethodHandler {
		return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
			if method != "resources/templates/list" {
				return next(ctx, method, req)
			}
			n.Add(1)
			cursor := ""
			if p, ok := req.GetParams().(*sdk.ListResourceTemplatesParams); ok && p != nil {
				cursor = p.Cursor
			}
			return answer(cursor)
		}
	})
	return server
}

func toolNamed(t *testing.T, tools []agenttool.Tool, name string) agenttool.Tool {
	t.Helper()
	for _, tl := range tools {
		if tl.Name() == name {
			return tl
		}
	}
	t.Fatalf("no %s in %v", name, names(tools))
	return nil
}

// TestListResourcesTemplatePages: the templates' listing ends on a
// repeated cursor and after a cap of pages, an error on a later page or
// the end of the call is the call's error and never a partial listing,
// and only a refusal of the first page lists no templates.
func TestListResourcesTemplatePages(t *testing.T) {
	refused := &jsonrpc.Error{Code: jsonrpc.CodeMethodNotFound, Message: "no templates"}
	one := []*sdk.ResourceTemplate{{URITemplate: "x://{a}", Name: "t"}}
	cases := []struct {
		name    string
		answer  func(cursor string) (*sdk.ListResourceTemplatesResult, error)
		want    string // the output, or
		wantErr string // a part of the error
		most    int64  // requests at most
	}{
		{"repeated cursor", func(string) (*sdk.ListResourceTemplatesResult, error) {
			return &sdk.ListResourceTemplatesResult{NextCursor: "same", ResourceTemplates: one}, nil
		}, "", "repeated", 3},
		{"endless pages", func(c string) (*sdk.ListResourceTemplatesResult, error) {
			return &sdk.ListResourceTemplatesResult{NextCursor: c + "x", ResourceTemplates: one}, nil
		}, "", "pages", maxTemplatePages + 1},
		{"later page fails", func(c string) (*sdk.ListResourceTemplatesResult, error) {
			if c == "" {
				return &sdk.ListResourceTemplatesResult{NextCursor: "2", ResourceTemplates: one}, nil
			}
			return nil, refused
		}, "", "method not found", 2},
		{"first page refused", func(string) (*sdk.ListResourceTemplatesResult, error) { return nil, refused },
			"resource_link file:///a: a", "", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var n atomic.Int64
			s := connect(t, templateServer(&n, tc.answer))
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			res, err := toolNamed(t, s.ResourceTools(), "list_resources").Execute(ctx, agenttool.Call{ID: "c", Args: json.RawMessage(`{}`)})
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("err = %v, output %q; want an error naming %q", err, res.Output.Text, tc.wantErr)
				}
			} else if err != nil || res.Output.Text != tc.want {
				t.Errorf("output = %q, %v; want %q", res.Output.Text, err, tc.want)
			}
			if got := n.Load(); got > tc.most {
				t.Errorf("%d templates requests, want at most %d", got, tc.most)
			}
		})
	}
}

// TestResourceToolsUnderCallTimeout: the server's call limits bound its
// resource tools too, since they are that server's tools.
func TestResourceToolsUnderCallTimeout(t *testing.T) {
	server := sdk.NewServer(&sdk.Implementation{Name: "hang", Version: "1"}, nil)
	server.AddResource(&sdk.Resource{URI: "file:///slow", Name: "slow"}, func(ctx context.Context, _ *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	s := connect(t, server, WithPrefix("h"), WithCallTimeout(100*time.Millisecond, 0))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := toolNamed(t, s.ResourceTools(), "h__read_resource").Execute(ctx, agenttool.Call{ID: "c", Args: json.RawMessage(`{"uri":"file:///slow"}`)})
	var te *TimeoutError
	if !errors.As(err, &te) || !te.Idle {
		t.Errorf("err = %v, want the server's idle limit", err)
	}
	// A tool's own limit names it by its resource tool's name.
	s = connect(t, server, WithCallTimeout(0, 0), WithCallTimeout(0, 100*time.Millisecond, "read_resource"))
	_, err = toolNamed(t, s.ResourceTools(), "read_resource").Execute(ctx, agenttool.Call{ID: "c", Args: json.RawMessage(`{"uri":"file:///slow"}`)})
	if !errors.As(err, &te) || te.Idle {
		t.Errorf("err = %v, want read_resource's own maximum", err)
	}
}
