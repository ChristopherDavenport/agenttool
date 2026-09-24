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
make check        # gofmt, tidy, vet, deps, replaces, staticcheck, govulncheck, race tests, every module
```

The root module depends on `openresponses` and the standard library
only; `make deps` fails if anything else creeps in. The MCP adapters are
nested modules, `mcpclient` and `mcpserver`, listed under `SUBMODULES`
in the Makefile and joined to the root by `go.work`. A bare `go test
./...` at the root does not cover them, even in workspace mode; the
Makefile targets do. Workspace mode rejects `-mod=mod`, so a
`GOFLAGS=-mod=mod` in your environment has to go.

Each nested module's `go.mod` requires the root — and any sibling it
uses — at **exactly the version the whole repository is released at**,
and carries a matching `replace` pointing at the tree. The two go
together. Requiring the version being released means the release commit
names a version the proxy cannot serve until its tag is pushed, and
`go mod tidy` ignores `go.work`; the `replace` is what lets tidy, build
and test resolve it locally. Consumers ignore a `replace` in a
dependency and get the `require`, which names the commit the module was
tagged from.

`make replaces`, part of `check`, refuses a first-party require that
lacks a replace — losing one would make the next release fail at `make
tidy`, or silently pin that module to the previous release. This is the
same shape OpenTelemetry-Go publishes.

A side effect worth knowing: the nested modules now carry no first-party
`go.sum` entries at all, because tidy never resolves one from the proxy.
Two of those entries used to be wrong — see `CLAUDE.md` — and the class
of bug is gone rather than fixed.

The upshot: a consumer who takes only `agenttool/mcpserver` at vX.Y.Z
gets root vX.Y.Z and `mcpclient` vX.Y.Z, the exact commit it was built
and tested against. The workspace build and the consumer build are the
same code, so there is no `release-check` and no phased release — both
existed to manage a nested module requiring a *previous* root, which no
longer happens.

## Releases

`CLAUDE.md` holds the full procedure and the reasoning, including what to
do when a tag goes out wrong. The essentials:

Every published module is released at one version, from one commit, and
requires its first-party siblings at exactly that version. With the
changelog's *Unreleased* section written:

```sh
make release VERSION=v0.1.0
```

points every nested module's first-party requires at `v0.1.0`, dates the
changelog, runs `make tidy` and `make check`, reads the requires back to
confirm tidy did not move them, commits, then guards and tags the root,
guards and tags `mcpclient/v0.1.0` and `mcpserver/v0.1.0`, and pushes the
branch and all three tags with `git push origin --atomic`.

That `mcpserver` requires `mcpclient` no longer changes anything. Both
are tagged from the same commit, and `mcpserver` resolves `mcpclient`
through the replace rather than the proxy, so there is no ordering to
respect between them.

`make release-guard TAG=<tag>` is what stands between a mistake and a
permanent one. It refuses a dirty tree, a tag that already exists, a
version that sorts below the current root release or does not move its
module forward, a first-party require that does not name that version, a
root tag that is not this commit, and a module that will not build with
`GOWORK=off`. `make release` runs it for every tag it writes, and the
root is guarded and tagged first because a nested module's guard needs
the root tag to exist.

Nothing is public until the push. If a guard refuses, `git reset --hard
HEAD~1` and `git tag -d` whatever was written.

The release workflow publishes a GitHub release per tag, and the Go
module proxy picks the versions up. Before v1.0.0 the API may change
between minor versions; the changelog records every break. A pushed
version is permanent — the proxy and the checksum database keep it
forever — so a bad one is superseded and `retract`ed, never deleted.
