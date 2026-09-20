package agenttool

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite golden files")

type readFileArgs struct {
	Path     string `json:"path" desc:"Absolute path to read"`
	MaxBytes int    `json:"max_bytes,omitempty" desc:"Stop after this many bytes"`
}

type nested struct {
	Name string `json:"name"`
	Tags []string
}

type everything struct {
	Str      string            `json:"str"`
	Int      int64             `json:"int" desc:"an integer"`
	Uint     uint8             `json:"uint"`
	Float    float64           `json:"float"`
	Bool     bool              `json:"bool"`
	Level    string            `json:"level" enum:"low,medium,high"`
	Count    *int              `json:"count,omitempty" enum:"1,2,3"`
	When     time.Time         `json:"when"`
	Raw      json.RawMessage   `json:"raw,omitempty"`
	Any      any               `json:"any,omitempty"`
	Bytes    []byte            `json:"bytes,omitempty"`
	List     []nested          `json:"list"`
	Matrix   [][]float32       `json:"matrix,omitempty"`
	Nested   nested            `json:"nested"`
	Pointer  *nested           `json:"pointer,omitempty"`
	Labels   map[string]string `json:"labels,omitempty"`
	Duration time.Duration     `json:"duration,omitempty"`
	hidden   int               // unexported fields are skipped
	Skipped  int               `json:"-"`
}

type embedded struct {
	nested
	Extra string `json:"extra,omitempty"`
}

type strictArgs struct {
	Query string `json:"query" desc:"Search terms"`
	Limit *int   `json:"limit" desc:"Maximum results"`
	Inner struct {
		Deep bool `json:"deep"`
	} `json:"inner"`
}

type custom struct{}

func (custom) JSONSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"x":{"type":"string"}}}`)
}

type textKey string

func (k textKey) MarshalText() ([]byte, error) { return []byte(k), nil }

type textArgs struct {
	Key textKey `json:"key"`
}

type recursive struct {
	Child *recursive `json:"child,omitempty"`
}

func TestSchemaGolden(t *testing.T) {
	cases := []struct {
		name   string
		typ    reflect.Type
		strict bool
	}{
		{"read_file", reflect.TypeFor[readFileArgs](), false},
		{"read_file_strict", reflect.TypeFor[readFileArgs](), true},
		{"everything", reflect.TypeFor[everything](), false},
		{"embedded", reflect.TypeFor[embedded](), false},
		{"strict", reflect.TypeFor[strictArgs](), true},
		{"no_args", reflect.TypeFor[NoArgs](), false},
		{"no_args_strict", reflect.TypeFor[NoArgs](), true},
		{"custom", reflect.TypeFor[custom](), false},
		{"text_marshaler", reflect.TypeFor[textArgs](), false},
		{"pointer_to_struct", reflect.TypeFor[*readFileArgs](), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SchemaOf(tc.typ, strictOpts(tc.strict)...)
			if err != nil {
				t.Fatal(err)
			}
			var pretty json.RawMessage
			if err := json.Unmarshal(got, &pretty); err != nil {
				t.Fatalf("schema is not JSON: %v\n%s", err, got)
			}
			path := filepath.Join("testdata", "schema", tc.name+".json")
			if *update {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, append(indent(t, got), '\n'), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (run with -update to create)", err)
			}
			if strings.TrimSpace(string(want)) != string(indent(t, got)) {
				t.Errorf("schema mismatch\n got: %s\nwant: %s", indent(t, got), want)
			}
		})
	}
}

func indent(t *testing.T, raw []byte) []byte {
	t.Helper()
	out, err := indentJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestSchemaKeyOrder(t *testing.T) {
	got, err := SchemaOf(reflect.TypeFor[everything]())
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	if !strings.HasPrefix(s, `{"type":"object","properties":{"str":`) {
		t.Errorf("properties do not follow field order: %s", s)
	}
	if strings.Index(s, `"list"`) > strings.Index(s, `"nested"`) {
		t.Errorf("field order lost: %s", s)
	}
}

func TestSchemaErrors(t *testing.T) {
	cases := []struct {
		name   string
		typ    reflect.Type
		strict bool
		want   string
	}{
		{"recursive", reflect.TypeFor[recursive](), false, "recursive"},
		{"strict map", reflect.TypeFor[everything](), true, "strict"},
		{"not a struct", reflect.TypeFor[string](), false, "must be a struct"},
		{"bad map key", reflect.TypeFor[struct {
			M map[int]string `json:"m"`
		}](), false, "map key"},
		{"chan", reflect.TypeFor[struct {
			C chan int `json:"c"`
		}](), false, "unsupported"},
		{"bad enum", reflect.TypeFor[struct {
			N int `json:"n" enum:"a,b"`
		}](), false, "enum value"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := SchemaOf(tc.typ, strictOpts(tc.strict)...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestNewPanicsOnBadSchema(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic")
		}
	}()
	New("bad", "", func(context.Context, recursive) (string, error) { return "", nil })
}

var _ = everything{}.hidden

// strictOpts turns a table's strict flag into options.
func strictOpts(strict bool) []Option {
	if strict {
		return []Option{WithStrict()}
	}
	return nil
}
