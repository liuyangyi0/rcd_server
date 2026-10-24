//! 软件配置模块。
//!
//! 从 `config/config.toml` 文件中加载串口参数、服务器参数和全局设置。

use log::info;
use serde::{Deserialize, Serialize};
use serialport::{DataBits, Parity, StopBits};
use std::net::SocketAddr;

use crate::logging::LoggingConfig;

pub const DEFAULT_POLL_INTERVAL_MS: u64 = 1;
pub const DEFAULT_COMMAND_RETRY_COUNT: u32 = 3;
pub const DEFAULT_COMMAND_INTERVAL_MS: u64 = 0;
pub const MAX_COMMAND_INTERVAL_MS: u64 = 60_000;
pub const DEFAULT_QUERY_RETRY_COUNT: u32 = 0;
pub const DEFAULT_PARITY_SWITCH_DELAY_MS: u64 = 1;
pub const DEFAULT_TRANSMIT_SETTLE_GUARD_MS: u64 = 1;

// ============================================================
//  远程类型桥接（serde 无法直接反序列化外部 crate 的枚举）
// ============================================================

#[derive(Deserialize)]
#[serde(remote = "DataBits")]
pub enum DataBitsDef {
    Five,
    Six,
    Seven,
    Eight,
}

#[derive(Deserialize)]
#[serde(remote = "Parity")]
pub enum ParityDef {
    None,
    Odd,
    Even,
    Mark,
    Space,
}

#[derive(Deserialize)]
#[serde(remote = "StopBits")]
pub enum StopBitsDef {
    One,
    Two,
}

// ============================================================
//  配置结构体
// ============================================================

/// 程序运行角色：主机 / 备机。
#[derive(Deserialize, Debug, Clone, PartialEq)]
pub enum RunLocation {
    #[serde(rename = "primary")]
    Primary,
    #[serde(rename = "secondary")]
    Secondary,
}

/// 计算规则的执行位置。缺省保持旧版的下级本地计算行为。
#[derive(Deserialize, Serialize, Debug, Clone, Copy, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum CalculationMode {
    Local,
    Manager,
}

impl Default for CalculationMode {
    fn default() -> Self {
        Self::Local
    }
}

impl CalculationMode {
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Local => "local",
            Self::Manager => "manager",
        }
    }
}

/// 下级是否执行并发布 calc.toml 中的计算点。
#[derive(Deserialize, Serialize, Debug, Clone, Default)]
#[serde(default, deny_unknown_fields)]
pub struct Calculation {
    pub mode: CalculationMode,
}

/// 服务器网络与主备切换配置。
#[derive(Deserialize, Debug, Clone)]
pub struct Server {
    /// 主机服务地址，同时作为主机角色的 OPC UA 广告地址来源。
    #[serde(rename = "primary_ip")]
    pub primary_ip: String,
    /// 备机服务地址，同时作为备机角色的 OPC UA 广告地址来源。
    #[serde(rename = "secondary_ip")]
    pub secondary_ip: String,
    /// 配置文件中指定的运行角色。
    pub run_on: RunLocation,
    /// 运行时实际生效的角色（可动态切换）。
    pub current_run: RunLocation,
}

impl Server {
    /// 返回当前物理部署位置对客户端可访问的主机名/IP，用于 OPC UA EndpointUrl。
    /// `current_run` 可在主备切换时改变，但机器的网络地址应始终由 `run_on` 决定。
    pub fn opcua_advertised_host(&self) -> Result<String, String> {
        let configured = match self.run_on {
            RunLocation::Primary => &self.primary_ip,
            RunLocation::Secondary => &self.secondary_ip,
        };
        advertised_host_from_server_address(configured)
    }
}

fn advertised_host_from_server_address(address: &str) -> Result<String, String> {
    let address = address.trim();
    if address.is_empty() {
        return Err("服务器地址为空".to_string());
    }

    let host = if let Ok(socket_addr) = address.parse::<SocketAddr>() {
        match socket_addr {
            SocketAddr::V4(addr) => addr.ip().to_string(),
            SocketAddr::V6(addr) => format!("[{}]", addr.ip()),
        }
    } else if let Some((host, port)) = address.rsplit_once(':') {
        if host.is_empty() || port.parse::<u16>().is_err() {
            return Err(format!("服务器地址格式无效: {address}"));
        }
        host.to_string()
    } else {
        address.to_string()
    };

    if matches!(host.as_str(), "0.0.0.0" | "::" | "[::]") {
        return Err(format!("OPC UA 广告地址不能使用通配地址: {host}"));
    }
    if host.contains("/") || host.contains("://") {
        return Err(format!("OPC UA 广告主机名格式无效: {host}"));
    }

    Ok(host)
}

/// 串口全局默认参数。
#[derive(Deserialize, Debug, Clone)]
pub struct Serial {
    /// 全局默认波特率（各 CSV 可覆盖）。
    #[serde(rename = "baud_rate")]
    pub _baud_rate: u32,
    #[serde(with = "DataBitsDef")]
    pub data_bits: DataBits,
    #[serde(with = "ParityDef")]
    pub parity: Parity,
    #[serde(with = "StopBitsDef")]
    pub stop_bits: StopBits,
    /// 流控方式（预留）。
    #[serde(rename = "flow_control")]
    pub _flow_control: String,
}

/// 杂项设置。
#[derive(Deserialize, Debug, Clone)]
pub struct Settings {
    /// 串口读取超时（毫秒）。
    #[serde(rename = "timeout")]
    pub timeout: u64,
    /// 串口轮询间隔（毫秒）。
    #[serde(default = "default_poll_interval_ms")]
    pub poll_interval_ms: u64,
    /// 命令未收到 ACK 时的最大连续下发次数。
    #[serde(default = "default_command_retry_count")]
    pub command_retry_count: u32,
    /// 同一物理串口的上一条控制事务结束至下一条首次发送的最小间隔。
    /// 不限制普通轮询或同一控制事务内的 ACK 重试；0 保持旧行为。
    #[serde(
        default = "default_command_interval_ms",
        deserialize_with = "deserialize_command_interval_ms"
    )]
    pub command_interval_ms: u64,
    /// 自动轮询查询失败后的补发次数。快速模式使用 0，避免一次故障等待两个读取超时。
    #[serde(default = "default_query_retry_count")]
    pub query_retry_count: u32,
    /// 普通串口切换 Mark/Space 校验位后、写入前的驱动保护时间（毫秒）。
    #[serde(default = "default_parity_switch_delay_ms")]
    pub parity_switch_delay_ms: u64,
    /// 驱动发送队列清空并等待线速发送后追加的保护时间（毫秒）。
    #[serde(default = "default_transmit_settle_guard_ms")]
    pub transmit_settle_guard_ms: u64,
}

/// 顶层配置结构，对应 `config.toml` 全部内容。
#[derive(Deserialize, Debug, Clone)]
pub struct Config {
    pub server: Server,
    pub serial: Serial,
    /// 杂项设置。
    pub settings: Settings,
    /// 日志设置。
    #[serde(default)]
    pub logging: LoggingConfig,
    /// 计算可以保留在单机下级，也可以由管理 OPC 集中执行。
    #[serde(default)]
    pub calculation: Calculation,
}

fn default_poll_interval_ms() -> u64 {
    DEFAULT_POLL_INTERVAL_MS
}

fn default_command_retry_count() -> u32 {
    DEFAULT_COMMAND_RETRY_COUNT
}

fn default_command_interval_ms() -> u64 {
    DEFAULT_COMMAND_INTERVAL_MS
}

fn deserialize_command_interval_ms<'de, D>(deserializer: D) -> Result<u64, D::Error>
where
    D: serde::Deserializer<'de>,
{
    let value = u64::deserialize(deserializer)?;
    if value > MAX_COMMAND_INTERVAL_MS {
        return Err(serde::de::Error::custom(
            "command_interval_ms 必须为 0–60000 毫秒的整数",
        ));
    }
    Ok(value)
}

fn default_query_retry_count() -> u32 {
    DEFAULT_QUERY_RETRY_COUNT
}

fn default_parity_switch_delay_ms() -> u64 {
    DEFAULT_PARITY_SWITCH_DELAY_MS
}

fn default_transmit_settle_guard_ms() -> u64 {
    DEFAULT_TRANSMIT_SETTLE_GUARD_MS
}

// ============================================================
//  路径与加载
// ============================================================

/// 获取启动时选定的配置目录；无显式设置时仍为 EXE 同级 `config`。
pub fn config_dir() -> Result<std::path::PathBuf, Box<dyn std::error::Error>> {
    Ok(crate::runtime_paths::current()?.config)
}

/// 从可执行文件同级的 `config/config.toml` 读取并反序列化配置。
pub fn init_config() -> Result<Config, Box<dyn std::error::Error>> {
    let config_path = config_dir()?.join("config.toml");
    info!("配置文件路径: {:?}", config_path);

    let config_str = std::fs::read_to_string(&config_path)?;
    let config: Config = toml::from_str(&config_str)?;
    Ok(config)
}

#[cfg(test)]
mod tests {
    use super::*;

    const BASE_CONFIG: &str = r#"
[server]
primary_ip = "192.168.10.40:11001"
secondary_ip = "192.168.101.92:8080"
run_on = "primary"
current_run = "primary"

[serial]
baud_rate = 28800
data_bits = "Eight"
parity = "None"
stop_bits = "One"
flow_control = "none"
"#;

    #[test]
    fn settings_fast_serial_defaults_are_applied() {
        let toml_str = format!(
            r#"{BASE_CONFIG}
[settings]
timeout = 300

[logging]
"#
        );

        let config: Config = toml::from_str(&toml_str).unwrap();

        assert_eq!(config.settings.timeout, 300);
        assert_eq!(config.settings.poll_interval_ms, DEFAULT_POLL_INTERVAL_MS);
        assert_eq!(
            config.settings.command_retry_count,
            DEFAULT_COMMAND_RETRY_COUNT
        );
        assert_eq!(config.settings.query_retry_count, DEFAULT_QUERY_RETRY_COUNT);
        assert_eq!(
            config.settings.command_interval_ms,
            DEFAULT_COMMAND_INTERVAL_MS
        );
        assert_eq!(
            config.settings.parity_switch_delay_ms,
            DEFAULT_PARITY_SWITCH_DELAY_MS
        );
        assert_eq!(
            config.settings.transmit_settle_guard_ms,
            DEFAULT_TRANSMIT_SETTLE_GUARD_MS
        );
        assert_eq!(config.logging.level, "warn,rcd_server=info");
        assert_eq!(config.logging.keep_files, 30);
    }

    #[test]
    fn settings_poll_interval_can_be_configured() {
        let toml_str = format!(
            r#"{BASE_CONFIG}
[settings]
timeout = 300
poll_interval_ms = 50
command_retry_count = 5
command_interval_ms = 500
query_retry_count = 1
parity_switch_delay_ms = 2
transmit_settle_guard_ms = 3

[logging]
level = "debug"
duplicate_to_stderr = "info"
serial_frame_logs = true
"#
        );

        let config: Config = toml::from_str(&toml_str).unwrap();

        assert_eq!(config.settings.timeout, 300);
        assert_eq!(config.settings.poll_interval_ms, 50);
        assert_eq!(config.settings.command_retry_count, 5);
        assert_eq!(config.settings.command_interval_ms, 500);
        assert_eq!(config.settings.query_retry_count, 1);
        assert_eq!(config.settings.parity_switch_delay_ms, 2);
        assert_eq!(config.settings.transmit_settle_guard_ms, 3);
        assert_eq!(config.logging.level, "debug");
        assert!(config.logging.serial_frame_logs);
    }

    #[test]
    fn calculation_mode_defaults_to_local_and_accepts_manager() {
        let local: Config = toml::from_str(&format!(
            r#"{BASE_CONFIG}
[settings]
timeout = 300
"#
        ))
        .unwrap();
        assert_eq!(local.calculation.mode, CalculationMode::Local);

        let manager: Config = toml::from_str(&format!(
            r#"{BASE_CONFIG}
[settings]
timeout = 300
[calculation]
mode = "manager"
"#
        ))
        .unwrap();
        assert_eq!(manager.calculation.mode, CalculationMode::Manager);
    }

    #[test]
    fn command_interval_accepts_bounds_and_rejects_invalid_config_at_startup() {
        for value in ["0", "60000"] {
            let text =
                format!("{BASE_CONFIG}\n[settings]\ntimeout=50\ncommand_interval_ms={value}\n");
            let config: Config = toml::from_str(&text).unwrap();
            assert_eq!(
                config.settings.command_interval_ms,
                value.parse::<u64>().unwrap()
            );
        }
        for value in ["-1", "60001", "1.5", "\"500\"", "true"] {
            let text =
                format!("{BASE_CONFIG}\n[settings]\ntimeout=50\ncommand_interval_ms={value}\n");
            assert!(toml::from_str::<Config>(&text).is_err(), "accepted {value}");
        }
    }

    #[test]
    fn calculation_mode_rejects_unknown_values_and_fields() {
        for calculation in [
            "[calculation]\nmode = \"automatic\"\n",
            "[calculation]\nmode = \"manager\"\nautomatic = true\n",
        ] {
            let content = format!("{BASE_CONFIG}\n[settings]\ntimeout = 300\n{calculation}");
            assert!(toml::from_str::<Config>(&content).is_err());
        }
    }

    #[test]
    fn opcua_advertised_host_follows_physical_run_location() {
        let server = Server {
            primary_ip: "192.168.10.40:11001".to_string(),
            secondary_ip: "opc-secondary.local:11002".to_string(),
            run_on: RunLocation::Secondary,
            current_run: RunLocation::Primary,
        };

        assert_eq!(
            server.opcua_advertised_host().unwrap(),
            "opc-secondary.local"
        );

        let server = Server {
            run_on: RunLocation::Primary,
            current_run: RunLocation::Secondary,
            ..server
        };
        assert_eq!(server.opcua_advertised_host().unwrap(), "192.168.10.40");
    }

    #[test]
    fn opcua_advertised_host_supports_ipv6_and_rejects_wildcards() {
        assert_eq!(
            advertised_host_from_server_address("[2001:db8::1]:11001").unwrap(),
            "[2001:db8::1]"
        );
        assert!(advertised_host_from_server_address("0.0.0.0:11001").is_err());
        assert!(advertised_host_from_server_address("[::]:11001").is_err());
    }
}
