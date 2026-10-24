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
use log::{debug, error, info, warn};
use tokio::task::JoinHandle;
use tokio_util::sync::CancellationToken;

#[cfg(test)]
pub(super) use super::command_transaction::execute_command_with_ack;
use super::command_transaction::{execute_queued_command, send_ack_status};
use super::manager::{QueuedCommand, SerialConfig, SerialManager};
use super::port_config::SerialPortConfig;
use crate::broadcast::Broadcaster;
use crate::config;
use crate::config::RunLocation;
use crate::model::{
    get_sum, Command, CommandAckStatus, CommandAckUpdate, CommandType, SendData, SystemState,
};
use crate::operations::{CommandPermit, Operations};
use crate::protocol::{cmd, threshold};
use crate::runtime_cache::LatestStatusCache;

const WORKER_HEARTBEAT_INTERVAL: Duration = Duration::from_secs(30);
const MAX_COMMANDS_PER_CYCLE: usize = 1024;
const SLOW_RECEIVE_LOG_THRESHOLD: Duration = Duration::from_secs(2);

enum CycleResult {
    Idle,
    QuerySent(SendData),
    PassiveReceive,
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
    operations: Operations,
) -> JoinHandle<()> {
    tokio::task::spawn_blocking(move || {
        let port_name = serial_config.port_name.clone();
        let poll_interval = Duration::from_millis(software_config.settings.poll_interval_ms);
        let command_retry_count = software_config.settings.command_retry_count.max(1);
        let query_retry_count = software_config.settings.query_retry_count.min(1);
        let run_on = software_config.server.run_on.clone();
        let current_run = software_config.server.current_run.clone();
        let mut port_config = port_config;
        port_config.status = operations.initial_port_enabled(&port_name);
        let mut manager = SerialManager::new(
            serial_config,
            port_config,
            broadcaster,
            latest_status_cache,
            run_on,
            current_run,
            system_record,
        );
        manager.operations = Some(operations.clone());
        manager.command_pacing = super::command_pacing::CommandPacing::new(Duration::from_millis(
            software_config.settings.command_interval_ms,
        ));
        for index in 0..manager.port_config.devices.len() {
            let id = manager.port_config.devices[index].config.device_id;
            if !operations.initial_device_read_enabled(&port_name, id) {
                let _ = manager.set_device_read_enabled(id, false);
            }
        }
        manager.schedule_configured_domestic_version_queries();
        manager.refresh_operations_state();

        info!(
            "串口 {} worker 已启动，当前角色: {:?}，设备数: {}，端口启用: {}，控制命令间隔: {}ms",
            port_name,
            manager.current_run,
            manager.port_config.devices.len(),
            manager.port_config.status,
            software_config.settings.command_interval_ms,
        );

        let mut restart_count = 0u32;
        loop {
            let result = catch_unwind(AssertUnwindSafe(|| {
                run_worker_loop(
                    &mut manager,
                    &port_name,
                    &rx,
                    &ack_status_tx,
                    &system_state,
                    &cancel,
                    poll_interval,
                    command_retry_count,
                    query_retry_count,
                );
            }));

            match result {
                Ok(()) => break,
                Err(payload) => {
                    restart_count = restart_count.saturating_add(1);
                    let panic_text = panic_payload_text(payload.as_ref());
                    error!(
                        "[串口worker异常] port={} restart_count={} panic={:?} action=publish_all_offline_restart_worker",
                        port_name, restart_count, panic_text,
                    );
                    manager.log_record(format!(
                        "串口 worker 异常: restart_count={} panic={:?} action=publish_all_offline_restart_worker",
                        restart_count, panic_text
                    ));
                    manager.prepare_worker_restart();
                    operations.fail_pending_controls(
                        &port_name,
                        "serial worker panicked before confirmation",
                    );
                    operations.fail_inflight_station_queries(
                        &port_name,
                        "station version query interrupted by serial worker panic",
                    );
                    reject_queued_commands(&mut manager, None, &ack_status_tx, "worker_panic");
                    if cancel.is_cancelled() {
                        reject_incoming_commands(&manager, &rx, &ack_status_tx);
                        break;
                    }
                    std::thread::sleep(Duration::from_secs(1));
                    manager.reconnect();
                    manager.schedule_configured_domestic_version_queries();
                    manager.refresh_operations_state();
                    info!(
                        "[串口worker重启] port={} restart_count={} action=resume_polling",
                        port_name, restart_count
                    );
                }
            }
        }
        // Drop the physical handle before publishing stopped state.
        manager.port = None;
        operations.fail_inflight_station_queries(
            &port_name,
            "station version query interrupted by serial worker shutdown",
        );
        operations.worker_stopped(&port_name);
    })
}

#[allow(clippy::too_many_arguments)]
fn run_worker_loop(
    manager: &mut SerialManager,
    port_name: &str,
    rx: &Receiver<Command>,
    ack_status_tx: &Sender<CommandAckUpdate>,
    system_state: &SystemState,
    cancel: &CancellationToken,
    poll_interval: Duration,
    command_retry_count: u32,
    query_retry_count: u32,
) {
    let mut last_idle_log = Instant::now() - WORKER_HEARTBEAT_INTERVAL;
    let mut last_heartbeat_log = Instant::now() - WORKER_HEARTBEAT_INTERVAL;

    loop {
        log_worker_heartbeat(manager, port_name, &mut last_heartbeat_log);

        if cancel.is_cancelled() {
            info!("串口 {} worker 收到取消信号，退出", port_name);
            reject_queued_commands(manager, None, ack_status_tx, "worker_shutdown");
            reject_incoming_commands(manager, rx, ack_status_tx);
            return;
        }

        process_incoming_commands(manager, rx, system_state, ack_status_tx);
        manager.refresh_operations_state();

        if !manager.port_config.status {
            log_idle_reason(
                port_name,
                &mut last_idle_log,
                "端口被上层设置为禁用，暂停轮询发送",
            );
            std::thread::sleep(Duration::from_millis(100));
            continue;
        }

        if manager.port_config.devices.is_empty() {
            log_idle_reason(port_name, &mut last_idle_log, "没有设备配置，暂停轮询发送");
            std::thread::sleep(Duration::from_millis(100));
            continue;
        }

        let cycle_result = match manager.current_run {
            RunLocation::Primary => run_primary_cycle(manager, ack_status_tx, command_retry_count),
            RunLocation::Secondary => {
                run_secondary_cycle(manager, ack_status_tx, command_retry_count)
            }
        };

        if manager.current_run == RunLocation::Secondary {
            log_idle_reason(
                port_name,
                &mut last_idle_log,
                "当前是备机监听模式，不主动轮询发送",
            );
        } else if matches!(&cycle_result, CycleResult::Idle) {
            log_idle_reason(
                port_name,
                &mut last_idle_log,
                "本轮没有发送命令，通常是设备禁用或超时跳过窗口",
            );
        }

        let completed_normal_slot =
            matches!(&cycle_result, CycleResult::QuerySent(_) | CycleResult::Idle);
        let command_handled = matches!(&cycle_result, CycleResult::CommandHandled);
        match cycle_result {
            CycleResult::QuerySent(query) => {
                let device_id = manager
                    .port_config
                    .devices
                    .get(manager.index)
                    .map(|d| d.config.device_id)
                    .unwrap_or_default();
                debug!(
                    "串口 {} 进入查询接收: role={:?}, index={}, device_id={}",
                    port_name, manager.current_run, manager.index, device_id
                );
                let receive_started = Instant::now();
                manager.receive_query_with_retry(query, query_retry_count);
                let elapsed = receive_started.elapsed();
                if elapsed >= SLOW_RECEIVE_LOG_THRESHOLD {
                    debug!(
                        "串口 {} 查询收发耗时 {:?}，可能包含一次读取超时和补发",
                        port_name, elapsed
                    );
                }
            }
            CycleResult::PassiveReceive => {
                debug!(
                    "串口 {} 进入备机监听接收: role={:?}",
                    port_name, manager.current_run
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
            CycleResult::Idle | CycleResult::CommandHandled => {}
        }

        // A control transaction takes priority over the current acquisition
        // slot, but does not consume it. Advancing here would skip a station
        // whenever a queued command interrupts its turn (and alternating
        // commands could permanently starve that station).
        // 备机模式：index 由 extract_frame 从转发头解析。
        if manager.current_run == RunLocation::Primary && !command_handled {
            manager.index = (manager.index + 1) % manager.port_config.devices.len();
            if completed_normal_slot && manager.index == 0 {
                manager.execute_one_scheduled_station_version_query();
            }
        }
        manager.refresh_operations_state();
        std::thread::sleep(poll_interval);
    }
}

fn panic_payload_text(payload: &(dyn std::any::Any + Send)) -> String {
    if let Some(message) = payload.downcast_ref::<&str>() {
        (*message).to_string()
    } else if let Some(message) = payload.downcast_ref::<String>() {
        message.clone()
    } else {
        "non-string panic payload".to_string()
    }
}

fn log_idle_reason(port_name: &str, last_log: &mut Instant, reason: &str) {
    if last_log.elapsed() >= WORKER_HEARTBEAT_INTERVAL {
        debug!("串口 {} worker 正常运行但未发送: {}", port_name, reason);
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

    debug!(
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

/// Accepted commands may still be in the ingress channel rather than the
/// pacing FIFO when cancellation arrives. Reject them without applying port
/// settings or performing any serial I/O, and release their admission permits.
fn reject_incoming_commands(
    manager: &SerialManager,
    rx: &Receiver<Command>,
    ack_status_tx: &Sender<CommandAckUpdate>,
) {
    while let Ok(command) = rx.try_recv() {
        drop(command.permit);
        let report_ack = matches!(&command.command, CommandType::SendDataWithAck(_));
        if matches!(
            &command.command,
            CommandType::SendData(_) | CommandType::SendDataWithAck(_)
        ) {
            if let Some(operations) = &manager.operations {
                operations.command_tracker().update(
                    &manager.port_config.port_number,
                    command.ack_id,
                    |v| {
                        if !v.terminal() {
                            v.phase = "Failed".into();
                            v.error = "worker_shutdown_before_send".into();
                        }
                    },
                );
            }
            if report_ack {
                send_ack_status(
                    manager,
                    ack_status_tx,
                    command.ack_id,
                    CommandAckStatus::Failed,
                );
            }
        }
        if let Some(receipt) = command.control_receipt {
            let outcome = Err("serial worker shut down before applying control".into());
            if let Some(operations) = &manager.operations {
                operations.complete_control(
                    &manager.port_config.port_number,
                    receipt.revision,
                    &outcome,
                );
            }
            let _ = receipt.reply.send(outcome);
        }
    }
}

/// 处理接收通道中的所有待处理命令。
fn process_incoming_commands(
    manager: &mut SerialManager,
    rx: &Receiver<Command>,
    system_state: &SystemState,
    ack_status_tx: &Sender<CommandAckUpdate>,
) {
    let mut processed = 0usize;
    while processed < MAX_COMMANDS_PER_CYCLE {
        let cmd = match rx.try_recv() {
            Ok(cmd) => cmd,
            Err(_) => return,
        };
        processed += 1;
        let Command {
            command,
            ack_id,
            expires_at,
            control_receipt,
            permit,
            ..
        } = cmd;
        if let Some(receipt) = &control_receipt {
            if !manager.operations.as_ref().is_some_and(|operations| {
                operations.control_is_current(&manager.port_config.port_number, receipt.revision)
            }) {
                let _ = receipt.reply.send(Err(
                    "control request was superseded or invalidated; refresh status".into(),
                ));
                continue;
            }
        }
        let mut control_outcome = Ok(());
        match command {
            CommandType::SendData(data) => {
                enqueue_or_reject_command(
                    manager,
                    data,
                    false,
                    ack_id,
                    expires_at,
                    ack_status_tx,
                    permit,
                );
            }
            CommandType::SendDataWithAck(data) => {
                enqueue_or_reject_command(
                    manager,
                    data,
                    true,
                    ack_id,
                    expires_at,
                    ack_status_tx,
                    permit,
                );
            }
            CommandType::PortStatus(status) => {
                let previous = manager.port_config.status;
                control_outcome = manager.set_port_enabled(status.device_status);
                if previous != status.device_status {
                    if status.device_status {
                        manager.schedule_configured_domestic_version_queries();
                        info!(
                            "[串口已启用] port={} action=wait_for_valid_device_frames",
                            manager.serial_config.port_name
                        );
                        manager.log_record(
                            "串口已启用: action=wait_for_valid_device_frames".to_string(),
                        );
                    } else {
                        reject_queued_commands(manager, None, ack_status_tx, "port_disabled");
                    }
                }
                if let Err(e) = system_state
                    .set_port_status(&manager.port_config.port_number, status.device_status)
                {
                    error!("设置端口状态失败: {}", e);
                }
            }
            CommandType::DeviceSetting(setting) => {
                let matching_indexes: Vec<usize> = manager
                    .port_config
                    .devices
                    .iter()
                    .enumerate()
                    .filter_map(|(index, dev)| {
                        (dev.config.device_id == setting.device_id).then_some(index)
                    })
                    .collect();
                for index in matching_indexes {
                    let previous = manager.device_states[index].is_enabled;
                    manager.device_states[index].is_enabled = setting.device_status;
                    if previous != setting.device_status {
                        if setting.device_status {
                            if manager.operations.as_ref().is_some_and(|operations| {
                                operations.initial_module_type(
                                    &manager.port_config.port_number,
                                    setting.device_id,
                                ) == crate::model::ModuleType::Domestic
                            }) {
                                manager.schedule_station_version_query(setting.device_id);
                            }
                            info!(
                                "[串口设备已启用] port={} device_id={} index={} action=wait_for_valid_frame",
                                manager.serial_config.port_name,
                                setting.device_id,
                                index,
                            );
                            manager.log_record(format!(
                                "串口设备已启用: device_id={} index={} action=wait_for_valid_frame",
                                setting.device_id, index
                            ));
                        } else {
                            manager.publish_device_disabled(index);
                        }
                    }
                }
                if !setting.device_status {
                    reject_queued_commands(
                        manager,
                        Some(u32::from(setting.device_id)),
                        ack_status_tx,
                        "device_disabled",
                    );
                }
                if let Err(e) = system_state.set_device_status(
                    &manager.port_config.port_number,
                    setting.device_id,
                    setting.device_status,
                ) {
                    error!("设置设备状态失败: {}", e);
                }
            }
            CommandType::DeviceReadSetting(setting) => {
                control_outcome =
                    manager.set_device_read_enabled(setting.device_id, setting.read_enabled);
                if setting.read_enabled
                    && manager.operations.as_ref().is_some_and(|operations| {
                        operations.initial_module_type(
                            &manager.port_config.port_number,
                            setting.device_id,
                        ) == crate::model::ModuleType::Domestic
                    })
                {
                    manager.schedule_station_version_query(setting.device_id);
                }
            }
            CommandType::StationModuleTypeSetting(setting) => {
                if setting.module_type == crate::model::ModuleType::Domestic {
                    manager.schedule_station_version_query(setting.device_id);
                } else {
                    manager.cancel_scheduled_station_version_query(setting.device_id);
                }
            }
            CommandType::QueryStationVersion(query) => {
                let result = manager.perform_station_version_query(query.device_id);
                control_outcome = manager
                    .operations
                    .as_ref()
                    .ok_or_else(|| "operations state is unavailable".to_string())
                    .and_then(|operations| {
                        operations.report_station_query_result(
                            &manager.port_config.port_number,
                            query.device_id,
                            result,
                        )
                    });
            }
        }
        if let Some(receipt) = control_receipt {
            manager.refresh_operations_state();
            if let Some(operations) = &manager.operations {
                operations.complete_control(
                    &manager.port_config.port_number,
                    receipt.revision,
                    &control_outcome,
                );
            }
            let _ = receipt.reply.send(control_outcome);
        }
    }

    warn!(
        "串口 {} 单轮处理命令达到上限 {}，本轮先进入轮询，剩余命令下轮继续处理",
        manager.serial_config.port_name, MAX_COMMANDS_PER_CYCLE
    );
}

fn enqueue_or_reject_command(
    manager: &mut SerialManager,
    data: SendData,
    report_ack: bool,
    ack_id: Option<u64>,
    expires_at: Option<Instant>,
    ack_status_tx: &Sender<CommandAckUpdate>,
    permit: Option<CommandPermit>,
) {
    let configured_device_disabled = manager
        .port_config
        .devices
        .iter()
        .position(|device| u32::from(device.config.device_id) == data.device_id)
        .is_some_and(|index| !manager.device_states[index].is_enabled);
    let reject_reason = if !manager.port_config.status {
        Some("port_disabled")
    } else if configured_device_disabled {
        Some("device_disabled")
    } else {
        None
    };

    if let Some(reason) = reject_reason {
        drop(permit);
        if let Some(operations) = &manager.operations {
            operations
                .command_tracker()
                .update(&manager.port_config.port_number, ack_id, |v| {
                    v.phase = "Failed".into();
                    v.error = reason.into();
                });
        }
        warn!(
            "[串口命令拒绝] port={} device_id={} reason={} report_ack={} action=drop_command",
            manager.serial_config.port_name, data.device_id, reason, report_ack,
        );
        if report_ack {
            send_ack_status(manager, ack_status_tx, ack_id, CommandAckStatus::Failed);
        }
        return;
    }

    manager
        .command_queue
        .lock()
        .unwrap_or_else(|e| {
            warn!("命令队列锁中毒，恢复: {}", e);
            e.into_inner()
        })
        .push_back(QueuedCommand {
            data,
            report_ack,
            ack_id,
            expires_at,
            _permit: permit,
        });
}

/// 丢弃因端口或设备被禁用而不再允许执行的命令，并立即结束其 ACK 等待状态。
fn reject_queued_commands(
    manager: &mut SerialManager,
    target_device_id: Option<u32>,
    ack_status_tx: &Sender<CommandAckUpdate>,
    reason: &str,
) {
    let rejected = {
        let mut queue = manager.command_queue.lock().unwrap_or_else(|e| {
            warn!("命令队列锁中毒，恢复: {}", e);
            e.into_inner()
        });
        let mut retained = std::collections::VecDeque::with_capacity(queue.len());
        let mut rejected = Vec::new();
        while let Some(command) = queue.pop_front() {
            if target_device_id.is_none_or(|device_id| command.data.device_id == device_id) {
                rejected.push(command);
            } else {
                retained.push_back(command);
            }
        }
        *queue = retained;
        rejected
    };

    if rejected.is_empty() {
        return;
    }
    warn!(
        "[串口命令队列清理] port={} reason={} target_device_id={:?} dropped={} action=fail_pending_ack",
        manager.serial_config.port_name,
        reason,
        target_device_id,
        rejected.len(),
    );
    for command in rejected {
        drop(command._permit);
        if let Some(operations) = &manager.operations {
            operations.command_tracker().update(
                &manager.port_config.port_number,
                command.ack_id,
                |v| {
                    v.phase = "Failed".into();
                    v.error = reason.into();
                },
            );
        }
        if command.report_ack {
            send_ack_status(
                manager,
                ack_status_tx,
                command.ack_id,
                CommandAckStatus::Failed,
            );
        }
    }
}

/// 主机模式的一次轮询周期。
///
/// 返回本轮的接收动作，查询成功发送时保留原始帧用于可能的一次补发。
fn run_primary_cycle(
    manager: &mut SerialManager,
    ack_status_tx: &Sender<CommandAckUpdate>,
    command_retry_count: u32,
) -> CycleResult {
    if execute_next_ready_command(manager, ack_status_tx, command_retry_count).is_some() {
        return CycleResult::CommandHandled;
    }

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

    if !manager.device_states[idx].is_enabled || !manager.device_states[idx].read_enabled {
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

    debug!(
        "串口 {} 准备发送轮询: index={}, device_id={}, data_len={}",
        manager.serial_config.port_name, idx, device_id, data_len
    );

    let query = SendData {
        device_id: u32::from(device_id),
        command: frame,
    };
    manager.prepare_automatic_query(device_id, idx);
    let sent = manager.send_command(query.clone());

    debug!(
        "串口 {} 轮询发送完成: index={}, device_id={}, success={}",
        manager.serial_config.port_name, idx, device_id, sent
    );

    if sent {
        CycleResult::QuerySent(query)
    } else {
        CycleResult::Idle
    }
}

/// 备机模式的一次轮询周期。
///
/// 备机始终被动监听主机转发帧，不使用自动查询补发。
fn run_secondary_cycle(
    manager: &mut SerialManager,
    ack_status_tx: &Sender<CommandAckUpdate>,
    command_retry_count: u32,
) -> CycleResult {
    if execute_next_ready_command(manager, ack_status_tx, command_retry_count) == Some(true) {
        return CycleResult::CommandHandled;
    }
    // 备机始终需要监听转发数据
    CycleResult::PassiveReceive
}

/// A waiting control remains in the original FIFO, retaining its admission
/// permit. Returning None allows the caller to continue normal acquisition.
/// Expired heads bypass only the pacing check so they can fail without I/O.
fn execute_next_ready_command(
    manager: &mut SerialManager,
    ack_status_tx: &Sender<CommandAckUpdate>,
    command_retry_count: u32,
) -> Option<bool> {
    let now = Instant::now();
    let command = {
        let mut queue = manager.command_queue.lock().unwrap_or_else(|e| {
            warn!("命令队列锁中毒，恢复: {}", e);
            e.into_inner()
        });
        let head = queue.front()?;
        let expired = head.expires_at.is_some_and(|deadline| now >= deadline);
        if !expired && !manager.command_pacing.ready(now) {
            return None;
        }
        queue.pop_front()?
    };
    let report_ack = command.report_ack;
    let already_expired = command
        .expires_at
        .is_some_and(|deadline| Instant::now() >= deadline);
    execute_queued_command(manager, command, ack_status_tx, command_retry_count);
    if !already_expired {
        manager.command_pacing.completed(Instant::now());
    }
    Some(report_ack)
}

#[cfg(test)]
pub(super) fn test_secondary_cycle_is_passive(
    manager: &mut SerialManager,
    acknowledgements: &Sender<CommandAckUpdate>,
) -> bool {
    matches!(
        run_secondary_cycle(manager, acknowledgements, 1),
        CycleResult::PassiveReceive
    )
}

#[cfg(test)]
pub(super) fn test_cancelled_worker(
    manager: &mut SerialManager,
    receiver: &Receiver<Command>,
    state: &SystemState,
    acknowledgements: &Sender<CommandAckUpdate>,
) {
    let cancel = CancellationToken::new();
    cancel.cancel();
    test_worker_until_cancelled(manager, receiver, state, acknowledgements, &cancel);
}

#[cfg(test)]
pub(super) fn test_worker_until_cancelled(
    manager: &mut SerialManager,
    receiver: &Receiver<Command>,
    state: &SystemState,
    acknowledgements: &Sender<CommandAckUpdate>,
    cancel: &CancellationToken,
) {
    let port = manager.serial_config.port_name.clone();
    run_worker_loop(
        manager,
        &port,
        receiver,
        acknowledgements,
        state,
        cancel,
        Duration::ZERO,
        1,
        0,
    );
}

#[cfg(test)]
pub(super) fn process_test_commands(
    manager: &mut SerialManager,
    receiver: &Receiver<Command>,
    state: &SystemState,
    acknowledgements: &Sender<CommandAckUpdate>,
) {
    process_incoming_commands(manager, receiver, state, acknowledgements);
}

#[cfg(test)]
pub(super) fn test_primary_cycle_is_idle(
    manager: &mut SerialManager,
    acknowledgements: &Sender<CommandAckUpdate>,
) -> bool {
    matches!(
        run_primary_cycle(manager, acknowledgements, 1),
        CycleResult::Idle
    )
}
