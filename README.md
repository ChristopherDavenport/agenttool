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
and the output is ignored. `Details` that implement `Recordable`, one
method naming a namespace, can be written to a session by a recorder
that does not know their type: `RecordOf` gives the namespace and the
value's JSON. Optional interfaces refine a tool: `Sequential` forces a
batch containing it to run one call at a time, `Resource` serialises
the calls that touch one piece of shared state, `Annotated` carries
MCP's behavioural hints for a policy layer to read, `Confined` says
whether a call will run in a sandbox and by what, so a shared
permission preset can ask about the calls that leave one without
knowing a product's own argument for leaving it, and `Strict` marks
its schema strict; `WithSequential()`, `WithResource()`,
`WithAnnotations()` and `WithStrict()` set them on a tool from `New` or
`NewFunc`. `NewFunc` builds a tool from plain values
and a raw function for schemas that come from elsewhere; `SchemaFor[T]()`
gives the schema `New` would reflect; `Set` is a list with lookup;
`Definition` produces the `openresponses.FunctionTool` for a request.

## A handle on the record before the call ends

`Result.Details` is read when the call ends, so a tool that is killed
while it runs leaves nothing behind, and that is the one case where a
handle to what it started is wanted. A tool knows the moment the side
effect begins, so it writes the handle then:

```go
var Bash = agenttool.New("bash", "Run a shell command",
	func(ctx context.Context, a BashArgs) (string, error) {
		cmd := exec.CommandContext(ctx, "bash", "-c", a.Command)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			return "", err
		}
		// Durable before the first byte of output: a daemon that comes
		// back after a crash can reap the group.
		if err := agenttool.WriteRecord(ctx, ProcessGroup{PGID: cmd.Process.Pid}); err != nil {
			return "", err
		}
		return wait(cmd)
	})

type ProcessGroup struct {
	PGID int `json:"pgid"`
}

func (ProcessGroup) RecordNS() string { return "shell:process-group" }
```

`WriteRecord` returns when the write is durable and is a no-op when no
recorder is installed, so a tool calls it unconditionally and a test
installs nothing. The harness puts the recorder on the context, either
per call with `ContextWithRecorder` or once on the executor:

```go
exec := agenttool.Executor{Recorder: func(ctx context.Context, rec *agenttool.Record) error {
	call, _ := agenttool.CallFrom(ctx) // which call this belongs to
	return session.AppendCustom(ctx, rec.NS, call.ID, rec.Data)
}}
```

The record a tool writes this way and the `Recordable` it returns as
`Details` are the same namespace at two moments: what it started, and
how it ended.

## Interrupting a call, closing a tool

A tool that owns a process, a container or a persistent shell has two
moments, and the contract answers them separately.

Stopping a call in flight is the call's context. It is cancelled from
another goroutine while `Execute` runs, which is what a host's Ctrl-C
is, so a tool signals or kills what that call started and returns; a
partial result with no error is the right answer when the output so far
is worth the model's while. The shell the tool value owns is untouched,
because the context belongs to the call.

```go
func (s *Shell) Execute(ctx context.Context, call agenttool.Call) (agenttool.Result, error) {
	out, err := s.start(call.Args)
	select {
	case res := <-out:
		return res, err
	case <-ctx.Done():
		s.signal(os.Interrupt)               // the foreground command, not the shell
		return agenttool.Text(s.drain()), nil // what it printed before the interrupt
	}
}

func (s *Shell) Close() error { return s.container.Remove() }
```

There is no `Interrupt` method. The reference agent that has one has it
because its language has no cancellation to hand, and a second way to
say the same thing would leave a tool guessing which one a host used.
The model's own interrupt, "send `C-c` and keep the shell", is an
argument of the shell tool and needs nothing from the contract.

Releasing what outlives the call is `io.Closer`. It belongs to the host
and never to a run or a batch, since a session outlives many runs and
the executor closes nothing; `agenttool.Set(tools).Close()` closes the
ones that implement it and joins their errors.

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
completion order, not job order.

A tool that owns shared state names it, and only the calls that touch
it wait for each other:

```go
var Bash = agenttool.New("bash", "Run a shell command", run,
	agenttool.WithResource("shell:session"))
```

Two `bash` calls in one batch then run one after the other in the
model's order, while the five reads beside them still run together.
`Sequential` remains the wider claim, "nothing else runs while I do",
and takes the whole batch; a tool that reports both is sequential. Two
tools that name the same resource share it, which is how a shell tool
and the tool that restarts that shell stay apart, and
`mcpclient.WithResource("shell:session", "bash")` names it for a remote
tool, since MCP has no field for one. Whether the second call waits or
is refused stays the tool's choice. A tool that panics completes with a
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
parts, `isError` to a returned error, a tool's annotations to
`agenttool.Annotations`, progress notifications to the call's progress
callback, and refreshes its snapshot on tool-list-changed. The hints
say a `search` is read-only and a `delete_repo` destructive, which is
what a permission layer keys on instead of a per-server name list;
`AnnotationsOf` reads them from any tool and `mcpserver` serves the
ones a Go tool carries, so they survive a round trip. They are the
server's word, so a policy may use them to be stricter and may not use
them alone to allow a call. The refresh is a round trip: `Await` blocks until
it has landed, and a call that returns after the notification arrived
waits for it. A server need not notify before it answers, and the
reference Go SDK arms the notification on a 10 ms timer, so a call
whose list looks unchanged waits up to
`mcpclient.DefaultNotificationGrace` for one before returning; a tool
that adds a tool then returns with the list already current.
`WithNotificationGrace` bounds or disables that wait, which is skipped
for a read-only tool and for a server that advertises no
tool-list-changed notification. `mcpserver` validates arguments against each tool's
schema, maps outputs back to MCP content and forwards progress when
the request carries a token. One `Tool` value serves every client that
connects, so the call's context carries the session it arrived on:
`mcpserver.SessionFrom(ctx)` is what a tool holding a working
directory, a container or a shell keys that state on, and two editor
windows then get two shells instead of one. `make interop` checks both against the
upstream reference server and the MCP Inspector over stdio.

## Development

```sh
make check    # gofmt, vet, deps, staticcheck, govulncheck, race tests, every module
make interop  # upstream MCP interoperability; needs npx and the network
```

See `CONTRIBUTING.md`.

## License

MIT. See `LICENSE`.
