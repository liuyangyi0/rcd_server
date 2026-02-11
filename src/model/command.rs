//! 命令体系类型。

use serde::{Deserialize, Serialize};

/// 指向特定串口的命令。
#[derive(Serialize, Deserialize, Debug, Clone)]
pub struct Command {
    /// 目标串口号，例如 `"COM3"`。
    pub com: String,
    /// 具体命令类型。
    pub command: CommandType,
}

/// 命令类型枚举。
#[derive(Serialize, Deserialize, Debug, Clone)]
pub enum CommandType {
    /// 向设备发送原始数据。
    SendData(SendData),
    /// 设置整个串口的启用/禁用状态。
    PortStatus(PortStatus),
    /// 设置单个设备的启用/禁用状态。
    DeviceSetting(DeviceSetting),
}

/// 发送到设备的原始数据。
#[derive(Serialize, Deserialize, Debug, Clone)]
pub struct SendData {
    /// 目标设备 ID。
    pub device_id: u32,
    /// 待发送的字节序列。
    pub command: Vec<u8>,
}

impl SendData {
    /// 将 `command` 转换为大写十六进制字符串（如 `"0A1BFF"`）。
    pub fn command_as_string(&self) -> String {
        self.command
            .iter()
            .map(|b| format!("{:02X}", b))
            .collect()
    }
}

/// 串口启用/禁用状态。
#[derive(Serialize, Deserialize, Debug, Clone)]
pub struct PortStatus {
    pub device_status: bool,
}

/// 单个设备启用/禁用设置。
#[derive(Serialize, Deserialize, Debug, Clone)]
pub struct DeviceSetting {
    pub device_id: u8,
    pub device_status: bool,
}
