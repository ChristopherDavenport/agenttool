package agenttool

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type validArgs struct {
	Path   string   `json:"path"`
	Level  string   `json:"level,omitempty" enum:"low,high"`
	Count  *int     `json:"count,omitempty" enum:"1,2"`
	Tags   []string `json:"tags,omitempty"`
	Nested struct {
		Deep bool `json:"deep"`
	} `json:"nested,omitempty"`
	Ratio float64 `json:"ratio,omitempty"`
}

func TestSchemaValidate(t *testing.T) {
	plain, err := Reflect(reflect.TypeFor[validArgs]())
	if err != nil {
		t.Fatal(err)
	}
	strict, err := Reflect(reflect.TypeFor[validArgs](), WithStrict())
	if err != nil {
		t.Fatal(err)
	}
	full := `{"path":"/x","level":"low","count":2,"tags":["a"],"nested":{"deep":true},"ratio":0.5}`
	cases := []struct {
		name   string
		schema *Schema
		args   string
		want   string // substring of the error, or "" for valid
	}{
		{"valid", plain, `{"path":"/x"}`, ""},
		{"valid full", plain, full, ""},
		{"empty is an object", plain, ``, `missing required property "path"`},
		{"missing required", plain, `{"level":"low"}`, `missing required property "path"`},
		{"wrong type", plain, `{"path":1}`, `path: expected string, got 1`},
		{"enum string", plain, `{"path":"/x","level":"mid"}`, `level: expected one of ["low", "high"], got "mid"`},
		{"enum integer", plain, `{"path":"/x","count":3}`, `count: expected one of [1, 2], got 3`},
		{"integer accepts 2.0", plain, `{"path":"/x","count":2.0}`, ""},
		{"integer rejects 1.5", plain, `{"path":"/x","count":1.5}`, `count: expected one of [1, 2], got 1.5`},
		{"array element", plain, `{"path":"/x","tags":["a",2]}`, `tags[1]: expected string, got 2`},
		{"nested", plain, `{"path":"/x","nested":{"deep":"yes"}}`, `nested.deep: expected boolean, got "yes"`},
		{"nested required", plain, `{"path":"/x","nested":{}}`, `nested: missing required property "deep"`},
		{"number", plain, `{"path":"/x","ratio":"high"}`, `ratio: expected number, got "high"`},
		{"extra ignored when not strict", plain, `{"path":"/x","bogus":1}`, ""},
		{"null for non-nullable", plain, `{"path":null}`, `path: expected string, got null`},
		{"not an object", plain, `[1]`, `expected object, got array`},
		{"not json", plain, `{`, `not valid JSON`},
		{"strict requires everything", strict, `{"path":"/x"}`, `missing required property "level"`},
		{"strict nullable pointer", strict, `{"path":"/x","level":"low","count":null,"tags":[],"nested":{"deep":false},"ratio":1}`, ""},
		{"strict rejects extra", strict, `{"path":"/x","level":"low","count":null,"tags":[],"nested":{"deep":false},"ratio":1,"bogus":true}`, `unexpected property "bogus"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.schema.ValidateJSON(json.RawMessage(tc.args))
			if tc.want == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("err = %v, want a *ValidationError", err)
			}
			if !strings.HasPrefix(err.Error(), "invalid arguments: ") || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q, want containing %q", err, tc.want)
			}
		})
	}
	if err := (&Schema{}).Validate(map[string]any{"anything": []any{1}}); err != nil {
		t.Errorf("untyped schema rejected a value: %v", err)
	}
}

func TestNewValidatesBeforeDecoding(t *testing.T) {
	type args struct {
		Path string `json:"path"`
		N    int    `json:"n,omitempty" enum:"1,2"`
	}
	fn := func(_ context.Context, a args) (string, error) { return a.Path, nil }
	validated := New("v", "", fn)
	unchecked := New("u", "", fn, WithoutValidation())
	customSchema := New("c", "", fn, WithParameters(json.RawMessage(`{"type":"object"}`)))

	_, err := validated.Execute(context.Background(), Call{Args: json.RawMessage(`{"n":1}`)})
	var ve *ValidationError
	if !errors.As(err, &ve) || !strings.Contains(err.Error(), `missing required property "path"`) {
		t.Errorf("validated tool err = %v", err)
	}
	if _, err := validated.Execute(context.Background(), Call{Args: json.RawMessage(`{"path":"/x","n":3}`)}); err == nil {
		t.Error("enum violation accepted")
	}
	for name, tl := range map[string]Tool{"unchecked": unchecked, "custom schema": customSchema} {
		res, err := tl.Execute(context.Background(), Call{Args: json.RawMessage(`{"n":3}`)})
		if err != nil || res.Output.Text != "" {
			t.Errorf("%s: res=%+v err=%v; the decoder alone should accept this", name, res, err)
		}
	}
	// Custom schemas from a Schemer are not validated either.
	schemerTool := New("s", "", func(context.Context, custom) (string, error) { return "ok", nil })
	if _, err := schemerTool.Execute(context.Background(), Call{Args: json.RawMessage(`{"x":1}`)}); err != nil {
		t.Errorf("schemer tool: %v", err)
	}
}

func TestValidateUnknownType(t *testing.T) {
	cases := []struct {
		name    string
		schema  *Schema
		value   any
		wantErr string
	}{
		{"misspelt type", &Schema{Type: "strng"}, "x", `"strng" is not a JSON Schema type`},
		{"null accepts null", &Schema{Type: "null"}, nil, ""},
		{"null rejects a value", &Schema{Type: "null"}, "x", "expected null"},
		{"no type accepts anything", &Schema{}, "x", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.schema.Validate(tc.value)
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
