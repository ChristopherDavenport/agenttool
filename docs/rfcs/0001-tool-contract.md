# RFC 0001: Agent Tool Contract

Status: draft 0.6
Author: Christopher Davenport
Discussion: to be opened against this repository. The Go module at its
root is the reference binding; agentturn is the reference harness.

## Summary

A tool is something a model can call: a **definition** the model reads,
a set of **properties** a harness reads, and an **execute** that turns
one call's arguments into one result. The definition is the Open
Responses function tool, byte for byte, so a tool's identity is the same
object a request carries and a session records. The properties are
optional, each with a default, so a tool that declares none is a
complete tool and a harness that knows none runs it correctly. Execute
is raw JSON in and a result out, with a single error convention, so a
tool written against one implementation of this contract runs under
another and the record of the two runs is the same.

This document states the contract that the Go module carries in doc
comments: what a definition is and how it is canonicalised and hashed;
which properties exist, what each defaults to, and the rule that a
wrapper forwards all of them; what a call and a result are; how an
error, a panic, a progress update and a record reach the harness; what
a batch is and how its calls are ordered, bounded, serialised and
stopped; and what stopping a call and closing a tool each mean. The Go
binding and the MCP binding are given as tables against it.

## Motivation

The README's promise is that a tool is written once and runs under any
loop. The promise held only as far as the Go type system: the interface
is four methods, and everything around it — what an error becomes, how a
batch orders its results, what one `Sequential` tool does to the batch,
when `Terminate` is honoured, how a progress update is shaped, which
optional interfaces exist and what a wrapper owes them — was in doc
comments on exported identifiers. A tool written against a second
implementation was compatible by accident (#26).

Two consumers made the gap concrete. agentsession wants to name a tool
definition by the hash of its canonical JSON, so a shared schema is
stored once and a changed one is found (agentsession #72); that hash is
only meaningful if the definition's shape is specified, and the strict
schema in particular was whatever Go reflection emitted (#27). And a
host that wraps a tool — to audit it, to grant on use, to rename it —
loses every property the wrapped tool declared, silently, because the
properties are found by type assertion and nothing says a wrapper must
forward them (#33).

This RFC is the specification those three issues asked for. It changes
no behaviour of the Go module at draft 0.1; where the module falls short
of what is written here, the shortfall is listed under open questions
with the issue that tracks it.

## Goals

- **One definition.** A tool's identity is its function tool object as
  the request carries it, with one canonical form and one hash, so a
  harness, a recorder and a cache agree on what "the same tool" means.
- **Properties with defaults.** Every fact about a tool beyond its
  definition is optional and defaulted, so an implementation binds each
  as it likes and a tool that declares none is complete.
- **One error convention.** A failure reaches the model in one shape,
  so a model retries the same way under every harness.
- **Batch rules stated.** Ordering, parallelism, serialisation and
  termination are properties of the contract, not of one executor.
- **Bindable.** The contract is language-neutral; the Go module and the
  MCP adapters are bindings of it, given here as tables.

## Non-goals

- Defining a loop. When a model is called, what the transcript holds,
  and how a hook may block a call are the harness's, and agentturn's
  own RFC is where they belong. This document ends where the harness
  hands a batch of calls to an executor and begins again where the
  results come back.
- Defining a session record. What a recorder writes beside a call is
  agentsession's; this document defines only what a tool offers it.
- Permissions. Annotations and confinement are what a tool reports.
  Nothing here decides whether a call may run.
- Replacing MCP. MCP is a transport for a subset of this contract, and
  the binding says which subset.

## Terminology

The key words MUST, MUST NOT, SHOULD and MAY are to be interpreted as in
RFC 2119.

- **Tool**: a definition, a set of properties, and an execute.
- **Definition**: the function tool object the model reads: name,
  description, parameters and the strict flag.
- **Property**: an optional, defaulted fact about a tool that a harness
  reads and the model does not: sequential, resource, annotations,
  confinement, whether a call may run again, and whether the tool owns
  something to close.
- **Harness**: whatever builds requests, hands calls to tools and
  returns their outputs: an agent loop, an MCP server serving Go
  tools, a test.
- **Executor**: the part of a harness that runs one batch of calls.
- **Call**: one invocation: a call ID, an arguments object, an
  idempotency key, and a progress channel.
- **Result**: what one call produced: an output for the model, details
  for the harness, and a terminate hint.
- **Batch**: the function calls of one model response, in the order the
  model wrote them.
- **Record**: a durable, namespaced JSON value a tool leaves beside a
  call for a reader that does not know the tool.

## The definition

A tool's definition is the Open Responses function tool:

```json
{"type":"function","name":"read_file","description":"Read a file from disk",
 "parameters":{"type":"object","properties":{…},"required":[…]},"strict":true}
```

- `name` MUST be non-empty. A set of tools offered on one request MUST
  NOT contain two with the same name.
- `description` is what the model reads to decide when to call the
  tool. It MAY be empty.
- `parameters` is a JSON Schema object describing the arguments object.
  It is always an object: a tool that takes no arguments carries the
  empty object schema, `{"type":"object","properties":{},"required":[]}`,
  and never `null`, so "no arguments" is one definition and not three.
  A tool with no arguments is still called with an arguments object,
  which is `{}`; see [Arguments](#arguments).
- `strict` is present and `true` when the tool declares strict, and
  absent otherwise; a reader that receives `"strict": false` treats it
  as absent. It is the one property that reaches the model, because
  the provider reads it. A binding sets it when it generated the
  schema under the strict rules of the [schema section](#schema-generation);
  a tool that supplies its own schema and declares strict is making
  its author's claim, and the binding does not check it.

Everything else a tool declares is a property, and no property is on the
request. A harness MUST NOT put a property on the definition, and a
reader MUST NOT infer one from it: two tools with the same definition
may schedule differently, and one tool's schedule may change between
requests without its definition changing.

### Canonical definition and definition hash

The **canonical definition** is an object with exactly these members
and no others: `type`, always the string `function`; `name`;
`description`, always present, the empty string when the tool has
none; `parameters`, always an object schema; and `strict`, present
only when `true`. It is serialised with the JSON Canonicalization
Scheme (RFC 8785): members sorted by UTF-16 code units, no
insignificant whitespace, numbers and strings in their canonical
forms. The **definition hash** is `sha256:` followed by the lowercase
hexadecimal SHA-256 of those bytes.

Stating the members matters because the wire form is looser than the
hash can afford to be. A writer that omits an empty description, sends
`null` for no parameters, or keeps a decoded `"strict": false` produces
a different hash for the same tool, so a writer normalises to the
form above before hashing, whatever it sent.

Two definitions are the same tool when their hashes are equal. This is
the identity agentsession names a tool by (agentsession #72), and it is
chosen to be the same object the request carries so that a recorder
computes it from the request alone and a tool need not be reachable to
be identified.

Canonicalisation sorts an object's members, so the order in which a
generator emits schema keys does not affect the hash. It does not sort
arrays, so the order of `required`, of `enum`, and of the members of a
type union such as `["integer","null"]` does affect it. The
[schema section](#schema-generation) fixes those orders for a generated
schema; a tool that supplies its own schema is identified by that
schema as written.

A description change is a definition change. That is deliberate: the
model reads the description, so a request carrying a different one is a
different request, and a cache keyed on the prefix was already broken by
it.

## Properties

A property is a fact about a tool that a harness reads and the model
does not. Each is optional and has a default, and a tool that declares
none of them is a complete tool that runs in parallel with everything,
claims no shared state, says nothing about its behaviour, claims no
sandbox, never runs a call twice, is its own one fact and owns nothing
that outlives a call.

| Property | Type | Default | Read by |
| --- | --- | --- | --- |
| strict | boolean | false | the provider, via the definition |
| sequential | boolean | false | the executor |
| resource | string | `""`, none | the executor |
| annotations | object | unstated | a policy layer |
| confined | (ctx, args) → (boolean, string) | (false, `""`), unstated | a policy layer, per call |
| replay | (ctx, args) → unknown, safe or keyed | unknown | a harness resuming or retrying, per call |
| facts | (ctx, args) → (calls, rewrite) or error | absent: the call itself | a policy layer, per call |
| closer | presence | absent, owns nothing | the host |

- **strict** is the tool's claim that its parameters schema keeps the
  strict rules, so the provider may enforce them. It is set on the
  definition. A binding makes the claim true for a schema it generated
  under those rules and does not check one the author supplied.
- **sequential** says the tool must not run alongside any other tool in
  the same batch. When any tool in a batch reports it, the whole batch
  runs one call at a time in the model's order.
- **resource** names shared state a call of the tool touches, free-form
  and `<kind>:<id>` by convention, `shell:session` or `container:47`.
  Two calls in one batch that name the same resource run one after the
  other in the model's order, while the rest of the batch runs
  alongside them. Two tools that name the same resource share it. A
  tool that reports both sequential and a resource is sequential, and
  its resource reads as none: the batch is what it claims.
- **annotations** are the behavioural hints MCP defines: a title, and
  whether the tool is read-only, destructive, idempotent and
  open-world. They are hints from the tool's author and never
  authoritative: a policy MAY use them to be stricter and MUST NOT use
  them alone to allow a call. The default is *unstated*, which is not
  the same as harmless; an implementation MUST let a reader tell a
  tool that said nothing from one that said it is safe, or MUST
  document that it cannot. The Go binding cannot, and says so.
- **confined** answers, for one call's arguments under one context,
  whether the call will run inside a sandbox and what confines it,
  `seatbelt`, `landlock+seccomp`, `container:agent-sandbox`. A tool
  that does not answer is read as *unconfined*, since the safe mistake
  is to ask. Nothing here enforces anything: this is what a tool
  reports, never what it runs.
- **replay** answers, for one call's arguments under one context,
  whether a call that may already have run can run again. *Safe* says
  a second run has no effect beyond the first's; *keyed* says the tool
  deduplicates on the call's [idempotency key](#a-call); *unknown*,
  the default, says nothing, and a call that says nothing is not run
  twice. See [Running a call again](#running-a-call-again). It is a
  claim, and the annotations do not stand in for it: a tool whose hints
  say read-only or idempotent and that does not answer is *unknown*,
  since running a call twice is an allow and a hint MUST NOT allow
  alone. A reader MUST treat a value it does not know as *unknown*, so
  a value added to this list later is read by an older harness as the
  safe mistake.
- **facts** answer, for one call's arguments under one context and
  without acting on anything, what the call would touch, in tool-call
  terms. *Calls* are the calls it amounts to, each a tool name, its
  arguments and a text for a question about it: a shell command
  `cat .env > out/x` amounts to a read of `.env` and a write of
  `out/x`. An empty tool name is the claiming tool. Absent calls are the
  call itself, its own arguments; an empty list says the call amounts
  to nothing the tool can state, and a reader MUST NOT take it for the
  call itself. *Rewrite*, when present, is the arguments the call runs
  with if a policy allows it, in place of the model's: a plan stamped
  under a key only the tool holds. An error is a call nothing can be
  said about, and a policy MUST NOT allow a call on it. Facts are a
  claim with presence: a tool that does not make it is its own one
  fact, decided on its arguments, and a binding MUST let a reader tell
  that apart without asking about a call, so that a harness that would
  pay a round trip for facts skips the tool. Like confined, this is
  what a tool reports, never what it runs; a tool that rewrites checks
  when it runs that what it runs is what it claimed.
- **closer** says the tool owns something that outlives a call, a
  container, a persistent shell, a pool, and how to release it. See
  [Lifecycle](#lifecycle).

### Binding and forwarding

An implementation binds each property however its language does
optional things: an optional interface found by type assertion, a
protocol, a mixin, a field with a zero value. Whatever the binding, it
MUST offer one documented way to *ask* for each property that applies
the default, so a harness asks once and never learns the binding.

A tool that stands in for another — a wrapper that audits, a proxy that
renames, a decorator that records — MUST report every property of the
tool it wraps, unchanged, unless it means to change one. The set of
properties is this document's and grows with it, so a wrapper written
outside the binding is correct only until the next property is added;
a binding SHOULD therefore provide the wrapper, and a harness SHOULD
use it. Dropping a property is the failure #33 describes: a wrapped
`bash` that no longer reports sequential runs two shell commands at
once in one working directory, and nothing fails.

## Arguments

The model writes a call's arguments as a string. A harness MUST parse it
and hand the tool the arguments as a JSON object, byte for byte as the
model wrote it after parsing. A harness MUST NOT reorder, normalise or
fill it.

- An empty arguments string is the empty object `{}`, and a tool with
  no parameters receives `{}`.
- Arguments that do not parse as a JSON object are refused before the
  tool runs, with an error beginning `invalid arguments:`. The
  reference loop says `invalid arguments: expected a JSON object`; a
  harness that validates with a general validator says what the
  validator says after the prefix.
- A tool whose schema the binding generated validates the arguments
  against it before decoding them, so a missing required property, a
  wrong type or a value outside an enum reaches the model as an error
  it can retry on rather than a zero value. A tool whose schema came
  from elsewhere cannot know what the schema promises and hands the
  arguments to its decoder as they are; a harness that serves such a
  tool, as the MCP server does, validates with a general validator
  instead.
- A validation failure is an error whose message is phrased for the
  model and begins `invalid arguments:`. The reference binding's own
  validator continues `<path>: <message>`, where `<path>` is the
  dotted property path with `[i]` for array elements and is omitted at
  the root: `invalid arguments: max_bytes: expected integer, got
  "ten"`. A general validator, as the MCP server uses, continues in
  its own words, so only the prefix is the contract.

## A call

A call is a call ID, an arguments object, an idempotency key and a
progress channel.

- The **call ID** is the `call_id` of the function call item, and it is
  what the result is matched to and what a record is filed beside. A
  harness that invents a call outside a model turn MUST still give it
  an ID unique within the batch.
- The **arguments** are as above.
- The **idempotency key** names the logical operation the call is, for
  a tool that deduplicates on it. The harness mints it, opaque, and it
  is not the call ID: a call ID is unique within one response, which
  is too narrow to deduplicate against a service that outlives the
  session. A harness that runs a call again MUST give it the key the
  first attempt carried, and MUST give every other call a key of its
  own, so a call the model makes again, with a new call ID, is a new
  operation. A harness MAY supply none; a keyed tool then deduplicates
  nothing, and its calls read as *unknown*.
- **Progress** is a channel from the tool back to the harness that the
  harness opens when it wants updates and leaves closed otherwise. A
  tool MAY send any number of intermediate results before its final
  one; the harness serialises them; a tool with nothing to report sends
  none. An update after the final result is dropped.

The call reaches the tool on its **context**, and the context carries
three things:

- **The call itself**, so a tool built without a typed wrapper, or a
  recorder installed for the batch, can name the call it is serving.
  An executor MUST put it there for every tool it runs, not only for
  those its own constructors built.
- **The recorder**, when the harness has one; see [Records](#records).
- **Cancellation**, which is the harness's interrupt; see
  [Stopping a call](#stopping-a-call).

### A result

A result is an output, details and a terminate hint.

- The **output** is what the model sees: text, or a list of content
  parts, which is what an image-returning tool sends. It becomes the
  `output` of the function call output item.
- **Details** are for the harness alone and MUST NOT reach the model:
  the raw result of a remote call, a live handle, the numbers behind a
  progress update. A details value that is meant to outlive the run is
  a [record](#records).
- **Terminate** hints that the harness should stop after this batch
  instead of calling the model again, because the tool answered on the
  model's behalf. See [Termination](#termination) for how a batch
  honours it.

### Errors

A tool that fails **returns an error**. It MUST NOT encode a failure as
ordinary output, because the model then reads the failure as an answer
and does not retry.

The harness renders a returned error as the output of the call, as text
of the form `Error: ` followed by the error's message, and ignores the
result's output. The prefix is applied exactly once, at the boundary
where the output is built for the model, so an error that crosses a
transport is still one `Error:` when it lands. The result's details are
kept, since an adapter may have something to say about a failed call
that the harness wants.

A tool that panics, throws, or otherwise fails outside its own error
path completes with an error whose message is `tool "<name>" panicked:
<value>`; the harness keeps whatever diagnostic the language offers, a
stack, for itself and never sends it to the model.

An error, in either form, is a result for the purposes of the batch: it
is matched to its call, returned in order, and its terminate hint is
honoured as any other's.

### Progress

A progress update is a result sent before the final one. Its output is
what a front shows; its details MAY be a **progress info** carrying
`progress`, `total` and `message`, which mirror the MCP progress
notification so the two adapters forward it in either direction. A tool
with no numbers sends text alone.

Updates are delivered to the harness on the harness's own goroutine or
thread, one at a time, in the order they were sent, and never after the
call's final result.

### Records

A tool that owns something the harness cannot see — which container
served this call, which process group it forked, where it spilled a
large output — leaves a **record**: a namespace and a JSON value. The
namespace is `<owner>:<kind>` by convention, `shell:process-group`,
`workspace:shell`, MUST be non-empty, and names the entry a recorder
writes beside the call without knowing the tool. The value is the
record's own JSON.

A record is left at two moments, and they are the same namespace:

- **At the end**, as the result's details. A recorder reads the details
  when the call ends and writes the record if the details are
  recordable. Details that are for the harness alone, a live handle or
  a progress info, are not recordable and are not written.
- **During the call**, through the recorder on the context. A tool that
  has just started something, forked a process, made a directory, been
  given a remote job ID, writes it then, and the write MUST be durable
  before it returns: a tool killed mid-call leaves nothing behind
  otherwise, and that is the one case where the handle is wanted.
  Writing with no recorder installed is a no-op, so a tool writes
  unconditionally and a test installs nothing.

A record written both ways is recorded under one namespace twice, the
later write describing the call as it ended. The recorder finds the
call the record belongs to on the context, which is why an executor
puts it there.

## A batch

A batch is the function calls of one model response, in the model's
order. The harness hands it to an executor and receives one result per
call; the harness then appends one function call output per call, in
the model's order, whatever order the results arrived in.

### Ordering

- **Results are matched by call**, never by position, and returned to
  the model in the batch's order.
- **Completion order is free.** An executor MAY report completions as
  they happen, and a consumer that needs the batch's order reorders.
- **Every call completes exactly once**, with a result or an error,
  including a call that never started: a call cancelled before it ran
  completes with the cancellation as its error.
- **A call starts when it is handed to its tool**, not when its batch
  is handed to the executor. A call waiting for a slot in the bound or
  for its turn in a chain has not started, and a harness that records
  that a call was dispatched, so a reader can tell a call that never
  ran from one that may have, records it at that moment and not
  before. An executor SHOULD tell the harness when each call starts,
  synchronously, so a record that must be durable before the tool runs
  can be, and MUST let the harness refuse the call there: a dispatch
  that could not be recorded is never followed by a side effect the
  record cannot see.

### Parallelism

Calls run in parallel up to a bound the harness sets, default 8, in
whatever order the executor schedules them. Two calls of one batch MAY
therefore overlap, and a tool that cannot tolerate that declares
[sequential or a resource](#properties).

### Serialisation

- **A serial batch.** When the harness asks for it, or when any call's
  tool reports sequential, every call of the batch runs one after the
  other in the model's order, and resources are ignored, since a serial
  batch is already stricter.
- **A resource chain.** Otherwise the calls whose tools name one
  resource run one after the other in the model's order, and alongside
  everything else. A call whose tool names no resource is a chain of
  its own.
- **The scope is one batch.** Serialisation is between calls of one
  batch; nothing here orders a call in one batch against a call in
  another, in a sub-agent's batch, or in a concurrent run. A tool whose
  state outlives a batch, a persistent shell, a container, a pool,
  MUST guard that state itself against a second call arriving while
  one runs, and MAY answer the second call with an error rather than
  wait. The scope stays one batch (#35): the executor sees one batch,
  and the tool value is the thing that outlives batches as the state
  does, so a guard across batches belongs on it. A tool that pools, and
  can serve two calls at once, names no resource and guards nothing,
  which is the distinction a lock held by the harness for every tool
  could not make. OpenHands takes a lock per conversation, taken by the
  executor for every call, with the terminal declaring a resource for
  one shell and none for a pool of panes; a contract that one day wants
  a lock across batches copies that shape, and puts the lock on the
  tool value rather than in the executor.

### Termination

A result MAY set terminate. When **every** result of a batch sets it,
the batch terminates: the harness stops after appending the outputs
and does not call the model again. When **some** do, the harness
decides its own policy, and MUST report the outcome as partial rather
than as a termination, since the model asked for something the batch
did not finish. When none do, the batch continues.

### Cancellation

Cancelling the batch cancels every call's context. A running tool
stops as [Stopping a call](#stopping-a-call) says; a call not yet
started completes with the cancellation as its error. The executor
waits for running tools to return before it reports the batch done,
so a harness that cancelled still receives every result and can record
the outputs of calls that finished before the cut.

## Lifecycle

A tool that owns something has two moments, and the contract answers
them separately.

### Stopping a call

Stopping a call in flight is the call's context. It is cancelled from
outside while the tool runs, which is what a harness's interrupt is. A
tool that owns a process waits on the context, signals or kills what
*that call* started, and returns; a partial result with no error is the
right answer when the output so far is worth the model's while. What
the tool value owns across calls, the shell, the container, is
untouched, because the context belongs to the call and not to the tool.

There is deliberately no interrupt method. The reference agent that
has one has it because its language has no cancellation to hand, and a
second way to say "stop" would leave every tool guessing which one a
harness used. The model's own interrupt, "send Ctrl-C to the shell and
keep it running", is an argument of the shell tool and needs nothing
from the contract.

### Closing a tool

Releasing what outlives the call is the **closer** property. A tool
that owns a container, a persistent shell or a pool declares it, and
its close releases what the value owns. Close belongs to the **host**
and never to a run or a batch: a session outlives many runs, an
executor closes nothing, and a host closes what it built when it is
done with it. A harness that builds tools on the host's behalf, per
turn or per session, MUST say who closes them.

### Running a call again

A call's outcome is **ambiguous** when the tool may have started it
and the harness does not have its result: the harness was killed
between the dispatch and the result and is resuming, or a transport to
a remote tool broke mid-call, or the call failed in a way that may have
come after its effect. The dispatch is the moment the executor hands
the call to its tool, and a harness that records it knows which calls
of a batch can be ambiguous and which provably never started, which
it runs as it would have.

For an ambiguous call the harness asks the tool's **replay** property
with the call's arguments, under a context like the one the call would
run under:

- **safe**: the harness MAY run it again.
- **keyed**: the harness MAY run it again with the idempotency key
  the first attempt carried, and MUST NOT run it with any other or
  with none.
- **unknown**: the harness MUST NOT run it again. It completes the
  call with an error saying the outcome is unknown, so the model sees
  that the call may or may not have taken effect and can check before
  it asks again.

A tool that answers *keyed* MUST give a second call carrying the key of
a call that completed no effect beyond the first's. It SHOULD return
that call's outcome, its error included; SHOULD refuse the key while
the first call is still running; and SHOULD refuse it with different
arguments. Those three are the behaviour a service with idempotency
keys commonly gives, and they are recommended rather than required so
that a tool over a service that gives only the first stays conforming.
Where the tool keeps what it deduplicates on, and for how long, is the
tool's: a tool over a service that deduplicates passes the key
through, and one that deduplicates itself keeps a record that outlives
the harness process, or its claim does not survive the kill it is for.

Running a call again is the harness's decision and never the
executor's: an executor runs what it is given once. Nothing here says
when a failure is worth retrying, only whether a retry is safe; see
the open questions.

## Bindings

### Go

The root module of this repository is the reference binding. The
contract maps onto it as follows; `Tool` is the four-method interface
and everything else is optional.

| Contract | Go |
| --- | --- |
| definition | `Definition(t)` → `*openresponses.FunctionTool`; `Set.Definitions()` for a request |
| name, description, parameters | `Tool.Name`, `Tool.Description`, `Tool.Parameters` (nil is no arguments, and `Definition` serves it as `NoArgsSchema`) |
| strict | `Strict` interface, read by `IsStrict`, set by `WithStrict()` |
| sequential | `Sequential` interface, read by `IsSequential`, set by `WithSequential()` |
| resource | `Resource` interface, read by `ResourceOf` (which applies the sequential rule), set by `WithResource(name)` |
| annotations | `Annotated` interface and `Annotations` struct, read by `AnnotationsOf`, set by `WithAnnotations(a)` |
| confined | `Confined` interface, read by `ConfinedBy(ctx, t, args)`, set by `WithConfined(fn)` |
| replay | `Replayable` interface and `Replay` (`ReplayUnknown`, `ReplaySafe`, `ReplayKeyed`), read by `ReplayOf(ctx, t, args)`, which reads an unknown value as `ReplayUnknown`, set by `WithReplay(fn)` |
| facts | `Factual` interface, `Facts{Calls, Rewrite}` and `FactCall{Tool, Args, Text}`, read by `FactsOf(ctx, t, args)`, which also reports whether the tool claims and returns its error as is, presence read by `IsFactual(t)`, set by `WithFacts(fn)`; a `New` or `NewFunc` tool built without it, and a `Wrap` of a tool that does not claim, has the method and reads as not claiming |
| closer | `io.Closer`; `Set.Close()` closes a list in order and joins errors; `WithCloser(fn)` makes a `New` or `NewFunc` tool one, and without it the tool is not |
| forwarding wrapper | `Wrap(t, exec)` forwards every property of `t` and closes it; `Unwrap(t)` returns it |
| call | `Call{ID, Args, IdempotencyKey, OnUpdate}`; `Call.Update` sends progress |
| call on the context | `WithCall`, `CallFrom`; `Executor` installs it for every job |
| result | `Result{Output, Details, Terminate}`; `Text`, `Parts`, `Output` build one |
| error convention | `ErrorResult(err)` renders `Error: ` + message as a fresh result; the harness calls it and copies the failed result's `Details` and `Terminate` onto it, as agentturn does |
| panic | `PanicError{Tool, Value, Stack}` |
| progress info | `ProgressInfo{Progress, Total, Message}`; `Progress(ctx, r)` sends from a typed tool |
| record | `Recordable` (`RecordNS() string`), `Record{NS, Data}`, `RecordOf(details)` |
| recorder on the context | `RecordFunc`, `ContextWithRecorder`, `RecorderFrom`, `WriteRecord`; `Executor.Recorder` installs it per batch |
| elicitor on the context | `Elicitor` answers an `Elicitation{Message, Schema, URL}` with an `Answer{Action, Content}`, `ActionAccept`, `ActionDecline` or `ActionCancel`; `ContextWithElicitor`, `ElicitorFrom`; the harness installs it, and a tool with none has nobody to ask |
| arguments validation | `New` validates a reflected schema with `Schema.ValidateJSON`; `ValidationError{Path, Msg}` |
| executor | `Executor{MaxParallel, Sequential, Recorder, OnStart}`; `Execute` yields `Event{Index, Final, Result, Err}`; `Results` collects in job order |
| a call starts | `Executor.OnStart(ctx, job) error`, on the job's goroutine, after the slot and the turn, with the call on `ctx`, before `Execute`; an error completes the job without running the tool |
| the chains of a batch | `Executor.Chains(jobs) [][]int` returns the chains `Execute` runs the batch in, each in the model's order, one chain for a serial batch; a harness that waits for a call's predecessor to settle reads it rather than restating the rules |
| schema generation | `New`, `SchemaFor`, `SchemaOf`, `Reflect`; `Schemer` supplies a schema; see the [schema section](#schema-generation) |

One place where the binding does not yet do what this document says:
the annotations default is the zero `Annotations` value, which a reader
cannot tell from a tool that declared every hint false. The doc comment
says so, and it is an open question below.

The forwarding rule is bound by `Wrap`, and a test in the package reads
its exported interfaces and refuses a new optional interface `Wrap`
does not forward, which is how the binding stays right as properties
are added.

### MCP

The adapters map to and from the contract and add nothing it cannot
express. Where MCP has no field for a property, the serving side
states it in the tool's `_meta` under one namespaced key, `ToolMetaKey`,
as the server's statement rather than a hint, and the per-call claims
are answered by a method of their own; a property neither carries is
set on the local side by the host and does not cross.

| Contract | MCP | Direction |
| --- | --- | --- |
| name, description | `name`, `description` | both |
| parameters | `inputSchema`; a nil schema is served as an empty object schema | both |
| strict | no field; not carried | — |
| sequential, resource | no field in MCP; the serving side puts them in the tool's `_meta` under `ToolMetaKey` (`sequential`, `resource`), and the consuming side declares them from there. `mcpclient.WithSequential(names…)` adds sequential, and `WithResource(resource, names…)` names a remote tool's state and wins over the server's, an empty resource withdrawing it, since the host sees every server it composes and the server only itself | both |
| annotations | `annotations`: `title`, `readOnlyHint`, `destructiveHint` (absent is true), `idempotentHint`, `openWorldHint` (absent is true); a tool with no block is unstated | both |
| confined | no field; `mcpclient.WithConfined(by, names…)` says a remote tool runs confined, and `WithConfinedFunc(fn, names…)` answers per call; the claim is the host's, which placed the server, and the server's does not cross | consume |
| replay | answered by `execution/facts` beside the call's facts, for a tool whose `_meta` marks `replay`; *keyed* is sent and read as *unknown*, since the idempotency key does not cross. A consumed tool of a server that does not answer is *unknown* whatever its `idempotentHint`, since MCP defines no deduplication and a call whose stream broke may or may not have run | both |
| facts | `execution/facts`, a method of its own beside `tools/call`, so it cannot reach the model's tool list: its params are a response's calls together, `{name, arguments}`, and its result one answer per call in order, `facts` (`{calls: [{tool, args, text}], rewrite}`, calls `null` for the call itself and `[]` for nothing the tool can state), or `error` in its place, or neither for a tool that makes no claim, with the call's `replay`. Answering never runs a tool. The server advertises it as the experimental capability `FactsCapability`, and marks a claiming tool `facts` in its `_meta` under `ToolMetaKey`, which is the presence a reader sees without asking. A consumed tool so marked on a server that advertises it is factual and asks under the call's context; a server that does not advertise it is never asked | both |
| read-only | the tool's `_meta` under `ToolMetaKey` carries `readOnly`, the served tool's annotation, as the server's statement rather than a hint; the consuming side reads it with `ToolMetaOf` | both |
| idempotency key | no field; a consumed call's key is not sent, and a served call has none | — |
| closer | not a closer: the remote session is closed through `mcpclient.Remote.Close` | consume |
| arguments | `arguments`; the server validates against `inputSchema` with a general validator | both |
| output | text, image and resource content ↔ output parts; text alone ↔ text; audio content consumed becomes an input file part, and is served back as an embedded blob rather than audio | both |
| error | `isError` with the message as text; the consuming side returns it as an error and the harness applies `Error:` once | both |
| progress | `notifications/progress` when the request carries a token; `progress`, `total`, `message` ↔ progress info | both |
| details | the consuming side sets the raw `CallToolResult` as details | consume |
| record | the serving side puts a recordable result's record in `_meta` under `RecordMetaKey`, the data as a JSON string so it crosses byte for byte, or the record error in its place; the consuming side makes a record the details as a `RemoteRecord`, SDK result inside, and leaves the SDK result otherwise | both |
| call on the context | the server installs it for every call, as the executor does | serve |
| recorder on the context | the handler leaves one it finds; the go-sdk at v1.8.0 derives each call's context from the one the session was opened with, `Run` or `Connect` over stdio and in memory, the initialize request over streamable HTTP, which is observed behaviour a test pins and not a documented guarantee | serve |
| client identity | the server puts the MCP session on the context: `mcpserver.SessionFrom` | serve |
| elicitation | `elicitation/create` ↔ the elicitor on the call's context. The client offers it only with `mcpclient.WithElicitation`; from 2026-07-28 the question returns with its call and is that call's, and before it a question the server sends on its own is put to the one call in flight, and to nobody when there is none or several; nobody to ask answers `cancel`. The server installs an elicitor on a served call's context when the client offers elicitation: from 2026-07-28 a question goes back as the call's input request, the tool running on while the client asks, and before it as `elicitation/create`; a question of a mode the client does not take answers `cancel` | both |

### Open Responses

The definition is the `function` tool of the request's `tools`. A call
is a `function_call` item: `call_id`, `name`, and `arguments` as a
string. A result is a `function_call_output` item: the same `call_id`,
and `output` as a string or a list of content parts. The error
convention is a `function_call_output` whose `output` is the `Error:`
text; nothing in the item says it was an error, which is why the
convention is the model's only signal and why a tool never writes one
as content.

## Schema generation

A binding that builds a tool from a typed function generates the
parameters schema from the argument type. That schema is the
definition's largest member, it goes into the definition hash, and
agenteval's strict replay compares that hash across runs, so a second
implementation that generates a different but equivalent schema for
the same shape breaks every hash it touches. This section states what
a generated schema looks like in terms of the *shape* it was generated
from, so that two generators built separately agree byte for byte, and
the fixture corpus in `testdata/schema` is described the same way.

### The argument shape

An **argument shape** is an ordered list of fields. A field has:

- a `name`, the property name as it appears in the arguments object;
- a `type`, from the table below;
- optionally a `description`;
- optionally an `enum`, a list of JSON values of the field's type,
  in the order they were declared;
- `optional`, true when the caller may leave the field out;
- `nullable`, true when the caller may send `null` for it.

A type is one of:

| Type | Schema |
| --- | --- |
| `boolean` | `{"type":"boolean"}` |
| `integer` | `{"type":"integer"}` |
| `number` | `{"type":"number"}` |
| `string` | `{"type":"string"}`, with `"format":"date-time"` when the shape says so |
| `any` | `{}`, which admits every value |
| array of T | `{"type":"array","items":<T>}` |
| map of T | `{"type":"object","additionalProperties":<T>}`; rejected in strict mode |
| object with fields | an object schema as below |

An argument shape may not contain itself, at any depth: a recursive
shape is rejected, since a schema with no `$ref` cannot express it.

### The generated schema

An object schema is emitted for the root shape and for every object
field. It always carries `type`, `properties` and `required`, even when
the last two are empty, so a tool with no arguments has the schema
`{"type":"object","properties":{},"required":[]}`. Its properties are
the shape's fields, in field order, each mapped as its type says with
`description`, `format` and `enum` added when present.

The members of any generated schema object appear in this order, and
no others appear:

```
type, description, format, enum, properties, required, items, additionalProperties
```

That order is for a reader of the request. RFC 8785 sorts members, so
the definition hash does not see it. The hash does see array order,
and the three arrays a generator emits are fixed as follows:

- `required` lists property names in **field order**.
- `enum` lists values in **declaration order**, as the JSON values they
  are: `[1, 2, 3]` for integers, `["low","medium","high"]` for strings.
- A type union is `[<type>, "null"]`, the type first and `"null"`
  second, and is emitted only under the strict rules.

**Outside strict mode**:

- `required` holds the fields that are not `optional`.
- Nullability is not expressed. A nullable field's schema is its
  type's, and a caller sending `null` for it is outside the schema.
  Nothing more is emitted for it, because outside strict mode the
  field is also optional and leaving it out is what a caller does.
- `additionalProperties` is omitted from every object, so unknown
  properties are allowed and a binding ignores them.
- A map is an object with `additionalProperties` and neither
  `properties` nor `required`.

**Under strict mode** (`"strict": true` on the definition):

- `required` holds **every** field, in field order, `optional` or not.
- A `nullable` field's type is the union `[<type>, "null"]`. A
  nullable `any` stays `{}`, which already admits `null`. Its `enum`,
  when it has one, lists the declared values and then `null`, last:
  `enum` is an assertion of its own in JSON Schema, so a validator
  that reads it would refuse the `null` the type union admits unless
  the enum admits it too.
- `additionalProperties` is `false` on **every** object, the root and
  each nested one.
- A map is rejected, because strict mode cannot express an object
  with unknown keys.

A field that is `optional` and not `nullable` is therefore required
under strict mode with no way to send "absent". That is the current
rule and a binding MUST follow it; whether it should change is an open
question below.

### Validation against a generated schema

A binding that generated a schema validates arguments against it
before decoding them; the [Arguments](#arguments) section gives the
error form. The checks are the ones the generated schema can express:
the type, the enum, the required properties, `additionalProperties`
when false, and the items and properties recursively. A `{}` accepts
anything. An `integer` accepts a JSON number with no fractional part.
A `null` is accepted where the type union says so, and where the
schema is `{}`, and only if the enum, when there is one, lists it.

### The Go source mapping

The reference binding reads the argument shape from a Go struct:

| Go | Shape |
| --- | --- |
| exported field | a field named by its `json` tag, or the Go name without one; `json:"-"` is skipped |
| `desc` tag | `description` |
| `enum` tag, comma separated | `enum`, parsed as the field's kind |
| `omitempty` or `omitzero` in the tag, or a pointer | `optional` |
| a pointer | `nullable` |
| `bool` | `boolean` |
| the integer kinds, `time.Duration` included | `integer` |
| `float32`, `float64` | `number` |
| `string`, `[]byte`, a type implementing `encoding.TextMarshaler` | `string` |
| `[N]byte` | array of `integer`, since the encoder writes a byte array as numbers and a byte slice as base64 |
| `time.Time` | `string` with `format` `date-time` |
| `json.RawMessage`, an interface, a non-pointer type implementing `json.Marshaler` | `any` |
| a slice or array of T | array of T |
| a map with string keys | map of T; a non-string key is rejected |
| a struct | object |
| an embedded struct | its fields promoted in place, under encoding/json's rules: the shallowest wins, a tagged one wins among equals, and a tie is dropped |

Everything not in the table is rejected, so a shape the binding cannot
express fails at registration rather than at call time. An argument
type that implements `Schemer` supplies its own schema, which is served
verbatim and not validated by the binding, since the binding cannot
know what it promises.

### The fixture corpus

`testdata/schema/` holds one golden file per fixture, pretty-printed
with two-space indentation, and `manifest.json` describes each
fixture's argument shape in the terms of this section, so the corpus
can be regenerated by any implementation. Each entry has:

- `name`, which is also the golden file's base name;
- `strict`, whether the strict rules apply;
- `source`, a note on the Go declaration the fixture came from, for a
  reader of the reference binding; it is informative;
- either `fields`, the argument shape, or `supplied`, a schema the
  type supplies itself, which the golden is verbatim.

A field is `{"name", "type", "description"?, "enum"?, "optional"?,
"nullable"?}` with `items`, `values` or `fields` beside `type` for an
array, a map or an object, and `format` beside a string. The reference
binding's test suite interprets the manifest with the rules above and
requires the result to equal each golden, so the manifest, the rules
and the generator are held to one another.

## Conformance

**A conforming tool** has a non-empty name; returns a parameters schema
that is a JSON Schema object or none; returns an error rather than
encoding one; sends progress only before its final result; stops what a
call started when the context is cancelled and keeps what the value
owns; declares sequential or a resource if two of its calls must not
overlap; declares a closer if it owns something beyond a call; answers
*safe* only for a call whose second run has no further effect, and
*keyed* only when it deduplicates on the key; acts on nothing while it
answers facts; and reports every property of a tool it wraps.

**A conforming harness** offers a set of tools with distinct names;
hands each call its arguments as a JSON object, `{}` when empty, and
refuses arguments that are not one; validates against a schema it
generated, or with a general validator when serving a schema it did
not; renders a returned error as `Error: ` and the message, once;
completes every call of a batch exactly once; returns outputs in the
model's order; bounds parallelism; runs a batch serially when any tool
asks; serialises calls that name one resource in the model's order;
honours terminate only when unanimous and reports a partial batch as
partial; puts the call on the context for every tool it runs; installs
a recorder if it has one; cancels running calls when it abandons a
batch and waits for them; gives each call its own idempotency key, if
it gives keys, and a call it runs again the key it first carried; runs
an ambiguous call again only when replay allows it, and otherwise
completes it saying the outcome is unknown; and closes nothing that
the host built.

**A conforming adapter** maps each row of its binding table and adds
nothing the contract cannot express.

**A conforming schema generator** produces, for every fixture in
`testdata/schema/manifest.json`, the bytes of that fixture's golden
file after canonicalisation, and rejects the shapes the
[schema section](#schema-generation) says to reject.

## Versioning

This document is versioned with the Go module. A draft number changes
when a rule is added or changed; a rule's removal or a change that
makes a conforming tool non-conforming is a breaking change to the
module and is listed in the changelog as one.

## Prior art

- **MCP** defines a tool as a name, a description, an input schema and
  annotations, called over a transport with `isError` and progress
  notifications. It is the closest published contract and the one this
  document binds to; it has no notion of a batch, of sequencing, or of
  a record.
- **Open Responses** defines the function tool, the function call item
  and the function call output item, which are this document's
  definition, call and output on the wire.
- **The OpenAI function-calling guide** defines `strict` and the schema
  restrictions it implies, which the schema section binds.
- **OpenHands SDK** gives a tool an `executor`, an `annotations` block
  taken from MCP, a per-conversation resource lock, and an
  interrupt method, the last of which this document deliberately
  omits.
- **Idempotency keys** are how services that take writes make them
  safe to retry: Stripe's `Idempotency-Key` header and AWS's
  `ClientToken` are per request and minted by the caller, and the IETF
  HTTP API working group's `Idempotency-Key` header draft states the
  common behaviour, which *keyed* recommends. RPC protocols stop
  short of a key: protobuf's `idempotency_level` is per method,
  `NO_SIDE_EFFECTS` or `IDEMPOTENT`, as MCP's `idempotentHint` is per
  tool, and neither can answer for one call of a shell.
- **MCP's request idempotency proposal**, SEP-3182, closed unmerged,
  would have added an `idempotencyKey` to `tools/call` with the three
  behaviours *keyed* recommends: replay a completed call's outcome,
  refuse a key in progress, refuse a key with different arguments.
  The name of the key here is chosen to meet it if it returns. The
  2026-07-28 specification removed stream resumption, so a call whose
  stream breaks is re-issued with a new request ID and its first
  attempt's outcome is ambiguous, which is the word this document
  uses.
- **agentsession RFC 0001** is the record a tool's outputs and records
  land in, and the source of the canonicalisation and hash notation
  used here.

## Open questions

- **Unstated versus false annotations.** The Go binding cannot tell a
  tool that declared no annotations from one that declared them all
  false. MCP can, because the block is optional. A pointer, a presence
  flag, or a separate `Annotated` check are the options.
- **Definition hash in the binding.** The hash is defined here and
  computed by agentsession, which has a canonicaliser. The root module
  depends on `openresponses` and the standard library alone, and RFC
  8785 over this shape is small enough to carry; whether it should is
  open.
- **The arguments-object check.** Refusing arguments that are not a
  JSON object is done by agentturn before the executor and by
  mcpserver before the handler. Whether the executor should do it, so
  a tool run any other way gets the same guard, is open.
- **Optional fields under strict mode.** A field that is optional but
  not nullable, a Go `int` with `omitempty`, is required under strict
  mode with no way to send "absent", so the model must always supply
  it. The provider's guidance is to express an optional field as a
  union with `null`. Widening every optional field to nullable under
  strict mode would follow that guidance and change every strict
  schema's hash, so it is a draft change rather than a fix.
- **Transient errors.** Replay says whether running a call again is
  safe, not whether it is worth it. A harness that retries a dropped
  connection or a rate limit without the model needs a tool to mark
  the failure transient, and perhaps when to try again; a transient
  failure of a call that is *unknown* is still ambiguous and still
  not run again. MCP has no merged shape for it, and the proposals
  differ, `retryable` with a not-before time in a `_meta` block, a
  rate-limit error code, so it waits for one to settle. The MCP
  client handles its own transport failures meanwhile.

## Changes since 0.5

- The MCP binding carries facts and replay (#79): `execution/facts`, a
  method that takes a response's calls together and answers each with
  its facts, or their error, and its replay, advertised as an
  experimental capability. Facts presence, replay, read-only,
  sequential and resource cross in each tool's `_meta`, so a consuming
  host no longer names sequential and resource by hand; its own
  resource still wins. *Keyed* replay reads as *unknown* over MCP,
  since the idempotency key does not cross.

## Changes since 0.4

- Properties gain **facts**, per call, absent by default: the calls a
  call amounts to in tool-call terms and the arguments it runs with if
  a policy allows it, with presence a reader can see without asking.
  The Go binding forwards it through `Wrap`; the MCP binding does not
  carry it yet (#77).

## Changes since 0.3

- The Go binding exposes the chains a batch runs in through
  `Executor.Chains`, so a harness that orders a call's dispatch after
  its predecessor's settlement reads the executor's grouping rather
  than copying its rules (#66). The grouping itself is unchanged.
- The MCP binding carries elicitation both ways: a tool served by
  mcpserver asks through the elicitor on its context, as it would in
  process (#51).
- The MCP binding sets confined on the consuming side, through
  `mcpclient.WithConfined` and `WithConfinedFunc`, as resource is set
  through `WithResource` (#52).

## Changes since 0.2

- Properties gain **replay**, per call, *unknown* by default, with the
  rule that annotations do not stand in for it and that an unknown
  value reads as *unknown*.
- A call gains an **idempotency key**, minted by the harness and kept
  when a call runs again.
- Lifecycle gains [Running a call again](#running-a-call-again): what
  makes an outcome ambiguous, what a harness may do for each answer,
  and what a keyed tool owes.
- Conformance, both bindings and prior art follow.
- Open questions gain transient errors.
- The Go binding sets confined, replay and closer on a tool built by
  `New` or `NewFunc`, through `WithConfined`, `WithReplay` and
  `WithCloser`, so a tool that owns a process and runs it in a sandbox
  needs no type of its own (#49). A tool built without them reads as
  before. The open question on replay without a type is closed by it.
- The Go binding gains an elicitor on the call's context, so a question
  a tool asks the user mid-call reaches the harness as part of that
  call, and mcpclient carries MCP's elicitation to it (#48).
- Resource scope is settled as one batch, with the guard across
  batches the tool's own (#35); its open question is closed.

## Changes since 0.1

- The schema section is written. It defines the argument shape a
  generator reads, the members a generated schema carries and their
  order, the three arrays whose order the definition hash sees, the
  non-strict and strict rules as schema rules, the validation a
  generated schema supports, the Go source mapping as a table, and the
  fixture corpus with its manifest.
- Conformance gains a schema generator, held to the corpus.
- Open questions gain optional fields under strict mode.
- From review of the draft: a nullable field's enum lists `null`
  under strict mode, since `enum` is an assertion of its own; a byte
  array is an array of integers, as the encoder writes it; and a tool
  with no arguments is served the empty object schema by every
  constructor, never `null`, so its definition has one hash.
