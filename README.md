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
		if err != nil {
			return "", err
		}
		return string(b), nil
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
`Details`, and a `Terminate` hint. On error the model sees the error
and the output is ignored. Two optional interfaces refine a tool:
`Sequential` forces a batch containing it to run one call at a time,
and `Strict` marks its schema strict; `WithSequential()` and
`WithStrict()` set them on a tool from `New` or `NewFunc`. `NewFunc`
builds a tool from plain values and a raw function for schemas that
come from elsewhere; `SchemaFor[T]()` gives the schema `New` would
reflect; `Set` is a list with lookup; `Definition` produces the
`openresponses.FunctionTool` for a request.

## A batch

`Executor` runs a batch: parallel up to a bound, or sequential when
asked. Events are yielded from the caller's goroutine, so a consumer
never sees two at once.

```go
jobs := []agenttool.Job{{Tool: ReadFile, Call: agenttool.Call{ID: call.ID, Args: call.Arguments}}}
outputs := make([]agenttool.Result, len(jobs))
for ev := range (agenttool.Executor{MaxParallel: 4}).Execute(ctx, jobs) {
	switch {
	case !ev.Final:
		show(ev.Result) // progress the tool reported through Call.Update
	case ev.Err != nil:
		outputs[ev.Index] = agenttool.ErrorResult(ev.Err)
	default:
		outputs[ev.Index] = ev.Result
	}
}
```

An `Event` names its job by `Index`. It is `Final` exactly once per
job, carrying the `Result` or the `Err`, and completions arrive in
completion order, not job order. A tool that panics completes with a
`PanicError` whose message is one line; the stack is on the value for
`errors.As`. `Results` is the shortcut when progress is not needed. A
loop that owns its own scheduling needs only the interface.

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
server, err := mcpserver.NewServer("my-tools", "0.1.0", ReadFile)
err = server.Run(ctx, &mcp.StdioTransport{})
```

`mcpclient` maps text, image, audio and resource content to output
parts, `isError` to a returned error, progress notifications to the
call's progress callback, and refreshes its snapshot on
tool-list-changed. The refresh is a round trip: `Await` blocks until
it has landed, and a call that returns after the notification arrived
waits for it, so a tool that adds a tool usually returns with the list
already current. `mcpserver` validates arguments against each tool's
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
