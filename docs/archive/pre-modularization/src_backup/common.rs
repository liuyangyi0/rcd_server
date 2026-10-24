use std::collections::HashMap;
use std::sync::Arc;
use serde::{Deserialize, Serialize};
use crate::tcp_server::DeviceStatus;
use tokio::sync::Mutex;

//消息类型枚举
#[derive(Serialize, Deserialize, Debug, Clone)]
pub enum MessageType {
    //Command(SendData),
    Command(Command),
    DeviceStatus(DeviceStatus),
    QueryAllStatus,
    AllStatus(Vec<PortRuntimeState>),
    QueryRecord,
    AllRecord(Vec<String>),
}


// 命令结构体，包含 `com` 字段和 `command` 字段
#[derive(Serialize, Deserialize, Debug, Clone)]
pub struct Command {
    pub com: String,
    pub command: CommandType,
}

// 枚举，包含不同的命令类型
#[derive(Serialize, Deserialize, Debug, Clone)]
pub enum CommandType {
    SendData(SendData),
    PortStatus(PortStatus),
    DeviceSetting(DeviceSetting),
}

// 各个命令类型的具体结构体，不再包含 `com` 字段
#[derive(Serialize, Deserialize, Debug)]
pub struct SendData {
    pub device_id: u32,
    pub command: Vec<u8>,
}

impl SendData {
    /// 将 `command` 转换为十六进制字符串
    pub fn command_as_string(&self) -> String {
        self.command.iter()
            .map(|byte| format!("{:02X}", byte)) // 使用大写十六进制，如果需要小写可用 {:02x}
            .collect()
    }
}

// 串口状态结构体
#[derive(Serialize, Deserialize, Debug, Clone)]
pub struct PortStatus {
    pub device_status: bool,
}

//  设备设置结构体
#[derive(Serialize, Deserialize, Debug, Clone)]
pub struct DeviceSetting {
    pub device_id: u8,
    pub device_status: bool,
}



//
impl Clone for SendData {
    fn clone(&self) -> Self {
        SendData {
            device_id: self.device_id,
            command: self.command.clone(),
        }
    }
}

//串口状态 结构体
#[derive(Serialize, Deserialize, Debug, Clone)]
pub struct PortRuntimeState{
    pub port_number: String,
    pub status: bool,
    pub device_status: HashMap<u8,bool>
}
#[derive(Debug, Clone)]
pub struct SystemState {
    pub inner: Arc<Mutex<Vec<PortRuntimeState>>>,
}


impl SystemState {
    /// 创建一个新的 SystemState 实例
    pub fn new() -> Self {
        SystemState {
            inner: Arc::new(Mutex::new(Vec::new())),
        }
    }

    /// 添加一个新的 PortRuntimeState
    pub async fn add_port(&self, port: PortRuntimeState) {
        let mut ports = self.inner.lock().await;
        ports.push(port);
    }

    /// 根据 port_number 修改 PortRuntimeState 的 status
    /// 返回是否成功找到并修改了端口
    pub async fn set_port_status(&self, port_number: &str, status: bool) -> Result<(), String> {
        let mut ports = self.inner.lock().await;
        match ports.iter_mut().find(|p| p.port_number == port_number) {
            Some(port) => {
                port.status = status;
                Ok(())
            },
            None => Err(format!("Port {} not found", port_number)),
        }
    }

    /// 为特定端口添加一个新的 device_status 条目
    /// 如果端口不存在，返回错误
    /// 如果设备 ID 已存在，返回错误
    pub async fn add_device_status(&self, port_number: &str, device_id: u8, status: bool) -> Result<(), String> {
        let mut ports = self.inner.lock().await;
        match ports.iter_mut().find(|p| p.port_number == port_number) {
            Some(port) => {
                if port.device_status.contains_key(&device_id) {
                    Err(format!("Device ID {} already exists in port {}", device_id, port_number))
                } else {
                    port.device_status.insert(device_id, status);
                    Ok(())
                }
            },
            None => Err(format!("Port {} not found", port_number)),
        }
    }

    /// 修改特定端口中某个设备的状态
    /// 如果端口或设备不存在，返回错误
    pub async fn set_device_status(&self, port_number: &str, device_id: u8, status: bool) -> Result<(), String> {
        let mut ports = self.inner.lock().await;
        match ports.iter_mut().find(|p| p.port_number == port_number) {
            Some(port) => {
                match port.device_status.get_mut(&device_id) {
                    Some(device_status) => {
                        *device_status = status;
                        Ok(())
                    },
                    None => Err(format!("Device ID {} not found in port {}", device_id, port_number)),
                }
            },
            None => Err(format!("Port {} not found", port_number)),
        }
    }

    /// 获取当前系统状态的副本
    pub async fn get_state(&self) -> Vec<PortRuntimeState> {
        let ports = self.inner.lock().await;
        ports.clone()
    }
}



// 值类型枚举
#[derive(Debug)]
#[derive(PartialEq)]
#[derive(Clone)]
#[derive(Serialize, Deserialize)]
pub enum Value {
    UInt(u32),
    Bool(bool),
    Float(f32),
}

pub fn get_sum(array: &[u8]) -> u8 {
    let mut checksum: u8 = 0;
    for &item in array {
        checksum = checksum.wrapping_add(item);
    }
    checksum
}