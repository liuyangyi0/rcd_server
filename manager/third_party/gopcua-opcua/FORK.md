# RcdManager gopcua/opcua client fork

This is the client dependency closure of `github.com/gopcua/opcua v0.9.1`,
selected by a local `replace` in the manager module. The requested version stays
`v0.9.1`; the upstream MIT license is retained in `LICENSE`. This is not an
upgrade of the OPC client, and does not modify endpoint, certificate, identity,
or authentication policies. RcdManager's lower supervisors continue to use
`AutoReconnect(false)` and manage their own connection epochs.

Upstream module checksum:
`h1:Qp40I5JmiiKXYIWmk7xECYNrXs5unohH24jKWnSRyIE=`.
The imported files were checked against the downloaded v0.9.1 module ZIP;
`UPSTREAM.sha256` records the original bytes of every retained upstream file.
Only `client.go` and `client_sub.go` have local runtime modifications.

## Scope of the source copy

The root client package and `debug`, `errors`, `id`, `monitor`, `stats`, `ua`,
`uacp`, `uapolicy`, and `uasc` are included, with their self-contained tests.
The large generated `id` tables are required runtime source, not generated
build output. Upstream `go.mod` and `go.sum` are unchanged.

The following are intentionally not imported:

- `server`, `schema`, generators, examples, command-line tools, Python/Go
  external-server integration harnesses, artwork, PKI, and build output;
- `config_test.go`, `config_pkcs8_test.go`, and `uapolicy/cert_utils_test.go`,
  which embed or require fixed certificate/private-key fixtures;
- `monitor/subscription_test.go`, which requires the omitted upstream server;
- `uasc/secure_channel_test.go` and its dependent chunk/thumbprint tests, which
  require the omitted external test harness;
- `uacp/conn_test.go`, which binds a hard-coded OPC port 4840, and
  `uacp/endpoint_test.go`, which depends on public DNS.

No static private keys or certificates are included. The new transport test
owns an ephemeral loopback listener; it never borrows an existing OPC service.

## Local recovery patches

1. A successful reactivation of the original Session explicitly snapshots its
   existing subscription IDs. Previously this path skipped TransferSubscriptions
   but also left the restore list empty, so it reported Connected without
   restarting Publish. Existing Session and Subscription IDs are retained.
2. Publish pause/resume has one atomic state and a coalesced wake-up. Duplicate
   pause events cannot be consumed after a later resume, and a late Publish
   error cannot pause a newer recovery generation. Subscriptions are registered
   before their first Publish wake-up.
3. Replay waits for any old Publish to finish before changing sequence and ACK
   state. Republish starts at the existing expected sequence, forwards each
   notification once, and adds its acknowledgement to the next Publish request.
   The reserved sequence zero is skipped on UInt32 wraparound. There is no
   artificial one-second delay per replayed notification.
4. `BadMessageNotAvailable` terminates replay normally. Session/channel failures
   request another recovery pass instead of pretending that the subscription
   recovered. Failed replay is not counted twice if subscription recreation is
   needed. Malformed successful replay responses cannot advance the cursor or
   cause an infinite replay loop. The upstream failed-recreate retry protection
   remains intact.
5. A lost ActivateSession response does not discard the original Session.
   Only `BadSessionIdInvalid`/`BadSessionClosed` prove that another Session is
   needed. A newly created replacement is also retained if its activation
   response is lost. Transfer state survives another channel failure; unknown
   or malformed responses never imply that subscriptions should be deleted.
   Retried transfers returning `BadNothingToDo` (already owned by the current
   Session) replay from the existing cursor. See the operation result table in
   [OPC UA Part 4, TransferSubscriptions](https://reference.opcfoundation.org/specs/OPC-10000-4/5.14.7).
6. `BadMessageNotAvailable` is a normal request-level Republish terminator, not
   a channel failure. The connection monitor ignores it, including outside an
   active recovery pass, so a following Read/Publish keeps its Session/channel.
7. Dial/Close publish and extract transports under a short mutex, with an
   epoch/cancel token preventing late Dial success after Close. All connection
   and secure-channel I/O remains outside that mutex. The request timeout also
   bounds the UACP HEL/ACK phase, which otherwise only observes deadlines.

No empty subscription, manual re-subscription, or client-side timer is used to
wake Publish as a workaround.

## Regression checks (run on the remote test host)

From this module:

```sh
go test -race -count=3 -timeout=2m ./...
```

`client_resume_test.go` contains the deterministic protocol test
`TestReconnectOriginalSessionResumesOriginalSubscription` and focused replay,
pause-generation, failure, malformed-response, ACK, and wraparound regressions.
The protocol peer closes TCP after accepting a Publish request, retains the
original Session and Subscription, replays the unreceived notification, then
requires a real Publish request on the new channel before returning a new
value. It checks that no CreateSubscription or TransferSubscriptions workaround
was used.

`TestReconnectRetainsIdentityAcrossSecondDisconnect` also cuts TCP after the
next ActivateSession is accepted, and before/after a replacement Session's
TransferSubscriptions is applied. It checks Session tokens, unchanged
Subscription ID, replay/ACK/Publish sequence, and zero DeleteSubscriptions.
`TestRepublishMissDoesNotReconnect` sends an actual ServiceFault, then verifies
that the connection and subsequent Read survive. The gated
`TestCloseWhileDialIsOpeningHasNoLateTransport` overlaps Close with an unfinished
OpenSecureChannel and verifies cancellation, no blocking transport lock, and no
late connection publication. The peer uses fully initialized UA response
headers and endpoint descriptions; it does not change the UA encoder.

From the manager module, with explicit test endpoints and fixture control:

```sh
go test -v -count=3 -timeout=5m ./internal/opc -run '^TestLiveDeviceGroupsSessionResume'
go test -race -count=1 -timeout=3m ./...
```

The live test covers native automatic Session/subscription recovery and an
independent raw outstanding-Publish scenario against the deployed manager.
The general manager suite also exercises its separate manual lower supervisor.

When changing the base version, compare against `UPSTREAM.sha256`, port the
named patches deliberately, and run both the fork and real manager regressions
before changing the `replace` target.
