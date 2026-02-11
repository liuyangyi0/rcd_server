//! 设备数据状态类型。

use std::collections::HashMap;

use serde::{Deserialize, Serialize};

use super::value::Value;

/// 设备数据状态——串口线程解析后推送给上层的核心数据结构。
#[derive(Serialize, Deserialize, Debug, Clone)]
pub struct DeviceStatus {
    /// 串口索引编号。
    pub id: u64,
    /// 设备站地址。
    pub device_id: u8,
    /// 串口号。
    pub com: String,
    /// 串口通信是否正常。
    pub com_status: bool,
    /// 设备通信是否正常。
    pub device_status: bool,
    /// 解析后的 KKS 键值对。
    pub value: HashMap<String, Value>,
    /// 原始数据帧。
    pub raw_data: Vec<u8>,
}
