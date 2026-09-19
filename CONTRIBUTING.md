# Contributing

Issues and pull requests are welcome.

## Before you start

This library is the tool contract, not an agent loop. A change that
needs to know about turns, transcripts or models belongs in
`agentturn`, which depends on this module and not the reverse. The
native contract is the centre and MCP is an edge: the adapters map to
and from `Tool` and add nothing the contract cannot express.

For anything larger than a bug fix, open an issue first so the shape of
the change can be discussed before you spend time on it.

## Development

Go 1.25 or later is required. The full local check is:

```sh
make check        # gofmt, tidy, vet, deps, staticcheck, govulncheck, race tests, every module
```

The root module depends on `openresponses` and the standard library
only; `make deps` fails if anything else creeps in. The MCP adapters are
nested modules, `mcpclient` and `mcpserver`, listed under `SUBMODULES`
in the Makefile. A bare `go test ./...` at the root does not cover
them; the Makefile targets do.

The adapters also have an interoperability run against the upstream
implementations, the reference `server-everything` and the Inspector
CLI, both started through `npx`:

```sh
make interop
```

Schema generation has golden fixtures under `testdata/schema/`;
regenerate them with `go test . -update` and review the diff.

## Pull requests

- Keep the change focused; unrelated cleanups belong in their own PR.
- Add or update tests. Tests are table-driven and run offline.
- Run `make check` before pushing. CI runs the same steps on the minimum
  and current Go versions.
- Note user-visible changes under *Unreleased* in `CHANGELOG.md`.

## Releases

Releases are annotated tags. The tag message becomes the GitHub release
notes, so write it as one:

```sh
git tag -a v0.1.0 -m "v0.1.0: one line per user-visible change"
git push origin v0.1.0
```

The release workflow publishes the GitHub release, and the Go module
proxy picks the version up from the tag. Before v1.0.0 the API may
change between minor versions; the changelog records every break.
