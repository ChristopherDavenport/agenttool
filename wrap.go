package agenttool

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
)

// Wrap returns a tool that runs exec in place of t.Execute and is t in
// every other way: its name, description and parameters, and every
// property t declares, strict, sequential, resource, annotations,
// confined and closer, reported exactly as t reports them. It is how a
// policy that grants on use, a decorator that records, or a proxy that
// audits stands in for a tool without changing what the executor and a
// policy layer learn about it.
//
// Embedding [Tool] in a struct forwards the four methods alone and
// silently drops every optional interface, so a wrapped bash that was
// [Sequential] runs in a parallel batch and nothing fails. The set of
// optional interfaces is this package's and grows with it, so a wrapper
// written elsewhere is right only until the next one is added; this
// one is kept right here, and a test refuses a new optional interface
// it does not forward.
//
// The wrapper reports each property through the same reader a harness
// uses, so a property t does not declare reads as its default, which
// is what t reports too. It implements [io.Closer] and closes t when t
// is one, and does nothing otherwise. [Unwrap] returns t. A nil exec
// panics, like a nil function in [NewFunc].
func Wrap(t Tool, exec func(ctx context.Context, call Call) (Result, error)) Tool {
	if t == nil {
		panic("agenttool.Wrap: nil tool")
	}
	if exec == nil {
		panic(fmt.Sprintf("agenttool.Wrap(%q): nil function", t.Name()))
	}
	return &wrapped{Tool: t, exec: exec}
}

// Unwrap returns the tool t was built around by [Wrap], and nil when t
// is not a wrapper. A host that needs the concrete tool behind a chain
// of wrappers calls it until it returns nil.
func Unwrap(t Tool) Tool {
	w, ok := t.(*wrapped)
	if !ok {
		return nil
	}
	return w.Tool
}

// wrapped forwards every optional interface to the tool it embeds. Each
// method goes through the package's reader, so the default for a tool
// that lacks the interface is the reader's and not a second copy of it.
type wrapped struct {
	Tool
	exec func(ctx context.Context, call Call) (Result, error)
}

func (w *wrapped) Execute(ctx context.Context, call Call) (Result, error) {
	return w.exec(ctx, call)
}

func (w *wrapped) Strict() bool             { return IsStrict(w.Tool) }
func (w *wrapped) Sequential() bool         { return IsSequential(w.Tool) }
func (w *wrapped) Annotations() Annotations { return AnnotationsOf(w.Tool) }

// Resource forwards what the tool declares rather than [ResourceOf]'s
// reading of it, so a reader applying the sequential rule to the
// wrapper sees exactly what it would see on the tool.
func (w *wrapped) Resource() string {
	r, ok := w.Tool.(Resource)
	if !ok {
		return ""
	}
	return r.Resource()
}

func (w *wrapped) Confined(ctx context.Context, args json.RawMessage) (bool, string) {
	return ConfinedBy(ctx, w.Tool, args)
}

func (w *wrapped) Close() error {
	c, ok := w.Tool.(io.Closer)
	if !ok {
		return nil
	}
	return c.Close()
}

var (
	_ Tool       = (*wrapped)(nil)
	_ Strict     = (*wrapped)(nil)
	_ Sequential = (*wrapped)(nil)
	_ Resource   = (*wrapped)(nil)
	_ Annotated  = (*wrapped)(nil)
	_ Confined   = (*wrapped)(nil)
	_ io.Closer  = (*wrapped)(nil)
)
