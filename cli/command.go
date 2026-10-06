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
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

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

// Param is one top-level property of a tool's arguments.
type Param struct {
	Name        string
	Description string
	// Type is the property's JSON Schema type, with a nullable union
	// reduced to the type it admits besides null; empty when the schema
	// names none or several.
	Type string
	// Items is the type of an array's elements, read as Type is; empty
	// for anything but an array.
	Items    string
	Required bool
	Enum     []any
}

// Flag reports whether the parameter has a flag: a string, integer,
// number or boolean has one, and an array of them has one that repeats.
// Anything else, an object, a map, an array of objects or a value of
// any type, arrives only in the JSON argument, as does a property whose
// name a flag cannot carry: empty, starting with a dash, or holding an
// equals sign.
func (p Param) Flag() bool {
	if p.Name == "" || strings.HasPrefix(p.Name, "-") || strings.Contains(p.Name, "=") {
		return false
	}
	return scalar(p.Type) || p.Repeated()
}

// Repeated reports whether the parameter's flag is given once per
// element of an array.
func (p Param) Repeated() bool {
	return p.Type == "array" && scalar(p.Items)
}

// value is the type of one flag value: the parameter's, or its
// elements' when it repeats.
func (p Param) value() string {
	if p.Repeated() {
		return p.Items
	}
	return p.Type
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
// one named as a command of the program's own, help or schema, or a
// schema that is not a JSON object.
func Commands(set agenttool.Set) ([]Command, error) {
	if err := set.Validate(); err != nil {
		return nil, fmt.Errorf("cli: %w", err)
	}
	out := make([]Command, 0, len(set))
	for _, t := range set {
		if reserved(t.Name()) {
			return nil, fmt.Errorf("cli: tool %q is named as one of the program's own commands", t.Name())
		}
		c, err := Describe(t)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

func reserved(name string) bool {
	return name == "help" || name == "schema"
}

// Describe describes one tool as a command. Its parameters are read
// from the schema the tool serves, so a tool from [agenttool.NewFunc]
// or mcpclient is described as one from [agenttool.New] is. Only the
// keywords the parser needs are read, and the schema is otherwise
// passed through: a keyword this does not know does not make a property
// lose its flag, and the tool remains what validates its arguments.
func Describe(t agenttool.Tool) (Command, error) {
	schema := t.Parameters()
	if len(schema) == 0 {
		schema = agenttool.NoArgsSchema
	}
	params, err := paramsOf(schema)
	if err != nil {
		return Command{}, fmt.Errorf("cli: tool %q: %w", t.Name(), err)
	}
	return Command{
		Name:        t.Name(),
		Description: t.Description(),
		Annotations: agenttool.AnnotationsOf(t),
		Params:      params,
		Schema:      schema,
	}, nil
}

// node is the part of a property's schema a command needs.
type node struct {
	Type        json.RawMessage `json:"type"`
	Description string          `json:"description"`
	Enum        []any           `json:"enum"`
	Items       *node           `json:"items"`
}

func paramsOf(schema json.RawMessage) ([]Param, error) {
	var root struct {
		Properties json.RawMessage `json:"properties"`
		Required   []string        `json:"required"`
	}
	if err := json.Unmarshal(schema, &root); err != nil {
		return nil, fmt.Errorf("parameters schema: %w", err)
	}
	if len(root.Properties) == 0 || string(root.Properties) == "null" {
		return nil, nil
	}
	required := make(map[string]bool, len(root.Required))
	for _, name := range root.Required {
		required[name] = true
	}
	var params []Param
	seen := map[string]bool{}
	err := members(root.Properties, func(name string, raw json.RawMessage) error {
		if seen[name] {
			return fmt.Errorf("property %q appears twice", name)
		}
		seen[name] = true
		var n node
		if err := json.Unmarshal(raw, &n); err != nil {
			return fmt.Errorf("property %q: %w", name, err)
		}
		p := Param{
			Name:        name,
			Description: n.Description,
			Type:        typeOf(n.Type),
			Required:    required[name],
			Enum:        n.Enum,
		}
		if p.Type == "array" && n.Items != nil {
			p.Items = typeOf(n.Items.Type)
		}
		params = append(params, p)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("parameters schema: %w", err)
	}
	return params, nil
}

// members calls fn for each member of the JSON object raw, in the
// order written, which a map would lose.
func members(raw json.RawMessage, fn func(name string, value json.RawMessage) error) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if tok != json.Delim('{') {
		return errors.New("properties is not an object")
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		name, _ := tok.(string)
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return err
		}
		if err := fn(name, value); err != nil {
			return err
		}
	}
	return nil
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
		return json.Marshal(s)
	case "integer":
		if _, err := strconv.ParseInt(s, 10, 64); err == nil {
			return json.RawMessage(s), nil
		}
		if _, err := strconv.ParseUint(s, 10, 64); err == nil {
			return json.RawMessage(s), nil
		}
		return nil, fmt.Errorf("%q is not an integer", s)
	case "number":
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
