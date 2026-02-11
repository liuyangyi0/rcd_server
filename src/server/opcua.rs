//! OPC UA 服务器模块。
//!
//! 将串口采集到的设备数据发布为 OPC UA 变量节点，支持客户端写命令下发。

use std::collections::HashMap;
use std::io;
use std::sync::{Arc, mpsc};
use std::time::Duration;

use bounded_vec_deque::BoundedVecDeque;
use log::{info, warn};
use tokio::sync::Mutex as TokioMutex;

use crate::broadcast::Broadcaster;
use crate::model::{Command, CommandType, SendData, SystemState, Value, get_sum};
use crate::protocol::hex_str_to_bytes;
use crate::serial::port_config::SerialPortConfig;

// OPC UA 相关导入
use opcua::nodes::AccessLevel;
use opcua::server::address_space::{
    EventNotifier, MethodBuilder, NodeType, ObjectBuilder, Variable,
};
use opcua::server::diagnostics::NamespaceMetadata;
use opcua::server::node_manager::memory::{SimpleNodeManager, simple_node_manager};
use opcua::server::{
    ServerBuilder, ServerEndpoint, ServerUserToken, SubscriptionCache,
    ANONYMOUS_USER_TOKEN_ID,
};
use opcua::types::{
    BuildInfo, DataEncoding, DataTypeId, DataValue, DateTime as UaDateTime,
    NodeId, NumericRange, ObjectId, StatusCode, TimestampsToReturn, UAString, Variant,
};

// ============================================================
//  OPC UA 辅助类型与函数
// ============================================================

/// OPC UA 节点期望的数据类型。
#[derive(Clone, Copy, Debug)]
enum ExpectedType {
    UInt32,
    Bool,
    Float32,
    Float64,
}

/// 从类型字符串映射到 [`ExpectedType`]。
fn expected_type_from_str(s: &str) -> ExpectedType {
    match s.to_ascii_lowercase().as_str() {
        "u32" | "uint32" | "uint" => ExpectedType::UInt32,
        "bool" | "boolean"        => ExpectedType::Bool,
        "f32" | "float"           => ExpectedType::Float32,
        "f64" | "double"          => ExpectedType::Float64,
        _                         => ExpectedType::Float32,
    }
}

/// 根据期望类型返回 OPC UA 数据类型 NodeId 与初始值。
fn datatype_and_initial(expected: ExpectedType) -> (NodeId, Variant) {
    match expected {
        ExpectedType::UInt32  => (NodeId::from(DataTypeId::UInt32),  Variant::UInt32(0)),
        ExpectedType::Bool    => (NodeId::from(DataTypeId::Boolean), Variant::Boolean(false)),
        ExpectedType::Float32 => (NodeId::from(DataTypeId::Float),   Variant::Float(0.0)),
        ExpectedType::Float64 => (NodeId::from(DataTypeId::Double),  Variant::Double(0.0)),
    }
}

// ============================================================
//  OPC UA 服务器
// ============================================================

/// 运行 OPC UA 服务器，将串口数据发布为独立变量节点。
///
/// 节点命名：`<串口号>_<设备ID>_<kks>`。
/// 每个串口额外创建一个可写的 `<串口号>_Send` 字符串节点，
/// 客户端可通过写入十六进制字符串向设备下发命令。
pub async fn run_opcua_server(
    serial_ports: Vec<SerialPortConfig>,
    broadcaster: Broadcaster,
    _system_state: SystemState,
    _system_record: Arc<TokioMutex<BoundedVecDeque<String>>>,
    txs: Vec<mpsc::Sender<Command>>,
) -> io::Result<()> {
    let host = "0.0.0.0";
    let port: u16 = 4840;
    let base = format!("opc.tcp://{}:{}/", host, port);

    // ---- 构建 OPC UA 服务器 ----
    let (server, handle) = ServerBuilder::new()
        .host(host)
        .port(port)
        .discovery_urls(vec![base.clone()])
        .application_name("Serial OPC UA Server")
        .application_uri("urn:SerialServer:rcd_server")
        .pki_dir("./pki")
        .create_sample_keypair(true)
        .add_user_token("user1", ServerUserToken::user_pass("user1", "pwd"))
        .add_endpoint(
            "none",
            ServerEndpoint::new_none("/", &[ANONYMOUS_USER_TOKEN_ID.to_string()]),
        )
        .add_endpoint(
            "basic256sha256_signenc",
            ServerEndpoint::new_basic256sha256_sign_encrypt(
                "/",
                &[ANONYMOUS_USER_TOKEN_ID.to_string(), "user1".to_string()],
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
        .map_err(|e| io::Error::new(io::ErrorKind::Other, format!("构建 OPC UA 服务器失败: {e}")))?;

    let node_manager: Arc<SimpleNodeManager> = handle
        .node_managers()
        .get_of_type::<SimpleNodeManager>()
        .expect("获取节点管理器失败");
    let subscriptions: Arc<SubscriptionCache> = handle.subscriptions().clone();
    let ns = handle
        .get_namespace_index("urn:SerialServer")
        .expect("自定义命名空间未注册");

    // ---- 创建 OPC UA 节点（变量 + Method）----
    let opcua_nodes = create_opcua_nodes(&node_manager, ns, &serial_ports);
    let send_node_ids = opcua_nodes.send_node_ids;
    let kks_node_map = Arc::new(opcua_nodes.kks_node_map);

    // ---- 注册 Method 回调 ----
    register_send_methods(&node_manager, ns, &serial_ports, &txs);

    // ---- 注册本地通道接收串口数据 ----
    let mut rx_opcua = broadcaster.subscribe(32).await;

    // ---- 任务 1: 串口数据 -> OPC UA 节点更新 ----
    let nm_update = node_manager.clone();
    let subs_update = subscriptions.clone();
    let kks_map = kks_node_map.clone();
    tokio::spawn(async move {
        while let Some(status) = rx_opcua.recv().await {
            // 批量收集本次广播的所有变量更新
            let updates: Vec<_> = status.value.iter().filter_map(|(kks, val)| {
                let browse_name = format!("{}_{}_{}", status.com, status.device_id, kks);
                let node_id = kks_map.get(&browse_name)?;
                let variant = match val {
                    Value::UInt(n)  => Variant::UInt32(*n),
                    Value::Bool(b)  => Variant::Boolean(*b),
                    Value::Float(f) => Variant::Float(*f),
                };
                Some((node_id, None, DataValue::new_now(variant)))
            }).collect();

            if !updates.is_empty() {
                // 一次性写入所有变量，只获取一次写锁
                if let Err(e) = nm_update.set_values(&subs_update, updates.into_iter()) {
                    warn!("批量更新 OPC UA 变量失败: {:?}", e);
                }
            }
        }
    });

    // ---- 任务 2: 监听客户端写命令节点 ----
    let nm_send = node_manager.clone();
    let subs_send = subscriptions.clone();
    let txs_clone = txs.clone();
    let serial_ports_clone = serial_ports.clone();
    let system_record_clone = _system_record.clone();

    tokio::spawn(async move {
        loop {
            let to_clear = poll_and_forward_commands(
                &nm_send, &send_node_ids, &serial_ports_clone,
                &txs_clone, &system_record_clone,
            ).await;

            // 清零已处理的写命令节点
            for id in to_clear {
                let clear_val = DataValue::new_now(Variant::String(UAString::from("0")));
                let iter = vec![(&id, None, clear_val)].into_iter();
                if let Err(e) = nm_send.set_values(&subs_send, iter) {
                    warn!("清零写命令节点失败: {:?}", e);
                }
            }

            tokio::time::sleep(Duration::from_millis(200)).await;
        }
    });

    // ---- Ctrl+C 优雅退出 ----
    {
        let handle_c = handle.clone();
        tokio::spawn(async move {
            if let Err(e) = tokio::signal::ctrl_c().await {
                warn!("注册 Ctrl+C 处理器失败: {e}");
                return;
            }
            handle_c.cancel();
        });
    }

    server
        .run()
        .await
        .map_err(|e| io::Error::new(io::ErrorKind::Other, format!("OPC UA 服务器错误: {e}")))
}

/// 初始化后的 OPC UA 节点信息。
struct OpcuaNodes {
    /// 写命令节点：(COM, NodeId)。
    send_node_ids: Vec<(String, NodeId)>,
    /// KKS browse_name → 预构建的 NodeId，避免运行时重复分配。
    kks_node_map: HashMap<String, NodeId>,
}

/// 在地址空间中创建所有设备变量节点和写命令节点。
fn create_opcua_nodes(
    node_manager: &Arc<SimpleNodeManager>,
    ns: u16,
    serial_ports: &[SerialPortConfig],
) -> OpcuaNodes {
    let devices_folder_id = NodeId::new(ns, "Devices");
    let mut send_node_ids: Vec<(String, NodeId)> = Vec::new();
    let mut kks_node_map = HashMap::new();

    let mut address_space = node_manager.address_space().write();
    address_space.add_folder(&devices_folder_id, "Devices", "Devices", &NodeId::objects_folder_id());

    let mut variables = Vec::new();

    for port in serial_ports {
        // 数据变量节点
        for cfg in &port.devices {
            for record in &cfg.records {
                let browse_name = format!("{}_{}_{}", port.port_number, cfg.config.device_id, record.kks);
                let node_id = NodeId::new(ns, browse_name.clone());
                let expected = expected_type_from_str(&record.type_);
                let (dt, init) = datatype_and_initial(expected);
                variables.push(Variable::new_data_value(
                    &node_id, &browse_name, &browse_name, dt, Some(-1), None, init,
                ));
                kks_node_map.insert(browse_name, node_id);
            }
        }

        // 写命令节点（可写字符串）
        let send_name = format!("{}_Send", port.port_number);
        let send_node_id = NodeId::new(ns, send_name.clone());
        variables.push(Variable::new_data_value(
            &send_node_id, &send_name, &send_name,
            NodeId::from(DataTypeId::String), Some(-1), None,
            Variant::String(UAString::from("")),
        ));
        send_node_ids.push((port.port_number.clone(), send_node_id));
    }

    if !variables.is_empty() {
        address_space.add_variables(variables, &devices_folder_id);
    }

    // 在同一次写锁内设置写命令节点的写权限
    for (_, node_id) in &send_node_ids {
        if let Some(NodeType::Variable(var)) = address_space.find_mut(node_id) {
            var.set_access_level(AccessLevel::CURRENT_READ | AccessLevel::CURRENT_WRITE);
            var.set_user_access_level(AccessLevel::CURRENT_READ | AccessLevel::CURRENT_WRITE);
        } else {
            warn!("未找到写命令节点: {:?}", node_id);
        }
    }

    OpcuaNodes { send_node_ids, kks_node_map }
}

/// 为每个串口创建 `SendCommand` Method 节点，客户端可通过 OPC UA Call 服务直接调用。
///
/// 输入参数：`HexData`（String）——十六进制命令字符串。
/// 输出参数：`Result`（String）——执行结果描述。
fn register_send_methods(
    node_manager: &Arc<SimpleNodeManager>,
    ns: u16,
    serial_ports: &[SerialPortConfig],
    txs: &[mpsc::Sender<Command>],
) {
    // 创建 Methods 父对象文件夹
    let methods_folder_id = NodeId::new(ns, "Methods");
    {
        let mut address_space = node_manager.address_space().write();
        ObjectBuilder::new(&methods_folder_id, "Methods", "Methods")
            .event_notifier(EventNotifier::empty())
            .organized_by(ObjectId::ObjectsFolder)
            .insert(&mut *address_space);
    }

    for (idx, port) in serial_ports.iter().enumerate() {
        let method_name = format!("{}_SendCommand", port.port_number);
        let fn_node_id = NodeId::new(ns, method_name.clone());
        let input_id = NodeId::new(ns, format!("{}_Input", method_name));
        let output_id = NodeId::new(ns, format!("{}_Output", method_name));

        {
            let mut address_space = node_manager.address_space().write();
            MethodBuilder::new(&fn_node_id, &method_name, &method_name)
                .component_of(methods_folder_id.clone())
                .input_args(
                    &mut *address_space,
                    &input_id,
                    &[("HexData", DataTypeId::String).into()],
                )
                .output_args(
                    &mut *address_space,
                    &output_id,
                    &[("Result", DataTypeId::String).into()],
                )
                .insert(&mut *address_space);
        }

        let com = port.port_number.clone();
        let tx = txs[idx].clone();

        node_manager.inner().add_method_callback(fn_node_id, move |args| {
            let Some(Variant::String(ref hex_ua)) = args.first() else {
                return Err(StatusCode::BadTypeMismatch);
            };
            let hex_str = hex_ua.as_ref().trim();
            if hex_str.is_empty() {
                return Err(StatusCode::BadInvalidArgument);
            }

            match forward_hex_command(hex_str, &com, &tx) {
                Ok(msg) => Ok(vec![Variant::String(UAString::from(msg))]),
                Err(msg) => {
                    warn!("Method 调用失败: {}", msg);
                    Ok(vec![Variant::String(UAString::from(msg))])
                }
            }
        });
    }
}

/// 解析十六进制字符串并转发到串口线程（同步，供 Method 回调和轮询共用）。
fn forward_hex_command(
    hex_str: &str,
    com: &str,
    tx: &mpsc::Sender<Command>,
) -> Result<String, String> {
    let mut bytes = hex_str_to_bytes(hex_str).map_err(|e| format!("十六进制解析失败: {}", e))?;
    if bytes.len() < 2 {
        return Err("十六进制长度过短（至少需 2 字节）".to_string());
    }
    bytes.push(get_sum(&bytes));
    let device_id = bytes[0] as u32;
    let send_data = SendData { device_id, command: bytes };
    let cmd = Command {
        com: com.to_string(),
        command: CommandType::SendData(send_data),
    };
    tx.send(cmd).map_err(|e| format!("转发命令到串口失败: {:?}", e))?;
    Ok(format!("已发送到 {} 设备 {}", com, device_id))
}

/// 轮询写命令节点，转发命令到串口线程，返回需要清零的节点 ID 列表。
async fn poll_and_forward_commands(
    nm: &Arc<SimpleNodeManager>,
    send_node_ids: &[(String, NodeId)],
    serial_ports: &[SerialPortConfig],
    txs: &[mpsc::Sender<Command>],
    system_record: &Arc<TokioMutex<BoundedVecDeque<String>>>,
) -> Vec<NodeId> {
    let mut to_clear = Vec::new();
    let address_space = nm.address_space().read();

    for (com, node_id) in send_node_ids {
        let node = match address_space.find_node(node_id) {
            Some(n) => n,
            None => continue,
        };

        if let NodeType::Variable(var) = node {
            let dv = var.value(
                TimestampsToReturn::Both,
                &NumericRange::default(),
                &DataEncoding::default(),
                0.0,
            );

            if let Some(Variant::String(ref ua_str)) = dv.value {
                let v = ua_str.as_ref().trim();
                if v.is_empty() || v == "0" {
                    continue;
                }

                info!("收到 OPC 客户端写命令: 串口 {} 数据 {}", com, v);

                if let Some((idx, _)) = serial_ports.iter().enumerate().find(|(_, p)| &p.port_number == com) {
                    match forward_hex_command(v, com, &txs[idx]) {
                        Ok(msg) => {
                            info!("{}", msg);
                            let local = chrono::Local::now();
                            let mut record = system_record.lock().await;
                            record.push_back(format!(
                                "时间: {} 串口: {} 数据: {}",
                                local.format("%Y-%m-%d %H:%M:%S"),
                                com, v,
                            ));
                        }
                        Err(e) => warn!("{}", e),
                    }
                } else {
                    warn!("未找到匹配的串口: {}", com);
                }

                to_clear.push(node_id.clone());
            }
        }
    }

    to_clear
}
