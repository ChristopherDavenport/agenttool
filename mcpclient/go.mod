module github.com/ChristopherDavenport/agenttool/mcpclient

go 1.25.0

// The root requirement names the released version a consumer fetches.
// The workspace builds this module against the tree instead; there is
// deliberately no replace, so release-check can build it the way a
// consumer does and fail while the version named here is too old.
require (
	github.com/ChristopherDavenport/agenttool v0.0.5
	github.com/ChristopherDavenport/openresponses v0.0.9
	github.com/modelcontextprotocol/go-sdk v1.8.0
)

require (
	github.com/google/jsonschema-go v0.4.3 // indirect
	github.com/segmentio/asm v1.1.3 // indirect
	github.com/segmentio/encoding v0.5.4 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	golang.org/x/oauth2 v0.35.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/sys v0.41.0 // indirect
	golang.org/x/time v0.15.0 // indirect
)
