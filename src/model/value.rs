//! 值类型。

use serde::{Deserialize, Serialize};

/// 解析后的设备数值，可为无符号整数、布尔或浮点。
#[derive(Debug, PartialEq, Clone, Serialize, Deserialize)]
pub enum Value {
    UInt(u32),
    Bool(bool),
    Float(f32),
}
