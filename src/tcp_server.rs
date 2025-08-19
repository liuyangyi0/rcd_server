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

// ========== OPC UA 所需的新增导入 ==========
use opcua::server::address_space::Variable;
use opcua::server::diagnostics::NamespaceMetadata;
use opcua::server::node_manager::memory::{simple_node_manager, SimpleNodeManager};
use opcua::server::{ServerBuilder, ServerEndpoint, ServerUserToken, SubscriptionCache, ANONYMOUS_USER_TOKEN_ID};
use opcua::types::{BuildInfo, DataTypeId, DataValue, DateTime as UaDateTime, NodeId, UAString, Variant};
use log::warn;
use env_logger;


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


/// 运行 OPC UA 服务器，将串口数据发布为独立变量节点。
///
/// 每个节点的命名规则为 `<串口号>_<设备ID>_<kks>`，如 `COM3_1_Value`。
/// 当串口线程发送 DeviceStatus 时，会根据其中的 `value` 字典更新对应节点的实际数值。
// pub async fn run_opcua_server(
//     serial_ports: Vec<SerialPortConfig>,
//     global_sender: Arc<TokioMutex<Vec<Arc<tokio_mpsc::Sender<DeviceStatus>>>>>,
//     _system_state: SystemState,
//     _system_record: Arc<TokioMutex<BoundedVecDeque<String>>>,
// ) -> io::Result<()> {
//     // 初始化日志（多次调用也安全）
//     let _ = env_logger::try_init();
//
//     let host = "0.0.0.0";
//     let port: u16 = 4840;
//     let base = format!("opc.tcp://{}:{}/", host, port);
//
//     // 创建 OPC UA 服务器，设置产品信息和节点管理器
//     let (server, handle) = ServerBuilder::new()
//         .host(host)                           // 监听地址
//         .port(port)                           // 监听端口
//         .discovery_urls(vec![base.clone()])   // 至少一个 discovery url
//         // 配置一个匿名登录的 Endpoint（无加密）
//         .add_endpoint(
//             "none",
//             ServerEndpoint::new_none("/", &[ANONYMOUS_USER_TOKEN_ID.to_string()]),
//         )
//         .default_endpoint("none")
//
//
//
//
//         .build_info(BuildInfo {
//             product_uri: "urn:SerialServer".into(),
//             manufacturer_name: "Rust Serial OPC UA Server".into(),
//             product_name: "Serial OPC UA Server".into(),
//             software_version: "0.1.0".into(),
//             build_number: "1".into(),
//             build_date: UaDateTime::now(),
//         })
//         .create_sample_keypair(true)  // 会在 pki 目录下生成示例密钥/证书
//         .pki_dir("./pki")
//
//         .with_node_manager(simple_node_manager(
//             NamespaceMetadata {
//                 namespace_uri: "urn:SerialServer".to_owned(),
//                 ..Default::default()
//             },
//             "simple",
//         ))
//         .trust_client_certs(true)
//         .diagnostics_enabled(true)
//         .build()
//         .map_err(|e| io::Error::new(io::ErrorKind::Other, format!("Failed to build OPC UA server: {e}")))?;
//
//     // 获取内存节点管理器和订阅缓存
//     let node_manager: Arc<SimpleNodeManager> = handle
//         .node_managers()
//         .get_of_type::<SimpleNodeManager>()
//         .expect("Failed to get simple node manager");
//     let subscriptions: Arc<SubscriptionCache> = handle.subscriptions().clone();
//
//     // 获取自定义命名空间索引
//     let ns = handle
//         .get_namespace_index("urn:SerialServer")
//         .expect("Custom namespace not registered");
//
//     // 创建 Devices 文件夹，并为每个串口设备的每个 kks 建立变量节点
//     let devices_folder_id = NodeId::new(ns, "Devices");
//     {
//         let mut address_space = node_manager.address_space().write();
//         // 添加 Devices 文件夹到 Objects 文件夹下
//         address_space.add_folder(
//             &devices_folder_id,
//             "Devices",
//             "Devices",
//             &NodeId::objects_folder_id(),
//         );
//
//         // 创建变量集合
//         let mut variables = Vec::new();
//         for port in &serial_ports {
//             for cfg in &port.commands {
//                 for record in &cfg.records {
//                     // 节点名形如 "COM3_1_Value"
//                     let browse_name = format!("{}_{}_{}", port.port_number, cfg.config.device_id, record.kks);
//                     let node_id = NodeId::new(ns, browse_name.clone());
//                     // 依据 record.type_ 显式指定 DataType，并给一个合规初始值
//                     let expected = expected_type_from_str(&record.type_);
//                     let (dt, init) = dt_and_init(expected);
//
//                     variables.push(Variable::new_data_value(
//                         &node_id,
//                         &browse_name,  // BrowseName
//                         &browse_name,  // DisplayName
//                         dt,            // DataType: NodeId::from(DataTypeId::XXX)
//                         Some(-1),      // value_rank: -1 表示标量
//                         None,          // 非数组
//                         init,          // 初始值：与 DataType 匹配
//                     ));
//
//                 }
//             }
//         }
//         if !variables.is_empty() {
//             address_space.add_variables(variables, &devices_folder_id);
//         }
//     }
//
//     // 创建本地通道接收串口数据，并注册到全局发送器列表
//     let (tx_opcua, mut rx_opcua) = tokio_mpsc::channel::<DeviceStatus>(32);
//     let local_tx_arc = Arc::new(tx_opcua);
//     {
//         let mut senders = global_sender.lock().await;
//         senders.push(local_tx_arc.clone());
//     }
//
//     // 异步任务：循环接收串口线程发送的 DeviceStatus，更新对应变量节点的实际值
//     let nm = node_manager.clone();
//     let subs = subscriptions.clone();
//     let ns_index = ns;
//     tokio::spawn(async move {
//         while let Some(status) = rx_opcua.recv().await {
//             // 针对每个 kks 更新一个变量节点
//             for (kks, val) in &status.value {
//                 // 构造节点名，例如 "COM3_1_Value"
//                 let browse_name = format!("{}_{}_{}", status.com, status.device_id, kks);
//                 let node_id = NodeId::new(ns_index, browse_name.clone());
//
//                 // 根据 Value 类型创建 DataValue，使用 Variant
//                 let variant = match val {
//                     Value::UInt(n) => Variant::UInt32(*n),
//                     Value::Bool(b) => Variant::Boolean(*b),
//                     Value::Float(f) => Variant::Float(*f as f32), // 假设 Float 是 f64，转为 f32 或用 Double 如果是 f64
//                 };
//                 let data_value = DataValue::new_now(variant);
//
//                 // 更新 OPC UA 服务器中的变量值
//                 if let Err(e) = nm.set_values(
//                     &subs,
//                     [(&node_id, None, data_value)].into_iter(),
//                 ) {
//                     warn!("Failed to set OPC UA variable value for {}: {:?}", browse_name, e);
//                 }
//             }
//         }
//     });
//
//     // Ctrl+C 处理：收到中断信号后取消服务器运行
//     {
//         let handle_c = handle.clone();
//         tokio::spawn(async move {
//             if let Err(e) = tokio::signal::ctrl_c().await {
//                 warn!("Failed to register CTRL-C handler: {e}");
//                 return;
//             }
//             handle_c.cancel();
//         });
//     }
//
//     // 运行 OPC UA 服务器（阻塞直到退出）
//     server
//         .run()
//         .await
//         .map_err(|e| io::Error::new(io::ErrorKind::Other, format!("OPC UA server error: {e}")))
// }





// ====== 你的函数：带证书/安全端点的完整版本（已标注改动点） ======
pub async fn run_opcua_server(
    serial_ports: Vec<SerialPortConfig>,
    global_sender: Arc<TokioMutex<Vec<Arc<tokio_mpsc::Sender<DeviceStatus>>>>>,
    _system_state: SystemState,
    _system_record: Arc<TokioMutex<BoundedVecDeque<String>>>,
) -> io::Result<()> {
    // 初始化日志（多次调用也安全）
    let _ = env_logger::try_init();

    let host = "0.0.0.0";
    let port: u16 = 4840;
    let base = format!("opc.tcp://{}:{}/", host, port);

    // 创建 OPC UA 服务器，设置产品信息和节点管理器
    let (server, handle) = ServerBuilder::new()
        .host(host)                           // 监听地址
        .port(port)                           // 监听端口
        .discovery_urls(vec![base.clone()])   // 至少一个 discovery url
        // [改动] —— 建议设置应用标识（很多客户端会校验） ——
        .application_name("Serial OPC UA Server")
        .application_uri("urn:SerialServer:rcd_server")
        // [改动] —— 指定 PKI 目录与证书策略 ——
        // 会在 ./pki/own/... 等目录下生成/读取证书与私钥
        .pki_dir("./pki")
        .create_sample_keypair(true)  // 若缺失则自动生成样例密钥/证书（便于调试）
        // 生产环境建议改为你自己的证书路径，并注释掉上面一行：
        // .certificate_path("./pki/own/certs/server.der")         // DER 格式证书
        // .private_key_path("./pki/own/private/server_key.pem")   // PEM 格式私钥


        // === 改成：===
        .add_user_token("user1", ServerUserToken::user_pass("user1", "pwd")) // 仅注册需要凭据的令牌

        // 无安全端点：允许匿名
        .add_endpoint(
            "none",
            ServerEndpoint::new_none("/", &[ANONYMOUS_USER_TOKEN_ID.to_string()]),
        )

        // 安全端点：允许匿名 + 用户名口令（或你也可以只允许 "user1"） //aes256_signenc  basic256sha256_signenc
        .add_endpoint(
            "basic256sha256_signenc",
            ServerEndpoint::new_basic256sha256_sign_encrypt(
                "/",
                &[
                    ANONYMOUS_USER_TOKEN_ID.to_string(), // 允许匿名登录（通道仍加密）
                    "user1".to_string(),                 // 允许用户名口令
                ],
            ),
        )

        // [改动] —— 将安全端点设为默认 //aes256_signenc  basic256sha256_signenc
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
        // 测试期可设为 true：自动信任首次连接的客户端证书
        // 生产建议设为 false，并手动维护 pki/trusted/certs
        .trust_client_certs(true)
        .diagnostics_enabled(true)
        .build()
        .map_err(|e| io::Error::new(io::ErrorKind::Other, format!("Failed to build OPC UA server: {e}")))?;

    // 获取内存节点管理器和订阅缓存
    let node_manager: Arc<SimpleNodeManager> = handle
        .node_managers()
        .get_of_type::<SimpleNodeManager>()
        .expect("Failed to get simple node manager");
    let subscriptions: Arc<SubscriptionCache> = handle.subscriptions().clone();

    // 获取自定义命名空间索引
    let ns = handle
        .get_namespace_index("urn:SerialServer")
        .expect("Custom namespace not registered");

    // 创建 Devices 文件夹，并为每个串口设备的每个 kks 建立变量节点
    let devices_folder_id = NodeId::new(ns, "Devices");
    {
        let mut address_space = node_manager.address_space().write();
        // 添加 Devices 文件夹到 Objects 文件夹下
        address_space.add_folder(
            &devices_folder_id,
            "Devices",
            "Devices",
            &NodeId::objects_folder_id(),
        );

        // 创建变量集合
        let mut variables = Vec::new();
        for port in &serial_ports {
            for cfg in &port.commands {
                for record in &cfg.records {
                    // 节点名形如 "COM3_1_Value"
                    let browse_name = format!("{}_{}_{}", port.port_number, cfg.config.device_id, record.kks);
                    let node_id = NodeId::new(ns, browse_name.clone());

                    // [改动] —— 显式指定 DataType，避免 Variant::Empty 导致的推断失败 panic
                    let expected = expected_type_from_str(&record.type_);
                    let (dt, init) = dt_and_init(expected);

                    variables.push(Variable::new_data_value(
                        &node_id,
                        &browse_name,  // BrowseName
                        &browse_name,  // DisplayName
                        dt,            // DataType
                        Some(-1),      // 标量
                        None,          // 非数组
                        init,          // 初始值
                    ));
                }
            }
        }
        if !variables.is_empty() {
            address_space.add_variables(variables, &devices_folder_id);
        }
    }

    // 创建本地通道接收串口数据，并注册到全局发送器列表
    let (tx_opcua, mut rx_opcua) = tokio_mpsc::channel::<DeviceStatus>(32);
    let local_tx_arc = Arc::new(tx_opcua);
    {
        let mut senders = global_sender.lock().await;
        senders.push(local_tx_arc.clone());
    }

    // 异步任务：循环接收串口线程发送的 DeviceStatus，更新对应变量节点的实际值
    let nm = node_manager.clone();
    let subs = subscriptions.clone();
    let ns_index = ns;
    tokio::spawn(async move {
        while let Some(status) = rx_opcua.recv().await {
            // 针对每个 kks 更新一个变量节点
            for (kks, val) in &status.value {
                // 构造节点名，例如 "COM3_1_Value"
                let browse_name = format!("{}_{}_{}", status.com, status.device_id, kks);
                let node_id = NodeId::new(ns_index, browse_name.clone());

                // 根据 Value 类型创建 DataValue，类型需与建点时一致
                let variant = match val {
                    Value::UInt(n) => Variant::UInt32(*n),
                    Value::Bool(b) => Variant::Boolean(*b),
                    Value::Float(f)=> Variant::Float(*f), // 若你的 Float 是 f64，请改用 Variant::Double(*f)
                };
                let data_value = DataValue::new_now(variant);

                // 更新 OPC UA 服务器中的变量值
                if let Err(e) = nm.set_values(&subs, [(&node_id, None, data_value)].into_iter()) {
                    warn!("Failed to set OPC UA variable value for {}: {:?}", browse_name, e);
                }
            }
        }
    });

    // Ctrl+C 处理：收到中断信号后取消服务器运行
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