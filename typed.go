package agenttool

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/ChristopherDavenport/openresponses"
)

// NoArgs is the argument type of a tool that takes no arguments.
type NoArgs struct{}

// Option configures a tool built by [New].
type Option func(*typedOptions)

type typedOptions struct {
	strict       bool
	sequential   bool
	schema       json.RawMessage
	noValidation bool
}

// WithStrict generates the schema under the strict rules and sets the
// strict flag on the function tool.
func WithStrict() Option { return func(o *typedOptions) { o.strict = true } }

// WithSequential marks the tool [Sequential].
func WithSequential() Option { return func(o *typedOptions) { o.sequential = true } }

// WithParameters replaces the reflected schema with schema. Arguments
// are then not validated before decoding, because the tool cannot know
// what the schema promises.
func WithParameters(schema json.RawMessage) Option {
	return func(o *typedOptions) { o.schema = schema }
}

// WithoutValidation skips the argument check against the reflected
// schema, leaving the decoder as the only guard.
func WithoutValidation() Option { return func(o *typedOptions) { o.noValidation = true } }

// New builds a Tool from a typed function. The schema is reflected from
// Args at registration time (see [SchemaOf]); a type that cannot be
// expressed panics here rather than at call time, like a bad regexp in
// regexp.MustCompile. Call.Args is validated against that schema, so a
// missing required property, a wrong type or a value outside an enum
// is returned as an error the model can retry on, then decoded into
// Args with the standard decoder. Properties the schema does not name
// are ignored, or rejected under [WithStrict], whose schema says so.
//
// Out maps to the output the model sees: a string passes through as
// text, openresponses.Contents goes out as parts, a [Result] or an
// openresponses.FunctionCallOutputData is used as is, and anything else
// is marshalled to JSON.
//
// The function can reach its [Call] through [CallFrom] on the context,
// for the call ID or to report progress.
func New[Args, Out any](name, description string, fn func(context.Context, Args) (Out, error), opts ...Option) Tool {
	var o typedOptions
	for _, opt := range opts {
		opt(&o)
	}
	schema := o.schema
	var tree *Schema
	if schema == nil {
		var zero Args
		t := reflect.TypeOf(&zero).Elem()
		if s, ok := schemerFor(t); ok {
			schema = s
		} else {
			var err error
			tree, err = Reflect(t, o.strict)
			if err != nil {
				panic(fmt.Sprintf("tool.New(%q): %v", name, err))
			}
			schema, err = json.Marshal(tree)
			if err != nil {
				panic(fmt.Sprintf("tool.New(%q): %v", name, err))
			}
		}
	}
	if o.noValidation {
		tree = nil
	}
	return &Typed[Args, Out]{
		name:        name,
		description: description,
		schema:      schema,
		tree:        tree,
		strict:      o.strict,
		sequential:  o.sequential,
		fn:          fn,
	}
}

// Typed is the Tool returned by [New].
type Typed[Args, Out any] struct {
	name        string
	description string
	schema      json.RawMessage
	tree        *Schema
	strict      bool
	sequential  bool
	fn          func(context.Context, Args) (Out, error)
}

// Name returns the tool name.
func (t *Typed[Args, Out]) Name() string { return t.name }

// Description returns the tool description.
func (t *Typed[Args, Out]) Description() string { return t.description }

// Parameters returns the argument schema.
func (t *Typed[Args, Out]) Parameters() json.RawMessage { return t.schema }

// Strict reports whether the schema is strict.
func (t *Typed[Args, Out]) Strict() bool { return t.strict }

// Sequential reports whether the tool runs alone.
func (t *Typed[Args, Out]) Sequential() bool { return t.sequential }

// Execute validates and decodes the arguments, calls the function and
// converts the output.
func (t *Typed[Args, Out]) Execute(ctx context.Context, call Call) (Result, error) {
	if t.tree != nil {
		if err := t.tree.ValidateJSON(call.Args); err != nil {
			return Result{}, err
		}
	}
	args, err := Decode[Args](call.Args)
	if err != nil {
		return Result{}, err
	}
	out, err := t.fn(WithCall(ctx, call), args)
	if err != nil {
		return Result{}, err
	}
	return Output(out)
}

// Decode unmarshals the raw arguments into T. Empty arguments decode as
// an empty object. Errors are phrased for the model.
func Decode[T any](raw json.RawMessage) (T, error) {
	var v T
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, fmt.Errorf("invalid arguments: %w", err)
	}
	return v, nil
}

// Output converts a Go value into a Result following the rules of [New].
func Output(v any) (Result, error) {
	switch out := v.(type) {
	case Result:
		return out, nil
	case *Result:
		if out == nil {
			return Result{}, nil
		}
		return *out, nil
	case string:
		return Text(out), nil
	case openresponses.Contents:
		return Parts(out...), nil
	case openresponses.FunctionCallOutputData:
		return Result{Output: out}, nil
	case nil:
		return Result{}, nil
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Pointer && rv.IsNil() {
		return Result{}, nil
	}
	data, err := json.Marshal(v)
	if err != nil {
		return Result{}, fmt.Errorf("encode result: %w", err)
	}
	return Text(string(data)), nil
}

type callKey struct{}

// WithCall attaches the call to ctx so the tool function can find it
// with [CallFrom]. [Typed.Execute] does this before calling the function.
func WithCall(ctx context.Context, call Call) context.Context {
	return context.WithValue(ctx, callKey{}, call)
}

// CallFrom returns the call attached to ctx, if any.
func CallFrom(ctx context.Context) (Call, bool) {
	c, ok := ctx.Value(callKey{}).(Call)
	return c, ok
}

// Progress reports progress on the call attached to ctx. It is a no-op
// when there is no call or the caller did not ask for updates.
func Progress(ctx context.Context, r Result) {
	if c, ok := CallFrom(ctx); ok {
		c.Update(r)
	}
}

var (
	_ Tool       = (*Typed[NoArgs, string])(nil)
	_ Strict     = (*Typed[NoArgs, string])(nil)
	_ Sequential = (*Typed[NoArgs, string])(nil)
)
