//! 串口线程入口与轮询调度。
//!
//! 负责启动串口通信线程，以及主/备机模式的轮询逻辑。
//!
//! 使用 `tokio::task::spawn_blocking` 在主运行时的阻塞线程池中
//! 运行同步串口 I/O，通过 `Handle` 桥接异步操作。

use std::panic::{catch_unwind, AssertUnwindSafe};
use std::sync::mpsc::{Receiver, Sender};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use bounded_vec_deque::BoundedVecDeque;
use log::{error, info, warn};
use tokio::task::JoinHandle;
use tokio_util::sync::CancellationToken;

use super::manager::{QueuedCommand, SerialConfig, SerialManager};
use super::port_config::SerialPortConfig;
use crate::broadcast::Broadcaster;
use crate::config;
use crate::config::RunLocation;
use crate::model::{
    get_sum, Command, CommandAckStatus, CommandAckUpdate, CommandType, SendData, SystemState,
};
use crate::protocol::{cmd, threshold};
use crate::runtime_cache::LatestStatusCache;

const WORKER_HEARTBEAT_INTERVAL: Duration = Duration::from_secs(30);
const MAX_COMMANDS_PER_CYCLE: usize = 1024;
const SLOW_RECEIVE_LOG_THRESHOLD: Duration = Duration::from_secs(2);

enum CycleResult {
    Idle,
    NormalReceive,
    CommandHandled,
}

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
    ack_status_tx: Sender<CommandAckUpdate>,
    software_config: config::Config,
    system_state: SystemState,
    system_record: Arc<Mutex<BoundedVecDeque<String>>>,
    latest_status_cache: LatestStatusCache,
    cancel: CancellationToken,
) -> JoinHandle<()> {
    tokio::task::spawn_blocking(move || {
        let port_name = serial_config.port_name.clone();
        let result = catch_unwind(AssertUnwindSafe(|| {
            let poll_interval = Duration::from_millis(software_config.settings.poll_interval_ms);
            let command_retry_count = software_config.settings.command_retry_count.max(1);
            let run_on = software_config.server.run_on.clone();
            let current_run = software_config.server.current_run.clone();
            let mut manager = SerialManager::new(
                serial_config,
                port_config,
                broadcaster,
                latest_status_cache,
                run_on,
                current_run,
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

                // 根据主/备角色决定行为。
                let cycle_result = match manager.current_run {
                    RunLocation::Primary => {
                        run_primary_cycle(&mut manager, &ack_status_tx, command_retry_count)
                    }
                    RunLocation::Secondary => {
                        run_secondary_cycle(&mut manager, &ack_status_tx, command_retry_count)
                    }
                };

                if manager.current_run == RunLocation::Secondary {
                    log_idle_reason(
                        &port_name,
                        &mut last_idle_log,
                        "当前是备机监听模式，不主动轮询发送",
                    );
                } else if matches!(cycle_result, CycleResult::Idle) {
                    log_idle_reason(
                        &port_name,
                        &mut last_idle_log,
                        "本轮没有发送命令，通常是设备禁用或超时跳过窗口",
                    );
                }

                // 仅在实际发送了命令（或备机监听模式）时才接收数据
                if matches!(cycle_result, CycleResult::NormalReceive) {
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
                std::thread::sleep(poll_interval);
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
        let Command {
            command, ack_id, ..
        } = cmd;
        match command {
            CommandType::SendData(data) => {
                manager
                    .command_queue
                    .lock()
                    .unwrap_or_else(|e| {
                        warn!("命令队列锁中毒，恢复: {}", e);
                        e.into_inner()
                    })
                    .push_back(QueuedCommand {
                        data,
                        report_ack: false,
                        ack_id,
                    });
            }
            CommandType::SendDataWithAck(data) => {
                manager
                    .command_queue
                    .lock()
                    .unwrap_or_else(|e| {
                        warn!("命令队列锁中毒，恢复: {}", e);
                        e.into_inner()
                    })
                    .push_back(QueuedCommand {
                        data,
                        report_ack: true,
                        ack_id,
                    });
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
fn run_primary_cycle(
    manager: &mut SerialManager,
    ack_status_tx: &Sender<CommandAckUpdate>,
    command_retry_count: u32,
) -> CycleResult {
    let mut queue = manager.command_queue.lock().unwrap_or_else(|e| {
        warn!("命令队列锁中毒，恢复: {}", e);
        e.into_inner()
    });
    if let Some(cmd) = queue.pop_front() {
        drop(queue);
        execute_queued_command(manager, cmd, ack_status_tx, command_retry_count);
        return CycleResult::CommandHandled;
    }
    drop(queue);

    if manager.port_config.devices.is_empty() {
        return CycleResult::Idle;
    }

    let idx = manager.index;

    // 设备超时过多时跳过若干轮
    if manager.device_states[idx].timeout_count > threshold::DEVICE_TIMEOUT_SKIP {
        if manager.device_states[idx].current_round > threshold::DEVICE_SKIP_ROUNDS {
            manager.device_states[idx].current_round = 0;
        } else {
            manager.device_states[idx].current_round += 1;
            std::thread::sleep(Duration::from_millis(10));
            return CycleResult::Idle;
        }
    }

    if !manager.device_states[idx].is_enabled {
        std::thread::sleep(Duration::from_millis(20));
        return CycleResult::Idle;
    }

    // 构造轮询查询命令
    let dev = &manager.port_config.devices[idx];
    let device_id = dev.config.device_id;
    let data_len = dev.config.data_len;
    let mut frame = vec![
        device_id,
        cmd::READ_STATUS,
        cmd::READ_SUBCMD,
        cmd::STATUS_PAGE,
        cmd::STATUS_OFFSET,
        data_len,
    ];
    frame.push(get_sum(&frame));

    info!(
        "串口 {} 准备发送轮询: index={}, device_id={}, data_len={}",
        manager.serial_config.port_name, idx, device_id, data_len
    );

    let sent = manager.send_command(SendData {
        device_id: 1,
        command: frame,
    });

    info!(
        "串口 {} 轮询发送完成: index={}, device_id={}, success={}",
        manager.serial_config.port_name, idx, device_id, sent
    );

    if sent {
        CycleResult::NormalReceive
    } else {
        CycleResult::Idle
    }
}

/// 备机模式的一次轮询周期。
///
/// 返回 `true` 表示备机始终需要监听数据（主机转发帧）。
fn run_secondary_cycle(
    manager: &mut SerialManager,
    ack_status_tx: &Sender<CommandAckUpdate>,
    command_retry_count: u32,
) -> CycleResult {
    let mut queue = manager.command_queue.lock().unwrap_or_else(|e| {
        warn!("命令队列锁中毒，恢复: {}", e);
        e.into_inner()
    });
    if let Some(cmd) = queue.pop_front() {
        drop(queue);
        if cmd.report_ack {
            execute_queued_command(manager, cmd, ack_status_tx, command_retry_count);
            return CycleResult::CommandHandled;
        }
        manager.send_command(cmd.data);
    }
    // 备机始终需要监听转发数据
    CycleResult::NormalReceive
}

fn execute_queued_command(
    manager: &mut SerialManager,
    command: QueuedCommand,
    ack_status_tx: &Sender<CommandAckUpdate>,
    command_retry_count: u32,
) -> bool {
    if !command.report_ack {
        return manager.send_command(command.data);
    }

    execute_command_with_ack(
        manager,
        command.data,
        command.ack_id,
        ack_status_tx,
        command_retry_count,
    )
}

fn execute_command_with_ack(
    manager: &mut SerialManager,
    command: SendData,
    ack_id: Option<u64>,
    ack_status_tx: &Sender<CommandAckUpdate>,
    command_retry_count: u32,
) -> bool {
    let attempts = command_retry_count.max(1);
    for attempt in 1..=attempts {
        info!(
            "串口 {} 下发命令 ACK 尝试 {}/{}",
            manager.serial_config.port_name, attempt, attempts
        );
        if !manager.send_command(command.clone()) {
            send_ack_status(manager, ack_status_tx, ack_id, CommandAckStatus::Failed);
            return false;
        }

        send_ack_status(manager, ack_status_tx, ack_id, CommandAckStatus::Waiting);
        if manager.receive_command_ack() {
            send_ack_status(manager, ack_status_tx, ack_id, CommandAckStatus::Received);
            return true;
        }
    }

    send_ack_status(manager, ack_status_tx, ack_id, CommandAckStatus::Failed);
    false
}

fn send_ack_status(
    manager: &SerialManager,
    ack_status_tx: &Sender<CommandAckUpdate>,
    ack_id: Option<u64>,
    status: CommandAckStatus,
) {
    if let Err(e) = ack_status_tx.send(CommandAckUpdate {
        com: manager.port_config.port_number.clone(),
        ack_id,
        status,
    }) {
        warn!(
            "串口 {} 命令 ACK 状态回写通道发送失败: {:?}",
            manager.serial_config.port_name, e
        );
    }
}
