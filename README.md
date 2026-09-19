# agenttool

The tool contract for Go agents over
[Open Responses](https://www.openresponses.org): what a tool is, how a
typed Go function becomes one, how its JSON Schema is generated and
validated, how a batch of calls executes, and MCP adapters in both
directions. It is the contract that
[agentturn](https://github.com/ChristopherDavenport/agentturn) runs and
that any other loop can run; no loop is imported here.

- The root module depends on `openresponses` and the standard library.
- `mcpclient` and `mcpserver` are nested modules on the official MCP
  Go SDK.

## Install

```sh
go get github.com/ChristopherDavenport/agenttool
```

Go 1.25 or later.

## A tool

```go
type ReadFileArgs struct {
	Path     string `json:"path" desc:"Absolute path to read"`
	MaxBytes int    `json:"max_bytes,omitempty" desc:"Stop after this many bytes"`
}

var ReadFile = agenttool.New("read_file", "Read a file from disk",
	func(ctx context.Context, a ReadFileArgs) (string, error) {
		b, err := os.ReadFile(a.Path)
		return string(b), err
	})
```

`New` reflects the schema from the argument struct at registration
time. The generator understands `json`, `desc` and `enum` tags, nested
structs, slices, maps, pointers and `time.Time`, and produces the strict
form on request (`WithStrict()`: every field required,
`additionalProperties: false`, pointers nullable). Arguments are
validated against that schema before the function runs, so a missing
required property or a value outside an enum reaches the model as an
error it can retry on rather than a zero value.

The return type decides what the model sees: a `string` as text,
`openresponses.Contents` as parts, so image-returning tools need no
second constructor, and anything else as JSON. Errors returned from the
function become error outputs; a tool never encodes an error as content.

## The contract

```go
type Tool interface {
	Name() string
	Description() string
	Parameters() json.RawMessage
	Execute(ctx context.Context, call Call) (Result, error)
}
```

`Call` carries the call ID, the raw arguments and an optional progress
callback; `Result` carries the output the model sees, app-only
`Details`, and a `Terminate` hint. Two optional interfaces refine a
tool: `Sequential` forces a batch containing it to run one call at a
time, and `Strict` marks its schema strict. `Func` builds a tool from
plain values for schemas that come from elsewhere; `Set` is a list with
lookup; `Definition` produces the `openresponses.FunctionTool` for a
request.

`Executor` runs a batch: parallel up to a bound, or sequential when
asked, with progress forwarded and completions yielded in completion
order from the caller's goroutine. A loop that owns its own scheduling
needs only the interface.

## MCP

The native contract is the centre and MCP is an edge: an MCP tool is a
name, a description, an input schema and a call over a transport, which
is a subset of `Tool`. Both adapters use
`github.com/modelcontextprotocol/go-sdk` and map, nothing more.

```go
// Consume: a remote server's tools as Tool values.
s, err := mcpclient.Connect(ctx, &mcp.CommandTransport{Command: cmd}, mcpclient.WithPrefix("fs"))
tools := s.Tools() // fs__read, fs__write, ...

// Serve: Go tools over MCP.
server := mcpserver.NewServer("my-tools", "0.1.0", ReadFile)
server.Run(ctx, &mcp.StdioTransport{})
```

`mcpclient` maps text, image, audio and resource content to output
parts, `isError` to a returned error, progress notifications to the
call's progress callback, and refreshes its snapshot on
tool-list-changed. `mcpserver` validates arguments against each tool's
schema, maps outputs back to MCP content and forwards progress when
the request carries a token. `make interop` checks both against the
upstream reference server and the MCP Inspector over stdio.

## Development

```sh
make check    # gofmt, vet, deps, staticcheck, govulncheck, race tests, every module
make interop  # upstream MCP interoperability; needs npx and the network
```

See `CONTRIBUTING.md`.

## License

MIT. See `LICENSE`.
