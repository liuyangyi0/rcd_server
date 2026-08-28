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

The focused regression suite is `server/fork_extensions_test.go`. When
upgrading the base version, port these named APIs and tests first, then run both
the fork's `go test ./server` and the manager's real gopcua loopback tests before
changing the replace target.
