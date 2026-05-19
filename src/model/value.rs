//! 值类型。

use serde::{Deserialize, Serialize};

/// 解析后的设备数值，可为无符号整数、布尔或浮点。
#[derive(Debug, PartialEq, Clone, Serialize, Deserialize)]
pub enum Value {
    UInt(u32),
    Bool(bool),
    Float(f32),
    Double(f64),
}

impl Value {
    /// 转换为 `f64`，供计算引擎作为统一中间类型。
    pub fn to_f64(&self) -> f64 {
        match self {
            Value::UInt(n) => *n as f64,
            Value::Bool(b) => {
                if *b {
                    1.0
                } else {
                    0.0
                }
            }
            Value::Float(f) => *f as f64,
            Value::Double(f) => *f,
        }
    }
}
