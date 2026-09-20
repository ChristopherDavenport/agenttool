// Package agenttool is the tool contract for Go agents over Open
// Responses: what a tool is, how a typed Go function becomes one, how
// its JSON Schema is generated and validated, and how a batch of calls
// executes.
//
// The package imports openresponses and the standard library only, so a
// tool written with [New] can be handed to any loop that speaks Open
// Responses. agentturn is one such loop and depends on this module; the
// reverse never holds, and a test enforces the boundary. The MCP
// adapters, mcpclient and mcpserver, are nested modules that map to and
// from this contract.
//
//	type ReadFileArgs struct {
//		Path     string `json:"path" desc:"Absolute path to read"`
//		MaxBytes int    `json:"max_bytes,omitempty" desc:"Stop after this many bytes"`
//	}
//
//	var ReadFile = agenttool.New("read_file", "Read a file from disk",
//		func(ctx context.Context, a ReadFileArgs) (string, error) { ... })
//
// Errors returned from Execute become error outputs the model sees; a
// tool never encodes an error as normal content.
package agenttool

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ChristopherDavenport/openresponses"
)

// Tool is something the model can call. Name and Parameters become the
// function tool on the request; Execute runs one call.
type Tool interface {
	Name() string
	Description() string
	// Parameters is the JSON Schema of the arguments object. nil means
	// the tool takes no arguments.
	Parameters() json.RawMessage
	// Execute runs one call. On error the model sees the error and
	// Result.Output is ignored; Result.Details may still be set for
	// subscribers, as mcpclient does with the raw MCP result.
	Execute(ctx context.Context, call Call) (Result, error)
}

// Call is one invocation of a tool.
type Call struct {
	// ID is the call_id of the function_call item.
	ID string
	// Args is the raw JSON arguments object as the model wrote it.
	Args json.RawMessage
	// OnUpdate, when set, receives progress before the final result. It
	// may be called from the tool's goroutine; the caller serialises it.
	OnUpdate func(Result)
}

// Update reports progress to the caller when it asked for it.
func (c Call) Update(r Result) {
	if c.OnUpdate != nil {
		c.OnUpdate(r)
	}
}

// Result is what a tool produced.
type Result struct {
	// Output is what the model sees.
	Output openresponses.FunctionCallOutputData
	// Details is app-only data for subscribers and fronts. It is never
	// sent to the model.
	Details any
	// Terminate hints that the loop should stop after this batch instead
	// of calling the model again. The loop honours it only when every
	// result in the batch sets it.
	Terminate bool
}

// ProgressInfo, when set as the Details of a progress update, carries
// the numbers behind it: how far the tool is, out of how much, and a
// message. The fields mirror the MCP progress notification so the two
// adapters forward it in either direction; a tool with no numbers to
// report leaves Details unset and sends text alone.
type ProgressInfo struct {
	Progress float64
	Total    float64
	Message  string
}

// Text builds a result whose output is a string.
func Text(s string) Result {
	return Result{Output: openresponses.FunctionCallOutputData{Text: s}}
}

// Parts builds a result whose output is a list of content parts.
func Parts(parts ...openresponses.Content) Result {
	return Result{Output: openresponses.FunctionCallOutputData{Parts: openresponses.Contents(parts)}}
}

// ErrorResult builds the output the model sees when a tool fails. The
// text form is "Error: <message>" so the model can tell it apart from a
// normal result and retry.
func ErrorResult(err error) Result {
	return Text("Error: " + err.Error())
}

// Sequential is implemented by tools that must not run alongside other
// tools in the same batch. When any tool in a batch reports true, the
// whole batch runs one call at a time in the model's order.
type Sequential interface {
	Sequential() bool
}

// Strict is implemented by tools whose schema was generated under the
// strict rules (every field required, additionalProperties false,
// optional fields nullable). The flag is set on the function tool.
type Strict interface {
	Strict() bool
}

// IsSequential reports whether t asks to run alone.
func IsSequential(t Tool) bool {
	s, ok := t.(Sequential)
	return ok && s.Sequential()
}

// IsStrict reports whether t's schema is strict.
func IsStrict(t Tool) bool {
	s, ok := t.(Strict)
	return ok && s.Strict()
}

// Definition builds the function tool that describes t on a request.
func Definition(t Tool) *openresponses.FunctionTool {
	ft := openresponses.NewFunctionTool(t.Name(), t.Description(), t.Parameters())
	if IsStrict(t) {
		strict := true
		ft.Strict = &strict
	}
	return ft
}

// Set is a list of tools with lookup by name.
type Set []Tool

// Lookup returns the tool named name.
func (s Set) Lookup(name string) (Tool, bool) {
	for _, t := range s {
		if t.Name() == name {
			return t, true
		}
	}
	return nil, false
}

// Definitions returns the function tools for a request, in order.
func (s Set) Definitions() openresponses.Tools {
	if len(s) == 0 {
		return nil
	}
	out := make(openresponses.Tools, 0, len(s))
	for _, t := range s {
		out = append(out, Definition(t))
	}
	return out
}

// Validate reports an empty or duplicate name.
func (s Set) Validate() error {
	seen := make(map[string]bool, len(s))
	for i, t := range s {
		name := t.Name()
		if name == "" {
			return fmt.Errorf("tool[%d]: empty name", i)
		}
		if seen[name] {
			return fmt.Errorf("tool %q: duplicate name", name)
		}
		seen[name] = true
	}
	return nil
}

// NewFunc builds a Tool from plain values and a function that takes the
// raw call: the untyped counterpart of [New] for tools whose schema
// comes from elsewhere, such as a remote server. parameters is served
// verbatim and never validated; nil means the tool takes no arguments.
// [WithStrict] and [WithSequential] apply; the options that shape a
// reflected schema do not. A nil fn panics here, like a bad schema in
// [New].
func NewFunc(name, description string, parameters json.RawMessage, fn func(ctx context.Context, call Call) (Result, error), opts ...Option) Tool {
	if fn == nil {
		panic(fmt.Sprintf("agenttool.NewFunc(%q): nil function", name))
	}
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	return &funcTool{name: name, description: description, schema: parameters, fn: fn, strict: o.strict, sequential: o.sequential}
}

type funcTool struct {
	name        string
	description string
	schema      json.RawMessage
	fn          func(ctx context.Context, call Call) (Result, error)
	strict      bool
	sequential  bool
}

func (f *funcTool) Name() string                { return f.name }
func (f *funcTool) Description() string         { return f.description }
func (f *funcTool) Parameters() json.RawMessage { return f.schema }
func (f *funcTool) Sequential() bool            { return f.sequential }
func (f *funcTool) Strict() bool                { return f.strict }

func (f *funcTool) Execute(ctx context.Context, call Call) (Result, error) {
	return f.fn(ctx, call)
}

var (
	_ Tool       = (*funcTool)(nil)
	_ Sequential = (*funcTool)(nil)
	_ Strict     = (*funcTool)(nil)
)
