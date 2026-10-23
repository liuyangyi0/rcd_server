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
Local runtime modifications are in `client.go`, `client_sub.go`,
`subscription.go` and `uasc/secure_channel.go`.

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
8. A request cancelled while waiting for the channel-instance send lock does
   not reserve a wire sequence number. Encoding/signing/cancellation failures
   before any TCP write restore the unsent sequence reservation and remove only
   that request's registered response handler. The same rule applies to the
   response send path. A failed/short write or interruption after a logical
   message's first chunk closes and poisons the transport: the following request
   cannot continue an incomplete stream. This does not retry requests or weaken
   peer sequence validation; in particular it must never resend control commands.
9. Secure-channel renewal and wire admission use one cancellable send gate.
   Selecting the active token and writing a logical message are atomic with
   respect to renewal; response waits happen outside the gate. This replaces
   the condition-variable/WaitGroup gap which allowed a sender to retain an old
   token and duplicate the sequence after renewal, and raced Add against Wait.
   A queued renewal for a replaced token is skipped. Close wakes queued senders
   and closes a busy socket without waiting behind an unanswered OPN. A failed
   Open/Renew result closes the transport, because a lost reply cannot establish
   which sequence/token state is safe to continue. There is no automatic resend.
10. A decoded response with `ServiceResult=BadTimeout` is a request-level error,
    delivered to the original caller rather than the connection monitor. An
    expired Publish may request the next notification on the same Session;
    a Call still returns its timeout without resending. Transport failures,
    secure-channel failures and a Publish `StatusChangeNotification` reporting
    subscription expiry retain their existing error paths. An unsuccessful
    channel Open/Renew still closes the transport.
11. The Publish request timeout includes a margin of the larger of one keepalive
    period and the configured request timeout, capped at the UA maximum with
    overflow-safe arithmetic. For the revised 50 ms / 3000 keepalive settings
    the request budget is 300 s, not exactly the 150 s keepalive deadline.
    Sampling, publishing, subscription lifetime and serial timing are unchanged.

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

`uasc/secure_channel_send_test.go` uses a separate ephemeral loopback peer to
check pre-send and mid-encode cancellation, cancelled concurrent send waiters,
encoding-error handler cleanup, response cancellation, and an interrupted
multi-chunk stream. The peer reads actual chunk headers and requires consecutive
sequence numbers; no installed OPC service or hardware is used.
`uasc/secure_channel_renewal_test.go` adds a strict OPN peer and covers stale-token
renewals, an unanswered Publish during renewal, cancellable gate admission,
Close interrupting OPN, and an unanswered initial/renewal OPN which must not be
retried on the same transport. Duplicate-handler ownership and cancellation at
sequence wrap are covered by the send tests.

From the manager module, with explicit test endpoints and fixture control:

```sh
go test -v -count=3 -timeout=5m ./internal/opc -run '^TestLiveDeviceGroupsSessionResume'
go test -race -count=1 -timeout=3m ./...
```

The live test covers native automatic Session/subscription recovery and an
independent raw outstanding-Publish scenario against the deployed manager.
The general manager suite also exercises its separate manual lower supervisor.

The opt-in `TestTransportSequenceRealRustInterop` accepts only an isolated Rust
binary via `RCD_SEQUENCE_INTEROP_BINARY`; it creates its own configuration,
disabled nonexistent serial port, PKI and ephemeral OPC listener. Run it from
the manager module with `-race -count=3`. It checks cancelled requests followed
by successful reads and concurrent Read/readonly Call/Subscribe/actual Renew
against both None and Basic256Sha256, with automatic reconnect disabled. It
does not use a field endpoint or send hardware commands. See the dated
[2026-10-11 evidence](../../docs/validation/opc-transport-audit-fixes-20261011.md).

When changing the base version, compare against `UPSTREAM.sha256`, port the
named patches deliberately, and run both the fork and real manager regressions
before changing the `replace` target.

The opt-in `TestIdlePublishRealRustInterop` uses the same isolated real Rust
server with a constant monitored NamespaceArray and the field 50 ms / 3000
keepalive settings. Both None and Basic256Sha256 must keep the original
Session/channel for 330 s with reconnect disabled and concurrent read checks.
Before patches 10–11 both failed at 151 s. `client_timeout_test.go` additionally
forces repeated request-level `BadTimeout` ServiceFaults over the wire, proves
that Call is not retried and that real channel faults still disconnect.
`subscription_timeout_test.go` covers margin, overflow, and subscription-expiry
notification delivery. Run the opt-in test with `-race -count=1 -timeout=8m`;
short read tests cannot cover this idle-time failure.
