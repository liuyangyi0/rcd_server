use serialport::{SerialPort, DataBits, StopBits, Parity};
use std::io::{ErrorKind, Write};  // 正确引入ErrorKind
use std::{thread, time::Duration};
use std::sync::mpsc::Receiver;
use crate::csv_parser::{BitIndex, Record};
use std::collections::{HashMap, VecDeque};
use std::sync::{Arc, Mutex};
use tokio::runtime::Runtime;
use crate::common::{Command, get_sum, Value};
use tokio::sync::{mpsc as tokio_mpsc, Mutex as TokioMutex};
use crate::serial_port_config::SerialPortConfig;


// 定义一个枚举来表示奇数和偶数
// pub enum NumberType {
//     Odd,
//     Even,
// }



// 串口配置结构体
pub struct SerialConfig {
    pub port_name: String,                    // 串口名称
    pub baud_rate: u32,                       // 串口波特率
    pub data_bits: DataBits,                  // 串口数据位设置
    pub stop_bits: StopBits,                  // 串口停止位设置
    pub parity: Parity,                       // 串口奇偶校验设置
}

//串口数据包
struct DataPacket {
    #[allow(unused)]
    addr: u8,  // 地址
    #[allow(unused)]
    cmd: u8,   // 命令
    #[allow(unused)]
    len: u8,   // 长度
    #[allow(unused)]
    status: Vec<u8>, // 状态信息，键为KKS标识符，值为解析出的值
    #[allow(unused)]
    crc: u8,   // 校验字节
}

impl DataPacket {
    // Define a constructor for DataPacket
    pub fn new(addr: u8, cmd: u8, len: u8, status: Vec<u8>, crc: u8) -> Self {
        DataPacket {
            addr,
            cmd,
            len,
            status,
            crc
        }
    }
}


// 串口通信管理器结构体
struct SerialManager {
    // port: Box<dyn SerialPort>,             // 串口对象
    port: Option<Box<dyn SerialPort>>, // 使用Option来允许空值
    command_queue: Arc<Mutex<VecDeque<Command>>>,  // 线程安全的命令队列
    serial_config: SerialConfig,            // 串口配置
    serial_config_data:SerialPortConfig, //串口配置数据
    global_sender: Arc<TokioMutex<Vec<Arc<tokio_mpsc::Sender<HashMap<String, Value>>>>>>, // 全局发送器
    index: usize,
}

impl SerialManager {
    // 构造函数：初始化串口通信管理器
    pub async fn new(
        serial_config: SerialConfig,
        serial_config_data: SerialPortConfig,
        global_sender: Arc<TokioMutex<Vec<Arc<tokio_mpsc::Sender<HashMap<String, Value>>>>>>
    ) -> Self {
        let port = match open_serial_port_with_retries(
            &serial_config.port_name,
            serial_config.baud_rate,
            serial_config.data_bits,
            serial_config.stop_bits,
            serial_config.parity
        ).await {
            Ok(port) => Some(port),
            Err(e) => {
                eprintln!("Error opening serial port: {:?}", e);
                None
            }
        };

        SerialManager {
            port,
            command_queue: Arc::new(Mutex::new(VecDeque::new())),
            serial_config,
            serial_config_data,
            global_sender,
            index: 0,
        }
    }


    // 重新连接串口的方法
    async fn reconnect(&mut self) {
        match  open_serial_port_with_retries(&self.serial_config.port_name, self.serial_config.baud_rate, self.serial_config.data_bits, self.serial_config.stop_bits, self.serial_config.parity).await{
            Ok(port) => {
                self.port = Some(port);
                println!("串口重新连接成功");
            },
            Err(e) => {
                eprintln!("重新连接串口失败: {:?}", e);
                self.port = None;
            }
        }
        // eprintln!("串口重新连接成功");
    }

    // async fn open_port(&mut self) {
    //     let result = open_serial_port_with_retries(
    //         &self.serial_config.port_name,
    //         self.serial_config.baud_rate,
    //         self.serial_config.data_bits,
    //         self.serial_config.stop_bits,
    //         self.serial_config.parity
    //     ).await;
    //
    //     println!("parity {}", self.serial_config.parity);
    //
    //     match result {
    //         Ok(port) => {
    //             self.port = Some(port);
    //             println!("串口打开成功");
    //         },
    //         Err(e) => {
    //             eprintln!("打开串口失败: {:?}", e);
    //             self.port = None;
    //         }
    //     }
    // }


    // async fn close_port(&mut self) {
    //     if self.port.is_some() {
    //         println!("串口正在关闭...");
    //         self.port = None;  // 将 port 设置为 None，强制调用 Drop trait
    //     } else {
    //         println!("串口已经是关闭状态");
    //     }
    // }



    // 发送命令到设备的方法
    async fn send_command(&mut self, mut command: Command) {
        if command.command.len() == 0 {
            return;
        }

        let selected_data = vec![command.command[0]];  // 创建一个只包含所选元素的新Vec
        //移除第一个元素
        command.command.remove(0);

        match self.port{
            Some(ref mut port) => {
                port.set_parity(Parity::Mark).expect("TODO: panic message");


                if let Err(e) = port.write(&selected_data) {
                    eprintln!("写入错误: {:?}", e);
                    //self.reconnect().await; // 发生写入错误时，尝试重新连接
                }

                if let Err(e) = port.flush() {
                    eprintln!("flush错误: {:?}", e);
                    //self.reconnect().await; // 发生写入错误时，尝试重新连接
                }
            },
            None => {
                eprintln!("串口未打开");
                self.reconnect().await; // 串口未打开时，尝试重新连接
            }
        }

        tokio::time::sleep(Duration::from_millis(2)).await;

        match self.port{
            Some(ref mut port) => {
                port.set_parity(Parity::Space).expect("TODO: panic message");
                if let Err(e) = port.write(&command.command) {
                    eprintln!("写入错误: {:?}", e);
                    self.reconnect().await; // 发生写入错误时，尝试重新连接
                }
            },
            None => {
                eprintln!("串口未打开");
                self.reconnect().await; // 串口未打开时，尝试重新连接
            }
        }


        //暂停一段时间
        //tokio::time::sleep(Duration::from_millis(20)).await;
    }

    // async fn try_set_parity(&mut self, parity: Parity) {
    //     match self.port {
    //         Some(ref mut port) => {
    //             if let Err(e) = port.set_parity(parity) {
    //                 eprintln!("设置错误: {:?}", e);
    //                 self.reconnect().await; // 设置发生错误时，尝试重新连接
    //             }
    //         },
    //         None => {
    //             eprintln!("串口未打开");
    //             self.reconnect().await; // 串口未打开时，尝试重新连接
    //         }
    //     }
    // }

    // async fn set_parity_based_on_number_type(&mut self, parity: NumberType) {
    //     match parity {
    //         NumberType::Odd => self.try_set_parity(Parity::Odd).await,
    //         NumberType::Even => self.try_set_parity(Parity::Even).await,
    //     }
    // }


    //计算字节1个个数 是奇数还是偶数
    // fn count_bits(data: &[u8]) -> NumberType {
    //     // Count the total number of '1' bits in all bytes
    //     let total_bits: usize = data.iter()
    //         .map(|&byte| byte.count_ones() as usize)
    //         .sum();
    //     // 根据总数的奇偶性返回枚举值
    //     if total_bits % 2 == 0 {
    //         NumberType::Even
    //     } else {
    //         NumberType::Odd
    //     }
    // }

    // 从串口接收数据的方法
    async fn receive_data(&mut self) {
        let mut buffer = vec![0; 1024];
        match self.port {
            Some(ref mut port) => {
                match port.read(&mut buffer) {
                    Ok(bytes_read) => {
                        println!("接收到数据: {:?}", &buffer[..bytes_read]);
                        self.serial_config_data.commands[self.index].timeout = 0;
                        //处理数据包
                        let data = parse_data_packet(&buffer[..bytes_read]);
                        match data {
                            Ok(packet) => {
                                //处理数据包内的状态信息
                                let data = parse_status(&packet.status, self.serial_config_data.commands[self.index].records.as_slice());
                                //println!("解析数据包: {:?}", data);
                                //将数据发送到全局发送器
                                let sender = self.global_sender.lock().await;
                                for s in sender.iter() {
                                    if let Err(e) = s.send(data.clone()).await {
                                        eprintln!("发送数据失败: {:?}", e);
                                    }
                                }
                            },
                            Err(e) => eprintln!("解析数据包错误: {:?}", e),
                        }
                    },
                    Err(e) if e.kind() == ErrorKind::TimedOut => {
                        self.serial_config_data.commands[self.index].timeout += 1;
                        eprintln!("读取超时"); // 更新超时处理
                    }
                    Err(e) => {
                        eprintln!("读取错误: {:?}", e);
                        self.reconnect().await; // 发生读取错误时，尝试重新连接
                    }
                }
            },
            None => {
                eprintln!("串口未打开");
                self.reconnect().await; // 串口未打开时，尝试重新连接
            }
        }

    }
}

async fn open_serial_port_with_retries(
    port_name: &str,
    baud_rate: u32,
    data_bits: DataBits,
    stop_bits: StopBits,
    parity: Parity
) -> Result<Box<dyn SerialPort>, serialport::Error> {
    loop {
        tokio::time::sleep(Duration::from_millis(1000)).await;
        match serialport::new(port_name, baud_rate)
            .data_bits(data_bits)
            .stop_bits(stop_bits)
            .parity(parity)
            .timeout(Duration::from_millis(50))
            .open() {
            Ok(port) => return Ok(port), // 直接返回port，不需要再次包装
            Err(e) => {
                return Err(e); // 返回最后一次尝试的错误
                // eprintln!("打开串口失败: {:?}", e);
                // tokio::time::sleep(Duration::from_millis(1000)).await;
            }
        }
    }
}


// 启动串口通信线程的函数
pub async fn start_serial_thread_1(
    global_sender: Arc<TokioMutex<Vec<Arc<tokio_mpsc::Sender<HashMap<String, Value>>>>>>,
    serial_config: SerialConfig,
    serial_config_data:SerialPortConfig,
    rx: Receiver<Command>,
) -> thread::JoinHandle<()> {
    let mut manager = SerialManager::new(serial_config, serial_config_data, global_sender.clone()).await;

    thread::spawn(move || {
        let rt = Runtime::new().unwrap(); // 创建一个新的Tokio运行时
        // let mut query_index = 0; // 添加一个索引来追踪当前应发送的查询命令
        rt.block_on(async { // 在运行时中执行异步代码块
            loop {
                // 处理命令rx.try_recv()
                while let Ok(cmd) = rx.try_recv() {
                    manager.command_queue.lock().unwrap().push_back(cmd);
                }

                // 处理命令队列
                {
                    let mut queue = manager.command_queue.lock().unwrap();
                    if let Some(cmd) = queue.pop_front() {
                        drop(queue);
                        manager.send_command(cmd).await; // 发送命令
                    } else {
                        drop(queue);

                        //如果超时次数大于等于3，且当前轮询次数大于10，则切换到下一个设备
                        if !manager.serial_config_data.commands.is_empty() {
                            if manager.serial_config_data.commands[manager.index].timeout > 3 {
                                if manager.serial_config_data.commands[manager.index].current_round > 10 {
                                    manager.serial_config_data.commands[manager.index].current_round = 0;
                                }else {
                                    manager.serial_config_data.commands[manager.index].current_round += 1;
                                    //打印当前轮询次数
                                    println!("当前轮询次数:{}", manager.serial_config_data.commands[manager.index].current_round);
                                    manager.index = (manager.index + 1) % manager.serial_config_data.commands.len(); // 更新索引，并防止溢出
                                    tokio::time::sleep(Duration::from_millis(200)).await; // 暂停以避免过载
                                    continue;
                                }
                            }


                            let dev_config = &manager.serial_config_data.commands[manager.index];
                            let mut c = vec![
                                dev_config.config.device_id.clone(),
                                0x08,
                                0x02,
                                0x30,
                                0x10,
                                dev_config.config.data_len.clone(),
                            ];
                            c.push(get_sum(&c));
                            let q = Command {com: String::from(""), device_id: 1, command: c };

                           // let query = queries[query_index % queries.len()].clone(); // 循环使用查询命令
                            manager.send_command(q).await; // 发送查询命令

                        }
                    }
                }
                tokio::time::sleep(Duration::from_millis(30)).await;
                // 异步接收数据
                manager.receive_data().await; // 以异步方式接收数据
                manager.index = (manager.index + 1) % manager.serial_config_data.commands.len(); // 更新索引，并防止溢出

                // 使用异步sleep
                tokio::time::sleep(Duration::from_millis(1)).await; // 暂停以避免过载
            }
        })
    })
}


//累加和校验 超出255会自动回到0
fn verify_checksum(data: &[u8]) -> bool {
    if data.is_empty() {
        return false;
    }

    let checksum: u8 = data.iter()
        .take(data.len() - 1)
        .fold(0u8, |acc, &x| acc.wrapping_add(x));
    let received_checksum = *data.last().unwrap();

    checksum == received_checksum
}


//解析数据
fn parse_data_packet(data: &[u8]) -> Result<DataPacket, &'static str> {
    //累加和校验
    if !verify_checksum(data) {
        return Err("Checksum mismatch");
    }

    // 验证数据包长度是否大于4
    if data.len() < 4 {
        return Err("Data packet too short");
    }

    let addr = data[0];
    let cmd = data[1];
    let len = data[2] as usize;  // Len字段值，16进制转十进制

    // 验证从Len字段之后到数据包结尾前（不包括CRC）的字节数是否为Ln+1
    if (len + 2) != (data.len() - 3) {  // 总长度减去Addr, CMD, Len，再加上1
        return Err("Data packet length mismatch");
    }

    let crc = data[data.len() - 1];
    // 根据Len的解释取出状态数据
    let status_bytes = data[3..data.len() - 1].to_vec();  // 直接复制状态数据到Vec<u8>

    Ok(DataPacket::new(addr, cmd, len as u8, status_bytes, crc))
}

//解析状态
fn parse_status(data: &[u8], records: &[Record]) -> HashMap<String, Value> {
    let mut results = HashMap::new();
    for record in records {
        let byte_index = record.byte_index as usize - 1;  // 确保byte_index是从0开始的索引
        if byte_index < data.len() {
            let value = match &record.bit_index {
                BitIndex::Single(bit) => {
                    let bit = *bit as usize;
                    ((data[byte_index] >> bit) & 1) as u32
                },
                BitIndex::Range(range) => {

                    let total_bits = (data.len() * 8) as u32; // 计算数组总共包含的位数，并将结果转换为u32

                    let start_bit = *range.start();
                    let end_bit = *range.end();
                    let start_byte_index = byte_index as usize + (start_bit / 8) as usize;
                    let end_byte_index = byte_index as usize + (end_bit / 8) as usize;


                    //打印一下
                    if start_bit > end_bit  || end_bit >= total_bits {
                        panic!("Invalid bit range or start byte"); // 如果范围无效或开始字节不正确，则抛出错误
                    }
                    let mut combined_data = 0u32;
                    for i in start_byte_index..=end_byte_index{
                        let byte = data[i as usize] as u32;
                            if record.lh == 1{
                                combined_data = (combined_data << 8) | byte;
                            }else {
                                combined_data |= byte << (8 * (i as usize - start_byte_index));
                            }
                    }

                    // 3. 提取特定位
                    let bit_offset = (start_bit % 8) as u32;
                    let num_bits = end_bit - start_bit + 1;
                    let mask = (1u32 << num_bits) - 1;
                    let result = (combined_data >> bit_offset) & mask;
                    result
                },
            };

            let value = match record.type_.as_ref() {
                "uint" => Value::UInt(value),
                "bool" => Value::Bool(value != 0),
                "float" => {
                    // 假设浮点数解析的简化示例
                    Value::Float((value as u32 as f32) / 100.0)
                },
                _ => continue,
            };
            // println!("{:?}", value.clone());
            results.insert(record.kks.clone(), value);
        }
    }
    results
}

