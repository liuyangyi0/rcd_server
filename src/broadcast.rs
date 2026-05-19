//! 多订阅者广播器模块。
//!
//! 基于 `tokio::sync::broadcast` 封装，串口（阻塞线程）可直接调用同步
//! `broadcast()` 下发数据，多个消费者（OPC UA / TCP）通过 `subscribe()`
//! 获取各自的接收端。订阅者生命周期由 `Receiver` drop 自动管理，无需
//! 手动 register / unregister。

use tokio::sync::broadcast;

use crate::model::DeviceStatus;

/// 广播通道默认容量（条）。超出容量时最慢的订阅者会收到 `Lagged` 错误并丢帧。
pub const DEFAULT_CAPACITY: usize = 128;

/// 多订阅者广播器。
///
/// 可在阻塞线程和异步任务间共享：`broadcast()` 为同步方法，不触发 await；
/// `subscribe()` 返回异步 `Receiver`，供 tokio 任务消费。
#[derive(Clone)]
pub struct Broadcaster {
    sender: broadcast::Sender<DeviceStatus>,
}

impl Broadcaster {
    /// 创建默认容量 [`DEFAULT_CAPACITY`] 的广播器。
    pub fn new() -> Self {
        Self::with_capacity(DEFAULT_CAPACITY)
    }

    /// 创建指定容量的广播器。
    pub fn with_capacity(capacity: usize) -> Self {
        let (sender, _) = broadcast::channel(capacity);
        Self { sender }
    }

    /// 订阅广播通道，返回异步接收端。
    ///
    /// `Receiver` drop 时自动注销；慢速消费者可能收到 `RecvError::Lagged`。
    pub fn subscribe(&self) -> broadcast::Receiver<DeviceStatus> {
        self.sender.subscribe()
    }

    /// 广播一条数据（同步，非阻塞）。
    ///
    /// - 无订阅者时 `send` 返回 Err，静默忽略。
    /// - 订阅者通道容量溢出时 tokio 会在该订阅者下次 `recv()` 时返回 `Lagged`，
    ///   不会阻塞此调用。
    pub fn broadcast(&self, data: DeviceStatus) {
        let _ = self.sender.send(data);
    }

    /// 当前订阅者数量。
    #[cfg(test)]
    pub fn receiver_count(&self) -> usize {
        self.sender.receiver_count()
    }
}

impl Default for Broadcaster {
    fn default() -> Self {
        Self::new()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::HashMap;

    fn sample(id: u64) -> DeviceStatus {
        DeviceStatus {
            id,
            device_id: 1,
            com: "COM1".to_string(),
            com_status: true,
            device_status: true,
            value: HashMap::new(),
            raw_data: vec![],
        }
    }

    #[tokio::test]
    async fn broadcast_delivers_to_subscribers() {
        let bc = Broadcaster::new();
        let mut rx1 = bc.subscribe();
        let mut rx2 = bc.subscribe();

        bc.broadcast(sample(1));

        let a = rx1.recv().await.unwrap();
        let b = rx2.recv().await.unwrap();
        assert_eq!(a.id, 1);
        assert_eq!(b.id, 1);
    }

    #[tokio::test]
    async fn broadcast_without_subscribers_is_noop() {
        let bc = Broadcaster::new();
        // 不应 panic，也不应阻塞
        bc.broadcast(sample(1));
        assert_eq!(bc.receiver_count(), 0);
    }

    #[tokio::test]
    async fn subscriber_drop_frees_slot() {
        let bc = Broadcaster::new();
        let rx = bc.subscribe();
        assert_eq!(bc.receiver_count(), 1);
        drop(rx);
        assert_eq!(bc.receiver_count(), 0);
    }

    #[tokio::test]
    async fn lagged_subscriber_yields_lagged_error() {
        let bc = Broadcaster::with_capacity(2);
        let mut rx = bc.subscribe();
        // 超容量发送使订阅者落后
        for i in 0..5 {
            bc.broadcast(sample(i));
        }
        let err = rx.recv().await;
        assert!(matches!(err, Err(broadcast::error::RecvError::Lagged(_))));
    }
}
