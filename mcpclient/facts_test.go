package mcpclient

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agenttool"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestToolMetaOf(t *testing.T) {
	for _, tc := range []struct {
		name string
		meta sdk.Meta
		want ToolMeta
		ok   bool
	}{
		{name: "none"},
		{name: "other keys", meta: sdk.Meta{"x": 1}},
		{name: "all", meta: sdk.Meta{ToolMetaKey: map[string]any{"facts": true, "replay": true, "readOnly": true, "sequential": true, "resource": "shell:1"}},
			want: ToolMeta{Facts: true, Replay: true, ReadOnly: true, Sequential: true, Resource: "shell:1"}, ok: true},
		{name: "wrong types read as unset", meta: sdk.Meta{ToolMetaKey: map[string]any{"facts": "yes", "sequential": 1, "resource": true}}, ok: true},
		{name: "not an object", meta: sdk.Meta{ToolMetaKey: "facts"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ToolMetaOf(&sdk.Tool{Name: "x", Meta: tc.meta})
			if got != tc.want || ok != tc.ok {
				t.Errorf("got %+v, %v; want %+v, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

type wireParams struct {
	sdk.ParamsBase
	Calls []struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"calls"`
}

type wireResult struct {
	sdk.ResultBase
	Results []json.RawMessage `json:"results"`
}

// factsServer is an SDK server, not mcpserver, that advertises the
// capability and answers every call with answer, or with one answer too
// few when short, so the client is held to the wire rather than to the
// other module.
func factsServer(t *testing.T, answer string, short bool) *sdk.Server {
	t.Helper()
	s := sdk.NewServer(&sdk.Implementation{Name: "facts", Version: "1"}, &sdk.ServerOptions{
		Capabilities: &sdk.ServerCapabilities{Experimental: map[string]any{FactsCapability: map[string]any{}}},
	})
	s.AddTool(&sdk.Tool{Name: "sh", InputSchema: json.RawMessage(`{"type":"object"}`), Meta: sdk.Meta{ToolMetaKey: map[string]any{"facts": true, "replay": true}}},
		func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			t.Error("a tool ran")
			return &sdk.CallToolResult{}, nil
		})
	err := sdk.AddReceivingCustomMethod(s, FactsMethod, func(_ context.Context, _ *sdk.ServerSession, p *wireParams) (*wireResult, error) {
		res := &wireResult{}
		for _, c := range p.Calls {
			if c.Name != "sh" || string(c.Arguments) != `{"command":"ls"}` {
				t.Errorf("asked about %s %s", c.Name, c.Arguments)
			}
			res.Results = append(res.Results, json.RawMessage(answer))
		}
		if short {
			res.Results = res.Results[1:]
		}
		return res, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestFactsFromAnyServer(t *testing.T) {
	s := connect(t, factsServer(t, `{"facts":{"calls":[{"tool":"read","args":{"path":"a"}}],"rewrite":null},"replay":"keyed"}`, false))
	tl := lookup(t, s, "sh")
	args := json.RawMessage(`{"command":"ls"}`)
	f, claims, err := agenttool.FactsOf(context.Background(), tl, args)
	if err != nil || !claims || len(f.Calls) != 1 || f.Calls[0].Tool != "read" || string(f.Calls[0].Args) != `{"path":"a"}` {
		t.Errorf("facts %+v, claims %v, err %v", f, claims, err)
	}
	if f.Rewrite != nil {
		t.Errorf("a null rewrite read as %s", f.Rewrite)
	}
	// keyed needs the idempotency key, which this client does not send.
	if r := agenttool.ReplayOf(context.Background(), tl, args); r != agenttool.ReplayUnknown {
		t.Errorf("replay %v, want unknown", r)
	}
}

func TestFactsShortAnswer(t *testing.T) {
	s := connect(t, factsServer(t, `{"replay":"safe"}`, true))
	_, err := s.Facts(context.Background(), FactsCall{Name: "sh", Args: json.RawMessage(`{"command":"ls"}`)})
	if err == nil || !strings.Contains(err.Error(), "0 answers for 1 calls") {
		t.Errorf("err = %v", err)
	}
	// The tool's own claim fails the same way, and replay reads unknown.
	tl := lookup(t, s, "sh")
	if _, _, err := agenttool.FactsOf(context.Background(), tl, json.RawMessage(`{"command":"ls"}`)); err == nil {
		t.Error("a short answer read as a claim")
	}
	if r := agenttool.ReplayOf(context.Background(), tl, json.RawMessage(`{"command":"ls"}`)); r != agenttool.ReplayUnknown {
		t.Errorf("replay %v, want unknown", r)
	}
}
