# Changelog

All user-visible changes to this library. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project
uses [Semantic Versioning](https://semver.org/); before v1.0.0 minor
versions may break the API.

## Unreleased

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
