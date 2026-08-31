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
use tokio_util::sync::CancellationToken;

use crate::model::command::{ControlReceipt, DeviceReadSetting, PortStatus};
use crate::model::{Command, CommandType};
use crate::serial::port_config::SerialPortConfig;

pub const MAX_REQUEST_BYTES: usize = 13 * 1024 * 1024;
const MAX_STATE_BYTES: u64 = 2 * 1024 * 1024;
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
    json!({"schema_version":1,"features":["acquisition","configuration","logs"],
        "max_config_bytes":2_097_152,"max_log_bytes":65_536,"configuration_apply":"restart"})
}

#[derive(Clone, Serialize)]
pub struct DeviceSnapshot {
    pub device_id: String,
    pub name: String,
    pub read_enabled: bool,
    pub applied_read_enabled: bool,
    pub communication_ok: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub last_data_at: Option<String>,
}

#[derive(Clone, Serialize)]
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

struct State {
    saved: StoredState,
    ports: BTreeMap<String, PortSnapshot>,
    pending: BTreeMap<String, u64>,
}

struct Inner {
    state: Mutex<State>,
    senders: Mutex<HashMap<String, mpsc::Sender<Command>>>,
    files: Mutex<files::FileRepository>,
    state_path: PathBuf,
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
        let mut saved = read_state(&state_path)?;
        let mut snapshots = BTreeMap::new();
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
                let enabled = *desired
                    .device_read_enabled
                    .entry(id.clone())
                    .or_insert(true);
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
        let repository = files::FileRepository::new(config_dir, log_dir, log_basename)?;
        Ok(Self {
            inner: Arc::new(Inner {
                state: Mutex::new(State {
                    saved,
                    ports: snapshots,
                    pending: BTreeMap::new(),
                }),
                senders: Mutex::new(HashMap::new()),
                files: Mutex::new(repository),
                state_path,
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

    pub fn status(&self) -> Value {
        let state = self.inner.state.lock().unwrap_or_else(|e| e.into_inner());
        json!({"schema_version":1,"revision":state.saved.revision,"ports":state.ports.values().collect::<Vec<_>>()})
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
            }
        }
    }
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
