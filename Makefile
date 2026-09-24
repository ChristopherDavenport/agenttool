GO ?= go
STATICCHECK ?= $(GO) run honnef.co/go/tools/cmd/staticcheck@latest
GOVULNCHECK ?= $(GO) run golang.org/x/vuln/cmd/govulncheck@latest
# The module path of the root, which every first-party require and
# replace is written against. go list -m reports every module in the
# workspace, so the root has to be asked for outside it.
MODULE := $(shell GOWORK=off $(GO) list -m)

# Nested modules that are tested alongside the library but keep their own
# dependencies out of it. Each requires the root, and any sibling it uses,
# at exactly the version the whole repository is released at, and carries
# a replace pointing at the tree — see replaces below, and CLAUDE.md for
# why the two go together. mcpserver requires mcpclient, so the list is
# in dependency order.
SUBMODULES = mcpclient mcpserver

.PHONY: build deps replaces test vet fmt tidy tidy-check lint vuln check \
	extracted interop release-guard release release-commit clean

build:
	$(GO) build ./...
	@for m in $(SUBMODULES); do (cd $$m && $(GO) build ./...) || exit 1; done

# The root module is the tool contract and must build from openresponses
# and the standard library alone; the MCP adapters are nested modules.
deps:
	@deps=$$($(GO) list -deps -f '{{if not .Standard}}{{.ImportPath}}{{end}}' ./... | grep -v '^github.com/ChristopherDavenport/agenttool' | grep -v '^github.com/ChristopherDavenport/openresponses' || true); \
	  test -z "$$deps" || { echo "root module depends on: $$deps"; exit 1; }

# Every first-party module a nested module requires must also be
# replaced, at a path that exists. This is the inverse of the rule this
# repository used to carry, and it is load-bearing rather than
# cosmetic: go mod tidy ignores go.work, so a require naming the version
# being released resolves from the proxy, where that version does not
# exist until the tag is pushed. The replace is what lets a release name
# its own version. Lose one and the next release fails at make tidy, or
# worse, silently pins the module to the previous release.
#
# A replace is a property of the main module, so consumers ignore it and
# get the require. That is safe here only because the require names the
# commit the module is tagged from; release-guard is what proves it.
replaces:
	@scripts/check-replaces.sh $(SUBMODULES)

test:
	$(GO) test -race ./...
	@for m in $(SUBMODULES); do (cd $$m && $(GO) test -race ./...) || exit 1; done

vet:
	$(GO) vet ./...
	@for m in $(SUBMODULES); do (cd $$m && $(GO) vet ./...) || exit 1; done

tidy:
	$(GO) mod tidy
	@for m in $(SUBMODULES); do (cd $$m && $(GO) mod tidy) || exit 1; done

# Fails when go mod tidy would change any go.mod or go.sum, without
# writing, so a stray dependency shows up in make check and not only in
# CI's diff.
tidy-check:
	$(GO) mod tidy -diff
	@for m in $(SUBMODULES); do (cd $$m && $(GO) mod tidy -diff) || exit 1; done

fmt:
	gofmt -l . && test -z "$$(gofmt -l .)"

lint:
	$(STATICCHECK) ./...
	@for m in $(SUBMODULES); do (cd $$m && $(STATICCHECK) ./...) || exit 1; done

vuln:
	$(GOVULNCHECK) ./...
	@for m in $(SUBMODULES); do (cd $$m && $(GOVULNCHECK) ./...) || exit 1; done

# Everything CI runs.
check: fmt tidy-check vet deps replaces lint vuln test

# Builds, vets and tests each nested module the way a consumer gets it:
# extracted to
# a directory with no parent go.mod, with the in-tree replaces dropped,
# so the require lines are answered by the proxy. replaces above checks
# that a require is present and release-guard checks that it names the
# version being tagged; both are claims about a version string, and
# neither compiles anything against it. Point agenttool's mcpclient at
# agenttool v0.0.1 and both stay silent while this fails with
# "undefined: agenttool.WithResource".
#
# release-guard.sh had this once, at openresponses v0.0.11: "build it the
# way a consumer does", running build, vet and test. Introducing the
# replace at v0.0.12 turned that line into a build against the tree,
# because GOWORK=off stopped meaning "no local root", and it went on
# printing ok. release-guard.sh now calls this script in its place.
#
# Needs the network, so it is not part of check. It could not be anyway:
# release-commit points every require at the version being released, and
# the proxy cannot serve that until the tag is pushed. CI runs it on
# pull requests and on main.
extracted:
	@scripts/check-extracted.sh $(SUBMODULES)

# Interoperability with the upstream MCP implementations: mcpclient
# against @modelcontextprotocol/server-everything and mcpserver under the
# MCP Inspector CLI, both over stdio. Needs npx and the network, so it is
# not part of check.
interop:
	cd mcpclient && MCP_INTEROP=1 $(GO) test -race -count=1 -run TestInterop ./...
	cd mcpserver && MCP_INTEROP=1 $(GO) test -race -count=1 -run TestInterop ./...

# Checks one tag is safe to push, before it is pushed. A pushed tag is
# permanent — the proxy and the checksum database keep the version
# forever — so this is the last point at which a mistake is free:
#   make release-guard TAG=mcpclient/v0.0.7
release-guard:
	@test -n "$(TAG)" || { echo "usage: make release-guard TAG=<tag>"; exit 1; }
	@scripts/release-guard.sh "$(TAG)"

# Every tag a release writes: the root and one per nested module, all at
# the same version, all from the one commit below.
RELEASE_TAGS = $(VERSION) $(patsubst %,%/$(VERSION),$(SUBMODULES))

# Cut a release:
#
#   make release VERSION=v0.1.0
#
# Every published module is released at one version, from one commit, and
# requires its first-party siblings at exactly that version. So the first
# thing this does is point every nested module at VERSION — a version
# that does not exist yet. That resolves because each nested go.mod
# replaces its first-party requirements with the tree (see replaces
# above); tidy, build and test all see the code being tagged, which is
# the code the version will contain.
#
# The consequence worth naming: a consumer who takes only
# agenttool/mcpclient at vX.Y.Z gets root vX.Y.Z, the exact commit that
# module was built and tested against. There is no drift to gate against,
# which is why there is no release-check here and no phased release.
#
# --atomic lands every ref in one transaction, so no window exists in
# which one tag is visible without the others, and none in which a
# published go.mod names a version the proxy cannot serve.
#
# Nothing is public until the push on the last line. If a guard refuses,
# undo with git reset --hard HEAD~1 and git tag -d the tags written.
# The root is guarded and tagged first, then each nested module, because
# a nested module's guard proves the root tag of that version names this
# commit — which it cannot do before that tag exists. Every tag is local
# until the push on the last line.
release: release-commit
	@scripts/release-guard.sh "$(VERSION)"
	@notes="$$(scripts/release-notes.sh $(VERSION))" || exit 1; \
	 git tag -a $(VERSION) -m "$$notes"
	@notes="$$(scripts/release-notes.sh $(VERSION))" || exit 1; \
	 for m in $(SUBMODULES); do \
	   scripts/release-guard.sh "$$m/$(VERSION)" || exit 1; \
	   git tag -a $$m/$(VERSION) -m "$$notes" || exit 1; \
	 done
	git push origin --atomic HEAD $(RELEASE_TAGS)

# Bump every first-party requirement to VERSION, date the changelog,
# check everything, commit. Nothing here is pushed, so a failure costs a
# git reset and no more. TRAILER, when set, is appended to the commit
# message.
#
# go mod tidy is free to move a requirement the go mod edit just set, so
# what landed is read back and asserted before anything is committed.
#
# The changelog is dated through a temp file rather than sed -i, which is
# a GNU-ism: BSD sed reads the argument after -i as a backup suffix, so
# the GNU spelling fails outright on macOS, where these releases are cut.
# The temp file is removed if sed dies, so a failed run leaves nothing
# untracked behind for the clean-tree gate to trip over next time.
release-commit:
	@test -n "$(VERSION)" || { echo "usage: make release VERSION=vX.Y.Z"; exit 1; }
	@test $(words $(RELEASE_TAGS)) -le 3 || { \
	  echo "$(words $(RELEASE_TAGS)) tags would be pushed at once, and GitHub creates no events"; \
	  echo "for a push of more than three tags — every tag would land and the release"; \
	  echo "workflow would silently never run. Adding a fourth published module means"; \
	  echo "choosing: push the tags one at a time and lose the atomic push (what agentturn"; \
	  echo "does), or keep --atomic and create the GitHub releases from here with gh."; \
	  exit 1; }
	@test "$(origin SUBMODULES)" = file || { echo "do not override SUBMODULES here: a command-line override propagates into the bump, tidy and check below, so a module would be tagged having checked a subset."; exit 1; }
	@grep -q '^## Unreleased$$' CHANGELOG.md || { echo "CHANGELOG.md has no Unreleased section"; exit 1; }
	@test -z "$$(git status --porcelain)" || { echo "working tree is not clean"; exit 1; }
	@scripts/versions.sh set $(VERSION) $(SUBMODULES)
	sed 's/^## Unreleased$$/## $(VERSION) - '"$$(date +%F)"'/' CHANGELOG.md > CHANGELOG.md.tmp \
	  && mv CHANGELOG.md.tmp CHANGELOG.md \
	  || { rm -f CHANGELOG.md.tmp; exit 1; }
	$(MAKE) tidy
	$(MAKE) check
	@scripts/versions.sh check $(VERSION) $(SUBMODULES)
	git add -A && git commit -q -m "Release $(VERSION)" $(if $(TRAILER),-m "$(TRAILER)")

clean:
	rm -rf .cache
