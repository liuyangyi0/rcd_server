# RcdManager awcullen/opcua fork

This directory is a source copy of `github.com/awcullen/opcua` version
`v1.4.0`. The upstream MIT license is retained in `LICENSE`. The manager module
selects this copy with a Go module `replace` directive so production builds do
not silently move to a different server implementation.

## Local server patches

- `EndpointProfile` / `WithEndpointProfiles` advertise and accept only the
  configured security-policy, message-mode, and identity-token combinations.
  Unlisted secure-channel policy/mode pairs are rejected.
- `WithListenAddress` keeps the local TCP bind address independent from the
  advertised OPC UA EndpointURL.
- `SessionEvent`, `SessionSnapshot`, and `WithSessionEventHandler` expose
  Created, Activated, Attached, Detached, and Closed events. Close events carry
  client, timeout, or server-shutdown reasons. Snapshots include the peer remote
  address and do not expose mutable `Session` objects.
- Lifecycle snapshots and channel binding changes are committed under the same
  Session lock and delivered through a per-Session FIFO. A delayed old-channel
  detach cannot describe a new binding or arrive after its Attached event.
  Repeated detach callbacks are suppressed. Activated/Attached processing must
  complete before an ActivateSession success response is written, so a client's
  immediate Read sees initialized Session-local routes. Callbacks run without
  Session/Manager locks; Snapshot, Detach, and Delete callback reentry is safe.
  Reentrant events are appended for the existing drainer, with no background
  goroutine. Closed is ordered with other lifecycle events. The deterministic
  and concurrent checks are in `server/session_lifecycle_order_test.go`.
- Session identity reads use `Session.UserIdentity()` under the session lock.
- `WithSessionScopedWriteHandlers` lets a successful custom write handler
  perform session-local routing without replacing a variable's global backing
  value.
- `DataChangeMonitoredItem.ForceSample` and
  `Server.ForceResampleSessionNodes` enqueue a fresh sample only for matching
  `(session, node)` monitored items after a route switch.
- `Server.Sessions` returns immutable lifecycle snapshots. Endpoint caching is
  synchronized and returns defensive slice copies.
- Username-token RSA ciphertext blocks use the modulus size, not the private
  exponent length. A valid key may have a shorter exponent; the old calculation
  rejected such keys during otherwise valid username connections. Endpoint,
  identity and authorization policies are unchanged. The deterministic size
  regression is `server/rsa_username_test.go`.
- Publish queues reject closed, expired, or superseded SecureChannels. Session
  reactivation filters old requests under the same lock used for admission,
  preserving requests on the new channel and keeping the original Session and
  subscriptions alive. A failed timeout/overflow response on an old channel
  does not prevent a queued request on the replacement channel from proceeding.
  Transport close and failed/partial writes invalidate queued requests promptly.
  A consumed valid Publish resets the subscription lifetime even if its response
  cannot be written, avoiding premature expiry during recovery.
- Scheduled and late Publish paths commit each notification's sequence number
  when inserting it into the retransmission queue, before writing the response.
  A failed transport write leaves the original notification available for
  Republish, but its sequence number is never reused for a new value. Sequence
  rollover skips zero; keepalives neither consume sequence numbers nor replace
  acknowledgement errors with Good. Session locks are never held across network
  writes. Deterministic transport-failure and concurrent reactivation regressions
  live in `server/publish_reconnect_test.go`.

The focused regression suites are `server/fork_extensions_test.go` and
`server/publish_reconnect_test.go`. When
upgrading the base version, port these named APIs and tests first, then run both
the fork's `go test ./server` and the manager's real gopcua loopback tests before
changing the replace target.

The upstream network test fixtures now use separate client/server ports
(46010/46011), wait for their own server to become ready, and close it before
exiting. They no longer silently borrow another process's listener. An external
test endpoint requires explicit `OPCUA_TEST_ENDPOINT`; the client examples still
use localhost:46010, so select `-run '^Test'` for an external endpoint elsewhere.
These changes are test-only and do not change production listeners.
