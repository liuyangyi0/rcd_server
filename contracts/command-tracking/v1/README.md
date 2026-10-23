# Command tracking v1

Additive actions on `urn:SerialServer:operations`, `Operations/Execute`.
Capability: `command_tracking`. Business `ns=2` and TCP encodings do not change.

- `command_tracking_status`: gateway instance, boot identity, decimal revision string, audit health.
- `submit_command`: `schema_version:1`, `manager_instance_id`, `command_id`, physical `port_id`, legacy-format `hex_data`, `expected_gateway_instance_id`, `expected_boot_id`, `timeout_ms` (1–30000). Returns `{command,duplicate,health}`. No transport retry or fallback after an ambiguous response.
- `get_command`: manager ID + command ID; returns `{command,health}`. Missing evidence is `command:null`, not proof that the device did not execute.
- `list_command_events`: manager ID, decimal `after` cursor and `limit` (1–200); returns full snapshots, `next_cursor`, `oldest_revision`, `history_gap`, health. Snapshots include all retained attempts; intermediate memory updates can coalesce before persistence. Consumers upsert by pinned identity and monotonically increasing revision.
- `Operations/CommandRevision`: read-only String invalidation, not a result itself.

Snapshot golden fixture: `ack-received.json`. `ack_id` and cursor/revision-node values are strings, preserving 64-bit identifiers. Snapshot numeric revision must remain within safe JSON integer range for Web display; clients must not use JavaScript arithmetic to implement backend sync cursors.

Phases: Accepted, Executing, AckReceived, Failed, Expired, Unknown, SentUnconfirmed. The last phase is for a legacy direct lower Method that never waited for ACK; it must not be presented as device success. `accepted` means handed to the serial worker, not an ACK. `attempt_count` counts write attempts; `sent_count` counts completed writes, not verified physical actions. Failed partial writes can have emitted bytes. `audit_incomplete` is sticky for the command. The serial ACK has no correlation ID; this protocol cannot guarantee physical exactly-once execution.

Independent managers use independent persisted manager IDs and cursors. Duplicate IDs are checked against retained journal evidence. Old-boot submit requests are rejected. Normal retention is 7 days / 100000 completed records; this is not an unlimited idempotency archive. The manager never automatically resubmits a command, even after journal expiry.

Audit storage failures do not stop acquisition/control. Commands continue with degraded evidence; missing historical counts are unknown. No HTTP log heuristic may be used to invent a correlation.
