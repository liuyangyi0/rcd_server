use tokio::net::{TcpListener, TcpStream};
use tokio::sync::{mpsc as tokio_mpsc, Mutex as TokioMutex};
use std::sync::{Arc, mpsc};
use futures::stream::StreamExt;
use tokio_util::codec::{Framed, LengthDelimitedCodec};
use std::collections::HashMap;
use std::io;
use std::time::Duration;
use bounded_vec_deque::BoundedVecDeque;
use bytes::Bytes;
use futures::SinkExt;
use serde::{Deserialize, Serialize};
use tokio::time::timeout;
use crate::common::{Command, CommandType, MessageType, SystemState, Value};
use crate::serial_port_config::SerialPortConfig;
use chrono::prelude::*;
use std::fmt::Write;
use bytes::BytesMut;
use tokio::io::AsyncWriteExt;

/// 解析失败时的错误类型

#[derive(Debug)]
pub enum ParseError {
    /// 条目中缺少等号，例如 "foo" 或 "foo=bar=baz"
    MissingEq(String),
    /// 键为空，例如 "=bar"
    EmptyKey(String),
    /// 值为空，例如 "foo="
    EmptyValue(String),
    /// 键重复
    DuplicateKey(String),
    /// 不是合法 UTF-8
    InvalidUtf8,          // ← 一定要有这行
}


/// 将形如 "a=1;b=2;c=3" 的字符串解析成 HashMap
pub fn parse_kv_line(buf: &BytesMut) -> Result<HashMap<String, String>, ParseError> {
    // 1) 先尝试把整段缓冲区按 UTF-8 解释
    let line = std::str::from_utf8(buf).map_err(|_| ParseError::InvalidUtf8)?;

    let mut map = HashMap::new();

    // 2) 与之前相同的逐项解析
    for raw in line.split(';').filter(|s| !s.is_empty()) {
        let mut parts = raw.splitn(2, '=');

        let key = parts.next().unwrap();
        let value_opt = parts.next();

        // 缺少 '='
        let value = value_opt.ok_or_else(|| ParseError::MissingEq(raw.to_string()))?;

        if key.is_empty() {
            return Err(ParseError::EmptyKey(raw.to_string()));
        }
        if value.is_empty() {
            return Err(ParseError::EmptyValue(raw.to_string()));
        }
        if map.contains_key(key) {
            return Err(ParseError::DuplicateKey(key.to_string()));
        }

        map.insert(key.to_owned(), value.to_owned());
    }

    Ok(map)
}


fn hex_str_to_bytes(hex_str: &str) -> Result<Vec<u8>, String> {
    if hex_str.len() % 2 != 0 {
        return Err("Hex字符串的长度不是偶数".to_string());
    }
    (0..hex_str.len())
        .step_by(2)
        .map(|i| {
            u8::from_str_radix(&hex_str[i..i + 2], 16)
                .map_err(|e| e.to_string())
        })
        .collect()
}



#[derive(Serialize, Deserialize, Debug, Clone)]
pub struct DeviceStatus {
    pub id: u64, 
    pub device_id: u8,
    pub com : String,
    pub com_status: bool,
    pub device_status: bool,
    pub value: HashMap<String, Value>,
    pub raw_data: Vec<u8>, // 原始数据
}

pub async fn handle_client_1(mut framed: Framed<TcpStream, LengthDelimitedCodec>,
                             global_sender: Arc<TokioMutex<Vec<Arc<tokio_mpsc::Sender<DeviceStatus>>>>>,
                             serial_ports:Vec<SerialPortConfig>,
                             txs: Vec<mpsc::Sender<Command>>,
                             system_state: SystemState,
                             system_record: Arc<TokioMutex<BoundedVecDeque<String>>>
) -> io::Result<()> {
    println!("handle_client");
    // 创建一个Tokio异步消息通道，缓冲区大小为32 tx_serial 用于向客户端发送数据，rx 用于接收其他任务发送的数据 串口数据从tx_serial发送到rx
    let (tx_serial, mut rx) = tokio_mpsc::channel(32);
    let local_tx_arc = Arc::new(tx_serial); // 正确包装为 Arc
    // 获取全局发送器的锁，并将当前客户端的发送器添加到全局列表中
    {
        let mut sender = global_sender.lock().await; // 异步等待并获取锁
        sender.push(local_tx_arc.clone()); // 将 Arc 包装的 tx 添加到全局发送器列表
    }

    // 无限循环，处理接收到的消息或待发送的数据
    loop {
        tokio::select! {
            // 接收客户端发送的消息
            message = framed.next() => match message {
                // 如果成功接收到消息，输出消息内容
                Some(Ok(msg)) => {
                    //let message: MessageType = bincode::deserialize(&msg).unwrap();
                    match bincode::deserialize::<MessageType>(&msg){
                        Ok(message) => {
                            match message {
                                MessageType::Command(a) => {
                                    //把数据发送到串口
                                    println!("Received MessageA: {:?}", a.clone());
                                    //tx.send(a).unwrap();

                                    //时间戳
                                    let local: DateTime<Local> = Local::now();

                                    //记录到系统日志
                                    let mut record = system_record.lock().await;


                                    match a.command.clone() {
                                        CommandType::SendData(data) => {
                                            record.push_back(format!("时间{} 串口{} 数据{}", local.format("%Y-%m-%d %H:%M:%S"), a.com, data.command_as_string()));
                                        },
                                        _ => {}
                                    }


                                    //发送到串口线程 serial_prots
                                    for (i, serial_prot) in serial_ports.iter().enumerate() {
                                        if a.com == serial_prot.port_number {
                                            // txs[i].send(a.clone()).unwrap();
                                            match txs[i].send(a.clone()) {
                                                Ok(_) => {
                                                    // println!("Message sent successfully to serial port thread");
                                                },
                                                Err(e) => {
                                                    eprintln!("Failed to send message: {:?}", e);
                                                    // 此时可以根据需要进行进一步处理，比如记录日志，或者跳过。
                                                }
                                            }
                                            println!("serialized: {:?}", a);
                                        }
                                    }

                                },
                                MessageType::DeviceStatus(b) => {
                                    println!("Received MessageB: {:?}", b);
                                },
                                MessageType::QueryAllStatus => {
                                    // 查询所有设备状态
                                    let msg = MessageType::AllStatus(system_state.get_state().await);
                                    println!("Sending MessageC: {:?}", msg);

                                    let serialized = bincode::serialize(&msg).expect("Failed to serialize message");
                                    if let Err(_e) = timeout(Duration::from_secs(1), framed.send(Bytes::from(serialized))).await {
                                        return Err(io::Error::new(io::ErrorKind::TimedOut, "发送消息超时"));
                                    }
                                }
                                MessageType::AllStatus(c) => {
                                    // 查询所有设备状态
                                    println!("Received MessageC: {:?}", c);
                                }
                                MessageType::QueryRecord => {
                                    // 查询所有设备状态
                                    //记录到系统日志
                                    let record = system_record.lock().await;
                                    let mut records = vec![];
                                    for r in record.iter() {
                                        records.push(r.clone());
                                    }

                                    let msg = MessageType::AllRecord(records);

                                    let serialized = bincode::serialize(&msg).expect("Failed to serialize message");
                                    if let Err(_e) = timeout(Duration::from_secs(1), framed.send(Bytes::from(serialized))).await {
                                        return Err(io::Error::new(io::ErrorKind::TimedOut, "发送消息超时"));
                                    }
                                }
                                MessageType::AllRecord(d) => {
                                    // 查询所有设备状态
                                    println!("Received MessageD: {:?}", d);
                                }
                            }
                        },
                        Err(_e) => {
                            //struct SendData {
                            //     pub device_id: u32,
                            //     pub command: Vec<u8>,
                            // }  创建一个 SendData 结构体

                            match parse_kv_line(&msg) {
                                Ok(map) => {
                                    // ① 同时遍历键和值（最常用）
                                    for (key, value) in &map {
                                        let bytes = match hex_str_to_bytes(&value) {
                                            Ok(bytes) => bytes,
                                            Err(e) => {
                                                eprintln!("Hex字符串转换失败: {}", e);
                                                continue; // 跳过当前循环，继续处理下一个键值对
                                            }
                                        };
                                        let send_data = crate::common::SendData{
                                            device_id: 0, // 这里可以根据实际情况设置设备ID
                                            command: bytes, // 将接收到的消息转换为 Vec<u8>
                                        };
                                            // 然后用它来构造 Command
                                        let a = Command {
                                            com: key.clone(),
                                            command: CommandType::SendData(send_data),
                                        };
                                        
                                        
                                        //发送到串口线程 serial_prots
                                        for (i, serial_prot) in serial_ports.iter().enumerate() {
                                            if a.com.clone() == serial_prot.port_number {
                                                // txs[i].send(a.clone()).unwrap();
                                                match txs[i].send(a.clone()) {
                                                    Ok(_) => {
                                                        // println!("Message sent successfully to serial port thread");
                                                    },
                                                    Err(e) => {
                                                        eprintln!("Failed to send message: {:?}", e);
                                                        // 此时可以根据需要进行进一步处理，比如记录日志，或者跳过。
                                                    }
                                                }
                                                println!("serialized: {:?}", a);
                                            }
                                        }
                                    }
                                }
                                Err(e) => eprintln!("解析失败: {e:?}"),
                            }
                        }
                    }



                }
                // 如果接收消息时出错或流结束（None），则处理客户端断开连接的情况
                Some(Err(e)) => {
                    // 出错处理，移除发送器
                    let mut sender = global_sender.lock().await;
                     sender.retain(|x| !Arc::ptr_eq(x, &local_tx_arc)); // 使用 Arc::ptr_eq 正确比较
                    eprintln!("Error receiving from client: {}", e);
                    break;
                },
                None => {
                    // 处理连接关闭
                    let mut sender = global_sender.lock().await;
                    sender.retain(|x| !Arc::ptr_eq(x, &local_tx_arc)); // 使用 Arc::ptr_eq 正确比较
                    println!("Connection closed by client.");
                    break;
                }
            },
            // 从其他任务或处理逻辑接收到要发送的数据
            data_to_send = rx.recv() => {
                // 如果有数据待发送
                if let Some(data) = data_to_send {
                    println!("Preparing to send data: {:?}", data);
                    // 这里注释的部分是将数据发送到客户端的代码，需要解开注释以实际发送数据
                    //let message = MessageType::DeviceStatus(data);
                    // 序列化消息。
                    //let serialized = bincode::serialize(&message).expect("Failed to serialize message");

                        // ① 打平成字符串
                    let plain = build_status_line(&data);
                    if let Err(_e) = timeout(Duration::from_secs(1), framed.send(Bytes::from(plain.clone()))).await {
                        //error!("发送消息超时: {:?}", e);
                        return Err(io::Error::new(io::ErrorKind::TimedOut, "发送消息超时"));
                    }

                    // 发送序列化后的消息。
                    // if let Err(_e) = timeout(Duration::from_secs(1), framed.send(Bytes::from(serialized))).await {
                    //     //error!("发送消息超时: {:?}", e);
                    //     return Err(io::Error::new(io::ErrorKind::TimedOut, "发送消息超时"));
                    // }
                }
            }
        }
    }

    // 正常退出循环后返回Ok，表示函数执行成功
    Ok(())
}




fn build_status_line(ds: &DeviceStatus) -> String {
    // com_status / device_status 先缓存，避免反复借用
    let com_ok   = ds.com_status;
    let dev_ok   = ds.device_status;

    let mut s = String::new();

    for (k, v) in &ds.value {
        // 1) 普通数据
        write!(&mut s, "{}={}\n", k, value_to_str(v)).unwrap();
        // 2) com_status / device_status
        write!(&mut s, "{}_com_status={}\n", k, bool_to_int(com_ok).to_string()).unwrap();
        write!(&mut s, "{}_device_status={}\n", k, bool_to_int(dev_ok).to_string()).unwrap();
    }

    // 去掉最后一个分号
    // if s.ends_with(';') {
    //     s.pop();
    // }
    s.chars()
        .filter(|c| c.is_ascii_graphic() || c.is_ascii_whitespace())
        .collect()
}

#[inline]
fn bool_to_int(b: bool) -> u8 {
    if b { 1 } else { 0 }
}

/// 把 `serde_json::Value` 转成适合人看的字符串
fn value_to_str(v: &Value) -> String {
    match v {
        Value::Float(f)      => f.to_string(),
        Value::Bool(b)  => bool_to_int(*b).to_string(),
        Value::UInt(n) => n.to_string(),
    }
}

pub async fn run_tcp_server_1(serial_ports:Vec<SerialPortConfig>,
                              txs: Vec<mpsc::Sender<Command>>,
                              global_sender: Arc<TokioMutex<Vec<Arc<tokio_mpsc::Sender<DeviceStatus>>>>>,
                              system_state: SystemState,
                              system_record: Arc<TokioMutex<BoundedVecDeque<String>>>
                              ) -> io::Result<()> {
    // 绑定一个TCP监听器到本地的8080端口
    let listener = TcpListener::bind("0.0.0.0:11002").await?;

    // 无限循环，用于不断接受连接请求
    loop {
        // 克隆全局发送器，以便在多个任务中安全使用
        let global_sender_clone = global_sender.clone();

        // 异步接受一个连接请求
        match listener.accept().await {
            // 如果接受成功，获取到连接的流和地址
            Ok((stream, _addr)) => {
                // 克隆设备状态，用于跨任务共享
                // 将TCP流包装成帧，使用`LengthDelimitedCodec`解码器处理数据帧
                let framed = Framed::new(stream, LengthDelimitedCodec::new());
                // 异步启动一个新的任务来处理客户端，传递处理好的帧和克隆的状态
                tokio::spawn(handle_client_1(framed, global_sender_clone, serial_ports.clone(), txs.clone(),system_state.clone(), system_record.clone()));
            },
            // 如果接受连接失败，则输出错误信息
            Err(e) => {
                eprintln!("Failed to accept connection: {:?}", e);
            }
        }
    }
}



pub async fn run_tcp_server_2(serial_ports: Vec<SerialPortConfig>,
                              txs: Vec<mpsc::Sender<Command>>,
                              global_sender_plain: Arc<TokioMutex<Vec<Arc<tokio_mpsc::Sender<DeviceStatus>>>>>,
                              system_state: SystemState,
                              system_record: Arc<TokioMutex<BoundedVecDeque<String>>>
) -> io::Result<()> {
    // 绑定一个新的TCP监听器到本地的11003端口（你可以修改端口）
    let listener = TcpListener::bind("0.0.0.0:11003").await?;

    // 无限循环，用于不断接受连接请求
    loop {
        // 克隆全局发送器，以便在多个任务中安全使用
        let global_sender_plain_clone = global_sender_plain.clone();

        // 异步接受一个连接请求
        match listener.accept().await {
            Ok((stream, _addr)) => {
                // 不使用Framed，直接用TcpStream
                tokio::spawn(handle_client_2(stream, global_sender_plain_clone, serial_ports.clone(), txs.clone(), system_state.clone(), system_record.clone()));
            },
            Err(e) => {
                eprintln!("Failed to accept connection on plain server: {:?}", e);
            }
        }
    }
}


pub async fn handle_client_2(mut stream: TcpStream,
                             global_sender_plain: Arc<TokioMutex<Vec<Arc<tokio_mpsc::Sender<DeviceStatus>>>>>,
                             _serial_ports: Vec<SerialPortConfig>,  // 未使用，但保持参数一致
                             _txs: Vec<mpsc::Sender<Command>>,      // 未使用
                             _system_state: SystemState,            // 未使用
                             _system_record: Arc<TokioMutex<BoundedVecDeque<String>>>  // 未使用
) -> io::Result<()> {
    println!("handle_client_2 (plain server)");

    // 创建一个Tokio异步消息通道，缓冲区大小为32
    let (tx_serial, mut rx) = tokio_mpsc::channel(32);
    let local_tx_arc = Arc::new(tx_serial);

    // 获取全局发送器的锁，并将当前客户端的发送器添加到全局列表中
    {
        let mut sender = global_sender_plain.lock().await;
        sender.push(local_tx_arc.clone());
    }

    // 无限循环，只处理待发送的数据（不接收客户端消息）
    loop {
        // 从其他任务或处理逻辑接收到要发送的数据
        if let Some(data) = rx.recv().await {
            println!("Preparing to send data (plain): {:?}", data);

            // 构建plain字符串（与原有相同）
            let plain = build_status_line(&data);

            // 直接发送纯字符串，不带帧头
            if let Err(e) = timeout(Duration::from_secs(1), stream.write_all(plain.as_bytes())).await {
                eprintln!("发送消息超时 (plain): {:?}", e);
                return Err(io::Error::new(io::ErrorKind::TimedOut, "发送消息超时"));
            }

            // 可选：flush确保发送
            if let Err(e) = stream.flush().await {
                eprintln!("Flush failed (plain): {:?}", e);
                break;
            }
        } else {
            // rx关闭，连接结束
            println!("Connection closed by server (plain).");
            break;
        }
    }

    // 移除发送器
    let mut sender = global_sender_plain.lock().await;
    sender.retain(|x| !Arc::ptr_eq(x, &local_tx_arc));

    Ok(())
}

