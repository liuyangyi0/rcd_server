//! TCP 服务器模块。
//!
//! 基于 bincode 长度分帧协议的 TCP 接口，将串口采集到的设备数据推送给客户端。

use std::io;
use std::sync::{Arc, mpsc};
use std::time::Duration;

use bounded_vec_deque::BoundedVecDeque;
use bytes::Bytes;
use futures::{SinkExt, StreamExt};
use log::{info, warn, error};
use tokio::net::{TcpListener, TcpStream};
use tokio::sync::{mpsc as tokio_mpsc, Mutex as TokioMutex};
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
    system_record: Arc<TokioMutex<BoundedVecDeque<String>>>,
) -> io::Result<()> {
    info!("新客户端已连接");

    // 创建本连接专用通道并注册到广播器
    let (tx_local, mut rx_local) = tokio_mpsc::channel(32);
    let local_tx_arc = Arc::new(tx_local);
    broadcaster.register(local_tx_arc.clone()).await;

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
                    broadcaster.unregister(&local_tx_arc).await;
                    error!("接收客户端数据出错: {}", e);
                    break;
                }
                None => {
                    broadcaster.unregister(&local_tx_arc).await;
                    info!("客户端断开连接");
                    break;
                }
            },
            // ---- 推送串口数据给客户端 ----
            data = rx_local.recv() => {
                if let Some(status) = data {
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
    system_record: &Arc<TokioMutex<BoundedVecDeque<String>>>,
) -> io::Result<()> {
    match message {
        MessageType::Command(cmd) => {
            // 记录日志
            if let CommandType::SendData(ref data) = cmd.command {
                let local = chrono::Local::now();
                let mut record = system_record.lock().await;
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
            let msg = MessageType::AllStatus(system_state.get_state().await);
            send_framed(framed, &msg).await?;
        }
        MessageType::QueryRecord => {
            let record = system_record.lock().await;
            let records: Vec<String> = record.iter().cloned().collect();
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
    system_record: Arc<TokioMutex<BoundedVecDeque<String>>>,
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
