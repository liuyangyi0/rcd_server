use serialport::{SerialPort, DataBits, StopBits, Parity};
use std::io::{ErrorKind, Write};  // 正确引入ErrorKind
use std::{thread, time::Duration};
use std::sync::mpsc::Receiver;
use crate::csv_parser::{BitIndex, Record};
use std::collections::{HashMap, VecDeque};
use std::sync::{Arc, Mutex};
use bounded_vec_deque::BoundedVecDeque;
use chrono::{DateTime, Local};
use serde::Deserialize;
use tokio::runtime::Runtime;
use crate::common::{SendData, get_sum, Value, Command, CommandType, SystemState};
use tokio::sync::{mpsc as tokio_mpsc, Mutex as TokioMutex};
use tokio::time::timeout;
use crate::config;
use crate::config::RunLocation;
use crate::serial_port_config::SerialPortConfig;
use crate::tcp_server::DeviceStatus;
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

impl SerialConfig {
    // 构造函数
    pub fn new(port_name: String, baud_rate: u32, data_bits: DataBits, stop_bits: StopBits, parity: Parity) -> Self {
        SerialConfig {
            port_name,
            baud_rate,
            data_bits,
            stop_bits,
            parity
        }
    }
}

//串口数据包
#[derive(Debug, Deserialize, Clone)]
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
    port: Option<Box<dyn SerialPort>>, // 使用Option来允许空值
    command_queue: Arc<Mutex<VecDeque<SendData>>>,  // 线程安全的命令队列
    serial_config: SerialConfig,            // 串口配置
    serial_config_data:SerialPortConfig, //串口配置数据
    global_sender: Arc<TokioMutex<Vec<Arc<tokio_mpsc::Sender<DeviceStatus>>>>>, // 全局发送器
    global_sender_plain: Arc<TokioMutex<Vec<Arc<tokio_mpsc::Sender<DeviceStatus>>>>>,
    index: usize,
    run_on: RunLocation, // 程序运行是主还是备用
    //程序当前是主还是备用
    current_run: RunLocation,
    //整个串口读取超时次数
    time_out_count: u32,
    //串口解析失败次数
    parse_fail_count: u32,
    system_record: Arc<TokioMutex<BoundedVecDeque<String>>>
}

impl SerialManager {
    // 构造函数：初始化串口通信管理器
    pub async fn new(
        serial_config: SerialConfig,
        serial_config_data: SerialPortConfig,
        global_sender: Arc<TokioMutex<Vec<Arc<tokio_mpsc::Sender<DeviceStatus>>>>>,
        global_sender_plain: Arc<TokioMutex<Vec<Arc<tokio_mpsc::Sender<DeviceStatus>>>>>,
        run_on: RunLocation,
        current_run: RunLocation,
        system_record: Arc<TokioMutex<BoundedVecDeque<String>>>
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
            global_sender_plain,
            index: 0,
            run_on,
            current_run,
            time_out_count: 0,
            parse_fail_count: 0,
            system_record
            //serial_struct: true,
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
                eprintln!("{:?}:重新连接串口失败: {:?}", e, self.serial_config.port_name);
                let v: Vec<u8> = vec![0; self.serial_config_data.commands[self.index].config.data_len as usize];

                let data = parse_status(&v, self.serial_config_data.commands[self.index].records.as_slice());
                if self.serial_config_data.commands[self.index].check_data(v.clone()){
                    let data = DeviceStatus { id: self.serial_config_data.commands[self.index].config.com_index as i64 as u64,
                        device_id: self.serial_config_data.commands[self.index].config.device_id,
                        com: self.serial_config_data.port_number.clone(),
                        com_status: false,
                        device_status: false,
                        value: data.clone(),
                        raw_data: v};

                    send_to_all_senders(&self.global_sender,&self.global_sender_plain, data).await;
                }
                //send_to_all_senders(&self.global_sender, DeviceStatus { id: self.serial_config_data.commands[self.index].config.com_index as i64 as u64, com_status: false,device_status: false, value: data.clone()}).await;
                self.port = None;
            }
        }
        // eprintln!("串口重新连接成功");
    }


    // 发送命令到设备的方法
    async fn send_command(&mut self, mut command: SendData) {
        if command.command.len() == 0 {
            return;
        }

        match self.port{
            Some(ref mut port) => {
                if self.serial_config_data.is_special {
                    match port.write(&command.command) {  
                        Ok(_) => {
                            return ;
                        },
                        Err(e) => {
                            eprintln!("写入错误: {:?}", e);
                            self.reconnect().await; // 发生写入错误时，尝试重新连接
                        }
                    }
                }
            },
            None => {
                //打印未打开串口
                //eprintln!("{:}串口未打开",command.com);
                self.reconnect().await; // 串口未打开时，尝试重新连接
            }
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
                //打印未打开串口
                //eprintln!("{:}串口未打开",command.com);
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
                //eprintln!("{:}串口未打开",command.com);
                self.reconnect().await; // 串口未打开时，尝试重新连接
            }
        }

    }


    // 从串口接收数据的方法
    async fn receive_data(&mut self) {
        let mut buffer = vec![0; 1024];
        match self.port {
            Some(ref mut port) => {


                match port.read(&mut buffer) {
                    Ok(mut bytes_read) => {
                        //println!("读取数据: {:?} 数据长度{:?}", &buffer[..bytes_read], bytes_read);
                        self.serial_config_data.commands[self.index].timeout_count = 0;
                        self.time_out_count = 0;
                        // //如果是secondary,则移除 00 之前的数据
                        if self.current_run == RunLocation::Secondary {
                            if let Some(index) = find_double_zero(&buffer[..bytes_read]) {
                                // 提取 '00 00' 之前的数据
                                let before_00_data = &buffer[..index];

                                // 验证长度是否为7
                                if before_00_data.len() == 7 {
                                    // 验证累加和校验
                                    if verify_checksum(before_00_data) {
                                        // 取第一个字节
                                        let first_byte = before_00_data[0];
                                        //first_byte  16进制转10进制

                                        // 对第一个字节进行处理，这里您可以根据需求添加相应的逻辑
                                        //println!("提取的第一个字节: {:?}", first_byte);

                                        for (i, dev) in self.serial_config_data.commands.iter().enumerate() {
                                            if dev.config.device_id == first_byte {
                                                self.index = i;
                                                break;
                                            }
                                        }

                                    } else {
                                        //eprintln!("累加和校验失败");
                                        return;
                                    }
                                } else {
                                    //eprintln!("数据长度错误，期望长度为7，实际长度为{}", before_00_data.len());
                                    return;
                                }


                                buffer = buffer[index..].to_vec();  // 移除 '00 00' 之前的数据
                                bytes_read = bytes_read - index;
                                if bytes_read >= 4 {
                                    let len = buffer[2] as usize + 5;
                                    bytes_read = len;
                                }

                                //print!("移除后的数据: {:?} 数据长度{:?}", &buffer[..bytes_read], bytes_read);
                            }
                        }

                        //处理数据包
                        let data = parse_data_packet(&buffer[..bytes_read]);


                        match data {
                            Ok(packet) => {
                                //解析成功 次数清零
                                self.serial_config_data.commands[self.index].parse_fail_count  = 0;
                                //解析成功 次数清零
                                self.parse_fail_count = 0;
                                //处理数据包内的状态信息
                                let data = parse_status(&packet.status, self.serial_config_data.commands[self.index].records.as_slice());
                                if self.serial_config_data.commands[self.index].check_data(packet.status.clone()){

                                    let data = DeviceStatus { id: self.serial_config_data.commands[self.index].config.com_index as i64 as u64,
                                        device_id: self.serial_config_data.commands[self.index].config.device_id,
                                        com: self.serial_config_data.port_number.clone(),
                                        com_status: true,
                                        device_status: true,
                                        value: data.clone(),
                                        raw_data: buffer[..bytes_read].to_vec()};
                                    send_to_all_senders(&self.global_sender, &self.global_sender,data).await;
                                }

                                //send_to_all_senders(&self.global_sender, DeviceStatus { id: self.serial_config_data.commands[self.index].config.com_index as i64 as u64, com_status: true,device_status: true, value: data.clone()}).await;
                            },
                            Err(_e) => {
                                //解析失败 次数加1
                                self.serial_config_data.commands[self.index].parse_fail_count += 1;
                                //串口解析失败次数加1
                                self.parse_fail_count += 1;

                                //如果解析失败次数大于100次，且是primary,则切换到secondary
                                if self.parse_fail_count > 100 && self.current_run == RunLocation::Primary && self.run_on == RunLocation::Secondary {
                                    self.current_run = RunLocation::Secondary;
                                }

                                //eprintln!("解析数据包错误: {:?} 解析错误次数: {:?}", _e,self.parse_fail_count)
                            },
                        }
                    },
                    Err(e) if e.kind() == ErrorKind::TimedOut => {
                        //对应的设备超时次数加1
                        self.serial_config_data.commands[self.index].timeout_count += 1;
                        //串口读取超时次数加1
                        self.time_out_count += 1;

                        //eprintln!("读取超时 超时次数{:}",self.time_out_count); // 更新超时处理
                        let v: Vec<u8> = vec![0; self.serial_config_data.commands[self.index].config.data_len as usize];
                        let data = parse_status(&v, self.serial_config_data.commands[self.index].records.as_slice());
                        if self.serial_config_data.commands[self.index].check_data(v.clone()){
                            let data = DeviceStatus { id: self.serial_config_data.commands[self.index].config.com_index as i64 as u64,
                                device_id: self.serial_config_data.commands[self.index].config.device_id,
                                com: self.serial_config_data.port_number.clone(),
                                com_status: true,
                                device_status: false,
                                value: data.clone(),
                                raw_data: v};

                            send_to_all_senders(&self.global_sender, &self.global_sender, data).await;
                        }

                        //send_to_all_senders(&self.global_sender, DeviceStatus { id: self.serial_config_data.commands[self.index].config.com_index as i64 as u64, com_status: true,device_status: false, value: data.clone()}).await;

                        //如果串口读取超时次数大于100次，且是primary,则切换到secondary
                        if self.time_out_count > 20 && self.current_run == RunLocation::Secondary && self.run_on == RunLocation::Secondary {
                            self.current_run = RunLocation::Primary;
                        }
                    }
                    Err(e) => {
                        eprintln!("读取错误: {:?}", e);

                        //时间戳
                        {
                            let local: DateTime<Local> = Local::now();
                            let mut record = self.system_record.lock().await;
                            record.push_back(format!(
                                "串口{}时间{} 读取错误",
                                self.serial_config.port_name,
                                local.format("%Y-%m-%d %H:%M:%S")
                            ));
                            // 这里 record 的生命周期在此花括号结束后就被释放
                        }
                        self.reconnect().await; // 发生读取错误时，尝试重新连接
                    }
                }
            },
            None => {
                //时间戳
                {
                    let local: DateTime<Local> = Local::now();
                    let mut record = self.system_record.lock().await;
                    record.push_back(format!(
                        "串口{}时间{} 串口未打开",
                        self.serial_config.port_name,
                        local.format("%Y-%m-%d %H:%M:%S")
                    ));

                    // 这里 record 的生命周期在此花括号结束后就被释放
                }

                eprintln!("串口未打开");
                self.reconnect().await; // 串口未打开时，尝试重新连接
            }
        }

    }
}

// 重新连接串口的函数
async fn open_serial_port_with_retries(
    port_name: &str,
    baud_rate: u32,
    data_bits: DataBits,
    stop_bits: StopBits,
    parity: Parity
) -> Result<Box<dyn SerialPort>, serialport::Error> {
    loop {
        tokio::time::sleep(Duration::from_millis(2000)).await;
        match serialport::new(port_name, baud_rate)
            .data_bits(data_bits)
            .stop_bits(stop_bits)
            .parity(parity)
            .timeout(Duration::from_millis(400))
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


    // 验证数据包长度是否大于4
    if data.len() < 4 {
        return Err("Data packet too short");
    }

    let addr = data[0];
    let cmd = data[1];
    let len = data[2] as usize;  // Len字段值，16进制转十进制

    // 验证从Len字段之后到数据包结尾前（不包括CRC）的字节数是否为Ln+1
    if (len + 2) != (data.len() - 3) {  // 总长度减去Addr, CMD, Len
        return Err("Data packet length mismatch");
    }

    //累加和校验
    if !verify_checksum(data) {
        return Err("Checksum mismatch");
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

// 启动串口通信线程的函数
//global_sender: 全局发送器
//serial_config: 串口配置
//serial_config_data: 串口配置数据
//rx: 接收器
//software_config: 软件配置
//system_state: 系统状态
pub async fn start_serial_thread_1(
    global_sender: Arc<TokioMutex<Vec<Arc<tokio_mpsc::Sender<DeviceStatus>>>>>,
    global_sender_plain: Arc<TokioMutex<Vec<Arc<tokio_mpsc::Sender<DeviceStatus>>>>>,
    serial_config: SerialConfig,
    serial_config_data:SerialPortConfig,
    rx: Receiver<Command>,
    software_config: config::Config,
    system_state: SystemState,
    system_record: Arc<TokioMutex<BoundedVecDeque<String>>>
) -> thread::JoinHandle<()> {
    let mut manager = SerialManager::new(serial_config, serial_config_data, global_sender.clone(), global_sender_plain.clone(), software_config.server.run_on,software_config.server.current_run, system_record).await;

    thread::spawn(move || {
        let rt = Runtime::new().unwrap(); // 创建一个新的Tokio运行时
        rt.block_on(async { // 在运行时中执行异步代码块
            loop {
              
                while let Ok(cmd) = rx.try_recv() {
                    match cmd.command {
                        CommandType::SendData(data) => {
                            manager.command_queue.lock().unwrap().push_back(data);
                        },
                        CommandType::PortStatus(status) => {
                            manager.serial_config_data.status = status.device_status;
                            match system_state
                                .set_port_status(&manager.serial_config_data.port_number, status.device_status)
                                .await
                            {
                                Ok(_) => {
                                    // 成功处理
                                },
                                Err(e) => {
                                    eprintln!("设置端口状态失败: {}", e);
                                    // 进行适当的错误处理，例如重试、记录日志或优雅退出
                                }
                            }


                        },
                        CommandType::DeviceSetting(setting) => {
                            for dev in &mut manager.serial_config_data.commands {
                                if dev.config.device_id == setting.device_id {
                                    dev.is_read = setting.device_status;
                                }
                            }
                            //设置 system_state
                            match system_state
                                .set_device_status(&manager.serial_config_data.port_number, setting.device_id, setting.device_status)
                                .await
                            {
                                Ok(_) => {
                                    // 成功处理
                                },
                                Err(e) => {
                                    eprintln!("设置设备状态失败: {}", e);
                                    // 进行适当的错误处理，例如重试、记录日志或优雅退出
                                }
                            }
                        },
                    }
                    //manager.command_queue.lock().unwrap().push_back(cmd);
                }

                //如果设置为false，则等待
                if !manager.serial_config_data.status {
                    tokio::time::sleep(Duration::from_millis(100)).await;
                    continue;
                }

                //如果程序是Primary，则执行 发送命令  如果是Secondary，则不执行
                match manager.current_run {
                    RunLocation::Primary => {
                        // 处理命令rx.try_recv()

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
                                    if manager.serial_config_data.commands[manager.index].timeout_count > 3 {
                                        if manager.serial_config_data.commands[manager.index].current_round > 10 {
                                            manager.serial_config_data.commands[manager.index].current_round = 0;
                                        }else {
                                            manager.serial_config_data.commands[manager.index].current_round += 1;
                                            //打印当前轮询次数
                                            //println!("当前轮询次数:{}", manager.serial_config_data.commands[manager.index].current_round);
                                            manager.index = (manager.index + 1) % manager.serial_config_data.commands.len(); // 更新索引，并防止溢出
                                            tokio::time::sleep(Duration::from_millis(10)).await; // 暂停以避免过载
                                            continue;
                                        }
                                    }


                                    let dev_config = &manager.serial_config_data.commands[manager.index];
                                    if dev_config.is_read {
                                        let mut c = vec![
                                            dev_config.config.device_id.clone(),
                                            0x08,
                                            0x02,
                                            0x30,
                                            0x10,
                                            dev_config.config.data_len.clone(),
                                        ];
                                        c.push(get_sum(&c));
                                        let q = SendData {device_id: 1, command: c };

                                        manager.send_command(q).await; // 发送查询命令
                                    }else {
                                        tokio::time::sleep(Duration::from_millis(20)).await;
                                        continue;
                                    }
                                    
                                }
                            }
                        }
                    }
                    RunLocation::Secondary => {
                        // while let Ok(cmd) = rx.try_recv() {
                        //     manager.command_queue.lock().unwrap().push_back(cmd);
                        // }
                        {
                            let mut queue = manager.command_queue.lock().unwrap();
                            if let Some(cmd) = queue.pop_front() {
                                drop(queue);
                                manager.send_command(cmd).await; // 发送命令
                            } else {
                                drop(queue);
                            }
                        }

                    }
                }

                tokio::time::sleep(Duration::from_millis(20)).await;
                // 异步接收数据
                manager.receive_data().await; // 以异步方式接收数据


                manager.index = (manager.index + 1) % manager.serial_config_data.commands.len(); // 更新索引，并防止溢出
                // if manager.serial_config_data.commands[manager.index].is_read {
                //
                // }

                // let total_commands = manager.serial_config_data.commands.len();
                // for _ in 0..total_commands {
                //     // 更新索引，并防止溢出
                //     manager.index = (manager.index + 1) % manager.serial_config_data.commands.len();
                //     // 获取当前命令
                //     let current_command = &manager.serial_config_data.commands[manager.index];
                //     // 检查是否为读取命令
                //     if current_command.is_read {
                //         // 满足条件，跳出循环
                //         break;
                //     }
                // }

                // 使用异步sleep
                tokio::time::sleep(Duration::from_millis(1)).await; // 暂停以避免过载
            }
        })
    })
}


fn find_double_zero(buffer: &[u8]) -> Option<usize> {
    buffer.windows(2).position(|window| window == [0, 0])
}



// 发送数据到所有发送器
// pub async fn send_to_all_senders(
//     global_sender: &Arc<TokioMutex<Vec<Arc<tokio_mpsc::Sender<DeviceStatus>>>>>,
//     global_sender_plain: &Arc<TokioMutex<Vec<Arc<tokio_mpsc::Sender<DeviceStatus>>>>>,
//     data: DeviceStatus,
// ) {
//     let mut sender = global_sender.lock().await;
//     let mut indices_to_remove = Vec::new();
//
//     for (i, s) in sender.iter().enumerate() {
//         if let Err(e) = s.send(data.clone()).await {
//             eprintln!("发送数据失败，移除发送器: {:?}", e);
//             indices_to_remove.push(i);
//         }
//     }
//
//     // 逆序移除发送器，防止索引混乱
//     for &i in indices_to_remove.iter().rev() {
//         sender.remove(i);
//     }
//
//     // 发送到新global_sender_plain（不带帧头客户端）
//     let mut sender_plain = global_sender_plain.lock().await;
//     let mut indices_to_remove_plain = Vec::new();
//     for (i, s) in sender_plain.iter().enumerate() {
//         if let Err(e) = s.send(data.clone()).await {
//             eprintln!("发送数据失败，移除发送器 (plain): {:?}", e);
//             indices_to_remove_plain.push(i);
//         }
//     }
//     for &i in indices_to_remove_plain.iter().rev() {
//         sender_plain.remove(i);
//     }
//
// }


// 发送数据到所有发送器
pub async fn send_to_all_senders(
    global_sender: &Arc<TokioMutex<Vec<Arc<tokio_mpsc::Sender<DeviceStatus>>>>>,
    global_sender_plain: &Arc<TokioMutex<Vec<Arc<tokio_mpsc::Sender<DeviceStatus>>>>>,
    data: DeviceStatus,
) {
    // 处理 global_sender
    let senders = {
        let guard = global_sender.lock().await;
        guard.iter().cloned().collect::<Vec<_>>()
    };

    let mut failed = Vec::new();
    for s in senders {
        match timeout(Duration::from_millis(500), s.send(data.clone())).await {
            Ok(Ok(_)) => {}, // 发送成功
            Ok(Err(e)) => {
                eprintln!("发送数据失败: {:?}", e); // 接收端已关闭
                failed.push(s);
            }
            Err(_) => {
                eprintln!("发送数据超时，客户端可能缓慢"); // 缓冲区满或其他超时，视为失败
                failed.push(s);
            }
        }
    }

    if !failed.is_empty() {
        let mut guard = global_sender.lock().await;
        guard.retain(|existing| !failed.iter().any(|f| Arc::ptr_eq(f, existing)));
    }

    // 处理 global_sender_plain
    let senders_plain = {
        let guard = global_sender_plain.lock().await;
        guard.iter().cloned().collect::<Vec<_>>()
    };

    let mut failed_plain = Vec::new();
    for s in senders_plain {
        match timeout(Duration::from_millis(500), s.send(data.clone())).await {
            Ok(Ok(_)) => {}, // 发送成功
            Ok(Err(e)) => {
                eprintln!("发送数据失败 (plain): {:?}", e); // 接收端已关闭
                failed_plain.push(s);
            }
            Err(_) => {
                eprintln!("发送数据超时 (plain)，客户端可能缓慢"); // 缓冲区满或其他超时，视为失败
                failed_plain.push(s);
            }
        }
    }

    if !failed_plain.is_empty() {
        let mut guard = global_sender_plain.lock().await;
        guard.retain(|existing| !failed_plain.iter().any(|f| Arc::ptr_eq(f, existing)));
    }
}