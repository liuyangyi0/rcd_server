//! Version queries own their queue and frame assembly, not the polling index,
//! KKS cache, communication counters or reconnect policy. Only the port worker
//! calls this module, borrowing its existing handle for a single transaction.
use super::transport::{read_before_deadline, wire_duration};
use crate::protocol::{self, StationVersionResponse};
use log::{info, warn};
use serialport::{ClearBuffer, SerialPort};
use std::collections::VecDeque;
use std::io::{self, ErrorKind};
use std::time::{Duration, Instant};

const RESPONSE_LEN: usize = 9;
const COMPLETION_GUARD: Duration = Duration::from_millis(5);
const LATE_FRAME_QUARANTINE: Duration = Duration::from_millis(50);

#[derive(Default)]
pub(super) struct VersionQueries(VecDeque<u8>);

impl VersionQueries {
    pub(super) fn schedule(&mut self, device: u8) {
        if !self.0.contains(&device) {
            self.0.push_back(device);
        }
    }
    pub(super) fn cancel(&mut self, device: u8) {
        self.0.retain(|id| *id != device);
    }
    pub(super) fn next(&mut self) -> Option<u8> {
        self.0.pop_front()
    }
}

pub(super) enum VersionReadError {
    Rejected(String),
    Transport(io::Error),
}

pub(super) fn quarantine(port: &dyn SerialPort, port_name: &str) {
    std::thread::sleep(LATE_FRAME_QUARANTINE);
    if let Err(error) = port.clear(ClearBuffer::Input) {
        warn!(
            "[版本查询迟到帧清理失败] port={} error={:?}",
            port_name, error
        );
    }
}

pub(super) fn receive(
    port: &mut dyn SerialPort,
    buffer: &mut [u8],
    first_byte_timeout: Duration,
    baud_rate: u32,
    port_name: &str,
    device_id: u8,
) -> Result<StationVersionResponse, VersionReadError> {
    let mut received = Vec::with_capacity(RESPONSE_LEN);
    let mut deadline = Instant::now() + first_byte_timeout;
    let mut completion_extended = false;
    let read_result: io::Result<()> = loop {
        if received.len() >= RESPONSE_LEN {
            break Ok(());
        }
        match read_before_deadline(port, buffer, deadline) {
            Ok(0) => {
                break Err(io::Error::new(
                    ErrorKind::TimedOut,
                    "empty version response",
                ))
            }
            Ok(count) => {
                received.extend_from_slice(&buffer[..count]);
                if !completion_extended {
                    deadline = Instant::now()
                        + wire_duration(RESPONSE_LEN, baud_rate).saturating_add(COMPLETION_GUARD);
                    completion_extended = true;
                }
            }
            Err(error) => break Err(error),
        }
    };
    match read_result {
        Ok(()) => {
            let result = if received.len() != RESPONSE_LEN {
                Err(format!(
                    "station version response length must be exactly {RESPONSE_LEN} bytes, got {}",
                    received.len()
                ))
            } else {
                protocol::parse_station_version_response(&received).map_err(str::to_string)
            };
            let hex = received
                .iter()
                .map(|b| format!("{b:02X}"))
                .collect::<Vec<_>>()
                .join(" ");
            info!(
                "[版本查询响应] port={} device_id={} frame_len={} frame_hex={:?} valid={}",
                port_name,
                device_id,
                received.len(),
                hex,
                result.is_ok()
            );
            if result.is_err() {
                quarantine(port, port_name);
            }
            result.map_err(VersionReadError::Rejected)
        }
        Err(error) if error.kind() == ErrorKind::TimedOut => {
            quarantine(port, port_name);
            Err(VersionReadError::Rejected(format!(
                "station version query timed out after {} ms",
                first_byte_timeout.as_millis()
            )))
        }
        Err(error) => Err(VersionReadError::Transport(error)),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn startup_queue_deduplicates_preserves_order_and_cancels() {
        let mut queue = VersionQueries::default();
        queue.schedule(11);
        queue.schedule(12);
        queue.schedule(11);
        queue.schedule(13);
        queue.cancel(12);
        assert_eq!(queue.next(), Some(11));
        assert_eq!(queue.next(), Some(13));
        assert_eq!(queue.next(), None);
        queue.schedule(11);
        assert_eq!(queue.next(), Some(11));
    }
}
