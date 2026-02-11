//! 串口端口配置模块。
//!
//! 将同一串口下的多个设备配置聚合在一起。

use std::collections::HashMap;
use crate::csv_parser::{self, DeviceConfig};
use crate::model::PortRuntimeState;

/// 单个串口的聚合配置（一个串口可挂载多个设备）。
#[derive(Debug, Clone)]
pub struct SerialPortConfig {
    /// 串口号，如 `"COM3"`。
    pub port_number: String,
    /// 波特率。
    pub baud_rate: u32,
    /// 是否为特殊串口（Mark/Space 奇偶校验模式）。
    pub is_special: bool,
    /// 该串口下所有设备的配置列表。
    pub devices: Vec<DeviceConfig>,
    /// 串口通信状态。
    pub status: bool,
}

impl SerialPortConfig {
    /// 创建新的串口配置。
    pub fn new(
        port_number: String,
        baud_rate: u32,
        is_special: bool,
        devices: Vec<DeviceConfig>,
    ) -> Self {
        Self {
            port_number,
            baud_rate,
            is_special,
            devices,
            status: true,
        }
    }

    /// 从 CSV 配置头创建（初始无设备）。
    pub fn from_csv_config(conf: &csv_parser::Config) -> Self {
        Self::new(conf.com.clone(), conf.baud_rate, conf.is_special, Vec::new())
    }

    /// 生成该串口的初始运行时状态快照。
    pub fn to_runtime_state(&self) -> PortRuntimeState {
        let device_status: HashMap<u8, bool> = self
            .devices
            .iter()
            .map(|d| (d.config.device_id, true))
            .collect();

        PortRuntimeState {
            port_number: self.port_number.clone(),
            status: true,
            device_status,
        }
    }
}
