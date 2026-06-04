//! 串口线程入口与轮询调度。
//!
//! 负责启动串口通信线程，以及主/备机模式的轮询逻辑。
//!
//! 使用 `tokio::task::spawn_blocking` 在主运行时的阻塞线程池中
//! 运行同步串口 I/O，通过 `Handle` 桥接异步操作。

use std::panic::{catch_unwind, AssertUnwindSafe};
use std::sync::mpsc::Receiver;
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use bounded_vec_deque::BoundedVecDeque;
use log::{error, info, warn};
use tokio::task::JoinHandle;
use tokio_util::sync::CancellationToken;

use super::manager::{SerialConfig, SerialManager};
use super::port_config::SerialPortConfig;
use crate::broadcast::Broadcaster;
use crate::config;
use crate::config::RunLocation;
use crate::model::{get_sum, Command, CommandType, SendData, SystemState};
use crate::protocol::{cmd, threshold};
use crate::runtime_cache::LatestStatusCache;

const WORKER_HEARTBEAT_INTERVAL: Duration = Duration::from_secs(30);
const MAX_COMMANDS_PER_CYCLE: usize = 1024;
const SLOW_RECEIVE_LOG_THRESHOLD: Duration = Duration::from_secs(2);

// ============================================================
//  串口线程入口
// ============================================================

/// 启动串口通信工作线程。
///
/// 使用 `tokio::task::spawn_blocking` 在主运行时的阻塞线程池中运行，
/// 无需创建独立的 tokio `Runtime`。
#[allow(clippy::too_many_arguments)]
pub fn spawn_serial_worker(
    broadcaster: Broadcaster,
    serial_config: SerialConfig,
    port_config: SerialPortConfig,
    rx: Receiver<Command>,
    software_config: config::Config,
    system_state: SystemState,
    system_record: Arc<Mutex<BoundedVecDeque<String>>>,
    latest_status_cache: LatestStatusCache,
    cancel: CancellationToken,
) -> JoinHandle<()> {
    tokio::task::spawn_blocking(move || {
        let port_name = serial_config.port_name.clone();
        let result = catch_unwind(AssertUnwindSafe(|| {
            let mut manager = SerialManager::new(
                serial_config,
                port_config,
                broadcaster,
                latest_status_cache,
                software_config.server.run_on,
                software_config.server.current_run,
                system_record,
            );

            info!(
                "串口 {} worker 已启动，当前角色: {:?}，设备数: {}，端口启用: {}",
                port_name,
                manager.current_run,
                manager.port_config.devices.len(),
                manager.port_config.status
            );

            let mut last_idle_log = Instant::now() - WORKER_HEARTBEAT_INTERVAL;
            let mut last_heartbeat_log = Instant::now() - WORKER_HEARTBEAT_INTERVAL;

            loop {
                log_worker_heartbeat(&manager, &port_name, &mut last_heartbeat_log);

                if cancel.is_cancelled() {
                    info!("串口 {} worker 收到取消信号，退出", port_name);
                    return;
                }

                // 处理来自上层的命令
                process_incoming_commands(&mut manager, &rx, &system_state);

                // 串口被禁用时跳过轮询
                if !manager.port_config.status {
                    log_idle_reason(
                        &port_name,
                        &mut last_idle_log,
                        "端口被上层设置为禁用，暂停轮询发送",
                    );
                    std::thread::sleep(Duration::from_millis(100));
                    continue;
                }

                // 无设备配置时跳过轮询
                if manager.port_config.devices.is_empty() {
                    log_idle_reason(&port_name, &mut last_idle_log, "没有设备配置，暂停轮询发送");
                    std::thread::sleep(Duration::from_millis(100));
                    continue;
                }

                // 根据主/备角色决定行为，返回是否已发送命令（需要接收响应）
                let should_receive = match manager.current_run {
                    RunLocation::Primary => run_primary_cycle(&mut manager),
                    RunLocation::Secondary => run_secondary_cycle(&mut manager),
                };

                if manager.current_run == RunLocation::Secondary {
                    log_idle_reason(
                        &port_name,
                        &mut last_idle_log,
                        "当前是备机监听模式，不主动轮询发送",
                    );
                } else if !should_receive {
                    log_idle_reason(
                        &port_name,
                        &mut last_idle_log,
                        "本轮没有发送命令，通常是设备禁用或超时跳过窗口",
                    );
                }

                // 仅在实际发送了命令（或备机监听模式）时才接收数据
                if should_receive {
                    let device_id = manager
                        .port_config
                        .devices
                        .get(manager.index)
                        .map(|d| d.config.device_id)
                        .unwrap_or_default();
                    info!(
                        "串口 {} 进入接收: role={:?}, index={}, device_id={}",
                        port_name, manager.current_run, manager.index, device_id
                    );
                    let receive_started = Instant::now();
                    manager.receive_data();
                    let elapsed = receive_started.elapsed();
                    if elapsed >= SLOW_RECEIVE_LOG_THRESHOLD {
                        warn!(
                            "串口 {} 接收耗时 {:?}，可能存在串口 read 阻塞或设备响应异常",
                            port_name, elapsed
                        );
                    }
                }

                // 主机模式：轮询下一个设备；备机模式：index 由 extract_frame 从转发头解析
                if manager.current_run == RunLocation::Primary {
                    manager.index = (manager.index + 1) % manager.port_config.devices.len();
                }
                std::thread::sleep(Duration::from_millis(1));
            }
        }));

        if result.is_err() {
            error!("串口 {} worker panic，线程已退出", port_name);
        }
    })
}

fn log_idle_reason(port_name: &str, last_log: &mut Instant, reason: &str) {
    if last_log.elapsed() >= WORKER_HEARTBEAT_INTERVAL {
        info!("串口 {} worker 正常运行但未发送: {}", port_name, reason);
        *last_log = Instant::now();
    }
}

fn log_worker_heartbeat(manager: &SerialManager, port_name: &str, last_log: &mut Instant) {
    if last_log.elapsed() < WORKER_HEARTBEAT_INTERVAL {
        return;
    }

    let device = manager.port_config.devices.get(manager.index);
    let device_id = device.map(|d| d.config.device_id).unwrap_or_default();
    let timeout_count = manager
        .device_states
        .get(manager.index)
        .map(|s| s.timeout_count)
        .unwrap_or_default();
    let queued_commands = manager
        .command_queue
        .lock()
        .map(|q| q.len())
        .unwrap_or_default();

    info!(
        "串口 {} worker 心跳: role={:?}, port_enabled={}, index={}, device_id={}, timeout_count={}, queued_commands={}",
        port_name,
        manager.current_run,
        manager.port_config.status,
        manager.index,
        device_id,
        timeout_count,
        queued_commands
    );
    *last_log = Instant::now();
}

/// 处理接收通道中的所有待处理命令。
fn process_incoming_commands(
    manager: &mut SerialManager,
    rx: &Receiver<Command>,
    system_state: &SystemState,
) {
    let mut processed = 0usize;
    while processed < MAX_COMMANDS_PER_CYCLE {
        let cmd = match rx.try_recv() {
            Ok(cmd) => cmd,
            Err(_) => return,
        };
        processed += 1;
        match cmd.command {
            CommandType::SendData(data) => {
                manager
                    .command_queue
                    .lock()
                    .unwrap_or_else(|e| {
                        warn!("命令队列锁中毒，恢复: {}", e);
                        e.into_inner()
                    })
                    .push_back(data);
            }
            CommandType::PortStatus(status) => {
                manager.port_config.status = status.device_status;
                if let Err(e) = system_state
                    .set_port_status(&manager.port_config.port_number, status.device_status)
                {
                    error!("设置端口状态失败: {}", e);
                }
            }
            CommandType::DeviceSetting(setting) => {
                for (i, dev) in manager.port_config.devices.iter().enumerate() {
                    if dev.config.device_id == setting.device_id {
                        manager.device_states[i].is_enabled = setting.device_status;
                    }
                }
                if let Err(e) = system_state.set_device_status(
                    &manager.port_config.port_number,
                    setting.device_id,
                    setting.device_status,
                ) {
                    error!("设置设备状态失败: {}", e);
                }
            }
        }
    }

    warn!(
        "串口 {} 单轮处理命令达到上限 {}，本轮先进入轮询，剩余命令下轮继续处理",
        manager.serial_config.port_name, MAX_COMMANDS_PER_CYCLE
    );
}

/// 主机模式的一次轮询周期。
///
/// 返回 `true` 表示已发送命令，需要接收响应；`false` 表示跳过本轮，不需要接收。
fn run_primary_cycle(manager: &mut SerialManager) -> bool {
    let mut queue = manager.command_queue.lock().unwrap_or_else(|e| {
        warn!("命令队列锁中毒，恢复: {}", e);
        e.into_inner()
    });
    if let Some(cmd) = queue.pop_front() {
        drop(queue);
        return manager.send_command(cmd);
    }
    drop(queue);

    if manager.port_config.devices.is_empty() {
        return false;
    }

    let idx = manager.index;

    // 设备超时过多时跳过若干轮
    if manager.device_states[idx].timeout_count > threshold::DEVICE_TIMEOUT_SKIP {
        if manager.device_states[idx].current_round > threshold::DEVICE_SKIP_ROUNDS {
            manager.device_states[idx].current_round = 0;
        } else {
            manager.device_states[idx].current_round += 1;
            std::thread::sleep(Duration::from_millis(10));
            return false;
        }
    }

    if !manager.device_states[idx].is_enabled {
        std::thread::sleep(Duration::from_millis(20));
        return false;
    }

    // 构造轮询查询命令
    let dev = &manager.port_config.devices[idx];
    let mut frame = vec![
        dev.config.device_id,
        cmd::READ_STATUS,
        cmd::READ_SUBCMD,
        cmd::STATUS_PAGE,
        cmd::STATUS_OFFSET,
        dev.config.data_len,
    ];
    frame.push(get_sum(&frame));

    manager.send_command(SendData {
        device_id: 1,
        command: frame,
    })
}

/// 备机模式的一次轮询周期。
///
/// 返回 `true` 表示备机始终需要监听数据（主机转发帧）。
fn run_secondary_cycle(manager: &mut SerialManager) -> bool {
    let mut queue = manager.command_queue.lock().unwrap_or_else(|e| {
        warn!("命令队列锁中毒，恢复: {}", e);
        e.into_inner()
    });
    if let Some(cmd) = queue.pop_front() {
        drop(queue);
        manager.send_command(cmd);
    }
    // 备机始终需要监听转发数据
    true
}
