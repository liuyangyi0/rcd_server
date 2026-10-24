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
use crate::common::{get_sum, Command, CommandType, MessageType, SendData, SystemState, Value};
use crate::serial_port_config::SerialPortConfig;
use chrono::prelude::*;

// ========== OPC UA 所需的新增导入 ==========
use opcua::server::address_space::{Variable, NodeType};
use opcua::server::diagnostics::NamespaceMetadata;
use opcua::server::node_manager::memory::{simple_node_manager, SimpleNodeManager};
use opcua::server::{ServerBuilder, ServerEndpoint, ServerUserToken, SubscriptionCache, ANONYMOUS_USER_TOKEN_ID};
use opcua::types::{BuildInfo, DataEncoding, DataTypeId, DataValue, DateTime as UaDateTime, NodeId, NumericRange, TimestampsToReturn, UAString, Variant};
use log::warn;
use env_logger;
use opcua::nodes::AccessLevel;

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
                    let message: MessageType = bincode::deserialize(&msg).unwrap();
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
                            let mut record = system_record.lock().await;
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



#[derive(Clone, Copy, Debug)]
pub enum ExpectedType {
    UInt32,
    Bool,
    Float32,
    Float64,
}

// 你可以把字符串或你自己的类型映射到 ExpectedType
fn expected_type_from_str(s: &str) -> ExpectedType {
    match s.to_ascii_lowercase().as_str() {
        "u32" | "uint32" | "uint" => ExpectedType::UInt32,
        "bool" | "boolean"        => ExpectedType::Bool,
        "f32" | "float"           => ExpectedType::Float32,
        "f64" | "double"          => ExpectedType::Float64,
        _ => ExpectedType::Float32, // 实在未知时用 Float32（你当前 Value::Float 是 f32）
    }
}


// 由期望类型给出 OPC UA 节点的数据类型 NodeId 与一个合规的初始值
fn dt_and_init(expected: ExpectedType) -> (NodeId, Variant) {
    match expected {
        ExpectedType::UInt32 => (NodeId::from(DataTypeId::UInt32), Variant::UInt32(0)),
        ExpectedType::Bool   => (NodeId::from(DataTypeId::Boolean), Variant::Boolean(false)),
        ExpectedType::Float32=> (NodeId::from(DataTypeId::Float),   Variant::Float(0.0)),
        ExpectedType::Float64=> (NodeId::from(DataTypeId::Double),  Variant::Double(0.0)),
    }
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

fn modbus_crc16_special(data: &[u8]) -> (u8, u8) {
    let mut crc: u16 = 0xFFFF;

    for &b in data {
        crc ^= b as u16;
        for _ in 0..8 {
            crc = (crc >> 1) & 0x7FFF;
            if (crc & 0x0001) == 1 {
                crc ^= 0xA001;
            }
        }
    }

    let low_byte  = (crc & 0x00FF) as u8;
    let high_byte = ((crc >> 8) & 0x00FF) as u8;
    (low_byte, high_byte)
}







/// 运行 OPC UA 服务器，将串口数据发布为独立变量节点。
///
/// 每个节点的命名规则为 `<串口号>_<设备ID>_<kks>`，如 `COM3_1_Value`。
/// 当串口线程发送 DeviceStatus 时，会根据其中的 `value` 字典更新对应节点的实际数值。
// ====== 你的函数：带证书/安全端点的完整版本（已标注改动点） ======
pub async fn run_opcua_server(
    serial_ports: Vec<SerialPortConfig>,
    global_sender: Arc<TokioMutex<Vec<Arc<tokio_mpsc::Sender<DeviceStatus>>>>>,
    _system_state: SystemState,
    _system_record: Arc<TokioMutex<BoundedVecDeque<String>>>,
    txs: Vec<mpsc::Sender<Command>>,  // 新增：添加此参数
) -> io::Result<()> {
    // 初始化日志
    let _ = env_logger::try_init();

    // 服务监听地址与端口
    let host = "0.0.0.0";
    let port: u16 = 4840;
    let base = format!("opc.tcp://{}:{}/", host, port);

    // 构建 OPC UA 服务器
    let (server, handle) = ServerBuilder::new()
        .host(host)
        .port(port)
        .discovery_urls(vec![base.clone()])
        .application_name("Serial OPC UA Server")
        .application_uri("urn:SerialServer:rcd_server")
        .pki_dir("./pki")
        .create_sample_keypair(true)
        // 配置用户名/密码
        .add_user_token("user1", ServerUserToken::user_pass("user1", "pwd"))
        // 添加匿名端点
        .add_endpoint(
            "none",
            ServerEndpoint::new_none("/", &[ANONYMOUS_USER_TOKEN_ID.to_string()]),
        )
        // 添加加密端点
        .add_endpoint(
            "basic256sha256_signenc",
            ServerEndpoint::new_basic256sha256_sign_encrypt(
                "/",
                &[
                    ANONYMOUS_USER_TOKEN_ID.to_string(),
                    "user1".to_string(),
                ],
            ),
        )
        .default_endpoint("basic256sha256_signenc")
        .build_info(BuildInfo {
            product_uri: "urn:SerialServer".into(),
            manufacturer_name: "Rust Serial OPC UA Server".into(),
            product_name: "Serial OPC UA Server".into(),
            software_version: "0.1.0".into(),
            build_number: "1".into(),
            build_date: UaDateTime::now(),
        })
        .with_node_manager(simple_node_manager(
            NamespaceMetadata {
                namespace_uri: "urn:SerialServer".to_owned(),
                ..Default::default()
            },
            "simple",
        ))
        .trust_client_certs(true)
        .diagnostics_enabled(true)
        .build()
        .map_err(|e| io::Error::new(io::ErrorKind::Other, format!("Failed to build OPC UA server: {e}")))?;

    // 获取节点管理器和订阅缓存
    let node_manager: Arc<SimpleNodeManager> = handle
        .node_managers()
        .get_of_type::<SimpleNodeManager>()
        .expect("Failed to get simple node manager");
    let subscriptions: Arc<SubscriptionCache> = handle.subscriptions().clone();

    // 获取自定义命名空间索引
    let ns = handle
        .get_namespace_index("urn:SerialServer")
        .expect("Custom namespace not registered");

    // 创建 Devices 文件夹及变量
    let devices_folder_id = NodeId::new(ns, "Devices");
    let mut send_node_ids: Vec<(String, NodeId)> = Vec::new();
    {
        let mut address_space = node_manager.address_space().write();
        // 在 Objects 文件夹下创建 Devices 文件夹
        address_space.add_folder(
            &devices_folder_id,
            "Devices",
            "Devices",
            &NodeId::objects_folder_id(),
        );

        let mut variables = Vec::new();
        // 遍历串口配置
        for port in &serial_ports {
            for cfg in &port.commands {
                for record in &cfg.records {
                    let browse_name = format!("{}_{}_{}", port.port_number, cfg.config.device_id, record.kks);
                    let node_id = NodeId::new(ns, browse_name.clone());
                    let expected = expected_type_from_str(&record.type_);
                    let (dt, init) = dt_and_init(expected);
                    variables.push(Variable::new_data_value(
                        &node_id,
                        &browse_name,
                        &browse_name,
                        dt,
                        Some(-1),
                        None,
                        init,
                    ));
                }
            }
            // 为每个串口添加一个可写字符串变量
            let send_name = format!("{}_Send", port.port_number);
            let send_node_id = NodeId::new(ns, send_name.clone());
            variables.push(Variable::new_data_value(
                &send_node_id,
                &send_name,
                &send_name,
                NodeId::from(DataTypeId::String),
                Some(-1),
                None,
                Variant::String(UAString::from("")),
            ));

            send_node_ids.push((port.port_number.clone(), send_node_id));
        }
        // 添加所有变量
        if !variables.is_empty() {
            address_space.add_variables(variables, &devices_folder_id);
        }
    }


    //后加入的 添加写权限
    // 为每个发送节点设置写权限
    {
        let mut address_space = node_manager.address_space().write();
        for (_, node_id) in &send_node_ids {
            if let Some(NodeType::Variable(var)) = address_space.find_mut(node_id) {
                // 读 + 写
                var.set_access_level(AccessLevel::CURRENT_READ | AccessLevel::CURRENT_WRITE);
                var.set_user_access_level(AccessLevel::CURRENT_READ | AccessLevel::CURRENT_WRITE);
            } else {
                warn!("未找到发送节点(设置写权限失败): {:?}", node_id);
            }
        }
    }

    // 创建本地通道用于接收串口线程发送的数据，并注册到全局发送器列表
    let (tx_opcua, mut rx_opcua) = tokio_mpsc::channel::<DeviceStatus>(32);
    let local_tx_arc = Arc::new(tx_opcua);
    {
        let mut senders = global_sender.lock().await;
        senders.push(local_tx_arc.clone());
    }

    // 更新串口数据节点的异步任务
    let nm_update = node_manager.clone();
    let subs_update = subscriptions.clone();
    tokio::spawn(async move {
        while let Some(status) = rx_opcua.recv().await {
            for (kks, val) in &status.value {
                let browse_name = format!("{}_{}_{}", status.com, status.device_id, kks);
                let node_id = NodeId::new(ns, browse_name.clone());
                let variant = match val {
                    Value::UInt(n) => Variant::UInt32(*n),
                    Value::Bool(b) => Variant::Boolean(*b),
                    Value::Float(f) => Variant::Float(*f),
                };
                let data_value = DataValue::new_now(variant);
                // 克隆 NodeId，避免双重引用
                let id_clone = node_id.clone();
                // 使用 Vec 包装再 into_iter，生成 (&NodeId, Option, DataValue)
                let iter = vec![(&id_clone, None, data_value)].into_iter();
                if let Err(e) = nm_update.set_values(&subs_update, iter) {
                    warn!("Failed to set OPC UA variable value for {}: {:?}", browse_name, e);
                }
            }
        }
    });

    // 监听客户端写字符串变量的异步任务
    let nm_send = node_manager.clone();
    let subs_send = subscriptions.clone();
    let txs_clone = txs.clone();  // 新增：克隆用于任务中
    let serial_ports_clone = serial_ports.clone();  // 新增：克隆用于任务中
    let txs_clone = txs.clone();  // 新增：克隆用于任务中
    let system_record_clone = _system_record.clone();  // 新增：用于日志记录



    tokio::spawn(async move {
        loop {
            // 1) 本轮需要清零的节点（只清 com*_Send）
            let mut to_clear: Vec<NodeId> = Vec::new();

            // 2) 仅在读锁内读取与转发，不在读锁内 set_values
            {
                let address_space = nm_send.address_space().read();

                // 遍历每个发送变量（com*_Send）
                for (com, node_id) in &send_node_ids {
                    if let Some(node) = address_space.find_node(node_id) {
                        if let NodeType::Variable(var) = node {
                            // 读取变量值（保持你项目里的调用签名）
                            let dv = var.value(
                                TimestampsToReturn::Both,
                                &NumericRange::default(),
                                &DataEncoding::default(),
                                0.0,
                            );

                            if let Some(Variant::String(ref ua_str)) = dv.value {
                                let v = ua_str.as_ref().trim();

                                // 非空且不等于 "0" 时才处理；无论成功失败，稍后都把该点清为 "0"
                                if !v.is_empty() && v != "0" {
                                    println!("从 OPC 客户端收到串口 {} 的消息: {}", com, v);

                                    // 十六进制字符串 -> 字节，并追加 CRC（对 bytes[1..]）
                                    match hex_str_to_bytes(v) {
                                        Ok(mut bytes) => {
                                            if bytes.len() < 2 {
                                                warn!("收到的十六进制长度过短（至少需要2字节，含设备地址）: {}", v);
                                            } else {
                                                // let (low, high) = modbus_crc16_special(&bytes[1..]);
                                                // bytes.push(low);
                                                // bytes.push(high);
                                                bytes.push(get_sum(&bytes));

                                                // device_id 用首字节
                                                let device_id = bytes[0] as u32;
                                                let cmd = Command {
                                                    com: com.clone(),
                                                    command: CommandType::SendData(SendData {
                                                        device_id,
                                                        command: bytes,
                                                    }),
                                                };

                                                // 找到对应串口并发送
                                                if let Some((index, _)) = serial_ports_clone
                                                    .iter()
                                                    .enumerate()
                                                    .find(|(_, port)| &port.port_number == com)
                                                {
                                                    if let Err(e) = txs_clone[index].send(cmd.clone()) {
                                                        warn!("转发命令到串口失败: {:?}", e);
                                                    } else {
                                                        // 记录日志
                                                        let local = chrono::Local::now();
                                                        let mut record = system_record_clone.lock().await;
                                                        if let CommandType::SendData(data) = cmd.command.clone() {
                                                            record.push_back(format!(
                                                                "时间: {}, 串口: {}, 数据: {}",
                                                                local.format("%Y-%m-%d %H:%M:%S"),
                                                                cmd.com,
                                                                data.command_as_string()
                                                            ));
                                                        }
                                                    }
                                                } else {
                                                    warn!("未找到匹配的串口: {}", com);
                                                }
                                            }
                                        }
                                        Err(e) => {
                                            warn!("十六进制解析失败: {}", e);
                                        }
                                    }

                                    // 处理过就记下这个节点，出锁后统一清为 "0"
                                    to_clear.push(node_id.clone());
                                }
                            }
                        }
                    }
                }
            } // —— 读锁在此释放 ——

            // 3) 出锁后统一把触发变量清为 "0"（仅 com*_Send）
            for id in to_clear {
                let clear_val = DataValue::new_now(Variant::String(UAString::from("0")));
                let iter = vec![(&id, None, clear_val)].into_iter();
                if let Err(e) = nm_send.set_values(&subs_send, iter) {
                    warn!("清零 OPC 写变量失败: {:?}", e);
                }
            }

            // 4) 轮询间隔
            tokio::time::sleep(Duration::from_millis(200)).await;
        }
    });


    // Ctrl+C 取消运行
    {
        let handle_c = handle.clone();
        tokio::spawn(async move {
            if let Err(e) = tokio::signal::ctrl_c().await {
                warn!("Failed to register CTRL-C handler: {e}");
                return;
            }
            handle_c.cancel();
        });
    }

    // 运行 OPC UA 服务器（阻塞直到退出）
    server
        .run()
        .await
        .map_err(|e| io::Error::new(io::ErrorKind::Other, format!("OPC UA server error: {e}")))
}