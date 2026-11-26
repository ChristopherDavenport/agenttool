# MCP Events (SEP-3415) in the MCP layer

Status: poll and webhook shipped on `main` (PRs #108 to #112), unreleased;
push is not served; wiring into dax and agentkit has not started. Written
2026-11-25, updated 2026-11-26 to match what shipped.

## Sources

- SEP-3415, Events Extension (accepted 2026-11-11):
  `modelcontextprotocol/modelcontextprotocol#3415`, `seps/3415-events-extension.md`.
- TypeScript reference: `modelcontextprotocol/typescript-sdk#3209` (all three modes).
- Python reference: `modelcontextprotocol/python-sdk#3822` (poll and webhook).
- Conformance: `modelcontextprotocol/conformance#521` (five server
  scenarios) and #541, which stacks on it; run against `c8625de3`.

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

## What shipped

| PR | What |
| --- | --- |
| #108 | Poll: `mcpserver.AddEvents`, the registry, ring buffer and cursors, poll leases, header middleware, `events/list` and `events/poll`; `mcpclient` list, poll and a poll loop. The push wire frames and a `streamSink` seam, golden-tested, unwired. |
| #109 | Webhook: `events/subscribe` and `events/unsubscribe`, TTLs and secrets, verification handshake, SSRF-guarded sender, signing, retry queue, gap bookkeeping, suspension, `deliveryStatus`; `mcpclient` subscribe, refresh and a receiver. |
| #110 | `mcpserver/examples/events-conformance`, a server for the referee, with `RESULTS.md` explaining every failure and warning; the runtime origin trust as a module-internal test hook. |
| #111 | An emit-only type may offer webhook without poll; `events/list` pages (`EventsOptions.ListPageSize`); `Events.TerminateWebhook`, `Events.GapWebhook` and `ErrWebhookNotFound`. |
| #112 | `Events.HTTPHandler`: the real HTTP 400 for a routing-header mismatch. |

Public surface, all experimental: `mcpserver.AddEvents`, `EventsOptions`
(including `Webhook` and `ListPageSize`), `Events.Register`,
`EventType.Emit`, `Events.TerminateWebhook`, `Events.GapWebhook`,
`Events.HTTPHandler`; `mcpclient.WithEvents`, `Remote.ListEvents`,
`PollEvents`, `RunEvents`, `SubscribeEvents`, `UnsubscribeEvents`,
`KeepSubscribed`, `NewWebhookSecret`, `NewWebhookReceiver` and
`EventHeaders`.

### Where it landed

- `mcpserver` holds the registry, emit path, poll, webhook and header
  checks. About 4,150 non-test lines at the first webhook merge, plus the
  later PRs; `mcpclient` about 1,580. The root module is untouched and
  stays free of the MCP SDK (`make deps`).
- A server is built as `AddEvents(srv, EventsOptions{...})`, which returns
  the handle types are registered on, not by changing `Options.NewServer`.
- Webhook subscription state is an interface (get, put, delete,
  listByName) with a map default. Cursors are not pluggable: check-type
  cursors belong to the author and emit-only types use an in-memory ring
  (`e<epoch>.<seq>`), as in the TypeScript PR.
- Webhook has no default principal: subscribe is refused (-32024) until
  the embedder supplies one (library is capability, app does auth).
- Webhook is served for emit-only types only; a type with a `Check` is
  refused at `Register`, since its author owns its cursors.
- Tests follow the TypeScript PR: golden-frame wire tests, injectable
  clocks, requirement ids `events:<area>:<rule>` for ids shared with other
  SDKs and `go:events:...` for ours, and a coverage test that every id is
  named by a test.

### Decisions worth knowing

- **The origin trust the referee needs is not API.** The referee's
  receiver picks its port at run time, so the example must trust that
  origin after the server starts. That goes through
  `mcpserver/internal/testhook`, which no code outside the module can
  import, as the TypeScript reference does with a test-only hook. An
  embedder that knows its origins sets `WebhookOptions.TrustedOrigins`.
- **`TerminateWebhook` and `GapWebhook` check no principal.** A
  subscription id is not a secret, so the docs say to call them only from
  code the embedder controls. Alternatives not taken: methods that take a
  principal and refuse a mismatch, addressing a subscription by
  (principal, url, name, arguments), a gap per event type.
- **Only finite TTLs are granted.** The SEP allows a subscription with no
  expiry; we do not grant one, so the three `ttl-no-expiry` rows stay
  warnings.

## What go-sdk v1.8.0 gave us, and what we did by hand

Checked against the module source.

| Need | v1.8.0 |
| --- | --- |
| Custom request/response methods | `AddReceivingCustomMethod`, `AddSendingCustomMethod`, `CallCustomMethod`; used. |
| Extension capability entry | `Extensions` + `AddExtension` on both capability structs; used. |
| 2026-07-28 pieces (`server/discover`, per-request client capabilities) | Served well enough. `resultType` is set only on the SDK's own results, so ours carry `resultType: "complete"` by hand. |
| Handler sees request headers | No. Receiving middleware sees `RequestExtra.Header`; the events methods are answered there. |
| `Mcp-Name` / `Mcp-Param-*` validation for custom methods | Not done by the SDK; ours, in middleware. |
| Status of a refusal from a custom method | Always HTTP 200 with the error in the body. `Events.HTTPHandler` runs the same check in front of the SDK and sends the 400. |
| Custom notifications | No. |
| Per-request client headers | No option; `mcpclient.EventHeaders` is a `RoundTripper` that adds them from the context. |

go-sdk#1031 (client-to-server custom-method ergonomics) changes none of
the above, and v1.9.0-pre.1 adds nothing for custom notifications or
handler access to the request.

## Push

Not served. The SEP's frames are custom notifications
(`notifications/events/{active,event,heartbeat,error,terminated}`, each
stamped with `_meta["io.modelcontextprotocol/subscriptionId"]`, then an
empty result) and go-sdk has no way to send one. The TypeScript reference
allows a server to leave push out (`delivery: {push: false}`), so this is
conformant. Descriptors omit `push` from `delivery`, and `events/stream`
answers the SEP's unsupported error (-32026).

### Spike (not merged)

A throwaway branch tried a transport wrapper: a `Connection` wrapper for
stdio and in-memory, and an `http.Handler` in front of the stock stateless
handler that rewrites the SSE event the SDK writes. It worked, with the
referee's push scenario going from 3 to 19 of 28 passes, at about 820 lines
including tests, but it depends on three undocumented SDK behaviours (the
`idContextKey` request routing, the SSE framing of `writeEvent`, and a
progress notification used as a carrier). Hosting our own
`StreamableServerTransport` does not work, because custom methods are
rejected while its unexported `server` field is unset. The recommendation
was not to ship it. The spike lives on a local branch and was never
pushed; its one reusable idea, the HTTP 400, is `Events.HTTPHandler`.

### Staying ready

- Push is one interface in `mcpserver`, `streamSink`; emit, cursors,
  matching and replay do not know how frames travel, so poll, webhook and
  push share the same core.
- `delivery` is data on the event definition. Turning push on is a change
  in one place plus a sink, and a server that enabled it only adds `push`
  to `delivery`; no public API changes.
- The wire frame types are defined and golden-tested.
- The client's stream receiver would have the same shape as the poll
  loop: consume frames, dedupe by `eventId`, reconnect with the cursor.

Ways in, none taken: custom notifications and handler access to the
request id in go-sdk, or our own thin wire layer for `events/stream` at the
HTTP level plus a `Connection` wrapper for stdio. Nothing has been filed
upstream.

## Conformance

Referee: conformance#541 at `c8625de3` (#521 at `76fef32d`). Both PRs were
open; rerun before relying on a row. Commands and the reason for every
failure and warning are in
`mcpserver/examples/events-conformance/RESULTS.md`.

| Scenario | Result |
| --- | --- |
| events-discovery | 15/15 |
| events-poll | 33/33, 4 warnings |
| events-webhook | 27/27, 4 warnings |
| events-webhook-delivery | 24/24, 6 warnings |
| events-push | 3/26, 23 failures: no type advertises push |

Warnings are all "unverified, not violated": controls the example does not
serve (restart, generation), features not built, and rows the suite cannot
grade.

## Not done

- Push (above) and the client's stream receiver.
- `server-identity-key-discovery`: the optional signature and Server Card
  `signingKeys`; depends on ext-server-card#55.
- Webhook: a per-principal subscription quota, delivery-time
  re-authorization, `throttled` / `retryAfterMs`, the well-known and
  allowlist verification options, and subscriptions with no expiry.
- `listChanged` is not offered; `RunEvents` does not retry.
- The events methods are still served on protocols before 2026-07-28,
  where the reference answers -32601; routing headers are checked only from
  2026-07-28.
- Client mode selection per SEP Appendix A.
- Phase 4: wire event sources into dax and agentkit as separate changes in
  those repositories.

## Open questions

- Which way to take push: upstream custom notifications, our own wire
  layer, or wait.
- Error codes -32023..-32027 are provisional in the SEP.
- SEP Open Questions that could change the wire: multi-name subscriptions
  with one cursor, events for resource and list-changed notifications.
