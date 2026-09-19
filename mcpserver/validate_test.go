package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agenttool"
)

func TestArgumentsValidatedAgainstSchema(t *testing.T) {
	s := roundTrip(t, NewServer("t", "0", fixtures...))
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

func TestUnresolvableSchemaIsNotValidated(t *testing.T) {
	odd := &agenttool.Func{ToolName: "odd", Schema: json.RawMessage(`{"$schema":"http://example.com/unknown","type":"object"}`),
		Fn: func(_ context.Context, c agenttool.Call) (agenttool.Result, error) {
			return agenttool.Text(string(c.Args)), nil
		}}
	s := roundTrip(t, NewServer("t", "0", odd))
	tl, _ := agenttool.Set(s.Tools()).Lookup("odd")
	res, err := tl.Execute(context.Background(), agenttool.Call{ID: "1", Args: json.RawMessage(`{"x":1}`)})
	if err != nil || res.Output.Text != `{"x":1}` {
		t.Errorf("res = %+v err = %v", res, err)
	}
}
