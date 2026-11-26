# Conformance results, and why each failure and warning is there

Run on 2026-11-26 against modelcontextprotocol/conformance #541 at
`c8625de3` (stacked on #521 at `76fef32d`), spec version 2026-07-28, using
this server. See the package comment in `main.go` for how to run it. Both
PRs were open then; the scenarios may change, so rerun before relying on
any row.

| Scenario | Pass | Fail | Warn |
| --- | --- | --- | --- |
| events-discovery | 15/15 | 0 | 0 |
| events-poll | 33/33 | 0 | 4 |
| events-push | 3/26 | 23 | 2 |
| events-webhook | 27/27 | 0 | 4 |
| events-webhook-delivery | 24/24 | 0 | 6 |

A **failure** means the referee graded a requirement and the server did
not meet it, or the referee could not test it at all. A **warning** means
the referee could neither verify nor disprove the requirement ("unverified,
not violated"). No failure here is a bug found in the server; each one
below is a known, chosen gap or a limit of the platform.

## Failures

### Push is not served (events-push, 23 failures)

Every one reads "no event type advertises push". The server does not offer
push on purpose. The SEP's `events/stream` frames are custom notifications
(`notifications/events/*`), and go-sdk v1.8.0 has no way to send custom
notifications, so the frames cannot be sent. The TypeScript reference
allows a server to leave push out (`delivery: {push: false}`), so this is
conformant, and the scenario's rows cannot run. `events/stream` answers
`-32026` (`{"feature":"deliveryMode","value":"push"}`), the SEP's error for
an unsupported mode. Three rows pass that need no open stream, among them
`removal-additive-no-terminate`; the removal rows that need an open stream
cannot run.

Revisit when go-sdk can send custom notifications; the push seam
(`streamSink` and the frame types, golden-tested) is already in place, see
`docs/plans/mcp-events.md`.

## Warnings

All are "unverified, not violated".

### Needs a control the server does not serve

- `ttl-no-expiry-persisted` and `ttl-no-expiry-gc-terminated` (webhook),
  and `verification-persisted-for-no-expiry` (delivery): the referee
  restarts the server and reads its generation, to check that a
  subscription with no expiry survives and one that is never refreshed is
  collected. This server never grants a subscription without an expiry,
  which the SEP allows, so there is nothing to persist or collect, and it
  serves neither control. Verification is soft state per process: after a
  restart the next subscribe verifies again. In the TypeScript PR the
  no-expiry rows are failures that it baselines for the same reason.
- `ttl-long-grant-retained` (webhook): a grant is at most
  `WebhookOptions.TTL`, an hour by default, held in the store the embedder
  chose. A long grant over the default map store would not survive a
  restart, and the referee checks that by restarting.

The code's own list of these, with its reasons, is `eventsWebhookBaselined`
in `events_test.go`; a test fails if a test claims one of them.

### Not implemented

- `server-identity-key-discovery` (delivery): the optional ed25519
  signature and the Server Card `signingKeys` that let a client verify a
  server's webhook identity. Not built; it depends on ext-server-card#55.
  Also in `eventsWebhookBaselined`.

### The referee cannot grade it

- `delivery-dual-sign-on-rotation`: the suite has no way to rotate a
  secret mid-test. Rotation is covered by this repository's tests.
- `delivery-status-last-error-category` and `subscribe-refresh-reactivates`:
  they depend on the optional `deliveryStatus` field, which the suite does
  not force a failure to produce.
- `verification-uses-ssrf-hardened-path` and
  `verification-cached-per-principal-url`: the suite grades the SSRF rules
  first, then allows its own receiver's origin through
  `events_conformance_allow_callback_origin`, so it has no later view of
  which path the verification took or what is cached. Both are covered by
  this repository's tests.
- `next-poll-ms-ignored-when-has-more`, `truncated-returns-fresh-cursor`,
  `max-age-ms-sets-truncated` and `replay-ceiling-sets-truncated` (poll):
  the suite cannot produce a stale or aged cursor against a live server in
  one run. The ring buffer's age and count limits are covered by this
  repository's tests.

Where a warning says "covered by this repository's tests" it means the
behaviour has a test in `mcpserver` under its requirement id; the id is
checked by the coverage test, so it cannot be dropped silently.

## Not run

Server restart and generation, and push beyond the `-32026` answer, were
not exercised. A wrapper to turn the
`-32020` status into 400 was not tried.
