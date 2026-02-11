//! 公共数据模型模块。
//!
//! 将系统中通用的消息类型、命令结构、设备状态、运行时状态和值类型
//! 按关注点拆分为独立子模块。

pub mod command;
pub mod device;
pub mod message;
pub mod state;
pub mod value;

// 重新导出常用类型，方便外部引用
pub use command::{Command, CommandType, SendData};
pub use device::DeviceStatus;
pub use message::MessageType;
pub use state::{PortRuntimeState, SystemState};
pub use value::Value;

/// 计算字节数组的累加和校验值（溢出自动回绕）。
pub fn get_sum(data: &[u8]) -> u8 {
    data.iter().fold(0u8, |acc, &b| acc.wrapping_add(b))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn get_sum_normal() {
        assert_eq!(get_sum(&[1, 2, 3, 4]), 10);
    }

    #[test]
    fn get_sum_empty() {
        assert_eq!(get_sum(&[]), 0);
    }

    #[test]
    fn get_sum_wrapping_overflow() {
        // 0xFF + 0x01 = 0x00 (wrapping), + 0x02 = 0x02
        assert_eq!(get_sum(&[0xFF, 0x01, 0x02]), 0x02);
    }

    #[test]
    fn get_sum_single() {
        assert_eq!(get_sum(&[0xAB]), 0xAB);
    }
}
