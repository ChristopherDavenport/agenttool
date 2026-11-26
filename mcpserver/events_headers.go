package mcpserver

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// schemaCheck is a JSON Schema ready to check instances against.
type schemaCheck struct{ resolved *jsonschema.Resolved }

func newSchemaCheck(raw json.RawMessage) (*schemaCheck, error) {
	r, err := resolveSchema(raw)
	if err != nil {
		return nil, err
	}
	return &schemaCheck{resolved: r}, nil
}

func (c *schemaCheck) validate(raw json.RawMessage) error {
	var instance any
	if err := json.Unmarshal(raw, &instance); err != nil {
		return err
	}
	return c.resolved.Validate(instance)
}

// objectRoot says whether the schema's root type is "object".
func objectRoot(raw json.RawMessage) bool {
	var s struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(raw, &s) == nil && s.Type == "object"
}

const (
	nameHeader        = "Mcp-Name"
	paramHeaderPrefix = "Mcp-Param-"
	protocolHeader    = "Mcp-Protocol-Version"
	// headersFrom is the first protocol version whose clients send the
	// routing headers.
	headersFrom  = "2026-07-28"
	base64Prefix = "=?base64?"
	base64Suffix = "?="
	maxSafeInt   = 1<<53 - 1
)

// headerBinding ties an argument, by its path of property names, to the
// header that carries it.
type headerBinding struct {
	path   []string
	header string
}

type headerProp struct {
	Type       json.RawMessage       `json:"type"`
	Header     json.RawMessage       `json:"x-mcp-header"`
	Properties map[string]headerProp `json:"properties"`
}

// scanHeaderBindings finds the x-mcp-header annotations of an input
// schema, on properties reached through "properties" alone, and checks
// them against the transport's rules: a name of HTTP token characters,
// no two the same ignoring case, and a property of type string, integer
// or boolean.
func scanHeaderBindings(schema json.RawMessage) ([]headerBinding, error) {
	var root headerProp
	if err := json.Unmarshal(schema, &root); err != nil {
		return nil, fmt.Errorf("input schema: %w", err)
	}
	var out []headerBinding
	seen := map[string]bool{}
	var walk func(props map[string]headerProp, prefix []string) error
	walk = func(props map[string]headerProp, prefix []string) error {
		names := make([]string, 0, len(props))
		for n := range props {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			p := props[n]
			path := append(append([]string(nil), prefix...), n)
			if len(p.Header) > 0 {
				var h string
				if err := json.Unmarshal(p.Header, &h); err != nil || h == "" {
					return fmt.Errorf("x-mcp-header of %q must be a non-empty string", strings.Join(path, "."))
				}
				for _, c := range h {
					if !isTokenChar(c) {
						return fmt.Errorf("x-mcp-header %q of %q has the character %q, which a header name may not", h, strings.Join(path, "."), c)
					}
				}
				var typ string
				_ = json.Unmarshal(p.Type, &typ)
				if typ != "string" && typ != "integer" && typ != "boolean" {
					return fmt.Errorf("x-mcp-header on %q needs type string, integer or boolean, not %q", strings.Join(path, "."), typ)
				}
				if seen[strings.ToLower(h)] {
					return fmt.Errorf("x-mcp-header %q is used twice", h)
				}
				seen[strings.ToLower(h)] = true
				out = append(out, headerBinding{path: path, header: h})
			}
			if err := walk(p.Properties, path); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(root.Properties, nil); err != nil {
		return nil, err
	}
	return out, nil
}

func isTokenChar(c rune) bool {
	switch {
	case c >= '0' && c <= '9', c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z':
		return true
	}
	return strings.ContainsRune("!#$%&'*+-.^_`|~", c)
}

// mismatch is a header that disagrees with the request's params.
type mismatch struct {
	header string
	body   any
	why    string
}

func (m *mismatch) err() *EventsError {
	return &EventsError{
		Code:    EventsCodeHeaderMismatch,
		Message: "Bad Request: the request headers and body disagree: " + m.why,
		Data:    map[string]any{"mismatch": map[string]any{"header": m.header, "body": m.body}},
	}
}

// checkHeaders validates the routing headers of an events request, and
// returns nil when they agree or there are none to check: no request
// headers (stdio) or a protocol before 2026-07-28, whose clients send no
// such headers. events/list has no Mcp-Name; the other four need one
// that is the params' name, and an Mcp-Param-* for each annotated
// argument the params carry.
func (e *Events) checkHeaders(method string, p *eventsParams, h http.Header) *EventsError {
	if h == nil || h.Get(protocolHeader) < headersFrom || method == EventsListMethod || p == nil || p.Name == "" {
		return nil
	}
	got := h.Get(nameHeader)
	if got == "" {
		return (&mismatch{header: nameHeader, body: p.Name, why: "the Mcp-Name header is missing"}).err()
	}
	dec, ok := decodeHeaderValue(got)
	if !ok {
		return (&mismatch{header: nameHeader, body: p.Name, why: "the Mcp-Name header is not valid Base64"}).err()
	}
	if dec != p.Name {
		return (&mismatch{header: nameHeader, body: p.Name, why: fmt.Sprintf("Mcp-Name %q, params name %q", dec, p.Name)}).err()
	}
	t := e.lookup(p.Name)
	if t == nil || len(t.bind) == 0 {
		return nil
	}
	var args map[string]json.RawMessage
	if len(p.Arguments) > 0 {
		_ = json.Unmarshal(p.Arguments, &args)
	}
	for _, b := range t.bind {
		full := paramHeaderPrefix + b.header
		hv := h.Get(full)
		raw, present := lookupPath(args, b.path)
		if !present || string(raw) == "null" {
			if hv != "" {
				return (&mismatch{header: full, body: nil, why: fmt.Sprintf("%s is sent but the argument %q is absent", full, strings.Join(b.path, "."))}).err()
			}
			continue
		}
		var body any
		_ = json.Unmarshal(raw, &body)
		if hv == "" {
			return (&mismatch{header: full, body: body, why: fmt.Sprintf("%s is missing for the argument %q", full, strings.Join(b.path, "."))}).err()
		}
		dec, ok := decodeHeaderValue(hv)
		if !ok {
			return (&mismatch{header: full, body: body, why: full + " is not valid Base64"}).err()
		}
		if !headerEquals(dec, body) {
			return (&mismatch{header: full, body: body, why: fmt.Sprintf("%s %q, argument %q differs", full, dec, strings.Join(b.path, "."))}).err()
		}
	}
	return nil
}

func lookupPath(args map[string]json.RawMessage, path []string) (json.RawMessage, bool) {
	cur, ok := args[path[0]]
	if !ok {
		return nil, false
	}
	for _, part := range path[1:] {
		var obj map[string]json.RawMessage
		if json.Unmarshal(cur, &obj) != nil {
			return nil, false
		}
		if cur, ok = obj[part]; !ok {
			return nil, false
		}
	}
	return cur, true
}

// headerEquals compares a decoded header with a JSON primitive: strings
// and booleans as text, integers as numbers.
func headerEquals(header string, body any) bool {
	switch v := body.(type) {
	case string:
		return header == v
	case bool:
		return header == strconv.FormatBool(v)
	case float64:
		if v != math.Trunc(v) || math.Abs(v) > maxSafeInt {
			return false
		}
		n, err := strconv.ParseFloat(header, 64)
		return err == nil && n == v
	}
	return false
}

// decodeHeaderValue reads a header value, which is Base64 between the
// sentinels when it could not travel as it is.
func decodeHeaderValue(v string) (string, bool) {
	if enc, ok := strings.CutPrefix(v, base64Prefix); ok {
		if enc, ok = strings.CutSuffix(enc, base64Suffix); ok {
			b, err := base64.StdEncoding.DecodeString(enc)
			if err != nil {
				return "", false
			}
			return string(b), true
		}
	}
	return v, true
}

// requestInfo is what the middleware knows of a request and a handler,
// which sees no headers, is given by the context.
type requestInfo struct {
	extra     *sdk.RequestExtra
	principal string
}

type requestInfoKey struct{}
