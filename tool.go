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
	// sent to the model. A value that implements [Recordable] can also
	// be written to a session by a recorder that does not know its
	// type; see [RecordOf].
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

// Recordable is implemented by a Details value that is meant to outlive
// the run. A recorder that does not know the type can still write it,
// as a namespaced JSON entry beside the call it came from, through
// [RecordOf]. RecordNS names the entry's namespace, "owner:kind" by
// convention, and must not be empty. The data is the value's JSON as
// json.Marshal produces it, so a type shapes it with MarshalJSON like
// anywhere else; a MarshalJSON on a pointer receiver applies only when
// Details holds the pointer. Details for in-process subscribers alone,
// such as [ProgressInfo] or a live handle, do not implement it and are
// not recorded.
type Recordable interface {
	RecordNS() string
}

// Record is the durable form of a [Recordable] Details value: the
// namespace and the JSON a recorder writes under it.
type Record struct {
	NS   string
	Data json.RawMessage
}

// RecordOf returns the record of details when it implements
// [Recordable], and nil when it is nil or any other value, which is not
// an error: most Details are for subscribers in the same process. An
// empty namespace or a value that does not marshal is an error.
func RecordOf(details any) (*Record, error) {
	rec, ok := details.(Recordable)
	if !ok {
		return nil, nil
	}
	ns := rec.RecordNS()
	if ns == "" {
		return nil, fmt.Errorf("agenttool: record %T: empty namespace", details)
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return nil, fmt.Errorf("agenttool: record %s: %w", ns, err)
	}
	return &Record{NS: ns, Data: data}, nil
}

// RecordFunc writes one record durably. A harness installs it with
// [ContextWithRecorder] so that a tool can put a handle on the record
// at the moment the side effect begins, rather than at the end of the
// call, which is where [RecordOf] reads Result.Details. It is called
// from the tool's goroutine, possibly more than once and possibly
// concurrently with another call's, so an implementation must be safe
// for concurrent use and must have written the record durably before it
// returns; that is the whole point of the seam. The error it returns
// reaches the tool, which decides whether a record it could not write
// fails the call.
//
// The call the record belongs to is on the context: a recorder reads it
// with [CallFrom], which [Executor] fills in for every tool it runs.
type RecordFunc func(ctx context.Context, rec *Record) error

type recorderKey struct{}

// ContextWithRecorder returns ctx carrying fn as the recorder that
// [WriteRecord] calls. A harness installs it around a tool call, per
// call, so the record it writes can be filed beside that call; a nil fn
// removes any recorder already on the context.
func ContextWithRecorder(ctx context.Context, fn RecordFunc) context.Context {
	return context.WithValue(ctx, recorderKey{}, fn)
}

// RecorderFrom returns the recorder on ctx, if any. A tool that would
// spend real work building its details can ask first; [WriteRecord]
// makes the same check.
func RecorderFrom(ctx context.Context) (RecordFunc, bool) {
	fn, ok := ctx.Value(recorderKey{}).(RecordFunc)
	return fn, ok && fn != nil
}

// WriteRecord writes details to the record now, through the recorder on
// ctx, and returns when the write is durable. It is what a tool calls
// during Execute for the handle to something it has just started: a
// process group after the fork, a temporary directory after the
// mkdir, a remote job ID once the server has answered. A tool that is
// killed mid-call leaves nothing behind otherwise, since Result.Details
// is read only when the call ends, and that is the one case where the
// handle is wanted.
//
// It is a no-op returning nil when no recorder is installed, so a tool
// can call it unconditionally and tests need to install nothing. The
// namespace and JSON are [RecordOf]'s, so a value written here and
// returned again as Result.Details is recorded under one namespace
// twice, the later write describing the call as it ended.
func WriteRecord(ctx context.Context, details Recordable) error {
	fn, ok := RecorderFrom(ctx)
	if !ok || details == nil {
		return nil
	}
	rec, err := RecordOf(details)
	if err != nil {
		return err
	}
	if rec == nil {
		return nil
	}
	return fn(ctx, rec)
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

// Resource is implemented by a tool that owns shared state, naming it,
// so that [Executor] runs two calls that touch the same state one after
// the other, in the model's order, while everything else in the batch
// runs alongside them. The name is free-form and "<kind>:<id>" by
// convention, "shell:session" or "container:47"; two tools that return
// the same name share the lock, which is how a shell tool and a tool
// that restarts that shell stay apart.
//
// It is the answer for a tool that must not run twice at once, and
// [Sequential] is the answer for a tool that must not run while
// anything else does. A tool that reports both is sequential: the
// batch, not the resource, is what it claims. A tool that reports
// neither runs in parallel with everything, which is the default and
// stays the default.
//
// Whether a second call waits or is refused stays the tool's choice:
// nothing here stops a tool from answering "the previous command is
// still running" instead of blocking, which is what a persistent shell
// usually wants the model to see.
type Resource interface {
	Resource() string
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

// ResourceOf returns the shared state t names, or "" when it names
// none. A [Sequential] tool takes the whole batch and reports "" here
// whatever it says, so a caller scheduling by resource does not have to
// check both.
func ResourceOf(t Tool) string {
	if IsSequential(t) {
		return ""
	}
	r, ok := t.(Resource)
	if !ok {
		return ""
	}
	return r.Resource()
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
// [WithStrict], [WithSequential] and [WithResource] apply; the options
// that shape a reflected schema do not. A nil fn panics here, like a bad schema in
// [New].
func NewFunc(name, description string, parameters json.RawMessage, fn func(ctx context.Context, call Call) (Result, error), opts ...Option) Tool {
	if fn == nil {
		panic(fmt.Sprintf("agenttool.NewFunc(%q): nil function", name))
	}
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	return &funcTool{name: name, description: description, schema: parameters, fn: fn, strict: o.strict, sequential: o.sequential, resource: o.resource}
}

type funcTool struct {
	name        string
	description string
	schema      json.RawMessage
	fn          func(ctx context.Context, call Call) (Result, error)
	strict      bool
	sequential  bool
	resource    string
}

func (f *funcTool) Name() string                { return f.name }
func (f *funcTool) Description() string         { return f.description }
func (f *funcTool) Parameters() json.RawMessage { return f.schema }
func (f *funcTool) Sequential() bool            { return f.sequential }
func (f *funcTool) Strict() bool                { return f.strict }
func (f *funcTool) Resource() string            { return f.resource }

func (f *funcTool) Execute(ctx context.Context, call Call) (Result, error) {
	return f.fn(ctx, call)
}

var (
	_ Tool       = (*funcTool)(nil)
	_ Sequential = (*funcTool)(nil)
	_ Strict     = (*funcTool)(nil)
	_ Resource   = (*funcTool)(nil)
)
