use serde::Deserialize;
use crate::csv_parser::DeviceConfiguration;



// 串口配置包含接收通道

#[derive(Debug, Deserialize, Clone)]
pub struct SerialPortConfig {
    pub port_number: String,      // 串口号
    pub baud_rate : u32,          // 波特率
    pub is_special: bool,                     // 是否是特殊串口
    pub commands: Vec<DeviceConfiguration>,    // 设备配置
    //pub rx_channel: mpsc::Receiver<Command>,   // 发送数据到串口的通道
    //串口通讯状态
    pub status: bool,

}


impl SerialPortConfig {
    //rx: mpsc::Receiver<Command>
    // 构造函数
    pub fn new(port_number: String, baud_rate: u32, is_special: bool, commands: Vec<DeviceConfiguration>) -> SerialPortConfig {

        // 创建 SerialPortConfig 实例
        SerialPortConfig {
            port_number,
            baud_rate,
            is_special,
            commands,
            status: true,
            //rx_channel: rx,
        }
    }

    pub fn clone(&self) -> SerialPortConfig {
        SerialPortConfig {
            port_number: self.port_number.clone(),
            baud_rate: self.baud_rate,
            is_special: self.is_special,
            commands: self.commands.clone(),
            status: self.status,
        }
    }
}

