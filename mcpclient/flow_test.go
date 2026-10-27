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
	add("closed_says_pure", &sdk.ToolAnnotations{OpenWorldHint: &no}, map[string]any{"reads": []any{}, "sends": false})
	add("open_says_pure", nil, map[string]any{"reads": []any{}, "sends": false})
	add("send_email", nil, map[string]any{"reads": []any{}, "sends": true, "to": []any{"to"}, "carries": []any{"subject", "body"}})

	openWorld := agenttool.Flow{Reads: []agenttool.Label{agenttool.LabelUntrusted}, Sends: true}
	untrusted := map[string]*agenttool.Flow{
		"no_block":    &openWorld,
		"hint_absent": &openWorld,
		"open":        &openWorld,
		"closed":      nil,
		// An untrusted server's flow only adds to the open-world reading:
		// it never states less than that, and saying nothing more leaves
		// the tool as its hint left it.
		"closed_private":   {Reads: []agenttool.Label{agenttool.LabelUntrusted, agenttool.LabelPrivate}, Sends: true},
		"closed_says_pure": nil,
		"open_says_pure":   &openWorld,
		"send_email":       &openWorld,
	}
	trusted := map[string]*agenttool.Flow{
		"no_block":         &openWorld,
		"hint_absent":      &openWorld,
		"open":             &openWorld,
		"closed":           nil,
		"closed_private":   {Reads: []agenttool.Label{agenttool.LabelPrivate}},
		"closed_says_pure": {},
		"open_says_pure":   {},
		"send_email":       {Sends: true, To: []string{"to"}, Carries: []string{"subject", "body"}},
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

// TestHostFlowWins: a host's declaration of a consumed tool's flow wins
// over the server's open-world hint and over its _meta, with or without
// WithClaims, since the host is the one that knows what the tool reaches.
func TestHostFlowWins(t *testing.T) {
	server := sdk.NewServer(&sdk.Implementation{Name: "mail", Version: "1"}, nil)
	add := func(name string, flow map[string]any) {
		tool := &sdk.Tool{Name: name, InputSchema: json.RawMessage(`{"type":"object"}`)}
		if flow != nil {
			tool.Meta = sdk.Meta{ToolMetaKey: map[string]any{"flow": flow}}
		}
		server.AddTool(tool, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return &sdk.CallToolResult{}, nil
		})
	}
	add("read_email", nil)
	add("send_email", map[string]any{"reads": []any{}, "sends": false})
	server.AddResource(&sdk.Resource{URI: "file:///a", Name: "a"}, func(context.Context, *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
		return &sdk.ReadResourceResult{}, nil
	})
	inbox := agenttool.Flow{Reads: []agenttool.Label{agenttool.LabelUntrusted, agenttool.LabelPrivate}}
	mail := agenttool.Flow{Sends: true, To: []string{"to"}, Carries: []string{"body"}}
	local := agenttool.Flow{}
	for name, opts := range map[string][]Option{"untrusted": nil, "with claims": {WithClaims()}} {
		t.Run(name, func(t *testing.T) {
			s := connect(t, server, append(opts, WithPrefix("m"),
				WithFlow(inbox, "read_email"), WithFlow(agenttool.Flow{}, "m__send_email"), WithFlow(mail, "send_email"),
				WithFlow(local, "read_resource"))...)
			for tool, want := range map[string]agenttool.Flow{"m__read_email": inbox, "m__send_email": mail} {
				if got, ok := agenttool.FlowOf(lookup(t, s, tool)); !ok || !sameFlow(got, want) {
					t.Errorf("%s: flow = %+v, %v; want the host's %+v", tool, got, ok, want)
				}
			}
			flows := map[string]agenttool.Flow{}
			for _, tl := range s.ResourceTools() {
				f, ok := agenttool.FlowOf(tl)
				if !ok {
					t.Errorf("%s states no flow", tl.Name())
				}
				flows[tl.Name()] = f
			}
			open := agenttool.Flow{Reads: []agenttool.Label{agenttool.LabelUntrusted}, Sends: true}
			if !sameFlow(flows["m__list_resources"], open) || !sameFlow(flows["m__read_resource"], local) {
				t.Errorf("resource tools' flows = %+v; want list_resources open-world and read_resource the host's", flows)
			}
		})
	}
}
