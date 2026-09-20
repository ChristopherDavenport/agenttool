package agenttool

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// ValidationError reports arguments that do not satisfy a schema. Its
// message is phrased for the model, which sees it as the error output
// and can retry.
type ValidationError struct {
	// Path locates the offending value: "" for the root, otherwise a
	// dotted property path with [i] for array elements.
	Path string
	Msg  string
}

// Error returns "invalid arguments: <path>: <msg>".
func (e *ValidationError) Error() string {
	if e.Path == "" {
		return "invalid arguments: " + e.Msg
	}
	return "invalid arguments: " + e.Path + ": " + e.Msg
}

// ValidateJSON checks raw against s. Empty raw is an empty object.
func (s *Schema) ValidateJSON(raw json.RawMessage) error {
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return &ValidationError{Msg: "not valid JSON: " + err.Error()}
	}
	return s.Validate(v)
}

// Validate checks a decoded JSON value (maps, slices, strings,
// json.Number or float64, bools, nil) against s: its type, enum,
// required properties, additionalProperties when false, and its items
// and properties recursively. A schema with no Type accepts anything;
// one whose Type is not a JSON Schema type name accepts nothing, so a
// misspelt hand-built schema fails on its first use rather than
// silently passing everything.
func (s *Schema) Validate(v any) error {
	return s.validate(v, "")
}

func (s *Schema) validate(v any, path string) error {
	if s == nil {
		return nil
	}
	if v == nil {
		if s.Nullable || s.Type == "" || s.Type == "null" {
			return nil
		}
		return &ValidationError{Path: path, Msg: "expected " + s.Type + ", got null"}
	}
	if s.Enum != nil && !inEnum(s.Enum, v) {
		return &ValidationError{Path: path, Msg: fmt.Sprintf("expected one of %s, got %s", describeEnum(s.Enum), describe(v))}
	}
	switch s.Type {
	case "":
		return nil
	case "object":
		obj, ok := v.(map[string]any)
		if !ok {
			return &ValidationError{Path: path, Msg: "expected object, got " + describe(v)}
		}
		return s.validateObject(obj, path)
	case "array":
		arr, ok := v.([]any)
		if !ok {
			return &ValidationError{Path: path, Msg: "expected array, got " + describe(v)}
		}
		for i, e := range arr {
			if err := s.Items.validate(e, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
		return nil
	case "string":
		if _, ok := v.(string); !ok {
			return &ValidationError{Path: path, Msg: "expected string, got " + describe(v)}
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return &ValidationError{Path: path, Msg: "expected boolean, got " + describe(v)}
		}
	case "number":
		if _, ok := toFloat(v); !ok {
			return &ValidationError{Path: path, Msg: "expected number, got " + describe(v)}
		}
	case "integer":
		f, ok := toFloat(v)
		if !ok {
			return &ValidationError{Path: path, Msg: "expected integer, got " + describe(v)}
		}
		if f != math.Trunc(f) {
			return &ValidationError{Path: path, Msg: "expected integer, got " + describe(v)}
		}
	case "null":
		return &ValidationError{Path: path, Msg: "expected null, got " + describe(v)}
	default:
		return &ValidationError{Path: path, Msg: fmt.Sprintf("schema type %q is not a JSON Schema type", s.Type)}
	}
	return nil
}

func (s *Schema) validateObject(obj map[string]any, path string) error {
	props := make(map[string]*Schema, len(s.Properties))
	for _, p := range s.Properties {
		props[p.Name] = p.Schema
	}
	for _, name := range s.Required {
		if _, ok := obj[name]; !ok {
			return &ValidationError{Path: path, Msg: fmt.Sprintf("missing required property %q", name)}
		}
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		child := join(path, k)
		ps, known := props[k]
		switch {
		case known:
			if err := ps.validate(obj[k], child); err != nil {
				return err
			}
		case s.NoAdditional:
			return &ValidationError{Path: path, Msg: fmt.Sprintf("unexpected property %q", k)}
		case s.AdditionalProperties != nil:
			if err := s.AdditionalProperties.validate(obj[k], child); err != nil {
				return err
			}
		}
	}
	return nil
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}

// inEnum compares v against enum values by JSON value: strings by
// equality, numbers by value, bools by equality.
func inEnum(enum []any, v any) bool {
	for _, e := range enum {
		switch ev := e.(type) {
		case string:
			if s, ok := v.(string); ok && s == ev {
				return true
			}
		case bool:
			if b, ok := v.(bool); ok && b == ev {
				return true
			}
		default:
			ef, okE := toFloat(e)
			vf, okV := toFloat(v)
			if okE && okV && ef == vf {
				return true
			}
		}
	}
	return false
}

func describeEnum(enum []any) string {
	parts := make([]string, 0, len(enum))
	for _, e := range enum {
		parts = append(parts, describe(e))
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// describe renders a value for an error message: strings quoted,
// numbers and bools as written, containers by kind.
func describe(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case string:
		return strconv.Quote(x)
	case json.Number:
		return x.String()
	case bool:
		return strconv.FormatBool(x)
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case int64:
		return strconv.FormatInt(x, 10)
	case int:
		return strconv.Itoa(x)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	}
	return fmt.Sprintf("%v", v)
}
