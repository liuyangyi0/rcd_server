//! One bounded summary per executed ACK command; never changes retry policy.
use std::time::Instant;

use serde::Serialize;

use crate::model::SendData;

#[derive(Debug, Serialize)]
pub(super) struct CommandAudit {
    schema_version: u8,
    port_id: String,
    device_id: String,
    // A string preserves the u64 identifier in JavaScript. Scope: source + boot.
    ack_id: Option<String>,
    request: String,
    request_bytes: usize,
    request_truncated: bool,
    max_attempts: u32,
    pub attempt_count: u32,
    pub sent_count: u32,
    pub result: &'static str,
    pub last_error: &'static str,
    failure_count: u32,
    failures: Vec<AttemptFailure>,
    duration_ms: u64,
    #[serde(skip)]
    started: Option<Instant>,
}

#[derive(Debug, Serialize)]
struct AttemptFailure {
    attempt: u32,
    reason: &'static str,
}

impl CommandAudit {
    pub fn new(port: &str, command: &SendData, ack_id: Option<u64>, max_attempts: u32) -> Self {
        Self {
            schema_version: 1,
            port_id: port.into(),
            device_id: command.device_id.to_string(),
            ack_id: ack_id.map(|id| id.to_string()),
            request: command
                .command
                .iter()
                .take(256)
                .map(|v| format!("{v:02X}"))
                .collect(),
            request_bytes: command.command.len(),
            request_truncated: command.command.len() > 256,
            max_attempts,
            attempt_count: 0,
            sent_count: 0,
            result: "interrupted",
            last_error: "",
            failure_count: 0,
            failures: Vec::new(),
            duration_ms: 0,
            started: Some(Instant::now()),
        }
    }

    pub fn failure(&mut self, reason: &'static str) {
        self.last_error = reason;
        self.failure_count = self.failure_count.saturating_add(1);
        if self.failures.len() == 8 {
            self.failures.remove(0);
        }
        self.failures.push(AttemptFailure {
            attempt: self.attempt_count,
            reason,
        });
    }
}

impl Drop for CommandAudit {
    fn drop(&mut self) {
        self.duration_ms = self
            .started
            .map(|v| v.elapsed().as_millis().min(u64::MAX as u128) as u64)
            .unwrap_or(0);
        if let Ok(message) = serde_json::to_string(self) {
            crate::logging::command_audit(
                &message,
                self.sent_count > 1 || self.result != "received",
            );
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn command_audit_preserves_retry_reasons_after_success_and_bounds_payload() {
        let mut value = CommandAudit::new(
            "/dev/ttyWCH0",
            &SendData {
                device_id: 17,
                command: vec![0xAA; 300],
            },
            Some(u64::MAX),
            3,
        );
        value.attempt_count = 1;
        value.sent_count = 1;
        value.failure("ack_timeout");
        value.attempt_count = 2;
        value.sent_count = 2;
        value.failure("ack_mismatch");
        value.attempt_count = 3;
        value.sent_count = 3;
        value.result = "received";
        value.last_error = "";
        let json = serde_json::to_value(&value).unwrap();
        assert_eq!(json["sent_count"], 3);
        assert_eq!(json["ack_id"], u64::MAX.to_string());
        assert_eq!(json["failures"][0]["reason"], "ack_timeout");
        assert_eq!(json["failures"][1]["reason"], "ack_mismatch");
        assert_eq!(json["request"].as_str().unwrap().len(), 512);
        assert_eq!(json["request_truncated"], true);
    }

    #[test]
    fn command_audit_distinguishes_write_attempt_from_complete_send() {
        let mut value = CommandAudit::new(
            "COM1",
            &SendData {
                device_id: 1,
                command: vec![1],
            },
            None,
            3,
        );
        value.attempt_count = 1;
        value.failure("write_failed");
        value.result = "failed";
        let json = serde_json::to_value(&value).unwrap();
        assert_eq!(json["sent_count"], 0);
        assert_eq!(json["attempt_count"], 1);
        for _ in 0..20 {
            value.failure("ack_timeout");
        }
        assert_eq!(value.failures.len(), 8);
        assert_eq!(value.failure_count, 21);
    }
}
