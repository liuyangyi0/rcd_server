//! Periodic bidirectional data synchronization, not control command retries.
//! One serial owner receives continuously and yields its transmit slot to an
//! in-progress peer frame. Only fresh, explicitly supplied snapshots are sent.
use crate::{
    config::{Config, ConfigError, PortConfig},
    frame::{FrameDecoder, Limits},
    points::{PointStore, Quality},
    poller::PollTask,
    protocol::Response,
    serial::{self, Transport},
    unit_report::Layout,
};
use serde::Deserialize;
use std::{
    collections::BTreeMap,
    io,
    sync::Mutex,
    time::{Duration, Instant},
};
use tokio_util::sync::CancellationToken;

#[derive(Clone, Debug, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Settings {
    pub transmit_enabled: bool,
    pub transmit_protocol: String,
    pub send_interval_ms: u64,
    pub initial_delay_ms: u64,
    pub idle_gap_ms: u64,
    pub receive_timeout_ms: u64,
    #[serde(default = "default_refresh_interval")]
    pub input_refresh_interval_ms: u64,
    #[serde(default = "default_input_timeout")]
    pub snapshot_max_age_ms: u64,
}
fn default_refresh_interval() -> u64 {
    5000
}
fn default_input_timeout() -> u64 {
    15000
}

pub fn validate_port(port: &PortConfig) -> Result<(), ConfigError> {
    let known = port
        .tasks
        .iter()
        .filter(|t| Layout::from_protocol(&t.protocol).is_some())
        .count();
    let reject = |s: &str| ConfigError::Invalid(format!("port {}: {s}", port.id));
    if known > 0 && (known != 1 || port.tasks.len() != 1 || port.synchronization.is_none()) {
        return Err(reject(
            "unit reports need one task and explicit synchronization settings",
        ));
    }
    let Some(settings) = &port.synchronization else {
        return Ok(());
    };
    if port.id.contains('.') {
        return Err(reject("synchronization port id must not contain '.'"));
    }
    let receive = port
        .tasks
        .first()
        .and_then(|t| Layout::from_protocol(&t.protocol));
    let transmit = Layout::from_protocol(&settings.transmit_protocol);
    if known != 1 || receive.is_none() || transmit.is_none() || receive == transmit {
        return Err(reject(
            "synchronization requires opposite supported receive/transmit layouts",
        ));
    }
    if !(100..=3_600_000).contains(&settings.send_interval_ms)
        || settings.initial_delay_ms >= settings.send_interval_ms
        || !(1..=60_000).contains(&settings.idle_gap_ms)
        || settings.idle_gap_ms >= settings.send_interval_ms
        || !(100..=3_600_000).contains(&settings.receive_timeout_ms)
        || settings.receive_timeout_ms < port.frame_timeout_ms
        || !(100..=3_600_000).contains(&settings.snapshot_max_age_ms)
        || !(100..=3_600_000).contains(&settings.input_refresh_interval_ms)
        || settings.snapshot_max_age_ms <= settings.input_refresh_interval_ms
        || port.max_payload_bytes
            < receive
                .unwrap()
                .payload_len()
                .max(transmit.unwrap().payload_len())
    {
        return Err(reject(
            "invalid synchronization deadlines or payload capacity",
        ));
    }
    // Worst case is an uncompressed snapshot. Never accept a configuration
    // that only works while the payload happens to compress well.
    if port.max_frame_bytes < transmit.unwrap().payload_len() + 10
        || port.max_frame_bytes < receive.unwrap().payload_len() + 10
    {
        return Err(reject(
            "frame capacity must fit the full uncompressed snapshot",
        ));
    }
    Ok(())
}

struct Snapshot {
    bytes: Vec<u8>,
    supplied: Instant,
}
struct Slot {
    settings: Settings,
    enabled: bool,
    revision: u64,
    snapshot: Option<Snapshot>,
    sent: u64,
    error: String,
    fields: BTreeMap<String, (crate::points::Value, Instant, chrono::DateTime<chrono::Utc>)>,
    field_payload: Vec<u8>,
    atomic_mode: bool,
}
pub struct Mailboxes(Mutex<BTreeMap<String, Slot>>);
impl Mailboxes {
    pub fn submit_json(&self, port: &str, input: &str) -> String {
        #[derive(Deserialize)]
        #[serde(deny_unknown_fields)]
        struct Request {
            revision: u64,
            payload_hex: String,
        }
        let result = (|| -> Result<u64, String> {
            if input.len() > 8192 {
                return Err("snapshot request too large".into());
            }
            let request: Request = serde_json::from_str(input).map_err(|e| e.to_string())?;
            let hex: String = request
                .payload_hex
                .chars()
                .filter(|c| !c.is_ascii_whitespace())
                .collect();
            if !hex.len().is_multiple_of(2) || !hex.is_ascii() {
                return Err("invalid hexadecimal payload".into());
            }
            let bytes: Result<Vec<_>, _> = (0..hex.len())
                .step_by(2)
                .map(|i| u8::from_str_radix(&hex[i..i + 2], 16))
                .collect();
            self.submit(
                port,
                request.revision,
                &bytes.map_err(|_| "invalid hexadecimal payload")?,
            )
        })();
        match result { Ok(revision) => serde_json::json!({"ok":true,"revision":revision,"state":"snapshot_accepted_not_delivered"}),
            Err(error) => serde_json::json!({"ok":false,"error":error}) }.to_string()
    }
    pub fn new(config: &Config) -> Self {
        Self(Mutex::new(
            config
                .ports
                .iter()
                .filter_map(|p| {
                    p.synchronization.clone().map(|settings| {
                        (
                            p.id.clone(),
                            Slot {
                                settings,
                                enabled: p.enabled && p.tasks.iter().any(|t| t.enabled),
                                revision: 0,
                                snapshot: None,
                                sent: 0,
                                error: String::new(),
                                fields: BTreeMap::new(),
                                field_payload: Vec::new(),
                                atomic_mode: false,
                            },
                        )
                    })
                })
                .collect(),
        ))
    }
    /// CAS replaces the whole uncompressed snapshot. No partial field updates,
    /// no persistence/replay of stale process state, no queue of old snapshots.
    pub fn submit(&self, port: &str, revision: u64, payload: &[u8]) -> Result<u64, String> {
        let mut slots = self.0.lock().map_err(|_| "snapshot lock poisoned")?;
        let slot = slots.get_mut(port).ok_or("unknown synchronization port")?;
        if !slot.enabled || !slot.settings.transmit_enabled {
            return Err("transmission disabled".into());
        }
        if revision != slot.revision {
            return Err("snapshot revision conflict".into());
        }
        Layout::from_protocol(&slot.settings.transmit_protocol)
            .ok_or("unsupported layout")?
            .validate_payload(payload)
            .map_err(|e| e.to_string())?;
        slot.revision = slot
            .revision
            .checked_add(1)
            .ok_or("snapshot revision exhausted")?;
        slot.snapshot = Some(Snapshot {
            bytes: payload.to_vec(),
            supplied: Instant::now(),
        });
        slot.fields.clear();
        slot.atomic_mode = true;
        Ok(slot.revision)
    }
    /// Whole field set accepted in one critical section. Validation builds a
    /// private candidate; even an invalid last field cannot mutate live data.
    pub fn submit_fields_json(&self, port: &str, input: &str) -> Result<u64, String> {
        #[derive(Deserialize)]
        #[serde(deny_unknown_fields)]
        struct Request {
            revision: u64,
            #[serde(deserialize_with = "unique_fields")]
            fields: BTreeMap<String, serde_json::Value>,
        }
        if input.len() > 64 * 1024 {
            return Err("snapshot request too large".into());
        }
        let request: Request = serde_json::from_str(input).map_err(|e| e.to_string())?;
        let mut slots = self.0.lock().map_err(|_| "snapshot lock poisoned")?;
        let slot = slots.get_mut(port).ok_or("unknown port")?;
        if !slot.enabled || !slot.settings.transmit_enabled {
            return Err("transmission disabled".into());
        }
        if request.revision != slot.revision {
            return Err("snapshot revision conflict".into());
        }
        let layout =
            Layout::from_protocol(&slot.settings.transmit_protocol).ok_or("unsupported layout")?;
        let definitions = layout.transmit_definitions(port);
        if request.fields.len() != definitions.len() {
            return Err("snapshot requires every input exactly once".into());
        }
        let mut payload = layout.empty_transmit_payload();
        let mut fields = BTreeMap::new();
        let now = Instant::now();
        let utc = chrono::Utc::now();
        for definition in definitions {
            let raw = request
                .fields
                .get(&definition.name)
                .ok_or("missing snapshot input")?;
            let value = match definition.data_type {
                crate::points::DataType::UInt32 => crate::points::Value::UInt32(
                    raw.as_u64()
                        .and_then(|v| u32::try_from(v).ok())
                        .ok_or("input must be UInt32")?,
                ),
                crate::points::DataType::String => {
                    crate::points::Value::String(raw.as_str().ok_or("input must be String")?.into())
                }
                _ => return Err("unsupported input type".into()),
            };
            layout
                .write_transmit_field(port, &definition.name, &value, &mut payload)
                .map_err(|e| e.to_string())?;
            fields.insert(definition.name, (value, now, utc));
        }
        let revision = slot
            .revision
            .checked_add(1)
            .ok_or("snapshot revision exhausted")?;
        slot.fields = fields;
        slot.field_payload = payload;
        slot.snapshot = None;
        slot.atomic_mode = true;
        slot.revision = revision;
        Ok(revision)
    }
    /// Client writes, including equal values, renew only this field's deadline.
    /// No manager subscription or read can keep a stopped producer alive.
    pub fn write_field(
        &self,
        port: &str,
        name: &str,
        value: crate::points::Value,
    ) -> Result<(), String> {
        let mut slots = self.0.lock().map_err(|_| "snapshot lock poisoned")?;
        let slot = slots.get_mut(port).ok_or("unknown port")?;
        if !slot.enabled || !slot.settings.transmit_enabled {
            return Err("transmission disabled".into());
        }
        if slot.atomic_mode {
            return Err("atomic snapshot active; submit a complete snapshot".into());
        }
        let layout =
            Layout::from_protocol(&slot.settings.transmit_protocol).ok_or("unsupported layout")?;
        if slot.field_payload.is_empty() {
            slot.field_payload = layout.empty_transmit_payload();
        }
        let revision = slot
            .revision
            .checked_add(1)
            .ok_or("snapshot revision exhausted")?;
        layout
            .write_transmit_field(port, name, &value, &mut slot.field_payload)
            .map_err(|e| e.to_string())?;
        slot.snapshot = None;
        slot.fields
            .insert(name.into(), (value, Instant::now(), chrono::Utc::now()));
        slot.revision = revision;
        Ok(())
    }
    pub fn field_sample(
        &self,
        port: &str,
        name: &str,
        data_type: crate::points::DataType,
    ) -> Result<crate::points::Sample, String> {
        let slots = self.0.lock().map_err(|_| "snapshot lock poisoned")?;
        let slot = slots.get(port).ok_or("unknown port")?;
        let sample = slot.fields.get(name);
        let quality = if !slot.enabled || !slot.settings.transmit_enabled {
            Quality::OutOfService
        } else if sample.is_none() {
            Quality::Waiting
        } else if sample.is_some_and(|(_, at, _)| {
            at.elapsed() >= Duration::from_millis(slot.settings.snapshot_max_age_ms)
        }) {
            Quality::NoCommunication
        } else {
            Quality::Good
        };
        Ok(crate::points::Sample {
            value: sample
                .map(|(v, _, _)| v.clone())
                .unwrap_or_else(|| data_type.zero()),
            source_timestamp: sample.map(|(_, _, at)| *at),
            quality,
            error: String::new(),
        })
    }
    pub fn current(&self, port: &str, now: Instant) -> Option<Vec<u8>> {
        let slots = self.0.lock().ok()?;
        let slot = slots.get(port)?;
        if !slot.enabled || !slot.settings.transmit_enabled {
            return None;
        }
        if !slot.fields.is_empty() {
            let layout = Layout::from_protocol(&slot.settings.transmit_protocol)?;
            if slot.fields.len() != layout.transmit_definitions(port).len()
                || slot.fields.values().any(|(_, at, _)| {
                    now.saturating_duration_since(*at)
                        >= Duration::from_millis(slot.settings.snapshot_max_age_ms)
                })
            {
                return None;
            }
            return Some(slot.field_payload.clone());
        }
        slot.snapshot
            .as_ref()
            .filter(|s| {
                now.saturating_duration_since(s.supplied)
                    < Duration::from_millis(slot.settings.snapshot_max_age_ms)
            })
            .map(|s| s.bytes.clone())
    }
    pub fn status(&self, port: &str) -> Result<String, String> {
        let slots = self.0.lock().map_err(|_| "snapshot lock poisoned")?;
        let slot = slots.get(port).ok_or("unknown synchronization port")?;
        let age = slot
            .snapshot
            .as_ref()
            .map(|s| s.supplied.elapsed().as_millis());
        let required = Layout::from_protocol(&slot.settings.transmit_protocol)
            .map(|l| l.transmit_definitions(port).len())
            .unwrap_or(0);
        let stale = slot
            .fields
            .values()
            .filter(|(_, at, _)| {
                at.elapsed() >= Duration::from_millis(slot.settings.snapshot_max_age_ms)
            })
            .count();
        let fresh = if slot.fields.is_empty() {
            age.is_some_and(|a| a < slot.settings.snapshot_max_age_ms as u128)
        } else {
            slot.fields.len() == required && stale == 0
        };
        Ok(serde_json::json!({"revision":slot.revision,"input_mode":if slot.atomic_mode {"atomic"} else {"individual"},"transmit_enabled":slot.settings.transmit_enabled,
            "snapshot_age_ms":age,"snapshot_fresh":fresh,"required_inputs":required,"received_inputs":slot.fields.len(),"stale_inputs":stale,
            "input_refresh_interval_ms":slot.settings.input_refresh_interval_ms,"input_timeout_ms":slot.settings.snapshot_max_age_ms,
            "complete_writes":slot.sent,"last_send_error":slot.error,
            "meaning":"完整写出不代表对方已接收或业务动作完成"}).to_string())
    }
    fn result(&self, port: &str, result: &Result<(), serial::WriteError>) {
        if let Ok(mut slots) = self.0.lock() {
            if let Some(slot) = slots.get_mut(port) {
                match result {
                    Ok(()) => {
                        slot.sent = slot.sent.saturating_add(1);
                        slot.error.clear();
                    }
                    Err(e) => slot.error = e.to_string(),
                }
            }
        }
    }
}

fn unique_fields<'de, D: serde::Deserializer<'de>>(
    deserializer: D,
) -> Result<BTreeMap<String, serde_json::Value>, D::Error> {
    struct Unique;
    impl<'de> serde::de::Visitor<'de> for Unique {
        type Value = BTreeMap<String, serde_json::Value>;
        fn expecting(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
            f.write_str("unique input fields")
        }
        fn visit_map<M: serde::de::MapAccess<'de>>(
            self,
            mut map: M,
        ) -> Result<Self::Value, M::Error> {
            let mut result = BTreeMap::new();
            while let Some((key, value)) = map.next_entry::<String, serde_json::Value>()? {
                if result.insert(key, value).is_some() {
                    return Err(serde::de::Error::custom("duplicate input field"));
                }
            }
            Ok(result)
        }
    }
    deserializer.deserialize_map(Unique)
}

/// Clock-injected scheduling for deterministic collision/timeout tests.
pub struct Schedule {
    due: Instant,
    last_rx: Instant,
    partial_since: Option<Instant>,
    period: Duration,
    gap: Duration,
    frame_timeout: Duration,
}
impl Schedule {
    pub fn new(settings: &Settings, frame_timeout: Duration, now: Instant) -> Self {
        Self {
            due: now + Duration::from_millis(settings.initial_delay_ms),
            last_rx: now,
            partial_since: None,
            period: Duration::from_millis(settings.send_interval_ms),
            gap: Duration::from_millis(settings.idle_gap_ms),
            frame_timeout,
        }
    }
    pub fn received(&mut self, now: Instant, partial: bool) {
        self.last_rx = now;
        if partial {
            self.partial_since.get_or_insert(now);
        } else {
            self.partial_since = None;
        }
    }
    pub fn partial_expired(&self, now: Instant) -> bool {
        self.partial_since
            .is_some_and(|t| now.saturating_duration_since(t) >= self.frame_timeout)
    }
    pub fn discard_partial(&mut self) {
        self.partial_since = None;
    }
    pub fn may_send(&self, now: Instant) -> bool {
        now >= self.due
            && self.partial_since.is_none()
            && now.saturating_duration_since(self.last_rx) >= self.gap
    }
    pub fn sent_or_skipped(&mut self, now: Instant) {
        self.due = now + self.period;
    }
}

/// Returns only on cancellation or transport failure. Noise/invalid snapshots
/// never refresh peer liveness and never cause outgoing command replays.
/// The result reports whether a valid peer snapshot was published on this
/// connection, so the supervisor can reset its reconnect backoff.
pub fn run_stream(
    transport: &mut dyn Transport,
    port: &PortConfig,
    task: &mut PollTask,
    store: &PointStore,
    cancel: &CancellationToken,
) -> Result<bool, crate::points::StoreError> {
    let settings = port
        .synchronization
        .as_ref()
        .expect("validated stream settings");
    let limits = Limits {
        frame: port.max_frame_bytes,
        payload: port.max_payload_bytes,
    };
    let mut decoder = FrameDecoder::new(limits).expect("validated frame limits");
    let mut schedule = Schedule::new(
        settings,
        Duration::from_millis(port.frame_timeout_ms),
        Instant::now(),
    );
    let mut last_valid = Instant::now();
    let mut timed_out = false;
    let mut received_valid = false;
    let mut last_frame_warning: Option<Instant> = None;
    let mut buffer = vec![0; port.max_frame_bytes.min(1024)];
    while !cancel.is_cancelled() {
        // Read before each send decision, including when a scheduled slot is due.
        match transport.read(&mut buffer, Duration::from_millis(10)) {
            Ok(n) if n > 0 && n <= buffer.len() => {
                let now = Instant::now();
                match decoder.feed(&buffer[..n]) {
                    Ok(frames) => {
                        report_frame_errors(
                            store,
                            &port.id,
                            &frames,
                            false,
                            &mut last_frame_warning,
                        )?;
                        if !frames.payloads.is_empty() {
                            schedule.discard_partial();
                        }
                        schedule.received(now, decoder.has_partial_frame());
                        for payload in frames.payloads {
                            match task.protocol.response(&task.config, &payload) {
                                Ok(Response::Matched(points)) => {
                                    store.attempt(&task.config.id)?;
                                    match store.publish(
                                        &task.config.id,
                                        &points,
                                        chrono::Utc::now(),
                                    ) {
                                        Ok(()) => {
                                            last_valid = now;
                                            timed_out = false;
                                            received_valid = true;
                                        }
                                        Err(e) => store.fail(
                                            &task.config.id,
                                            if timed_out {
                                                Quality::NoCommunication
                                            } else {
                                                Quality::Configuration
                                            },
                                            &e.to_string(),
                                        )?,
                                    }
                                }
                                Ok(Response::Ignore) => {}
                                Err(e) => store.fail(
                                    &task.config.id,
                                    if timed_out {
                                        Quality::NoCommunication
                                    } else {
                                        Quality::Configuration
                                    },
                                    &e.to_string(),
                                )?,
                            }
                        }
                    }
                    Err(e) => {
                        schedule.received(now, false);
                        store.fail(
                            &task.config.id,
                            if timed_out {
                                Quality::NoCommunication
                            } else {
                                Quality::Configuration
                            },
                            &e.to_string(),
                        )?;
                    }
                }
            }
            Err(e)
                if matches!(
                    e.kind(),
                    io::ErrorKind::TimedOut
                        | io::ErrorKind::WouldBlock
                        | io::ErrorKind::Interrupted
                ) => {}
            result => {
                store.port_state(
                    &port.id,
                    "Disconnected",
                    &format!("serial receive: {result:?}"),
                    false,
                )?;
                break;
            }
        }
        let now = Instant::now();
        if schedule.partial_expired(now) {
            let recovered = decoder.recover_expired();
            report_frame_errors(store, &port.id, &recovered, true, &mut last_frame_warning)?;
            for payload in recovered.payloads {
                match task.protocol.response(&task.config, &payload) {
                    Ok(Response::Matched(points)) => {
                        store.attempt(&task.config.id)?;
                        match store.publish(&task.config.id, &points, chrono::Utc::now()) {
                            Ok(()) => {
                                last_valid = now;
                                timed_out = false;
                                received_valid = true;
                            }
                            Err(e) => store.fail(
                                &task.config.id,
                                if timed_out {
                                    Quality::NoCommunication
                                } else {
                                    Quality::Configuration
                                },
                                &e.to_string(),
                            )?,
                        }
                    }
                    Ok(Response::Ignore) => {}
                    Err(e) => store.fail(
                        &task.config.id,
                        if timed_out {
                            Quality::NoCommunication
                        } else {
                            Quality::Configuration
                        },
                        &e.to_string(),
                    )?,
                }
            }
            schedule.discard_partial();
        }
        if !timed_out
            && now.saturating_duration_since(last_valid)
                >= Duration::from_millis(settings.receive_timeout_ms)
        {
            store.fail(
                &task.config.id,
                Quality::NoCommunication,
                "peer snapshot timeout",
            )?;
            timed_out = true;
        }
        if schedule.may_send(now) {
            if let Some(payload) = store.synchronization.current(&port.id, now) {
                let frame = crate::frame::encode(&payload, limits)
                    .map_err(|e| crate::points::StoreError::InvalidBatch(e.to_string()))?;
                let result = serial::write_once(
                    transport,
                    &frame,
                    now + Duration::from_millis(port.write_timeout_ms),
                    cancel,
                );
                store.synchronization.result(&port.id, &result);
                if let Err(error) = result {
                    store.port_state(&port.id, "Disconnected", &error.to_string(), false)?;
                    break;
                }
            }
            schedule.sent_or_skipped(Instant::now());
        }
    }
    Ok(received_valid)
}

fn report_frame_errors(
    store: &PointStore,
    port: &str,
    frames: &crate::frame::FeedResult,
    expired: bool,
    last_warning: &mut Option<Instant>,
) -> Result<(), crate::points::StoreError> {
    if frames.rejected_frames == 0 && frames.discarded_bytes == 0 && !expired {
        return Ok(());
    }
    store.record_frame_errors(
        port,
        frames.rejected_frames,
        frames.discarded_bytes,
        expired,
    )?;
    let now = Instant::now();
    if last_warning.is_none_or(|at| now.duration_since(at) >= Duration::from_secs(10)) {
        let totals = store.port(port)?;
        log::warn!(
            "port={} frame errors (cumulative): rejected={} discarded_bytes={} partial_timeouts={}",
            port,
            totals.rejected_frames,
            totals.discarded_bytes,
            totals.partial_timeouts
        );
        *last_warning = Some(now);
    }
    Ok(())
}
