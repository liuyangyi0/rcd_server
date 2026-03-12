//! 多订阅者广播器模块。
//!
//! 封装串口数据到多个消费者（OPC UA / TCP）的广播分发逻辑。

use std::sync::Arc;

use log::warn;
use tokio::sync::{mpsc, Mutex};

use crate::model::DeviceStatus;

/// 多订阅者广播器：串口线程发送数据，多个消费者（OPC UA / TCP）接收。
///
/// 内部使用 `Arc` 保证线程安全，可安全克隆后跨任务共享。
#[derive(Clone)]
pub struct Broadcaster {
    senders: Arc<Mutex<Vec<Arc<mpsc::Sender<DeviceStatus>>>>>,
}

impl Broadcaster {
    /// 创建空的广播器。
    pub fn new() -> Self {
        Self {
            senders: Arc::new(Mutex::new(Vec::new())),
        }
    }

    /// 注册一个新的订阅者，返回接收端。
    pub async fn subscribe(&self, buffer_size: usize) -> mpsc::Receiver<DeviceStatus> {
        let (tx, rx) = mpsc::channel(buffer_size);
        self.senders.lock().await.push(Arc::new(tx));
        rx
    }

    /// 注册一个已有的发送器。
    pub async fn register(&self, sender: Arc<mpsc::Sender<DeviceStatus>>) {
        self.senders.lock().await.push(sender);
    }

    /// 移除指定的发送器。
    pub async fn unregister(&self, target: &Arc<mpsc::Sender<DeviceStatus>>) {
        self.senders.lock().await.retain(|s| !Arc::ptr_eq(s, target));
    }

    /// 将数据广播给所有订阅者，自动移除已关闭的通道。
    ///
    /// 使用 `try_send` 进行非阻塞发送，避免某个消费者缓冲区满时
    /// 阻塞整个串口线程（串口线程通过 `block_on` 调用此方法）。
    pub async fn broadcast(&self, data: DeviceStatus) {
        let senders: Vec<_> = self.senders.lock().await.clone();

        let mut failed = Vec::new();
        for s in &senders {
            match s.try_send(data.clone()) {
                Ok(()) => {}
                Err(mpsc::error::TrySendError::Full(_)) => {
                    warn!("订阅者缓冲区已满，丢弃本次数据");
                }
                Err(mpsc::error::TrySendError::Closed(_)) => {
                    failed.push(s.clone());
                }
            }
        }

        // 移除已断开的订阅者
        if !failed.is_empty() {
            let mut senders = self.senders.lock().await;
            for f in &failed {
                warn!("订阅者已断开，移除");
                senders.retain(|s| !Arc::ptr_eq(s, f));
            }
        }
    }
}
