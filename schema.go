package agenttool

import (
	"bytes"
	"encoding"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// Schemer is implemented by argument types that supply their own JSON
// Schema instead of the reflected one.
type Schemer interface {
	JSONSchema() json.RawMessage
}

// Schema is a JSON Schema fragment as the generator builds it. Keys are
// emitted in a fixed order and properties keep struct field order, so
// the output is stable across runs and readable in a request.
type Schema struct {
	// Type is a JSON Schema type name, or empty for any value.
	Type string
	// Nullable adds "null" to the type, as strict mode requires for
	// optional fields.
	Nullable    bool
	Description string
	Format      string
	Enum        []any
	Properties  []Property
	Required    []string
	// AdditionalProperties is emitted when set: false, or a schema for
	// map values.
	AdditionalProperties *Schema
	NoAdditional         bool
	Items                *Schema
}

// Property is one named member of an object schema.
type Property struct {
	Name   string
	Schema *Schema
}

// MarshalJSON emits the schema with a fixed key order.
func (s *Schema) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	first := true
	field := func(key string, value any) error {
		if !first {
			buf.WriteByte(',')
		}
		first = false
		k, _ := json.Marshal(key)
		buf.Write(k)
		buf.WriteByte(':')
		v, err := json.Marshal(value)
		if err != nil {
			return err
		}
		buf.Write(v)
		return nil
	}
	if s.Type != "" {
		var typ any = s.Type
		if s.Nullable {
			typ = []string{s.Type, "null"}
		}
		if err := field("type", typ); err != nil {
			return nil, err
		}
	}
	if s.Description != "" {
		if err := field("description", s.Description); err != nil {
			return nil, err
		}
	}
	if s.Format != "" {
		if err := field("format", s.Format); err != nil {
			return nil, err
		}
	}
	if s.Enum != nil {
		if err := field("enum", s.Enum); err != nil {
			return nil, err
		}
	}
	if s.Properties != nil {
		if !first {
			buf.WriteByte(',')
		}
		first = false
		buf.WriteString(`"properties":{`)
		for i, p := range s.Properties {
			if i > 0 {
				buf.WriteByte(',')
			}
			k, _ := json.Marshal(p.Name)
			buf.Write(k)
			buf.WriteByte(':')
			v, err := json.Marshal(p.Schema)
			if err != nil {
				return nil, err
			}
			buf.Write(v)
		}
		buf.WriteByte('}')
	}
	if s.Required != nil {
		if err := field("required", s.Required); err != nil {
			return nil, err
		}
	}
	if s.Items != nil {
		if err := field("items", s.Items); err != nil {
			return nil, err
		}
	}
	if s.NoAdditional {
		if err := field("additionalProperties", false); err != nil {
			return nil, err
		}
	} else if s.AdditionalProperties != nil {
		if err := field("additionalProperties", s.AdditionalProperties); err != nil {
			return nil, err
		}
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// SchemaFor returns the JSON Schema for T. See [SchemaOf].
func SchemaFor[T any](opts ...Option) (json.RawMessage, error) {
	var zero T
	return SchemaOf(reflect.TypeOf(&zero).Elem(), opts...)
}

// SchemaOf returns the JSON Schema object [New] would use for an
// argument type t: the one t supplies when it implements [Schemer],
// otherwise the reflected schema of [Reflect]. Of the options only
// [WithStrict] applies, and not to a Schemer's own schema.
//
// Exported fields become properties named by their json tag. A "desc"
// tag becomes the description and an "enum" tag, comma separated,
// becomes the enum. Embedded structs are flattened. Supported kinds are
// bool, the integer and float kinds, string, slices and arrays, maps
// with string keys, nested structs, pointers, time.Time (a date-time
// string), []byte (a string), json.RawMessage and interfaces (any
// value) and types implementing encoding.TextMarshaler (a string).
//
// In strict mode every property is required, additionalProperties is
// false on every object, pointer fields are nullable, and maps are
// rejected because strict mode cannot express them.
//
// Outside strict mode a field is optional when its tag says omitempty
// or omitzero or when it is a pointer.
func SchemaOf(t reflect.Type, opts ...Option) (json.RawMessage, error) {
	if t == nil {
		return nil, fmt.Errorf("agenttool: schema of nil type")
	}
	if s, ok := schemerFor(t); ok {
		return s, nil
	}
	s, err := Reflect(t, opts...)
	if err != nil {
		return nil, err
	}
	return json.Marshal(s)
}

// Reflect returns the schema tree that [SchemaOf] serialises, for
// callers that want to validate with it. t must be a struct or a
// pointer to one; [Schemer] is not consulted, since a Schemer supplies
// JSON, not a tree. Of the options only [WithStrict] applies.
func Reflect(t reflect.Type, opts ...Option) (*Schema, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	return reflectStruct(t, o.strict)
}

func reflectStruct(t reflect.Type, strict bool) (*Schema, error) {
	if t == nil {
		return nil, fmt.Errorf("agenttool: schema of nil type")
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil, fmt.Errorf("agenttool: arguments must be a struct, got %s", t)
	}
	g := &generator{strict: strict, seen: map[reflect.Type]bool{}}
	return g.object(t)
}

func schemerFor(t reflect.Type) (json.RawMessage, bool) {
	schemer := reflect.TypeFor[Schemer]()
	if t.Implements(schemer) {
		return reflect.Zero(t).Interface().(Schemer).JSONSchema(), true
	}
	if reflect.PointerTo(t).Implements(schemer) {
		return reflect.New(t).Interface().(Schemer).JSONSchema(), true
	}
	return nil, false
}

type generator struct {
	strict bool
	seen   map[reflect.Type]bool
}

var (
	timeType          = reflect.TypeFor[time.Time]()
	rawMessageType    = reflect.TypeFor[json.RawMessage]()
	textMarshalerType = reflect.TypeFor[encoding.TextMarshaler]()
	jsonMarshalerType = reflect.TypeFor[json.Marshaler]()
)

func (g *generator) object(t reflect.Type) (*Schema, error) {
	if g.seen[t] {
		return nil, fmt.Errorf("agenttool: recursive type %s cannot be expressed as a schema", t)
	}
	g.seen[t] = true
	defer delete(g.seen, t)

	s := &Schema{Type: "object", Properties: []Property{}, Required: []string{}}
	if g.strict {
		s.NoAdditional = true
	}
	if err := g.fields(t, s); err != nil {
		return nil, err
	}
	return s, nil
}

func (g *generator) fields(t reflect.Type, s *Schema) error {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		if f.Anonymous && name == "" {
			ft := f.Type
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				if err := g.fields(ft, s); err != nil {
					return err
				}
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		optional := f.Type.Kind() == reflect.Pointer || hasOpt(opts, "omitempty") || hasOpt(opts, "omitzero")
		prop, err := g.schema(f.Type, f.Name)
		if err != nil {
			return err
		}
		if d, ok := f.Tag.Lookup("desc"); ok {
			prop.Description = d
		}
		if e, ok := f.Tag.Lookup("enum"); ok {
			prop.Enum, err = enumValues(f.Type, e)
			if err != nil {
				return fmt.Errorf("agenttool: field %s: %w", f.Name, err)
			}
		}
		if g.strict {
			if f.Type.Kind() == reflect.Pointer {
				prop.Nullable = true
			}
			s.Required = append(s.Required, name)
		} else if !optional {
			s.Required = append(s.Required, name)
		}
		s.Properties = append(s.Properties, Property{Name: name, Schema: prop})
	}
	return nil
}

func hasOpt(opts, want string) bool {
	for opts != "" {
		var o string
		o, opts, _ = strings.Cut(opts, ",")
		if o == want {
			return true
		}
	}
	return false
}

func (g *generator) schema(t reflect.Type, field string) (*Schema, error) {
	switch {
	case t == timeType:
		return &Schema{Type: "string", Format: "date-time"}, nil
	case t == rawMessageType:
		return &Schema{}, nil
	case t.Kind() != reflect.Pointer && t.Kind() != reflect.Struct && t.Implements(jsonMarshalerType):
		return &Schema{}, nil
	case t.Implements(textMarshalerType) || reflect.PointerTo(t).Implements(textMarshalerType):
		if t.Kind() == reflect.Pointer {
			return g.schema(t.Elem(), field)
		}
		return &Schema{Type: "string"}, nil
	}
	switch t.Kind() {
	case reflect.Pointer:
		return g.schema(t.Elem(), field)
	case reflect.Bool:
		return &Schema{Type: "boolean"}, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return &Schema{Type: "integer"}, nil
	case reflect.Float32, reflect.Float64:
		return &Schema{Type: "number"}, nil
	case reflect.String:
		return &Schema{Type: "string"}, nil
	case reflect.Interface:
		return &Schema{}, nil
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 {
			return &Schema{Type: "string"}, nil
		}
		items, err := g.schema(t.Elem(), field)
		if err != nil {
			return nil, err
		}
		return &Schema{Type: "array", Items: items}, nil
	case reflect.Map:
		if t.Key().Kind() != reflect.String {
			return nil, fmt.Errorf("agenttool: field %s: map key must be a string, got %s", field, t.Key())
		}
		if g.strict {
			return nil, fmt.Errorf("agenttool: field %s: maps cannot be expressed in a strict schema", field)
		}
		values, err := g.schema(t.Elem(), field)
		if err != nil {
			return nil, err
		}
		return &Schema{Type: "object", AdditionalProperties: values}, nil
	case reflect.Struct:
		return g.object(t)
	default:
		return nil, fmt.Errorf("agenttool: field %s: unsupported type %s", field, t)
	}
}

// enumValues parses an enum tag into values of the field's kind.
func enumValues(t reflect.Type, tag string) ([]any, error) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	parts := strings.Split(tag, ",")
	out := make([]any, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		switch t.Kind() {
		case reflect.String:
			out = append(out, p)
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
			reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			n, err := strconv.ParseInt(p, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("enum value %q: %w", p, err)
			}
			out = append(out, n)
		case reflect.Float32, reflect.Float64:
			n, err := strconv.ParseFloat(p, 64)
			if err != nil {
				return nil, fmt.Errorf("enum value %q: %w", p, err)
			}
			out = append(out, n)
		case reflect.Bool:
			b, err := strconv.ParseBool(p)
			if err != nil {
				return nil, fmt.Errorf("enum value %q: %w", p, err)
			}
			out = append(out, b)
		default:
			return nil, fmt.Errorf("enum tag on unsupported type %s", t)
		}
	}
	return out, nil
}
