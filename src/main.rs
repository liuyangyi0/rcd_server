mod csv_parser;
mod serial_manager;
mod common;
mod tcp_server;
mod file_processor;
mod serial_port_config;
mod config;

// use serialport::{self, DataBits, Parity, StopBits};  // 引入serial port库，用于串口通信。
use std::{env, io, vec};
use std::collections::HashMap;
use std::io::ErrorKind;
use std::sync::{Arc, mpsc};
use tokio::io::Result;               // 引入IO结果类型。
// use bincode;
use crate::common::{Command, PortRuntimeState, SystemState};
use crate::csv_parser::DeviceConfiguration;
use crate::file_processor::read_and_process_files;
use crate::serial_manager::{SerialConfig, start_serial_thread_1};
use crate::serial_port_config::SerialPortConfig;
use crate::tcp_server::{run_tcp_server_1, run_tcp_server_2, DeviceStatus}; // 引入串口管理模块。

use tokio::sync::{mpsc as tokio_mpsc, Mutex as TokioMutex};
use bounded_vec_deque::BoundedVecDeque;




// 程序主函数，设置并启动TCP服务器和串口读取线程。
#[tokio::main]
async fn main() -> Result<()> {
    // 创建全局发送器列表 当有新的客户端连接时，将其本地发送器注册到全局发送器列表中
    let global_sender = Arc::new(TokioMutex::new(Vec::new()));
    // 创建新全局发送器列表（用于不带帧头的客户端）
    let global_sender_plain = Arc::new(TokioMutex::new(Vec::new()));

    // 初始化配置
    let software_config = config::init_config().unwrap();
    // 创建系统状态列表
    let system_state = SystemState::new();
    let system_record = Arc::new(TokioMutex::new(BoundedVecDeque::<String>::new(20000)));
    
    // 初始化 串口配置和发送器列表
    match init(global_sender.clone(),global_sender_plain.clone(), software_config, system_state.clone(),system_record.clone()).await {
        Ok((configs, txs)) => {
            // 启动 TCP 服务器
            // match run_tcp_server_1(configs, txs, global_sender.clone(), system_state.clone(), system_record).await {
            //     Ok(_) => println!("Server terminated successfully."),
            //     Err(e) => eprintln!("Server failed with error: {}", e),
            // }

            // 启动原有 TCP 服务器（带帧头）
            let global_sender_clone = global_sender.clone();
            let system_state_clone = system_state.clone();
            let system_record_clone = system_record.clone();
            let configs_clone = configs.clone();
            let txs_clone = txs.clone();
            tokio::spawn(async move {
                match run_tcp_server_1(configs_clone, txs_clone, global_sender_clone, system_state_clone, system_record_clone).await {
                    Ok(_) => println!("Server 1 terminated successfully."),
                    Err(e) => eprintln!("Server 1 failed with error: {}", e),
                }
            });

            // 启动新 TCP 服务器（不带帧头）
            let global_sender_plain_clone = global_sender_plain.clone();
            let system_state_clone2 = system_state.clone();
            let system_record_clone2 = system_record.clone();
            let configs_clone2 = configs.clone();
            let txs_clone2 = txs.clone();
            match run_tcp_server_2(configs_clone2, txs_clone2, global_sender_plain_clone, system_state_clone2, system_record_clone2).await {
                Ok(_) => println!("Server 2 terminated successfully."),
                Err(e) => eprintln!("Server 2 failed with error: {}", e),
            }

        },
        Err(e) => {
            eprintln!("初始化失败: {}", e);
        }
    }

    Ok(())
}

///Arc<TokioMutex<Vec<Arc<tokio_mpsc::Sender<DeviceStatus>>>>> 表示一个线程安全的、可以异步访问的动态数组，数组中的每个元素都是一个可以发送 DeviceStatus 类型消息的发送者
async fn init(global_sender: Arc<TokioMutex<Vec<Arc<tokio_mpsc::Sender<DeviceStatus>>>>>,
              global_sender_plain: Arc<TokioMutex<Vec<Arc<tokio_mpsc::Sender<DeviceStatus>>>>>,
              software_config: config::Config,
              system_state: SystemState,
              system_record: Arc<TokioMutex<BoundedVecDeque<String>>>)
    ->  io::Result<(Vec<SerialPortConfig>, Vec<mpsc::Sender<Command>>)> {
    let exe_path = env::current_exe()?; // 获取可执行文件路径
    let exe_dir = exe_path.parent().ok_or_else(|| io::Error::new(ErrorKind::NotFound, "无法获取可执行文件目录"))?; // 获取可执行文件目录
    let binding = exe_dir.join("config");
    let path = binding.as_path();

    // 定义文件名模式，这里以 .txt 结尾的文件
    let pattern = r"^rcd.*\.csv$";

    let mut files: Vec<std::path::PathBuf> = vec![];

    match read_and_process_files(path, pattern) {
        Ok(f) => {
            files = f;
        }
        Err(e) => {
            eprintln!("读取文件时出错: {}", e);
        }
    }

    //打印文件列表
    // for file in files.iter() {
    //     println!("文件: {:?}", file);
    // }    //files 遍历
    let mut serial_port_configs: Vec<SerialPortConfig> = vec![];
    let mut txs: Vec<mpsc::Sender<Command>> = vec![];

    for file in files {
        match csv_parser::parse_csv(file) {
            Ok((conf, recs)) => {
                    let mut found = false;
                    // 判断是否有重复的串口配置
                    for serial_port_config in serial_port_configs.iter_mut() {
                        if serial_port_config.port_number == conf.com {
                            let device_configuration = DeviceConfiguration::new(conf.clone(), recs.clone());
                            serial_port_config.commands.push(device_configuration);
                            found = true;
                            break;
                        }
                    }
                    // 如果没有找到相同的串口配置，创建新的配置
                    if !found {
                        let mut  new_config = SerialPortConfig::new(conf.com.clone(),conf.baud_rate, conf.is_special, vec![]);
                        let device_configuration = DeviceConfiguration::new(conf.clone(), recs.clone());
                        new_config.commands.push(device_configuration);
                        serial_port_configs.push(new_config);
                    }
            }
            Err(e) => println!("读取csv: {}", e),
        }
    }

    // 循环 serial_port_configs 创建串口读取线程
    for (_, serial_port_config) in serial_port_configs.iter_mut().enumerate() {

        let serial_config = SerialConfig::new(serial_port_config.port_number.clone(), serial_port_config.baud_rate,
                                              software_config.serial.data_bits, software_config.serial.stop_bits, software_config.serial.parity);

        let (tx, rx) = mpsc::channel();
        txs.push(tx);
        
        // 创建串口状态 串口状态默认为打开 返回给客户端
        let mut state = PortRuntimeState{
            port_number: serial_port_config.port_number.clone(),
            status: true,
            device_status: HashMap::new(),
        };
        for device in serial_port_config.commands.iter() {
            state.device_status.insert(device.config.device_id, true);
        }
        system_state.add_port(state).await;
        
        start_serial_thread_1(global_sender.clone(), global_sender_plain.clone(),serial_config,serial_port_config.clone(),rx, software_config.clone(), system_state.clone(), system_record.clone()).await;
    }

    Ok((serial_port_configs,txs))
}

