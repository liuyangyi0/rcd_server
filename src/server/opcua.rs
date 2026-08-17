//! OPC UA 服务器模块。
//!
//! 将串口采集到的设备数据发布为 OPC UA 变量节点，支持客户端写命令下发。

/// OPC UA 默认监听地址。
pub const OPCUA_BIND_HOST: &str = "0.0.0.0";
/// OPC UA 默认监听端口。
pub const OPCUA_BIND_PORT: u16 = 4840;
/// 命令 ACK 状态回写检查间隔。
pub const ACK_STATUS_DRAIN_INTERVAL_MS: u64 = 50;
/// OPC UA 大批量 Browse / 订阅的变量数余量。
pub const OPCUA_LIMIT_MARGIN: usize = 512;
/// OPC UA 大批量响应的最小消息上限。
pub const OPCUA_MIN_MAX_MESSAGE_SIZE: usize = 16 * 1024 * 1024;
/// OPC UA 大批量响应的最小分片数上限。
pub const OPCUA_MIN_MAX_CHUNK_COUNT: usize = 256;
/// OPC 写命令日志最多展示的字符数，避免异常大输入阻塞控制台和日志文件。
const OPC_WRITE_LOG_PREVIEW_CHARS: usize = 256;
/// `_Send` 节点启动时的非空字符串值，兼容按当前值推断点类型的 OPC 客户端。
const SEND_NODE_IDLE_VALUE: &str = "IDLE";

use std::collections::hash_map::Entry;
use std::collections::HashMap;
use std::io;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{mpsc, Arc, Mutex};
use std::time::{Duration, Instant};

use bounded_vec_deque::BoundedVecDeque;
use log::{info, warn};
use tokio::net::TcpListener;
use tokio_util::sync::CancellationToken;

use crate::broadcast::Broadcaster;
use crate::calc_engine::config::{normalize_data_type, CalcDataType};
use crate::calc_engine::engine::CalcEngine;
use crate::calc_engine::runner::CalcRunner;
use crate::model::{
    get_sum, Command, CommandAckStatus, CommandAckUpdate, CommandType, DeviceStatus, SendData,
    SystemState, Value,
};
use crate::protocol::hex_str_to_bytes;
use crate::runtime_cache::LatestStatusCache;
use crate::serial::port_config::SerialPortConfig;

// OPC UA 相关导入
use opcua::nodes::AccessLevel;
use opcua::server::address_space::{
    AddressSpace, EventNotifier, MethodBuilder, NodeType, ObjectBuilder, ReferenceDirection,
    Variable,
};
use opcua::server::diagnostics::NamespaceMetadata;
use opcua::server::node_manager::memory::{simple_node_manager, SimpleNodeManager};
use opcua::server::{
    ServerBuilder, ServerEndpoint, ServerUserToken, SubscriptionCache, ANONYMOUS_USER_TOKEN_ID,
};
use opcua::types::{
    BuildInfo, DataTypeId, DataValue, DateTime as UaDateTime, NodeId, NumericRange, ObjectId,
    ObjectTypeId, QualifiedName, ReferenceTypeId, StatusCode, UAString, VariableTypeId, Variant,
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

/// 根据业务值类型生成对应的 OPC UA 零值。
fn zero_variant_for_value(v: &Value) -> Variant {
    match v {
        Value::UInt(_) => Variant::UInt32(0),
        Value::Bool(_) => Variant::Boolean(false),
        Value::Float(_) => Variant::Float(0.0),
        Value::Double(_) => Variant::Double(0.0),
    }
}

type OpcuaValueUpdate<'a> = (&'a NodeId, Option<&'a NumericRange>, DataValue);

fn unavailable_data_value(value: Variant, status: StatusCode) -> DataValue {
    DataValue {
        // async-opcua 0.16 的 Variable::value 仅在 value 为 Some 时复制 StatusCode；
        // 启动等待数据时使用 Empty，运行中通信异常时使用类型匹配的零值。
        value: Some(value),
        status: Some(status),
        source_timestamp: None,
        source_picoseconds: None,
        server_timestamp: Some(UaDateTime::now()),
        server_picoseconds: Some(0),
    }
}

pub fn command_pending_timeout(timeout_ms: u64, command_retry_count: u32) -> Duration {
    let attempts = u64::from(command_retry_count.max(1));
    let timeout = timeout_ms.saturating_mul(attempts).saturating_add(2_000);
    Duration::from_millis(timeout.max(5_000))
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
struct PendingCommand {
    ack_id: u64,
    deadline: Instant,
}

#[derive(Clone)]
struct SendNodeWriteContext {
    system_record: Arc<Mutex<BoundedVecDeque<String>>>,
    pending_commands: Arc<Mutex<HashMap<String, PendingCommand>>>,
    local_ack_status_tx: mpsc::Sender<CommandAckUpdate>,
    command_pending_timeout: Duration,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
struct OpcuaVariableCounts {
    device_variables: usize,
    send_variables: usize,
    calc_variables: usize,
}

impl OpcuaVariableCounts {
    fn total(self) -> usize {
        self.device_variables + self.send_variables + self.calc_variables
    }
}

fn count_opcua_variables(
    serial_ports: &[SerialPortConfig],
    calc_engine: &CalcEngine,
) -> OpcuaVariableCounts {
    let device_variables = serial_ports
        .iter()
        .map(|port| {
            port.devices
                .iter()
                .map(|device| device.records.len())
                .sum::<usize>()
        })
        .sum();
    OpcuaVariableCounts {
        device_variables,
        send_variables: serial_ports.len(),
        calc_variables: calc_engine.output_names().len(),
    }
}

fn opcua_limit_target(total_variables: usize) -> usize {
    total_variables.saturating_add(OPCUA_LIMIT_MARGIN).max(1000)
}

fn configure_opcua_limits(builder: &mut ServerBuilder, total_variables: usize) -> usize {
    let limit_target = opcua_limit_target(total_variables);
    let message_size = OPCUA_MIN_MAX_MESSAGE_SIZE.max(limit_target.saturating_mul(2048));

    let limits = builder.limits_mut();
    limits.operational.max_references_per_browse_node = limit_target;
    limits.operational.max_monitored_items_per_call = limit_target;
    limits.subscriptions.max_monitored_items_per_sub = limit_target;
    limits.max_message_size = message_size;
    limits.max_chunk_count = OPCUA_MIN_MAX_CHUNK_COUNT;

    limit_target
}

fn collect_device_updates<'a>(
    status: &DeviceStatus,
    kks_node_map: &'a HashMap<String, NodeId>,
    updates: &mut Vec<OpcuaValueUpdate<'a>>,
    unavailable_status: StatusCode,
) {
    for (kks, val) in &status.value {
        if let Some(node_id) = kks_node_map.get(kks.as_str()) {
            let data_value = if status.com_status && status.device_status {
                DataValue::new_now(value_to_variant(val))
            } else {
                let unavailable_value = if unavailable_status == StatusCode::BadNoCommunication {
                    zero_variant_for_value(val)
                } else {
                    Variant::Empty
                };
                unavailable_data_value(unavailable_value, unavailable_status)
            };
            updates.push((node_id, None, data_value));
        }
    }
}

fn collect_calc_updates<'a>(
    calc_results: Vec<(String, Value)>,
    kks_node_map: &'a HashMap<String, NodeId>,
    updates: &mut Vec<OpcuaValueUpdate<'a>>,
) {
    for (kks_calc, val) in calc_results {
        if let Some(node_id) = kks_node_map.get(&kks_calc) {
            updates.push((node_id, None, DataValue::new_now(value_to_variant(&val))));
        }
    }
}

fn build_incremental_updates<'a>(
    status: &DeviceStatus,
    kks_node_map: &'a HashMap<String, NodeId>,
    calc_runner: &mut CalcRunner,
) -> Vec<OpcuaValueUpdate<'a>> {
    let mut updates = Vec::with_capacity(status.value.len());
    collect_device_updates(
        status,
        kks_node_map,
        &mut updates,
        StatusCode::BadNoCommunication,
    );
    collect_calc_updates(calc_runner.on_update(status), kks_node_map, &mut updates);
    updates
}

fn build_full_sync_updates<'a>(
    statuses: &[DeviceStatus],
    kks_node_map: &'a HashMap<String, NodeId>,
    calc_runner: &mut CalcRunner,
    unavailable_status: StatusCode,
) -> Vec<OpcuaValueUpdate<'a>> {
    if statuses.is_empty() {
        return Vec::new();
    }

    let value_count = statuses.iter().map(|s| s.value.len()).sum();
    let mut updates = Vec::with_capacity(value_count);
    for status in statuses {
        collect_device_updates(status, kks_node_map, &mut updates, unavailable_status);
    }
    collect_calc_updates(
        calc_runner.rebuild_from_statuses(statuses),
        kks_node_map,
        &mut updates,
    );
    updates
}

/// 将广播接收器移动到当前通道尾部，丢弃快照之前已经排队的旧状态。
///
/// 状态生产者始终先更新 [`LatestStatusCache`]、再广播，因此重置接收游标后读取缓存快照
/// 不会漏掉并发更新：快照之前的更新由缓存覆盖，快照之后的更新由新接收器继续消费。
fn discard_pending_broadcasts<T: Clone>(receiver: &mut tokio::sync::broadcast::Receiver<T>) {
    *receiver = receiver.resubscribe();
}

fn build_config_zero_statuses(serial_ports: &[SerialPortConfig]) -> Vec<DeviceStatus> {
    let mut statuses = Vec::new();
    for port in serial_ports {
        for device in &port.devices {
            let zero_data = vec![0u8; device.config.data_len as usize];
            statuses.push(DeviceStatus {
                id: device.config.com_index as u64,
                device_id: device.config.device_id,
                com: port.port_number.clone(),
                com_status: false,
                device_status: false,
                value: crate::protocol::parse_status(&zero_data, &device.records),
                raw_data: zero_data,
            });
        }
    }
    statuses
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
/// - 写命令节点（可写）：`<串口名>_Send`，如 `/dev/ttyAP0` 对应 `ttyAP0_Send`，
///   客户端可通过写入十六进制字符串向设备下发命令。
#[allow(clippy::too_many_arguments)]
pub async fn run_opcua_server(
    serial_ports: Vec<SerialPortConfig>,
    broadcaster: Broadcaster,
    _system_state: SystemState,
    _system_record: Arc<Mutex<BoundedVecDeque<String>>>,
    txs: Vec<mpsc::Sender<Command>>,
    ack_status_rx: mpsc::Receiver<CommandAckUpdate>,
    command_pending_timeout: Duration,
    advertised_host: String,
    calc_engine: Arc<CalcEngine>,
    latest_status_cache: LatestStatusCache,
    cancel: CancellationToken,
) -> io::Result<()> {
    validate_opcua_kks_namespace(&serial_ports, &calc_engine)?;
    let variable_counts = count_opcua_variables(&serial_ports, &calc_engine);

    let port: u16 = OPCUA_BIND_PORT;
    let base = format!("opc.tcp://{}:{}/", advertised_host, port);

    // ---- 构建 OPC UA 服务器 ----
    let mut builder = ServerBuilder::new()
        .host(&advertised_host)
        .port(port)
        .discovery_urls(vec![base.clone()])
        .application_name("Serial OPC UA Server")
        .application_uri("urn:SerialServer:rcd_server")
        .product_uri("urn:SerialServer")
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
        .diagnostics_enabled(true);

    let limit_target = configure_opcua_limits(&mut builder, variable_counts.total());
    info!(
        "[OPC UA] 变量规划: 设备变量={}, 写命令变量={}, 计算变量={}, 总变量={}, 动态限制={}",
        variable_counts.device_variables,
        variable_counts.send_variables,
        variable_counts.calc_variables,
        variable_counts.total(),
        limit_target,
    );

    let (server, handle) = builder.build().map_err(|e| {
        io::Error::new(io::ErrorKind::Other, format!("构建 OPC UA 服务器失败: {e}"))
    })?;
    let listener = TcpListener::bind((OPCUA_BIND_HOST, port))
        .await
        .map_err(|e| {
            io::Error::new(
                io::ErrorKind::AddrNotAvailable,
                format!("OPC UA 监听 {}:{} 失败: {e}", OPCUA_BIND_HOST, port),
            )
        })?;
    info!(
        "[OPC UA] 监听地址={}:{}, 广告地址={}",
        OPCUA_BIND_HOST, port, base
    );

    let node_manager: Arc<SimpleNodeManager> = handle
        .node_managers()
        .get_of_type::<SimpleNodeManager>()
        .expect("获取节点管理器失败");
    let subscriptions: Arc<SubscriptionCache> = handle.subscriptions().clone();
    let ns = handle
        .get_namespace_index("urn:SerialServer")
        .expect("自定义命名空间未注册");

    // 先建立广播接收器，再创建节点。节点创建期间的状态由最新值缓存承接；
    // 节点就绪后会把接收游标切到通道尾部，再用缓存快照完成一致的初始同步。
    let mut rx_opcua = broadcaster.subscribe();

    // ---- 创建 OPC UA 节点（变量 + Method）----
    let opcua_nodes = create_opcua_nodes(&node_manager, ns, &serial_ports, &calc_engine)?;
    info!(
        "[OPC UA] 节点创建完成: 设备变量={}, 写命令变量={}, 计算变量={}, 总变量={}, 成功插入={}, kks_map={}",
        opcua_nodes.counts.device_variables,
        opcua_nodes.counts.send_variables,
        opcua_nodes.counts.calc_variables,
        opcua_nodes.counts.total(),
        opcua_nodes.inserted_variable_count,
        opcua_nodes.kks_node_map.len(),
    );
    let send_node_ids = opcua_nodes.send_node_ids;
    let kks_node_map = opcua_nodes.kks_node_map;
    let pending_commands = Arc::new(Mutex::new(HashMap::<String, PendingCommand>::new()));
    let next_ack_id = Arc::new(AtomicU64::new(1));
    let (local_ack_status_tx, local_ack_status_rx) = mpsc::channel::<CommandAckUpdate>();
    let send_write_context = SendNodeWriteContext {
        system_record: _system_record.clone(),
        pending_commands: pending_commands.clone(),
        local_ack_status_tx,
        command_pending_timeout,
    };
    let tx_by_com: HashMap<String, mpsc::Sender<Command>> = serial_ports
        .iter()
        .zip(txs.iter())
        .map(|(port, tx)| (port.port_number.clone(), tx.clone()))
        .collect();

    // ---- 注册 Method 回调 ----
    register_send_methods(&node_manager, ns, &serial_ports, &txs)?;
    register_send_node_write_callbacks(
        &node_manager,
        &send_node_ids,
        &tx_by_com,
        send_write_context,
        next_ack_id,
    );

    let mut calc_runner = CalcRunner::new(calc_engine.clone());
    // 节点创建期间的状态已经进入最新值缓存。先丢弃这段时间排队的广播，再读取快照，
    // 避免快照应用后继续消费更旧的状态而造成数值回滚。
    discard_pending_broadcasts(&mut rx_opcua);
    let mut initial_statuses = latest_status_cache.snapshot_statuses();
    let initial_statuses_from_config = initial_statuses.is_empty();
    if initial_statuses_from_config {
        initial_statuses = build_config_zero_statuses(&serial_ports);
    }
    let initial_unavailable_status = if initial_statuses_from_config {
        StatusCode::BadWaitingForInitialData
    } else {
        StatusCode::BadNoCommunication
    };
    let initial_updates = build_full_sync_updates(
        &initial_statuses,
        &kks_node_map,
        &mut calc_runner,
        initial_unavailable_status,
    );
    if !initial_updates.is_empty() {
        node_manager
            .set_values(&subscriptions, initial_updates.into_iter())
            .map_err(|e| {
                io::Error::new(io::ErrorKind::Other, format!("OPC UA 缓存预热失败: {e:?}"))
            })?;
        if initial_statuses_from_config {
            info!(
                "[OPC UA] 最新值缓存为空，已用配置零值预热: 设备状态={} 条",
                initial_statuses.len()
            );
        } else {
            info!(
                "[OPC UA] 已用最新值缓存预热: 设备状态={} 条",
                initial_statuses.len()
            );
        }
    } else {
        info!("[OPC UA] 最新值缓存为空，节点保持初始值等待首帧数据");
    }

    // ---- 任务 1: 串口数据 -> OPC UA 节点更新 ----
    let nm_update = node_manager.clone();
    let subs_update = subscriptions.clone();
    let kks_node_map = Arc::new(kks_node_map);
    let kks_map = kks_node_map.clone();
    let latest_cache_update = latest_status_cache.clone();
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
                        // Lagged 会把游标留在通道中仍保留的最旧消息处。先切到尾部，
                        // 再以缓存快照恢复，防止下一轮重新播放快照之前的旧状态。
                        discard_pending_broadcasts(&mut rx_opcua);
                        let statuses = latest_cache_update.snapshot_statuses();
                        let updates = build_full_sync_updates(
                            &statuses,
                            &kks_map,
                            &mut calc_runner,
                            StatusCode::BadNoCommunication,
                        );
                        if !updates.is_empty() {
                            if let Err(e) = nm_update.set_values(&subs_update, updates.into_iter()) {
                                warn!("OPC UA 全量重同步失败: {:?}", e);
                            } else {
                                info!("[OPC UA] 已从最新值缓存完成全量重同步: {} 条设备状态", statuses.len());
                            }
                        }
                        continue;
                    }
                    Err(tokio::sync::broadcast::error::RecvError::Closed) => break,
                }
            };

            let updates = build_incremental_updates(&status, &kks_map, &mut calc_runner);

            // 批量写入所有变量（设备 + 计算），只获取一次写锁。
            if !updates.is_empty() {
                if let Err(e) = nm_update.set_values(&subs_update, updates.into_iter()) {
                    warn!("批量更新 OPC UA 变量失败: {:?}", e);
                }
            }
        }
    });

    // ---- 任务 2: 串口命令 ACK -> OPC UA 写命令节点状态回写 ----
    let nm_ack = node_manager.clone();
    let subs_ack = subscriptions.clone();
    let pending_ack = pending_commands.clone();
    let cancel_ack = cancel.clone();

    let task_ack = tokio::spawn(async move {
        loop {
            tokio::select! {
                biased;
                _ = cancel_ack.cancelled() => break,
                _ = tokio::time::sleep(Duration::from_millis(ACK_STATUS_DRAIN_INTERVAL_MS)) => {}
            }

            drain_command_ack_updates(
                &ack_status_rx,
                &local_ack_status_rx,
                &pending_ack,
                &nm_ack,
                &subs_ack,
                &send_node_ids,
            );
            for update in collect_expired_pending_commands(&pending_ack, Instant::now()) {
                write_send_status(&nm_ack, &subs_ack, &send_node_ids, &update);
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
        .run_with(listener)
        .await
        .map_err(|e| io::Error::new(io::ErrorKind::Other, format!("OPC UA 服务器错误: {e}")));

    // 服务器退出后，确保取消信号触发 + 等待子任务收尾
    cancel.cancel();
    let _ = task_update.await;
    let _ = task_ack.await;

    result
}

/// 初始化后的 OPC UA 节点信息。
struct OpcuaNodes {
    /// 写命令节点：(COM, NodeId)。
    send_node_ids: Vec<(String, NodeId)>,
    /// KKS browse_name → 预构建的 NodeId，避免运行时重复分配。
    kks_node_map: HashMap<String, NodeId>,
    /// 变量数量规划。
    counts: OpcuaVariableCounts,
    /// 成功插入地址空间的变量节点数量。
    inserted_variable_count: usize,
}

/// 校验扁平化 OPC UA 命名空间不会出现 NodeId/BrowseName 冲突。
fn validate_opcua_kks_namespace(
    serial_ports: &[SerialPortConfig],
    calc_engine: &CalcEngine,
) -> io::Result<()> {
    let mut device_kks_owners: HashMap<String, (String, u8)> = HashMap::new();
    let mut duplicate_device_kks: Vec<String> = Vec::new();

    for port in serial_ports {
        for dev in &port.devices {
            for rec in &dev.records {
                let owner = (port.port_number.clone(), dev.config.device_id);
                match device_kks_owners.entry(rec.kks.clone()) {
                    Entry::Vacant(entry) => {
                        entry.insert(owner);
                    }
                    Entry::Occupied(entry) => {
                        let prev = entry.get();
                        duplicate_device_kks.push(format!(
                            "{}: {}:{} conflicts with {}:{}",
                            rec.kks, owner.0, owner.1, prev.0, prev.1
                        ));
                    }
                }
            }
        }
    }

    let mut send_node_owners: HashMap<String, String> = HashMap::new();
    let mut duplicate_send_nodes = Vec::new();
    let mut send_device_collisions = Vec::new();
    for port in serial_ports {
        let send_name = opc_send_node_name(&port.port_number);
        if let Some(previous_port) =
            send_node_owners.insert(send_name.clone(), port.port_number.clone())
        {
            duplicate_send_nodes.push(format!(
                "{}: {} conflicts with {}",
                send_name, port.port_number, previous_port
            ));
        }
        if let Some((device_port, device_id)) = device_kks_owners.get(&send_name) {
            send_device_collisions.push(format!(
                "{}: 写命令节点 {} conflicts with 设备 KKS {}:{}",
                send_name, port.port_number, device_port, device_id
            ));
        }
    }

    let mut infrastructure_node_owners: HashMap<String, String> = HashMap::new();
    let mut duplicate_infrastructure_nodes = Vec::new();
    let mut infrastructure_device_collisions = Vec::new();
    let mut planned_infrastructure_nodes = vec![
        ("Devices".to_string(), "系统 Devices 对象".to_string()),
        ("Methods".to_string(), "系统 Methods 对象".to_string()),
    ];
    for port in serial_ports {
        let method_name = opc_send_method_name(&port.port_number);
        planned_infrastructure_nodes.extend([
            (
                method_name.clone(),
                format!("串口 {} Method", port.port_number),
            ),
            (
                format!("{method_name}_Input"),
                format!("串口 {} Method 输入参数", port.port_number),
            ),
            (
                format!("{method_name}_Output"),
                format!("串口 {} Method 输出参数", port.port_number),
            ),
        ]);
    }
    for (node_name, owner) in planned_infrastructure_nodes {
        if let Some(previous_owner) =
            infrastructure_node_owners.insert(node_name.clone(), owner.clone())
        {
            duplicate_infrastructure_nodes.push(format!(
                "{node_name}: {owner} conflicts with {previous_owner}"
            ));
        }
        if let Some((device_port, device_id)) = device_kks_owners.get(&node_name) {
            infrastructure_device_collisions.push(format!(
                "{node_name}: {owner} conflicts with 设备 KKS {device_port}:{device_id}"
            ));
        }
    }

    let calc_output_names = calc_engine.output_names();
    let calc_collisions: Vec<String> = calc_output_names
        .iter()
        .copied()
        .filter(|name| device_kks_owners.contains_key(*name))
        .map(str::to_string)
        .collect();
    let send_calc_collisions: Vec<String> = send_node_owners
        .keys()
        .filter(|send_name| {
            calc_output_names
                .iter()
                .any(|calc_name| *calc_name == send_name.as_str())
        })
        .cloned()
        .collect();
    let infrastructure_calc_collisions: Vec<String> = infrastructure_node_owners
        .iter()
        .filter(|(node_name, _)| {
            calc_output_names
                .iter()
                .any(|calc_name| *calc_name == node_name.as_str())
        })
        .map(|(node_name, owner)| format!("{node_name}: {owner}"))
        .collect();

    if duplicate_device_kks.is_empty()
        && calc_collisions.is_empty()
        && duplicate_send_nodes.is_empty()
        && send_device_collisions.is_empty()
        && send_calc_collisions.is_empty()
        && duplicate_infrastructure_nodes.is_empty()
        && infrastructure_device_collisions.is_empty()
        && infrastructure_calc_collisions.is_empty()
    {
        return Ok(());
    }

    let mut parts = Vec::new();
    if !duplicate_device_kks.is_empty() {
        parts.push(format!(
            "设备 KKS 重复 {} 个，前 5 条: [{}]",
            duplicate_device_kks.len(),
            duplicate_device_kks
                .iter()
                .take(5)
                .cloned()
                .collect::<Vec<_>>()
                .join("; ")
        ));
    }
    if !calc_collisions.is_empty() {
        parts.push(format!(
            "计算输出与设备 KKS 重名 {} 个，前 20 条: [{}]",
            calc_collisions.len(),
            calc_collisions
                .iter()
                .take(20)
                .cloned()
                .collect::<Vec<_>>()
                .join(", ")
        ));
    }
    if !duplicate_send_nodes.is_empty() {
        parts.push(format!(
            "串口路径归一化后写命令节点重名 {} 个，前 5 条: [{}]",
            duplicate_send_nodes.len(),
            duplicate_send_nodes
                .iter()
                .take(5)
                .cloned()
                .collect::<Vec<_>>()
                .join("; ")
        ));
    }
    if !send_device_collisions.is_empty() {
        parts.push(format!(
            "写命令节点与设备 KKS 重名 {} 个，前 5 条: [{}]",
            send_device_collisions.len(),
            send_device_collisions
                .iter()
                .take(5)
                .cloned()
                .collect::<Vec<_>>()
                .join("; ")
        ));
    }
    if !send_calc_collisions.is_empty() {
        parts.push(format!(
            "写命令节点与计算输出重名 {} 个，前 20 条: [{}]",
            send_calc_collisions.len(),
            send_calc_collisions
                .iter()
                .take(20)
                .cloned()
                .collect::<Vec<_>>()
                .join(", ")
        ));
    }
    if !duplicate_infrastructure_nodes.is_empty() {
        parts.push(format!(
            "OPC UA 系统节点重名 {} 个，前 5 条: [{}]",
            duplicate_infrastructure_nodes.len(),
            duplicate_infrastructure_nodes
                .iter()
                .take(5)
                .cloned()
                .collect::<Vec<_>>()
                .join("; ")
        ));
    }
    if !infrastructure_device_collisions.is_empty() {
        parts.push(format!(
            "OPC UA 系统节点与设备 KKS 重名 {} 个，前 5 条: [{}]",
            infrastructure_device_collisions.len(),
            infrastructure_device_collisions
                .iter()
                .take(5)
                .cloned()
                .collect::<Vec<_>>()
                .join("; ")
        ));
    }
    if !infrastructure_calc_collisions.is_empty() {
        parts.push(format!(
            "OPC UA 系统节点与计算输出重名 {} 个，前 20 条: [{}]",
            infrastructure_calc_collisions.len(),
            infrastructure_calc_collisions
                .iter()
                .take(20)
                .cloned()
                .collect::<Vec<_>>()
                .join("; ")
        ));
    }

    Err(io::Error::new(
        io::ErrorKind::InvalidData,
        format!("OPC UA 扁平化命名空间冲突，拒绝启动: {}", parts.join("; ")),
    ))
}

/// 将系统串口路径转换为稳定的 OPC 节点前缀。
///
/// Linux 的 `/dev/ttyAP0` 和 Windows 的 `\\.\COM3` 分别映射为
/// `ttyAP0`、`COM3`，避免路径分隔符被 OPC 客户端解释为额外层级。
fn opc_port_stem(port_number: &str) -> &str {
    port_number
        .rsplit(['/', '\\'])
        .find(|segment| !segment.is_empty())
        .unwrap_or(port_number)
}

fn opc_send_node_name(port_number: &str) -> String {
    format!("{}_Send", opc_port_stem(port_number))
}

fn opc_send_method_name(port_number: &str) -> String {
    format!("{}_SendCommand", opc_port_stem(port_number))
}

/// 计算所有 OPC UA 节点的 browse_name（纯逻辑，便于单元测试）。
///
/// 返回 `(data_browse_names, send_pairs)`：
/// - `data_browse_names`：所有只读节点（设备数据 + 计算衍生），均为纯 KKS 名。
/// - `send_pairs`：每个串口的写命令节点 `(port_number, "<port_stem>_Send")`。
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
            opc_send_node_name(&port.port_number),
        ));
    }
    for (kks_calc, _) in calc_engine.output_definitions() {
        data_names.push(kks_calc.to_string());
    }
    (data_names, send_pairs)
}

/// 为每个数据变量补齐 OPC UA 标准要求的类型定义引用。
fn add_base_data_variable_type_references(
    address_space: &mut AddressSpace,
    variable_node_ids: &[NodeId],
) -> io::Result<()> {
    let base_data_variable_type = NodeId::from(VariableTypeId::BaseDataVariableType);
    for node_id in variable_node_ids {
        address_space.insert_reference(
            node_id,
            &base_data_variable_type,
            ReferenceTypeId::HasTypeDefinition,
        );
        if !address_space.has_reference(
            node_id,
            &base_data_variable_type,
            ReferenceTypeId::HasTypeDefinition,
        ) {
            return Err(io::Error::new(
                io::ErrorKind::InvalidData,
                format!("OPC UA 变量缺少 BaseDataVariableType 类型定义: {node_id}"),
            ));
        }
    }
    Ok(())
}

/// 将一组 DataVariable 作为父对象的组成部分插入地址空间。
///
/// `AddressSpace::add_variables` 会使用 `Organizes`，但 OPC UA DataVariable 应至少通过一条
/// `HasComponent` 引用归属于 Object、ObjectType、DataVariable 或 VariableType。
fn add_data_variables(
    address_space: &mut AddressSpace,
    variables: Vec<Variable>,
    parent_node_id: &NodeId,
) -> Vec<bool> {
    variables
        .into_iter()
        .map(|variable| {
            address_space.insert(
                variable,
                Some(&[(
                    parent_node_id,
                    &ReferenceTypeId::HasComponent,
                    ReferenceDirection::Inverse,
                )]),
            )
        })
        .collect()
}

/// 在地址空间中创建所有设备变量节点、写命令节点和计算引擎衍生变量节点。
fn create_opcua_nodes(
    node_manager: &Arc<SimpleNodeManager>,
    ns: u16,
    serial_ports: &[SerialPortConfig],
    calc_engine: &CalcEngine,
) -> io::Result<OpcuaNodes> {
    let devices_folder_id = NodeId::new(ns, "Devices");
    let mut send_node_ids: Vec<(String, NodeId)> = Vec::new();
    let mut kks_node_map = HashMap::new();
    let counts = count_opcua_variables(serial_ports, calc_engine);

    let mut address_space = node_manager.address_space().write();
    if !address_space.add_folder(
        &devices_folder_id,
        QualifiedName::new(ns, "Devices"),
        "Devices",
        &NodeId::objects_folder_id(),
    ) {
        return Err(io::Error::new(
            io::ErrorKind::AlreadyExists,
            "创建 OPC UA Devices 文件夹失败",
        ));
    }

    let mut variables = Vec::new();
    let mut variable_names = Vec::with_capacity(counts.total());
    let mut variable_node_ids = Vec::with_capacity(counts.total());

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
                    QualifiedName::new(ns, browse_name.clone()),
                    &browse_name,
                    dt,
                    Some(-1),
                    None,
                    init,
                ));
                variable_names.push(browse_name.clone());
                variable_node_ids.push(node_id.clone());
                kks_node_map.insert(browse_name, node_id);
            }
        }

        // 写命令节点（可写字符串）
        let send_name = opc_send_node_name(&port.port_number);
        let send_node_id = NodeId::new(ns, send_name.clone());
        variables.push(Variable::new_data_value(
            &send_node_id,
            QualifiedName::new(ns, send_name.clone()),
            &send_name,
            NodeId::from(DataTypeId::String),
            Some(-1),
            None,
            Variant::String(UAString::from(SEND_NODE_IDLE_VALUE)),
        ));
        variable_names.push(send_name.clone());
        variable_node_ids.push(send_node_id.clone());
        send_node_ids.push((port.port_number.clone(), send_node_id));
    }

    // ---- 计算引擎衍生变量节点 ----
    for (kks_calc, data_type) in calc_engine.output_definitions() {
        let node_id = NodeId::new(ns, kks_calc.to_string());
        let expected = expected_type_from_str(data_type);
        let (dt, init) = datatype_and_initial(expected);
        variables.push(Variable::new_data_value(
            &node_id,
            QualifiedName::new(ns, kks_calc),
            kks_calc,
            dt,
            Some(-1),
            None,
            init,
        ));
        variable_names.push(kks_calc.to_string());
        variable_node_ids.push(node_id.clone());
        kks_node_map.insert(kks_calc.to_string(), node_id);
    }

    let inserted_results = if variables.is_empty() {
        Vec::new()
    } else {
        add_data_variables(&mut address_space, variables, &devices_folder_id)
    };
    let inserted_variable_count = inserted_results.iter().filter(|ok| **ok).count();

    if inserted_variable_count != counts.total() || inserted_results.len() != variable_names.len() {
        let failed_names: Vec<String> = variable_names
            .iter()
            .enumerate()
            .filter(|(idx, _)| !inserted_results.get(*idx).copied().unwrap_or(false))
            .map(|(_, name)| name.clone())
            .take(20)
            .collect();
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            format!(
                "OPC UA 变量节点创建不完整: 预期 {}, 成功 {}, 前 20 个失败节点: [{}]",
                counts.total(),
                inserted_variable_count,
                failed_names.join(", ")
            ),
        ));
    }

    add_base_data_variable_type_references(&mut address_space, &variable_node_ids)?;

    // 在同一次写锁内设置写命令节点的写权限
    for (_, node_id) in &send_node_ids {
        if let Some(NodeType::Variable(var)) = address_space.find_mut(node_id) {
            var.set_access_level(AccessLevel::CURRENT_READ | AccessLevel::CURRENT_WRITE);
            var.set_user_access_level(AccessLevel::CURRENT_READ | AccessLevel::CURRENT_WRITE);
        } else {
            return Err(io::Error::new(
                io::ErrorKind::InvalidData,
                format!("未找到写命令节点: {:?}", node_id),
            ));
        }
    }

    Ok(OpcuaNodes {
        send_node_ids,
        kks_node_map,
        counts,
        inserted_variable_count,
    })
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
) -> io::Result<()> {
    if serial_ports.len() != txs.len() {
        return Err(io::Error::new(
            io::ErrorKind::InvalidInput,
            format!(
                "OPC UA Method 注册参数不一致: 串口数={}, 发送队列数={}",
                serial_ports.len(),
                txs.len()
            ),
        ));
    }

    // 创建 Methods 父对象文件夹
    let methods_folder_id = NodeId::new(ns, "Methods");
    {
        let mut address_space = node_manager.address_space().write();
        let inserted = ObjectBuilder::new(
            &methods_folder_id,
            QualifiedName::new(ns, "Methods"),
            "Methods",
        )
        .event_notifier(EventNotifier::empty())
        .has_type_definition(ObjectTypeId::BaseObjectType)
        .organized_by(ObjectId::ObjectsFolder)
        .insert(&mut *address_space);
        if !inserted {
            return Err(io::Error::new(
                io::ErrorKind::AlreadyExists,
                "创建 OPC UA Methods 对象失败",
            ));
        }
    }

    for (idx, port) in serial_ports.iter().enumerate() {
        let method_name = opc_send_method_name(&port.port_number);
        let fn_node_id = NodeId::new(ns, method_name.clone());
        let input_id = NodeId::new(ns, format!("{}_Input", method_name));
        let output_id = NodeId::new(ns, format!("{}_Output", method_name));

        {
            let mut address_space = node_manager.address_space().write();
            let inserted = MethodBuilder::new(
                &fn_node_id,
                QualifiedName::new(ns, method_name.clone()),
                &method_name,
            )
            .executable(true)
            .user_executable(true)
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
            if !inserted {
                return Err(io::Error::new(
                    io::ErrorKind::AlreadyExists,
                    format!("创建 OPC UA Method 失败: {method_name}"),
                ));
            }
            for (argument_id, argument_name) in [
                (&input_id, "InputArguments"),
                (&output_id, "OutputArguments"),
            ] {
                if !matches!(
                    address_space.find_node(argument_id),
                    Some(NodeType::Variable(_))
                ) {
                    return Err(io::Error::new(
                        io::ErrorKind::InvalidData,
                        format!("OPC UA Method {method_name} 缺少 {argument_name}"),
                    ));
                }
            }
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

                match forward_hex_command(hex_str, &com, &tx, false) {
                    Ok(msg) => Ok(vec![Variant::String(UAString::from(msg))]),
                    Err(msg) => {
                        warn!("Method 调用失败: {}", msg);
                        Ok(vec![Variant::String(UAString::from(msg))])
                    }
                }
            });
    }

    Ok(())
}

fn register_send_node_write_callbacks(
    node_manager: &Arc<SimpleNodeManager>,
    send_node_ids: &[(String, NodeId)],
    tx_by_com: &HashMap<String, mpsc::Sender<Command>>,
    context: SendNodeWriteContext,
    next_ack_id: Arc<AtomicU64>,
) {
    for (com, node_id) in send_node_ids {
        let Some(tx) = tx_by_com.get(com).cloned() else {
            warn!("写命令节点回调注册失败，未找到串口 {} 的命令通道", com);
            continue;
        };
        let com = com.clone();
        let callback_node_id = node_id.clone();
        let context = context.clone();
        let next_ack_id = next_ack_id.clone();

        log_opc_write_info(format!(
            "[OPC 写诊断] 回调已注册: 串口={}, NodeId={}, 属性=Value, 类型=String, 权限=CurrentRead|CurrentWrite",
            com, callback_node_id
        ));

        node_manager
            .inner()
            .add_write_callback(node_id.clone(), move |data_value, range| {
                let ack_id = next_ack_id.fetch_add(1, Ordering::Relaxed);
                let (value_type, value_summary) =
                    opc_write_value_diagnostic(data_value.value.as_ref());
                log_opc_write_info(format!(
                    "[OPC 写诊断] 回调触发: 串口={}, NodeId={}, 属性=Value, IndexRange={:?}, Variant类型={}, DataValue状态={:?}, 值摘要={}",
                    com,
                    callback_node_id,
                    range,
                    value_type,
                    data_value.status,
                    value_summary,
                ));

                let status = handle_send_node_write(data_value, &com, &tx, &context, ack_id);
                log_opc_write_info(format!(
                    "[OPC 写诊断] 处理完成: 串口={}, NodeId={}, ack_id={}, 返回状态={:?}",
                    com, callback_node_id, ack_id, status
                ));
                status
            });
    }
}

fn handle_send_node_write(
    data_value: DataValue,
    com: &str,
    tx: &mpsc::Sender<Command>,
    context: &SendNodeWriteContext,
    ack_id: u64,
) -> StatusCode {
    let Some(value) = data_value.value else {
        log_opc_write_warn(format!(
            "[OPC 写诊断] 拒绝写入: 串口={}, 原因=未携带 Value",
            com
        ));
        return StatusCode::BadTypeMismatch;
    };
    let actual_type = format!("{:?}", value.type_id());
    let Variant::String(hex_ua) = value else {
        log_opc_write_warn(format!(
            "[OPC 写诊断] 拒绝写入: 串口={}, 期望类型=String, 实际类型={}",
            com, actual_type
        ));
        return StatusCode::BadTypeMismatch;
    };
    let hex_str = hex_ua.as_ref().trim();
    if is_send_node_idle_or_status(hex_str) {
        log_opc_write_info(format!(
            "[OPC 写诊断] 忽略写入: 串口={}, 值={:?}, 原因=空闲值或内部状态值(IDLE/0/1/2/3)",
            com, hex_str
        ));
        return StatusCode::Good;
    }

    let (preview, truncated) = opc_write_log_preview(hex_str);
    let suffix = if truncated { "…(已截断)" } else { "" };
    let cmd = match build_hex_command(hex_str, com, true, Some(ack_id)) {
        Ok(cmd) => cmd,
        Err(e) => {
            log_opc_write_warn(format!(
                "[OPC 写诊断] 拒绝写入: 串口={}, 数据长度={}, 数据预览={}{}，原因={}",
                com,
                hex_str.len(),
                preview,
                suffix,
                e
            ));
            return StatusCode::BadInvalidArgument;
        }
    };

    let mut pending = context.pending_commands.lock().unwrap_or_else(|e| {
        warn!("命令 pending 状态锁中毒，恢复: {}", e);
        e.into_inner()
    });
    if pending.contains_key(com) {
        log_opc_write_warn(format!(
            "[OPC 写诊断] 拒绝写入: 串口={}, 原因=上一条命令仍在等待设备回复",
            com
        ));
        return StatusCode::BadWaitingForResponse;
    }

    if let Err(e) = tx.send(cmd) {
        log_opc_write_warn(format!(
            "[OPC 写诊断] 拒绝写入: 串口={}, 原因=命令通道不可用, 错误={:?}",
            com, e
        ));
        return StatusCode::BadResourceUnavailable;
    }
    pending.insert(
        com.to_string(),
        PendingCommand {
            ack_id,
            deadline: Instant::now() + context.command_pending_timeout,
        },
    );
    drop(pending);

    log_opc_write_info(format!(
        "收到 OPC 客户端写值并已入队: 串口={} ack_id={} 数据长度={} 数据预览={}{}",
        com,
        ack_id,
        hex_str.len(),
        preview,
        suffix,
    ));

    if let Err(e) = context.local_ack_status_tx.send(CommandAckUpdate {
        com: com.to_string(),
        ack_id: Some(ack_id),
        status: CommandAckStatus::Waiting,
    }) {
        warn!("本地 ACK 等待状态入队失败: 串口={}, 错误={:?}", com, e);
    }

    let local = chrono::Local::now();
    let mut record = context.system_record.lock().unwrap_or_else(|e| {
        warn!("系统操作记录锁中毒，恢复: {}", e);
        e.into_inner()
    });
    record.push_back(format!(
        "时间: {} 串口: {} 数据: {}",
        local.format("%Y-%m-%d %H:%M:%S"),
        com,
        hex_str,
    ));

    StatusCode::Good
}

fn log_opc_write_info(message: String) {
    println!("{message}");
    info!("{message}");
}

fn log_opc_write_warn(message: String) {
    println!("{message}");
    warn!("{message}");
}

fn opc_write_log_preview(value: &str) -> (String, bool) {
    let mut chars = value.chars();
    let preview: String = chars.by_ref().take(OPC_WRITE_LOG_PREVIEW_CHARS).collect();
    (preview, chars.next().is_some())
}

fn opc_write_value_diagnostic(value: Option<&Variant>) -> (String, String) {
    let Some(value) = value else {
        return ("None".to_string(), "<未携带值>".to_string());
    };

    let value_type = format!("{:?}", value.type_id());
    let summary = match value {
        Variant::Empty => "<Empty>".to_string(),
        Variant::Boolean(value) => value.to_string(),
        Variant::SByte(value) => value.to_string(),
        Variant::Byte(value) => value.to_string(),
        Variant::Int16(value) => value.to_string(),
        Variant::UInt16(value) => value.to_string(),
        Variant::Int32(value) => value.to_string(),
        Variant::UInt32(value) => value.to_string(),
        Variant::Int64(value) => value.to_string(),
        Variant::UInt64(value) => value.to_string(),
        Variant::Float(value) => value.to_string(),
        Variant::Double(value) => value.to_string(),
        Variant::String(value) => {
            let raw = value.as_ref();
            let (preview, truncated) = opc_write_log_preview(raw);
            format!(
                "长度={}, 内容={}{}",
                raw.len(),
                preview,
                if truncated { "…(已截断)" } else { "" }
            )
        }
        Variant::Array(value) => format!(
            "数组长度={}, 维度={:?}",
            value.values.len(),
            value.dimensions
        ),
        _ => "<复杂值已省略>".to_string(),
    };

    (value_type, summary)
}

fn build_hex_command(
    hex_str: &str,
    com: &str,
    with_ack: bool,
    ack_id: Option<u64>,
) -> Result<Command, String> {
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
    Ok(Command {
        com: com.to_string(),
        command: if with_ack {
            CommandType::SendDataWithAck(send_data)
        } else {
            CommandType::SendData(send_data)
        },
        ack_id: if with_ack { ack_id } else { None },
    })
}

/// 解析十六进制字符串并转发到串口线程（同步，供 Method 回调共用）。
fn forward_hex_command(
    hex_str: &str,
    com: &str,
    tx: &mpsc::Sender<Command>,
    with_ack: bool,
) -> Result<String, String> {
    let cmd = build_hex_command(hex_str, com, with_ack, None)?;
    let device_id = match &cmd.command {
        CommandType::SendData(data) | CommandType::SendDataWithAck(data) => data.device_id,
        CommandType::PortStatus(_) | CommandType::DeviceSetting(_) => 0,
    };
    tx.send(cmd)
        .map_err(|e| format!("转发命令到串口失败: {:?}", e))?;
    Ok(format!("已发送到 {} 设备 {}", com, device_id))
}

fn is_send_node_idle_or_status(value: &str) -> bool {
    matches!(value, "" | SEND_NODE_IDLE_VALUE | "0" | "1" | "2" | "3")
}

fn drain_command_ack_updates(
    ack_status_rx: &mpsc::Receiver<CommandAckUpdate>,
    local_ack_status_rx: &mpsc::Receiver<CommandAckUpdate>,
    pending_commands: &Arc<Mutex<HashMap<String, PendingCommand>>>,
    node_manager: &Arc<SimpleNodeManager>,
    subscriptions: &Arc<SubscriptionCache>,
    send_node_ids: &[(String, NodeId)],
) {
    while let Ok(update) = ack_status_rx.try_recv() {
        process_command_ack_update(
            update,
            pending_commands,
            node_manager,
            subscriptions,
            send_node_ids,
        );
    }
    while let Ok(update) = local_ack_status_rx.try_recv() {
        process_command_ack_update(
            update,
            pending_commands,
            node_manager,
            subscriptions,
            send_node_ids,
        );
    }
}

fn process_command_ack_update(
    update: CommandAckUpdate,
    pending_commands: &Arc<Mutex<HashMap<String, PendingCommand>>>,
    node_manager: &Arc<SimpleNodeManager>,
    subscriptions: &Arc<SubscriptionCache>,
    send_node_ids: &[(String, NodeId)],
) {
    let should_write = {
        let mut pending = pending_commands.lock().unwrap_or_else(|e| {
            warn!("命令 pending 状态锁中毒，恢复: {}", e);
            e.into_inner()
        });
        apply_ack_pending_state(&mut pending, &update)
    };

    if should_write {
        write_send_status(node_manager, subscriptions, send_node_ids, &update);
    }
}

fn apply_ack_pending_state(
    pending_commands: &mut HashMap<String, PendingCommand>,
    update: &CommandAckUpdate,
) -> bool {
    let Some(update_ack_id) = update.ack_id else {
        return false;
    };
    let Some(pending) = pending_commands.get(&update.com) else {
        return false;
    };
    if pending.ack_id != update_ack_id {
        return false;
    }

    if update.status.is_terminal() {
        pending_commands.remove(&update.com);
    }
    true
}

fn collect_expired_pending_commands(
    pending_commands: &Arc<Mutex<HashMap<String, PendingCommand>>>,
    now: Instant,
) -> Vec<CommandAckUpdate> {
    let mut pending = pending_commands.lock().unwrap_or_else(|e| {
        warn!("命令 pending 状态锁中毒，恢复: {}", e);
        e.into_inner()
    });
    let expired: Vec<(String, u64)> = pending
        .iter()
        .filter(|(_, command)| command.deadline <= now)
        .map(|(com, command)| (com.clone(), command.ack_id))
        .collect();

    for (com, _) in &expired {
        pending.remove(com);
    }

    expired
        .into_iter()
        .map(|(com, ack_id)| CommandAckUpdate {
            com,
            ack_id: Some(ack_id),
            status: CommandAckStatus::Failed,
        })
        .collect()
}

fn send_status_variant(status: crate::model::CommandAckStatus) -> Variant {
    Variant::String(UAString::from(status.as_opc_value()))
}

fn send_status_data_value(status: crate::model::CommandAckStatus) -> DataValue {
    DataValue::new_now(send_status_variant(status))
}

fn send_status_value(status: crate::model::CommandAckStatus) -> &'static str {
    status.as_opc_value()
}

#[cfg(test)]
fn data_value_string(value: &str) -> DataValue {
    DataValue::new_now(Variant::String(UAString::from(value)))
}

fn write_send_status(
    node_manager: &Arc<SimpleNodeManager>,
    subscriptions: &Arc<SubscriptionCache>,
    send_node_ids: &[(String, NodeId)],
    update: &CommandAckUpdate,
) {
    let Some((_, node_id)) = send_node_ids.iter().find(|(com, _)| com == &update.com) else {
        warn!("命令 ACK 状态回写失败，未找到串口节点: {}", update.com);
        return;
    };

    let status_value = send_status_value(update.status);
    let data_value = send_status_data_value(update.status);
    let iter = vec![(node_id, None, data_value)].into_iter();
    if let Err(e) = node_manager.set_values(subscriptions, iter) {
        warn!(
            "命令 ACK 状态回写失败: 串口={}, 状态={}, 错误={:?}",
            update.com, status_value, e
        );
    }
}

// ============================================================
//  单元测试
// ============================================================

#[cfg(test)]
mod tests {
    use super::*;
    use crate::calc_engine::config::CalcRule;
    use crate::csv_parser::{BitIndex, Config, DeviceConfig, Record};
    use crate::model::CommandAckStatus;
    use opcua::server::address_space::NodeBase;
    use opcua::types::{DataEncoding, TimestampsToReturn};

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

    #[tokio::test]
    async fn discarding_pending_broadcasts_skips_startup_backlog() {
        let (tx, mut rx) = tokio::sync::broadcast::channel(4);
        tx.send(1u32).expect("发送旧状态");
        tx.send(2u32).expect("发送旧状态");

        discard_pending_broadcasts(&mut rx);

        assert!(matches!(
            rx.try_recv(),
            Err(tokio::sync::broadcast::error::TryRecvError::Empty)
        ));
        tx.send(3u32).expect("发送新状态");
        assert_eq!(rx.recv().await.expect("接收新状态"), 3);
    }

    #[tokio::test]
    async fn discarding_pending_broadcasts_skips_retained_items_after_lag() {
        let (tx, mut rx) = tokio::sync::broadcast::channel(2);
        tx.send(1u32).expect("发送旧状态");
        tx.send(2u32).expect("发送旧状态");
        tx.send(3u32).expect("制造接收器落后");

        assert!(matches!(
            rx.try_recv(),
            Err(tokio::sync::broadcast::error::TryRecvError::Lagged(_))
        ));
        discard_pending_broadcasts(&mut rx);

        assert!(matches!(
            rx.try_recv(),
            Err(tokio::sync::broadcast::error::TryRecvError::Empty)
        ));
        tx.send(4u32).expect("发送快照后的新状态");
        assert_eq!(rx.recv().await.expect("接收快照后的新状态"), 4);
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
    fn send_node_uses_port_stem_without_os_path() {
        let port = make_port("/dev/ttyAP0", vec![make_device(11, &["X"])]);
        let engine = make_calc_engine(vec![]);
        let (_, send_pairs) = plan_browse_names(&[port], &engine);
        assert_eq!(send_pairs.len(), 1);
        assert_eq!(send_pairs[0].0, "/dev/ttyAP0");
        assert_eq!(send_pairs[0].1, "ttyAP0_Send");
    }

    #[test]
    fn opc_port_names_normalize_linux_and_windows_paths() {
        assert_eq!(opc_port_stem("/dev/ttyAP0"), "ttyAP0");
        assert_eq!(opc_port_stem(r"\\.\COM3"), "COM3");
        assert_eq!(opc_port_stem("COM4"), "COM4");
        assert_eq!(opc_send_node_name("/dev/ttyAP0"), "ttyAP0_Send");
        assert_eq!(opc_send_method_name("/dev/ttyAP0"), "ttyAP0_SendCommand");
    }

    #[test]
    fn string_variable_has_data_type_and_base_variable_type_reference() {
        let mut address_space = AddressSpace::new();
        address_space.add_namespace("http://opcfoundation.org/UA/", 0);
        address_space.add_namespace("urn:test", 1);
        let devices_id = NodeId::new(1, "Devices");
        assert!(address_space.add_folder(
            &devices_id,
            QualifiedName::new(1, "Devices"),
            "Devices",
            &NodeId::objects_folder_id(),
        ));
        let node_id = NodeId::new(1, "ttyAP0_Send");
        let variable = Variable::new_data_value(
            &node_id,
            QualifiedName::new(1, "ttyAP0_Send"),
            "ttyAP0_Send",
            NodeId::from(DataTypeId::String),
            Some(-1),
            None,
            Variant::String(UAString::from(SEND_NODE_IDLE_VALUE)),
        );
        assert_eq!(
            add_data_variables(&mut address_space, vec![variable], &devices_id),
            vec![true]
        );

        add_base_data_variable_type_references(&mut address_space, std::slice::from_ref(&node_id))
            .expect("类型定义引用应添加成功");

        let Some(NodeType::Variable(variable)) = address_space.find_node(&node_id) else {
            panic!("应创建字符串 Variable");
        };
        assert_eq!(
            variable.browse_name(),
            &QualifiedName::new(1, "ttyAP0_Send")
        );
        assert_eq!(variable.data_type(), NodeId::from(DataTypeId::String));
        assert_eq!(variable.value_rank(), -1);
        assert_eq!(
            variable
                .value(
                    TimestampsToReturn::Neither,
                    &NumericRange::None,
                    &DataEncoding::Binary,
                    0.0,
                )
                .value,
            Some(Variant::String(UAString::from(SEND_NODE_IDLE_VALUE)))
        );
        assert!(address_space.has_reference(
            &node_id,
            &NodeId::from(VariableTypeId::BaseDataVariableType),
            ReferenceTypeId::HasTypeDefinition,
        ));
        assert!(address_space.has_reference(&devices_id, &node_id, ReferenceTypeId::HasComponent,));
        assert!(!address_space.has_reference(&devices_id, &node_id, ReferenceTypeId::Organizes,));
    }

    #[test]
    fn all_custom_data_variables_are_components_of_devices() {
        let mut address_space = AddressSpace::new();
        address_space.add_namespace("http://opcfoundation.org/UA/", 0);
        address_space.add_namespace("urn:test", 1);

        let devices_id = NodeId::new(1, "Devices");
        assert!(address_space.add_folder(
            &devices_id,
            QualifiedName::new(1, "Devices"),
            "Devices",
            &NodeId::objects_folder_id(),
        ));

        let definitions = [
            ("DEVICE_KKS", DataTypeId::UInt32, Variant::UInt32(0)),
            (
                "ttyAP0_Send",
                DataTypeId::String,
                Variant::String(UAString::from(SEND_NODE_IDLE_VALUE)),
            ),
            ("CALC_RESULT", DataTypeId::Float, Variant::Float(0.0)),
        ];
        let node_ids: Vec<NodeId> = definitions
            .iter()
            .map(|(name, _, _)| NodeId::new(1, *name))
            .collect();
        let variables: Vec<Variable> = definitions
            .into_iter()
            .zip(&node_ids)
            .map(|((name, data_type, initial_value), node_id)| {
                Variable::new_data_value(
                    node_id,
                    QualifiedName::new(1, name),
                    name,
                    NodeId::from(data_type),
                    Some(-1),
                    None,
                    initial_value,
                )
            })
            .collect();

        assert_eq!(
            add_data_variables(&mut address_space, variables, &devices_id),
            vec![true; node_ids.len()]
        );
        for node_id in node_ids {
            assert!(address_space.has_reference(
                &devices_id,
                &node_id,
                ReferenceTypeId::HasComponent,
            ));
            assert!(!address_space.has_reference(
                &devices_id,
                &node_id,
                ReferenceTypeId::Organizes,
            ));
        }
    }

    #[test]
    fn custom_browse_names_use_application_namespace_and_method_arguments_use_zero() {
        let mut address_space = AddressSpace::new();
        address_space.add_namespace("http://opcfoundation.org/UA/", 0);
        address_space.add_namespace("urn:test", 1);

        let folder_id = NodeId::new(1, "Devices");
        assert!(
            ObjectBuilder::new(&folder_id, QualifiedName::new(1, "Devices"), "Devices",)
                .has_type_definition(ObjectTypeId::BaseObjectType)
                .insert(&mut address_space)
        );

        let method_id = NodeId::new(1, "ttyAP0_SendCommand");
        let input_id = NodeId::new(1, "ttyAP0_SendCommand_Input");
        let output_id = NodeId::new(1, "ttyAP0_SendCommand_Output");
        assert!(MethodBuilder::new(
            &method_id,
            QualifiedName::new(1, "ttyAP0_SendCommand"),
            "ttyAP0_SendCommand",
        )
        .executable(true)
        .user_executable(true)
        .input_args(
            &mut address_space,
            &input_id,
            &[("HexData", DataTypeId::String).into()],
        )
        .output_args(
            &mut address_space,
            &output_id,
            &[("Result", DataTypeId::String).into()],
        )
        .insert(&mut address_space));

        let Some(NodeType::Object(folder)) = address_space.find_node(&folder_id) else {
            panic!("应创建自定义文件夹");
        };
        assert_eq!(folder.browse_name(), &QualifiedName::new(1, "Devices"));
        assert!(address_space.has_reference(
            &folder_id,
            &NodeId::from(ObjectTypeId::BaseObjectType),
            ReferenceTypeId::HasTypeDefinition,
        ));

        let Some(NodeType::Method(method)) = address_space.find_node(&method_id) else {
            panic!("应创建自定义 Method");
        };
        assert_eq!(
            method.browse_name(),
            &QualifiedName::new(1, "ttyAP0_SendCommand")
        );
        assert!(method.executable());
        assert!(method.user_executable());

        for (node_id, browse_name) in [
            (&input_id, "InputArguments"),
            (&output_id, "OutputArguments"),
        ] {
            let Some(NodeType::Variable(argument)) = address_space.find_node(node_id) else {
                panic!("应创建 {browse_name} Variable");
            };
            assert_eq!(
                argument.browse_name(),
                &QualifiedName::new(0, browse_name),
                "标准 Method 参数 BrowseName 必须保留在命名空间 0"
            );
        }
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

    #[test]
    fn namespace_validation_accepts_unique_device_and_calc_kks() {
        let port = make_port("ttyAP0", vec![make_device(11, &["base"])]);
        let engine = make_calc_engine(vec![("derived", "uint", "base + 1")]);

        assert!(validate_opcua_kks_namespace(&[port], &engine).is_ok());
    }

    #[test]
    fn namespace_validation_rejects_duplicate_device_kks() {
        let port = make_port(
            "ttyAP0",
            vec![
                make_device(11, &["9CYE91GH201_READY"]),
                make_device(12, &["9CYE91GH201_READY"]),
            ],
        );
        let engine = make_calc_engine(vec![]);

        let err = validate_opcua_kks_namespace(&[port], &engine).unwrap_err();
        assert_eq!(err.kind(), io::ErrorKind::InvalidData);
        assert!(err.to_string().contains("设备 KKS 重复"));
        assert!(err.to_string().contains("9CYE91GH201_READY"));
    }

    #[test]
    fn namespace_validation_rejects_calc_output_device_collision() {
        let port = make_port("ttyAP0", vec![make_device(11, &["base"])]);
        let engine = make_calc_engine(vec![("base", "uint", "other + 1")]);

        let err = validate_opcua_kks_namespace(&[port], &engine).unwrap_err();
        assert_eq!(err.kind(), io::ErrorKind::InvalidData);
        assert!(err.to_string().contains("计算输出与设备 KKS 重名"));
        assert!(err.to_string().contains("base"));
    }

    #[test]
    fn namespace_validation_rejects_normalized_send_node_collision() {
        let linux_path = make_port("/dev/ttyAP0", vec![make_device(11, &["a"])]);
        let bare_name = make_port("ttyAP0", vec![make_device(12, &["b"])]);
        let engine = make_calc_engine(vec![]);

        let err = validate_opcua_kks_namespace(&[linux_path, bare_name], &engine).unwrap_err();
        assert_eq!(err.kind(), io::ErrorKind::InvalidData);
        assert!(err.to_string().contains("路径归一化后写命令节点重名"));
        assert!(err.to_string().contains("ttyAP0_Send"));
    }

    #[test]
    fn namespace_validation_rejects_system_node_device_collision() {
        let port = make_port("ttyAP0", vec![make_device(11, &["Methods"])]);
        let engine = make_calc_engine(vec![]);

        let err = validate_opcua_kks_namespace(&[port], &engine).unwrap_err();
        assert_eq!(err.kind(), io::ErrorKind::InvalidData);
        assert!(err.to_string().contains("系统节点与设备 KKS 重名"));
        assert!(err.to_string().contains("Methods"));
    }

    #[test]
    fn namespace_validation_rejects_method_argument_calc_collision() {
        let port = make_port("ttyAP0", vec![make_device(11, &["base"])]);
        let engine = make_calc_engine(vec![("ttyAP0_SendCommand_Input", "uint", "base + 1")]);

        let err = validate_opcua_kks_namespace(&[port], &engine).unwrap_err();
        assert_eq!(err.kind(), io::ErrorKind::InvalidData);
        assert!(err.to_string().contains("系统节点与计算输出重名"));
        assert!(err.to_string().contains("ttyAP0_SendCommand_Input"));
    }

    #[test]
    fn variable_count_includes_device_send_and_calc_nodes() {
        let p0 = make_port("ttyAP0", vec![make_device(11, &["a", "b"])]);
        let p1 = make_port("ttyAP1", vec![make_device(12, &["c"])]);
        let engine = make_calc_engine(vec![("calc1", "uint", "a + b"), ("calc2", "uint", "c")]);

        let counts = count_opcua_variables(&[p0, p1], &engine);
        assert_eq!(
            counts,
            OpcuaVariableCounts {
                device_variables: 3,
                send_variables: 2,
                calc_variables: 2,
            }
        );
        assert_eq!(counts.total(), 7);
    }

    #[test]
    fn opcua_limit_target_covers_large_flat_namespace() {
        assert_eq!(opcua_limit_target(10), 1000);
        assert_eq!(opcua_limit_target(2099), 2099 + OPCUA_LIMIT_MARGIN);
    }

    #[test]
    fn large_calc_plan_keeps_flat_browse_names_and_counts_all_outputs() {
        let port = make_port("ttyAP0", vec![make_device(11, &["base"])]);
        let rules: Vec<CalcRule> = (0..2099)
            .map(|idx| CalcRule {
                kks_calc: format!("calc_{idx:04}"),
                data_type: "uint".to_string(),
                expression: "base + 1".to_string(),
                max_limit: f64::MAX,
                min_limit: f64::MIN,
                default_value: 0.0,
            })
            .collect();
        let engine = CalcEngine::new(rules).expect("构建大规模测试引擎失败");

        let (data_names, send_pairs) = plan_browse_names(std::slice::from_ref(&port), &engine);
        let counts = count_opcua_variables(&[port], &engine);

        assert_eq!(send_pairs, vec![("ttyAP0".into(), "ttyAP0_Send".into())]);
        assert_eq!(counts.device_variables, 1);
        assert_eq!(counts.send_variables, 1);
        assert_eq!(counts.calc_variables, 2099);
        assert_eq!(counts.total(), 2101);
        assert_eq!(data_names.len(), 2100);
        assert!(data_names.contains(&"base".to_string()));
        assert!(data_names.contains(&"calc_2098".to_string()));
        assert!(!data_names.iter().any(|name| name.contains("Group_")));
    }

    #[test]
    fn full_sync_with_empty_cache_does_not_evaluate_calc_rules() {
        let engine = Arc::new(make_calc_engine(vec![("always_one", "uint", "1")]));
        let mut runner = CalcRunner::new(engine);
        let mut kks_node_map = HashMap::new();
        kks_node_map.insert("always_one".to_string(), NodeId::new(1, "always_one"));

        let updates = build_full_sync_updates(
            &[],
            &kks_node_map,
            &mut runner,
            StatusCode::BadNoCommunication,
        );

        assert!(updates.is_empty());
    }

    #[test]
    fn config_zero_statuses_seed_all_configured_variables() {
        let port = make_port(
            "ttyAP0",
            vec![make_device(11, &["9CYE91GH201_READY", "9CYE91GH201_SL1"])],
        );

        let statuses = build_config_zero_statuses(&[port]);

        assert_eq!(statuses.len(), 1);
        assert_eq!(statuses[0].com, "ttyAP0");
        assert_eq!(statuses[0].device_id, 11);
        assert_eq!(
            statuses[0].value.get("9CYE91GH201_READY"),
            Some(&Value::UInt(0))
        );
        assert_eq!(
            statuses[0].value.get("9CYE91GH201_SL1"),
            Some(&Value::UInt(0))
        );
    }

    #[test]
    fn zero_variant_for_value_preserves_opcua_types() {
        assert_eq!(zero_variant_for_value(&Value::UInt(42)), Variant::UInt32(0));
        assert_eq!(
            zero_variant_for_value(&Value::Bool(true)),
            Variant::Boolean(false)
        );
        assert_eq!(
            zero_variant_for_value(&Value::Float(42.5)),
            Variant::Float(0.0)
        );
        assert_eq!(
            zero_variant_for_value(&Value::Double(42.5)),
            Variant::Double(0.0)
        );
    }

    #[test]
    fn device_updates_publish_zero_with_bad_quality_when_offline() {
        let port = make_port("ttyAP0", vec![make_device(11, &["READY"])]);
        let mut status = build_config_zero_statuses(&[port]).remove(0);
        let mut kks_node_map = HashMap::new();
        kks_node_map.insert("READY".to_string(), NodeId::new(1, "READY"));

        let mut updates = Vec::new();
        collect_device_updates(
            &status,
            &kks_node_map,
            &mut updates,
            StatusCode::BadWaitingForInitialData,
        );
        assert_eq!(updates.len(), 1);
        assert_eq!(updates[0].2.value, Some(Variant::Empty));
        assert_eq!(
            updates[0].2.status,
            Some(StatusCode::BadWaitingForInitialData)
        );

        status.com_status = true;
        status.device_status = false;
        status.value.insert("READY".to_string(), Value::UInt(42));
        let mut offline_updates = Vec::new();
        collect_device_updates(
            &status,
            &kks_node_map,
            &mut offline_updates,
            StatusCode::BadNoCommunication,
        );
        assert_eq!(offline_updates.len(), 1);
        assert_eq!(offline_updates[0].2.value, Some(Variant::UInt32(0)));
        assert_eq!(
            offline_updates[0].2.status,
            Some(StatusCode::BadNoCommunication)
        );

        status.device_status = true;
        let mut online_updates = Vec::new();
        collect_device_updates(
            &status,
            &kks_node_map,
            &mut online_updates,
            StatusCode::BadNoCommunication,
        );
        assert_eq!(online_updates[0].2.value, Some(Variant::UInt32(42)));
        assert_eq!(online_updates[0].2.status, Some(StatusCode::Good));
    }

    #[test]
    fn send_node_ignores_idle_and_status_values() {
        for value in [SEND_NODE_IDLE_VALUE, "", "0", "1", "2", "3"] {
            assert!(is_send_node_idle_or_status(value));
        }

        assert!(!is_send_node_idle_or_status("00 50"));
        assert!(!is_send_node_idle_or_status("005050"));
    }

    #[test]
    fn opc_write_log_preview_is_bounded() {
        let short = "0102";
        assert_eq!(opc_write_log_preview(short), (short.to_string(), false));

        let long = "A".repeat(OPC_WRITE_LOG_PREVIEW_CHARS + 10);
        let (preview, truncated) = opc_write_log_preview(&long);
        assert_eq!(preview.len(), OPC_WRITE_LOG_PREVIEW_CHARS);
        assert!(truncated);
    }

    #[test]
    fn opc_write_diagnostic_reports_client_variant_type_and_bounded_value() {
        let numeric = Variant::Double(12.5);
        let (value_type, summary) = opc_write_value_diagnostic(Some(&numeric));
        assert!(value_type.contains("Double"));
        assert_eq!(summary, "12.5");

        let long = Variant::String(UAString::from("A".repeat(OPC_WRITE_LOG_PREVIEW_CHARS + 10)));
        let (value_type, summary) = opc_write_value_diagnostic(Some(&long));
        assert!(value_type.contains("String"));
        assert!(summary.contains("已截断"));
        assert!(summary.len() < OPC_WRITE_LOG_PREVIEW_CHARS + 80);

        assert_eq!(
            opc_write_value_diagnostic(None),
            ("None".to_string(), "<未携带值>".to_string())
        );
    }

    #[test]
    fn send_node_write_enqueues_ack_command_and_checksum() {
        let (tx, rx) = mpsc::channel();
        let (local_tx, local_rx) = mpsc::channel();
        let system_record = Arc::new(Mutex::new(BoundedVecDeque::<String>::new(10)));
        let pending = Arc::new(Mutex::new(HashMap::<String, PendingCommand>::new()));
        let context = SendNodeWriteContext {
            system_record: system_record.clone(),
            pending_commands: pending.clone(),
            local_ack_status_tx: local_tx,
            command_pending_timeout: Duration::from_secs(5),
        };

        let status = handle_send_node_write(data_value_string("01 02"), "COM1", &tx, &context, 42);

        assert_eq!(status, StatusCode::Good);
        let cmd = rx.try_recv().expect("命令应进入串口队列");
        assert_eq!(cmd.com, "COM1");
        assert_eq!(cmd.ack_id, Some(42));
        match cmd.command {
            CommandType::SendDataWithAck(data) => {
                assert_eq!(data.device_id, 1);
                assert_eq!(data.command, vec![0x01, 0x02, 0x03]);
            }
            other => panic!("应生成 SendDataWithAck，实际为 {:?}", other),
        }
        assert_eq!(pending.lock().unwrap().get("COM1").unwrap().ack_id, 42);
        let local_update = local_rx.try_recv().expect("应入队本地 Waiting 状态");
        assert_eq!(local_update.com, "COM1");
        assert_eq!(local_update.ack_id, Some(42));
        assert_eq!(local_update.status, CommandAckStatus::Waiting);
        assert_eq!(system_record.lock().unwrap().len(), 1);
    }

    #[test]
    fn send_node_write_ignores_idle_and_status_values() {
        let (tx, rx) = mpsc::channel();
        let (local_tx, local_rx) = mpsc::channel();
        let system_record = Arc::new(Mutex::new(BoundedVecDeque::<String>::new(10)));
        let pending = Arc::new(Mutex::new(HashMap::<String, PendingCommand>::new()));
        let context = SendNodeWriteContext {
            system_record: system_record.clone(),
            pending_commands: pending.clone(),
            local_ack_status_tx: local_tx,
            command_pending_timeout: Duration::from_secs(5),
        };

        for value in [SEND_NODE_IDLE_VALUE, "", "0", "1", "2", "3"] {
            let status =
                handle_send_node_write(data_value_string(value), "COM1", &tx, &context, 42);
            assert_eq!(status, StatusCode::Good);
        }

        assert!(rx.try_recv().is_err());
        assert!(local_rx.try_recv().is_err());
        assert!(pending.lock().unwrap().is_empty());
        assert_eq!(system_record.lock().unwrap().len(), 0);
    }

    #[test]
    fn send_node_write_rejects_bad_values() {
        let (tx, rx) = mpsc::channel();
        let (local_tx, local_rx) = mpsc::channel();
        let system_record = Arc::new(Mutex::new(BoundedVecDeque::<String>::new(10)));
        let pending = Arc::new(Mutex::new(HashMap::<String, PendingCommand>::new()));
        let context = SendNodeWriteContext {
            system_record: system_record.clone(),
            pending_commands: pending.clone(),
            local_ack_status_tx: local_tx,
            command_pending_timeout: Duration::from_secs(5),
        };

        assert_eq!(
            handle_send_node_write(
                DataValue::new_now(Variant::UInt32(1)),
                "COM1",
                &tx,
                &context,
                42,
            ),
            StatusCode::BadTypeMismatch
        );
        assert_eq!(
            handle_send_node_write(data_value_string("GG"), "COM1", &tx, &context, 42,),
            StatusCode::BadInvalidArgument
        );
        assert_eq!(
            handle_send_node_write(data_value_string("01"), "COM1", &tx, &context, 42,),
            StatusCode::BadInvalidArgument
        );

        assert!(rx.try_recv().is_err());
        assert!(local_rx.try_recv().is_err());
        assert!(pending.lock().unwrap().is_empty());
    }

    #[test]
    fn send_node_write_rejects_duplicate_until_terminal_ack() {
        let (tx, rx) = mpsc::channel();
        let (local_tx, _local_rx) = mpsc::channel();
        let system_record = Arc::new(Mutex::new(BoundedVecDeque::<String>::new(10)));
        let pending = Arc::new(Mutex::new(HashMap::<String, PendingCommand>::new()));
        let context = SendNodeWriteContext {
            system_record: system_record.clone(),
            pending_commands: pending.clone(),
            local_ack_status_tx: local_tx,
            command_pending_timeout: Duration::from_secs(5),
        };
        pending.lock().unwrap().insert(
            "COM1".into(),
            PendingCommand {
                ack_id: 7,
                deadline: Instant::now() + Duration::from_secs(5),
            },
        );

        assert_eq!(
            handle_send_node_write(data_value_string("01 02"), "COM1", &tx, &context, 42,),
            StatusCode::BadWaitingForResponse
        );
        assert!(rx.try_recv().is_err());

        apply_ack_pending_state(
            &mut pending.lock().unwrap(),
            &CommandAckUpdate {
                com: "COM1".into(),
                ack_id: Some(7),
                status: CommandAckStatus::Received,
            },
        );

        assert_eq!(
            handle_send_node_write(data_value_string("01 02"), "COM1", &tx, &context, 42,),
            StatusCode::Good
        );
        assert!(rx.try_recv().is_ok());
    }

    #[test]
    fn ack_pending_state_waiting_holds_terminal_clears() {
        let mut pending = HashMap::new();
        pending.insert(
            "COM1".into(),
            PendingCommand {
                ack_id: 9,
                deadline: Instant::now() + Duration::from_secs(5),
            },
        );

        assert!(apply_ack_pending_state(
            &mut pending,
            &CommandAckUpdate {
                com: "COM1".into(),
                ack_id: Some(9),
                status: CommandAckStatus::Waiting,
            },
        ));
        assert_eq!(pending.get("COM1").unwrap().ack_id, 9);

        assert!(!apply_ack_pending_state(
            &mut pending,
            &CommandAckUpdate {
                com: "COM1".into(),
                ack_id: Some(8),
                status: CommandAckStatus::Received,
            },
        ));
        assert!(pending.contains_key("COM1"));

        assert!(apply_ack_pending_state(
            &mut pending,
            &CommandAckUpdate {
                com: "COM1".into(),
                ack_id: Some(9),
                status: CommandAckStatus::Received,
            },
        ));
        assert!(!pending.contains_key("COM1"));

        pending.insert(
            "COM1".into(),
            PendingCommand {
                ack_id: 10,
                deadline: Instant::now() + Duration::from_secs(5),
            },
        );
        assert!(apply_ack_pending_state(
            &mut pending,
            &CommandAckUpdate {
                com: "COM1".into(),
                ack_id: Some(10),
                status: CommandAckStatus::Failed,
            },
        ));
        assert!(!pending.contains_key("COM1"));
    }

    #[test]
    fn expired_pending_commands_emit_failed_and_clear() {
        let pending = Arc::new(Mutex::new(HashMap::<String, PendingCommand>::new()));
        let now = Instant::now();
        pending.lock().unwrap().insert(
            "COM1".into(),
            PendingCommand {
                ack_id: 11,
                deadline: now - Duration::from_millis(1),
            },
        );
        pending.lock().unwrap().insert(
            "COM2".into(),
            PendingCommand {
                ack_id: 12,
                deadline: now + Duration::from_secs(1),
            },
        );

        let mut updates = collect_expired_pending_commands(&pending, now);
        updates.sort_by(|a, b| a.com.cmp(&b.com));

        assert_eq!(
            updates,
            vec![CommandAckUpdate {
                com: "COM1".into(),
                ack_id: Some(11),
                status: CommandAckStatus::Failed,
            }]
        );
        let guard = pending.lock().unwrap();
        assert!(!guard.contains_key("COM1"));
        assert_eq!(guard.get("COM2").unwrap().ack_id, 12);
    }

    #[test]
    fn command_pending_timeout_uses_retry_window_with_floor() {
        assert_eq!(command_pending_timeout(300, 3), Duration::from_secs(5));
        assert_eq!(command_pending_timeout(2_000, 3), Duration::from_secs(8));
    }

    #[test]
    fn ack_status_values_are_stable_opc_strings() {
        assert_eq!(send_status_value(CommandAckStatus::Received), "1");
        assert_eq!(send_status_value(CommandAckStatus::Waiting), "2");
        assert_eq!(send_status_value(CommandAckStatus::Failed), "3");
    }
}
