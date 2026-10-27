package mcpclient

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// carries reports whether one of texts already holds the JSON of
// structured, as MCP asks a server to send it: a text that parses to a
// value equal to it under RFC 8785, the canonical form RFC 0001 hashes
// definitions in. The structured content is then not sent to the model a
// second time.
func carries(texts []string, structured []byte) bool {
	want, ok := decodeJSON(structured)
	if !ok {
		return false
	}
	for _, t := range texts {
		if got, ok := decodeJSON([]byte(t)); ok && jcsEqual(got, want) {
			return true
		}
	}
	return false
}

// decodeJSON decodes one JSON value, numbers as their literal text, and
// reports false for anything else, trailing data included.
func decodeJSON(data []byte) (any, bool) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	if len(bytes.TrimSpace(data[dec.InputOffset():])) > 0 {
		return nil, false
	}
	return v, true
}

// jcsEqual reports whether a and b serialise to the same bytes under
// RFC 8785: objects with the same members, compared without regard to
// order; arrays with equal elements in order; equal strings, booleans
// and nulls; and numbers equal as IEEE 754 doubles, which is what the
// canonical form writes, so 1, 1.0 and 1e0 are one number and -0 is 0.
// A number a double cannot hold has no canonical form and equals
// nothing.
func jcsEqual(a, b any) bool {
	switch x := a.(type) {
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, xv := range x {
			yv, ok := y[k]
			if !ok || !jcsEqual(xv, yv) {
				return false
			}
		}
		return true
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !jcsEqual(x[i], y[i]) {
				return false
			}
		}
		return true
	case json.Number:
		y, ok := b.(json.Number)
		if !ok {
			return false
		}
		xf, err := strconv.ParseFloat(string(x), 64)
		if err != nil {
			return false
		}
		yf, err := strconv.ParseFloat(string(y), 64)
		if err != nil {
			return false
		}
		return xf == yf
	default:
		// string, bool and nil compare as values.
		return a == b
	}
}

// outputCheck is what a consumed tool checks its structured content
// against: the tool's output schema, resolved. A nil check is a tool
// that declares none, or one whose schema the validator cannot read.
type outputCheck struct {
	resolved *jsonschema.Resolved
}

// outputCheckOf resolves t's output schema. It returns nil when the
// tool declares none, and nil with the reason when the validator cannot
// read the one it declares: the tool is then unchecked, as a tool with
// no schema is, rather than failing every call for a fault in its
// listing, and the caller tells the host why.
func outputCheckOf(t *sdk.Tool) (*outputCheck, error) {
	if t.OutputSchema == nil {
		return nil, nil
	}
	raw, err := json.Marshal(t.OutputSchema)
	if err != nil {
		return nil, err
	}
	if string(raw) == "null" {
		return nil, nil
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil, err
	}
	if schema.Schema != "" {
		d, ok := dialect(schema.Schema)
		if !ok {
			return nil, fmt.Errorf("unsupported $schema %q: the validator handles draft-07 and 2020-12", schema.Schema)
		}
		schema.Schema = d
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		return nil, err
	}
	return &outputCheck{resolved: resolved}, nil
}

// dialect returns the spelling of the $schema the validator takes for
// uri, when uri names draft-07 or 2020-12 however it is spelt: over
// http or https, with or without the empty fragment. jsonschema-go
// compares the string itself, so the schema is given its spelling.
func dialect(uri string) (string, bool) {
	uri = strings.TrimSuffix(uri, "#")
	if rest, ok := strings.CutPrefix(uri, "https://"); ok {
		uri = rest
	} else if rest, ok := strings.CutPrefix(uri, "http://"); ok {
		uri = rest
	} else {
		return "", false
	}
	switch uri {
	case "json-schema.org/draft-07/schema":
		return "http://json-schema.org/draft-07/schema#", true
	case "json-schema.org/draft/2020-12/schema":
		return "https://json-schema.org/draft/2020-12/schema", true
	}
	return "", false
}

// check validates a successful result's structured content against the
// output schema, a missing one included. The error begins "invalid
// result:" and says the call ran, so the model reads it as a result it
// cannot trust rather than a call that failed and may be made again: the
// call has had whatever effect it has, and its outcome is not in doubt.
func (c *outputCheck) check(res *sdk.CallToolResult) error {
	const ran = "invalid result: the call ran, but"
	if res.StructuredContent == nil {
		return fmt.Errorf("%s the tool declares an output schema and its result has no structured content", ran)
	}
	// The SDK decoded the content into Go values; round trip it so the
	// validator sees JSON's types.
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		return fmt.Errorf("%s its structured content does not encode: %w", ran, err)
	}
	var instance any
	if err := json.Unmarshal(raw, &instance); err != nil {
		return fmt.Errorf("%s its structured content does not decode: %w", ran, err)
	}
	if err := c.resolved.Validate(instance); err != nil {
		return fmt.Errorf("%s its structured content does not match the tool's output schema: %w", ran, err)
	}
	return nil
}
