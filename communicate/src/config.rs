use std::collections::HashSet;
use std::fs::File;
use std::io::{self, Read};
use std::net::IpAddr;
use std::path::{Path, PathBuf};

use serde::Deserialize;
use thiserror::Error;

use crate::points::DataType;

const MAX_CONFIG_BYTES: u64 = 1024 * 1024;
const MAX_POINTS_BYTES: u64 = 8 * 1024 * 1024;
pub const MAX_POINTS: usize = 25_000;

#[derive(Debug, Error)]
pub enum ConfigError {
    #[error("configuration I/O: {0}")]
    Io(#[from] io::Error),
    #[error("invalid TOML: {0}")]
    Toml(#[from] toml::de::Error),
    #[error("invalid point CSV: {0}")]
    Csv(#[from] csv::Error),
    #[error("{0}")]
    Invalid(String),
}

#[derive(Clone, Debug, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Config {
    pub schema_version: u32,
    pub instance_id: String,
    pub points_file: PathBuf,
    pub opc: OpcConfig,
    #[serde(default)]
    pub ports: Vec<PortConfig>,
}

#[derive(Clone, Debug, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct OpcConfig {
    pub bind_ip: IpAddr,
    pub advertised_host: String,
    pub port: u16,
    pub allow_anonymous_none: bool,
    pub allow_anonymous_secure: bool,
    pub trust_client_certificates: bool,
}

#[derive(Clone, Copy, Debug, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Parity {
    None,
    Even,
    Odd,
}

#[derive(Clone, Debug, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PortConfig {
    pub id: String,
    pub device: PathBuf,
    pub enabled: bool,
    pub baud_rate: u32,
    pub data_bits: u8,
    pub parity: Parity,
    pub stop_bits: u8,
    pub first_byte_timeout_ms: u64,
    pub frame_timeout_ms: u64,
    pub write_timeout_ms: u64,
    pub late_frame_guard_ms: u64,
    pub reconnect_min_ms: u64,
    pub reconnect_max_ms: u64,
    pub max_frame_bytes: usize,
    pub max_payload_bytes: usize,
    pub tasks: Vec<TaskConfig>,
    #[serde(default)]
    pub synchronization: Option<crate::synchronization::Settings>,
}

#[derive(Clone, Debug, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct TaskConfig {
    pub id: String,
    pub enabled: bool,
    pub protocol: String,
    pub interval_ms: u64,
}

#[derive(Clone, Debug, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PointDefinition {
    #[serde(alias = "kks")]
    pub name: String,
    pub task_id: String,
    pub data_type: DataType,
    #[serde(default)]
    pub description: String,
}

#[derive(Debug)]
pub struct LoadedConfig {
    pub config: Config,
    pub points: Vec<PointDefinition>,
    pub config_text: String,
    pub points_text: String,
}

fn bounded_read(path: &Path, max: u64) -> Result<Vec<u8>, ConfigError> {
    let mut bytes = Vec::new();
    File::open(path)?.take(max + 1).read_to_end(&mut bytes)?;
    if bytes.len() as u64 > max {
        return Err(ConfigError::Invalid(format!(
            "file exceeds {max} bytes: {}",
            path.display()
        )));
    }
    Ok(bytes)
}

pub fn load(path: &Path) -> Result<LoadedConfig, ConfigError> {
    let bytes = bounded_read(path, MAX_CONFIG_BYTES)?;
    let text = std::str::from_utf8(&bytes)
        .map_err(|_| ConfigError::Invalid("configuration must be UTF-8".into()))?;
    let config: Config = toml::from_str(text)?;
    let points_path = if config.points_file.is_absolute() {
        config.points_file.clone()
    } else {
        path.parent()
            .unwrap_or_else(|| Path::new("."))
            .join(&config.points_file)
    };
    let bytes = bounded_read(&points_path, MAX_POINTS_BYTES)?;
    parse(
        text,
        std::str::from_utf8(&bytes)
            .map_err(|_| ConfigError::Invalid("point CSV must be UTF-8".into()))?,
    )
}

/// Pure cross-file validation also used by the configuration RPC; never opens
/// an arbitrary path mentioned in a remotely submitted draft.
pub fn parse(text: &str, points_text: &str) -> Result<LoadedConfig, ConfigError> {
    if text.len() as u64 > MAX_CONFIG_BYTES || points_text.len() as u64 > MAX_POINTS_BYTES {
        return Err(ConfigError::Invalid(
            "TOML 最大 1 MiB，点表最大 8 MiB".into(),
        ));
    }
    let config: Config = toml::from_str(text)?;
    let mut reader = csv::Reader::from_reader(points_text.as_bytes());
    let headers = reader.headers()?.iter().collect::<Vec<_>>();
    if headers != ["name", "task_id", "data_type", "description"]
        && headers != ["kks", "task_id", "data_type", "description"]
    {
        return Err(ConfigError::Invalid(
            "point CSV header must be name,task_id,data_type,description".into(),
        ));
    }
    let mut points = Vec::new();
    for row in reader.deserialize() {
        if points.len() == MAX_POINTS {
            return Err(ConfigError::Invalid("too many points".into()));
        }
        points.push(row?);
    }
    validate(&config, &points)?;
    crate::unit_report::validate_points(&config, &points)?;
    Ok(LoadedConfig {
        config,
        points,
        config_text: text.into(),
        points_text: points_text.into(),
    })
}

pub fn valid_id(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 128
        && value
            .bytes()
            .all(|c| c.is_ascii_alphanumeric() || b"-_.".contains(&c))
}

fn valid_point_name(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 128
        && value
            .bytes()
            .all(|c| c.is_ascii_alphanumeric() || b"-_.[]".contains(&c))
}

pub fn validate(config: &Config, points: &[PointDefinition]) -> Result<(), ConfigError> {
    let reject = |message: &str| ConfigError::Invalid(message.into());
    if config.schema_version != 1
        || !valid_id(&config.instance_id)
        || config.points_file.as_os_str().is_empty()
    {
        return Err(reject(
            "schema_version must be 1 and instance_id/points_file must be valid",
        ));
    }
    let opc = &config.opc;
    if opc.port == 0 || (!opc.allow_anonymous_none && !opc.allow_anonymous_secure) {
        return Err(reject(
            "OPC needs a nonzero port and at least one enabled endpoint",
        ));
    }
    if opc.advertised_host.is_empty()
        || opc.advertised_host.len() > 253
        || !opc
            .advertised_host
            .bytes()
            .all(|c| c.is_ascii_alphanumeric() || b".-".contains(&c))
        || opc
            .advertised_host
            .parse::<IpAddr>()
            .is_ok_and(|ip| ip.is_unspecified())
    {
        return Err(reject(
            "advertised_host must be a hostname or non-unspecified IPv4 address, not a URL",
        ));
    }
    if config.ports.len() > 32 || points.len() > MAX_POINTS {
        return Err(reject("maximum 32 ports and 25000 points"));
    }
    let mut ports = HashSet::new();
    let mut devices = HashSet::new();
    let mut tasks = HashSet::new();
    for port in &config.ports {
        if !valid_id(&port.id) || !ports.insert(&port.id) || !port.device.is_absolute() {
            return Err(reject(
                "port id must be unique; device must be an absolute path",
            ));
        }
        let physical = port
            .device
            .canonicalize()
            .unwrap_or_else(|_| port.device.clone());
        if !devices.insert(physical) {
            return Err(reject(
                "one physical serial device may only be configured once",
            ));
        }
        if !(300..=4_000_000).contains(&port.baud_rate)
            || !(5..=8).contains(&port.data_bits)
            || !(1..=2).contains(&port.stop_bits)
        {
            return Err(reject("invalid baud rate, data bits or stop bits"));
        }
        if [
            port.first_byte_timeout_ms,
            port.frame_timeout_ms,
            port.write_timeout_ms,
            port.late_frame_guard_ms,
        ]
        .iter()
        .any(|v| !(1..=60_000).contains(v))
            || port.frame_timeout_ms < port.first_byte_timeout_ms
            || port.reconnect_min_ms < 100
            || port.reconnect_max_ms > 60_000
            || port.reconnect_min_ms > port.reconnect_max_ms
        {
            return Err(reject("invalid timeout or reconnect limits"));
        }
        if !(18..=u16::MAX as usize).contains(&port.max_frame_bytes)
            || !(1..=u16::MAX as usize).contains(&port.max_payload_bytes)
            || port.tasks.len() > 256
        {
            return Err(reject("invalid frame/payload/task limits"));
        }
        for task in &port.tasks {
            if !valid_id(&task.id)
                || !tasks.insert(&task.id)
                || !valid_id(&task.protocol)
                || !(10..=3_600_000).contains(&task.interval_ms)
            {
                return Err(reject(
                    "task ids must be globally unique; interval must be 10..3600000 ms",
                ));
            }
        }
        crate::synchronization::validate_port(port)?;
    }
    let mut names = HashSet::new();
    for point in points {
        if !valid_point_name(&point.name)
            || ["Devices", "Methods"].contains(&point.name.as_str())
            || point.name.starts_with("RcdSync.")
            || point.name.ends_with("_Send")
            || point.name.ends_with("_SendCommand")
            || !names.insert(&point.name)
            || !tasks.contains(&point.task_id)
            || point.description.len() > 1024
            || point.description.chars().any(char::is_control)
        {
            return Err(reject(
                "invalid/duplicate/reserved variable name, missing task or invalid description",
            ));
        }
    }
    Ok(())
}

/// Validate before PKI creation, opening OPC, or serial I/O. Add real adapters
/// only after their request and response contracts have golden-frame tests.
pub fn validate_adapters(config: &Config) -> Result<(), ConfigError> {
    crate::protocol::production_registry()
        .prepare(config)
        .map(|_| ())
}
