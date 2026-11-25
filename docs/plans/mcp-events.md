# MCP Events (SEP-3415) in the MCP layer

Status: plan, nothing implemented. Written 2026-11-25.

## Sources

- SEP-3415, Events Extension (accepted 2026-11-11):
  `modelcontextprotocol/modelcontextprotocol#3415`, `seps/3415-events-extension.md`.
- TypeScript reference: `modelcontextprotocol/typescript-sdk#3209` (all three modes).
- Python reference: `modelcontextprotocol/python-sdk#3822` (poll and webhook).
- Conformance: `modelcontextprotocol/conformance#521` (five scenarios) and #541.

## What the extension is

Extension `io.modelcontextprotocol/events`, declared under
`capabilities.extensions`. Methods: `events/list`, `events/poll`,
`events/stream` (long-lived), `events/subscribe`, `events/unsubscribe`.
Three delivery modes, none mandatory: poll, push, webhook (Standard
Webhooks signing, TTL refresh). Opaque cursors resume after a gap; errors
-32023..-32027 (provisional) plus -32020 header mismatch.

The SEP is written against protocol 2026-07-28: `resultType`,
`server/discover`, `Mcp-Method` / `Mcp-Name` / `Mcp-Param-*` headers,
`subscriptions/listen` for list-changed only.

## What go-sdk v1.8.0 gives us

Checked against the module source (the fit study is in the session
scratchpad; the two claims that block push were re-read in the source).

| Need | v1.8.0 |
| --- | --- |
| Custom request/response methods | `AddReceivingCustomMethod`, `AddSendingCustomMethod`, `CallCustomMethod` |
| Extension capability entry | `Extensions` + `AddExtension` on both capability structs |
| Handler sees request headers | No. Handler gets `(ctx, *ServerSession, params)`. Receiving middleware sees `RequestExtra.Header`, so middleware can put them in ctx. |
| Custom notifications | No. |
| Streaming responses for a custom method | No. Workaround: park the handler, send frames with `NotifyProgress` or `Log` on the session; routed to that request's SSE stream. |
| Per-request client headers | No option; a `RoundTripper` on `StreamableClientTransport.HTTPClient` can add them from ctx. |

Open upstream: go-sdk#1031 (client-to-server custom-method ergonomics only,
changes none of the above). v1.9.0-pre.1 adds nothing for custom
notifications or handler access to `Extra`.

## Verdict per mode

- **Poll** (list, poll, subscribe, unsubscribe): doable on stock v1.8.0.
- **Webhook**: doable; delivery is plain `net/http`, independent of the SDK.
  Needs an authenticated principal, https-only URLs, a verification
  handshake, an SSRF-hardened dialer pinned to the validated IP, no
  redirects, HMAC v1 signing, a retry queue and a TTL store.
- **Push** (`events/stream`): not doable on stock v1.8.0. The SEP's
  frames are custom notifications (`notifications/events/{active,event,
  heartbeat,error,terminated}`, each stamped with
  `_meta["io.modelcontextprotocol/subscriptionId"]`, then an empty
  result), and go-sdk has no custom notifications. Progress/log frames
  are not the SEP wire format, so we do not use them. A parked handler
  can also deadlock session close (go-sdk#1160).

## Decision: push is off in the first release

The TypeScript reference allows this (`delivery: {push: false}`), so a
server without push is conformant. We ship poll and webhook, declare
push unsupported in every descriptor's `delivery` list, and answer
`events/stream` with the SEP's unsupported error. The client never
selects push when a descriptor omits it.

### Staying ready for go-sdk support

- Push is one interface in `mcpserver`, `streamSink` (send active,
  event, heartbeat, error, terminated; end with the result). Emit,
  cursors, subscription matching and replay do not know how frames
  travel; poll and webhook use the same core.
- The registry, emit path and cursor ring are written for a long-lived
  consumer already (a subscriber with a cursor that receives events as
  they are emitted). Poll drains it on request.
- `delivery` is data on the event definition, not a constant. Turning
  push on is a change in one place plus a sink implementation.
- Wire frame types are defined now, with golden-frame tests, so the
  sink can be added without a format decision.
- Two ways in later, behind the sink: go-sdk custom notifications (watch
  for an upstream issue; file one with this plan as the use case) or a
  `Transport` wrapper. Either is additive. No public API changes: a
  server that enabled push later only adds `push` to `delivery`.
- Client side: the stream receiver is the same shape (consume frames,
  dedupe by `eventId`, reconnect with the cursor), so it is added
  without touching the poll or webhook paths.

## Placement

- `mcpserver`: an events registry (type descriptors, `emit`, cursor store,
  subscription store) and the method handlers; `Options` gains a
  capabilities pass-through (`NewServer` builds `ServerOptions` only when
  `Instructions` or `Logger` is set).
- `mcpclient`: `events/*` calls via `WithClientSetup`, a poll loop with
  cursor persistence and `eventId` dedup, stream and webhook receivers,
  and a mode chooser per Appendix A.
- Root module: only types that tools or agent loops share (event
  descriptor, occurrence) and only if a consumer needs them; the root
  must stay free of the MCP SDK (`make deps`).
- Webhook subscription state is an interface (get, put, delete,
  listByName) with a map default, as in the TypeScript PR. Cursors are
  not pluggable there: check-type cursors belong to the author and
  emit-only types use an in-memory ring (`e<epoch>.<seq>`); we do the
  same.
- Header checks (`Mcp-Name`, `Mcp-Param-*`, 400 with -32020) are HTTP
  middleware, since handlers see no headers; it reads the registry for
  each type's `x-mcp-header` annotations.
- No default for the webhook principal: webhook stays refused until the
  embedder supplies one (library is capability, app does auth).

## Phases

1. Capability, `events/list`, `events/poll`, server emit, client poll
   loop, header middleware. Passes `events-discovery` and `events-poll`.
2. Webhook: subscribe / unsubscribe, delivery, security. Passes
   `events-webhook` and `events-webhook-delivery`. Expect the
   TypeScript baselines (ttl-no-expiry x2, list-pagination) unless we
   implement them.
3. Push, when go-sdk can carry the frames (see above). Passes
   `events-push`; until then that scenario is baselined as unsupported.
4. Wire into dax and agentkit (event sources into the agent loop) as
   separate changes in those repositories.

Tests follow the TypeScript PR: golden-frame wire tests, injectable
clocks, and requirement ids `events:<area>:<rule>` for ids shared with
other SDKs and `go:events:...` for ours, with a coverage check.

Run the conformance scenarios locally from a build of conformance#541;
no released referee carries them yet.

## Open questions

- Is 2026-07-28 (`server/discover`, `resultType`, `Mcp-*` headers) fully
  served by v1.8.0, or do we add pieces by hand? Verify before phase 1.
- Error codes -32023..-32027 are provisional in the SEP.
- SEP Open Questions that could change the wire: multi-name subscriptions
  with one cursor, events for resource and list-changed notifications.
