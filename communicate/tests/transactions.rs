use rcd_communicate::{
    config::{self, TaskConfig},
    frame::{self, Limits},
    points::{PointStore, PointUpdate, Quality, Value},
    poller::{self, ExchangeError, PollTask, Schedule},
    protocol::{DeviceProtocol, ProtocolError, Response},
    serial::{self, Transport},
};
use std::collections::VecDeque;
use std::io;
use std::sync::{
    atomic::{AtomicUsize, Ordering},
    Arc,
};
use std::time::{Duration, Instant};
use tokio_util::sync::CancellationToken;

fn port() -> config::PortConfig {
    let mut p = config::load(
        &std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("examples/config.toml"),
    )
    .unwrap()
    .config
    .ports
    .remove(0);
    p.first_byte_timeout_ms = 15;
    p.frame_timeout_ms = 40;
    p
}

struct FakeTransport {
    response: Vec<u8>,
    received: VecDeque<u8>,
    writes: Vec<Vec<u8>>,
    fail_after_first: bool,
    max_write: usize,
    clear_count: usize,
}
impl FakeTransport {
    fn new(response: Vec<u8>) -> Self {
        Self {
            response,
            received: VecDeque::new(),
            writes: Vec::new(),
            fail_after_first: false,
            max_write: usize::MAX,
            clear_count: 0,
        }
    }
}
impl Transport for FakeTransport {
    fn read(&mut self, bytes: &mut [u8], timeout: Duration) -> io::Result<usize> {
        let n = bytes.len().min(3).min(self.received.len());
        if n == 0 {
            std::thread::sleep(timeout.min(Duration::from_millis(1)));
            return Err(io::ErrorKind::TimedOut.into());
        }
        for byte in &mut bytes[..n] {
            *byte = self.received.pop_front().unwrap();
        }
        Ok(n)
    }
    fn write(&mut self, bytes: &[u8], _: Duration) -> io::Result<usize> {
        self.writes.push(bytes.to_vec());
        if self.fail_after_first && self.writes.len() > 1 {
            return Err(io::ErrorKind::BrokenPipe.into());
        }
        self.received.extend(std::mem::take(&mut self.response));
        Ok(bytes.len().min(self.max_write))
    }
    fn clear_input(&mut self) -> io::Result<()> {
        self.clear_count += 1;
        self.received.clear();
        Ok(())
    }
}
struct TestProtocol;
impl DeviceProtocol for TestProtocol {
    fn request(&mut self, _: &TaskConfig) -> Result<Vec<u8>, ProtocolError> {
        Ok(vec![1, 2, 3])
    }
    fn response(&mut self, _: &TaskConfig, payload: &[u8]) -> Result<Response, ProtocolError> {
        if payload != b"expected" {
            return Ok(Response::Ignore);
        }
        Ok(Response::Matched(vec![PointUpdate {
            name: "TEST".into(),
            value: Value::UInt32(42),
            quality: Quality::Good,
            error: String::new(),
        }]))
    }
}
fn task() -> PollTask {
    PollTask {
        config: port().tasks.remove(0),
        protocol: Box::new(TestProtocol),
    }
}
fn encoded(bytes: &[u8]) -> Vec<u8> {
    frame::encode(
        bytes,
        Limits {
            frame: 4096,
            payload: 65535,
        },
    )
    .unwrap()
}

#[test]
fn fragmented_response_matches_only_the_protocol_identity() {
    let input = [encoded(b"wrong-transaction"), encoded(b"expected")].concat();
    let mut transport = FakeTransport::new(input);
    let result = poller::exchange(
        &mut transport,
        &port(),
        &mut task(),
        &CancellationToken::new(),
    )
    .unwrap();
    assert_eq!(result[0].value, Value::UInt32(42));
    assert_eq!(transport.writes.len(), 1);
}

#[test]
fn unmatched_and_missing_replies_time_out_without_resending() {
    for bytes in [Vec::new(), encoded(b"wrong")] {
        let mut transport = FakeTransport::new(bytes);
        assert!(matches!(
            poller::exchange(
                &mut transport,
                &port(),
                &mut task(),
                &CancellationToken::new()
            ),
            Err(ExchangeError::Timeout { .. })
        ));
        assert_eq!(transport.writes.len(), 1);
    }
}

#[test]
fn partial_write_failure_never_restarts_from_byte_zero() {
    let mut transport = FakeTransport::new(Vec::new());
    transport.max_write = 2;
    transport.fail_after_first = true;
    assert!(serial::write_once(
        &mut transport,
        &[1, 2, 3, 4, 5],
        Instant::now() + Duration::from_secs(1),
        &CancellationToken::new()
    )
    .is_err());
    assert_eq!(transport.writes, vec![vec![1, 2, 3, 4, 5], vec![3, 4, 5]]);
}

struct SlicedWriter {
    calls: Vec<Vec<u8>>,
    remaining_errors: VecDeque<io::ErrorKind>,
    cancel: Option<CancellationToken>,
}
impl Transport for SlicedWriter {
    fn read(&mut self, _: &mut [u8], _: Duration) -> io::Result<usize> {
        unreachable!()
    }
    fn clear_input(&mut self) -> io::Result<()> {
        unreachable!()
    }
    fn write(&mut self, data: &[u8], _: Duration) -> io::Result<usize> {
        self.calls.push(data.to_vec());
        if self.calls.len() == 1 {
            return Ok(2);
        }
        if let Some(cancel) = &self.cancel {
            cancel.cancel();
        }
        match self.remaining_errors.pop_front() {
            Some(error) => Err(error.into()),
            None => Ok(data.len()),
        }
    }
}

#[test]
fn temporary_write_timeouts_resume_only_the_unsent_suffix() {
    let mut writer = SlicedWriter {
        calls: vec![],
        remaining_errors: VecDeque::from([
            io::ErrorKind::TimedOut,
            io::ErrorKind::WouldBlock,
            io::ErrorKind::Interrupted,
        ]),
        cancel: None,
    };
    serial::write_once(
        &mut writer,
        b"abcde",
        Instant::now() + Duration::from_secs(1),
        &CancellationToken::new(),
    )
    .unwrap();
    assert_eq!(
        writer.calls,
        vec![
            b"abcde".to_vec(),
            b"cde".to_vec(),
            b"cde".to_vec(),
            b"cde".to_vec(),
            b"cde".to_vec()
        ]
    );
}

#[test]
fn temporary_write_errors_still_honor_deadline_and_cancellation() {
    for cancelled in [false, true] {
        let cancel = CancellationToken::new();
        let mut writer = SlicedWriter {
            calls: vec![],
            remaining_errors: std::iter::repeat_n(io::ErrorKind::TimedOut, 1000).collect(),
            cancel: cancelled.then(|| cancel.clone()),
        };
        let result = serial::write_once(
            &mut writer,
            b"abcde",
            Instant::now() + Duration::from_millis(30),
            &cancel,
        );
        if cancelled {
            assert!(matches!(
                result,
                Err(serial::WriteError::Cancelled { written: 2 })
            ));
        } else {
            assert!(matches!(
                result,
                Err(serial::WriteError::Timeout { written: 2 })
            ));
        }
        assert!(writer.calls.iter().skip(1).all(|v| v == b"cde"));
    }
}

#[test]
fn cancellation_prevents_the_first_write() {
    let cancel = CancellationToken::new();
    cancel.cancel();
    let mut transport = FakeTransport::new(encoded(b"expected"));
    assert!(matches!(
        poller::exchange(&mut transport, &port(), &mut task(), &cancel),
        Err(ExchangeError::Cancelled)
    ));
    assert!(transport.writes.is_empty());
}

#[test]
fn quarantine_discards_late_bytes_and_is_bounded() {
    let mut transport = FakeTransport::new(Vec::new());
    transport.received.extend(encoded(b"late"));
    serial::quarantine(
        &mut transport,
        Duration::from_millis(5),
        &CancellationToken::new(),
    )
    .unwrap();
    assert!(transport.received.is_empty());
    assert_eq!(transport.clear_count, 2);
}

#[test]
fn schedule_is_fair_and_does_not_catch_up_missed_periods() {
    let at = Instant::now();
    let mut schedule = Schedule::new(2, at);
    assert_eq!(schedule.next(at), Some(0));
    schedule.completed(0, at + Duration::from_secs(10), Duration::from_millis(50));
    assert_eq!(schedule.next(at), Some(1));
    schedule.completed(1, at + Duration::from_secs(10), Duration::from_millis(50));
    assert_eq!(schedule.next(at + Duration::from_secs(10)), None);
    assert_eq!(schedule.next(at + Duration::from_millis(10050)), Some(0));
}

#[test]
fn disabled_worker_never_opens_a_serial_port() {
    let loaded = config::load(
        &std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("examples/config.toml"),
    )
    .unwrap();
    let store = Arc::new(PointStore::new(&loaded.config, &loaded.points));
    poller::run_worker(
        port(),
        vec![task()],
        store,
        CancellationToken::new(),
        |_| panic!("disabled port opened"),
    )
    .unwrap();
}

#[test]
fn failed_open_sets_bad_quality_and_cancellation_stops_retry() {
    let loaded = config::load(
        &std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("examples/config.toml"),
    )
    .unwrap();
    let store = Arc::new(PointStore::new(&loaded.config, &loaded.points));
    let cancel = CancellationToken::new();
    let count = Arc::new(AtomicUsize::new(0));
    let mut port = port();
    port.enabled = true;
    let mut t = task();
    t.config.enabled = true;
    poller::run_worker(port, vec![t], store.clone(), cancel.clone(), |_| {
        count.fetch_add(1, Ordering::SeqCst);
        cancel.cancel();
        Err(io::ErrorKind::NotFound.into())
    })
    .unwrap();
    assert_eq!(count.load(Ordering::SeqCst), 1);
    assert_eq!(store.port("example-port").unwrap().state, "Stopped");
    assert_eq!(
        store.sample("COMM_EXAMPLE_VALUE").unwrap().quality,
        Quality::OutOfService
    );
}
