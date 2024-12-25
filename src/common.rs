use serde::{Deserialize, Serialize};
use crate::tcp_server::DeviceStatus;

#[derive(Serialize, Deserialize, Debug)]
pub enum MessageType {
    //Command(SendData),
    Command(CommandType),
    DeviceStatus(DeviceStatus),
}


#[derive(Serialize, Deserialize, Debug)]
pub enum CommandType {
    SendData(SendData),
    PortStatus(PortStatus),
    DeviceStatus(DeviceSetting),
}

// 定义设备状态结构体。
#[derive(Serialize, Deserialize, Debug)]
pub struct SendData {
    pub com: String,
    pub device_id: u32,
    pub command: Vec<u8>,
}

#[derive(Serialize, Deserialize, Debug)]
pub struct PortStatus {
    pub com: String,
    pub device_status: bool,
}

#[derive(Serialize, Deserialize, Debug)]
pub struct DeviceSetting {
    pub id: u64,
    pub device_status: bool,
}



impl Clone for SendData {
    fn clone(&self) -> Self {
        SendData {
            com: self.com.clone(),
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