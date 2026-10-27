package mcpclient

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/ChristopherDavenport/agenttool"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestFlowOfConsumedTools: MCP's open-world hint, true or absent, reads
// as untrusted content that is sent on; false says nothing; and a
// server's own flow is taken as stated only from a server whose claims
// the host takes, and otherwise only where it is stricter (#92).
func TestFlowOfConsumedTools(t *testing.T) {
	no, yes := false, true
	server := sdk.NewServer(&sdk.Implementation{Name: "mail", Version: "1"}, nil)
	add := func(name string, a *sdk.ToolAnnotations, flow map[string]any) {
		tool := &sdk.Tool{Name: name, InputSchema: json.RawMessage(`{"type":"object"}`), Annotations: a}
		if flow != nil {
			tool.Meta = sdk.Meta{ToolMetaKey: map[string]any{"flow": flow}}
		}
		server.AddTool(tool, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return &sdk.CallToolResult{}, nil
		})
	}
	add("no_block", nil, nil)
	add("hint_absent", &sdk.ToolAnnotations{ReadOnlyHint: true}, nil)
	add("open", &sdk.ToolAnnotations{OpenWorldHint: &yes}, nil)
	add("closed", &sdk.ToolAnnotations{OpenWorldHint: &no}, nil)
	add("closed_private", &sdk.ToolAnnotations{OpenWorldHint: &no}, map[string]any{"reads": []any{"private"}, "sends": false})
	add("open_says_pure", nil, map[string]any{"reads": []any{}, "sends": false})
	add("send_email", nil, map[string]any{"reads": []any{}, "sends": true, "to": []any{"to"}, "carries": []any{"subject", "body"}})

	openWorld := agenttool.Flow{Reads: []agenttool.Label{agenttool.LabelUntrusted}, Sends: true}
	untrusted := map[string]*agenttool.Flow{
		"no_block":       &openWorld,
		"hint_absent":    &openWorld,
		"open":           &openWorld,
		"closed":         nil,
		"closed_private": {Reads: []agenttool.Label{agenttool.LabelPrivate}, Sends: true},
		"open_says_pure": &openWorld,
		"send_email":     &openWorld,
	}
	trusted := map[string]*agenttool.Flow{
		"no_block":       &openWorld,
		"hint_absent":    &openWorld,
		"open":           &openWorld,
		"closed":         nil,
		"closed_private": {Reads: []agenttool.Label{agenttool.LabelPrivate}},
		"open_says_pure": {},
		"send_email":     {Sends: true, To: []string{"to"}, Carries: []string{"subject", "body"}},
	}
	for name, cases := range map[string]struct {
		opts []Option
		want map[string]*agenttool.Flow
	}{"untrusted": {nil, untrusted}, "with claims": {[]Option{WithClaims()}, trusted}} {
		t.Run(name, func(t *testing.T) {
			s := connect(t, server, cases.opts...)
			for tool, want := range cases.want {
				got, ok := agenttool.FlowOf(lookup(t, s, tool))
				switch {
				case want == nil && ok:
					t.Errorf("%s: flow = %+v, want unstated", tool, got)
				case want != nil && (!ok || !sameFlow(got, *want)):
					t.Errorf("%s: flow = %+v, %v; want %+v", tool, got, ok, *want)
				}
			}
		})
	}
}

// sameFlow compares two flows, an empty list and none alike.
func sameFlow(a, b agenttool.Flow) bool {
	norm := func(f agenttool.Flow) agenttool.Flow {
		if len(f.Reads) == 0 {
			f.Reads = nil
		}
		if len(f.To) == 0 {
			f.To = nil
		}
		if len(f.Carries) == 0 {
			f.Carries = nil
		}
		return f
	}
	return reflect.DeepEqual(norm(a), norm(b))
}
