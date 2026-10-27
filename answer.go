package agenttool

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/mail"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// Check reports whether a is an answer to q that a harness may give,
// under RFC 0001's rules for questions. Content is present only with
// [ActionAccept], and an empty object is the same as none, whatever the
// action. On accept the content is a JSON object of members q's form
// defines, every member the form requires is present, and each member
// is what the form says of it, as MCP's elicitation restricts a form:
// a string, within minLength and maxLength, of its format, email, uri,
// date or date-time, and one of its enum or its titled choices (oneOf
// or anyOf of const); a number or an integer within minimum and
// maximum; a boolean; or an array of the choices its items offer,
// within minItems and maxItems. A question with no form, a
// confirmation, takes no members.
//
// A harness refuses an answer that fails rather than taking content the
// question did not ask for as consent: a model that wrote
// {"note": "not before Friday"} to a confirmation meant to say no. The
// note goes with any action and is not checked. A form keyword this
// does not know is not checked, so a form a tool wrote more loosely is
// read as far as MCP's restrictions go.
func (q Elicitation) Check(a Answer) error {
	switch a.Action {
	case ActionAccept, ActionDecline, ActionCancel:
	default:
		return fmt.Errorf("unknown action %q: an answer is accept, decline or cancel", a.Action)
	}
	var content map[string]json.RawMessage
	if c := bytes.TrimSpace(a.Content); len(c) > 0 && !bytes.Equal(c, []byte("null")) {
		if err := json.Unmarshal(c, &content); err != nil || content == nil {
			return errors.New("the answer's content is not a JSON object")
		}
	}
	if a.Action != ActionAccept {
		if len(content) > 0 {
			return fmt.Errorf("an answer has content only on accept, not on %s", a.Action)
		}
		return nil
	}
	f := q.form()
	var extra []string
	for name := range content {
		if _, ok := f.Properties[name]; !ok {
			extra = append(extra, name)
		}
	}
	if len(extra) > 0 {
		slices.Sort(extra)
		if len(f.Properties) == 0 {
			return fmt.Errorf("the question asks for no fields, and the answer gives %q", extra)
		}
		return fmt.Errorf("the question's form does not define %q", extra)
	}
	for _, name := range f.Required {
		if _, ok := content[name]; !ok {
			return fmt.Errorf("the question's form requires %q, and the answer leaves it out", name)
		}
	}
	names := make([]string, 0, len(content))
	for name := range content {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if err := f.Properties[name].check(content[name]); err != nil {
			return fmt.Errorf("%q: %w", name, err)
		}
	}
	return nil
}

// form is a question's schema as far as MCP's elicitation restricts
// one: an object of flat members, some required.
type form struct {
	Properties map[string]*formField
	Required   []string
}

// formField is one member's schema: a primitive, or an array of choices.
// A nil field is the schema true, which allows any value.
type formField struct {
	Type      json.RawMessage
	Format    string
	Enum      []json.RawMessage
	Const     json.RawMessage
	OneOf     []*formField
	AnyOf     []*formField
	MinLength *int
	MaxLength *int
	Minimum   *float64
	Maximum   *float64
	Items     *formField
	MinItems  *int
	MaxItems  *int
	// never is the schema false, which allows no value.
	never bool
}

// form returns q's form, read member by member and keyword by keyword,
// so a part that does not decode is skipped rather than losing the rest.
// A question without one, or whose schema is not an object, has an
// empty form.
func (q Elicitation) form() form {
	var top map[string]json.RawMessage
	if len(q.Schema) == 0 || json.Unmarshal(q.Schema, &top) != nil {
		return form{}
	}
	var f form
	var props map[string]json.RawMessage
	if json.Unmarshal(top["properties"], &props) == nil {
		f.Properties = make(map[string]*formField, len(props))
		for name, raw := range props {
			f.Properties[name] = fieldOf(raw)
		}
	}
	_ = json.Unmarshal(top["required"], &f.Required)
	return f
}

// fieldOf reads one schema, keyword by keyword: true is nil, any value;
// false allows nothing; a keyword absent or not of its type is skipped,
// and a count written as an integral number, 1.0, is that integer.
func fieldOf(raw json.RawMessage) *formField {
	var b bool
	if json.Unmarshal(raw, &b) == nil {
		if b {
			return nil
		}
		return &formField{never: true}
	}
	var kw map[string]json.RawMessage
	if json.Unmarshal(raw, &kw) != nil {
		return nil
	}
	f := &formField{Type: kw["type"], Const: kw["const"]}
	_ = json.Unmarshal(kw["format"], &f.Format)
	_ = json.Unmarshal(kw["enum"], &f.Enum)
	count := func(k string) *int {
		var n float64
		if json.Unmarshal(kw[k], &n) != nil || n != math.Trunc(n) || n < 0 || n > math.MaxInt32 {
			return nil
		}
		i := int(n)
		return &i
	}
	bound := func(k string) *float64 {
		var n float64
		if json.Unmarshal(kw[k], &n) != nil {
			return nil
		}
		return &n
	}
	choices := func(k string) []*formField {
		var list []json.RawMessage
		if json.Unmarshal(kw[k], &list) != nil {
			return nil
		}
		out := make([]*formField, len(list))
		for i, c := range list {
			out[i] = fieldOf(c)
		}
		return out
	}
	f.MinLength, f.MaxLength = count("minLength"), count("maxLength")
	f.MinItems, f.MaxItems = count("minItems"), count("maxItems")
	f.Minimum, f.Maximum = bound("minimum"), bound("maximum")
	f.OneOf, f.AnyOf = choices("oneOf"), choices("anyOf")
	if items, ok := kw["items"]; ok {
		f.Items = fieldOf(items)
	}
	return f
}

// types returns the types the field allows, none when it names none.
func (f *formField) types() []string {
	var one string
	if json.Unmarshal(f.Type, &one) == nil {
		return []string{one}
	}
	var many []string
	_ = json.Unmarshal(f.Type, &many)
	return many
}

// check reports whether raw is a value f allows.
func (f *formField) check(raw json.RawMessage) error {
	if f == nil {
		return nil
	}
	if f.never {
		return errors.New("the form allows no value here")
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	if types := f.types(); len(types) > 0 && !slices.ContainsFunc(types, func(t string) bool { return isType(v, t) }) {
		return fmt.Errorf("expected %s, got %s", strings.Join(types, " or "), raw)
	}
	switch v := v.(type) {
	case string:
		n := utf8.RuneCountInString(v)
		if f.MinLength != nil && n < *f.MinLength {
			return fmt.Errorf("shorter than %d characters", *f.MinLength)
		}
		if f.MaxLength != nil && n > *f.MaxLength {
			return fmt.Errorf("longer than %d characters", *f.MaxLength)
		}
		if err := checkFormat(f.Format, v); err != nil {
			return err
		}
	case float64:
		if f.Minimum != nil && v < *f.Minimum {
			return fmt.Errorf("below the minimum %v", *f.Minimum)
		}
		if f.Maximum != nil && v > *f.Maximum {
			return fmt.Errorf("above the maximum %v", *f.Maximum)
		}
	case []any:
		if f.MinItems != nil && len(v) < *f.MinItems {
			return fmt.Errorf("fewer than %d items", *f.MinItems)
		}
		if f.MaxItems != nil && len(v) > *f.MaxItems {
			return fmt.Errorf("more than %d items", *f.MaxItems)
		}
		for i, e := range v {
			data, _ := json.Marshal(e)
			if err := f.Items.check(data); err != nil {
				return fmt.Errorf("item %d: %w", i, err)
			}
		}
	}
	return f.checkChoice(v, raw)
}

// checkChoice reports whether v is one of the values f offers: its
// enum, its const, or one of its titled choices.
func (f *formField) checkChoice(v any, raw json.RawMessage) error {
	if f.Enum != nil && !slices.ContainsFunc(f.Enum, func(e json.RawMessage) bool { return sameJSON(e, v) }) {
		return fmt.Errorf("%s is not one of the choices offered", raw)
	}
	if len(f.Const) > 0 && !sameJSON(f.Const, v) {
		return fmt.Errorf("%s is not %s", raw, f.Const)
	}
	for _, choices := range [][]*formField{f.OneOf, f.AnyOf} {
		if len(choices) > 0 && !slices.ContainsFunc(choices, func(c *formField) bool { return c.check(raw) == nil }) {
			return fmt.Errorf("%s is not one of the choices offered", raw)
		}
	}
	return nil
}

// sameJSON reports whether the JSON e decodes to v.
func sameJSON(e json.RawMessage, v any) bool {
	var w any
	return json.Unmarshal(e, &w) == nil && reflect.DeepEqual(w, v)
}

// isType reports whether v, decoded from JSON, is of the JSON Schema
// type t. A type this does not know allows anything.
func isType(v any, t string) bool {
	switch t {
	case "string":
		_, ok := v.(string)
		return ok
	case "number":
		_, ok := v.(float64)
		return ok
	case "integer":
		n, ok := v.(float64)
		return ok && n == math.Trunc(n)
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "null":
		return v == nil
	}
	return true
}

// checkFormat reports whether s is of the format MCP's elicitation
// names: email, uri, date or date-time. Another format is not checked.
func checkFormat(format, s string) error {
	var err error
	switch format {
	case "email":
		var a *mail.Address
		if a, err = mail.ParseAddress(s); err == nil && a.Address != s {
			err = errors.New("not a bare address")
		}
	case "uri":
		var u *url.URL
		if u, err = url.Parse(s); err == nil && u.Scheme == "" {
			err = errors.New("no scheme")
		}
	case "date":
		_, err = time.Parse(time.DateOnly, s)
	case "date-time":
		// RFC 3339 allows t and z in lower case, section 5.6; Go's layout
		// takes them in upper case alone. A space in place of T is not in
		// its grammar, and stays refused.
		_, err = time.Parse(time.RFC3339, strings.Map(func(r rune) rune {
			if r == 't' || r == 'z' {
				return r - 'a' + 'A'
			}
			return r
		}, s))
	default:
		return nil
	}
	if err != nil {
		return fmt.Errorf("%q is not a %s: %w", s, format, err)
	}
	return nil
}
