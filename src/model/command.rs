//! 命令体系类型。

use serde::{Deserialize, Serialize};
use std::sync::mpsc;
use std::time::Instant;

use crate::operations::CommandPermit;

/// 指向特定串口的命令。
#[derive(Serialize, Deserialize, Debug, Clone)]
pub struct Command {
    /// 目标串口号，例如 `"COM3"`。
    pub com: String,
    /// 具体命令类型。
    pub command: CommandType,
    /// 内部 ACK 关联 ID，仅用于 OPC UA 写命令状态回写。
    #[serde(skip)]
    pub ack_id: Option<u64>,
    /// 内部执行截止时间，防止 OPC 已判定超时后 worker 再执行积压命令。
    #[serde(skip)]
    pub expires_at: Option<Instant>,
    /// In-process control confirmation and command reservation are not wire fields.
    #[serde(skip)]
    pub control_receipt: Option<ControlReceipt>,
    #[serde(skip)]
    pub permit: Option<CommandPermit>,
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
    /// 向设备发送原始数据，并向 OPC UA 写命令变量回写 ACK 状态。
    SendDataWithAck(SendData),
    /// Stop only acquisition; device control commands remain allowed.
    /// Appended to retain the existing bincode discriminants 0..=3.
    DeviceReadSetting(DeviceReadSetting),
}

#[derive(Debug, Clone)]
pub struct ControlReceipt {
    pub revision: u64,
    pub reply: mpsc::Sender<Result<(), String>>,
}

#[derive(Serialize, Deserialize, Debug, Clone)]
pub struct DeviceReadSetting {
    pub device_id: u8,
    pub read_enabled: bool,
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
        self.command.iter().map(|b| format!("{:02X}", b)).collect()
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

/// 串口命令 ACK 状态，供 worker 回写 OPC UA `<COM>_Send` 字符串变量。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum CommandAckStatus {
    /// 命令已写入串口，等待下位机回复。
    Waiting,
    /// 收到下位机 ACK。
    Received,
    /// 写入失败或重试耗尽。
    Failed,
}

impl CommandAckStatus {
    pub fn as_opc_value(self) -> &'static str {
        match self {
            Self::Received => "1",
            Self::Waiting => "2",
            Self::Failed => "3",
        }
    }

    pub fn is_terminal(self) -> bool {
        matches!(self, Self::Received | Self::Failed)
    }
}

/// 从串口 worker 发往 OPC UA 任务的命令 ACK 状态更新。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct CommandAckUpdate {
    pub com: String,
    pub ack_id: Option<u64>,
    pub status: CommandAckStatus,
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn command_ack_status_maps_to_opc_string_values() {
        assert_eq!(CommandAckStatus::Received.as_opc_value(), "1");
        assert_eq!(CommandAckStatus::Waiting.as_opc_value(), "2");
        assert_eq!(CommandAckStatus::Failed.as_opc_value(), "3");
    }

    #[test]
    fn command_ack_status_terminal_only_for_final_states() {
        assert!(CommandAckStatus::Received.is_terminal());
        assert!(CommandAckStatus::Failed.is_terminal());
        assert!(!CommandAckStatus::Waiting.is_terminal());
    }

    #[test]
    fn command_ack_id_is_internal_only() {
        let (reply, _rx) = mpsc::channel();
        let command = Command {
            com: "COM1".into(),
            command: CommandType::SendData(SendData {
                device_id: 1,
                command: vec![1, 2, 3],
            }),
            ack_id: Some(42),
            expires_at: Some(Instant::now()),
            control_receipt: Some(ControlReceipt { revision: 3, reply }),
            permit: None,
        };

        let bytes = bincode::serialize(&command).expect("serialize command");
        let decoded: Command = bincode::deserialize(&bytes).expect("deserialize command");

        assert_eq!(decoded.com, "COM1");
        assert_eq!(decoded.ack_id, None);
        assert_eq!(decoded.expires_at, None);
        assert!(decoded.control_receipt.is_none());
        assert!(decoded.permit.is_none());
        assert_eq!(
            bytes,
            bincode::serialize(&(&command.com, &command.command)).unwrap()
        );
    }

    #[test]
    fn acquisition_read_setting_preserves_old_bincode_variant_indices() {
        let send = SendData {
            device_id: 1,
            command: vec![1, 2],
        };
        let variants = [
            CommandType::SendData(send.clone()),
            CommandType::PortStatus(PortStatus {
                device_status: false,
            }),
            CommandType::DeviceSetting(DeviceSetting {
                device_id: 1,
                device_status: false,
            }),
            CommandType::SendDataWithAck(send),
            CommandType::DeviceReadSetting(DeviceReadSetting {
                device_id: 255,
                read_enabled: false,
            }),
        ];
        for (index, variant) in variants.into_iter().enumerate() {
            let bytes = bincode::serialize(&variant).unwrap();
            assert_eq!(&bytes[..4], &(index as u32).to_le_bytes());
        }
    }
}
