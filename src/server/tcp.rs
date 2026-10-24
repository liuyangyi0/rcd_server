//! TCP 服务器模块（未启用，保留作为可选前端）。
//!
//! 基于 bincode 长度分帧协议的 TCP 接口，将串口采集到的设备数据推送给客户端。
//!
//! # 安全警告
//!
//! 启用前必须解决两个问题：
//! 1. **无身份验证**：任何能连接到 `0.0.0.0:11002` 的客户端都可以向串口
//!    下发任意命令帧，等同于对现场设备的开放控制通道。
//! 2. **bincode 反序列化未经验证的数据**：恶意客户端可构造畸形帧触发
//!    解析异常或分配放大。
//!
//! `run_tcp_server` 目前标记为 `#[allow(unused)]`，`main.rs` 未调用它；
//! 不要在接上公网的场景下解除注释。

use std::io;
use std::sync::{mpsc, Arc, Mutex};
use std::time::Duration;

use bounded_vec_deque::BoundedVecDeque;
use bytes::Bytes;
use futures::{SinkExt, StreamExt};
use log::{error, info, warn};
use tokio::net::{TcpListener, TcpStream};
use tokio::sync::broadcast;
use tokio::time::timeout;
use tokio_util::codec::{Framed, LengthDelimitedCodec};

use crate::broadcast::Broadcaster;
use crate::model::{Command, CommandType, MessageType, SystemState};
use crate::serial::port_config::SerialPortConfig;

// ============================================================
//  TCP 服务器
// ============================================================

/// 处理单个 TCP 客户端连接的全部消息收发。
pub async fn handle_tcp_client(
    mut framed: Framed<TcpStream, LengthDelimitedCodec>,
    broadcaster: Broadcaster,
    serial_ports: Vec<SerialPortConfig>,
    txs: Vec<mpsc::Sender<Command>>,
    system_state: SystemState,
    system_record: Arc<Mutex<BoundedVecDeque<String>>>,
) -> io::Result<()> {
    info!("新客户端已连接");

    // 订阅广播；Receiver drop 时自动注销
    let mut rx_local = broadcaster.subscribe();

    loop {
        tokio::select! {
            // ---- 接收客户端消息 ----
            message = framed.next() => match message {
                Some(Ok(msg)) => {
                    let message: MessageType = match bincode::deserialize(&msg) {
                        Ok(m) => m,
                        Err(e) => {
                            warn!("反序列化客户端消息失败: {:?}", e);
                            continue;
                        }
                    };
                    handle_client_message(
                        message, &mut framed, &serial_ports, &txs,
                        &system_state, &system_record,
                    ).await?;
                }
                Some(Err(e)) => {
                    error!("接收客户端数据出错: {}", e);
                    break;
                }
                None => {
                    info!("客户端断开连接");
                    break;
                }
            },
            // ---- 推送串口数据给客户端 ----
            data = rx_local.recv() => match data {
                Ok(status) => {
                    let msg = MessageType::DeviceStatus(status);
                    match bincode::serialize(&msg) {
                        Ok(serialized) => {
                            if timeout(Duration::from_secs(1), framed.send(Bytes::from(serialized))).await.is_err() {
                                return Err(io::Error::new(io::ErrorKind::TimedOut, "发送消息超时"));
                            }
                        }
                        Err(e) => {
                            error!("序列化 DeviceStatus 失败: {:?}", e);
                        }
                    }
                }
                Err(broadcast::error::RecvError::Lagged(n)) => {
                    warn!("TCP 客户端广播订阅落后 {} 条，已丢帧", n);
                }
                Err(broadcast::error::RecvError::Closed) => {
                    info!("广播通道关闭");
                    break;
                }
            }
        }
    }

    Ok(())
}

/// 处理一条客户端消息。
async fn handle_client_message(
    message: MessageType,
    framed: &mut Framed<TcpStream, LengthDelimitedCodec>,
    serial_ports: &[SerialPortConfig],
    txs: &[mpsc::Sender<Command>],
    system_state: &SystemState,
    system_record: &Arc<Mutex<BoundedVecDeque<String>>>,
) -> io::Result<()> {
    match message {
        MessageType::Command(cmd) => {
            // 记录日志
            if let CommandType::SendData(ref data) = cmd.command {
                let local = chrono::Local::now();
                let mut record = system_record.lock().unwrap_or_else(|e| e.into_inner());
                record.push_back(format!(
                    "时间 {} 串口 {} 数据 {}",
                    local.format("%Y-%m-%d %H:%M:%S"),
                    cmd.com,
                    data.command_as_string(),
                ));
            }
            // 转发到对应串口线程
            for (i, port) in serial_ports.iter().enumerate() {
                if cmd.com == port.port_number {
                    if let Err(e) = txs[i].send(cmd.clone()) {
                        error!("转发命令到串口线程失败: {:?}", e);
                    }
                    break;
                }
            }
        }
        MessageType::QueryAllStatus => {
            let msg = MessageType::AllStatus(system_state.get_state());
            send_framed(framed, &msg).await?;
        }
        MessageType::QueryRecord => {
            let records: Vec<String> = {
                let record = system_record.lock().unwrap_or_else(|e| e.into_inner());
                record.iter().cloned().collect()
            };
            let msg = MessageType::AllRecord(records);
            send_framed(framed, &msg).await?;
        }
        // 服务端不处理以下消息类型
        _ => {}
    }
    Ok(())
}

/// 序列化并发送一条帧消息（带 1 秒超时）。
async fn send_framed(
    framed: &mut Framed<TcpStream, LengthDelimitedCodec>,
    msg: &MessageType,
) -> io::Result<()> {
    let serialized = match bincode::serialize(msg) {
        Ok(data) => data,
        Err(e) => {
            error!("序列化消息失败: {:?}", e);
            return Ok(());
        }
    };
    if timeout(Duration::from_secs(1), framed.send(Bytes::from(serialized)))
        .await
        .is_err()
    {
        return Err(io::Error::new(io::ErrorKind::TimedOut, "发送消息超时"));
    }
    Ok(())
}

/// 启动 TCP 服务器，监听 `0.0.0.0:11002`。
#[allow(unused)]
pub async fn run_tcp_server(
    serial_ports: Vec<SerialPortConfig>,
    txs: Vec<mpsc::Sender<Command>>,
    broadcaster: Broadcaster,
    system_state: SystemState,
    system_record: Arc<Mutex<BoundedVecDeque<String>>>,
) -> io::Result<()> {
    let listener = TcpListener::bind("0.0.0.0:11002").await?;

    loop {
        let bc = broadcaster.clone();
        match listener.accept().await {
            Ok((stream, _addr)) => {
                let framed = Framed::new(stream, LengthDelimitedCodec::new());
                tokio::spawn(handle_tcp_client(
                    framed,
                    bc,
                    serial_ports.clone(),
                    txs.clone(),
                    system_state.clone(),
                    system_record.clone(),
                ));
            }
            Err(e) => error!("接受连接失败: {:?}", e),
        }
    }
}
