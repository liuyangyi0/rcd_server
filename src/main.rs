//! RCD Server  串口采集 OPC UA 网关。
//!
//! 主要流程：
//! 1. 加载 `config/config.toml` 全局配置。
//! 2. 扫描 `config/rcd*.csv` 文件，解析设备通信参数与数据点定义。
//! 3. 为每个串口启动独立的通信线程（轮询采集 + 命令下发）。
//! 4. 启动 OPC UA 服务器，将采集数据发布为变量节点。

use std::collections::{HashMap, HashSet};
use std::fs::{File, OpenOptions};
use std::io::{self, Write};
use std::path::Path;
use std::sync::{mpsc, Arc, Mutex};
use std::time::Duration;

use bounded_vec_deque::BoundedVecDeque;
use fs2::FileExt;
use tokio::task::JoinHandle;
use tokio_util::sync::CancellationToken;

use log::{error, info, warn};
use rcd_server::broadcast::Broadcaster;
use rcd_server::calc_engine::config::load_calc_config;
use rcd_server::calc_engine::engine::CalcEngine;
use rcd_server::config;
use rcd_server::csv_parser::{self, DeviceConfig};
use rcd_server::file_scanner;
use rcd_server::logging;
use rcd_server::model::{Command, CommandAckUpdate, SystemState};
use rcd_server::operations::Operations;
use rcd_server::runtime_cache::LatestStatusCache;
use rcd_server::serial::manager::SerialConfig;
use rcd_server::serial::port_config::SerialPortConfig;
use rcd_server::serial::worker::spawn_serial_worker;
use rcd_server::server::opcua::{command_pending_timeout, run_opcua_server};

// ============================================================
//  程序入口
// ============================================================

#[tokio::main]
async fn main() -> tokio::io::Result<()> {
    // 必须在初始化文件 logger 和打开串口之前加锁，避免第二个实例扰乱
    // rCURRENT 日志、抢占串口或短暂创建一套重复 worker。
    let _instance_lock = match InstanceLock::acquire() {
        Ok(lock) => lock,
        Err(error) => {
            let message = format!(
                "无法获取单实例锁，程序可能已在运行: pid={} error={error}",
                std::process::id()
            );
            logging::write_startup_failure(&message);
            eprintln!("[程序启动失败] {message}");
            return Err(error);
        }
    };

    let _logger = logging::init();
    install_panic_hook();
    info!(
        "[程序启动] pid={} version={} exe={:?}",
        std::process::id(),
        env!("CARGO_PKG_VERSION"),
        std::env::current_exe().ok(),
    );

    let result = run_server().await;
    match &result {
        Ok(()) => info!("[程序退出] pid={} reason=normal", std::process::id()),
        Err(error) => error!(
            "[程序退出] pid={} reason=error error={:?}",
            std::process::id(),
            error
        ),
    }
    log::logger().flush();
    result
}

async fn run_server() -> tokio::io::Result<()> {
    // 全局取消信号：Ctrl+C / SIGTERM / 服务器异常退出时通知所有子任务。
    // 启动阶段提前返回也必须取消信号监听任务。
    let cancel = CancellationToken::new();
    let _cancel_on_return = cancel.clone().drop_guard();
    let _signal_listener = spawn_shutdown_listener(cancel.clone())?;

    // 全局广播器：串口数据会广播到所有订阅者（OPC UA / TCP 客户端）
    let broadcaster = Broadcaster::new();
    let latest_status_cache = LatestStatusCache::new();

    // 加载配置
    let software_config = config::init_config().expect("加载配置文件失败");
    let opcua_advertised_host = software_config
        .server
        .opcua_advertised_host()
        .map_err(|e| io::Error::new(io::ErrorKind::InvalidInput, e))?;

    // 系统状态 & 操作记录
    let system_state = SystemState::new();
    let system_record = Arc::new(Mutex::new(BoundedVecDeque::<String>::new(20000)));

    // 加载计算引擎：配置异常时降级为空引擎，不终止服务
    let calc_engine = {
        let config_dir = config::config_dir().expect("获取配置目录失败");
        let calc_path = config_dir.join("calc.toml");
        let calc_config = load_calc_config(&calc_path).unwrap_or_else(|e| {
            error!("[CalcEngine] 加载计算规则失败: {}，以空规则启动", e);
            rcd_server::calc_engine::config::CalcConfig { rules: Vec::new() }
        });

        if calc_config.rules.is_empty() {
            info!("[CalcEngine] 无计算规则，引擎未启用");
            Arc::new(CalcEngine::new(vec![]).unwrap())
        } else {
            match CalcEngine::new(calc_config.rules) {
                Ok(engine) => {
                    info!(
                        "[CalcEngine] 初始化成功，共 {} 条规则",
                        engine.output_names().len()
                    );
                    Arc::new(engine)
                }
                Err(e) => {
                    error!("[CalcEngine] 构建引擎失败: {}，以空引擎启动", e);
                    Arc::new(CalcEngine::new(vec![]).unwrap())
                }
            }
        }
    };

    // Load once: operations identities and worker configurations must refer to
    // the same validated CSV snapshot, including a persisted closed port.
    let serial_configs = load_and_group_csv_configs()?;
    let (log_dir, log_basename) = logging::startup_log_location()?;
    let operations = Operations::new_with_shutdown(
        config::config_dir().map_err(|e| io::Error::other(e.to_string()))?,
        log_dir,
        log_basename,
        &serial_configs,
        cancel.clone(),
    )?;
    if cancel.is_cancelled() {
        return Ok(());
    }
    let (ack_status_tx, ack_status_rx) = mpsc::channel::<CommandAckUpdate>();

    // 初始化串口并启动通信线程
    let result = match init_serial_ports(
        broadcaster.clone(),
        &software_config,
        system_state.clone(),
        system_record.clone(),
        latest_status_cache.clone(),
        ack_status_tx,
        cancel.clone(),
        serial_configs,
        operations.clone(),
    )
    .await
    {
        Ok((configs, txs, worker_handles)) => {
            if cancel.is_cancelled() {
                shutdown_workers(worker_handles).await;
                return Ok(());
            }
            // ---- 计算引擎基础变量是否在 CSV 数据点中定义 ----
            let mut all_kks: HashSet<String> = HashSet::new();
            for port in &configs {
                for dev in &port.devices {
                    for rec in &dev.records {
                        all_kks.insert(rec.kks.clone());
                    }
                }
            }

            if !calc_engine.is_empty() {
                let required = calc_engine.required_base_variables();
                let missing: Vec<&String> = required
                    .iter()
                    .filter(|v| !all_kks.contains(v.as_str()))
                    .collect();

                if !missing.is_empty() {
                    warn!(
                        "[CalcEngine] {} 个公式基础变量未在 CSV 数据点中定义，运行时将使用默认值。前 20 条: {:?}",
                        missing.len(),
                        missing.iter().take(20).collect::<Vec<_>>(),
                    );
                }
            }

            // 启动 OPC UA 服务器（阻塞直到退出或收到取消信号）
            let opcua_command_pending_timeout = command_pending_timeout(
                software_config.settings.timeout,
                software_config.settings.command_retry_count,
            );
            let server_result = run_opcua_server(
                configs,
                broadcaster,
                system_state,
                system_record,
                txs,
                ack_status_rx,
                opcua_command_pending_timeout,
                opcua_advertised_host,
                calc_engine,
                latest_status_cache,
                cancel.clone(),
                operations,
            )
            .await;
            match &server_result {
                Ok(_) => info!("OPC UA 服务器已正常退出"),
                Err(e) => error!("OPC UA 服务器运行失败: {}", e),
            }

            // 服务器退出后，确保取消信号已触发（可能是内部异常而非 Ctrl+C）
            cancel.cancel();

            // 等待串口 worker 结束，带 3 秒超时
            shutdown_workers(worker_handles).await;

            server_result
        }
        Err(e) => {
            error!("初始化失败: {}", e);
            cancel.cancel();
            Err(e)
        }
    };

    result
}

fn spawn_shutdown_listener(cancel: CancellationToken) -> io::Result<JoinHandle<()>> {
    // Register Unix handlers before loading configuration or starting workers.
    // A service supervisor sends SIGTERM; it must use the same cleanup path as
    // an interactive SIGINT, rather than the OS's immediate termination path.
    #[cfg(unix)]
    let signal = {
        use tokio::signal::unix::{signal, SignalKind};
        let mut interrupt = signal(SignalKind::interrupt())?;
        let mut terminate = signal(SignalKind::terminate())?;
        async move {
            let (received, name) = tokio::select! {
                received = interrupt.recv() => (received, "SIGINT"),
                received = terminate.recv() => (received, "SIGTERM"),
            };
            received.map(|()| name).ok_or_else(|| {
                io::Error::new(io::ErrorKind::BrokenPipe, "shutdown signal stream closed")
            })
        }
    };
    #[cfg(not(unix))]
    let signal = async {
        tokio::signal::ctrl_c().await?;
        Ok("Ctrl+C")
    };

    Ok(tokio::spawn(shutdown_on_signal(cancel, signal)))
}

async fn shutdown_on_signal(
    cancel: CancellationToken,
    signal: impl std::future::Future<Output = io::Result<&'static str>>,
) {
    tokio::select! {
        biased;
        _ = cancel.cancelled() => {}
        result = signal => {
            match result {
                Ok(name) => info!("收到 {}，开始优雅退出", name),
                Err(error) => error!("退出信号监听失败，开始关闭服务: {}", error),
            }
            cancel.cancel();
        }
    }
}

/// 持有操作系统级独占文件锁；进程正常退出、panic 展开或被系统终止时，
/// 文件句柄关闭会自动释放锁。锁文件本身保留，不能删除，否则可能产生 inode 竞争。
#[derive(Debug)]
struct InstanceLock {
    _file: File,
}

impl InstanceLock {
    fn acquire() -> io::Result<Self> {
        let exe = std::env::current_exe()?;
        let lock_path = exe
            .parent()
            .unwrap_or_else(|| Path::new("."))
            .join(".rcd_server.lock");
        Self::acquire_at(&lock_path)
    }

    fn acquire_at(path: &Path) -> io::Result<Self> {
        let mut file = OpenOptions::new()
            .create(true)
            .truncate(false)
            .read(true)
            .write(true)
            .open(path)?;

        // Windows 的 LockFileEx 锁冲突在部分 Rust 版本中仍映射为
        // Uncategorized，Linux 则通常是 WouldBlock。文件已经成功打开后，
        // try_lock_exclusive 失败在这里统一视为存在锁持有者。
        FileExt::try_lock_exclusive(&file).map_err(|error| {
            io::Error::new(
                io::ErrorKind::AlreadyExists,
                format!("另一个 rcd_server 实例持有锁文件 {path:?}: {error}"),
            )
        })?;

        file.set_len(0)?;
        writeln!(file, "{}", std::process::id())?;
        file.flush()?;
        Ok(Self { _file: file })
    }
}

fn install_panic_hook() {
    let default_hook = std::panic::take_hook();
    std::panic::set_hook(Box::new(move |panic_info| {
        let thread = std::thread::current();
        let thread_name = thread.name().unwrap_or("unnamed");
        let message = panic_info
            .payload()
            .downcast_ref::<&str>()
            .copied()
            .or_else(|| {
                panic_info
                    .payload()
                    .downcast_ref::<String>()
                    .map(String::as_str)
            })
            .unwrap_or("non-string panic payload");
        let (file, line, column) = panic_info
            .location()
            .map(|location| (location.file(), location.line(), location.column()))
            .unwrap_or(("unknown", 0, 0));
        let backtrace = std::backtrace::Backtrace::force_capture();

        error!(
            "[程序Panic] pid={} thread={:?} location={}:{}:{} message={:?} backtrace={}",
            std::process::id(),
            thread_name,
            file,
            line,
            column,
            message,
            backtrace,
        );
        // BufferAndFlush 模式下显式刷新，尽量在进程结束前落盘。
        log::logger().flush();
        default_hook(panic_info);
    }));
}

/// 等待所有串口 worker 自然结束。这里的超时只限制 join 等待；
/// 已开始的 spawn_blocking I/O 不能通过丢弃 JoinHandle 强制中断。
/// 外部守护进程应以真实进程退出为准，并单独记录超时强制结束。
async fn shutdown_workers(handles: Vec<JoinHandle<()>>) {
    if handles.is_empty() {
        return;
    }
    info!("等待 {} 个串口线程退出…", handles.len());
    let join_all = async {
        for h in handles {
            let _ = h.await;
        }
    };
    match tokio::time::timeout(Duration::from_secs(3), join_all).await {
        Ok(_) => info!("串口线程全部退出完毕"),
        Err(_) => warn!("串口线程 3s 内未退出，跳过等待"),
    }
}

// ============================================================
//  初始化
// ============================================================

/// 扫描 CSV 配置文件、聚合串口设备、启动通信线程。
///
/// 返回 `(串口配置列表, 命令发送器列表)` 供服务器使用。
/// 这是启动边界；显式传入各个共享资源，避免把可变全局状态隐藏在聚合对象中。
#[allow(clippy::too_many_arguments)]
async fn init_serial_ports(
    broadcaster: Broadcaster,
    software_config: &config::Config,
    system_state: SystemState,
    system_record: Arc<Mutex<BoundedVecDeque<String>>>,
    latest_status_cache: LatestStatusCache,
    ack_status_tx: mpsc::Sender<CommandAckUpdate>,
    cancel: CancellationToken,
    serial_port_configs: Vec<SerialPortConfig>,
    operations: Operations,
) -> io::Result<(
    Vec<SerialPortConfig>,
    Vec<mpsc::Sender<Command>>,
    Vec<JoinHandle<()>>,
)> {
    if serial_port_configs.is_empty() {
        warn!("未找到任何有效的设备配置文件");
    } else {
        info!(
            "已加载 {} 个串口, 共 {} 个设备",
            serial_port_configs.len(),
            serial_port_configs
                .iter()
                .map(|c| c.devices.len())
                .sum::<usize>(),
        );
    }

    // 2. 为每个串口启动通信线程
    let mut txs = Vec::with_capacity(serial_port_configs.len());
    let mut handles = Vec::with_capacity(serial_port_configs.len());

    for port_config in &serial_port_configs {
        if cancel.is_cancelled() {
            break;
        }
        let (tx, rx) = mpsc::channel();
        txs.push(tx);
        operations.register_worker(&port_config.port_number, txs.last().unwrap().clone());

        system_state.add_port(port_config.to_runtime_state());

        let handle = spawn_serial_worker(
            broadcaster.clone(),
            SerialConfig::new(
                port_config.port_number.clone(),
                port_config.baud_rate,
                software_config.serial.data_bits,
                software_config.serial.stop_bits,
                software_config.serial.parity,
                Duration::from_millis(software_config.settings.timeout),
            ),
            port_config.clone(),
            rx,
            ack_status_tx.clone(),
            software_config.clone(),
            system_state.clone(),
            system_record.clone(),
            latest_status_cache.clone(),
            cancel.clone(),
            operations.clone(),
        );
        handles.push(handle);
    }

    Ok((serial_port_configs, txs, handles))
}

/// 从 config 目录加载所有 `rcd*.csv` 文件并按串口号聚合。
fn load_and_group_csv_configs() -> io::Result<Vec<SerialPortConfig>> {
    let config_dir =
        config::config_dir().map_err(|e| io::Error::new(io::ErrorKind::NotFound, e.to_string()))?;
    load_and_group_csv_configs_from_dir(&config_dir)
}

fn load_and_group_csv_configs_from_dir(config_dir: &Path) -> io::Result<Vec<SerialPortConfig>> {
    let mut csv_files = file_scanner::read_and_process_files(config_dir, r"^rcd.*\.csv$")?;
    csv_files.sort();

    let mut configs: HashMap<String, SerialPortConfig> = HashMap::new();

    for file in &csv_files {
        // 不允许跳过坏文件后发布一份看似成功但实际漏设备的拓扑。
        // 该阶段尚未打开串口；先校验全部 CSV，再启动采集。
        let (conf, recs) = csv_parser::parse_csv(file).map_err(|error| {
            io::Error::new(
                io::ErrorKind::InvalidData,
                format!("解析设备配置 CSV {file:?} 失败，拒绝启动: {error}"),
            )
        })?;
        let port = configs
            .entry(conf.com.clone())
            .or_insert_with(|| SerialPortConfig::from_csv_config(&conf));
        // 一个物理串口只能使用一套通信参数，不能让后续设备静默继承
        // 第一份 CSV 的波特率或协议模式。仍在打开任何串口之前失败。
        if port.baud_rate != conf.baud_rate {
            return Err(io::Error::new(
                io::ErrorKind::InvalidData,
                format!(
                    "设备配置 CSV {file:?} 的串口 {} 参数冲突: baud_rate 已有 {}，当前 {}，拒绝启动",
                    conf.com, port.baud_rate, conf.baud_rate
                ),
            ));
        }
        if port.is_special != conf.is_special {
            return Err(io::Error::new(
                io::ErrorKind::InvalidData,
                format!(
                    "设备配置 CSV {file:?} 的串口 {} 参数冲突: is_special 已有 {}，当前 {}，拒绝启动",
                    conf.com, port.is_special, conf.is_special
                ),
            ));
        }
        port.devices.push(DeviceConfig::new(conf, recs));
    }

    let mut ports: Vec<_> = configs.into_values().collect();
    ports.sort_by(|left, right| left.port_number.cmp(&right.port_number));
    Ok(ports)
}

#[cfg(test)]
mod startup_tests {
    use super::*;

    #[tokio::test]
    async fn shutdown_signal_delivery_cancels_workers_and_admission_token() {
        for name in ["SIGINT", "SIGTERM", "Ctrl+C"] {
            let cancel = CancellationToken::new();
            let worker = cancel.child_token();
            let admission = cancel.clone();
            shutdown_on_signal(cancel.clone(), std::future::ready(Ok(name))).await;
            assert!(cancel.is_cancelled(), "{name}");
            assert!(worker.is_cancelled(), "{name}");
            assert!(admission.is_cancelled(), "{name}");
        }
    }

    #[tokio::test]
    async fn shutdown_signal_waiter_ends_on_internal_cancellation() {
        let cancel = CancellationToken::new();
        let listener = tokio::spawn(shutdown_on_signal(cancel.clone(), std::future::pending()));
        cancel.cancel();
        tokio::time::timeout(Duration::from_secs(1), listener)
            .await
            .expect("internal failure must not leave a signal waiter alive")
            .unwrap();
    }

    #[tokio::test]
    async fn shutdown_signal_failure_closes_admission_instead_of_ignoring_error() {
        let cancel = CancellationToken::new();
        shutdown_on_signal(
            cancel.clone(),
            std::future::ready(Err(io::Error::new(io::ErrorKind::BrokenPipe, "test error"))),
        )
        .await;
        assert!(cancel.is_cancelled());
    }

    fn device_csv(address: &str) -> String {
        format!(
            "com,COM1,,,,,,,\n\
             baud_rate,115200,,,,,,,\n\
             is_special,false,,,,,,,\n\
             com_index,1,,,,,,,\n\
             device_id,{address},,,,,,,\n\
             data_len,8,,,,,,,\n\
             kks_prefix,DEV{address}_,,,,,,,\n\
             kks,type_,f_type,byte_index,bit_index,def,max,min,lh\n\
             TEMP,uint,status,1,0,0,255,0,0\n"
        )
    }

    #[test]
    fn csv_loading_rejects_invalid_device_address_instead_of_omitting_device() {
        let dir = tempfile::tempdir().unwrap();
        std::fs::write(dir.path().join("rcd_a.csv"), device_csv("1")).unwrap();
        std::fs::write(dir.path().join("rcd_b.csv"), device_csv("256")).unwrap();
        let error = load_and_group_csv_configs_from_dir(dir.path()).unwrap_err();
        assert_eq!(error.kind(), io::ErrorKind::InvalidData);
        assert!(error.to_string().contains("rcd_b.csv"));
        assert!(error.to_string().contains("device_id"));
    }

    #[test]
    fn csv_loading_groups_all_valid_devices_and_preserves_rcd_address_range() {
        let dir = tempfile::tempdir().unwrap();
        std::fs::write(dir.path().join("rcd_a.csv"), device_csv("0")).unwrap();
        std::fs::write(dir.path().join("rcd_b.csv"), device_csv("255")).unwrap();
        let ports = load_and_group_csv_configs_from_dir(dir.path()).unwrap();
        assert_eq!(ports.len(), 1);
        assert_eq!(ports[0].devices.len(), 2);
        assert_eq!(ports[0].devices[0].config.device_id, 0);
        assert_eq!(ports[0].devices[1].config.device_id, 255);
    }

    #[test]
    fn csv_loading_rejects_conflicting_baud_rates_on_one_port() {
        let dir = tempfile::tempdir().unwrap();
        std::fs::write(dir.path().join("rcd_a.csv"), device_csv("1")).unwrap();
        std::fs::write(
            dir.path().join("rcd_b.csv"),
            device_csv("2").replace("baud_rate,115200", "baud_rate,9600"),
        )
        .unwrap();

        let error = load_and_group_csv_configs_from_dir(dir.path()).unwrap_err();
        assert_eq!(error.kind(), io::ErrorKind::InvalidData);
        for expected in ["rcd_b.csv", "COM1", "baud_rate", "115200", "9600"] {
            assert!(error.to_string().contains(expected), "{error}");
        }
    }

    #[test]
    fn csv_loading_rejects_conflicting_protocol_modes_on_one_port() {
        let dir = tempfile::tempdir().unwrap();
        std::fs::write(dir.path().join("rcd_a.csv"), device_csv("1")).unwrap();
        std::fs::write(
            dir.path().join("rcd_b.csv"),
            device_csv("2").replace("is_special,false", "is_special,true"),
        )
        .unwrap();

        let error = load_and_group_csv_configs_from_dir(dir.path()).unwrap_err();
        assert_eq!(error.kind(), io::ErrorKind::InvalidData);
        for expected in ["rcd_b.csv", "COM1", "is_special", "false", "true"] {
            assert!(error.to_string().contains(expected), "{error}");
        }
    }

    #[test]
    fn csv_loading_allows_different_settings_on_different_ports() {
        let dir = tempfile::tempdir().unwrap();
        std::fs::write(dir.path().join("rcd_a.csv"), device_csv("1")).unwrap();
        std::fs::write(
            dir.path().join("rcd_b.csv"),
            device_csv("2")
                .replace("com,COM1", "com,COM2")
                .replace("baud_rate,115200", "baud_rate,9600")
                .replace("is_special,false", "is_special,true"),
        )
        .unwrap();

        let ports = load_and_group_csv_configs_from_dir(dir.path()).unwrap();
        assert_eq!(ports.len(), 2);
        assert_eq!(ports[0].port_number, "COM1");
        assert_eq!(ports[0].baud_rate, 115200);
        assert!(!ports[0].is_special);
        assert_eq!(ports[1].port_number, "COM2");
        assert_eq!(ports[1].baud_rate, 9600);
        assert!(ports[1].is_special);
        assert_eq!(ports[1].devices[0].config.device_id, 2);
    }

    #[test]
    fn instance_lock_rejects_a_second_holder_and_releases_on_drop() {
        let dir = tempfile::tempdir().expect("创建单实例锁测试目录");
        let path = dir.path().join("rcd_server.lock");

        let first = InstanceLock::acquire_at(&path).expect("首次获取单实例锁应成功");
        let second = InstanceLock::acquire_at(&path).expect_err("第二个实例必须被拒绝");
        assert_eq!(second.kind(), io::ErrorKind::AlreadyExists);

        drop(first);
        InstanceLock::acquire_at(&path).expect("持有者退出后必须能重新获取锁");
    }
}
