//! Physical-gateway operations. Desired acquisition settings are durable;
//! applied state is reported by the serial worker, never inferred from enqueue.

pub mod files;

use std::collections::{BTreeMap, HashMap};
use std::fmt;
use std::fs::{self, OpenOptions};
use std::io::{self, Read, Write};
use std::path::{Path, PathBuf};
use std::sync::{mpsc, Arc, Mutex, Weak};
use std::time::Duration;

use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use sha2::{Digest, Sha256};
use tokio_util::sync::CancellationToken;

use crate::model::command::{
    ControlReceipt, DeviceReadSetting, PortStatus, QueryStationVersion, StationModuleTypeSetting,
};
use crate::model::{display_version, Command, CommandType, ModuleType, StationVersionRecord};
use crate::protocol::StationVersionResponse;
use crate::serial::port_config::SerialPortConfig;

pub const MAX_REQUEST_BYTES: usize = 13 * 1024 * 1024;
const MAX_STATE_BYTES: u64 = 2 * 1024 * 1024;
const MAX_STATION_STATE_BYTES: u64 = 8 * 1024 * 1024;
const CONTROL_WAIT: Duration = Duration::from_secs(2);

#[derive(Debug, Clone, Serialize)]
pub struct OperationError {
    pub code: String,
    pub message: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub current_revision: Option<String>,
}

impl OperationError {
    pub fn new(code: &str, message: impl Into<String>) -> Self {
        Self {
            code: code.into(),
            message: message.into(),
            current_revision: None,
        }
    }

    pub fn with_revision(mut self, revision: String) -> Self {
        self.current_revision = Some(revision);
        self
    }
}

impl fmt::Display for OperationError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "{}: {}", self.code, self.message)
    }
}
impl std::error::Error for OperationError {}

pub fn capabilities() -> Value {
    json!({"schema_version":1,"features":["acquisition","configuration","logs","station_versions"],
        "max_config_bytes":2_097_152,"max_log_bytes":65_536,"configuration_apply":"restart"})
}

pub fn software_version() -> &'static str {
    env!("CARGO_PKG_VERSION")
}

pub fn build_commit() -> &'static str {
    option_env!("RCD_BUILD_COMMIT").unwrap_or("unknown")
}

#[derive(Debug, Clone, Serialize)]
pub struct DeviceSnapshot {
    pub device_id: String,
    pub name: String,
    pub read_enabled: bool,
    pub applied_read_enabled: bool,
    pub communication_ok: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub last_data_at: Option<String>,
    #[serde(flatten)]
    pub station_version: StationVersionRecord,
}

#[derive(Debug, Clone, Serialize)]
pub struct PortSnapshot {
    pub port_id: String,
    pub port_name: String,
    pub enabled: bool,
    pub applied_enabled: bool,
    pub open: bool,
    pub devices: Vec<DeviceSnapshot>,
    pub pending_commands: usize,
    pub control_pending: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub last_control_error: Option<String>,
}

#[derive(Clone, Default, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
struct StoredPort {
    enabled: bool,
    #[serde(default)]
    device_read_enabled: BTreeMap<String, bool>,
}

#[derive(Clone, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
struct StoredState {
    schema_version: u32,
    revision: String,
    ports: BTreeMap<String, StoredPort>,
}

#[derive(Clone, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
struct StoredStationEntry {
    topology_fingerprint: String,
    record: StationVersionRecord,
}

#[derive(Clone, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
struct StoredStationState {
    schema_version: u32,
    revision: String,
    ports: BTreeMap<String, BTreeMap<String, StoredStationEntry>>,
}

#[derive(Debug, Clone, Serialize)]
pub struct StationVersionsSnapshot {
    pub software_version: String,
    pub build_commit: String,
    pub station_revision: String,
    pub ports: Vec<PortSnapshot>,
}

struct State {
    saved: StoredState,
    station_saved: StoredStationState,
    ports: BTreeMap<String, PortSnapshot>,
    pending: BTreeMap<String, u64>,
}

struct Inner {
    state: Mutex<State>,
    senders: Mutex<HashMap<String, mpsc::Sender<Command>>>,
    files: Mutex<files::FileRepository>,
    state_path: PathBuf,
    station_state_path: PathBuf,
    shutdown: CancellationToken,
}

#[derive(Clone)]
pub struct Operations {
    inner: Arc<Inner>,
}

/// A command reserves its physical port from admission until the worker has
/// finished it (including ACK retries). Clones share one reservation.
#[derive(Clone)]
pub struct CommandPermit(Arc<PermitInner>);
struct PermitInner {
    owner: Weak<Inner>,
    port_id: String,
}

impl fmt::Debug for CommandPermit {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_tuple("CommandPermit")
            .field(&self.0.port_id)
            .finish()
    }
}

impl Drop for PermitInner {
    fn drop(&mut self) {
        if let Some(owner) = self.owner.upgrade() {
            let mut state = owner.state.lock().unwrap_or_else(|e| e.into_inner());
            if let Some(port) = state.ports.get_mut(&self.port_id) {
                port.pending_commands = port.pending_commands.saturating_sub(1);
            }
        }
    }
}

impl Operations {
    pub fn new(
        config_dir: PathBuf,
        log_dir: PathBuf,
        log_basename: String,
        ports: &[SerialPortConfig],
    ) -> io::Result<Self> {
        Self::new_with_shutdown(
            config_dir,
            log_dir,
            log_basename,
            ports,
            CancellationToken::new(),
        )
    }

    /// Use the server's cancellation token to close command admission as soon
    /// as shutdown starts, without persisting any acquisition setting change.
    pub fn new_with_shutdown(
        config_dir: PathBuf,
        log_dir: PathBuf,
        log_basename: String,
        ports: &[SerialPortConfig],
        shutdown: CancellationToken,
    ) -> io::Result<Self> {
        let config_dir = fs::canonicalize(config_dir)?;
        if !config_dir.is_dir() || config_dir.parent().is_none() {
            return Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                "invalid operations configuration directory",
            ));
        }
        let state_path = config_dir.join("operations.json");
        let station_state_path = config_dir.join("station_versions.json");
        let mut saved = read_state(&state_path)?;
        let mut station_saved = read_station_state(&station_state_path)?;
        let station_state_before_reconcile =
            serde_json::to_vec(&station_saved).map_err(io::Error::other)?;
        let mut snapshots = BTreeMap::new();
        let mut configured_station_keys = BTreeMap::<String, BTreeMap<String, String>>::new();
        for port in ports {
            if snapshots.contains_key(&port.port_number) {
                return Err(io::Error::new(
                    io::ErrorKind::InvalidData,
                    "duplicate operations port",
                ));
            }
            let desired = saved
                .ports
                .entry(port.port_number.clone())
                .or_insert_with(|| StoredPort {
                    enabled: true,
                    device_read_enabled: BTreeMap::new(),
                });
            let mut devices = BTreeMap::new();
            for device in &port.devices {
                let id = device.config.device_id.to_string();
                let topology_fingerprint = station_topology_fingerprint(port, device);
                let enabled = *desired
                    .device_read_enabled
                    .entry(id.clone())
                    .or_insert(true);
                let stored = station_saved
                    .ports
                    .entry(port.port_number.clone())
                    .or_default()
                    .entry(id.clone())
                    .or_insert_with(|| StoredStationEntry {
                        topology_fingerprint: topology_fingerprint.clone(),
                        record: StationVersionRecord::default(),
                    });
                if stored.topology_fingerprint != topology_fingerprint {
                    let configured = stored.record.configured_module_type;
                    stored.topology_fingerprint = topology_fingerprint.clone();
                    stored.record.reset_for_configured_type(configured);
                    stored.record.stale = false;
                }
                if stored.record.version_state == "querying" {
                    stored.record.version_state = "failed".into();
                    stored.record.last_version_error =
                        "previous version query was interrupted by process shutdown".into();
                    stored.record.consecutive_failures =
                        stored.record.consecutive_failures.saturating_add(1);
                    stored.record.stale = stored.record.has_last_version();
                }
                configured_station_keys
                    .entry(port.port_number.clone())
                    .or_default()
                    .insert(id.clone(), topology_fingerprint);
                if devices
                    .insert(
                        id.clone(),
                        DeviceSnapshot {
                            device_id: id,
                            name: format!("设备 {}", device.config.device_id),
                            read_enabled: enabled,
                            applied_read_enabled: false,
                            communication_ok: false,
                            last_data_at: None,
                            station_version: stored.record.clone(),
                        },
                    )
                    .is_some()
                {
                    return Err(io::Error::new(
                        io::ErrorKind::InvalidData,
                        "duplicate operations device",
                    ));
                }
            }
            snapshots.insert(
                port.port_number.clone(),
                PortSnapshot {
                    port_id: port.port_number.clone(),
                    port_name: port.port_number.clone(),
                    enabled: desired.enabled,
                    applied_enabled: false,
                    open: false,
                    devices: devices.into_values().collect(),
                    pending_commands: 0,
                    control_pending: false,
                    last_control_error: None,
                },
            );
        }
        station_saved.ports.retain(|port_id, devices| {
            let Some(configured) = configured_station_keys.get(port_id) else {
                return false;
            };
            devices.retain(|device_id, _| configured.contains_key(device_id));
            true
        });
        if serde_json::to_vec(&station_saved).map_err(io::Error::other)?
            != station_state_before_reconcile
        {
            station_saved.revision = increment_persisted_revision(&station_saved.revision)?;
            persist_station_state(&station_state_path, &station_saved)?;
        }
        let repository = files::FileRepository::new(config_dir, log_dir, log_basename)?;
        Ok(Self {
            inner: Arc::new(Inner {
                state: Mutex::new(State {
                    saved,
                    station_saved,
                    ports: snapshots,
                    pending: BTreeMap::new(),
                }),
                senders: Mutex::new(HashMap::new()),
                files: Mutex::new(repository),
                state_path,
                station_state_path,
                shutdown,
            }),
        })
    }

    pub fn register_worker(&self, port_id: &str, sender: mpsc::Sender<Command>) {
        self.inner
            .senders
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .insert(port_id.into(), sender);
    }

    pub fn initial_port_enabled(&self, port_id: &str) -> bool {
        self.inner
            .state
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .ports
            .get(port_id)
            .is_some_and(|p| p.enabled)
    }

    pub fn initial_device_read_enabled(&self, port_id: &str, device_id: u8) -> bool {
        self.inner
            .state
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .ports
            .get(port_id)
            .and_then(|p| {
                p.devices
                    .iter()
                    .find(|d| d.device_id == device_id.to_string())
            })
            .is_some_and(|d| d.read_enabled)
    }

    pub fn initial_module_type(&self, port_id: &str, device_id: u8) -> ModuleType {
        self.inner
            .state
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .ports
            .get(port_id)
            .and_then(|port| {
                port.devices
                    .iter()
                    .find(|device| device.device_id == device_id.to_string())
            })
            .map(|device| device.station_version.configured_module_type)
            .unwrap_or_default()
    }

    /// 启动时只安排明确配置为国产且未处于“探测到俄供”冲突的站点。
    pub fn startup_version_query_devices(&self, port_id: &str) -> Vec<u8> {
        self.inner
            .state
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .ports
            .get(port_id)
            .into_iter()
            .flat_map(|port| &port.devices)
            .filter(|device| {
                device.station_version.configured_module_type == ModuleType::Domestic
                    && !(device.station_version.detected_module_type == ModuleType::LegacyRussian
                        && device.station_version.type_mismatch)
            })
            .filter_map(|device| device.device_id.parse().ok())
            .collect()
    }

    pub fn status(&self) -> Value {
        let state = self.inner.state.lock().unwrap_or_else(|e| e.into_inner());
        json!({
            "schema_version":1,
            "revision":state.saved.revision,
            "software_version":software_version(),
            "build_commit":build_commit(),
            "station_revision":state.station_saved.revision,
            "ports":state.ports.values().collect::<Vec<_>>()
        })
    }

    pub fn station_versions_snapshot(&self) -> StationVersionsSnapshot {
        let state = self.inner.state.lock().unwrap_or_else(|e| e.into_inner());
        StationVersionsSnapshot {
            software_version: software_version().into(),
            build_commit: build_commit().into(),
            station_revision: state.station_saved.revision.clone(),
            ports: state.ports.values().cloned().collect(),
        }
    }

    pub fn execute_json(&self, request: &str) -> String {
        let result = if request.len() > MAX_REQUEST_BYTES {
            Err(OperationError::new(
                "invalid_request",
                "operations request is too large",
            ))
        } else {
            serde_json::from_str::<Value>(request)
                .map_err(|_| OperationError::new("invalid_request", "invalid operations JSON"))
                .and_then(|request| self.execute(&request))
        };
        match result {
            Ok(value) => json!({"ok":true,"result":value}).to_string(),
            Err(error) => json!({"ok":false,"error":error}).to_string(),
        }
    }

    pub fn execute(&self, request: &Value) -> Result<Value, OperationError> {
        let action = string_arg(request, "action")?;
        match action {
            "status" => Ok(self.status()),
            "set_port_enabled" | "set_device_read_enabled" => {
                self.set_control(request, action, CONTROL_WAIT)
            }
            "set_device_module_type" => self.set_device_module_type(request, CONTROL_WAIT),
            "query_device_version" => self.query_device_version(request, CONTROL_WAIT),
            "list_configs" | "read_config" | "validate_config" | "write_config" | "list_logs"
            | "read_log" => self
                .inner
                .files
                .lock()
                .unwrap_or_else(|e| e.into_inner())
                .handle(request),
            _ => Err(OperationError::new(
                "invalid_request",
                "unsupported operations action",
            )),
        }
    }

    fn set_device_module_type(
        &self,
        request: &Value,
        wait: Duration,
    ) -> Result<Value, OperationError> {
        let port_id = string_arg(request, "port_id")?;
        let device_id_text = string_arg(request, "device_id")?;
        let device_id: u8 = device_id_text
            .parse()
            .map_err(|_| OperationError::new("invalid_request", "invalid device ID"))?;
        let module_type: ModuleType = string_arg(request, "module_type")?
            .parse()
            .map_err(|message| OperationError::new("invalid_request", message))?;
        let expected_revision = string_arg(request, "expected_revision")?;
        let expected_station_revision = string_arg(request, "expected_station_revision")?;
        let sender = self.worker_sender(port_id)?;
        let (reply, receiver) = mpsc::channel();
        let station_revision;
        {
            let mut state = self.inner.state.lock().unwrap_or_else(|e| e.into_inner());
            if self.inner.shutdown.is_cancelled() {
                return Err(
                    OperationError::new("operation_failed", "gateway is shutting down")
                        .with_revision(state.station_saved.revision.clone()),
                );
            }
            validate_station_request(
                &state,
                expected_revision,
                expected_station_revision,
                port_id,
                device_id_text,
                false,
            )?;
            let current = state
                .ports
                .get(port_id)
                .and_then(|port| {
                    port.devices
                        .iter()
                        .find(|device| device.device_id == device_id_text)
                })
                .unwrap()
                .station_version
                .configured_module_type;
            if current == module_type {
                drop(state);
                let mut snapshot = self.status();
                snapshot["status"] = json!("Unchanged");
                return Ok(snapshot);
            }

            let mut next = state.station_saved.clone();
            station_revision = increment_revision(&next.revision)?;
            next.revision = station_revision.to_string();
            let stored = next
                .ports
                .get_mut(port_id)
                .and_then(|devices| devices.get_mut(device_id_text))
                .expect("validated station exists");
            stored.record.reset_for_configured_type(module_type);
            persist_station_state(&self.inner.station_state_path, &next).map_err(|error| {
                OperationError::new(
                    "persistence_failed",
                    format!("could not save station version settings: {error}"),
                )
            })?;
            state.station_saved = next;
            let port = state.ports.get_mut(port_id).unwrap();
            port.devices
                .iter_mut()
                .find(|device| device.device_id == device_id_text)
                .unwrap()
                .station_version
                .reset_for_configured_type(module_type);
            port.control_pending = true;
            port.last_control_error = None;
            state.pending.insert(port_id.into(), station_revision);
            if sender
                .send(Command {
                    com: port_id.into(),
                    command: CommandType::StationModuleTypeSetting(StationModuleTypeSetting {
                        device_id,
                        module_type,
                    }),
                    ack_id: None,
                    expires_at: None,
                    control_receipt: Some(ControlReceipt {
                        revision: station_revision,
                        reply,
                    }),
                    permit: None,
                })
                .is_err()
            {
                state.pending.remove(port_id);
                let port = state.ports.get_mut(port_id).unwrap();
                port.control_pending = false;
                port.last_control_error = Some(
                    "worker channel closed; module type is saved and will apply on restart".into(),
                );
                return Err(OperationError::new(
                    "operation_failed",
                    "module type saved, but serial worker is unavailable",
                )
                .with_revision(station_revision.to_string()));
            }
        }
        self.wait_station_control(receiver, wait, station_revision)
    }

    fn query_device_version(
        &self,
        request: &Value,
        wait: Duration,
    ) -> Result<Value, OperationError> {
        let port_id = string_arg(request, "port_id")?;
        let device_id_text = string_arg(request, "device_id")?;
        let device_id: u8 = device_id_text
            .parse()
            .map_err(|_| OperationError::new("invalid_request", "invalid device ID"))?;
        let expected_revision = string_arg(request, "expected_revision")?;
        let expected_station_revision = string_arg(request, "expected_station_revision")?;
        let sender = self.worker_sender(port_id)?;
        let (reply, receiver) = mpsc::channel();
        let station_revision;
        {
            let mut state = self.inner.state.lock().unwrap_or_else(|e| e.into_inner());
            if self.inner.shutdown.is_cancelled() {
                return Err(
                    OperationError::new("operation_failed", "gateway is shutting down")
                        .with_revision(state.station_saved.revision.clone()),
                );
            }
            validate_station_request(
                &state,
                expected_revision,
                expected_station_revision,
                port_id,
                device_id_text,
                true,
            )?;
            station_revision = mark_station_query_pending(
                &mut state,
                &self.inner.station_state_path,
                port_id,
                device_id_text,
            )?;
            let port = state.ports.get_mut(port_id).unwrap();
            port.control_pending = true;
            port.last_control_error = None;
            state.pending.insert(port_id.into(), station_revision);
            if sender
                .send(Command {
                    com: port_id.into(),
                    command: CommandType::QueryStationVersion(QueryStationVersion { device_id }),
                    ack_id: None,
                    expires_at: None,
                    control_receipt: Some(ControlReceipt {
                        revision: station_revision,
                        reply,
                    }),
                    permit: None,
                })
                .is_err()
            {
                state.pending.remove(port_id);
                let port = state.ports.get_mut(port_id).unwrap();
                port.control_pending = false;
                port.last_control_error = Some("serial worker channel closed".into());
                drop(state);
                let _ = self.report_station_query_result(
                    port_id,
                    device_id,
                    Err("serial worker is unavailable".into()),
                );
                return Err(OperationError::new(
                    "operation_failed",
                    "serial worker is unavailable",
                )
                .with_revision(station_revision.to_string()));
            }
        }
        self.wait_station_control(receiver, wait, station_revision)
    }

    fn worker_sender(&self, port_id: &str) -> Result<mpsc::Sender<Command>, OperationError> {
        self.inner
            .senders
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .get(port_id)
            .cloned()
            .ok_or_else(|| OperationError::new("operation_failed", "serial worker is unavailable"))
    }

    fn wait_station_control(
        &self,
        receiver: mpsc::Receiver<Result<(), String>>,
        wait: Duration,
        station_revision: u64,
    ) -> Result<Value, OperationError> {
        let status = match receiver.recv_timeout(wait) {
            Ok(Ok(())) => "Applied",
            Ok(Err(message)) => {
                return Err(OperationError::new("operation_failed", message)
                    .with_revision(station_revision.to_string()))
            }
            Err(mpsc::RecvTimeoutError::Timeout) => "Pending",
            Err(mpsc::RecvTimeoutError::Disconnected) => {
                return Err(OperationError::new(
                    "operation_failed",
                    "worker ended without confirming station operation",
                )
                .with_revision(station_revision.to_string()))
            }
        };
        let mut snapshot = self.status();
        snapshot["status"] = json!(status);
        snapshot["station_operation_revision"] = json!(station_revision.to_string());
        Ok(snapshot)
    }

    /// Worker-only start of a throttled startup/re-enable version probe.
    pub(crate) fn begin_automatic_station_query(
        &self,
        port_id: &str,
        device_id: u8,
    ) -> Result<bool, String> {
        let device_id_text = device_id.to_string();
        let mut state = self.inner.state.lock().unwrap_or_else(|e| e.into_inner());
        if self.inner.shutdown.is_cancelled() {
            return Ok(false);
        }
        let Some(port) = state.ports.get(port_id) else {
            return Ok(false);
        };
        let Some(device) = port
            .devices
            .iter()
            .find(|device| device.device_id == device_id_text)
        else {
            return Ok(false);
        };
        if !port.enabled
            || !port.applied_enabled
            || !port.open
            || !device.read_enabled
            || !device.applied_read_enabled
            || device.station_version.configured_module_type != ModuleType::Domestic
            || device.station_version.version_state == "querying"
            || (device.station_version.detected_module_type == ModuleType::LegacyRussian
                && device.station_version.type_mismatch)
        {
            return Ok(false);
        }
        mark_station_query_pending(
            &mut state,
            &self.inner.station_state_path,
            port_id,
            &device_id_text,
        )
        .map(|_| true)
        .map_err(|error| error.to_string())
    }

    /// Records a dedicated query result without touching normal acquisition health/counters.
    pub(crate) fn report_station_query_result(
        &self,
        port_id: &str,
        device_id: u8,
        result: Result<StationVersionResponse, String>,
    ) -> Result<(), String> {
        let device_id_text = device_id.to_string();
        let mut state = self.inner.state.lock().unwrap_or_else(|e| e.into_inner());
        let mut next = state.station_saved.clone();
        next.revision = increment_revision(&next.revision)
            .map_err(|error| error.to_string())?
            .to_string();
        let stored = next
            .ports
            .get_mut(port_id)
            .and_then(|devices| devices.get_mut(&device_id_text))
            .ok_or_else(|| "station disappeared while query was running".to_string())?;
        apply_station_query_result(&mut stored.record, result);
        let record = stored.record.clone();
        persist_station_state(&self.inner.station_state_path, &next)
            .map_err(|error| format!("could not persist station version result: {error}"))?;
        state.station_saved = next;
        if let Some(device) = state.ports.get_mut(port_id).and_then(|port| {
            port.devices
                .iter_mut()
                .find(|device| device.device_id == device_id_text)
        }) {
            device.station_version = record;
        }
        Ok(())
    }

    pub(crate) fn fail_inflight_station_queries(&self, port_id: &str, message: &str) {
        let mut state = self.inner.state.lock().unwrap_or_else(|e| e.into_inner());
        let mut next = state.station_saved.clone();
        let Some(stations) = next.ports.get_mut(port_id) else {
            return;
        };
        let mut changed = BTreeMap::new();
        for (device_id, station) in stations {
            if station.record.version_state == "querying" {
                apply_station_query_result(&mut station.record, Err(message.into()));
                changed.insert(device_id.clone(), station.record.clone());
            }
        }
        if changed.is_empty() {
            return;
        }
        let Ok(revision) = increment_persisted_revision(&next.revision) else {
            return;
        };
        next.revision = revision;
        if let Err(error) = persist_station_state(&self.inner.station_state_path, &next) {
            log::error!(
                "[版本查询中断状态保存失败] port={} error={:?}",
                port_id,
                error
            );
            return;
        }
        state.station_saved = next;
        if let Some(port) = state.ports.get_mut(port_id) {
            for device in &mut port.devices {
                if let Some(record) = changed.get(&device.device_id) {
                    device.station_version = record.clone();
                }
            }
        }
    }

    pub(crate) fn admit_command(&self, port_id: &str) -> Result<CommandPermit, OperationError> {
        let mut state = self.inner.state.lock().unwrap_or_else(|e| e.into_inner());
        if self.inner.shutdown.is_cancelled() {
            return Err(
                OperationError::new("operation_failed", "gateway is shutting down")
                    .with_revision(state.saved.revision.clone()),
            );
        }
        let port = state
            .ports
            .get_mut(port_id)
            .ok_or_else(|| OperationError::new("not_found", "unknown port"))?;
        if port.control_pending {
            return Err(OperationError::new("busy", "port control is still pending"));
        }
        if !port.enabled || !port.applied_enabled {
            return Err(OperationError::new(
                "operation_failed",
                "port is disabled or not yet applied",
            ));
        }
        port.pending_commands += 1;
        Ok(CommandPermit(Arc::new(PermitInner {
            owner: Arc::downgrade(&self.inner),
            port_id: port_id.into(),
        })))
    }

    fn set_control(
        &self,
        request: &Value,
        action: &str,
        wait: Duration,
    ) -> Result<Value, OperationError> {
        let port_id = string_arg(request, "port_id")?;
        let device_id = if action == "set_device_read_enabled" {
            Some(string_arg(request, "device_id")?)
        } else {
            None
        };
        let enabled = request
            .get("enabled")
            .and_then(Value::as_bool)
            .ok_or_else(|| OperationError::new("invalid_request", "enabled must be Boolean"))?;
        let expected = string_arg(request, "expected_revision")?;
        let sender = self
            .inner
            .senders
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .get(port_id)
            .cloned()
            .ok_or_else(|| {
                OperationError::new("operation_failed", "serial worker is unavailable")
            })?;
        let (reply, receiver) = mpsc::channel();
        let revision;
        {
            // This lock is shared with command admission. It is released before
            // any acknowledgement wait; the worker needs it to publish state.
            let mut state = self.inner.state.lock().unwrap_or_else(|e| e.into_inner());
            if self.inner.shutdown.is_cancelled() {
                return Err(
                    OperationError::new("operation_failed", "gateway is shutting down")
                        .with_revision(state.saved.revision.clone()),
                );
            }
            if expected != state.saved.revision {
                return Err(
                    OperationError::new("conflict", "operations revision changed")
                        .with_revision(state.saved.revision.clone()),
                );
            }
            let port = state
                .ports
                .get(port_id)
                .ok_or_else(|| OperationError::new("not_found", "unknown port"))?;
            if port.control_pending || port.pending_commands != 0 {
                return Err(OperationError::new(
                    "busy",
                    "port has a pending command or control operation",
                )
                .with_revision(state.saved.revision.clone()));
            }
            let command = if let Some(device_id) = device_id {
                if !port.devices.iter().any(|d| d.device_id == device_id) {
                    return Err(OperationError::new("not_found", "unknown device"));
                }
                CommandType::DeviceReadSetting(DeviceReadSetting {
                    device_id: device_id
                        .parse()
                        .map_err(|_| OperationError::new("invalid_request", "invalid device ID"))?,
                    read_enabled: enabled,
                })
            } else {
                CommandType::PortStatus(PortStatus {
                    device_status: enabled,
                })
            };
            revision = state
                .saved
                .revision
                .parse::<u64>()
                .unwrap()
                .checked_add(1)
                .ok_or_else(|| OperationError::new("operation_failed", "revision exhausted"))?;
            let mut next = state.saved.clone();
            next.revision = revision.to_string();
            let stored = next.ports.get_mut(port_id).unwrap();
            if let Some(device_id) = device_id {
                stored.device_read_enabled.insert(device_id.into(), enabled);
            } else {
                stored.enabled = enabled;
            }
            persist_state(&self.inner.state_path, &next).map_err(|e| {
                OperationError::new(
                    "persistence_failed",
                    format!("could not save acquisition settings: {e}"),
                )
            })?;
            state.saved = next;
            let port = state.ports.get_mut(port_id).unwrap();
            if let Some(device_id) = device_id {
                port.devices
                    .iter_mut()
                    .find(|d| d.device_id == device_id)
                    .unwrap()
                    .read_enabled = enabled;
            } else {
                port.enabled = enabled;
            }
            port.control_pending = true;
            port.last_control_error = None;
            state.pending.insert(port_id.into(), revision);
            if sender
                .send(Command {
                    com: port_id.into(),
                    command,
                    ack_id: None,
                    expires_at: None,
                    control_receipt: Some(ControlReceipt { revision, reply }),
                    permit: None,
                })
                .is_err()
            {
                state.pending.remove(port_id);
                let port = state.ports.get_mut(port_id).unwrap();
                port.control_pending = false;
                port.last_control_error =
                    Some("worker channel closed; desired setting is saved but not applied".into());
                return Err(OperationError::new("operation_failed", "desired setting saved, worker unavailable; refresh status before further action").with_revision(revision.to_string()));
            }
        }
        let status = match receiver.recv_timeout(wait) {
            Ok(Ok(())) => "Applied",
            Ok(Err(message)) => {
                return Err(OperationError::new("operation_failed", message)
                    .with_revision(revision.to_string()))
            }
            Err(mpsc::RecvTimeoutError::Timeout) => "Pending",
            Err(mpsc::RecvTimeoutError::Disconnected) => {
                return Err(OperationError::new(
                    "operation_failed",
                    "worker ended without confirming; desired setting remains saved",
                )
                .with_revision(revision.to_string()))
            }
        };
        let mut snapshot = self.status();
        snapshot["status"] = json!(status);
        snapshot["operation_revision"] = json!(revision.to_string());
        Ok(snapshot)
    }

    /// Called only by the owning worker, after touching the actual port/state.
    pub(crate) fn report_runtime(
        &self,
        port_id: &str,
        enabled: bool,
        open: bool,
        devices: impl IntoIterator<Item = (u8, bool, bool, Option<String>)>,
    ) {
        let mut state = self.inner.state.lock().unwrap_or_else(|e| e.into_inner());
        let persisted_stale = state
            .station_saved
            .ports
            .get(port_id)
            .map(|stations| {
                stations
                    .iter()
                    .map(|(device_id, station)| {
                        (
                            device_id.clone(),
                            (station.record.stale, station.record.has_last_version()),
                        )
                    })
                    .collect::<BTreeMap<_, _>>()
            })
            .unwrap_or_default();
        if let Some(port) = state.ports.get_mut(port_id) {
            port.applied_enabled = enabled;
            port.open = open;
            for (id, read_enabled, communication_ok, last_data_at) in devices {
                if let Some(device) = port
                    .devices
                    .iter_mut()
                    .find(|d| d.device_id == id.to_string())
                {
                    device.applied_read_enabled = read_enabled;
                    device.communication_ok = communication_ok;
                    device.last_data_at = last_data_at;
                    let (saved_stale, has_version) = persisted_stale
                        .get(&device.device_id)
                        .copied()
                        .unwrap_or_default();
                    device.station_version.stale =
                        saved_stale || (!communication_ok && has_version);
                }
            }
        }
    }

    pub(crate) fn complete_control(
        &self,
        port_id: &str,
        revision: u64,
        outcome: &Result<(), String>,
    ) {
        let mut state = self.inner.state.lock().unwrap_or_else(|e| e.into_inner());
        if state.pending.get(port_id) == Some(&revision) {
            state.pending.remove(port_id);
            if let Some(port) = state.ports.get_mut(port_id) {
                port.control_pending = false;
                port.last_control_error = outcome.as_ref().err().cloned();
            }
        }
    }

    pub(crate) fn control_is_current(&self, port_id: &str, revision: u64) -> bool {
        self.inner
            .state
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .pending
            .get(port_id)
            == Some(&revision)
    }

    pub(crate) fn fail_pending_controls(&self, port_id: &str, message: &str) {
        let mut state = self.inner.state.lock().unwrap_or_else(|e| e.into_inner());
        state.pending.remove(port_id);
        if let Some(port) = state.ports.get_mut(port_id) {
            port.control_pending = false;
            port.last_control_error = Some(message.into());
        }
    }

    pub(crate) fn worker_stopped(&self, port_id: &str) {
        let mut state = self.inner.state.lock().unwrap_or_else(|e| e.into_inner());
        state.pending.remove(port_id);
        if let Some(port) = state.ports.get_mut(port_id) {
            port.applied_enabled = false;
            port.open = false;
            port.control_pending = false;
            port.last_control_error = Some("serial worker stopped".into());
            for device in &mut port.devices {
                device.communication_ok = false;
                device.applied_read_enabled = false;
                if device.station_version.has_last_version() {
                    device.station_version.stale = true;
                }
            }
        }
    }
}

fn validate_station_request(
    state: &State,
    expected_revision: &str,
    expected_station_revision: &str,
    port_id: &str,
    device_id: &str,
    query: bool,
) -> Result<(), OperationError> {
    if expected_revision != state.saved.revision {
        return Err(
            OperationError::new("conflict", "operations revision changed")
                .with_revision(state.saved.revision.clone()),
        );
    }
    if expected_station_revision != state.station_saved.revision {
        return Err(
            OperationError::new("conflict", "station version revision changed")
                .with_revision(state.station_saved.revision.clone()),
        );
    }
    let port = state
        .ports
        .get(port_id)
        .ok_or_else(|| OperationError::new("not_found", "unknown port"))?;
    if port.control_pending || port.pending_commands != 0 {
        return Err(
            OperationError::new("busy", "port has a pending command or control operation")
                .with_revision(state.station_saved.revision.clone()),
        );
    }
    let device = port
        .devices
        .iter()
        .find(|device| device.device_id == device_id)
        .ok_or_else(|| OperationError::new("not_found", "unknown device"))?;
    if query {
        if device.station_version.configured_module_type != ModuleType::Domestic {
            return Err(OperationError::new(
                "conflict",
                "version query is allowed only for a station configured as domestic",
            )
            .with_revision(state.station_saved.revision.clone()));
        }
        if !port.enabled || !port.applied_enabled {
            return Err(OperationError::new(
                "conflict",
                "port acquisition is disabled or not yet applied",
            ));
        }
        if !port.open {
            return Err(OperationError::new(
                "operation_failed",
                "serial port is offline",
            ));
        }
        if !device.read_enabled || !device.applied_read_enabled {
            return Err(OperationError::new(
                "conflict",
                "device acquisition is disabled or not yet applied",
            ));
        }
    }
    Ok(())
}

fn increment_revision(current: &str) -> Result<u64, OperationError> {
    current
        .parse::<u64>()
        .ok()
        .and_then(|revision| revision.checked_add(1))
        .ok_or_else(|| OperationError::new("operation_failed", "revision exhausted"))
}

fn mark_station_query_pending(
    state: &mut State,
    path: &Path,
    port_id: &str,
    device_id: &str,
) -> Result<u64, OperationError> {
    let revision = increment_revision(&state.station_saved.revision)?;
    let mut next = state.station_saved.clone();
    next.revision = revision.to_string();
    let stored = next
        .ports
        .get_mut(port_id)
        .and_then(|devices| devices.get_mut(device_id))
        .ok_or_else(|| OperationError::new("not_found", "unknown station"))?;
    stored.record.version_state = "querying".into();
    stored.record.last_version_attempt_at = Some(chrono::Utc::now().to_rfc3339());
    stored.record.last_version_error.clear();
    let record = stored.record.clone();
    persist_station_state(path, &next).map_err(|error| {
        OperationError::new(
            "persistence_failed",
            format!("could not save station query state: {error}"),
        )
    })?;
    state.station_saved = next;
    if let Some(device) = state.ports.get_mut(port_id).and_then(|port| {
        port.devices
            .iter_mut()
            .find(|device| device.device_id == device_id)
    }) {
        device.station_version = record;
    }
    Ok(revision)
}

fn apply_station_query_result(
    record: &mut StationVersionRecord,
    result: Result<StationVersionResponse, String>,
) {
    let now = chrono::Utc::now().to_rfc3339();
    match result {
        Ok(StationVersionResponse::Domestic {
            payload,
            communication_module_version,
            plc_version,
        }) => {
            record.detected_module_type = ModuleType::Domestic;
            record.type_mismatch = record.configured_module_type != ModuleType::Domestic;
            record.communication_module_version = display_version(communication_module_version);
            record.communication_module_version_raw = Some(communication_module_version);
            record.plc_version = display_version(plc_version);
            record.plc_version_raw = Some(plc_version);
            record.version_payload = hex_payload(payload);
            record.version_state = if record.type_mismatch {
                "type_mismatch".into()
            } else {
                "success".into()
            };
            record.last_version_success_at = Some(now);
            record.last_version_error.clear();
            record.consecutive_failures = 0;
            record.stale = false;
        }
        Ok(StationVersionResponse::LegacyRussian { payload }) => {
            record.detected_module_type = ModuleType::LegacyRussian;
            record.type_mismatch = record.configured_module_type != ModuleType::LegacyRussian;
            record.communication_module_version.clear();
            record.communication_module_version_raw = None;
            record.plc_version.clear();
            record.plc_version_raw = None;
            record.version_payload = hex_payload(payload);
            record.version_state = if record.type_mismatch {
                "type_mismatch".into()
            } else {
                "unsupported".into()
            };
            record.last_version_success_at = Some(now);
            record.last_version_error = if record.type_mismatch {
                "configured domestic station returned the legacy all-FF marker".into()
            } else {
                String::new()
            };
            record.consecutive_failures = 0;
            record.stale = false;
        }
        Err(message) => {
            record.version_state = "failed".into();
            record.last_version_error = message;
            record.consecutive_failures = record.consecutive_failures.saturating_add(1);
            record.stale = record.has_last_version();
        }
    }
}

fn hex_payload(payload: [u8; 4]) -> String {
    payload.iter().map(|byte| format!("{byte:02X}")).collect()
}

fn string_arg<'a>(request: &'a Value, key: &str) -> Result<&'a str, OperationError> {
    request
        .get(key)
        .and_then(Value::as_str)
        .filter(|s| !s.is_empty())
        .ok_or_else(|| {
            OperationError::new(
                "invalid_request",
                format!("{key} must be a non-empty String"),
            )
        })
}

fn read_state(path: &Path) -> io::Result<StoredState> {
    match fs::symlink_metadata(path) {
        Ok(metadata) if metadata.file_type().is_symlink() || !metadata.is_file() => {
            return Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                "operations state must be a regular file",
            ))
        }
        Ok(_) => {}
        Err(error) if error.kind() == io::ErrorKind::NotFound => {
            return Ok(StoredState {
                schema_version: 1,
                revision: "1".into(),
                ports: BTreeMap::new(),
            })
        }
        Err(error) => return Err(error),
    }
    let mut bytes = Vec::new();
    fs::File::open(path)?
        .take(MAX_STATE_BYTES + 1)
        .read_to_end(&mut bytes)?;
    if bytes.len() as u64 > MAX_STATE_BYTES {
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            "operations state exceeds size limit",
        ));
    }
    let value: StoredState = serde_json::from_slice(&bytes)
        .map_err(|e| io::Error::new(io::ErrorKind::InvalidData, e))?;
    if value.schema_version != 1
        || value
            .revision
            .parse::<u64>()
            .ok()
            .filter(|n| *n > 0)
            .map(|n| n.to_string())
            != Some(value.revision.clone())
    {
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            "invalid operations schema/revision",
        ));
    }
    Ok(value)
}

fn read_station_state(path: &Path) -> io::Result<StoredStationState> {
    let backup = station_backup_path(path);
    if !path.exists() && backup.exists() {
        fs::rename(&backup, path)?;
    } else if path.exists() && backup.exists() {
        let metadata = fs::symlink_metadata(&backup)?;
        if metadata.file_type().is_symlink() || !metadata.is_file() {
            return Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                "station version backup must be a regular file",
            ));
        }
        fs::remove_file(&backup)?;
    }
    match fs::symlink_metadata(path) {
        Ok(metadata) if metadata.file_type().is_symlink() || !metadata.is_file() => {
            return Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                "station version state must be a regular file",
            ))
        }
        Ok(_) => {}
        Err(error) if error.kind() == io::ErrorKind::NotFound => {
            return Ok(StoredStationState {
                schema_version: 1,
                revision: "1".into(),
                ports: BTreeMap::new(),
            })
        }
        Err(error) => return Err(error),
    }
    let mut bytes = Vec::new();
    fs::File::open(path)?
        .take(MAX_STATION_STATE_BYTES + 1)
        .read_to_end(&mut bytes)?;
    if bytes.len() as u64 > MAX_STATION_STATE_BYTES {
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            "station version state exceeds size limit",
        ));
    }
    let value: StoredStationState = serde_json::from_slice(&bytes)
        .map_err(|error| io::Error::new(io::ErrorKind::InvalidData, error))?;
    if value.schema_version != 1
        || value
            .revision
            .parse::<u64>()
            .ok()
            .filter(|revision| *revision > 0)
            .map(|revision| revision.to_string())
            != Some(value.revision.clone())
    {
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            "invalid station version schema/revision",
        ));
    }
    Ok(value)
}

fn persist_state(path: &Path, state: &StoredState) -> io::Result<()> {
    if let Ok(metadata) = fs::symlink_metadata(path) {
        if metadata.file_type().is_symlink() || !metadata.is_file() {
            return Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                "operations state is not a regular file",
            ));
        }
    }
    let bytes = serde_json::to_vec_pretty(state).map_err(io::Error::other)?;
    if bytes.len() as u64 > MAX_STATE_BYTES {
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            "operations state exceeds size limit",
        ));
    }
    let temp = path.with_file_name(format!(
        ".operations-{}-{}.tmp",
        std::process::id(),
        state.revision
    ));
    let mut file = OpenOptions::new()
        .write(true)
        .create_new(true)
        .open(&temp)?;
    let result = (|| {
        file.write_all(&bytes)?;
        file.sync_all()?;
        drop(file);
        fs::rename(&temp, path)
    })();
    if result.is_err() {
        let _ = fs::remove_file(&temp);
    }
    result
}

fn persist_station_state(path: &Path, state: &StoredStationState) -> io::Result<()> {
    if let Ok(metadata) = fs::symlink_metadata(path) {
        if metadata.file_type().is_symlink() || !metadata.is_file() {
            return Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                "station version state is not a regular file",
            ));
        }
    }
    let bytes = serde_json::to_vec_pretty(state).map_err(io::Error::other)?;
    if bytes.len() as u64 > MAX_STATION_STATE_BYTES {
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            "station version state exceeds size limit",
        ));
    }
    let temp = path.with_file_name(format!(
        ".station-versions-{}-{}.tmp",
        std::process::id(),
        state.revision
    ));
    let mut file = OpenOptions::new()
        .write(true)
        .create_new(true)
        .open(&temp)?;
    let result = (|| {
        file.write_all(&bytes)?;
        file.sync_all()?;
        drop(file);
        replace_file_with_backup(path, &temp)
    })();
    if result.is_err() {
        let _ = fs::remove_file(&temp);
    }
    result
}

fn station_backup_path(path: &Path) -> PathBuf {
    path.with_file_name(".station_versions.json.bak")
}

/// Cross-platform replacement. Windows cannot atomically rename over an open destination;
/// retain the previous complete file as a recoverable backup until the new file is in place.
fn replace_file_with_backup(path: &Path, temp: &Path) -> io::Result<()> {
    let backup = station_backup_path(path);
    if backup.exists() {
        if path.exists() {
            fs::remove_file(&backup)?;
        } else {
            fs::rename(&backup, path)?;
        }
    }
    let had_previous = path.exists();
    if had_previous {
        fs::rename(path, &backup)?;
    }
    match fs::rename(temp, path) {
        Ok(()) => {
            if had_previous {
                // The new primary is already committed. A stale backup is harmless and is
                // cleaned during the next read/replace; do not report a false failed write.
                let _ = fs::remove_file(&backup);
            }
            Ok(())
        }
        Err(error) => {
            if had_previous && !path.exists() && backup.exists() {
                let _ = fs::rename(&backup, path);
            }
            Err(error)
        }
    }
}

fn increment_persisted_revision(current: &str) -> io::Result<String> {
    current
        .parse::<u64>()
        .ok()
        .and_then(|revision| revision.checked_add(1))
        .map(|revision| revision.to_string())
        .ok_or_else(|| io::Error::new(io::ErrorKind::InvalidData, "station revision exhausted"))
}

fn station_topology_fingerprint(
    port: &SerialPortConfig,
    device: &crate::csv_parser::DeviceConfig,
) -> String {
    let mut hasher = Sha256::new();
    hasher.update(port.port_number.as_bytes());
    hasher.update([0]);
    hasher.update(device.config.device_id.to_be_bytes());
    hasher.update(device.config.kks_prefix.as_bytes());
    let mut node_ids = device
        .records
        .iter()
        .map(|record| record.kks.as_str())
        .collect::<Vec<_>>();
    node_ids.sort_unstable();
    for node_id in node_ids {
        hasher.update([0]);
        hasher.update(node_id.as_bytes());
    }
    format!("{:x}", hasher.finalize())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::csv_parser::{Config, DeviceConfig};
    use std::sync::Barrier;

    fn fixture() -> (
        tempfile::TempDir,
        Operations,
        Vec<SerialPortConfig>,
        mpsc::Receiver<Command>,
    ) {
        fixture_with_shutdown(CancellationToken::new())
    }

    fn fixture_with_shutdown(
        shutdown: CancellationToken,
    ) -> (
        tempfile::TempDir,
        Operations,
        Vec<SerialPortConfig>,
        mpsc::Receiver<Command>,
    ) {
        let root = tempfile::tempdir().unwrap();
        fs::create_dir(root.path().join("config")).unwrap();
        fs::create_dir(root.path().join("logs")).unwrap();
        let devices = [0, 255]
            .into_iter()
            .map(|device_id| {
                DeviceConfig::new(
                    Config {
                        com: "TEST-NOT-A-SERIAL-DEVICE".into(),
                        baud_rate: 9600,
                        is_special: false,
                        com_index: 0,
                        device_id,
                        data_len: 1,
                        kks_prefix: String::new(),
                    },
                    Vec::new(),
                )
            })
            .collect();
        let ports = vec![SerialPortConfig::new(
            "TEST-NOT-A-SERIAL-DEVICE".into(),
            9600,
            false,
            devices,
        )];
        let operations = Operations::new_with_shutdown(
            root.path().join("config"),
            root.path().join("logs"),
            "test".into(),
            &ports,
            shutdown,
        )
        .unwrap();
        let (tx, rx) = mpsc::channel();
        operations.register_worker(&ports[0].port_number, tx);
        // Simulated worker confirmation; no real port is opened in this suite.
        operations.report_runtime(
            &ports[0].port_number,
            true,
            false,
            [(0, true, false, None), (255, true, false, None)],
        );
        (root, operations, ports, rx)
    }

    fn stop_read(expected: &str) -> Value {
        json!({"action":"set_device_read_enabled","port_id":"TEST-NOT-A-SERIAL-DEVICE","device_id":"0","enabled":false,"expected_revision":expected})
    }

    fn set_module(module_type: &str, station_revision: &str) -> Value {
        json!({
            "action":"set_device_module_type",
            "port_id":"TEST-NOT-A-SERIAL-DEVICE",
            "device_id":"0",
            "module_type":module_type,
            "expected_revision":"1",
            "expected_station_revision":station_revision
        })
    }

    #[test]
    fn station_versions_are_flat_additive_and_default_unknown_without_query() {
        let (root, operations, ports, rx) = fixture();
        let status = operations.status();
        assert_eq!(status["schema_version"], 1);
        assert_eq!(status["station_revision"], "2");
        assert_eq!(
            status["ports"][0]["devices"][0]["configured_module_type"],
            "unknown"
        );
        assert_eq!(
            status["ports"][0]["devices"][0]["version_state"],
            "never_queried"
        );
        assert!(status["ports"][0]["devices"][0]
            .get("station_version")
            .is_none());
        assert!(operations
            .startup_version_query_devices(&ports[0].port_number)
            .is_empty());
        assert!(rx.try_recv().is_err());
        assert!(root.path().join("config/station_versions.json").is_file());
    }

    #[test]
    fn station_version_state_replaces_existing_file_and_survives_restart() {
        let (root, operations, ports, rx) = fixture();
        operations
            .set_device_module_type(&set_module("domestic", "2"), Duration::ZERO)
            .unwrap();
        let command = rx.recv().unwrap();
        assert!(matches!(
            command.command,
            CommandType::StationModuleTypeSetting(StationModuleTypeSetting {
                device_id: 0,
                module_type: ModuleType::Domestic
            })
        ));
        let receipt = command.control_receipt.unwrap();
        operations.complete_control(&ports[0].port_number, receipt.revision, &Ok(()));
        let _ = receipt.reply.send(Ok(()));
        operations.report_runtime(
            &ports[0].port_number,
            true,
            true,
            [(0, true, true, None), (255, true, false, None)],
        );
        assert!(operations
            .begin_automatic_station_query(&ports[0].port_number, 0)
            .unwrap());
        operations
            .report_station_query_result(
                &ports[0].port_number,
                0,
                Ok(StationVersionResponse::Domestic {
                    payload: [1, 4, 0, 5],
                    communication_module_version: 0x0104,
                    plc_version: 0x0005,
                }),
            )
            .unwrap();
        assert!(!root
            .path()
            .join("config/.station_versions.json.bak")
            .exists());
        let restarted = Operations::new(
            root.path().join("config"),
            root.path().join("logs"),
            "test".into(),
            &ports,
        )
        .unwrap();
        let restarted_status = restarted.status();
        let device = &restarted_status["ports"][0]["devices"][0];
        assert_eq!(device["communication_module_version"], "V1.4");
        assert_eq!(device["plc_version"], "V0.5");
        assert_eq!(device["version_payload"], "01040005");
        assert_eq!(device["version_state"], "success");
    }

    #[test]
    fn interrupted_query_recovers_as_failed_and_can_be_scheduled_again() {
        let (root, operations, ports, rx) = fixture();
        operations
            .set_device_module_type(&set_module("domestic", "2"), Duration::ZERO)
            .unwrap();
        let command = rx.recv().unwrap();
        let receipt = command.control_receipt.unwrap();
        operations.complete_control(&ports[0].port_number, receipt.revision, &Ok(()));
        let _ = receipt.reply.send(Ok(()));
        operations.report_runtime(
            &ports[0].port_number,
            true,
            true,
            [(0, true, true, None), (255, true, false, None)],
        );
        assert!(operations
            .begin_automatic_station_query(&ports[0].port_number, 0)
            .unwrap());
        drop(operations);

        let restarted = Operations::new(
            root.path().join("config"),
            root.path().join("logs"),
            "test".into(),
            &ports,
        )
        .unwrap();
        let status = restarted.status();
        let device = &status["ports"][0]["devices"][0];
        assert_eq!(device["version_state"], "failed");
        assert!(device["last_version_error"]
            .as_str()
            .unwrap()
            .contains("interrupted"));
        assert_eq!(
            restarted.startup_version_query_devices(&ports[0].port_number),
            vec![0]
        );
    }

    #[test]
    fn valid_all_ff_marks_mismatch_and_suppresses_future_automatic_probe() {
        let (_root, operations, ports, rx) = fixture();
        operations
            .set_device_module_type(&set_module("domestic", "2"), Duration::ZERO)
            .unwrap();
        let command = rx.recv().unwrap();
        let receipt = command.control_receipt.unwrap();
        operations.complete_control(&ports[0].port_number, receipt.revision, &Ok(()));
        let _ = receipt.reply.send(Ok(()));
        operations.report_runtime(
            &ports[0].port_number,
            true,
            true,
            [(0, true, true, None), (255, true, false, None)],
        );
        assert!(operations
            .begin_automatic_station_query(&ports[0].port_number, 0)
            .unwrap());
        operations
            .report_station_query_result(
                &ports[0].port_number,
                0,
                Ok(StationVersionResponse::LegacyRussian { payload: [0xFF; 4] }),
            )
            .unwrap();
        let status = operations.status();
        let device = &status["ports"][0]["devices"][0];
        assert_eq!(device["detected_module_type"], "legacy_russian");
        assert_eq!(device["type_mismatch"], true);
        assert_eq!(device["version_state"], "type_mismatch");
        assert!(operations
            .startup_version_query_devices(&ports[0].port_number)
            .is_empty());

        operations
            .report_station_query_result(
                &ports[0].port_number,
                0,
                Err("manual version query timed out".into()),
            )
            .unwrap();
        let status = operations.status();
        let device = &status["ports"][0]["devices"][0];
        assert_eq!(device["detected_module_type"], "legacy_russian");
        assert_eq!(device["version_payload"], "FFFFFFFF");
        assert_eq!(device["version_state"], "failed");
        assert_eq!(device["stale"], true);
        assert!(device["last_version_success_at"].is_string());
    }

    #[test]
    fn shutdown_rejects_new_commands_and_controls_without_mutating_settings() {
        let shutdown = CancellationToken::new();
        let (root, operations, ports, rx) = fixture_with_shutdown(shutdown.clone());
        let permit = operations.admit_command(&ports[0].port_number).unwrap();
        shutdown.cancel();

        let error = operations.admit_command(&ports[0].port_number).unwrap_err();
        assert_eq!(error.code, "operation_failed");
        assert!(error.message.contains("shutting down"));
        assert_eq!(operations.status()["ports"][0]["pending_commands"], 1);
        for request in [
            stop_read("1"),
            json!({"action":"set_port_enabled","port_id":ports[0].port_number,"enabled":false,"expected_revision":"1"}),
        ] {
            let error = operations.execute(&request).unwrap_err();
            assert_eq!(error.code, "operation_failed");
            assert_eq!(error.current_revision.as_deref(), Some("1"));
            assert!(error.message.contains("shutting down"));
        }
        let station_error = operations
            .set_device_module_type(&set_module("domestic", "2"), Duration::ZERO)
            .unwrap_err();
        assert_eq!(station_error.code, "operation_failed");
        assert_eq!(station_error.current_revision.as_deref(), Some("2"));
        assert!(station_error.message.contains("shutting down"));
        assert!(rx.try_recv().is_err());
        assert!(!root.path().join("config/operations.json").exists());
        let status = operations.execute(&json!({"action":"status"})).unwrap();
        assert_eq!(status["revision"], "1");
        assert_eq!(status["ports"][0]["enabled"], true);
        assert_eq!(status["ports"][0]["devices"][0]["read_enabled"], true);
        drop(permit);
        assert_eq!(operations.status()["ports"][0]["pending_commands"], 0);
    }

    #[test]
    fn shutdown_is_not_persisted_and_restart_keeps_acquisition_preferences() {
        let shutdown = CancellationToken::new();
        let (root, operations, ports, _rx) = fixture_with_shutdown(shutdown.clone());
        operations
            .set_control(&stop_read("1"), "set_device_read_enabled", Duration::ZERO)
            .unwrap();
        let path = root.path().join("config/operations.json");
        let saved = fs::read(&path).unwrap();
        shutdown.cancel();
        assert!(operations.admit_command(&ports[0].port_number).is_err());

        let restarted = Operations::new(
            root.path().join("config"),
            root.path().join("logs"),
            "test".into(),
            &ports,
        )
        .unwrap();
        assert_eq!(fs::read(&path).unwrap(), saved);
        assert_eq!(restarted.status()["revision"], "2");
        assert!(restarted.initial_port_enabled(&ports[0].port_number));
        assert!(!restarted.initial_device_read_enabled(&ports[0].port_number, 0));
        assert!(restarted.initial_device_read_enabled(&ports[0].port_number, 255));
        restarted.report_runtime(
            &ports[0].port_number,
            true,
            false,
            [(0, false, false, None), (255, true, false, None)],
        );
        assert!(restarted.admit_command(&ports[0].port_number).is_ok());
    }

    #[test]
    fn acquisition_cas_rejects_stale_revision_without_enqueue_or_persistence() {
        let (root, operations, _, rx) = fixture();
        let error = operations
            .set_control(&stop_read("0"), "set_device_read_enabled", Duration::ZERO)
            .unwrap_err();
        assert_eq!(error.code, "conflict");
        assert_eq!(error.current_revision.as_deref(), Some("1"));
        assert!(rx.try_recv().is_err());
        assert!(!root.path().join("config/operations.json").exists());
        assert_eq!(operations.status()["revision"], "1");
    }

    #[test]
    fn acquisition_pending_is_not_applied_and_survives_restart() {
        let (root, operations, ports, rx) = fixture();
        let response = operations
            .set_control(&stop_read("1"), "set_device_read_enabled", Duration::ZERO)
            .unwrap();
        assert_eq!(response["status"], "Pending");
        assert_eq!(response["revision"], "2");
        let device = &response["ports"][0]["devices"][0];
        assert_eq!(device["read_enabled"], false);
        assert_eq!(device["applied_read_enabled"], true);
        assert_eq!(response["ports"][0]["control_pending"], true);
        let command = rx.try_recv().unwrap();
        assert!(matches!(
            command.command,
            CommandType::DeviceReadSetting(DeviceReadSetting {
                device_id: 0,
                read_enabled: false
            })
        ));
        assert_eq!(command.control_receipt.as_ref().unwrap().revision, 2);
        let restarted = Operations::new(
            root.path().join("config"),
            root.path().join("logs"),
            "test".into(),
            &ports,
        )
        .unwrap();
        assert_eq!(restarted.status()["revision"], "2");
        assert!(!restarted.initial_device_read_enabled(&ports[0].port_number, 0));
        assert!(restarted.initial_device_read_enabled(&ports[0].port_number, 255));
        assert!(!restarted.status()["ports"][0]["open"].as_bool().unwrap());
    }

    #[test]
    fn acquisition_applied_requires_explicit_worker_confirmation() {
        let (_root, operations, ports, rx) = fixture();
        let caller = operations.clone();
        let task = std::thread::spawn(move || {
            caller.set_control(
                &stop_read("1"),
                "set_device_read_enabled",
                Duration::from_secs(2),
            )
        });
        let command = rx.recv_timeout(Duration::from_secs(1)).unwrap();
        assert_eq!(operations.status()["ports"][0]["control_pending"], true);
        let receipt = command.control_receipt.unwrap();
        operations.report_runtime(
            &ports[0].port_number,
            true,
            false,
            [(0, false, false, None), (255, true, false, None)],
        );
        operations.complete_control(&ports[0].port_number, receipt.revision, &Ok(()));
        receipt.reply.send(Ok(())).unwrap();
        let result = task.join().unwrap().unwrap();
        assert_eq!(result["status"], "Applied");
        assert_eq!(result["ports"][0]["control_pending"], false);
        assert_eq!(
            result["ports"][0]["devices"][0]["applied_read_enabled"],
            false
        );
        assert_eq!(
            result["ports"][0]["devices"][1]["applied_read_enabled"],
            true
        );
    }

    #[test]
    fn acquisition_busy_tracks_last_command_permit_clone() {
        let (_root, operations, ports, _rx) = fixture();
        let permit = operations.admit_command(&ports[0].port_number).unwrap();
        let retained = permit.clone();
        drop(permit);
        assert_eq!(operations.status()["ports"][0]["pending_commands"], 1);
        let error = operations
            .set_control(&stop_read("1"), "set_device_read_enabled", Duration::ZERO)
            .unwrap_err();
        assert_eq!(error.code, "busy");
        assert_eq!(operations.status()["revision"], "1");
        drop(retained);
        assert_eq!(operations.status()["ports"][0]["pending_commands"], 0);
        operations
            .set_control(&stop_read("1"), "set_device_read_enabled", Duration::ZERO)
            .unwrap();
        assert_eq!(
            operations
                .admit_command(&ports[0].port_number)
                .unwrap_err()
                .code,
            "busy"
        );
    }

    #[test]
    fn acquisition_command_and_control_admission_are_atomic() {
        let (_root, operations, ports, _rx) = fixture();
        let barrier = Arc::new(Barrier::new(3));
        let command_ops = operations.clone();
        let command_barrier = barrier.clone();
        let port_id = ports[0].port_number.clone();
        let command = std::thread::spawn(move || {
            command_barrier.wait();
            command_ops.admit_command(&port_id)
        });
        let control_ops = operations.clone();
        let control_barrier = barrier.clone();
        let control = std::thread::spawn(move || {
            control_barrier.wait();
            control_ops.set_control(&stop_read("1"), "set_device_read_enabled", Duration::ZERO)
        });
        barrier.wait();
        // Keep the returned permit alive until both competitors finish.
        let command = command.join().unwrap();
        let control = control.join().unwrap();
        assert_ne!(
            command.is_ok(),
            control.is_ok(),
            "exactly one operation may enter the physical port"
        );
        if let Err(error) = command {
            assert_eq!(error.code, "busy");
        }
        if let Err(error) = control {
            assert_eq!(error.code, "busy");
        }
    }

    #[test]
    fn acquisition_invalidated_receipt_cannot_clear_new_pending_operation() {
        let (_root, operations, ports, rx) = fixture();
        operations
            .set_control(&stop_read("1"), "set_device_read_enabled", Duration::ZERO)
            .unwrap();
        let old = rx.recv().unwrap().control_receipt.unwrap();
        operations.fail_pending_controls(&ports[0].port_number, "simulated worker panic");
        let mut second = stop_read("2");
        second["enabled"] = json!(true);
        operations
            .set_control(&second, "set_device_read_enabled", Duration::ZERO)
            .unwrap();
        let current = rx.recv().unwrap().control_receipt.unwrap();
        assert!(!operations.control_is_current(&ports[0].port_number, old.revision));
        assert!(operations.control_is_current(&ports[0].port_number, current.revision));
        operations.complete_control(&ports[0].port_number, old.revision, &Ok(()));
        assert_eq!(operations.status()["ports"][0]["control_pending"], true);
        assert_eq!(operations.status()["revision"], "3");
    }

    #[test]
    fn acquisition_failed_persistence_does_not_change_runtime_or_enqueue() {
        let (root, operations, _, rx) = fixture();
        fs::create_dir(root.path().join("config/operations.json")).unwrap();
        let error = operations
            .set_control(&stop_read("1"), "set_device_read_enabled", Duration::ZERO)
            .unwrap_err();
        assert_eq!(error.code, "persistence_failed");
        assert_eq!(operations.status()["revision"], "1");
        assert_eq!(
            operations.status()["ports"][0]["devices"][0]["read_enabled"],
            true
        );
        assert!(rx.try_recv().is_err());
    }

    #[test]
    fn acquisition_closed_worker_reports_saved_but_not_applied() {
        let (_root, operations, _, rx) = fixture();
        drop(rx);
        let error = operations
            .set_control(&stop_read("1"), "set_device_read_enabled", Duration::ZERO)
            .unwrap_err();
        assert_eq!(error.code, "operation_failed");
        assert_eq!(error.current_revision.as_deref(), Some("2"));
        let status = operations.status();
        assert_eq!(status["ports"][0]["control_pending"], false);
        assert_eq!(status["ports"][0]["devices"][0]["read_enabled"], false);
        assert_eq!(
            status["ports"][0]["devices"][0]["applied_read_enabled"],
            true
        );
    }

    #[test]
    fn acquisition_unknown_device_and_invalid_json_have_business_errors() {
        let (_root, operations, _, rx) = fixture();
        let mut request = stop_read("1");
        request["device_id"] = json!("1");
        assert_eq!(operations.execute(&request).unwrap_err().code, "not_found");
        for invalid in ["{", "[]", "{}", r#"{"action":"arbitrary_shell"}"#] {
            let result: Value = serde_json::from_str(&operations.execute_json(invalid)).unwrap();
            assert_eq!(result["ok"], false);
            assert_eq!(result["error"]["code"], "invalid_request");
        }
        assert!(rx.try_recv().is_err());
    }

    #[test]
    fn acquisition_state_rejects_noncanonical_revision_on_restart() {
        let (root, _operations, ports, _rx) = fixture();
        for revision in ["0", "01", "-1", "not-a-number", "18446744073709551616"] {
            fs::write(
                root.path().join("config/operations.json"),
                json!({"schema_version":1,"revision":revision,"ports":{}}).to_string(),
            )
            .unwrap();
            assert!(
                Operations::new(
                    root.path().join("config"),
                    root.path().join("logs"),
                    "test".into(),
                    &ports
                )
                .is_err(),
                "revision {revision}"
            );
        }
    }

    #[cfg(unix)]
    #[test]
    fn acquisition_state_does_not_follow_symlinks() {
        let (root, _operations, ports, _rx) = fixture();
        let outside = root.path().join("outside.json");
        let contents = r#"{"schema_version":1,"revision":"1","ports":{}}"#;
        fs::write(&outside, contents).unwrap();
        std::os::unix::fs::symlink(&outside, root.path().join("config/operations.json")).unwrap();
        assert!(Operations::new(
            root.path().join("config"),
            root.path().join("logs"),
            "test".into(),
            &ports
        )
        .is_err());
        assert_eq!(fs::read_to_string(outside).unwrap(), contents);
    }
}
