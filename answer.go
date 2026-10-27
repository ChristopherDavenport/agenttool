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
	Properties map[string]*formField `json:"properties"`
	Required   []string              `json:"required"`
}

// formField is one member's schema: a primitive, or an array of choices.
type formField struct {
	Type      json.RawMessage   `json:"type"`
	Format    string            `json:"format"`
	Enum      []json.RawMessage `json:"enum"`
	Const     json.RawMessage   `json:"const"`
	OneOf     []*formField      `json:"oneOf"`
	AnyOf     []*formField      `json:"anyOf"`
	MinLength *int              `json:"minLength"`
	MaxLength *int              `json:"maxLength"`
	Minimum   *float64          `json:"minimum"`
	Maximum   *float64          `json:"maximum"`
	Items     *formField        `json:"items"`
	MinItems  *int              `json:"minItems"`
	MaxItems  *int              `json:"maxItems"`
}

// form returns q's form, empty for a question without one or with a
// schema that does not decode as one.
func (q Elicitation) form() form {
	var f form
	if len(q.Schema) > 0 && json.Unmarshal(q.Schema, &f) == nil {
		return f
	}
	return form{}
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
		if len(choices) > 0 && !slices.ContainsFunc(choices, func(c *formField) bool { return c != nil && c.check(raw) == nil }) {
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
		_, err = time.Parse(time.RFC3339, s)
	default:
		return nil
	}
	if err != nil {
		return fmt.Errorf("%q is not a %s: %w", s, format, err)
	}
	return nil
}
