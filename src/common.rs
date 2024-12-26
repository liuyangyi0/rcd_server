use serde::{Deserialize, Serialize};
use crate::tcp_server::DeviceStatus;

#[derive(Serialize, Deserialize, Debug, Clone)]
pub enum MessageType {
    //Command(SendData),
    Command(Command),
    DeviceStatus(DeviceStatus),
}


// 顶层结构体，包含 `com` 字段和命令枚举
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

#[derive(Serialize, Deserialize, Debug, Clone)]
pub struct PortStatus {
    pub device_status: bool,
}

#[derive(Serialize, Deserialize, Debug, Clone)]
pub struct DeviceSetting {
    pub device_id: u8,
    pub device_status: bool,
}



impl Clone for SendData {
    fn clone(&self) -> Self {
        SendData {
            device_id: self.device_id,
            command: self.command.clone(),
        }
    }
}

// pub struct ChangeParsingScheme {
//     pub com: String,
//     pub device_id: u32,
//     pub command: Vec<u8>,
// }



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