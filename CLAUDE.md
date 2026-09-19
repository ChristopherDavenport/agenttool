# agenttool

The tool contract for Go agents over Open Responses: what a tool is, how
a typed Go function becomes one, how its JSON Schema is generated and
validated, how a batch of calls executes, and the MCP adapters that
serve Go tools and consume remote ones. It is the contract that
`../agentturn` runs and that any other loop can run; the loop is never
imported here.

## Module

- Module path: `github.com/ChristopherDavenport/agenttool`.
- Go 1.25 is the floor. The root package name is `agenttool`.
- The root module depends on
  `github.com/ChristopherDavenport/openresponses` (pin v0.0.9 or later)
  and the standard library. Nothing else. `make deps` and a test enforce
  it.
- Anything with another dependency is a nested module with its own
  `go.mod`: `mcpclient` (remote MCP tools as `Tool` values) and
  `mcpserver` (Go tools served over MCP), both on
  `github.com/modelcontextprotocol/go-sdk`.

## Siblings

Peer repositories, each independently versioned, each depending on
`openresponses` and never on each other's root module except as stated:

- `../open-responses`: the wire package. Copy its conventions.
- `../agentturn`: the agent loop. It depends on this module; this module
  never depends on it.
- `../agentsession`: the session format. Unrelated to this module.

## Conventions

Mirror `../open-responses`: a `Makefile` with `build`, `deps`, `test`,
`vet`, `fmt`, `tidy`, `lint`, `vuln` and `check` targets, a `SUBMODULES`
list for nested modules, the same CI workflow shape (minimum and stable
Go, lint, tidy-check), a `CHANGELOG.md` in Keep a Changelog form, and
annotated `v*` tags whose message becomes the release notes. `make
check` must pass before any commit. `make interop` runs the MCP adapters
against the upstream reference server and the MCP Inspector; it needs
`npx` and the network.

Tests are table-driven and offline. Schema generation has golden
fixtures under `testdata/schema/`; regenerate with `go test . -update`
and review the diff.
