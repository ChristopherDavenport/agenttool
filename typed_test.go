package agenttool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/openresponses"
)

func TestNewOutputs(t *testing.T) {
	type args struct {
		X int `json:"x"`
	}
	type out struct {
		Doubled int `json:"doubled"`
	}
	cases := []struct {
		name     string
		tool     Tool
		args     string
		wantText string
		wantPart bool
		wantErr  string
	}{
		{
			name:     "string passes through",
			tool:     New("s", "", func(_ context.Context, a args) (string, error) { return strings.Repeat("x", a.X), nil }),
			args:     `{"x":3}`,
			wantText: "xxx",
		},
		{
			name: "struct marshals to JSON",
			tool: New("j", "", func(_ context.Context, a args) (out, error) { return out{a.X * 2}, nil }),
			args: `{"x":2}`, wantText: `{"doubled":4}`,
		},
		{
			name: "contents become parts",
			tool: New("p", "", func(context.Context, NoArgs) (openresponses.Contents, error) {
				return openresponses.Contents{&openresponses.InputImage{ImageURL: "data:image/png;base64,AA=="}}, nil
			}),
			args: ``, wantPart: true,
		},
		{
			name: "result passes through",
			tool: New("r", "", func(context.Context, NoArgs) (Result, error) {
				return Result{Output: openresponses.FunctionCallOutputData{Text: "done"}, Terminate: true, Details: 7}, nil
			}),
			args: `{}`, wantText: "done",
		},
		{
			name:    "function error is returned",
			tool:    New("e", "", func(context.Context, args) (string, error) { return "", errors.New("boom") }),
			args:    `{"x":1}`,
			wantErr: "boom",
		},
		{
			name:    "bad arguments are an error",
			tool:    New("b", "", func(context.Context, args) (string, error) { return "", nil }),
			args:    `{"x":"not a number"}`,
			wantErr: "invalid arguments",
		},
		{
			name:     "unknown fields are ignored",
			tool:     New("u", "", func(_ context.Context, a args) (string, error) { return "ok", nil }),
			args:     `{"x":1,"extra":true}`,
			wantText: "ok",
		},
		{
			name:     "nil pointer result is empty",
			tool:     New("n", "", func(context.Context, NoArgs) (*out, error) { return nil, nil }),
			args:     `{}`,
			wantText: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := tc.tool.Execute(context.Background(), Call{ID: "c", Args: json.RawMessage(tc.args)})
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantPart {
				if len(res.Output.Parts) != 1 {
					t.Fatalf("parts = %v", res.Output.Parts)
				}
				return
			}
			if res.Output.Text != tc.wantText {
				t.Errorf("text = %q, want %q", res.Output.Text, tc.wantText)
			}
			if tc.name == "result passes through" && (!res.Terminate || res.Details != 7) {
				t.Errorf("result not passed through: %+v", res)
			}
		})
	}
}

func TestNewOptionsAndDefinition(t *testing.T) {
	plain := New("plain", "d", func(context.Context, readFileArgs) (string, error) { return "", nil })
	strict := New("strict", "d", func(context.Context, readFileArgs) (string, error) { return "", nil }, WithStrict(), WithSequential())
	custom := New("custom", "d", func(context.Context, readFileArgs) (string, error) { return "", nil }, WithParameters(json.RawMessage(`{"type":"object"}`)))

	if IsStrict(plain) || IsSequential(plain) {
		t.Error("plain tool should be neither strict nor sequential")
	}
	if !IsStrict(strict) || !IsSequential(strict) {
		t.Error("options not applied")
	}
	if string(custom.Parameters()) != `{"type":"object"}` {
		t.Errorf("custom schema = %s", custom.Parameters())
	}
	def := Definition(strict)
	if def.Name != "strict" || def.Description != "d" || def.Strict == nil || !*def.Strict {
		t.Errorf("definition = %+v", def)
	}
	if !strings.Contains(string(def.Parameters), `"additionalProperties":false`) {
		t.Errorf("strict schema missing additionalProperties: %s", def.Parameters)
	}
	if Definition(plain).Strict != nil {
		t.Error("plain definition should not set strict")
	}

	set := Set{plain, strict, custom}
	if err := set.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, ok := set.Lookup("strict"); !ok {
		t.Error("lookup failed")
	}
	if _, ok := set.Lookup("missing"); ok {
		t.Error("lookup found a missing tool")
	}
	if defs := set.Definitions(); len(defs) != 3 {
		t.Errorf("definitions = %d", len(defs))
	}
	if err := (Set{plain, plain}).Validate(); err == nil {
		t.Error("duplicate names accepted")
	}
	if err := (Set{NewFunc("", "", nil, echo)}).Validate(); err == nil {
		t.Error("empty name accepted")
	}
	if Set(nil).Definitions() != nil {
		t.Error("empty set should give nil definitions")
	}
}

func TestCallFromAndProgress(t *testing.T) {
	var updates []string
	var gotID string
	tl := New("p", "", func(ctx context.Context, _ NoArgs) (string, error) {
		c, ok := CallFrom(ctx)
		if !ok {
			t.Error("no call on context")
		}
		gotID = c.ID
		Progress(ctx, Text("half"))
		return "done", nil
	})
	_, err := tl.Execute(context.Background(), Call{ID: "call_1", OnUpdate: func(r Result) { updates = append(updates, r.Output.Text) }})
	if err != nil {
		t.Fatal(err)
	}
	if gotID != "call_1" || len(updates) != 1 || updates[0] != "half" {
		t.Errorf("id = %q, updates = %v", gotID, updates)
	}
	// Progress without a call is a no-op.
	Progress(context.Background(), Text("x"))
}

// echo is a raw tool function that returns its arguments as text.
func echo(_ context.Context, c Call) (Result, error) { return Text(string(c.Args)), nil }

func TestNewFunc(t *testing.T) {
	schema := json.RawMessage(`{"type":"object"}`)
	f := NewFunc("f", "d", schema, echo, WithSequential())
	if f.Name() != "f" || f.Description() != "d" || string(f.Parameters()) != string(schema) {
		t.Errorf("tool = %q %q %s", f.Name(), f.Description(), f.Parameters())
	}
	// The schema is served verbatim and never validated.
	res, err := f.Execute(context.Background(), Call{Args: json.RawMessage(`{"a":1}`)})
	if err != nil || res.Output.Text != `{"a":1}` {
		t.Errorf("res = %+v, err = %v", res, err)
	}
	if !IsSequential(f) || IsStrict(f) {
		t.Error("WithSequential not applied, or strict set")
	}
	if strict := NewFunc("s", "", nil, echo, WithStrict()); !IsStrict(strict) || IsSequential(strict) {
		t.Error("WithStrict not applied, or sequential set")
	}
	if NewFunc("n", "", nil, echo).Parameters() != nil {
		t.Error("nil parameters should stay nil")
	}
	func() {
		defer func() {
			if r := recover(); r == nil || !strings.Contains(fmt.Sprint(r), `agenttool.NewFunc("nofn")`) {
				t.Errorf("nil function panic = %v", r)
			}
		}()
		NewFunc("nofn", "", nil, nil)
	}()
	if ErrorResult(errors.New("x")).Output.Text != "Error: x" {
		t.Error("error result format")
	}
}
