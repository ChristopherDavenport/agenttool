package mcpclient

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"

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
// against: the tool's output schema, resolved, or the error that kept it
// from being resolved. A nil check is a tool that declares none.
type outputCheck struct {
	resolved *jsonschema.Resolved
	err      error
}

// outputCheckOf resolves t's output schema, and returns nil when it
// declares none. A schema the validator cannot read is kept as its
// error, so the tool is listed and each call says why its result could
// not be checked.
func outputCheckOf(t *sdk.Tool) *outputCheck {
	if t.OutputSchema == nil {
		return nil
	}
	raw, err := json.Marshal(t.OutputSchema)
	if err != nil {
		return &outputCheck{err: err}
	}
	if string(raw) == "null" {
		return nil
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(raw, &schema); err != nil {
		return &outputCheck{err: err}
	}
	switch schema.Schema {
	case "", "http://json-schema.org/draft-07/schema#", "https://json-schema.org/draft-07/schema#", "https://json-schema.org/draft/2020-12/schema":
	default:
		return &outputCheck{err: fmt.Errorf("unsupported $schema %q: the validator handles draft-07 and 2020-12", schema.Schema)}
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		return &outputCheck{err: err}
	}
	return &outputCheck{resolved: resolved}
}

// check validates a successful result's structured content against the
// output schema, a missing one included. The error begins "invalid
// result:" and says the call ran, so the model reads it as a result it
// cannot trust rather than a call that failed and may be made again: the
// call has had whatever effect it has, and its outcome is not in doubt.
func (c *outputCheck) check(res *sdk.CallToolResult) error {
	const ran = "invalid result: the call ran, but"
	switch {
	case c.err != nil:
		return fmt.Errorf("%s its result could not be checked: the tool's output schema: %w", ran, c.err)
	case res.StructuredContent == nil:
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
