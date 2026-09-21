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
in the Makefile and joined to the root by `go.work`. A bare `go test
./...` at the root does not cover them, even in workspace mode; the
Makefile targets do. Workspace mode rejects `-mod=mod`, so a
`GOFLAGS=-mod=mod` in your environment has to go.

A nested module's `go.mod` requires released versions of the root and of
any sibling, and carries no `replace`: the workspace is what builds it
against the tree. That is deliberate. A `replace` is a property of the
main module and consumers ignore it, so a nested module that carried one
would build green here while shipping a `go.mod` that names versions
without the API it uses. `make release-check` builds each nested module
with `GOWORK=off`, against the versions its own `go.mod` requires, which
is what a consumer gets.

`release-check` is not part of `check`, and it fails by design between a
root API addition and the next root tag — a nested module that uses the
new API cannot name a version that carries it until that version exists.
That failure is the release ordering, not a bug; see below.

`make no-replace` is what keeps `release-check` honest, and it *is* part
of `check` and of CI. Re-adding a `replace` makes every other gate green
again after one `make tidy` — including `release-check`, on a module no
consumer can build — so the absence of one is asserted on every run
rather than only at release.

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

Every module in the repository shares one version, but not one commit.
A nested module's requirement cannot name a tag that does not exist
yet, so the root is released first and the nested modules follow. With
the changelog's *Unreleased* section written:

```sh
make release-root VERSION=v0.1.0
```

dates the changelog, runs `make check`, commits, tags `v0.1.0` with the
changelog section as the message, and pushes the branch and the tag.
The nested modules still require the previous root release across this
commit, which is correct: `v0.1.0` did not exist when it was written.

Once that tag is on the module proxy:

```sh
make release-submodules VERSION=v0.1.0
```

walks `SUBMODULES` in order and, for each, sets its requirement on the
root and on any already-released sibling to the version, tidies, builds
and tests it with `GOWORK=off` against exactly those versions, commits,
tags `<dir>/v0.1.0` and pushes. One commit and one tag per module,
because `go mod tidy` and the `GOWORK=off` build both resolve a sibling
requirement from the proxy: `mcpclient` has to be published before
`mcpserver` is bumped. A module is built the way a consumer builds it
before its tag is written, and the release workflow runs `make
release-check` again on the tag — scoped to that tag's module, since the
others are still on the previous root at that commit.

If a module fails partway, the tags already pushed stay valid and
self-consistent; nothing has to be deleted. The failure will usually
have left that module's `go.mod` and `go.sum` rewritten, so:

```sh
git checkout -- mcpserver                 # the module that failed
make release-submodules VERSION=v0.1.0 RELEASE_SUBMODULES=mcpserver
```

Narrow `RELEASE_SUBMODULES`, never `SUBMODULES`: the first is the list to
release, the second is the list of siblings to bump, and narrowing the
second would tag `mcpserver` still requiring the old `mcpclient` —
which `release-check` cannot catch, because the old `mcpclient`
satisfies it. `release-submodules` refuses a `SUBMODULES` override for
that reason.

The release workflow publishes a GitHub release per tag, and the Go
module proxy picks the versions up. Before v1.0.0 the API may change
between minor versions; the changelog records every break.
