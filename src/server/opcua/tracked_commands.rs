//! Additive operations adapter. Both protocols share the exact serial admission
//! and ACK path; an ambiguous RPC is never retried through the legacy path.
use super::*;
use crate::operations::OperationError;
use serde_json::{json, Value as JsonValue};

pub(super) fn register_tracked_command_submit(
    operations: &Operations,
    senders: &HashMap<String, mpsc::Sender<Command>>,
    mut context: SendNodeWriteContext,
    ids: Arc<AtomicU64>,
) {
    let tracker = operations.command_tracker();
    let senders = senders.clone();
    // Avoid Operations -> callback -> Operations ownership cycles.
    let weak = operations.downgrade();
    context.operations = None;
    operations.register_command_submit(Arc::new(move |request: &JsonValue| {
        // The relative wire budget starts on receipt, not after journal lookup,
        // durable acceptance or the admission mutex. Those bounded waits are
        // still part of this request and must never renew a queued command.
        let received_at = Instant::now();
        let timeout = request["timeout_ms"]
            .as_u64()
            .unwrap_or(5000)
            .clamp(1, 30000);
        let deadline = received_at
            + context
                .command_pending_timeout
                .min(Duration::from_millis(timeout));
        if request["schema_version"].as_u64() != Some(1) {
            return Err(OperationError::new(
                "invalid_request",
                "unsupported command schema_version",
            ));
        }
        let field = |name: &str| {
            request[name]
                .as_str()
                .filter(|v| !v.is_empty())
                .ok_or_else(|| OperationError::new("invalid_request", format!("missing {name}")))
        };
        tracker
            .validate_identity(
                field("expected_gateway_instance_id")?,
                field("expected_boot_id")?,
            )
            .map_err(|e| OperationError::new("conflict", e))?;
        let port = field("port_id")?;
        let sender = senders
            .get(port)
            .ok_or_else(|| OperationError::new("not_found", "unknown physical port"))?;
        let payload = field("hex_data")?;
        // Validate before reserving a journal record; identical legacy encoding.
        if is_send_node_idle_or_status(payload)
            || build_hex_command(payload, port, true, None).is_err()
        {
            return Err(OperationError::new(
                "invalid_request",
                "invalid command hex data",
            ));
        }
        let manager = field("manager_instance_id")?;
        let command = field("command_id")?;
        let ack = ids.fetch_add(1, Ordering::Relaxed);
        let (value, new) = tracker
            .begin(manager, command, port, ack, payload)
            .map_err(|e| OperationError::new("conflict", e))?;
        if !new {
            return Ok(json!({"command":value,"duplicate":true,"health":tracker.identity()}));
        }
        let mut context = context.clone();
        context.operations = weak.upgrade();
        if context.operations.is_none() {
            return Err(OperationError::new("unavailable", "server shutting down"));
        }
        let status = handle_send_node_write_tracked_deadline(
            DataValue {
                value: Some(Variant::String(payload.into())),
                ..Default::default()
            },
            port,
            sender,
            &context,
            ack,
            true,
            Some(deadline),
        );
        tracker.update(port, Some(ack), |v| {
            v.status_code = status.bits();
            if status != StatusCode::Good {
                v.accepted = false;
                if status == StatusCode::BadTimeout {
                    v.phase = "Expired".into();
                    v.error = "deadline_expired_before_admission".into();
                } else {
                    v.phase = "Failed".into();
                    v.error = format!("{status:?}");
                }
            }
        });
        let value = tracker
            .get(manager, command)
            .map_err(|e| OperationError::new("audit_unavailable", e))?;
        Ok(json!({"command":value,"duplicate":false,"health":tracker.identity()}))
    }));
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn tracked_receipt_deadline_includes_journal_and_pending_lock_waits() {
        for wait_at in ["journal", "pending_lock"] {
            let dir = tempfile::tempdir().unwrap();
            let port = "TEST-NOT-A-SERIAL-DEVICE";
            let ports = vec![SerialPortConfig::new(port.into(), 9600, false, vec![])];
            let ops = Operations::new(dir.path().into(), dir.path().into(), "test".into(), &ports)
                .unwrap();
            ops.report_runtime(port, true, false, []);
            let tracker = ops.command_tracker();
            let (tx, rx) = mpsc::channel();
            let (ack_tx, ack_rx) = mpsc::channel();
            let context = SendNodeWriteContext {
                system_record: Arc::new(Mutex::new(BoundedVecDeque::new(10))),
                pending_commands: Arc::new(Mutex::new(HashMap::new())),
                local_ack_status_tx: ack_tx,
                command_pending_timeout: Duration::from_secs(5),
                operations: Some(ops.clone()),
            };
            register_tracked_command_submit(
                &ops,
                &HashMap::from([(port.into(), tx)]),
                context.clone(),
                Arc::new(AtomicU64::new(1)),
            );
            let identity = tracker.identity();
            let mut request = json!({"action":"submit_command","schema_version":1,
                "manager_instance_id":"manager-a","command_id":"expires-during-admission",
                "port_id":port,"hex_data":"11 08",
                "expected_gateway_instance_id":identity["gateway_instance_id"],
                "expected_boot_id":identity["boot_id"],"timeout_ms":5});
            let response = if wait_at == "journal" {
                // Hold only the writer, not the WAL reader. The acceptance
                // record cannot be durably confirmed within the 5 ms request
                // budget; SQLite's configured writer wait is 50 ms.
                let db =
                    rusqlite::Connection::open(dir.path().join("command-journal.sqlite")).unwrap();
                db.execute_batch("BEGIN IMMEDIATE").unwrap();
                let response = ops.execute(&request).unwrap();
                db.execute_batch("ROLLBACK").unwrap();
                response
            } else {
                // A deterministic admission barrier: the command record exists
                // before releasing a pending mutex held beyond its deadline.
                let held = context.pending_commands.lock().unwrap();
                let submitted_ops = ops.clone();
                let submitted_request = request.clone();
                let submitting =
                    std::thread::spawn(move || submitted_ops.execute(&submitted_request).unwrap());
                let started = Instant::now();
                while tracker
                    .get("manager-a", "expires-during-admission")
                    .unwrap()
                    .is_none()
                {
                    assert!(started.elapsed() < Duration::from_secs(2));
                    std::thread::sleep(Duration::from_millis(1));
                }
                std::thread::sleep(Duration::from_millis(10));
                drop(held);
                submitting.join().unwrap()
            };
            assert_eq!(response["command"]["phase"], "Expired", "{wait_at}");
            assert_eq!(response["command"]["accepted"], false, "{wait_at}");
            assert_eq!(
                response["command"]["status_code"],
                StatusCode::BadTimeout.bits(),
                "{wait_at}"
            );
            assert_eq!(response["command"]["attempt_count"], 0, "{wait_at}");
            assert_eq!(response["command"]["sent_count"], 0, "{wait_at}");
            assert!(
                rx.try_recv().is_err(),
                "expired command entered serial queue"
            );
            assert!(
                ack_rx.try_recv().is_err(),
                "unaccepted command published waiting"
            );
            assert!(context.pending_commands.lock().unwrap().is_empty());
            assert!(ops.admit_command(port).is_ok());
            // A retry with a larger budget returns the existing terminal
            // record; it cannot turn this identity into a new submission.
            request["timeout_ms"] = json!(30000);
            let duplicate = ops.execute(&request).unwrap();
            assert_eq!(duplicate["duplicate"], true);
            assert_eq!(duplicate["command"], response["command"]);
            assert!(rx.try_recv().is_err());
        }
    }

    #[test]
    fn maintenance_rejects_legacy_method_and_tracked_commands_without_enqueue() {
        let dir = tempfile::tempdir().unwrap();
        let port = "TEST-NOT-A-SERIAL-DEVICE";
        let ports = vec![SerialPortConfig::new(port.into(), 9600, false, vec![])];
        let ops =
            Operations::new(dir.path().into(), dir.path().into(), "test".into(), &ports).unwrap();
        ops.report_runtime(port, true, false, []);
        let (tx, rx) = mpsc::channel();
        let (ack_tx, _) = mpsc::channel();
        let context = SendNodeWriteContext {
            system_record: Arc::new(Mutex::new(BoundedVecDeque::new(10))),
            pending_commands: Arc::new(Mutex::new(HashMap::new())),
            local_ack_status_tx: ack_tx,
            command_pending_timeout: Duration::from_secs(5),
            operations: Some(ops.clone()),
        };
        register_tracked_command_submit(
            &ops,
            &HashMap::from([(port.into(), tx.clone())]),
            context.clone(),
            Arc::new(AtomicU64::new(1)),
        );
        let identity = ops.command_tracker().identity();
        ops.execute(&json!({"schema_version":1,"action":"enter_maintenance","deployment_id":"test-job","fencing_token":"1",
            "expected_gateway_instance_id":identity["gateway_instance_id"],"expected_boot_id":identity["boot_id"]})).unwrap();
        assert_eq!(
            handle_send_node_write(
                DataValue::new_now(Variant::String("11 08".into())),
                port,
                &tx,
                &context,
                999
            ),
            StatusCode::BadWaitingForResponse
        );
        assert_eq!(
            forward_hex_command("11 08", port, &tx, false, &ops)
                .unwrap_err()
                .code,
            "busy"
        );
        let request = json!({"action":"submit_command","schema_version":1,"manager_instance_id":"manager-a","command_id":"a",
            "port_id":port,"hex_data":"11 08","expected_gateway_instance_id":identity["gateway_instance_id"],"expected_boot_id":identity["boot_id"],"timeout_ms":5000});
        let rejected = ops.execute(&request).unwrap();
        assert_eq!(rejected["command"]["accepted"], false);
        assert_eq!(
            rejected["command"]["status_code"],
            StatusCode::BadWaitingForResponse.bits()
        );
        assert_eq!(ops.execute(&request).unwrap()["duplicate"], true);
        assert!(rx.try_recv().is_err());
    }

    #[test]
    fn tracked_and_legacy_share_admission_and_duplicates_never_enqueue() {
        let dir = tempfile::tempdir().unwrap();
        let port = "TEST-NOT-A-SERIAL-DEVICE";
        let ports = vec![SerialPortConfig::new(port.into(), 9600, false, vec![])];
        let ops =
            Operations::new(dir.path().into(), dir.path().into(), "test".into(), &ports).unwrap();
        ops.report_runtime(port, true, false, []);
        let (tx, rx) = mpsc::channel();
        let (ack_tx, _) = mpsc::channel();
        let context = SendNodeWriteContext {
            system_record: Arc::new(Mutex::new(BoundedVecDeque::new(10))),
            pending_commands: Arc::new(Mutex::new(HashMap::new())),
            local_ack_status_tx: ack_tx,
            command_pending_timeout: Duration::from_secs(5),
            operations: Some(ops.clone()),
        };
        let senders = HashMap::from([(port.into(), tx.clone())]);
        register_tracked_command_submit(
            &ops,
            &senders,
            context.clone(),
            Arc::new(AtomicU64::new(1)),
        );
        let identity = ops.command_tracker().identity();
        let mut request = json!({"action":"submit_command","schema_version":1,"manager_instance_id":"manager-a","command_id":"a","port_id":port,"hex_data":"11 08","expected_gateway_instance_id":identity["gateway_instance_id"],"expected_boot_id":identity["boot_id"],"timeout_ms":5000});
        let first = ops.execute(&request).unwrap();
        assert_eq!(first["command"]["accepted"], true);
        let queued = rx.try_recv().unwrap();
        let duplicate = ops.execute(&request).unwrap();
        assert_eq!(duplicate["duplicate"], true);
        assert!(rx.try_recv().is_err());
        request["hex_data"] = json!("12 08");
        assert!(ops.execute(&request).is_err());
        request["hex_data"] = json!("11 08");
        request["manager_instance_id"] = json!("manager-b");
        assert_eq!(ops.execute(&request).unwrap()["command"]["accepted"], false);
        assert_eq!(
            handle_send_node_write(
                DataValue::new_now(Variant::String("11 08".into())),
                port,
                &tx,
                &context,
                999
            ),
            StatusCode::BadWaitingForResponse
        );
        assert!(rx.try_recv().is_err());
        drop(queued);
        // The worker has completed but the 50 ms ACK drainer has not run yet.
        // A different command must be admitted without waiting for that timer,
        // while the stale ACK must not clear the new reservation.
        ops.command_tracker().update(port, Some(1), |v| {
            v.phase = "AckReceived".into();
        });
        request["manager_instance_id"] = json!("manager-a");
        request["command_id"] = json!("next");
        let next = ops.execute(&request).unwrap();
        assert_eq!(next["command"]["accepted"], true);
        let next_command = rx.try_recv().unwrap();
        assert!(!apply_ack_pending_state(
            &mut context.pending_commands.lock().unwrap(),
            &CommandAckUpdate {
                com: port.into(),
                ack_id: Some(1),
                status: CommandAckStatus::Received
            },
        ));
        // Even terminal tracking evidence must not bypass a still-held worker
        // permit. This also covers a second manager racing with completion.
        ops.command_tracker()
            .update(port, next_command.ack_id, |v| {
                v.phase = "Failed".into();
                v.error = "ack_timeout".into();
            });
        request["command_id"] = json!("while-worker-finishes");
        assert_eq!(ops.execute(&request).unwrap()["command"]["accepted"], false);
        assert!(rx.try_recv().is_err());
        drop(next_command);
        request["command_id"] = json!("after-timeout");
        assert_eq!(ops.execute(&request).unwrap()["command"]["accepted"], true);
        drop(rx.try_recv().unwrap());
        request["expected_boot_id"] = json!("stale-boot");
        assert!(ops.execute(&request).is_err());
    }
}
