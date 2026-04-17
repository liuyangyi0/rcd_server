//! 运行时状态类型。

use std::collections::HashMap;
use std::sync::{Arc, Mutex};

use serde::{Deserialize, Serialize};

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
/// 操作均为短小的列表查找与字段赋值，使用 `std::sync::Mutex` 即可；
/// 同时避免在阻塞线程（串口 worker）和异步任务（OPC UA / TCP）之间
/// 通过 `block_on` 桥接 `tokio::sync::Mutex`。
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
    pub fn add_port(&self, port: PortRuntimeState) {
        self.lock().push(port);
    }

    /// 设置指定串口的在线状态。
    pub fn set_port_status(&self, port_number: &str, status: bool) -> Result<(), String> {
        let mut ports = self.lock();
        match ports.iter_mut().find(|p| p.port_number == port_number) {
            Some(port) => {
                port.status = status;
                Ok(())
            }
            None => Err(format!("端口 {} 未找到", port_number)),
        }
    }

    /// 修改指定串口下某个设备的在线状态。
    pub fn set_device_status(
        &self,
        port_number: &str,
        device_id: u8,
        status: bool,
    ) -> Result<(), String> {
        let mut ports = self.lock();
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
    pub fn get_state(&self) -> Vec<PortRuntimeState> {
        self.lock().clone()
    }

    /// 内部 lock helper：遇到锁中毒时恢复 inner 值，避免级联 panic。
    fn lock(&self) -> std::sync::MutexGuard<'_, Vec<PortRuntimeState>> {
        self.inner
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner())
    }
}

impl Default for SystemState {
    fn default() -> Self {
        Self::new()
    }
}
