# Changelog

All user-visible changes to this library. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project
uses [Semantic Versioning](https://semver.org/); before v1.0.0 minor
versions may break the API.

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
