# Changelog

All user-visible changes to this library. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project
uses [Semantic Versioning](https://semver.org/); before v1.0.0 minor
versions may break the API.

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
