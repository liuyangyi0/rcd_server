use rcd_communicate::{
    config, frame,
    points::{PointStore, Quality},
    protocol::production_registry,
    serial::Transport,
    synchronization::{self, Schedule},
};
use std::{
    collections::VecDeque,
    io,
    sync::{Arc, Mutex},
    time::{Duration, Instant},
};
use tokio_util::sync::CancellationToken;

fn loaded() -> config::LoadedConfig {
    let path =
        std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("examples/unit1-receiver.toml");
    let mut loaded = config::load(&path).unwrap();
    let port = &mut loaded.config.ports[0];
    port.enabled = true;
    port.tasks[0].enabled = true;
    port.synchronization.as_mut().unwrap().transmit_enabled = true;
    loaded
}

#[test]
fn stagger_yield_timeout_and_no_catchup() {
    let loaded = loaded();
    let settings = loaded.config.ports[0].synchronization.as_ref().unwrap();
    let t = Instant::now();
    let mut schedule = Schedule::new(settings, Duration::from_millis(500), t);
    assert!(!schedule.may_send(t));
    assert!(schedule.may_send(t + Duration::from_millis(50)));
    schedule.received(t + Duration::from_millis(50), true);
    assert!(!schedule.may_send(t + Duration::from_millis(200)));
    assert!(!schedule.partial_expired(t + Duration::from_millis(549)));
    assert!(schedule.partial_expired(t + Duration::from_millis(550)));
    schedule.discard_partial();
    assert!(schedule.may_send(t + Duration::from_millis(550)));
    schedule.sent_or_skipped(t + Duration::from_secs(9));
    assert!(!schedule.may_send(t + Duration::from_secs(13)));
    schedule.received(t + Duration::from_millis(13990), false);
    assert!(!schedule.may_send(t + Duration::from_secs(14)));
    assert!(schedule.may_send(t + Duration::from_millis(14040)));
}

#[test]
fn outgoing_requires_full_fresh_snapshot_cas_and_enabled_port() {
    let loaded = loaded();
    let store = PointStore::new(&loaded.config, &loaded.points);
    assert!(store
        .synchronization
        .current("sync", Instant::now())
        .is_none());
    let mut payload = vec![0; 1156];
    payload[0] = 1;
    assert!(store
        .synchronization
        .submit("sync", 0, &payload[..1155])
        .is_err());
    payload[0] = 9;
    assert!(store.synchronization.submit("sync", 0, &payload).is_err());
    payload[0] = 1;
    assert_eq!(
        store.synchronization.submit("sync", 0, &payload).unwrap(),
        1
    );
    assert!(store.synchronization.submit("sync", 0, &payload).is_err());
    assert_eq!(
        store
            .synchronization
            .current("sync", Instant::now())
            .unwrap(),
        payload
    );
    assert!(store
        .synchronization
        .current("sync", Instant::now() + Duration::from_secs(16))
        .is_none());
    for input in [
        r#"{"revision":1,"payload_hex":"9"}"#,
        r#"{"revision":1,"payload_hex":"中文"}"#,
        r#"{"revision":1,"payload_hex":"GG"}"#,
    ] {
        assert_eq!(
            serde_json::from_str::<serde_json::Value>(
                &store.synchronization.submit_json("sync", input)
            )
            .unwrap()["ok"],
            false
        );
    }
    let mut disabled = loaded.config.clone();
    disabled.ports[0].enabled = false;
    assert!(PointStore::new(&disabled, &loaded.points)
        .synchronization
        .submit("sync", 0, &payload)
        .is_err());
}

struct Stream {
    input: VecDeque<u8>,
    writes: Arc<Mutex<Vec<u8>>>,
    cancel: CancellationToken,
    end: Instant,
    fail_write: bool,
}
impl Transport for Stream {
    fn read(&mut self, out: &mut [u8], _: Duration) -> io::Result<usize> {
        if let Some(b) = self.input.pop_front() {
            out[0] = b;
            return Ok(1);
        }
        if Instant::now() >= self.end {
            self.cancel.cancel();
        }
        std::thread::sleep(Duration::from_millis(1));
        Err(io::ErrorKind::TimedOut.into())
    }
    fn write(&mut self, bytes: &[u8], _: Duration) -> io::Result<usize> {
        if self.fail_write && !self.writes.lock().unwrap().is_empty() {
            return Err(io::ErrorKind::BrokenPipe.into());
        }
        let n = bytes.len().min(3);
        self.writes.lock().unwrap().extend_from_slice(&bytes[..n]);
        Ok(n)
    }
    fn clear_input(&mut self) -> io::Result<()> {
        panic!("continuous sync must not discard receive data before sending")
    }
}

fn run(stale: bool, fail_write: bool) -> (Arc<PointStore>, Vec<u8>) {
    let mut loaded = loaded();
    let port = &mut loaded.config.ports[0];
    port.frame_timeout_ms = 20;
    let settings = port.synchronization.as_mut().unwrap();
    settings.receive_timeout_ms = 30;
    settings.idle_gap_ms = 2;
    let store = Arc::new(PointStore::new(&loaded.config, &loaded.points));
    let mut outgoing = vec![0; 1156];
    outgoing[0] = 1;
    store.synchronization.submit("sync", 0, &outgoing).unwrap();
    let mut incoming = vec![0; 105];
    incoming[0] = 9;
    incoming[50..55].copy_from_slice(&[1, 0xc8, 0, 7, 0]);
    let limits = frame::Limits {
        frame: 4096,
        payload: 4096,
    };
    let wire = frame::encode(&incoming, limits).unwrap();
    let cancel = CancellationToken::new();
    let writes = Arc::new(Mutex::new(Vec::new()));
    let mut stream = Stream {
        input: [wire.clone(), wire].concat().into(),
        writes: writes.clone(),
        cancel: cancel.clone(),
        end: Instant::now() + Duration::from_millis(if stale { 60 } else { 15 }),
        fail_write,
    };
    let mut tasks = production_registry().prepare(&loaded.config).unwrap();
    synchronization::run_stream(
        &mut stream,
        &loaded.config.ports[0],
        &mut tasks[0][0],
        &store,
        &cancel,
    )
    .unwrap();
    let output = writes.lock().unwrap().clone();
    (store, output)
}

#[test]
fn continuous_receive_keeps_both_frames_sends_one_snapshot_and_times_out() {
    let (store, wire) = run(false, false);
    assert_eq!(store.port("sync").unwrap().successes, 2);
    assert_eq!(
        store.sample("DATA.Buildings.91UBH.Fire").unwrap().quality,
        Quality::Good
    );
    let payload = frame::decode(
        &wire,
        frame::Limits {
            frame: 4096,
            payload: 4096,
        },
    )
    .unwrap();
    assert_eq!(payload.len(), 1156);
    assert_eq!(payload[0], 1);
    let (store, _) = run(true, false);
    assert_eq!(
        store.sample("DATA.Buildings.91UBH.Fire").unwrap().quality,
        Quality::NoCommunication
    );
}

#[test]
fn partial_write_failure_never_restarts_frame_or_claims_delivery() {
    let (store, wire) = run(false, true);
    assert_eq!(wire.len(), 3);
    assert_eq!(store.port("sync").unwrap().state, "Disconnected");
    assert!(store.port("sync").unwrap().last_error.contains("3 bytes"));
    let status: serde_json::Value =
        serde_json::from_str(&store.synchronization.status("sync").unwrap()).unwrap();
    assert_eq!(status["complete_writes"], 0);
    assert!(status["last_send_error"]
        .as_str()
        .unwrap()
        .contains("3 bytes"));
}

struct ScheduledInput {
    events: VecDeque<(Duration, Vec<u8>)>,
    started: Instant,
    stop: Duration,
    cancel: CancellationToken,
}
impl Transport for ScheduledInput {
    fn read(&mut self, out: &mut [u8], _: Duration) -> io::Result<usize> {
        let elapsed = self.started.elapsed();
        if self.events.front().is_some_and(|(at, _)| elapsed >= *at) {
            let (_, bytes) = self.events.pop_front().unwrap();
            out[..bytes.len()].copy_from_slice(&bytes);
            return Ok(bytes.len());
        }
        if elapsed >= self.stop {
            self.cancel.cancel();
        }
        std::thread::sleep(Duration::from_millis(1));
        Err(io::ErrorKind::TimedOut.into())
    }
    fn write(&mut self, _: &[u8], _: Duration) -> io::Result<usize> {
        panic!("no producer")
    }
    fn clear_input(&mut self) -> io::Result<()> {
        panic!("continuous stream")
    }
}
fn scheduled_run(events: Vec<(u64, Vec<u8>)>, timeout: u64) -> Arc<PointStore> {
    let mut c = loaded();
    c.config.ports[0].frame_timeout_ms = 10;
    c.config.ports[0]
        .synchronization
        .as_mut()
        .unwrap()
        .receive_timeout_ms = timeout;
    let store = Arc::new(PointStore::new(&c.config, &c.points));
    let cancel = CancellationToken::new();
    let mut input = ScheduledInput {
        events: events
            .into_iter()
            .map(|(at, b)| (Duration::from_millis(at), b))
            .collect(),
        started: Instant::now(),
        stop: Duration::from_millis(90),
        cancel: cancel.clone(),
    };
    let mut tasks = production_registry().prepare(&c.config).unwrap();
    synchronization::run_stream(
        &mut input,
        &c.config.ports[0],
        &mut tasks[0][0],
        &store,
        &cancel,
    )
    .unwrap();
    store
}
fn peer_frame() -> Vec<u8> {
    let mut payload = vec![0; 105];
    payload[0] = 9;
    frame::encode(
        &payload,
        frame::Limits {
            frame: 4096,
            payload: 4096,
        },
    )
    .unwrap()
}
#[test]
fn stream_recovers_hidden_frame_and_records_crc_and_expired_noise() {
    let good = peer_frame();
    let hidden = [&[0x00, 0x10, 0x69, 0x00, 0xf6, 0x0f][..], &good].concat();
    let mut corrupt = good.clone();
    let end = corrupt.len() - 1;
    corrupt[end] ^= 1;
    let store = scheduled_run(vec![(0, hidden), (25, corrupt), (45, good)], 500);
    let port = store.port("sync").unwrap();
    assert_eq!(port.successes, 2);
    assert!(port.rejected_frames >= 1);
    assert!(port.discarded_bytes >= 6);
    assert!(port.partial_timeouts >= 1);
    assert_eq!(
        store.sample("DATA.Buildings.91UBH.Fire").unwrap().quality,
        Quality::Good
    );
}
#[test]
fn malformed_frame_after_timeout_does_not_mask_communication_loss() {
    let short = frame::encode(
        &[9, 0],
        frame::Limits {
            frame: 4096,
            payload: 4096,
        },
    )
    .unwrap();
    let store = scheduled_run(vec![(0, peer_frame()), (50, short)], 30);
    assert_eq!(
        store.sample("DATA.Buildings.91UBH.Fire").unwrap().quality,
        Quality::NoCommunication
    );
}
