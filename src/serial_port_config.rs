use std::sync::mpsc;
use std::thread;
use crate::common::Command;
use crate::csv_parser::DeviceConfiguration;

pub struct SerialPortConfig {
    pub port_number: String,      // 串口号
    pub commands: Vec<DeviceConfiguration>,    // 设备配置
    pub tx_channel: mpsc::Sender<Command>,   // 发送数据到串口的通道
}


impl SerialPortConfig {
    // 构造函数
    pub fn new(port_number: String, commands: Vec<DeviceConfiguration>, tx: mpsc::Sender<Command>) -> SerialPortConfig {

        // 创建 SerialPortConfig 实例
        SerialPortConfig {
            port_number,
            commands,
            tx_channel: tx,
        }
    }
}

