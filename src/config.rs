//! 软件配置模块。
//!
//! 从 `config/config.toml` 文件中加载串口参数、服务器参数和全局设置。

use log::info;
use serde::Deserialize;
use serialport::{DataBits, Parity, StopBits};

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
    /// 通信超时（毫秒，预留）。
    #[serde(rename = "timeout")]
    pub _timeout: u64,
}

/// 顶层配置结构，对应 `config.toml` 全部内容。
#[derive(Deserialize, Debug, Clone)]
pub struct Config {
    pub server: Server,
    pub serial: Serial,
    /// 杂项设置（预留）。
    #[serde(rename = "settings")]
    pub _settings: Settings,
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
