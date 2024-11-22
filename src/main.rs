mod csv_parser;
mod serial_manager;
mod common;
mod tcp_server;
mod file_processor;
mod serial_port_config;
mod config;

// use serialport::{self, DataBits, Parity, StopBits};  // 引入serial port库，用于串口通信。
use std::{env, io};
use std::io::ErrorKind;
use std::sync::{Arc, mpsc};
use tokio::io::Result;               // 引入IO结果类型。
// use bincode;
use crate::common::{SendData};
use crate::csv_parser::DeviceConfiguration;
use crate::file_processor::read_and_process_files;
use crate::serial_manager::{SerialConfig, start_serial_thread_1};
use crate::serial_port_config::SerialPortConfig;
use crate::tcp_server::{run_tcp_server_1, DeviceStatus}; // 引入串口管理模块。

use tokio::sync::{mpsc as tokio_mpsc, Mutex as TokioMutex};


// 程序主函数，设置并启动TCP服务器和串口读取线程。
#[tokio::main]
async fn main() -> Result<()> {
    // 创建全局发送器列表 当有新的客户端连接时，将其本地发送器注册到全局发送器列表中
    let global_sender = Arc::new(TokioMutex::new(Vec::new()));

    let software_config = config::init_config().unwrap();


    // 初始化 串口配置和发送器列表
    match init(global_sender.clone(), software_config).await {
        Ok((configs, txs)) => {
            // 启动 TCP 服务器
            match run_tcp_server_1(configs, txs, global_sender.clone()).await {
                Ok(_) => println!("Server terminated successfully."),
                Err(e) => eprintln!("Server failed with error: {}", e),
            }
        },
        Err(e) => {
            eprintln!("初始化失败: {}", e);
        }
    }

    Ok(())
}

///Arc<TokioMutex<Vec<Arc<tokio_mpsc::Sender<DeviceStatus>>>>> 表示一个线程安全的、可以异步访问的动态数组，数组中的每个元素都是一个可以发送 DeviceStatus 类型消息的发送者
async fn init(global_sender: Arc<TokioMutex<Vec<Arc<tokio_mpsc::Sender<DeviceStatus>>>>>, software_config: config::Config)
    ->  io::Result<(Vec<SerialPortConfig>, Vec<mpsc::Sender<SendData>>)> {
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
    for file in files.iter() {
        println!("文件: {:?}", file);
    }    //files 遍历
    let mut serial_port_configs: Vec<SerialPortConfig> = vec![];
    let mut txs: Vec<mpsc::Sender<SendData>> = vec![];

    for file in files {
        match csv_parser::parse_csv(file) {
            Ok((conf, recs)) => {
                    let mut found = false;
                    // 判断是否有重复的串口配置
                    for serial_port_config in serial_port_configs.iter_mut() {
                        if serial_port_config.port_number == conf.com {
                            let device_configuration = DeviceConfiguration {
                                config: conf.clone(),
                                timeout_count:0,
                                current_round:0,
                                records: recs.clone(),
                                parse_fail_count:0,
                                site_status: true,
                            };
                            serial_port_config.commands.push(device_configuration);
                            found = true;
                            break;
                        }
                    }
                    // 如果没有找到相同的串口配置，创建新的配置
                    if !found {
                        let mut  new_config = SerialPortConfig::new(conf.com.clone(),vec![]);
                        let device_configuration = DeviceConfiguration {
                            config: conf.clone(),
                            timeout_count:0,
                            current_round:0,
                            records: recs.clone(),
                            parse_fail_count:0,
                            site_status: true,
                        };
                        new_config.commands.push(device_configuration);
                        serial_port_configs.push(new_config);
                    }
            }
            Err(e) => println!("读取csv: {}", e),
        }
    }

    // 循环 serial_port_configs 创建串口读取线程
    for (_, serial_port_config) in serial_port_configs.iter_mut().enumerate() {
        let serial_config = SerialConfig{
            port_name: serial_port_config.port_number.clone(),
            baud_rate: software_config.serial.baud_rate,
            data_bits: software_config.serial.data_bits,
            stop_bits: software_config.serial.stop_bits,
            parity: software_config.serial.parity,
        };
        let (tx, rx) = mpsc::channel();
        txs.push(tx);
        start_serial_thread_1(global_sender.clone(), serial_config,serial_port_config.clone(),rx, software_config.clone()).await;
    }

    Ok((serial_port_configs,txs))
}

