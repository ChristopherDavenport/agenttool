package agenttool

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The manifest describes each fixture's argument shape in the terms of
// RFC 0001's schema section, so the corpus can be regenerated without
// Go. These tests hold the manifest, the rules and the generator to one
// another: the manifest interpreted under the rules must produce every
// golden, and the manifest must name exactly the fixtures the Go table
// generates.

type manifest struct {
	Fixtures []fixture `json:"fixtures"`
}

type fixture struct {
	Name     string          `json:"name"`
	Strict   bool            `json:"strict"`
	Source   string          `json:"source"`
	Fields   []shapeField    `json:"fields"`
	Supplied json.RawMessage `json:"supplied"`
}

// shapeType is a type of the argument shape: a scalar, any, an array
// with items, a map with values, or an object with fields.
type shapeType struct {
	Type   string       `json:"type"`
	Format string       `json:"format"`
	Items  *shapeType   `json:"items"`
	Values *shapeType   `json:"values"`
	Fields []shapeField `json:"fields"`
}

// shapeField is one field of an object: a type with a name and the
// per-field members.
type shapeField struct {
	shapeType
	Name        string `json:"name"`
	Description string `json:"description"`
	Enum        []any  `json:"enum"`
	Optional    bool   `json:"optional"`
	Nullable    bool   `json:"nullable"`
}

// buildShape is the RFC's generation rules over the manifest's shape
// language, written without reference to the reflection generator so a
// divergence between the two shows up here.
func buildShape(t shapeType, strict bool) (*Schema, error) {
	switch t.Type {
	case "any":
		return &Schema{}, nil
	case "boolean", "integer", "number", "string":
		return &Schema{Type: t.Type, Format: t.Format}, nil
	case "array":
		if t.Items == nil {
			return nil, fmt.Errorf("array without items")
		}
		items, err := buildShape(*t.Items, strict)
		if err != nil {
			return nil, err
		}
		return &Schema{Type: "array", Items: items}, nil
	case "map":
		if strict {
			return nil, fmt.Errorf("map under strict mode")
		}
		if t.Values == nil {
			return nil, fmt.Errorf("map without values")
		}
		values, err := buildShape(*t.Values, strict)
		if err != nil {
			return nil, err
		}
		return &Schema{Type: "object", AdditionalProperties: values}, nil
	case "object":
		s := &Schema{Type: "object", Properties: []Property{}, Required: []string{}, NoAdditional: strict}
		for _, f := range t.Fields {
			p, err := buildShape(f.shapeType, strict)
			if err != nil {
				return nil, fmt.Errorf("field %s: %w", f.Name, err)
			}
			p.Description = f.Description
			p.Enum = f.Enum
			p.Nullable = strict && f.Nullable
			// A nullable enum lists null last under strict, since enum
			// is an assertion of its own.
			if p.Nullable && p.Enum != nil {
				p.Enum = append(append([]any(nil), p.Enum...), nil)
			}
			s.Properties = append(s.Properties, Property{Name: f.Name, Schema: p})
			if strict || !f.Optional {
				s.Required = append(s.Required, f.Name)
			}
		}
		return s, nil
	}
	return nil, fmt.Errorf("unknown type %q", t.Type)
}

// writeSchema serialises a schema tree in the RFC's member order
// without Schema.MarshalJSON, so the bytes the manifest produces come
// from the RFC's list and not from the generator's own encoder: a
// change to either that the other does not follow fails the goldens.
func writeSchema(s *Schema) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	first := true
	raw := func(key string, v []byte) {
		if !first {
			buf.WriteByte(',')
		}
		first = false
		k, _ := json.Marshal(key)
		buf.Write(k)
		buf.WriteByte(':')
		buf.Write(v)
	}
	leaf := func(key string, v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		raw(key, b)
		return nil
	}
	if s.Type != "" {
		var typ any = s.Type
		if s.Nullable {
			typ = []string{s.Type, "null"}
		}
		if err := leaf("type", typ); err != nil {
			return nil, err
		}
	}
	if s.Description != "" {
		if err := leaf("description", s.Description); err != nil {
			return nil, err
		}
	}
	if s.Format != "" {
		if err := leaf("format", s.Format); err != nil {
			return nil, err
		}
	}
	if s.Enum != nil {
		if err := leaf("enum", s.Enum); err != nil {
			return nil, err
		}
	}
	if s.Properties != nil {
		var props bytes.Buffer
		props.WriteByte('{')
		for i, p := range s.Properties {
			if i > 0 {
				props.WriteByte(',')
			}
			k, _ := json.Marshal(p.Name)
			props.Write(k)
			props.WriteByte(':')
			child, err := writeSchema(p.Schema)
			if err != nil {
				return nil, err
			}
			props.Write(child)
		}
		props.WriteByte('}')
		raw("properties", props.Bytes())
	}
	if s.Required != nil {
		if err := leaf("required", s.Required); err != nil {
			return nil, err
		}
	}
	if s.Items != nil {
		items, err := writeSchema(s.Items)
		if err != nil {
			return nil, err
		}
		raw("items", items)
	}
	switch {
	case s.NoAdditional:
		raw("additionalProperties", []byte("false"))
	case s.AdditionalProperties != nil:
		add, err := writeSchema(s.AdditionalProperties)
		if err != nil {
			return nil, err
		}
		raw("additionalProperties", add)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

func readManifest(t *testing.T) manifest {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "schema", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	var m manifest
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("manifest: %v", err)
	}
	return m
}

// TestManifestProducesGoldens interprets each fixture's shape under the
// RFC's rules and requires the golden byte for byte, so the manifest is
// sufficient to regenerate the corpus and the rules match the generator.
func TestManifestProducesGoldens(t *testing.T) {
	for _, fx := range readManifest(t).Fixtures {
		t.Run(fx.Name, func(t *testing.T) {
			var got []byte
			switch {
			case fx.Supplied != nil && fx.Fields != nil:
				t.Fatal("fixture has both fields and supplied")
			case fx.Supplied != nil:
				got = fx.Supplied
			case fx.Fields != nil:
				s, err := buildShape(shapeType{Type: "object", Fields: fx.Fields}, fx.Strict)
				if err != nil {
					t.Fatal(err)
				}
				got, err = writeSchema(s)
				if err != nil {
					t.Fatal(err)
				}
			default:
				t.Fatal("fixture has neither fields nor supplied")
			}
			want, err := os.ReadFile(filepath.Join("testdata", "schema", fx.Name+".json"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.TrimSpace(string(want)) != string(indent(t, got)) {
				t.Errorf("manifest does not produce the golden\n got: %s\nwant: %s", indent(t, got), want)
			}
		})
	}
}

// TestManifestCoversGoldens requires the manifest, the golden files and
// the Go table to name the same fixtures with the same strictness, so a
// fixture added to one is added to all three.
func TestManifestCoversGoldens(t *testing.T) {
	m := readManifest(t)
	inManifest := make(map[string]bool, len(m.Fixtures))
	for _, fx := range m.Fixtures {
		if _, dup := inManifest[fx.Name]; dup {
			t.Errorf("manifest names %q twice", fx.Name)
		}
		inManifest[fx.Name] = fx.Strict
	}

	inTable := make(map[string]bool, len(schemaGoldens))
	for _, tc := range schemaGoldens {
		inTable[tc.name] = tc.strict
		strict, ok := inManifest[tc.name]
		switch {
		case !ok:
			t.Errorf("Go table generates %q, manifest does not describe it", tc.name)
		case strict != tc.strict:
			t.Errorf("%q: manifest strict=%v, Go table strict=%v", tc.name, strict, tc.strict)
		}
	}
	for name := range inManifest {
		if _, ok := inTable[name]; !ok {
			t.Errorf("manifest describes %q, Go table does not generate it", name)
		}
	}

	files, err := filepath.Glob(filepath.Join("testdata", "schema", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".json")
		if name == "manifest" {
			continue
		}
		if _, ok := inManifest[name]; !ok {
			t.Errorf("golden %s has no manifest entry", f)
		}
	}
}

// TestManifestRejects pins the shapes the RFC says a generator rejects.
func TestManifestRejects(t *testing.T) {
	str := shapeType{Type: "string"}
	cases := []struct {
		name   string
		shape  shapeType
		strict bool
	}{
		{"map under strict", shapeType{Type: "object", Fields: []shapeField{{Name: "m", shapeType: shapeType{Type: "map", Values: &str}}}}, true},
		{"unknown type", shapeType{Type: "object", Fields: []shapeField{{Name: "x", shapeType: shapeType{Type: "int"}}}}, false},
		{"array without items", shapeType{Type: "object", Fields: []shapeField{{Name: "a", shapeType: shapeType{Type: "array"}}}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := buildShape(tc.shape, tc.strict); err == nil {
				t.Error("expected an error")
			}
		})
	}
}
