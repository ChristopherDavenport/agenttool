// Package cli runs the tools of an [agenttool.Set] as the commands of
// one program, for a host that reaches tools through a shell rather
// than in process or over MCP. Each tool is a command named as the tool
// is, and its arguments are flags read from its schema, a JSON object,
// or both:
//
//	file-tools read_file --path go.mod
//	file-tools read_file '{"path": "go.mod"}'
//	file-tools read_file - <<'EOF'
//	{"path": "go.mod"}
//	EOF
//
// Nothing here is part of the contract, and a tool needs nothing to be
// run this way. The package is a translation: [Commands] describes a
// set as commands, [Runner] parses a command line into a [agenttool.Call]
// and runs it, and [Markdown] renders the usage a model reads, from the
// same description the parser uses so that the two cannot drift.
//
// The translation loses what one call per process cannot hold. A
// [agenttool.Resource] or [agenttool.Sequential] tool orders nothing
// against a call in another process, and a tool that keeps state across
// calls, a persistent shell or a container session, loses it at exit.
// Such a tool is better left out of the set than run here.
package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/ChristopherDavenport/agenttool"
)

// Command is one tool as a command: what [Runner] parses and [Markdown]
// renders.
type Command struct {
	// Name is the tool's name, which is the command's.
	Name        string
	Description string
	// Annotations are the tool's, the zero value when it carries none.
	Annotations agenttool.Annotations
	// Params are the top-level properties of the tool's schema, in
	// schema order.
	Params []Param
	// Schema is the tool's parameters schema, [agenttool.NoArgsSchema]
	// when it has none.
	Schema json.RawMessage
}

// Param is one property of a tool's arguments: a top-level one, or a
// field of an object that is one.
type Param struct {
	Name        string
	Description string
	// Type is the property's JSON Schema type, with a nullable union
	// reduced to the type it admits besides null; empty when the schema
	// names none or several.
	Type string
	// Items is the type of an array's elements, read as Type is; empty
	// for anything but an array.
	Items string
	// Fields are the properties of an object that declares them, read
	// as the top-level ones are, in schema order; nil for anything else.
	// Each field with a flag has one of its own, named by its path from
	// the top: --name.field.
	Fields []Param
	// Values is the type of a map's values, read as Type is: an object
	// that declares additionalProperties and no properties. Empty for
	// anything else.
	Values string
	// Required is whether the object that holds the property requires
	// it.
	Required bool
	Enum     []any
}

// Flag reports whether the parameter has a flag of its own: a string,
// integer, number or boolean has one, an array of them has one that
// repeats, and a map of them has one given once per entry, as
// key=value. An object with fields has none of its own, and its fields
// may have theirs; see [Param.Fields]. Anything else, an array of
// objects or a value of any type, arrives only in the JSON argument, as
// does a property whose name a flag cannot carry: empty, starting with
// a dash, or holding an equals sign.
func (p Param) Flag() bool {
	if !flagName(p.Name) {
		return false
	}
	return scalar(p.Type) || p.Repeated() || p.Map()
}

// Repeated reports whether the parameter's flag is given once per
// element of an array.
func (p Param) Repeated() bool {
	return p.Type == "array" && scalar(p.Items)
}

// Map reports whether the parameter is a map whose flag is given once
// per entry, as key=value.
func (p Param) Map() bool {
	return p.Type == "object" && len(p.Fields) == 0 && scalar(p.Values)
}

// value is the type of one flag value: the parameter's, or its
// elements' when it repeats, or its values' when it is a map.
func (p Param) value() string {
	switch {
	case p.Repeated():
		return p.Items
	case p.Map():
		return p.Values
	}
	return p.Type
}

func flagName(name string) bool {
	return name != "" && !strings.HasPrefix(name, "-") && !strings.Contains(name, "=")
}

// nests reports whether the parameter's fields have flags named by
// their path through it, which a name holding the separator would make
// ambiguous.
func (p Param) nests() bool {
	return len(p.Fields) > 0 && flagName(p.Name) && !strings.Contains(p.Name, ".")
}

// flagSpec is one flag of a command: a parameter or a field, and the
// path of names from the top that is the flag's name.
type flagSpec struct {
	path  []string
	param Param
	// required is whether the arguments must give the flag's value:
	// the parameter and every object holding it are required.
	required bool
}

// flagsOf lists the flags of params in schema order, each object's
// fields where the object stands. A name already listed is skipped, so
// a top-level property spelled as a field's path keeps its flag.
func flagsOf(params []Param) []flagSpec {
	var out []flagSpec
	seen := map[string]bool{}
	var walk func(prefix []string, p Param, required bool)
	walk = func(prefix []string, p Param, required bool) {
		path := append(append([]string(nil), prefix...), p.Name)
		required = required && p.Required
		switch {
		case p.flagAt(prefix):
			if name := strings.Join(path, "."); !seen[name] {
				seen[name] = true
				out = append(out, flagSpec{path: path, param: p, required: required})
			}
		case p.nests():
			for _, f := range p.Fields {
				walk(path, f, required)
			}
		}
	}
	for _, p := range params {
		walk(nil, p, true)
	}
	return out
}

// flagAt reports whether the parameter, under the path prefix, has a
// flag of its own: a field's name may not hold the separator.
func (p Param) flagAt(prefix []string) bool {
	return p.Flag() && (len(prefix) == 0 || !strings.Contains(p.Name, "."))
}

// needsJSON reports whether some value of the parameter, under prefix,
// can be given only in the JSON argument; requiredOnly asks the same of
// a value the arguments must give.
func (p Param) needsJSON(prefix []string, requiredOnly bool) bool {
	if requiredOnly && !p.Required {
		return false
	}
	switch {
	case p.flagAt(prefix):
		return false
	case p.nests():
		path := append(append([]string(nil), prefix...), p.Name)
		for _, f := range p.Fields {
			if f.needsJSON(path, requiredOnly) {
				return true
			}
		}
		return false
	}
	return true
}

func scalar(typ string) bool {
	switch typ {
	case "string", "integer", "number", "boolean":
		return true
	}
	return false
}

// Commands describes every tool of set, in order. A set the program
// could not run is an error: two tools of one name, a tool with none,
// one named as a command of the program's own, help or schema, or one
// whose name begins with a dash, which the command line would read as
// an option. A
// schema is never an error, so one odd tool cannot take the program's
// other commands down with it; see [Describe].
func Commands(set agenttool.Set) ([]Command, error) {
	if err := set.Validate(); err != nil {
		return nil, fmt.Errorf("cli: %w", err)
	}
	out := make([]Command, 0, len(set))
	for _, t := range set {
		if reserved(t.Name()) {
			return nil, fmt.Errorf("cli: tool %q is named as one of the program's own commands", t.Name())
		}
		if strings.HasPrefix(t.Name(), "-") {
			return nil, fmt.Errorf("cli: tool %q begins with a dash, which a command line reads as an option", t.Name())
		}
		out = append(out, Describe(t))
	}
	return out, nil
}

func reserved(name string) bool {
	return name == "help" || name == "schema"
}

// Describe describes one tool as a command. Its parameters are read
// from the schema the tool serves, so a tool from [agenttool.NewFunc]
// or mcpclient is described as one from [agenttool.New] is. Only the
// keywords the parser needs are read, and leniently: a property whose
// schema is not an object, or whose keywords are not the shape
// expected, has no flag and arrives in the JSON argument; a schema with
// no properties object gives no flags at all; of a name written twice
// the first is described. The tool remains what validates its
// arguments, and the schema is served as the tool gave it.
func Describe(t agenttool.Tool) Command {
	schema := t.Parameters()
	if len(bytes.TrimSpace(schema)) == 0 || string(bytes.TrimSpace(schema)) == "null" {
		schema = agenttool.NoArgsSchema
	}
	return Command{
		Name:        t.Name(),
		Description: t.Description(),
		Annotations: agenttool.AnnotationsOf(t),
		Params:      paramsOf(schema),
		Schema:      schema,
	}
}

func paramsOf(schema json.RawMessage) []Param {
	var root map[string]json.RawMessage
	if json.Unmarshal(schema, &root) != nil {
		return nil
	}
	return propertiesOf(root)
}

// propertiesOf reads the properties of an object schema, and of each
// object among them, in the order written.
func propertiesOf(root map[string]json.RawMessage) []Param {
	var required []string
	_ = json.Unmarshal(root["required"], &required)
	isRequired := make(map[string]bool, len(required))
	for _, name := range required {
		isRequired[name] = true
	}
	var params []Param
	seen := map[string]bool{}
	members(root["properties"], func(name string, raw json.RawMessage) {
		if seen[name] {
			return
		}
		seen[name] = true
		p := Param{Name: name, Required: isRequired[name]}
		var n map[string]json.RawMessage
		if json.Unmarshal(raw, &n) == nil {
			_ = json.Unmarshal(n["description"], &p.Description)
			p.Enum = enumOf(n["enum"])
			p.Type = typeOf(n["type"])
			var items map[string]json.RawMessage
			if p.Type == "array" && json.Unmarshal(n["items"], &items) == nil {
				p.Items = typeOf(items["type"])
			}
			if p.Type == "object" {
				p.Fields = propertiesOf(n)
				var values map[string]json.RawMessage
				if len(p.Fields) == 0 && json.Unmarshal(n["additionalProperties"], &values) == nil {
					p.Values = typeOf(values["type"])
				}
			}
		}
		params = append(params, p)
	})
	return params
}

// members calls fn for each member of the JSON object raw, in the
// order written, which a map would lose. Anything but an object has no
// members, and a member that does not decode ends the walk.
func members(raw json.RawMessage, fn func(name string, value json.RawMessage)) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return
		}
		name, _ := tok.(string)
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return
		}
		fn(name, value)
	}
}

// typeOf reads a schema's type keyword: a name, or a union of one name
// and null, as strict mode writes a pointer field. Anything else, no
// type or a union of several, is "".
func typeOf(raw json.RawMessage) string {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return one
	}
	var union []string
	if json.Unmarshal(raw, &union) != nil {
		return ""
	}
	typ := ""
	for _, t := range union {
		switch {
		case t == "null":
		case typ == "":
			typ = t
		default:
			return ""
		}
	}
	return typ
}

// convert turns one flag value into the JSON of the type typ names.
// Whether the value is in the enum is left to the tool, which says so
// in the words the model would read.
func convert(typ, s string) (json.RawMessage, error) {
	switch typ {
	case "string":
		if !utf8.ValidString(s) {
			return nil, fmt.Errorf("%q is not valid UTF-8", s)
		}
		return json.Marshal(s)
	case "integer":
		// Written back from the parsed value, so +5 and 007 become JSON.
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return json.RawMessage(strconv.FormatInt(n, 10)), nil
		}
		if n, err := strconv.ParseUint(s, 10, 64); err == nil {
			return json.RawMessage(strconv.FormatUint(n, 10)), nil
		}
		// A JSON number with no fraction is an integer to JSON Schema
		// however it is written, 1e3 or past 64 bits included.
		if number(s) {
			if f, ok := new(big.Float).SetString(s); ok && f.IsInt() {
				return json.RawMessage(s), nil
			}
		}
		return nil, fmt.Errorf("%q is not an integer", s)
	case "number":
		// Passed through as written when it is already JSON, so that a
		// value float64 cannot hold reaches the tool unrounded.
		if number(s) {
			return json.RawMessage(s), nil
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
			return nil, fmt.Errorf("%q is not a number", s)
		}
		return json.Marshal(f)
	case "boolean":
		b, err := strconv.ParseBool(s)
		if err != nil {
			return nil, fmt.Errorf("%q is not true or false", s)
		}
		return json.Marshal(b)
	}
	return nil, fmt.Errorf("a %s has no flag", typ)
}

// number reports whether s is a JSON number as written, which json.Valid
// alone does not say: it accepts any JSON value.
func number(s string) bool {
	if s == "" || (s[0] != '-' && (s[0] < '0' || s[0] > '9')) {
		return false
	}
	return json.Valid([]byte(s))
}

// enumOf reads an enum keyword with numbers kept as written, so a value
// float64 would round is shown to the model as the tool will accept it.
func enumOf(raw json.RawMessage) []any {
	if len(raw) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var enum []any
	if dec.Decode(&enum) != nil {
		return nil
	}
	return enum
}
