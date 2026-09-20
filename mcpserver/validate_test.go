package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agenttool"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestArgumentsValidatedAgainstSchema(t *testing.T) {
	s := roundTrip(t, newServer(t, "t", fixtures...))
	tools := agenttool.Set(s.Tools())
	cases := []struct {
		name    string
		tool    string
		args    string
		wantErr string
	}{
		{"missing required", "stats", `{}`, "required"},
		{"wrong type", "upper", `{"text": 1}`, "invalid arguments"},
		{"extra property under strict", "stats", `{"text":"a","more":true}`, "additional"},
		{"extra property allowed when not strict", "upper", `{"text":"a","more":true}`, ""},
		{"nil arguments as empty object", "picture", ``, ""},
		{"no schema accepts anything", "bare", `{"anything":[1,2]}`, ""},
		{"valid strict call", "stats", `{"text":"ABC"}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tl, ok := tools.Lookup(tc.tool)
			if !ok {
				t.Fatalf("tool %q missing", tc.tool)
			}
			_, err := tl.Execute(context.Background(), agenttool.Call{ID: "1", Args: json.RawMessage(tc.args)})
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestUnresolvableSchemaIsRefused(t *testing.T) {
	cases := []struct {
		name    string
		schema  string
		wantErr string
	}{
		{"unknown draft", `{"$schema":"http://example.com/unknown","type":"object"}`, "unsupported $schema"},
		{"not a schema", `{"type":5}`, "schema"},
		{"unresolvable ref", `{"type":"object","properties":{"a":{"$ref":"#/$defs/missing"}}}`, "schema"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			odd := agenttool.NewFunc("odd", "", json.RawMessage(tc.schema),
				func(context.Context, agenttool.Call) (agenttool.Result, error) { return agenttool.Text("ran"), nil })
			if _, err := Handler(odd); err == nil || !strings.Contains(err.Error(), `tool "odd"`) || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Handler err = %v, want tool name and %q", err, tc.wantErr)
			}
			s, err := NewServer("t", "0", fixtures[0], odd)
			if err == nil || s != nil {
				t.Errorf("NewServer = %v, %v; want nil server and an error", s, err)
			}
			// Nothing is registered when any tool fails.
			srv := sdkServer()
			if err := AddTools(srv, fixtures[0], odd); err == nil {
				t.Fatal("AddTools should fail")
			}
			rt := roundTrip(t, srv)
			if n := len(rt.Tools()); n != 0 {
				t.Errorf("%d tools registered after a failed AddTools, want 0", n)
			}
		})
	}
}

func sdkServer() *sdk.Server {
	return sdk.NewServer(&sdk.Implementation{Name: "t", Version: "0"}, nil)
}
