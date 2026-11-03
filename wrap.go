package agenttool

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
)

// Wrap returns a tool that runs exec in place of t.Execute and is t in
// every other way: its name, description and parameters, and every
// property t declares, strict, sequential, barrier, resource, annotations,
// confined, replay, facts, flow and closer, reported exactly as t reports
// them. It is how a policy that grants on use, a decorator that
// records, or a proxy that audits stands in for a tool without changing
// what the executor and a policy layer learn about it.
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
// is what t reports too. Closer is presence rather than a value, so
// the wrapper implements [io.Closer] exactly when t does, and its
// Close closes t; a wrapper around a tool that owns nothing owns
// nothing. Facts are presence as well as a value: [IsFactual] reports
// the wrapper as making the claim exactly when t does, and [FactsOf]
// asks t. [Unwrap] returns t. A nil exec panics, like a nil function
// in [NewFunc].
//
// Wrap stands in for t alone: exec runs t, or what t would run. A
// stand-in that may run a different tool from one call to the next,
// one it looks up at each call, is built with [Follow], since Wrap
// answers every property from t and a claim from a tool the call no
// longer runs would decide it.
func Wrap(t Tool, exec func(ctx context.Context, call Call) (Result, error)) Tool {
	if t == nil {
		panic("agenttool.Wrap: nil tool")
	}
	if exec == nil {
		panic(fmt.Sprintf("agenttool.Wrap(%q): nil function", t.Name()))
	}
	w := &wrapped{Tool: t, exec: exec}
	if c, ok := t.(io.Closer); ok {
		return &wrappedCloser{wrapped: w, closer: c}
	}
	return w
}

// Unwrap returns the tool t was built around by [Wrap], or the tool in
// force for one from [Follow], and nil when t is not a wrapper. A host that needs the concrete tool behind a chain
// of wrappers calls it until it returns nil.
func Unwrap(t Tool) Tool {
	switch w := t.(type) {
	case *wrapped:
		return w.Tool
	case *wrappedCloser:
		return w.Tool
	case *followed:
		return w.current()
	}
	return nil
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

// Barrier forwards what the tool declares rather than [IsBarrier]'s
// reading of it, as Resource does, so a reader applying the sequential
// rule to the wrapper sees exactly what it would see on the tool.
func (w *wrapped) Barrier() bool {
	b, ok := w.Tool.(Barrier)
	return ok && b.Barrier()
}

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

// Replay forwards the tool's claim, which is exec's to keep: a wrapper
// whose exec adds an effect of its own that is not safe to repeat
// reports less, and is built without Wrap.
func (w *wrapped) Replay(ctx context.Context, args json.RawMessage) Replay {
	return ReplayOf(ctx, w.Tool, args)
}

// Facts forwards the tool's claim, which is exec's to keep: a wrapper
// whose exec runs something other than what the tool claims reports
// that, and is built without Wrap: one that runs whichever tool is in
// force is built with [Follow]. A tool that makes no claim answers the
// call itself.
func (w *wrapped) Facts(ctx context.Context, args json.RawMessage) (Facts, error) {
	f, _, err := FactsOf(ctx, w.Tool, args)
	return f, err
}

func (w *wrapped) factsDeclared() bool { return IsFactual(w.Tool) }

// Flow forwards what the tool states, presence included, rather than
// [FlowOf]'s reading of it, as Resource does.
func (w *wrapped) Flow() (Flow, bool) {
	f, ok := w.Tool.(Flowing)
	if !ok {
		return Flow{}, false
	}
	return f.Flow()
}

// wrappedCloser is the wrapper around a tool that is an [io.Closer], so
// that closing the wrapper closes the tool and a wrapper around a tool
// that owns nothing is not a closer.
type wrappedCloser struct {
	*wrapped
	closer io.Closer
}

func (w *wrappedCloser) Close() error { return w.closer.Close() }

var (
	_ Tool       = (*wrapped)(nil)
	_ Strict     = (*wrapped)(nil)
	_ Sequential = (*wrapped)(nil)
	_ Barrier    = (*wrapped)(nil)
	_ Resource   = (*wrapped)(nil)
	_ Annotated  = (*wrapped)(nil)
	_ Confined   = (*wrapped)(nil)
	_ Replayable = (*wrapped)(nil)
	_ Factual    = (*wrapped)(nil)
	_ Flowing    = (*wrapped)(nil)
	_ Factual    = (*followed)(nil)
	_ Flowing    = (*followed)(nil)
	_ Tool       = (*wrappedCloser)(nil)
	_ io.Closer  = (*wrappedCloser)(nil)
)

// Follow returns a tool that stands in for whichever tool current
// returns at the moment it is asked: a skill tool a host rebuilds when
// its catalogue reloads, or a tool it swaps for another of the same
// name. Every property and every call resolves through current, so the
// facts claim a policy decides a call on is the claim of the tool that
// call runs, and a claim never outlives the tool that made it. A
// wrapper built with [Wrap] around the first tool, whose exec runs the
// one in force, answers every property from the first tool, which is
// what the contract's forwarding rule forbids of a stand-in that may
// change which tool it runs.
//
// The name, description and parameters are current's too, so a host
// that swaps tools of different definitions sends the model the one in
// force; every tool current returns has the name the stand-in is looked
// up by. A claim and the call it decides are each resolved when they
// are made, so a swap between the two is a call decided on the claim of
// a tool it does not run: a host that swaps tools while calls are
// decided serialises the swap with its decisions. A listing taken from
// the stand-in is as of when it was taken: mcpserver lists a served
// tool as it is when a client lists, and tells clients the list changed
// when a call finds the tool changed since it was added, so a client
// that took the old listing reads the new facts mark from its next
// listing on; a call from a client session listed the old tool is
// refused rather than run on the old mark. It validates a call's
// arguments against the schema of the tool in force at the call, so a
// session that never listed, which is let through, is held to the new
// schema, not the one the stand-in was added with.
//
// The stand-in owns nothing, and is not an [io.Closer]: the host built
// the tools current returns and closes them. [Unwrap] returns the tool
// in force. current must never return nil, and a nil current panics.
func Follow(current func() Tool) Tool {
	if current == nil {
		panic("agenttool.Follow: nil function")
	}
	return &followed{current: current}
}

// followed answers each method from the tool in force, through the
// package's readers, as [wrapped] does from the tool it embeds.
type followed struct {
	current func() Tool
}

func (f *followed) Name() string                { return f.current().Name() }
func (f *followed) Description() string         { return f.current().Description() }
func (f *followed) Parameters() json.RawMessage { return f.current().Parameters() }

func (f *followed) Execute(ctx context.Context, call Call) (Result, error) {
	return f.current().Execute(ctx, call)
}

func (f *followed) Strict() bool             { return IsStrict(f.current()) }
func (f *followed) Sequential() bool         { return IsSequential(f.current()) }
func (f *followed) Annotations() Annotations { return AnnotationsOf(f.current()) }

func (f *followed) Barrier() bool {
	b, ok := f.current().(Barrier)
	return ok && b.Barrier()
}

func (f *followed) Resource() string {
	r, ok := f.current().(Resource)
	if !ok {
		return ""
	}
	return r.Resource()
}

func (f *followed) Confined(ctx context.Context, args json.RawMessage) (bool, string) {
	return ConfinedBy(ctx, f.current(), args)
}

func (f *followed) Replay(ctx context.Context, args json.RawMessage) Replay {
	return ReplayOf(ctx, f.current(), args)
}

// Facts asks the tool in force. [FactsOf] reads presence through
// factsDeclared and then calls Facts, each asking current, so a swap
// between the two can pair one tool's presence with another's answer,
// as a swap between a claim and its call can decide the call on the
// claim of a tool it does not run. Neither is made safe here: a host
// that swaps the tool in force serialises the swap with its decisions
// and calls, as [Follow] says.
func (f *followed) Facts(ctx context.Context, args json.RawMessage) (Facts, error) {
	fs, _, err := FactsOf(ctx, f.current(), args)
	return fs, err
}

func (f *followed) factsDeclared() bool { return IsFactual(f.current()) }

func (f *followed) Flow() (Flow, bool) {
	fl, ok := f.current().(Flowing)
	if !ok {
		return Flow{}, false
	}
	return fl.Flow()
}
