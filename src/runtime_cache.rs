//! 运行期最新设备状态缓存。
//!
//! 广播通道只负责实时分发，不能保证订阅者启动前的数据仍可见。本缓存按
//! `(com, device_id)` 保存每个设备最近一次解析结果，供 OPC UA 启动预热
//! 和广播落后后的全量重同步使用。

use std::collections::BTreeMap;
use std::sync::{Arc, RwLock};

use crate::model::DeviceStatus;

type StatusKey = (String, u8);

#[derive(Clone, Default)]
pub struct LatestStatusCache {
    inner: Arc<RwLock<BTreeMap<StatusKey, DeviceStatus>>>,
}

impl LatestStatusCache {
    pub fn new() -> Self {
        Self::default()
    }

    pub fn update(&self, status: DeviceStatus) {
        let key = (status.com.clone(), status.device_id);
        let mut guard = self.inner.write().unwrap_or_else(|e| e.into_inner());
        guard.insert(key, status);
    }

    pub fn snapshot_statuses(&self) -> Vec<DeviceStatus> {
        let guard = self.inner.read().unwrap_or_else(|e| e.into_inner());
        guard.values().cloned().collect()
    }

    pub fn len(&self) -> usize {
        let guard = self.inner.read().unwrap_or_else(|e| e.into_inner());
        guard.len()
    }

    pub fn is_empty(&self) -> bool {
        self.len() == 0
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::HashMap;

    use crate::model::Value;

    fn status(com: &str, device_id: u8, kks: &str, value: Value) -> DeviceStatus {
        DeviceStatus {
            id: device_id as u64,
            device_id,
            com: com.to_string(),
            com_status: true,
            device_status: true,
            value: HashMap::from([(kks.to_string(), value)]),
            raw_data: vec![device_id],
        }
    }

    #[test]
    fn update_overwrites_same_device() {
        let cache = LatestStatusCache::new();
        cache.update(status("COM1", 1, "a", Value::UInt(1)));
        cache.update(status("COM1", 1, "a", Value::UInt(2)));

        let snapshot = cache.snapshot_statuses();
        assert_eq!(snapshot.len(), 1);
        assert_eq!(snapshot[0].value.get("a"), Some(&Value::UInt(2)));
    }

    #[test]
    fn snapshot_keeps_all_devices() {
        let cache = LatestStatusCache::new();
        cache.update(status("COM2", 1, "b", Value::UInt(2)));
        cache.update(status("COM1", 2, "a", Value::UInt(1)));

        let snapshot = cache.snapshot_statuses();
        assert_eq!(snapshot.len(), 2);
        assert_eq!(snapshot[0].com, "COM1");
        assert_eq!(snapshot[0].device_id, 2);
        assert_eq!(snapshot[1].com, "COM2");
        assert_eq!(snapshot[1].device_id, 1);
    }
}
