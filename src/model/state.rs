//! 运行时状态类型。

use std::collections::HashMap;
use std::sync::Arc;

use serde::{Deserialize, Serialize};
use tokio::sync::Mutex;

/// 单个串口的运行时状态快照。
#[derive(Serialize, Deserialize, Debug, Clone)]
pub struct PortRuntimeState {
    /// 串口号。
    pub port_number: String,
    /// 串口是否正常在线。
    pub status: bool,
    /// 该串口下每个设备的在线状态 `(device_id -> is_online)`。
    pub device_status: HashMap<u8, bool>,
}

/// 系统全局状态——管理所有串口及其设备状态。
///
/// 内部使用 `Arc<Mutex<..>>` 保证线程安全，可安全克隆后跨任务共享。
#[derive(Debug, Clone)]
pub struct SystemState {
    inner: Arc<Mutex<Vec<PortRuntimeState>>>,
}

impl SystemState {
    /// 创建空的系统状态。
    pub fn new() -> Self {
        Self {
            inner: Arc::new(Mutex::new(Vec::new())),
        }
    }

    /// 添加一个串口的运行时状态。
    pub async fn add_port(&self, port: PortRuntimeState) {
        self.inner.lock().await.push(port);
    }

    /// 设置指定串口的在线状态。
    pub async fn set_port_status(&self, port_number: &str, status: bool) -> Result<(), String> {
        let mut ports = self.inner.lock().await;
        match ports.iter_mut().find(|p| p.port_number == port_number) {
            Some(port) => {
                port.status = status;
                Ok(())
            }
            None => Err(format!("端口 {} 未找到", port_number)),
        }
    }

    /// 修改指定串口下某个设备的在线状态。
    pub async fn set_device_status(
        &self,
        port_number: &str,
        device_id: u8,
        status: bool,
    ) -> Result<(), String> {
        let mut ports = self.inner.lock().await;
        match ports.iter_mut().find(|p| p.port_number == port_number) {
            Some(port) => match port.device_status.get_mut(&device_id) {
                Some(ds) => {
                    *ds = status;
                    Ok(())
                }
                None => Err(format!("设备 {} 在端口 {} 中未找到", device_id, port_number)),
            },
            None => Err(format!("端口 {} 未找到", port_number)),
        }
    }

    /// 获取当前系统状态的完整快照。
    pub async fn get_state(&self) -> Vec<PortRuntimeState> {
        self.inner.lock().await.clone()
    }
}
