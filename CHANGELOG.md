# Changelog

All user-visible changes to this library. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project
uses [Semantic Versioning](https://semver.org/); before v1.0.0 minor
versions may break the API.

## v0.0.6 - 2026-09-21

- A tool can put a handle on the record before it finishes.
  `agenttool.WriteRecord(ctx, details)` writes a [Recordable] through
  the `RecordFunc` a harness installed with `ContextWithRecorder`, and
  returns when the write is durable, so the process group a `bash` tool
  just forked or the temporary directory it just made survives a kill
  mid-call, where `Result.Details` read at the end of the call does
  not. It is a no-op returning nil when no recorder is installed, so a
  tool calls it unconditionally and a test installs nothing.
  `Executor.Recorder` installs one for a whole batch, and every tool
  the executor runs now finds its `Call` on the context with `CallFrom`,
  not only a `New` one, so a recorder can name the call it is writing
  for. `RecorderFrom` reports whether a recorder is installed. The
  `RecordOf` read of `Result.Details` at the end of a call is unchanged;
  a value written both ways is recorded under one namespace twice.

- A call waits for a tool-list-changed notification that has not been
  sent yet. v0.0.4 made a call whose list changed wait for the refresh,
  but the reference Go SDK sends the notification about ten
  milliseconds after the change, so the result reached the client
  first, the wait never engaged, and a tool that added a tool still
  showed it a turn late. A call whose list looks unchanged now waits up
  to `DefaultNotificationGrace`, 50 ms, for a notification before
  returning, and `WithNotificationGrace(d)` bounds or, at zero, turns
  off that wait. The wait ends as soon as the notification lands, and
  is skipped for a server that advertises no tool-list-changed
  notification and for a tool the server annotated read-only, which
  cannot have changed the list without lying.

- A tool can say whether a call will run confined. `Confined` is an
  optional interface, `Confined(ctx, args) (bool, string)`, answering
  for the arguments of one call and naming what confines it, and
  `ConfinedBy` reads it from any tool. Both reference agents put a
  sandbox under one shell tool and make leaving it an argument of that
  tool, so a shared permission preset either prompted for every
  harmless command or named one product's field; now it can ask about
  the calls that leave the sandbox. A tool that does not implement it
  reports false, which says it claims no sandbox rather than that it has
  none, and nothing here enforces anything.

- A tool's lifecycle is stated, and it is two things, not three. A tool
  that owns a container or a persistent shell implements `io.Closer`,
  and `Set.Close` closes the tools of a set in order and joins their
  errors; it is the host's to call when the session is over, never a
  loop's, and the executor closes nothing. Stopping a call that is
  running stays the call's context, which is cancelled from another
  goroutine while `Execute` runs and is what a host's interrupt is: the
  documentation now says so, with the shell that signals its foreground
  command, returns what it printed and keeps the shell. No `Interrupter`
  interface is added, because the reference that has one has it for a
  language with no cancellation primitive, and a second way to say
  "stop" would leave a tool guessing which one a host used; the model's
  own interrupt is an argument of the shell tool.

- `mcpserver` can tell one client from another. The call's context now
  carries the MCP session it arrived on, read with
  `mcpserver.SessionFrom(ctx)` or `mcpserver.SessionID(ctx)` and
  installed by `ContextWithSession` for a call made outside a server.
  One `Tool` value serves every client, so a tool that owns a working
  directory, a container or a shell keys it on the session instead of
  serving two clients one environment; the session ID is set only by a
  transport that negotiates one, so a host maps the session itself.
  `agenttool.Call` is unchanged, since a field there would oblige every
  caller and adapter to fill it.

- A tool carries MCP's behavioural hints. `Annotations` holds the
  title, read-only, destructive, idempotent and open-world hints; a
  tool implements `Annotated` or is built with `WithAnnotations`, and
  `AnnotationsOf` reads them from any tool. `mcpclient` maps a remote
  tool's annotations onto the tools it builds, applying MCP's defaults
  for the hints a server omits from a block it sent and reporting
  nothing for a server that sent no block, and `mcpserver` serves the
  hints a Go tool carries, each stated rather than defaulted, so a
  proxied tool keeps them. They are hints from a server a client has no
  reason to trust: a policy may use them to be stricter and may not use
  them alone to allow a call, which the doc comments say.

- Serialising a tool against itself no longer costs the batch its
  parallelism. A tool that owns shared state implements `Resource`, one
  method naming it, or is built with `WithResource("shell:session")`,
  and `Executor` runs the calls that name the same state one after the
  other in the model's order while the rest of the batch runs alongside
  them; `ResourceOf` reads it. `Sequential` keeps its meaning, "the
  batch is the resource", and a tool that reports both is sequential.
  `mcpclient.WithResource(resource, names...)` names the state of
  remote tools, which carry none of their own. The executor's
  scheduling is rewritten around this: a goroutine per chain of jobs
  rather than per job, bounded by `MaxParallel` as before. Independent
  jobs may now start in any order, which the documented completion
  order already allowed; a serial batch is unchanged, in the model's
  order from end to end. A tool that already has a `Resource() string`
  method of its own now declares one, as any optional interface here
  works.

## v0.0.5 - 2026-09-20

- `Recordable` is implemented by a `Result.Details` value that is meant
  to outlive the run: one method, `RecordNS() string`, names the
  namespace it is recorded under, and its JSON is what `json.Marshal`
  produces, so a type shapes it with `MarshalJSON` as anywhere else.
  `RecordOf(details)` returns the namespace and bytes as a `*Record`,
  nil for any other value, so a recorder can write a tool's side data
  without being compiled against its type. `Details` stays `any`; the
  progress and child-run values that in-process subscribers read are
  unchanged and are not recorded.

## v0.0.4 - 2026-09-19

- `mcpclient.Remote.Await` blocks until the snapshot reflects every
  tool-list-changed notification received so far, returning the error
  of the refresh when it failed, so a loop can bound its wait before
  reading `Tools` instead of finding the refresh in flight. A call that
  returns after a notification arrived waits for the refresh before
  returning, so a tool that changes the tool list usually returns with
  the new list in place; the SDK delivers the notification on its own
  goroutine and a server may send it after the result, so a
  notification that lands after the call is reflected after the next
  `Await`, and `Refresh` remains the way to see a change for certain.
  Notifications in quick succession now cost one listing.

## v0.0.3 - 2026-09-19

- A tool panic now completes with a `PanicError` whose message is one
  line, `tool "name" panicked: value`, so the model no longer receives
  the goroutine stack; hosts recover the stack through `errors.As`.
- `mcpclient.Progress` and `mcpserver.Progress` are replaced by one
  `agenttool.ProgressInfo`. The two were unrelated types, so a remote
  tool consumed by `mcpclient` and served again by `mcpserver` lost its
  progress and total on the second hop; a round-trip test now covers the
  proxy. Callers that named either adapter's type switch to the root one.
- `mcpserver.NewServer`, `AddTools` and `Handler` return an error when a
  tool's schema cannot be parsed or resolved for validation, instead of
  serving the tool with no argument validation and telling nobody. No
  tool is registered when any fails.
- `mcpclient.WithRefreshError` receives the error when the refresh that
  follows a tool-list-changed notification fails; without it the SDK
  logger, when set, records it. Transport errors from a call are wrapped
  as `mcp: call "name": ...`, and an audio part is named after its media
  type, `audio.wav`, rather than a bare `audio`.
- One vocabulary for the contract. `Func` and its `ToolName`,
  `ToolDescription`, `Schema`, `Fn`, `RunAlone` and `StrictSchema`
  fields are replaced by `NewFunc(name, description, parameters, fn,
  opts...)`, which takes the same `WithStrict` and `WithSequential`
  options as `New` and panics on a nil function. `Reflect`, `SchemaOf`
  and `SchemaFor` take those options instead of a positional `strict`
  bool; `SchemaOf` and `SchemaFor` consult `Schemer`, `Reflect` does not,
  and each says so. `Typed` is unexported, since `New` returns `Tool`.
  Panic and error strings say `agenttool` rather than the pre-extraction
  `tool`. `Tool.Execute` documents that `Details` may accompany an error.
  In `mcpclient`, `Server` is `Remote` and `Result` is `ResultOf`, the
  counterpart of `mcpserver.ContentOf`.
- README: the first example handles its error, the `Executor` consumer
  loop and `Event` are shown, and the stdio example names its field and
  uses a `Set`.
- `Schema.MarshalJSON` has a value receiver, so a `Schema` value on its
  own or inside another struct marshals as a pointer does instead of
  silently emitting Go field names. The ordered writer is one type used
  at both levels. The type documents how `NoAdditional` and
  `AdditionalProperties` share a key and what a hand-built object omits.
- `Schema.Validate` rejects a `Type` that is not a JSON Schema type name
  and handles `"null"`, where an unknown name passed everything.
- The generator resolves fields that embedded structs promote under one
  name as `encoding/json` does: shallowest wins, tagged wins among
  equals, and a tie is dropped, where both used to be emitted. A struct
  field that implements `json.Marshaler` is now `{}` like any other
  self-encoding type. `enum` tags work on `uintptr` fields.
- `Executor.Execute` drops the gate, closed flag, done channel, drain
  loop and wait group it carried. The events channel is never closed:
  the consumer reads until it has one final event per job, so a final
  send is always received, and the deferred cancel releases a late
  progress sender. Behaviour is unchanged.

## v0.0.2 - 2026-09-19

- One version per repository. `mcpclient` and `mcpserver` require the
  released root, and `mcpserver` requires `mcpclient`, next to
  `replace` directives that build against the tree, so `go get` works
  for consumers; at v0.0.1 they required a nonexistent `v0.0.0` and
  could not be fetched. `make release VERSION=` sets the requirements,
  dates the changelog, and tags the root and both nested modules at one
  commit; the release workflow publishes nested tags too.

## v0.0.1 - 2026-09-19

- `make check` now includes `tidy-check`, which fails when `go mod tidy`
  would change any module's `go.mod` or `go.sum`; CI uses the same target.

Extracted from `agentturn`, where it was the `tool` package and the
`tools/mcp` and `front/mcp` modules, so the contract is versioned apart
from the loop and usable without it.

- The contract: `Tool`, `Call`, `Result`, `Sequential`, `Strict`,
  `Definition`, `Set`, `Func`.
- `New`: typed tools from a Go function, with a standard-library JSON
  Schema generator (`json`, `desc` and `enum` tags, nested structs,
  slices, maps, pointers, strict mode) and validation of arguments
  against the reflected schema before decoding. `Reflect`,
  `Schema.Validate` and `ValidateJSON` are exported for other uses.
- `Executor`: parallel and sequential batches with bounded concurrency,
  progress forwarding and panic recovery.
- `mcpclient`: a remote MCP server's tools as `Tool` values, with name
  prefixing, content and error mapping, progress and list-changed
  refresh.
- `mcpserver`: Go tools served over MCP, with schema validation of
  arguments, content mapping and progress notifications.
- `make interop` runs both adapters against the upstream reference
  server and the MCP Inspector.
