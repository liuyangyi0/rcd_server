use serde::{Deserialize, Serialize};





// 定义设备状态结构体。
#[derive(Serialize, Deserialize, Debug)]
pub struct Command {
    pub device_id: u32,
    pub command: Vec<u8>,
}

impl Clone for Command {
    fn clone(&self) -> Self {
        Command {
            device_id: self.device_id,
            command: self.command.clone(),
            // 手动处理复制逻辑，特别是对于不支持自动Clone的复杂类型
        }
    }
}


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