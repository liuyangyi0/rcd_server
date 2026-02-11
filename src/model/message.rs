//! 消息协议类型。

use serde::{Deserialize, Serialize};

use super::command::Command;
use super::device::DeviceStatus;
use super::state::PortRuntimeState;

/// 客户端与服务端之间传输的顶层消息类型。
#[derive(Serialize, Deserialize, Debug, Clone)]
pub enum MessageType {
    /// 客户端下发命令（写串口 / 控制端口 / 设备设置）。
    Command(Command),
    /// 服务端推送设备数据状态。
    DeviceStatus(DeviceStatus),
    /// 客户端请求查询全部端口状态。
    QueryAllStatus,
    /// 服务端返回全部端口运行时状态。
    AllStatus(Vec<PortRuntimeState>),
    /// 客户端请求查询操作记录。
    QueryRecord,
    /// 服务端返回所有操作记录。
    AllRecord(Vec<String>),
}
