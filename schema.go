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
//
// The type can be built by hand for [Schema.Validate]. A nil or empty
// slice is omitted from the JSON, so a hand-built object schema emits
// "properties" and "required" only when they are set, while the
// generator sets both to empty slices and always emits them. Type ""
// accepts any value.
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
	// AdditionalProperties and NoAdditional share the JSON key
	// "additionalProperties". NoAdditional emits false and wins when both
	// are set, as strict mode requires; AdditionalProperties emits a
	// schema for the values of a map. Neither set, the key is omitted and
	// unknown properties are allowed.
	AdditionalProperties *Schema
	NoAdditional         bool
	Items                *Schema
}

// Property is one named member of an object schema.
type Property struct {
	Name   string
	Schema *Schema
}

// MarshalJSON emits the schema with a fixed key order. The receiver is
// a value so that a Schema value, on its own or inside another struct,
// marshals the same way as a pointer; the validate methods take a
// pointer because a nil Items or AdditionalProperties accepts anything.
func (s Schema) MarshalJSON() ([]byte, error) {
	var o ordered
	if s.Type != "" {
		var typ any = s.Type
		if s.Nullable {
			typ = []string{s.Type, "null"}
		}
		o = append(o, kv{"type", typ})
	}
	if s.Description != "" {
		o = append(o, kv{"description", s.Description})
	}
	if s.Format != "" {
		o = append(o, kv{"format", s.Format})
	}
	if s.Enum != nil {
		o = append(o, kv{"enum", s.Enum})
	}
	if s.Properties != nil {
		props := make(ordered, 0, len(s.Properties))
		for _, p := range s.Properties {
			props = append(props, kv{p.Name, p.Schema})
		}
		o = append(o, kv{"properties", props})
	}
	if s.Required != nil {
		o = append(o, kv{"required", s.Required})
	}
	if s.Items != nil {
		o = append(o, kv{"items", s.Items})
	}
	switch {
	case s.NoAdditional:
		o = append(o, kv{"additionalProperties", false})
	case s.AdditionalProperties != nil:
		o = append(o, kv{"additionalProperties", s.AdditionalProperties})
	}
	return o.MarshalJSON()
}

// kv is one member of an ordered JSON object.
type kv struct {
	key   string
	value any
}

// ordered is a JSON object whose members are emitted in slice order,
// where encoding/json would sort a map's keys.
type ordered []kv

func (o ordered) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, m := range o {
		if i > 0 {
			buf.WriteByte(',')
		}
		k, err := json.Marshal(m.key)
		if err != nil {
			return nil, err
		}
		buf.Write(k)
		buf.WriteByte(':')
		v, err := json.Marshal(m.value)
		if err != nil {
			return nil, err
		}
		buf.Write(v)
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
// becomes the enum. Embedded structs are flattened under encoding/json's
// rules: of several fields promoted under one name the shallowest wins,
// a tagged one wins among equals, and the rest of a tie is dropped as
// the encoder would drop it. Supported kinds are
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
	var fields []field
	if err := g.collect(t, 0, &fields); err != nil {
		return nil, err
	}
	for _, f := range dominant(fields) {
		s.Properties = append(s.Properties, Property{Name: f.name, Schema: f.schema})
		if f.required {
			s.Required = append(s.Required, f.name)
		}
	}
	return s, nil
}

// field is one exported struct field the generator found, with what
// [dominant] needs to settle a name that several fields promote.
type field struct {
	name     string
	schema   *Schema
	required bool
	depth    int // embedding depth; 0 for the struct's own fields
	tagged   bool
}

// collect appends the fields of t in index order, recursing into
// embedded structs in place so the order matches encoding/json's.
func (g *generator) collect(t reflect.Type, depth int, out *[]field) error {
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
				if err := g.collect(ft, depth+1, out); err != nil {
					return err
				}
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		tagged := name != ""
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
		if g.strict && f.Type.Kind() == reflect.Pointer {
			prop.Nullable = true
		}
		*out = append(*out, field{name: name, schema: prop, required: g.strict || !optional, depth: depth, tagged: tagged})
	}
	return nil
}

// dominant keeps, for each name, the field encoding/json would encode:
// the shallowest, then the one tagged among equals, and none of a tie
// that has no tag or several. Order is preserved.
func dominant(fields []field) []field {
	byName := make(map[string][]int, len(fields))
	for i, f := range fields {
		byName[f.name] = append(byName[f.name], i)
	}
	keep := make([]bool, len(fields))
	for _, idx := range byName {
		shallowest := idx[0]
		for _, i := range idx[1:] {
			if fields[i].depth < fields[shallowest].depth {
				shallowest = i
			}
		}
		var equal, tagged []int
		for _, i := range idx {
			if fields[i].depth == fields[shallowest].depth {
				equal = append(equal, i)
				if fields[i].tagged {
					tagged = append(tagged, i)
				}
			}
		}
		switch {
		case len(equal) == 1:
			keep[equal[0]] = true
		case len(tagged) == 1:
			keep[tagged[0]] = true
		}
	}
	out := make([]field, 0, len(fields))
	for i, f := range fields {
		if keep[i] {
			out = append(out, f)
		}
	}
	return out
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
	case t.Kind() != reflect.Pointer && t.Implements(jsonMarshalerType):
		// A type that writes its own JSON tells reflection nothing about
		// the shape it writes, so it accepts any value. An argument type
		// that needs better implements [Schemer].
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
			reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
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
