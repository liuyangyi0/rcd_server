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

The focused regression suite is `server/fork_extensions_test.go`. When
upgrading the base version, port these named APIs and tests first, then run both
the fork's `go test ./server` and the manager's real gopcua loopback tests before
changing the replace target.

The upstream network test fixtures now use separate client/server ports
(46010/46011), wait for their own server to become ready, and close it before
exiting. They no longer silently borrow another process's listener. An external
test endpoint requires explicit `OPCUA_TEST_ENDPOINT`; the client examples still
use localhost:46010, so select `-run '^Test'` for an external endpoint elsewhere.
These changes are test-only and do not change production listeners.
