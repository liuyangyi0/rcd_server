//! OPC UA 服务器模块。
//!
//! 将串口采集到的设备数据发布为 OPC UA 变量节点，支持客户端写命令下发。

/// OPC UA 默认监听地址。
pub const OPCUA_BIND_HOST: &str = "0.0.0.0";
/// OPC UA 默认监听端口。
pub const OPCUA_BIND_PORT: u16 = 4840;
/// 写命令节点轮询间隔。
pub const SEND_NODE_POLL_INTERVAL_MS: u64 = 200;

use std::collections::HashMap;
use std::io;
use std::sync::{mpsc, Arc, Mutex};
use std::time::Duration;

use bounded_vec_deque::BoundedVecDeque;
use log::{info, warn};
use tokio_util::sync::CancellationToken;

use crate::broadcast::Broadcaster;
use crate::calc_engine::config::{normalize_data_type, CalcDataType};
use crate::calc_engine::engine::CalcEngine;
use crate::calc_engine::runner::CalcRunner;
use crate::model::{get_sum, Command, CommandType, SendData, SystemState, Value};
use crate::protocol::hex_str_to_bytes;
use crate::serial::port_config::SerialPortConfig;

// OPC UA 相关导入
use opcua::nodes::AccessLevel;
use opcua::server::address_space::{
    EventNotifier, MethodBuilder, NodeType, ObjectBuilder, Variable,
};
use opcua::server::diagnostics::NamespaceMetadata;
use opcua::server::node_manager::memory::{simple_node_manager, SimpleNodeManager};
use opcua::server::{
    ServerBuilder, ServerEndpoint, ServerUserToken, SubscriptionCache, ANONYMOUS_USER_TOKEN_ID,
};
use opcua::types::{
    BuildInfo, DataEncoding, DataTypeId, DataValue, DateTime as UaDateTime, NodeId, NumericRange,
    ObjectId, StatusCode, TimestampsToReturn, UAString, Variant,
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
    match normalize_data_type(s) {
        Some(CalcDataType::UInt) => ExpectedType::UInt32,
        Some(CalcDataType::Bool) => ExpectedType::Bool,
        Some(CalcDataType::Float) => ExpectedType::Float32,
        Some(CalcDataType::Double) => ExpectedType::Float64,
        None => ExpectedType::Float32,
    }
}

/// 根据期望类型返回 OPC UA 数据类型 NodeId 与初始值。
fn datatype_and_initial(expected: ExpectedType) -> (NodeId, Variant) {
    match expected {
        ExpectedType::UInt32 => (NodeId::from(DataTypeId::UInt32), Variant::UInt32(0)),
        ExpectedType::Bool => (NodeId::from(DataTypeId::Boolean), Variant::Boolean(false)),
        ExpectedType::Float32 => (NodeId::from(DataTypeId::Float), Variant::Float(0.0)),
        ExpectedType::Float64 => (NodeId::from(DataTypeId::Double), Variant::Double(0.0)),
    }
}

/// 将 `model::Value` 映射为 OPC UA `Variant`。
fn value_to_variant(v: &Value) -> Variant {
    match v {
        Value::UInt(n) => Variant::UInt32(*n),
        Value::Bool(b) => Variant::Boolean(*b),
        Value::Float(f) => Variant::Float(*f),
        Value::Double(f) => Variant::Double(*f),
    }
}

// ============================================================
//  OPC UA 服务器
// ============================================================

/// 运行 OPC UA 服务器，将串口数据发布为独立变量节点。
///
/// 节点命名约定：
/// - 设备数据节点（只读）：纯 KKS 名，如 `9CYE91GH201_READY`。
///   要求 KKS 在所有串口/设备间全局唯一（在 CSV 的 `kks_prefix` 中保证）。
/// - 计算衍生节点（只读）：纯 `kks_calc`，如 `91UGM91110_Fire`。
/// - 写命令节点（可写）：`<串口号>_Send`，如 `ttyAP0_Send`，
///   客户端可通过写入十六进制字符串向设备下发命令。
pub async fn run_opcua_server(
    serial_ports: Vec<SerialPortConfig>,
    broadcaster: Broadcaster,
    _system_state: SystemState,
    _system_record: Arc<Mutex<BoundedVecDeque<String>>>,
    txs: Vec<mpsc::Sender<Command>>,
    calc_engine: Arc<CalcEngine>,
    cancel: CancellationToken,
) -> io::Result<()> {
    let host = OPCUA_BIND_HOST;
    let port: u16 = OPCUA_BIND_PORT;
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
        .map_err(|e| {
            io::Error::new(io::ErrorKind::Other, format!("构建 OPC UA 服务器失败: {e}"))
        })?;

    let node_manager: Arc<SimpleNodeManager> = handle
        .node_managers()
        .get_of_type::<SimpleNodeManager>()
        .expect("获取节点管理器失败");
    let subscriptions: Arc<SubscriptionCache> = handle.subscriptions().clone();
    let ns = handle
        .get_namespace_index("urn:SerialServer")
        .expect("自定义命名空间未注册");

    // ---- 创建 OPC UA 节点（变量 + Method）----
    let opcua_nodes = create_opcua_nodes(&node_manager, ns, &serial_ports, &calc_engine);
    let send_node_ids = opcua_nodes.send_node_ids;
    let kks_node_map = Arc::new(opcua_nodes.kks_node_map);

    // ---- 注册 Method 回调 ----
    register_send_methods(&node_manager, ns, &serial_ports, &txs);

    // ---- 注册本地通道接收串口数据 ----
    let mut rx_opcua = broadcaster.subscribe();

    // ---- 任务 1: 串口数据 -> OPC UA 节点更新 ----
    let nm_update = node_manager.clone();
    let subs_update = subscriptions.clone();
    let kks_map = kks_node_map.clone();
    let mut calc_runner = CalcRunner::new(calc_engine.clone());
    let cancel_update = cancel.clone();
    let task_update = tokio::spawn(async move {
        loop {
            let status = tokio::select! {
                biased;
                _ = cancel_update.cancelled() => break,
                res = rx_opcua.recv() => match res {
                    Ok(s) => s,
                    Err(tokio::sync::broadcast::error::RecvError::Lagged(n)) => {
                        warn!("[OPC UA] 广播订阅落后 {} 条，已丢帧", n);
                        continue;
                    }
                    Err(tokio::sync::broadcast::error::RecvError::Closed) => break,
                }
            };

            // ---- 1. 更新设备 OPC UA 节点（按纯 KKS 查询）----
            let mut updates: Vec<_> = status
                .value
                .iter()
                .filter_map(|(kks, val)| {
                    let node_id = kks_map.get(kks.as_str())?;
                    Some((node_id, None, DataValue::new_now(value_to_variant(val))))
                })
                .collect();

            // ---- 2. 计算引擎：CalcRunner 累积变量并执行公式 ----
            for (kks_calc, val) in calc_runner.on_update(&status) {
                if let Some(node_id) = kks_map.get(&kks_calc) {
                    updates.push((node_id, None, DataValue::new_now(value_to_variant(&val))));
                }
            }

            // ---- 3. 批量写入所有变量（设备 + 计算），只获取一次写锁 ----
            if !updates.is_empty() {
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
    let cancel_poll = cancel.clone();

    let task_poll = tokio::spawn(async move {
        loop {
            tokio::select! {
                biased;
                _ = cancel_poll.cancelled() => break,
                _ = tokio::time::sleep(Duration::from_millis(SEND_NODE_POLL_INTERVAL_MS)) => {}
            }

            let to_clear = poll_and_forward_commands(
                &nm_send,
                &send_node_ids,
                &serial_ports_clone,
                &txs_clone,
                &system_record_clone,
            )
            .await;

            // 清零已处理的写命令节点
            for id in to_clear {
                let clear_val = DataValue::new_now(Variant::String(UAString::from("0")));
                let iter = vec![(&id, None, clear_val)].into_iter();
                if let Err(e) = nm_send.set_values(&subs_send, iter) {
                    warn!("清零写命令节点失败: {:?}", e);
                }
            }
        }
    });

    // 外部取消信号 → OPC UA 服务器自身取消（Ctrl+C 已在 main 注册）
    {
        let handle_c = handle.clone();
        let cancel_bridge = cancel.clone();
        tokio::spawn(async move {
            cancel_bridge.cancelled().await;
            handle_c.cancel();
        });
    }

    let result = server
        .run()
        .await
        .map_err(|e| io::Error::new(io::ErrorKind::Other, format!("OPC UA 服务器错误: {e}")));

    // 服务器退出后，确保取消信号触发 + 等待子任务收尾
    cancel.cancel();
    let _ = task_update.await;
    let _ = task_poll.await;

    result
}

/// 初始化后的 OPC UA 节点信息。
struct OpcuaNodes {
    /// 写命令节点：(COM, NodeId)。
    send_node_ids: Vec<(String, NodeId)>,
    /// KKS browse_name → 预构建的 NodeId，避免运行时重复分配。
    kks_node_map: HashMap<String, NodeId>,
}

/// 计算所有 OPC UA 节点的 browse_name（纯逻辑，便于单元测试）。
///
/// 返回 `(data_browse_names, send_pairs)`：
/// - `data_browse_names`：所有只读节点（设备数据 + 计算衍生），均为纯 KKS 名。
/// - `send_pairs`：每个串口的写命令节点 `(port_number, "<port>_Send")`。
#[cfg(test)]
fn plan_browse_names(
    serial_ports: &[SerialPortConfig],
    calc_engine: &CalcEngine,
) -> (Vec<String>, Vec<(String, String)>) {
    let mut data_names = Vec::new();
    let mut send_pairs = Vec::new();
    for port in serial_ports {
        for cfg in &port.devices {
            for record in &cfg.records {
                data_names.push(record.kks.clone());
            }
        }
        send_pairs.push((
            port.port_number.clone(),
            format!("{}_Send", port.port_number),
        ));
    }
    for (kks_calc, _) in calc_engine.output_definitions() {
        data_names.push(kks_calc.to_string());
    }
    (data_names, send_pairs)
}

/// 在地址空间中创建所有设备变量节点、写命令节点和计算引擎衍生变量节点。
fn create_opcua_nodes(
    node_manager: &Arc<SimpleNodeManager>,
    ns: u16,
    serial_ports: &[SerialPortConfig],
    calc_engine: &CalcEngine,
) -> OpcuaNodes {
    let devices_folder_id = NodeId::new(ns, "Devices");
    let mut send_node_ids: Vec<(String, NodeId)> = Vec::new();
    let mut kks_node_map = HashMap::new();

    let mut address_space = node_manager.address_space().write();
    address_space.add_folder(
        &devices_folder_id,
        "Devices",
        "Devices",
        &NodeId::objects_folder_id(),
    );

    let mut variables = Vec::new();

    for port in serial_ports {
        // 数据变量节点：使用纯 KKS 作为 browse_name（要求 KKS 全局唯一）
        for cfg in &port.devices {
            for record in &cfg.records {
                let browse_name = record.kks.clone();
                let node_id = NodeId::new(ns, browse_name.clone());
                let expected = expected_type_from_str(&record.type_);
                let (dt, init) = datatype_and_initial(expected);
                variables.push(Variable::new_data_value(
                    &node_id,
                    &browse_name,
                    &browse_name,
                    dt,
                    Some(-1),
                    None,
                    init,
                ));
                kks_node_map.insert(browse_name, node_id);
            }
        }

        // 写命令节点（可写字符串）
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

    // ---- 计算引擎衍生变量节点 ----
    for (kks_calc, data_type) in calc_engine.output_definitions() {
        let node_id = NodeId::new(ns, kks_calc.to_string());
        let expected = expected_type_from_str(data_type);
        let (dt, init) = datatype_and_initial(expected);
        variables.push(Variable::new_data_value(
            &node_id,
            kks_calc,
            kks_calc,
            dt,
            Some(-1),
            None,
            init,
        ));
        kks_node_map.insert(kks_calc.to_string(), node_id);
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

    OpcuaNodes {
        send_node_ids,
        kks_node_map,
    }
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

        node_manager
            .inner()
            .add_method_callback(fn_node_id, move |args| {
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
    // 移除空格，支持 "0A 1B FF" 格式
    let cleaned: String = hex_str
        .chars()
        .filter(|c| !c.is_ascii_whitespace())
        .collect();
    let mut bytes = hex_str_to_bytes(&cleaned).map_err(|e| format!("十六进制解析失败: {}", e))?;
    if bytes.len() < 2 {
        return Err("十六进制长度过短（至少需 2 字节）".to_string());
    }
    bytes.push(get_sum(&bytes));
    let device_id = bytes[0] as u32;
    let send_data = SendData {
        device_id,
        command: bytes,
    };
    let cmd = Command {
        com: com.to_string(),
        command: CommandType::SendData(send_data),
    };
    tx.send(cmd)
        .map_err(|e| format!("转发命令到串口失败: {:?}", e))?;
    Ok(format!("已发送到 {} 设备 {}", com, device_id))
}

/// 轮询写命令节点，转发命令到串口线程，返回需要清零的节点 ID 列表。
async fn poll_and_forward_commands(
    nm: &Arc<SimpleNodeManager>,
    send_node_ids: &[(String, NodeId)],
    serial_ports: &[SerialPortConfig],
    txs: &[mpsc::Sender<Command>],
    system_record: &Arc<Mutex<BoundedVecDeque<String>>>,
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

                if let Some((idx, _)) = serial_ports
                    .iter()
                    .enumerate()
                    .find(|(_, p)| &p.port_number == com)
                {
                    match forward_hex_command(v, com, &txs[idx]) {
                        Ok(msg) => {
                            info!("{}", msg);
                            let local = chrono::Local::now();
                            let mut record =
                                system_record.lock().unwrap_or_else(|e| e.into_inner());
                            record.push_back(format!(
                                "时间: {} 串口: {} 数据: {}",
                                local.format("%Y-%m-%d %H:%M:%S"),
                                com,
                                v,
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

// ============================================================
//  单元测试
// ============================================================

#[cfg(test)]
mod tests {
    use super::*;
    use crate::calc_engine::config::CalcRule;
    use crate::csv_parser::{BitIndex, Config, DeviceConfig, Record};

    fn make_record(kks: &str) -> Record {
        Record {
            kks: kks.to_string(),
            type_: "uint".to_string(),
            _f_type: String::new(),
            byte_index: 1,
            bit_index: BitIndex::Single(0),
            _def: 0,
            _max: 0,
            _min: 0,
            byte_order: 0,
        }
    }

    fn make_device(device_id: u8, kks_list: &[&str]) -> DeviceConfig {
        DeviceConfig {
            config: Config {
                com: "ttyAP0".into(),
                baud_rate: 115200,
                is_special: false,
                com_index: 0,
                device_id,
                data_len: 8,
                kks_prefix: String::new(),
            },
            records: kks_list.iter().map(|k| make_record(k)).collect(),
        }
    }

    fn make_port(port_number: &str, devices: Vec<DeviceConfig>) -> SerialPortConfig {
        SerialPortConfig {
            port_number: port_number.to_string(),
            baud_rate: 115200,
            is_special: false,
            devices,
            status: true,
        }
    }

    fn make_calc_engine(rules: Vec<(&str, &str, &str)>) -> CalcEngine {
        let cfg_rules: Vec<CalcRule> = rules
            .into_iter()
            .map(|(kks, dt, expr)| CalcRule {
                kks_calc: kks.into(),
                data_type: dt.into(),
                expression: expr.into(),
                max_limit: f64::MAX,
                min_limit: f64::MIN,
                default_value: 0.0,
            })
            .collect();
        CalcEngine::new(cfg_rules).expect("构建测试引擎失败")
    }

    #[test]
    fn device_data_node_uses_pure_kks() {
        let port = make_port(
            "ttyAP0",
            vec![make_device(11, &["9CYE91GH201_READY", "9CYE91GH201_SL1"])],
        );
        let engine = make_calc_engine(vec![]);
        let (data_names, _) = plan_browse_names(&[port], &engine);
        assert!(data_names.contains(&"9CYE91GH201_READY".to_string()));
        assert!(data_names.contains(&"9CYE91GH201_SL1".to_string()));
        assert!(
            !data_names.iter().any(|n| n.contains("ttyAP0")),
            "设备数据节点不应有串口前缀: {:?}",
            data_names,
        );
    }

    #[test]
    fn send_node_keeps_port_prefix() {
        let port = make_port("ttyAP0", vec![make_device(11, &["X"])]);
        let engine = make_calc_engine(vec![]);
        let (_, send_pairs) = plan_browse_names(&[port], &engine);
        assert_eq!(send_pairs.len(), 1);
        assert_eq!(send_pairs[0].0, "ttyAP0");
        assert_eq!(send_pairs[0].1, "ttyAP0_Send");
    }

    #[test]
    fn calc_derived_uses_pure_kks() {
        let port = make_port("ttyAP0", vec![make_device(11, &["base"])]);
        let engine = make_calc_engine(vec![("91UGM91110_Fire", "uint", "base + 1")]);
        let (data_names, _) = plan_browse_names(&[port], &engine);
        assert!(data_names.contains(&"91UGM91110_Fire".to_string()));
        assert!(
            !data_names.iter().any(|n| n.contains("ttyAP0_Fire")),
            "计算衍生节点不应有串口前缀: {:?}",
            data_names,
        );
    }

    #[test]
    fn multi_port_device_keeps_each_send() {
        let p0 = make_port("ttyAP0", vec![make_device(11, &["a"])]);
        let p1 = make_port("ttyAP1", vec![make_device(12, &["b"])]);
        let engine = make_calc_engine(vec![]);
        let (data_names, send_pairs) = plan_browse_names(&[p0, p1], &engine);
        assert_eq!(data_names, vec!["a", "b"]);
        assert_eq!(
            send_pairs,
            vec![
                ("ttyAP0".into(), "ttyAP0_Send".into()),
                ("ttyAP1".into(), "ttyAP1_Send".into()),
            ]
        );
    }
}
