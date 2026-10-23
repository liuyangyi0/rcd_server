//! Control transaction state machine; only the owning serial worker calls it.
use super::manager::{QueuedCommand, SerialManager};
use crate::model::{CommandAckStatus, CommandAckUpdate, SendData};
use log::{debug, info, warn};
use std::sync::mpsc::Sender;
use std::time::Instant;

struct ExecutionGuard {
    tracker: Option<crate::command_tracking::Tracker>,
    port: String,
    ack: Option<u64>,
}
impl Drop for ExecutionGuard {
    fn drop(&mut self) {
        if let Some(tracker) = &self.tracker {
            tracker.update(&self.port, self.ack, |v| {
                if !v.terminal() {
                    v.phase = "Unknown".into();
                    v.error = "serial transaction interrupted; device outcome unknown".into();
                    v.audit_incomplete = true;
                }
            });
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn interrupted_worker_records_unknown_without_replay() {
        let root = tempfile::tempdir().unwrap();
        let tracker = crate::command_tracking::Tracker::open(root.path().join("journal.sqlite"));
        tracker.begin("a", "c", "TEST", 1, "1108").unwrap();
        let result = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
            let _guard = ExecutionGuard {
                tracker: Some(tracker.clone()),
                port: "TEST".into(),
                ack: Some(1),
            };
            panic!("isolated worker interruption");
        }));
        assert!(result.is_err());
        let value = tracker.get("a", "c").unwrap().unwrap();
        assert_eq!(value.phase, "Unknown");
        assert!(value.audit_incomplete);
        assert!(!tracker.begin("a", "c", "TEST", 2, "1108").unwrap().1);
    }
}

pub(super) fn execute_queued_command(
    manager: &mut SerialManager,
    command: QueuedCommand,
    ack_status_tx: &Sender<CommandAckUpdate>,
    command_retry_count: u32,
) -> bool {
    let permit = command._permit;
    let _guard = ExecutionGuard {
        tracker: manager.operations.as_ref().map(|v| v.command_tracker()),
        port: manager.port_config.port_number.clone(),
        ack: command.ack_id,
    };
    if command
        .expires_at
        .is_some_and(|deadline| Instant::now() >= deadline)
    {
        drop(permit);
        warn!(
            "[串口命令过期] port={} device_id={} ack_id={:?} action=drop_without_sending",
            manager.serial_config.port_name, command.data.device_id, command.ack_id,
        );
        if let Some(operations) = &manager.operations {
            operations.command_tracker().update(
                &manager.port_config.port_number,
                command.ack_id,
                |v| {
                    v.phase = "Expired".into();
                    v.error = "deadline_expired_before_send".into();
                },
            );
        }
        // Publish the precise terminal evidence before the legacy failure
        // notification can wake a manager and finish its command.
        if command.report_ack {
            send_ack_status(
                manager,
                ack_status_tx,
                command.ack_id,
                CommandAckStatus::Failed,
            );
        }
        return false;
    }

    if !command.report_ack {
        let tracker = manager.operations.as_ref().map(|v| v.command_tracker());
        if let Some(t) = &tracker {
            t.update(&manager.port_config.port_number, command.ack_id, |v| {
                v.phase = "Executing".into();
                v.attempt_count = 1;
                v.attempts.push(crate::command_tracking::Attempt {
                    attempt: 1,
                    sent: false,
                    result: "Sending".into(),
                    at: chrono::Utc::now().to_rfc3339(),
                });
            });
        }
        let sent = manager.send_command(command.data);
        drop(permit);
        if let Some(t) = &tracker {
            t.update(&manager.port_config.port_number, command.ack_id, |v| {
                v.phase = if sent { "SentUnconfirmed" } else { "Failed" }.into();
                v.sent_count = u32::from(sent);
                if !sent {
                    v.error = "write_failed".into();
                }
                if let Some(a) = v.attempts.last_mut() {
                    a.sent = sent;
                    a.result = if sent {
                        "SentUnconfirmed"
                    } else {
                        "write_failed"
                    }
                    .into();
                }
            });
        }
        return sent;
    }

    execute_command_with_ack(
        manager,
        command.data,
        command.ack_id,
        command.expires_at,
        ack_status_tx,
        command_retry_count,
        permit,
    )
}

pub(super) fn execute_command_with_ack(
    manager: &mut SerialManager,
    command: SendData,
    ack_id: Option<u64>,
    expires_at: Option<Instant>,
    ack_status_tx: &Sender<CommandAckUpdate>,
    command_retry_count: u32,
    mut permit: Option<crate::operations::CommandPermit>,
) -> bool {
    let attempts = command_retry_count.max(1);
    let mut audit = super::command_audit::CommandAudit::new(
        &manager.serial_config.port_name,
        &command,
        ack_id,
        attempts,
    );
    for attempt in 1..=attempts {
        if expires_at.is_some_and(|deadline| Instant::now() >= deadline) {
            drop(permit.take());
            warn!(
                "[串口命令过期] port={} device_id={} ack_id={:?} attempt={} action=stop_retrying",
                manager.serial_config.port_name, command.device_id, ack_id, attempt,
            );
            audit.result = "expired";
            if let Some(operations) = &manager.operations {
                operations.command_tracker().update(
                    &manager.port_config.port_number,
                    ack_id,
                    |v| {
                        v.phase = "Expired".into();
                        v.error = "deadline_expired".into();
                    },
                );
            }
            send_ack_status(manager, ack_status_tx, ack_id, CommandAckStatus::Failed);
            audit.failure("deadline_expired");
            return false;
        }
        debug!(
            "串口 {} 下发命令 ACK 尝试 {}/{}",
            manager.serial_config.port_name, attempt, attempts
        );
        audit.attempt_count = attempt;
        if let Some(operations) = &manager.operations {
            operations
                .command_tracker()
                .update(&manager.port_config.port_number, ack_id, |v| {
                    v.phase = "Executing".into();
                    v.attempt_count = attempt;
                    v.attempts.push(crate::command_tracking::Attempt {
                        attempt,
                        sent: false,
                        result: "Sending".into(),
                        at: chrono::Utc::now().to_rfc3339(),
                    });
                });
        }
        if !manager.send_command(command.clone()) {
            drop(permit.take());
            if let Some(operations) = &manager.operations {
                operations.command_tracker().update(
                    &manager.port_config.port_number,
                    ack_id,
                    |v| v.error = "write_failed".into(),
                );
            }
            send_ack_status(manager, ack_status_tx, ack_id, CommandAckStatus::Failed);
            audit.result = "failed";
            audit.failure("write_failed");
            return false;
        }

        audit.sent_count += 1;
        if let Some(operations) = &manager.operations {
            operations
                .command_tracker()
                .update(&manager.port_config.port_number, ack_id, |v| {
                    v.sent_count += 1;
                    if let Some(a) = v.attempts.last_mut() {
                        a.sent = true;
                        a.result = "WaitingForAck".into();
                    }
                });
        }

        send_ack_status(manager, ack_status_tx, ack_id, CommandAckStatus::Waiting);
        match manager.receive_command_ack_result() {
            Ok(()) => {
                // Physical I/O has ended. Release admission before publishing
                // terminal evidence so a fast manager can accept its next item.
                drop(permit.take());
                send_ack_status(manager, ack_status_tx, ack_id, CommandAckStatus::Received);
                audit.result = "received";
                audit.last_error = "";
                return true;
            }
            Err(reason) => {
                audit.failure(reason);
                if let Some(operations) = &manager.operations {
                    operations.command_tracker().update(
                        &manager.port_config.port_number,
                        ack_id,
                        |v| {
                            v.error = reason.into();
                            if let Some(a) = v.attempts.last_mut() {
                                a.result = reason.into();
                            }
                        },
                    );
                }
            }
        }
    }

    drop(permit.take());
    send_ack_status(manager, ack_status_tx, ack_id, CommandAckStatus::Failed);
    audit.result = "failed";
    false
}

pub(super) fn send_ack_status(
    manager: &SerialManager,
    ack_status_tx: &Sender<CommandAckUpdate>,
    ack_id: Option<u64>,
    status: CommandAckStatus,
) {
    if let Some(operations) = &manager.operations {
        operations
            .command_tracker()
            .update(&manager.port_config.port_number, ack_id, |v| match status {
                CommandAckStatus::Waiting => {}
                CommandAckStatus::Received => {
                    v.phase = "AckReceived".into();
                    v.error.clear();
                    if let Some(a) = v.attempts.last_mut() {
                        a.result = "AckReceived".into();
                    }
                }
                CommandAckStatus::Failed => {
                    // The OPC deadline sweeper or a worker rejection may have
                    // already recorded a more precise terminal outcome. Never
                    // turn Expired/Unknown into a generic Failed notification.
                    if v.terminal() {
                        return;
                    }
                    v.phase = "Failed".into();
                    if v.error.is_empty() {
                        v.error = "serial command rejected or write failed".into();
                    }
                    if let Some(a) = v.attempts.last_mut() {
                        if a.result == "Sending" {
                            a.result = "write_failed".into();
                        }
                    }
                }
            });
    }
    match status {
        CommandAckStatus::Received => info!(
            "[串口命令结果] port={} ack_id={:?} result=ack_received",
            manager.serial_config.port_name, ack_id,
        ),
        CommandAckStatus::Failed => warn!(
            "[串口命令结果] port={} ack_id={:?} result=failed",
            manager.serial_config.port_name, ack_id,
        ),
        CommandAckStatus::Waiting => {}
    }
    if let Err(e) = ack_status_tx.send(CommandAckUpdate {
        com: manager.port_config.port_number.clone(),
        ack_id,
        status,
    }) {
        warn!(
            "串口 {} 命令 ACK 状态回写通道发送失败: {:?}",
            manager.serial_config.port_name, e
        );
    }
}
