//! 软件配置模块。
//!
//! 从 `config/config.toml` 文件中加载串口参数、服务器参数和全局设置。

use log::info;
use serde::Deserialize;
use serialport::{DataBits, Parity, StopBits};

pub const DEFAULT_POLL_INTERVAL_MS: u64 = 20;
pub const DEFAULT_COMMAND_RETRY_COUNT: u32 = 3;

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

/// 服务器网络与主备切换配置。
#[derive(Deserialize, Debug, Clone)]
pub struct Server {
    /// 主机 IP 地址（预留，主备切换时使用）。
    #[serde(rename = "primary_ip")]
    pub _primary_ip: String,
    /// 备机 IP 地址（预留，主备切换时使用）。
    #[serde(rename = "secondary_ip")]
    pub _secondary_ip: String,
    /// 配置文件中指定的运行角色。
    pub run_on: RunLocation,
    /// 运行时实际生效的角色（可动态切换）。
    pub current_run: RunLocation,
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
}

/// 顶层配置结构，对应 `config.toml` 全部内容。
#[derive(Deserialize, Debug, Clone)]
pub struct Config {
    pub server: Server,
    pub serial: Serial,
    /// 杂项设置。
    pub settings: Settings,
}

fn default_poll_interval_ms() -> u64 {
    DEFAULT_POLL_INTERVAL_MS
}

fn default_command_retry_count() -> u32 {
    DEFAULT_COMMAND_RETRY_COUNT
}

// ============================================================
//  路径与加载
// ============================================================

/// 获取可执行文件同级的 `config` 目录路径。
pub fn config_dir() -> Result<std::path::PathBuf, Box<dyn std::error::Error>> {
    let exe_dir = std::env::current_exe()?
        .parent()
        .ok_or_else(|| std::io::Error::new(std::io::ErrorKind::NotFound, "无法获取可执行文件目录"))?
        .to_path_buf();
    Ok(exe_dir.join("config"))
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
    fn settings_poll_interval_defaults_to_20_ms() {
        let toml_str = format!(
            r#"{BASE_CONFIG}
[settings]
timeout = 300
"#
        );

        let config: Config = toml::from_str(&toml_str).unwrap();

        assert_eq!(config.settings.timeout, 300);
        assert_eq!(config.settings.poll_interval_ms, DEFAULT_POLL_INTERVAL_MS);
        assert_eq!(
            config.settings.command_retry_count,
            DEFAULT_COMMAND_RETRY_COUNT
        );
    }

    #[test]
    fn settings_poll_interval_can_be_configured() {
        let toml_str = format!(
            r#"{BASE_CONFIG}
[settings]
timeout = 300
poll_interval_ms = 50
command_retry_count = 5
"#
        );

        let config: Config = toml::from_str(&toml_str).unwrap();

        assert_eq!(config.settings.timeout, 300);
        assert_eq!(config.settings.poll_interval_ms, 50);
        assert_eq!(config.settings.command_retry_count, 5);
    }
}
