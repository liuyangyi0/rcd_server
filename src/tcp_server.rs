use tokio::net::{TcpListener, TcpStream};
use tokio::sync::{mpsc as tokio_mpsc, Mutex as TokioMutex};
use std::sync::{Arc, mpsc};
use futures::stream::StreamExt;
use tokio_util::codec::{Framed, LengthDelimitedCodec};
use std::collections::HashMap;
use std::io;
use std::time::Duration;
use bytes::Bytes;
use futures::SinkExt;
use serde::{Deserialize, Serialize};
use tokio::time::timeout;
use tracing::error;
use crate::common::{Command, MessageType, SendData, Value};
use crate::serial_port_config::SerialPortConfig;








#[derive(Serialize, Deserialize, Debug, Clone)]
pub struct DeviceStatus {
    pub id: u64,
    pub com_status: bool,
    pub device_status: bool,
    pub value: HashMap<String, Value>,
}

pub async fn handle_client_1(mut framed: Framed<TcpStream, LengthDelimitedCodec>,
                             global_sender: Arc<TokioMutex<Vec<Arc<tokio_mpsc::Sender<DeviceStatus>>>>>,
                             serial_ports:Vec<SerialPortConfig>,
                             txs: Vec<mpsc::Sender<Command>>,
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
                    let message: MessageType = bincode::deserialize(&msg).unwrap();
                    match message {
                        MessageType::Command(a) => {
                            //把数据发送到串口
                            println!("Received MessageA: {:?}", a.clone());
                            //tx.send(a).unwrap();

                            //发送到串口线程 serial_prots
                            for (i, serial_prot) in serial_ports.iter().enumerate() {
                                if a.com == serial_prot.port_number {
                                    txs[i].send(a.clone()).unwrap();
                                    println!("serialized: {:?}", a);
                                }
                            }

                        },
                        MessageType::DeviceStatus(b) => {
                            println!("Received MessageB: {:?}", b);
                        },
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
                    let message = MessageType::DeviceStatus(data);
                    // 序列化消息。
                    let serialized = bincode::serialize(&message).expect("Failed to serialize message");
                    // 发送序列化后的消息。
                    // if let Err(e) = framed.send(Bytes::from(serialized)).await {
                    //     // 若发送失败，则记录错误并结束循环。
                    //     error!("Failed to send message: {:?}", e);
                    //     return Err(e);
                    // }
                    if let Err(_e) = timeout(Duration::from_secs(1), framed.send(Bytes::from(serialized))).await {
                        //error!("发送消息超时: {:?}", e);
                        return Err(io::Error::new(io::ErrorKind::TimedOut, "发送消息超时"));
                    }
                }
            }
        }
    }

    // 正常退出循环后返回Ok，表示函数执行成功
    Ok(())
}

pub async fn run_tcp_server_1(serial_ports:Vec<SerialPortConfig>,
                              txs: Vec<mpsc::Sender<Command>>,
                              global_sender: Arc<TokioMutex<Vec<Arc<tokio_mpsc::Sender<DeviceStatus>>>>>,) -> io::Result<()> {
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
                tokio::spawn(handle_client_1(framed, global_sender_clone, serial_ports.clone(), txs.clone()));
            },
            // 如果接受连接失败，则输出错误信息
            Err(e) => {
                eprintln!("Failed to accept connection: {:?}", e);
            }
        }
    }
}
