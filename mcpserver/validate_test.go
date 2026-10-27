package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
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

// TestSchemaDialects: a draft-07 or 2020-12 $schema is validated
// however its URI is spelt, and a schema of a dialect the validator does
// not handle is served without validation, which the host is told of,
// rather than refused: the tool's schema is sound, and its own decoding
// still guards it.
func TestSchemaDialects(t *testing.T) {
	tool := func(name, dialect string) agenttool.Tool {
		return agenttool.NewFunc(name, "", json.RawMessage(`{"$schema":"`+dialect+`","type":"object","properties":{"n":{"type":"integer"}}}`),
			func(context.Context, agenttool.Call) (agenttool.Result, error) { return agenttool.Text("ran"), nil })
	}
	var logged bytes.Buffer
	server, err := Options{Logger: slog.New(slog.NewTextHandler(&logged, nil))}.NewServer("t", "0",
		tool("short", "http://json-schema.org/draft-07/schema"),
		tool("hashed", "https://json-schema.org/draft/2020-12/schema#"),
		tool("other", "https://json-schema.org/draft/2019-09/schema"))
	if err != nil {
		t.Fatal(err)
	}
	tools := agenttool.Set(roundTrip(t, server).Tools())
	for name, wantErr := range map[string]bool{"short": true, "hashed": true, "other": false} {
		tl, _ := tools.Lookup(name)
		_, err := tl.Execute(context.Background(), agenttool.Call{ID: "1", Args: json.RawMessage(`{"n":"five"}`)})
		if wantErr != (err != nil) {
			t.Errorf("%s: err = %v, want validated %v", name, err, wantErr)
		}
	}
	if got := logged.String(); !strings.Contains(got, "other") || !strings.Contains(got, "2019-09") || strings.Contains(got, "short") {
		t.Errorf("logged %q, want the unvalidated tool and why", got)
	}
}

func sdkServer() *sdk.Server {
	return sdk.NewServer(&sdk.Implementation{Name: "t", Version: "0"}, nil)
}
