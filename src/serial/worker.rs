//! 串口线程入口与轮询调度。
//!
//! 负责启动串口通信线程，以及主/备机模式的轮询逻辑。
//!
//! 使用 `tokio::task::spawn_blocking` 在主运行时的阻塞线程池中
//! 运行同步串口 I/O，通过 `Handle` 桥接异步操作。

use std::sync::mpsc::Receiver;
use std::sync::{Arc, Mutex};
use std::time::Duration;

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

// ============================================================
//  串口线程入口
// ============================================================

/// 启动串口通信工作线程。
///
/// 使用 `tokio::task::spawn_blocking` 在主运行时的阻塞线程池中运行，
/// 无需创建独立的 tokio `Runtime`。
pub fn spawn_serial_worker(
    broadcaster: Broadcaster,
    serial_config: SerialConfig,
    port_config: SerialPortConfig,
    rx: Receiver<Command>,
    software_config: config::Config,
    system_state: SystemState,
    system_record: Arc<Mutex<BoundedVecDeque<String>>>,
    cancel: CancellationToken,
) -> JoinHandle<()> {
    tokio::task::spawn_blocking(move || {
        let port_name = serial_config.port_name.clone();
        let mut manager = SerialManager::new(
            serial_config,
            port_config,
            broadcaster,
            software_config.server.run_on,
            software_config.server.current_run,
            system_record,
        );

        loop {
            if cancel.is_cancelled() {
                info!("串口 {} worker 收到取消信号，退出", port_name);
                return;
            }

            // 处理来自上层的命令
            process_incoming_commands(&mut manager, &rx, &system_state);

            // 串口被禁用时跳过轮询
            if !manager.port_config.status {
                std::thread::sleep(Duration::from_millis(100));
                continue;
            }

            // 无设备配置时跳过轮询
            if manager.port_config.devices.is_empty() {
                std::thread::sleep(Duration::from_millis(100));
                continue;
            }

            // 根据主/备角色决定行为，返回是否已发送命令（需要接收响应）
            let should_receive = match manager.current_run {
                RunLocation::Primary => run_primary_cycle(&mut manager),
                RunLocation::Secondary => run_secondary_cycle(&mut manager),
            };

            // 仅在实际发送了命令（或备机监听模式）时才接收数据
            if should_receive {
                manager.receive_data();
            }

            // 主机模式：轮询下一个设备；备机模式：index 由 extract_frame 从转发头解析
            if manager.current_run == RunLocation::Primary {
                manager.index = (manager.index + 1) % manager.port_config.devices.len();
            }
            std::thread::sleep(Duration::from_millis(1));
        }
    })
}

/// 处理接收通道中的所有待处理命令。
fn process_incoming_commands(
    manager: &mut SerialManager,
    rx: &Receiver<Command>,
    system_state: &SystemState,
) {
    while let Ok(cmd) = rx.try_recv() {
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
        manager.send_command(cmd);
        return true;
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
            manager.index = (manager.index + 1) % manager.port_config.devices.len();
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
    });
    true
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
