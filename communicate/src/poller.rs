//! Serial scheduling and request transactions. Missed intervals are coalesced,
//! never accumulated as a backlog. Each worker exclusively owns its transport.
use std::io;
use std::sync::Arc;
use std::thread;
use std::time::{Duration, Instant};

use chrono::Utc;
use thiserror::Error;
use tokio_util::sync::CancellationToken;

use crate::config::{PortConfig, TaskConfig};
use crate::frame::{self, FrameDecoder, Limits};
use crate::points::{PointStore, PointUpdate, Quality, StoreError};
use crate::protocol::{DeviceProtocol, ProtocolError, Response};
use crate::serial::{self, Transport, WriteError};

pub struct PollTask {
    pub config: TaskConfig,
    pub protocol: Box<dyn DeviceProtocol>,
}

#[derive(Debug, Error)]
pub enum ExchangeError {
    #[error("request cancelled")]
    Cancelled,
    #[error("response timed out ({rejected} rejected frames, {ignored} unmatched replies)")]
    Timeout { rejected: u64, ignored: u64 },
    #[error(transparent)]
    Write(#[from] WriteError),
    #[error("serial read: {0}")]
    Read(#[from] io::Error),
    #[error(transparent)]
    Protocol(#[from] ProtocolError),
    #[error(transparent)]
    Frame(#[from] frame::FrameError),
}

pub fn exchange(
    transport: &mut dyn Transport,
    port: &PortConfig,
    task: &mut PollTask,
    cancel: &CancellationToken,
) -> Result<Vec<PointUpdate>, ExchangeError> {
    if cancel.is_cancelled() {
        return Err(ExchangeError::Cancelled);
    }
    let limits = Limits {
        frame: port.max_frame_bytes,
        payload: port.max_payload_bytes,
    };
    let payload = task.protocol.request(&task.config)?;
    if payload.is_empty() {
        return Err(ProtocolError("empty request is not allowed".into()).into());
    }
    let request = frame::encode(&payload, limits)?;
    let mut decoder = FrameDecoder::new(limits)?;
    transport.clear_input()?;
    serial::write_once(
        transport,
        &request,
        Instant::now() + Duration::from_millis(port.write_timeout_ms),
        cancel,
    )?;
    let started = Instant::now();
    let first_deadline = started + Duration::from_millis(port.first_byte_timeout_ms);
    let frame_deadline = started + Duration::from_millis(port.frame_timeout_ms);
    let mut received = false;
    let mut rejected: u64 = 0;
    let mut ignored: u64 = 0;
    let mut buffer = vec![0u8; port.max_frame_bytes.min(1024)];
    loop {
        if cancel.is_cancelled() {
            return Err(ExchangeError::Cancelled);
        }
        let deadline = if received {
            frame_deadline
        } else {
            first_deadline
        };
        if Instant::now() >= deadline {
            return Err(ExchangeError::Timeout { rejected, ignored });
        }
        match transport.read(
            &mut buffer,
            deadline
                .saturating_duration_since(Instant::now())
                .min(Duration::from_millis(10)),
        ) {
            Ok(0) => {
                return Err(io::Error::new(io::ErrorKind::UnexpectedEof, "serial closed").into())
            }
            Ok(n) if n <= buffer.len() => {
                received = true;
                let frames = decoder.feed(&buffer[..n])?;
                rejected = rejected.saturating_add(frames.rejected_frames);
                for payload in frames.payloads {
                    match task.protocol.response(&task.config, &payload)? {
                        Response::Ignore => ignored = ignored.saturating_add(1),
                        Response::Matched(points) => return Ok(points),
                    }
                }
            }
            Ok(_) => return Err(io::Error::other("invalid transport read count").into()),
            Err(e)
                if matches!(
                    e.kind(),
                    io::ErrorKind::TimedOut
                        | io::ErrorKind::WouldBlock
                        | io::ErrorKind::Interrupted
                ) => {}
            Err(e) => return Err(e.into()),
        }
    }
}

pub struct Schedule {
    due: Vec<Instant>,
    cursor: usize,
}
impl Schedule {
    pub fn new(count: usize, now: Instant) -> Self {
        Self {
            due: vec![now; count],
            cursor: 0,
        }
    }
    pub fn next(&mut self, now: Instant) -> Option<usize> {
        for offset in 0..self.due.len() {
            let index = (self.cursor + offset) % self.due.len();
            if self.due[index] <= now {
                self.cursor = (index + 1) % self.due.len();
                return Some(index);
            }
        }
        None
    }
    pub fn completed(&mut self, index: usize, now: Instant, interval: Duration) {
        self.due[index] = now + interval;
    }
}

fn wait(duration: Duration, cancel: &CancellationToken) {
    let end = Instant::now() + duration;
    while !cancel.is_cancelled() && Instant::now() < end {
        thread::sleep(
            end.saturating_duration_since(Instant::now())
                .min(Duration::from_millis(10)),
        );
    }
}

fn fail_tasks(
    store: &PointStore,
    tasks: &[PollTask],
    quality: Quality,
    message: &str,
) -> Result<(), StoreError> {
    for task in tasks {
        store.fail(&task.config.id, quality, message)?;
    }
    Ok(())
}

/// Run on one dedicated blocking thread, never on a Tokio async worker. The
/// caller supplies only validated, enabled tasks with real protocol adapters.
pub fn run_worker(
    port: PortConfig,
    tasks: Vec<PollTask>,
    store: Arc<PointStore>,
    cancel: CancellationToken,
    connect: impl FnMut(&PortConfig) -> io::Result<Box<dyn Transport>>,
) -> Result<(), StoreError> {
    run_worker_with_wait(port, tasks, store, cancel, connect, wait)
}

fn run_worker_with_wait(
    port: PortConfig,
    mut tasks: Vec<PollTask>,
    store: Arc<PointStore>,
    cancel: CancellationToken,
    mut connect: impl FnMut(&PortConfig) -> io::Result<Box<dyn Transport>>,
    mut wait: impl FnMut(Duration, &CancellationToken),
) -> Result<(), StoreError> {
    if !port.enabled || tasks.is_empty() {
        return Ok(());
    }
    let mut backoff = port.reconnect_min_ms;
    let mut ever_opened = false;
    while !cancel.is_cancelled() {
        let mut transport = match connect(&port) {
            Ok(transport) => transport,
            Err(e) => {
                let message = e.to_string();
                store.port_state(&port.id, "OpenFailed", &message, false)?;
                fail_tasks(&store, &tasks, Quality::NoCommunication, &message)?;
                log::warn!("port={} open failed: {}", port.id, message);
                wait(Duration::from_millis(backoff), &cancel);
                backoff = backoff.saturating_mul(2).min(port.reconnect_max_ms);
                continue;
            }
        };
        store.port_state(&port.id, "Open", "", ever_opened)?;
        ever_opened = true;
        if tasks[0].protocol.unsolicited() {
            let received_valid = crate::synchronization::run_stream(
                transport.as_mut(),
                &port,
                &mut tasks[0],
                &store,
                &cancel,
            )?;
            drop(transport);
            if !cancel.is_cancelled() {
                if received_valid {
                    backoff = port.reconnect_min_ms;
                }
                let error = store.port(&port.id)?.last_error;
                let error = if error.is_empty() {
                    "serial connection lost"
                } else {
                    &error
                };
                fail_tasks(&store, &tasks, Quality::NoCommunication, error)?;
                store.port_state(&port.id, "Reconnecting", error, false)?;
                wait(Duration::from_millis(backoff), &cancel);
                backoff = backoff.saturating_mul(2).min(port.reconnect_max_ms);
            }
            continue;
        }
        let mut schedule = Schedule::new(tasks.len(), Instant::now());
        while !cancel.is_cancelled() {
            let Some(index) = schedule.next(Instant::now()) else {
                wait(Duration::from_millis(10), &cancel);
                continue;
            };
            let task = &mut tasks[index];
            store.attempt(&task.config.id)?;
            let mut reopen = false;
            match exchange(transport.as_mut(), &port, task, &cancel) {
                Ok(points) => {
                    if let Err(e) = store.publish(&task.config.id, &points, Utc::now()) {
                        store.fail(&task.config.id, Quality::Configuration, &e.to_string())?;
                    } else {
                        backoff = port.reconnect_min_ms;
                    }
                }
                Err(ExchangeError::Cancelled) => break,
                Err(e) => {
                    let quality =
                        if matches!(e, ExchangeError::Protocol(_) | ExchangeError::Frame(_)) {
                            Quality::Configuration
                        } else {
                            Quality::NoCommunication
                        };
                    store.fail(&task.config.id, quality, &e.to_string())?;
                    reopen = matches!(e, ExchangeError::Write(_) | ExchangeError::Read(_));
                    log::debug!(
                        "port={} task={} exchange failed: {}",
                        port.id,
                        task.config.id,
                        e
                    );
                    if !reopen
                        && serial::quarantine(
                            transport.as_mut(),
                            Duration::from_millis(port.late_frame_guard_ms),
                            &cancel,
                        )
                        .is_err()
                    {
                        reopen = true;
                    }
                }
            }
            schedule.completed(
                index,
                Instant::now(),
                Duration::from_millis(task.config.interval_ms),
            );
            if reopen {
                break;
            }
        }
        // Drop the sole serial handle before reconnecting. The next connection
        // gets a new decoder; no bytes from this transport survive into it.
        drop(transport);
        if !cancel.is_cancelled() {
            fail_tasks(
                &store,
                &tasks,
                Quality::NoCommunication,
                "serial connection lost",
            )?;
            store.port_state(&port.id, "Reconnecting", "serial connection lost", false)?;
            wait(Duration::from_millis(backoff), &cancel);
            backoff = backoff.saturating_mul(2).min(port.reconnect_max_ms);
        }
    }
    fail_tasks(&store, &tasks, Quality::OutOfService, "acquisition stopped")?;
    store.port_state(&port.id, "Stopped", "", false)
}

#[cfg(test)]
mod tests {
    use super::*;
    struct DisconnectingStream {
        input: Option<Vec<u8>>,
    }
    impl Transport for DisconnectingStream {
        fn read(&mut self, out: &mut [u8], _: Duration) -> io::Result<usize> {
            if let Some(data) = self.input.take() {
                out[..data.len()].copy_from_slice(&data);
                Ok(data.len())
            } else {
                Err(io::ErrorKind::BrokenPipe.into())
            }
        }
        fn write(&mut self, _: &[u8], _: Duration) -> io::Result<usize> {
            panic!("transmit disabled")
        }
        fn clear_input(&mut self) -> io::Result<()> {
            panic!("stream must not clear input")
        }
    }

    #[test]
    fn stream_reconnect_resets_after_valid_receive_and_reports_closed_handle() {
        let mut loaded = crate::config::load(
            &PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("examples/unit1-receiver.toml"),
        )
        .unwrap();
        let port = &mut loaded.config.ports[0];
        port.enabled = true;
        port.tasks[0].enabled = true;
        port.reconnect_min_ms = 100;
        port.reconnect_max_ms = 800;
        let store = Arc::new(PointStore::new(&loaded.config, &loaded.points));
        let mut tasks = crate::protocol::production_registry()
            .prepare(&loaded.config)
            .unwrap();
        let mut payload = vec![0; 105];
        payload[0] = 9;
        let wire = frame::encode(
            &payload,
            Limits {
                frame: 4096,
                payload: 4096,
            },
        )
        .unwrap();
        let cancel = CancellationToken::new();
        let mut calls = 0;
        let mut waits = vec![];
        run_worker_with_wait(
            loaded.config.ports[0].clone(),
            tasks.remove(0),
            store.clone(),
            cancel.clone(),
            |_| {
                calls += 1;
                match calls {
                    1 | 2 => Err(io::ErrorKind::NotFound.into()),
                    3 => Ok(Box::new(DisconnectingStream {
                        input: Some(wire.clone()),
                    })),
                    4 => Ok(Box::new(DisconnectingStream { input: None })),
                    _ => {
                        cancel.cancel();
                        Err(io::ErrorKind::NotFound.into())
                    }
                }
            },
            |duration, token| {
                if !token.is_cancelled() {
                    waits.push(duration.as_millis());
                    let state = store.port("sync").unwrap();
                    if waits.len() >= 3 {
                        assert_eq!(state.state, "Reconnecting");
                        assert!(state.last_error.contains("serial receive"));
                    }
                }
            },
        )
        .unwrap();
        assert_eq!(waits, [100, 200, 100, 200]);
        assert_eq!(store.port("sync").unwrap().successes, 1);
        assert_eq!(store.port("sync").unwrap().state, "Stopped");
    }
    use std::path::PathBuf;
}
