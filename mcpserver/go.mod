module github.com/ChristopherDavenport/agenttool/mcpserver

go 1.25.0

// The root and sibling requirements name the released versions a
// consumer fetches. The workspace builds this module against the tree
// instead; there are deliberately no replaces, so release-check can
// build it the way a consumer does and fail while a version named here
// is too old.
require (
	github.com/ChristopherDavenport/agenttool v0.0.7
	github.com/ChristopherDavenport/agenttool/mcpclient v0.0.7
	github.com/ChristopherDavenport/openresponses v0.0.12
	github.com/google/jsonschema-go v0.4.3
	github.com/modelcontextprotocol/go-sdk v1.8.0
)

require (
	github.com/segmentio/asm v1.1.3 // indirect
	github.com/segmentio/encoding v0.5.4 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	golang.org/x/oauth2 v0.35.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/sys v0.41.0 // indirect
	golang.org/x/time v0.15.0 // indirect
)

replace github.com/ChristopherDavenport/agenttool => ..

replace github.com/ChristopherDavenport/agenttool/mcpclient => ../mcpclient
