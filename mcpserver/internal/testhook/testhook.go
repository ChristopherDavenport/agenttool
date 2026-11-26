// Package testhook holds the seams mcpserver gives its own tests and
// conformance example and nothing else. It is internal so no code outside
// the mcpserver module can import it, which is the point: each hook here
// loosens a guard a deployment must keep, so none of them is public API.
//
// Each variable is nil until mcpserver sets it at init, so a caller that
// imports mcpserver finds it set.
package testhook

// TrustWebhookOrigin adds the callback origin "scheme://host[:port]" to
// those a running [mcpserver.Events] delivers to without the https and
// address rules, for a receiver whose port is known only at run time. It
// takes the *mcpserver.Events as an any, so this package need not import
// mcpserver. The trust is never taken back.
var TrustWebhookOrigin func(events any, origin string) error
