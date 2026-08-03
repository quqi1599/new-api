# Caddy streaming hop policy

This policy applies to every controlled Caddy hop that forwards streaming API
requests. It complements New API request cancellation and timeout handling; a
proxy hop cannot replace the application fix, and an application fix cannot
propagate through a proxy that deliberately detaches the upstream request.

## Required invariants

1. Do not configure a negative `flush_interval` on streaming routes. Caddy
   already flushes `Content-Type: text/event-stream` responses immediately. A
   negative value keeps the upstream request alive after the downstream client
   disconnects, which breaks end-to-end cancellation.
2. Do not retry model `POST` requests at the Caddy layer. A missing response
   does not prove that the provider rejected the request, so replay can create
   duplicate generations, tool calls, tasks, or charges.
3. Keep timeouts ordered by responsibility:
   - New API owns provider dial, TLS, response-header, and stream-idle limits.
   - A transit Caddy timeout must be later than the corresponding New API
     timeout.
   - An ingress Caddy timeout must be later than the transit timeout.
4. Do not use `request_buffers` as the primary fix and do not enable it
   fleet-wide. Caddy reads only up to the configured size before contacting the
   upstream, and its documentation warns that this mode is very inefficient.
   First deploy the New API integrity and cancellation changes. If a real
   canary still reproduces upload truncation, test buffering on one first-ingress
   configuration class only, with a size that covers the admitted body limit,
   explicit concurrency admission, a resource budget, and cleanup monitoring.
5. Rotate access and service logs by size and age. Logs must not contain API
   keys, authorization headers, cookies, or complete request bodies.

## Layer responsibilities

### Regional ingress

- Terminate the public TLS connection and create or validate the platform trace
  identifier. New API accepts a validated `X-Oneapi-Request-Id`,
  `X-Request-Id`, `X-Trace-Id`, or `X-B3-TraceId`, returns the selected value as
  `X-Oneapi-Request-Id`, and keeps a separately generated internal ID. The edge
  should overwrite untrusted identifiers when customer-controlled collisions
  are not acceptable.
- Forward client cancellation to the selected transit hop.
- Do not replay model requests across transit nodes.
- Use a timeout longer than the transit and New API timeout budget.

### Transit relay

- Forward the same trace identifier.
- Omit negative `flush_interval` settings.
- Forward downstream cancellation to the stable origin entry.
- Keep streaming responses unbuffered through Caddy's native SSE behavior.

### Stable origin entry

- Be the only address used by relays; relays must not depend on a container's
  directly recreated port.
- Route new requests to the active version while existing requests drain on the
  previous version.
- Expose readiness separately from liveness and remove a backend from new
  traffic before shutdown.
- Never kill a draining backend merely because a fixed timer elapsed. Alert,
  keep waiting, or roll back the release unless forced termination is an
  explicit product decision.

### New API

- Bind every synchronous outbound request to the inbound request context.
- Stop retries when the client context is canceled.
- Separate dial, TLS handshake, response-header, first-event, and stream-idle
  timeouts.
- Enforce one request-wide first-valid-event deadline across all channel
  attempts. Disarm its response-header timer after headers arrive and its
  first-event timer after a valid protocol frame, so it never becomes a
  whole-stream lifetime limit.
- Reject an incomplete request body before provider dispatch or billing.

## Validation before a fleet rollout

The repository script is a preflight lint, not proof of correct runtime
behavior. Run it on the target node: when Caddy is available it adapts and
validates the full configuration so imported snippets and environment-expanded
values are included. A passing result still requires the cancellation and
stream tests below.

For every distinct Caddy configuration:

1. Run `caddy adapt` and `caddy validate` against the candidate file.
2. Run `scripts/check_caddy_streaming_policy.sh` against the candidate file.
3. Start a streaming request through that exact hop, disconnect the downstream
   client, and verify the next controlled hop observes cancellation within two
   seconds.
4. Keep a stream active across a Caddy reload and a New API version switch.
5. Upload a throttled request body through the hop and abort it at multiple
   sizes; verify that no provider request or charge is created.
6. Verify Chat Completions and Responses streams through the real public Host
   and TLS SNI, not only through an internal health endpoint.

Roll out one configuration class and one node at a time. Stop if cancellation,
body integrity, error rate, or stream completion metrics regress.
