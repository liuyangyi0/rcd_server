//! 串口通信管理器。
//!
//! 负责串口的打开、重连、数据收发、协议解析以及将解析结果分发给上层订阅者。
//!
//! 串口 I/O 是同步阻塞操作，运行在 `tokio::task::spawn_blocking` 线程池中。
//! 需要执行异步操作（广播、日志、延时）时，通过持有的 `tokio::runtime::Handle`
//! 调用 `block_on` 桥接到主运行时。

use std::collections::VecDeque;
use std::io::{ErrorKind, Write};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use bounded_vec_deque::BoundedVecDeque;
use chrono::{DateTime, Local};
use log::{error, info, warn};
use serialport::{ClearBuffer, DataBits, Parity, SerialPort, StopBits};

use super::port_config::SerialPortConfig;
use crate::broadcast::Broadcaster;
use crate::config::RunLocation;
use crate::logging;
use crate::model::{DeviceStatus, SendData};
use crate::protocol::{self, cmd, threshold};
use crate::runtime_cache::LatestStatusCache;

const COMMAND_ACK_BYTES: [u8; 3] = [0x00, 0x50, 0x50];
const FORWARD_HEADER_LEN: usize = 7;
const FORWARD_SEPARATOR_LEN: usize = 2;
const FORWARD_PREFIX_LEN: usize = FORWARD_HEADER_LEN + FORWARD_SEPARATOR_LEN;
const FORWARD_PREFIX_GRACE: Duration = Duration::from_millis(5);
const FAILURE_LOG_SUMMARY_INTERVAL: Duration = Duration::from_secs(60);
const BAD_FRAME_LOG_MAX_BYTES: usize = 64;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum DeviceFailureKind {
    ReadTimeout,
    BadFrame,
}

#[derive(Clone, Debug, PartialEq, Eq)]
enum QueryFailure {
    ReadTimeout,
    BadFrame { reason: String, frame: Vec<u8> },
}

impl QueryFailure {
    fn kind(&self) -> DeviceFailureKind {
        match self {
            Self::ReadTimeout => DeviceFailureKind::ReadTimeout,
            Self::BadFrame { .. } => DeviceFailureKind::BadFrame,
        }
    }

    fn cause_name(&self) -> &'static str {
        device_failure_kind_name(self.kind())
    }

    fn reason(&self) -> Option<&str> {
        match self {
            Self::ReadTimeout => None,
            Self::BadFrame { reason, .. } => Some(reason),
        }
    }

    fn frame(&self) -> Option<&[u8]> {
        match self {
            Self::ReadTimeout => None,
            Self::BadFrame { frame, .. } => Some(frame),
        }
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
struct QueryExpectation {
    device_id: u8,
    command: u8,
}

#[derive(Debug, PartialEq, Eq)]
enum ReceiveDataOutcome {
    Success,
    Retryable(QueryFailure),
    TransportHandled,
    UnattributedHandled,
}

#[derive(Clone, Debug)]
struct QueryFailureContext {
    attempts: u8,
    first_failure: QueryFailure,
}

#[derive(Clone, Debug, PartialEq, Eq)]
struct QueryRetryEpisodeStats {
    duration: Duration,
    retries_succeeded: u32,
    first_timeouts: u32,
    first_bad_frames: u32,
    suppressed: u32,
}

#[derive(Clone, Debug, PartialEq, Eq)]
enum QueryRetryLogAction {
    Immediate(QueryRetryEpisodeStats),
    Summary(QueryRetryEpisodeStats),
    Suppressed,
}

#[derive(Debug, Default)]
struct QueryRetryLogState {
    started_at: Option<Instant>,
    last_log_at: Option<Instant>,
    retries_succeeded: u32,
    first_timeouts: u32,
    first_bad_frames: u32,
    suppressed_since_summary: u32,
    timeout_logged: bool,
    bad_frame_logged: bool,
}

impl QueryRetryLogState {
    fn record_retry_success(
        &mut self,
        first_failure: DeviceFailureKind,
        now: Instant,
    ) -> QueryRetryLogAction {
        if self.started_at.is_none() {
            self.started_at = Some(now);
            self.last_log_at = Some(now);
        }
        self.retries_succeeded = self.retries_succeeded.saturating_add(1);

        let first_of_kind = match first_failure {
            DeviceFailureKind::ReadTimeout => {
                self.first_timeouts = self.first_timeouts.saturating_add(1);
                let first = !self.timeout_logged;
                self.timeout_logged = true;
                first
            }
            DeviceFailureKind::BadFrame => {
                self.first_bad_frames = self.first_bad_frames.saturating_add(1);
                let first = !self.bad_frame_logged;
                self.bad_frame_logged = true;
                first
            }
        };

        if first_of_kind {
            self.last_log_at = Some(now);
            return QueryRetryLogAction::Immediate(self.stats(now));
        }

        if self
            .last_log_at
            .is_some_and(|last| now.saturating_duration_since(last) >= FAILURE_LOG_SUMMARY_INTERVAL)
        {
            let stats = self.stats(now);
            self.last_log_at = Some(now);
            self.suppressed_since_summary = 0;
            QueryRetryLogAction::Summary(stats)
        } else {
            self.suppressed_since_summary = self.suppressed_since_summary.saturating_add(1);
            QueryRetryLogAction::Suppressed
        }
    }

    fn recover_on_first_attempt(&mut self, now: Instant) -> Option<QueryRetryEpisodeStats> {
        self.started_at?;
        let stats = self.stats(now);
        *self = Self::default();
        Some(stats)
    }

    fn stats(&self, now: Instant) -> QueryRetryEpisodeStats {
        QueryRetryEpisodeStats {
            duration: self
                .started_at
                .map(|started| now.saturating_duration_since(started))
                .unwrap_or_default(),
            retries_succeeded: self.retries_succeeded,
            first_timeouts: self.first_timeouts,
            first_bad_frames: self.first_bad_frames,
            suppressed: self.suppressed_since_summary,
        }
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
struct FailureEpisodeStats {
    duration: Duration,
    timeouts: u32,
    bad_frames: u32,
    suppressed: u32,
}

impl FailureEpisodeStats {
    fn total_failures(&self) -> u32 {
        self.timeouts.saturating_add(self.bad_frames)
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
enum FailureLogAction {
    Immediate(FailureEpisodeStats),
    Summary(FailureEpisodeStats),
    Suppressed,
}

#[derive(Debug, Default)]
struct DeviceFailureLogState {
    started_at: Option<Instant>,
    last_log_at: Option<Instant>,
    timeouts: u32,
    bad_frames: u32,
    suppressed_since_summary: u32,
    timeout_logged: bool,
    bad_frame_logged: bool,
}

impl DeviceFailureLogState {
    fn record_failure(&mut self, kind: DeviceFailureKind, now: Instant) -> FailureLogAction {
        if self.started_at.is_none() {
            self.started_at = Some(now);
            self.last_log_at = Some(now);
        }

        let first_of_kind = match kind {
            DeviceFailureKind::ReadTimeout => {
                self.timeouts = self.timeouts.saturating_add(1);
                let first = !self.timeout_logged;
                self.timeout_logged = true;
                first
            }
            DeviceFailureKind::BadFrame => {
                self.bad_frames = self.bad_frames.saturating_add(1);
                let first = !self.bad_frame_logged;
                self.bad_frame_logged = true;
                first
            }
        };

        if first_of_kind {
            self.last_log_at = Some(now);
            return FailureLogAction::Immediate(self.stats(now));
        }

        if self
            .last_log_at
            .is_some_and(|last| now.saturating_duration_since(last) >= FAILURE_LOG_SUMMARY_INTERVAL)
        {
            let stats = self.stats(now);
            self.last_log_at = Some(now);
            self.suppressed_since_summary = 0;
            FailureLogAction::Summary(stats)
        } else {
            self.suppressed_since_summary = self.suppressed_since_summary.saturating_add(1);
            FailureLogAction::Suppressed
        }
    }

    fn recover(&mut self, now: Instant) -> Option<FailureEpisodeStats> {
        self.started_at?;
        let stats = self.stats(now);
        *self = Self::default();
        Some(stats)
    }

    fn stats(&self, now: Instant) -> FailureEpisodeStats {
        FailureEpisodeStats {
            duration: self
                .started_at
                .map(|started| now.saturating_duration_since(started))
                .unwrap_or_default(),
            timeouts: self.timeouts,
            bad_frames: self.bad_frames,
            suppressed: self.suppressed_since_summary,
        }
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
struct ReconnectEpisodeStats {
    duration: Duration,
    attempts: u32,
    suppressed: u32,
    last_error: String,
}

#[derive(Clone, Debug, PartialEq, Eq)]
enum ReconnectLogAction {
    Immediate(ReconnectEpisodeStats),
    Summary(ReconnectEpisodeStats),
    Suppressed,
}

#[derive(Debug, Default)]
struct ReconnectFailureLogState {
    started_at: Option<Instant>,
    last_log_at: Option<Instant>,
    attempts: u32,
    suppressed_since_summary: u32,
    last_error: String,
}

impl ReconnectFailureLogState {
    fn record_failure(&mut self, error: &str, now: Instant) -> ReconnectLogAction {
        let first_failure = self.started_at.is_none();
        if first_failure {
            self.started_at = Some(now);
            self.last_log_at = Some(now);
        }
        self.attempts = self.attempts.saturating_add(1);

        let error_changed = !first_failure && self.last_error != error;
        self.last_error.clear();
        self.last_error.push_str(error);

        if first_failure || error_changed {
            self.last_log_at = Some(now);
            return ReconnectLogAction::Immediate(self.stats(now));
        }

        if self
            .last_log_at
            .is_some_and(|last| now.saturating_duration_since(last) >= FAILURE_LOG_SUMMARY_INTERVAL)
        {
            let stats = self.stats(now);
            self.last_log_at = Some(now);
            self.suppressed_since_summary = 0;
            ReconnectLogAction::Summary(stats)
        } else {
            self.suppressed_since_summary = self.suppressed_since_summary.saturating_add(1);
            ReconnectLogAction::Suppressed
        }
    }

    fn recover(&mut self, now: Instant) -> Option<ReconnectEpisodeStats> {
        self.started_at?;
        let stats = self.stats(now);
        *self = Self::default();
        Some(stats)
    }

    fn stats(&self, now: Instant) -> ReconnectEpisodeStats {
        ReconnectEpisodeStats {
            duration: self
                .started_at
                .map(|started| now.saturating_duration_since(started))
                .unwrap_or_default(),
            attempts: self.attempts,
            suppressed: self.suppressed_since_summary,
            last_error: self.last_error.clone(),
        }
    }
}

// ============================================================
//  串口参数
// ============================================================

/// 串口物理参数。
pub struct SerialConfig {
    pub port_name: String,
    pub baud_rate: u32,
    pub data_bits: DataBits,
    pub stop_bits: StopBits,
    pub parity: Parity,
    pub read_timeout: Duration,
}

impl SerialConfig {
    pub fn new(
        port_name: String,
        baud_rate: u32,
        data_bits: DataBits,
        stop_bits: StopBits,
        parity: Parity,
        read_timeout: Duration,
    ) -> Self {
        Self {
            port_name,
            baud_rate,
            data_bits,
            stop_bits,
            parity,
            read_timeout,
        }
    }
}

// ============================================================
//  设备运行时状态
// ============================================================

/// 单个设备的运行时状态（与 SerialPortConfig::devices 一一对应）。
pub(crate) struct DeviceRuntimeState {
    /// 连续超时次数。
    pub(crate) timeout_count: u32,
    /// 当前轮询轮次（超时后用于跳过若干轮）。
    pub(crate) current_round: u32,
    /// 连续解析失败次数。
    pub(crate) parse_fail_count: u32,
    /// 上次收到的原始数据。
    last_data: Vec<u8>,
    /// 上次广播的串口在线状态。
    last_com_status: Option<bool>,
    /// 上次广播的设备在线状态。
    last_device_status: Option<bool>,
    /// 连续收到相同数据的次数。
    same_count: u32,
    /// 是否处于轮询读取状态。
    pub(crate) is_enabled: bool,
    /// 当前设备通信故障周期的限频日志状态。
    failure_log: DeviceFailureLogState,
    /// 查询需要补发才能成功时的限频日志状态。
    query_retry_log: QueryRetryLogState,
}

impl DeviceRuntimeState {
    fn new() -> Self {
        Self {
            timeout_count: 0,
            current_round: 0,
            parse_fail_count: 0,
            last_data: Vec::new(),
            last_com_status: None,
            last_device_status: None,
            same_count: 0,
            is_enabled: true,
            failure_log: DeviceFailureLogState::default(),
            query_retry_log: QueryRetryLogState::default(),
        }
    }

    /// 判断新数据是否应该推送给上层。
    ///
    /// 规则：数据或在线状态变化时立即推送；连续完全相同时按阈值定期推送。
    pub(crate) fn should_broadcast(
        &mut self,
        data: &[u8],
        com_status: bool,
        device_status: bool,
    ) -> bool {
        let status_changed = self.last_com_status != Some(com_status)
            || self.last_device_status != Some(device_status);

        if data == self.last_data && !status_changed {
            self.same_count += 1;
            if self.same_count >= threshold::SAME_DATA_BROADCAST {
                self.same_count = 0;
                true
            } else {
                false
            }
        } else {
            self.same_count = 0;
            self.last_data = data.to_vec();
            self.last_com_status = Some(com_status);
            self.last_device_status = Some(device_status);
            true
        }
    }
}

// ============================================================
//  串口管理器
// ============================================================

/// 管理单个串口上所有设备的轮询与数据收发。
pub(crate) struct QueuedCommand {
    pub(crate) data: SendData,
    pub(crate) report_ack: bool,
    pub(crate) ack_id: Option<u64>,
}

pub(crate) struct SerialManager {
    pub(crate) port: Option<Box<dyn SerialPort>>,
    pub(crate) command_queue: Arc<Mutex<VecDeque<QueuedCommand>>>,
    pub(crate) serial_config: SerialConfig,
    pub(crate) port_config: SerialPortConfig,
    pub(crate) broadcaster: Broadcaster,
    pub(crate) latest_status_cache: LatestStatusCache,
    /// 每个设备的运行时状态（与 port_config.devices 一一对应）。
    pub(crate) device_states: Vec<DeviceRuntimeState>,
    /// 当前轮询的设备索引。
    pub(crate) index: usize,
    pub(crate) run_on: RunLocation,
    pub(crate) current_run: RunLocation,
    /// 整个串口连续超时次数。
    pub(crate) time_out_count: u32,
    /// 整个串口连续解析失败次数。
    pub(crate) parse_fail_count: u32,
    pub(crate) system_record: Arc<Mutex<BoundedVecDeque<String>>>,
    /// 复用的读取缓冲区，避免每次 receive_data 堆分配。
    read_buf: Vec<u8>,
    /// 上次重连尝试的时间，用于冷却控制。
    last_reconnect: Instant,
    /// 串口打开/重连故障周期的限频日志状态。
    reconnect_failure_log: ReconnectFailureLogState,
    /// 备机无法归属帧的端口级限频日志状态。
    unattributed_failure_log: DeviceFailureLogState,
}

impl SerialManager {
    /// 创建管理器并尝试打开串口。
    pub(crate) fn new(
        serial_config: SerialConfig,
        port_config: SerialPortConfig,
        broadcaster: Broadcaster,
        latest_status_cache: LatestStatusCache,
        run_on: RunLocation,
        current_run: RunLocation,
        system_record: Arc<Mutex<BoundedVecDeque<String>>>,
    ) -> Self {
        // 首次启动等待设备就绪
        std::thread::sleep(Duration::from_millis(2000));

        let opened = open_serial_port(
            &serial_config.port_name,
            serial_config.baud_rate,
            serial_config.data_bits,
            serial_config.stop_bits,
            serial_config.parity,
            serial_config.read_timeout,
        );

        let mut reconnect_failure_log = ReconnectFailureLogState::default();
        let (port, initial_open_error) = match opened {
            Ok(port) => (Some(port), None),
            Err(error) => {
                let error_text = format!("{error:?}");
                let _ = reconnect_failure_log.record_failure(&error_text, Instant::now());
                error!(
                    "[串口打开失败] port={} baud_rate={} affected_devices={} error={:?}",
                    serial_config.port_name,
                    serial_config.baud_rate,
                    port_config.devices.len(),
                    error_text,
                );
                (None, Some(error_text))
            }
        };

        let device_states = port_config
            .devices
            .iter()
            .map(|_| DeviceRuntimeState::new())
            .collect();

        let manager = Self {
            port,
            command_queue: Arc::new(Mutex::new(VecDeque::new())),
            serial_config,
            port_config,
            broadcaster,
            latest_status_cache,
            device_states,
            index: 0,
            run_on,
            current_run,
            time_out_count: 0,
            parse_fail_count: 0,
            system_record,
            read_buf: vec![0u8; 1024],
            last_reconnect: Instant::now(),
            reconnect_failure_log,
            unattributed_failure_log: DeviceFailureLogState::default(),
        };

        if let Some(error) = initial_open_error {
            manager.log_record(format!("串口打开失败: error={error:?}"));
        }

        manager
    }

    // ---- 连接管理 ----

    /// 尝试重新打开串口；失败时发送全零状态。
    ///
    /// 内置 2 秒冷却：距上次重连不足 2 秒则跳过，避免频繁重连阻塞线程。
    pub(crate) fn reconnect(&mut self) {
        const RECONNECT_COOLDOWN: Duration = Duration::from_secs(2);

        if self.last_reconnect.elapsed() < RECONNECT_COOLDOWN {
            return;
        }
        let now = Instant::now();
        self.last_reconnect = now;

        match open_serial_port(
            &self.serial_config.port_name,
            self.serial_config.baud_rate,
            self.serial_config.data_bits,
            self.serial_config.stop_bits,
            self.serial_config.parity,
            self.serial_config.read_timeout,
        ) {
            Ok(port) => {
                self.port = Some(port);
                if let Some(stats) = self.reconnect_failure_log.recover(now) {
                    let message = format!(
                        "串口重新连接成功: duration_ms={} attempts={} last_error={:?}",
                        stats.duration.as_millis(),
                        stats.attempts,
                        stats.last_error,
                    );
                    info!(
                        "[串口重新连接成功] port={} duration_ms={} attempts={} suppressed={} last_error={:?}",
                        self.serial_config.port_name,
                        stats.duration.as_millis(),
                        stats.attempts,
                        stats.suppressed,
                        stats.last_error,
                    );
                    self.log_record(message);
                } else {
                    info!("[串口重新连接成功] port={}", self.serial_config.port_name);
                }
            }
            Err(e) => {
                let error_text = format!("{e:?}");
                match self
                    .reconnect_failure_log
                    .record_failure(&error_text, now)
                {
                    ReconnectLogAction::Immediate(stats) => {
                        error!(
                            "[串口重连失败] port={} attempts={} duration_ms={} error={:?}",
                            self.serial_config.port_name,
                            stats.attempts,
                            stats.duration.as_millis(),
                            stats.last_error,
                        );
                        if stats.attempts == 1 {
                            self.log_record(format!(
                                "串口重连失败: attempts=1 error={:?}",
                                stats.last_error
                            ));
                        }
                    }
                    ReconnectLogAction::Summary(stats) => warn!(
                        "[串口重连持续失败] port={} duration_s={} attempts={} suppressed={} last_error={:?}",
                        self.serial_config.port_name,
                        stats.duration.as_secs(),
                        stats.attempts,
                        stats.suppressed,
                        stats.last_error,
                    ),
                    ReconnectLogAction::Suppressed => {}
                }
                self.send_zero_status(false, false);
                self.port = None;
            }
        }
    }

    /// 发送全零数据状态给所有订阅者。
    pub(crate) fn send_zero_status(&mut self, com_status: bool, device_status: bool) {
        let dev = &self.port_config.devices[self.index];
        let zero_data = vec![0u8; dev.config.data_len as usize];
        let parsed = protocol::parse_status(&zero_data, &dev.records);
        let status = DeviceStatus {
            id: dev.config.com_index as u64,
            device_id: dev.config.device_id,
            com: self.port_config.port_number.clone(),
            com_status,
            device_status,
            value: parsed,
            raw_data: zero_data.clone(),
        };
        self.latest_status_cache.update(status.clone());

        if self.device_states[self.index].should_broadcast(&zero_data, com_status, device_status) {
            self.broadcaster.broadcast(status);
        }
    }

    /// 记录一条系统日志。
    pub(crate) fn log_record(&self, msg: String) {
        let local: DateTime<Local> = Local::now();
        let mut record = self.system_record.lock().unwrap_or_else(|e| {
            warn!("system_record 锁中毒，恢复: {}", e);
            e.into_inner()
        });
        record.push_back(format!(
            "串口 {} 时间 {} {}",
            self.serial_config.port_name,
            local.format("%Y-%m-%d %H:%M:%S"),
            msg,
        ));
    }

    // ---- 数据发送 ----

    /// 向设备发送命令帧。
    pub(crate) fn send_command(&mut self, command: SendData) -> bool {
        if command.command.is_empty() {
            return false;
        }
        //info!("串口 {} 发送数据: {}", self.serial_config.port_name, format_hex(&command.command));
        // println!(
        //     "串口 {} 发送数据: {}",
        //     self.serial_config.port_name,
        //     format_hex(&command.command)
        // );
        if logging::serial_frame_logs_enabled() {
            info!(
                "串口 {} 发送数据: {}",
                self.serial_config.port_name,
                format_hex(&command.command)
            );
        }

        // 特殊串口：直接写入全部数据
        if self.port_config.is_special {
            match self.port {
                Some(ref mut port) => {
                    if let Err(e) = port.write_all(&command.command) {
                        error!("写入错误: {:?}", e);
                        self.port = None;
                        self.reconnect();
                        return false;
                    }
                    // if let Err(e) = port.flush() {
                    //     error!("刷新串口输出错误: {:?}", e);
                    //     self.port = None;
                    //     self.reconnect();
                    //     return false;
                    // }
                    return true;
                }
                None => {
                    self.reconnect();
                    return false;
                }
            }
        }

        // 普通串口：地址字节使用 Mark 校验，数据部分使用 Space 校验
        let addr_byte = [command.command[0]];
        let payload = &command.command[1..];

        // 发送地址字节（Mark 奇偶校验）
        match self.port {
            Some(ref mut port) => {
                if let Err(e) = port.set_parity(Parity::Mark) {
                    error!("设置 Mark 校验失败: {:?}", e);
                    self.port = None;
                    self.reconnect();
                    return false;
                }
                std::thread::sleep(Duration::from_millis(25));
                if let Err(e) = port.write_all(&addr_byte) {
                    warn!("写入地址字节错误: {:?}", e);
                    self.port = None;
                    self.reconnect();
                    return false;
                }
                // if let Err(e) = port.flush() {
                //     error!("刷新地址字节错误: {:?}", e);
                //     self.port = None;
                //     self.reconnect();
                //     return false;
                // }
            }
            None => {
                self.reconnect();
                return false;
            }
        }

        // 等待地址字节在物理线路上完全发出后再切换校验位
        std::thread::sleep(Duration::from_millis(10));

        // 发送数据部分（Space 奇偶校验）
        match self.port {
            Some(ref mut port) => {
                if let Err(e) = port.set_parity(Parity::Space) {
                    error!("设置 Space 校验失败: {:?}", e);
                    self.port = None;
                    self.reconnect();
                    return false;
                }
                if let Err(e) = port.write_all(payload) {
                    error!("写入数据部分错误: {:?}", e);
                    self.port = None;
                    self.reconnect();
                    return false;
                }
                // if let Err(e) = port.flush() {
                //     error!("刷新数据部分错误: {:?}", e);
                //     self.port = None;
                //     self.reconnect();
                //     return false;
                // }
            }
            None => {
                self.reconnect();
                return false;
            }
        }

        // 等待数据在物理线路上完全发出后再恢复校验位
        // std::thread::sleep(Duration::from_millis(2));

        // 恢复原始校验位，供后续 receive_data 使用
        // self.restore_parity();
        true
    }

    // 将串口校验位恢复为配置文件中指定的原始值。
    // fn restore_parity(&mut self) {
    //     if let Some(ref mut port) = self.port {
    //         if let Err(e) = port.set_parity(self.serial_config.parity) {
    //             error!("恢复校验位失败: {:?}", e);
    //         }
    //     }
    // }

    // ---- 数据接收与解析 ----

    /// 读取命令 ACK 回复。
    ///
    /// 该路径只用于客户端下发命令后的 ACK 判断，不进入采集帧解析，
    /// 也不更新设备超时、解析失败、缓存或广播状态。
    pub(crate) fn receive_command_ack(&mut self) -> bool {
        let port = match self.port {
            Some(ref mut p) => p,
            None => {
                self.log_record("命令 ACK 读取失败：串口未打开".into());
                self.reconnect();
                return false;
            }
        };

        let mut total = 0usize;
        loop {
            match port.read(&mut self.read_buf[total..]) {
                Ok(0) => break,
                Ok(n) => {
                    total += n;
                    if contains_command_ack(&self.read_buf[..total]) {
                        if logging::serial_frame_logs_enabled() {
                            info!(
                                "串口 {} 命令 ACK: {}",
                                self.serial_config.port_name,
                                format_hex(&self.read_buf[..total])
                            );
                        }
                        return true;
                    }
                    if total >= self.read_buf.len() {
                        break;
                    }
                }
                Err(e) if e.kind() == ErrorKind::TimedOut => break,
                Err(e) => {
                    error!("读取命令 ACK 错误: {:?}", e);
                    self.log_record("读取命令 ACK 错误".into());
                    self.reconnect();
                    return false;
                }
            }
        }

        if total > 0 {
            warn!(
                "串口 {} 命令 ACK 不匹配: {}",
                self.serial_config.port_name,
                format_hex(&self.read_buf[..total])
            );
        }
        false
    }

    /// 从串口读取并解析一帧数据；超时或坏帧立即按原有语义提交故障。
    pub(crate) fn receive_data(&mut self) {
        if let ReceiveDataOutcome::Retryable(failure) = self.receive_data_attempt(None) {
            self.commit_query_failure(failure, None);
        }
    }

    /// 接收主机自动查询响应，首轮超时或坏帧时向同一设备补发一次。
    pub(crate) fn receive_query_with_retry(&mut self, query: SendData) {
        let query_index = self.index;
        let Some(device) = self.port_config.devices.get(query_index) else {
            return;
        };
        let expectation = QueryExpectation {
            device_id: device.config.device_id,
            command: cmd::READ_STATUS,
        };
        let started_at = Instant::now();

        let first_failure = match self.receive_data_attempt(Some(expectation)) {
            ReceiveDataOutcome::Success => {
                if self.current_run == RunLocation::Primary && self.index == query_index {
                    self.log_query_first_attempt_recovery(query_index, Instant::now());
                }
                return;
            }
            ReceiveDataOutcome::Retryable(failure) => failure,
            ReceiveDataOutcome::TransportHandled | ReceiveDataOutcome::UnattributedHandled => {
                return
            }
        };

        // 临时主机在接收期间可能因检测到有效转发头切回备机，此时禁止主动补发。
        if self.current_run != RunLocation::Primary || self.index != query_index {
            self.commit_query_failure(first_failure, None);
            return;
        }

        if let Some(port) = self.port.as_ref() {
            if let Err(error) = port.clear(ClearBuffer::Input) {
                warn!(
                    "[串口查询补发清理失败] port={} device_id={} index={} error={:?} action=continue_resend",
                    self.serial_config.port_name,
                    expectation.device_id,
                    query_index,
                    error,
                );
            }
        }

        if !self.send_command(query) {
            let context = QueryFailureContext {
                attempts: 2,
                first_failure: first_failure.clone(),
            };
            // send_command 在写失败时可能已将端口置空或完成重连；
            // 提交首次接收故障时必须保留当前真实的串口在线状态。
            let com_status = self.port.is_some();
            self.commit_query_failure_with_com_status(first_failure, Some(&context), com_status);
            return;
        }

        match self.receive_data_attempt(Some(expectation)) {
            ReceiveDataOutcome::Success => {
                if self.current_run == RunLocation::Primary && self.index == query_index {
                    self.log_query_retry_success(
                        query_index,
                        &first_failure,
                        started_at.elapsed(),
                        Instant::now(),
                    );
                }
            }
            ReceiveDataOutcome::Retryable(final_failure) => {
                let context = QueryFailureContext {
                    attempts: 2,
                    first_failure,
                };
                self.commit_query_failure(final_failure, Some(&context));
            }
            ReceiveDataOutcome::TransportHandled => {
                // 非超时 I/O 错误已在读取路径中完成重连和离线广播，不重复提交设备故障。
            }
            ReceiveDataOutcome::UnattributedHandled => {}
        }
    }

    /// 只读取、校验并处理一帧；超时和坏帧返回给调用方决定是否提交故障。
    fn receive_data_attempt(
        &mut self,
        expectation: Option<QueryExpectation>,
    ) -> ReceiveDataOutcome {
        let port = match self.port {
            Some(ref mut p) => p,
            None => {
                self.reconnect();
                return ReceiveDataOutcome::TransportHandled;
            }
        };

        // ---- 循环读取，直到帧完整或超时 ----
        let mut total = 0usize;
        loop {
            match port.read(&mut self.read_buf[total..]) {
                Ok(0) => break,
                Ok(n) => {
                    total += n;
                    if is_complete_short_direct_candidate(
                        &self.read_buf[..total],
                        &self.current_run,
                        &self.run_on,
                    ) {
                        // 短直连帧与 7 字节转发头可能同样满足校验；给分隔符一个很短的到达窗口，
                        // 避免没有后续字节时等待完整的串口读取超时。
                        std::thread::sleep(FORWARD_PREFIX_GRACE);
                        if matches!(port.bytes_to_read(), Ok(0)) {
                            break;
                        }
                    }
                    // 已读到帧头(3字节)时，可计算期望帧长
                    if total >= 3 {
                        let expected = expected_frame_len(
                            &self.read_buf[..total],
                            &self.current_run,
                            &self.run_on,
                        );
                        if total >= expected {
                            break; // 帧已完整
                        }
                    }
                    // 防止缓冲区溢出
                    if total >= self.read_buf.len() {
                        break;
                    }
                }
                Err(e) if e.kind() == ErrorKind::TimedOut => {
                    if total > 0 {
                        break; // 已有部分数据，尝试解析
                    }
                    return ReceiveDataOutcome::Retryable(QueryFailure::ReadTimeout);
                }
                Err(e) => {
                    error!("读取错误: {:?}", e);
                    self.log_record("读取错误".into());
                    self.reconnect();
                    return ReceiveDataOutcome::TransportHandled;
                }
            }
        }

        if total == 0 {
            return ReceiveDataOutcome::Retryable(QueryFailure::ReadTimeout);
        }

        // ---- 提取有效数据切片 ----
        let (frame, device_attributed) = self.extract_frame(total);
        // println!(
        //     "串口 {} 原始数据: {}",
        //     self.serial_config.port_name,
        //     format_hex(&frame)
        // );
        if logging::serial_frame_logs_enabled() {
            info!(
                "串口 {} 原始数据: {}",
                self.serial_config.port_name,
                format_hex(&frame)
            );
        }

        // 备机监听到的无有效转发头数据无法归属到具体设备，不能沿用旧 index。
        if !device_attributed {
            self.handle_unattributed_frame(&frame);
            return ReceiveDataOutcome::UnattributedHandled;
        }

        // ---- 解析数据帧 ----
        match protocol::parse_data_packet(&frame) {
            Ok(packet) => {
                if self.current_run == RunLocation::Primary {
                    if let Some(expected) = expectation {
                        let mismatch = if packet.addr != expected.device_id {
                            Some(format!(
                                "响应设备地址不匹配(expected={}, actual={})",
                                expected.device_id, packet.addr
                            ))
                        } else if packet.cmd != expected.command {
                            Some(format!(
                                "响应命令码不匹配(expected=0x{:02X}, actual=0x{:02X})",
                                expected.command, packet.cmd
                            ))
                        } else {
                            None
                        };
                        if let Some(reason) = mismatch {
                            return ReceiveDataOutcome::Retryable(QueryFailure::BadFrame {
                                reason,
                                frame,
                            });
                        }
                    }
                }

                self.handle_valid_packet(packet, frame);
                ReceiveDataOutcome::Success
            }
            Err(reason) => ReceiveDataOutcome::Retryable(QueryFailure::BadFrame {
                reason: reason.to_string(),
                frame,
            }),
        }
    }

    fn handle_valid_packet(&mut self, packet: protocol::DataPacket, frame: Vec<u8>) {
        // 只有长度、校验和及查询归属均有效的完整帧才能证明通信恢复。
        let now = Instant::now();
        self.log_device_recovery(now, frame.len());
        self.log_unattributed_recovery(now, frame.len());
        self.device_states[self.index].timeout_count = 0;
        self.time_out_count = 0;
        self.device_states[self.index].parse_fail_count = 0;
        self.parse_fail_count = 0;

        let dev = &self.port_config.devices[self.index];
        let parsed = protocol::parse_status(&packet.status, &dev.records);
        if logging::serial_frame_logs_enabled() {
            info!(
                "串口 {} 解析数据: {:?}",
                self.serial_config.port_name, parsed
            );
        }
        let status = DeviceStatus {
            id: dev.config.com_index as u64,
            device_id: dev.config.device_id,
            com: self.port_config.port_number.clone(),
            com_status: true,
            device_status: true,
            value: parsed,
            raw_data: frame,
        };
        self.latest_status_cache.update(status.clone());

        if self.device_states[self.index].should_broadcast(&packet.status, true, true) {
            self.broadcaster.broadcast(status);
        }
    }

    /// 从 `read_buf` 中提取有效数据帧。
    ///
    /// 备机模式下会解析转发头以更新 `index`，并剥离前缀。
    /// 如果当前由备机临时升级为主机，检测到有效转发头说明真正主机已恢复，
    /// 则自动切回备机。
    fn extract_frame(&mut self, total: usize) -> (Vec<u8>, bool) {
        let accepts_forwarded_frame =
            self.current_run == RunLocation::Secondary || self.run_on == RunLocation::Secondary;
        let forwarded = if accepts_forwarded_frame {
            validated_forward_data_start(&self.read_buf[..total]).and_then(|data_start| {
                let device_addr = self.read_buf[0];
                self.port_config
                    .devices
                    .iter()
                    .position(|dev| dev.config.device_id == device_addr)
                    .map(|device_index| (data_start, device_index))
            })
        } else {
            None
        };

        // 回切检测：备机临时充当主机时，若收到转发头说明真正主机已恢复
        if self.current_run == RunLocation::Primary
            && self.run_on == RunLocation::Secondary
            && forwarded.is_some()
        {
            self.switch_role(RunLocation::Secondary, "检测到主机转发帧，切回备机");
        }

        if self.current_run == RunLocation::Secondary {
            if let Some((data_start, device_index)) = forwarded {
                self.index = device_index;
                let data = &self.read_buf[data_start..total];
                let effective_len = if data.len() >= 4 {
                    (data[2] as usize + 5).min(data.len())
                } else {
                    data.len()
                };
                (data[..effective_len].to_vec(), true)
            } else {
                (self.read_buf[..total].to_vec(), false)
            }
        } else {
            (self.read_buf[..total].to_vec(), true)
        }
    }

    /// 备机收到无法通过转发头归属到设备的数据时，只累计端口级故障。
    fn handle_unattributed_frame(&mut self, frame: &[u8]) {
        self.parse_fail_count += 1;
        self.time_out_count += 1;

        let now = Instant::now();
        let action = self
            .unattributed_failure_log
            .record_failure(DeviceFailureKind::BadFrame, now);
        let (frame_hex, truncated) = format_hex_preview(frame, BAD_FRAME_LOG_MAX_BYTES);
        match action {
            FailureLogAction::Immediate(stats) => {
                warn!(
                    "[串口无法归属帧] port={} role={:?} reason={:?} frame_len={} frame_hex={:?} truncated={} duration_ms={} bad_frames={} action=port_failure_count_incremented",
                    self.serial_config.port_name,
                    self.current_run,
                    "无有效转发头或设备地址未知",
                    frame.len(),
                    frame_hex,
                    truncated,
                    stats.duration.as_millis(),
                    stats.bad_frames,
                );
                if stats.total_failures() == 1 {
                    self.log_record(format!(
                        "收到无法归属帧: reason={:?} frame_len={}",
                        "无有效转发头或设备地址未知",
                        frame.len()
                    ));
                }
            }
            FailureLogAction::Summary(stats) => warn!(
                "[串口无法归属帧持续] port={} role={:?} duration_s={} bad_frames={} suppressed={} last_frame_len={} last_frame_hex={:?} truncated={}",
                self.serial_config.port_name,
                self.current_run,
                stats.duration.as_secs(),
                stats.bad_frames,
                stats.suppressed,
                frame.len(),
                frame_hex,
                truncated,
            ),
            FailureLogAction::Suppressed => {}
        }

        if self.time_out_count > threshold::PORT_FAILOVER_TIMEOUT
            && self.current_run == RunLocation::Secondary
            && self.run_on == RunLocation::Secondary
        {
            self.switch_role(RunLocation::Primary, "无法归属的坏帧过多，备机升级为主机");
        }
    }

    fn handle_invalid_frame_with_context(
        &mut self,
        reason: &str,
        frame: &[u8],
        context: Option<&QueryFailureContext>,
        com_status: bool,
    ) {
        self.device_states[self.index].parse_fail_count += 1;
        self.parse_fail_count += 1;

        // 坏帧不能证明设备在线；复用超时路径更新离线状态和故障计数。
        self.handle_device_failure(
            DeviceFailureKind::BadFrame,
            Some(reason),
            Some(frame),
            Instant::now(),
            context,
            com_status,
        );

        // 连续解析失败过多时尝试主备切换。
        if self.parse_fail_count > threshold::PARSE_FAIL_FAILOVER
            && self.current_run == RunLocation::Primary
            && self.run_on == RunLocation::Secondary
        {
            self.switch_role(RunLocation::Secondary, "解析失败过多，切换为备机");
        }
    }

    fn handle_timeout_with_context(
        &mut self,
        context: Option<&QueryFailureContext>,
        com_status: bool,
    ) {
        self.handle_device_failure(
            DeviceFailureKind::ReadTimeout,
            None,
            None,
            Instant::now(),
            context,
            com_status,
        );
    }

    fn commit_query_failure(
        &mut self,
        failure: QueryFailure,
        context: Option<&QueryFailureContext>,
    ) {
        self.commit_query_failure_with_com_status(failure, context, true);
    }

    fn commit_query_failure_with_com_status(
        &mut self,
        failure: QueryFailure,
        context: Option<&QueryFailureContext>,
        com_status: bool,
    ) {
        match failure {
            QueryFailure::ReadTimeout => self.handle_timeout_with_context(context, com_status),
            QueryFailure::BadFrame { reason, frame } => {
                self.handle_invalid_frame_with_context(&reason, &frame, context, com_status);
            }
        }
    }

    fn handle_device_failure(
        &mut self,
        kind: DeviceFailureKind,
        reason: Option<&str>,
        frame: Option<&[u8]>,
        now: Instant,
        context: Option<&QueryFailureContext>,
        com_status: bool,
    ) {
        self.device_states[self.index].timeout_count += 1;
        self.time_out_count += 1;

        self.log_device_failure(kind, reason, frame, now, context);

        self.send_zero_status(com_status, false);

        // 超时过多时尝试主备切换
        if self.time_out_count > threshold::PORT_FAILOVER_TIMEOUT
            && self.current_run == RunLocation::Secondary
            && self.run_on == RunLocation::Secondary
        {
            self.switch_role(RunLocation::Primary, "超时过多，备机升级为主机");
        }
    }

    fn log_device_failure(
        &mut self,
        kind: DeviceFailureKind,
        reason: Option<&str>,
        frame: Option<&[u8]>,
        now: Instant,
        context: Option<&QueryFailureContext>,
    ) {
        let index = self.index;
        let dev = &self.port_config.devices[index];
        let device_id = dev.config.device_id;
        let affected_points = dev.records.len();
        let consecutive = self.device_states[index].timeout_count;
        let parse_failures = self.device_states[index].parse_fail_count;
        let attempts = context.map(|value| value.attempts).unwrap_or(1);
        let first_cause = context
            .map(|value| value.first_failure.cause_name())
            .unwrap_or_else(|| device_failure_kind_name(kind));
        let first_reason = context
            .and_then(|value| value.first_failure.reason())
            .or(reason)
            .unwrap_or("-");
        let first_frame_len = context
            .and_then(|value| value.first_failure.frame())
            .or(frame)
            .map(|frame| frame.len())
            .unwrap_or_default();
        let action = self.device_states[index]
            .failure_log
            .record_failure(kind, now);

        match action {
            FailureLogAction::Immediate(stats) => {
                match kind {
                    DeviceFailureKind::ReadTimeout => warn!(
                        "[串口通信异常] port={} device_id={} index={} role={:?} cause=read_timeout timeout_ms={} attempts={} first_cause={} first_reason={:?} first_frame_len={} final_cause=read_timeout consecutive={} affected_points={} action=publish_zero_bad_no_communication",
                        self.serial_config.port_name,
                        device_id,
                        index,
                        self.current_run,
                        self.serial_config.read_timeout.as_millis(),
                        attempts,
                        first_cause,
                        first_reason,
                        first_frame_len,
                        consecutive,
                        affected_points,
                    ),
                    DeviceFailureKind::BadFrame => {
                        let frame = frame.unwrap_or_default();
                        let details =
                            bad_frame_log_details(reason.unwrap_or("未知解析错误"), frame);
                        warn!(
                            "[串口坏帧] port={} device_id={} index={} role={:?} reason={:?} frame_len={} frame_hex={:?} truncated={} attempts={} first_cause={} first_reason={:?} first_frame_len={} final_cause=bad_frame consecutive={} parse_failures={} affected_points={} action=publish_zero_bad_no_communication",
                            self.serial_config.port_name,
                            device_id,
                            index,
                            self.current_run,
                            details.reason,
                            details.frame_len,
                            details.frame_hex,
                            details.truncated,
                            attempts,
                            first_cause,
                            first_reason,
                            first_frame_len,
                            consecutive,
                            parse_failures,
                            affected_points,
                        );
                    }
                }

                if stats.total_failures() == 1 {
                    self.log_record(format!(
                        "设备通信异常: device_id={} attempts={} first_cause={} final_cause={} action=publish_zero_bad_no_communication",
                        device_id,
                        attempts,
                        first_cause,
                        device_failure_kind_name(kind)
                    ));
                }
            }
            FailureLogAction::Summary(stats) => warn!(
                "[串口异常持续] port={} device_id={} index={} role={:?} duration_s={} timeouts={} bad_frames={} suppressed={} consecutive={} last_attempts={} last_first_cause={} current_cause={} action=publish_zero_bad_no_communication",
                self.serial_config.port_name,
                device_id,
                index,
                self.current_run,
                stats.duration.as_secs(),
                stats.timeouts,
                stats.bad_frames,
                stats.suppressed,
                consecutive,
                attempts,
                first_cause,
                device_failure_kind_name(kind),
            ),
            FailureLogAction::Suppressed => {}
        }
    }

    fn log_query_retry_success(
        &mut self,
        index: usize,
        first_failure: &QueryFailure,
        exchange_duration: Duration,
        now: Instant,
    ) {
        let action = self.device_states[index]
            .query_retry_log
            .record_retry_success(first_failure.kind(), now);
        let device_id = self.port_config.devices[index].config.device_id;

        match action {
            QueryRetryLogAction::Immediate(stats) => match first_failure {
                QueryFailure::ReadTimeout => warn!(
                    "[串口查询补发成功] port={} device_id={} index={} role={:?} cause=read_timeout attempts=2 exchange_duration_ms={} retries_succeeded={} action=keep_good_state",
                    self.serial_config.port_name,
                    device_id,
                    index,
                    self.current_run,
                    exchange_duration.as_millis(),
                    stats.retries_succeeded,
                ),
                QueryFailure::BadFrame { reason, frame } => {
                    let details = bad_frame_log_details(reason, frame);
                    warn!(
                        "[串口查询补发成功] port={} device_id={} index={} role={:?} cause=bad_frame reason={:?} frame_len={} frame_hex={:?} truncated={} attempts=2 exchange_duration_ms={} retries_succeeded={} action=keep_good_state",
                        self.serial_config.port_name,
                        device_id,
                        index,
                        self.current_run,
                        details.reason,
                        details.frame_len,
                        details.frame_hex,
                        details.truncated,
                        exchange_duration.as_millis(),
                        stats.retries_succeeded,
                    );
                }
            },
            QueryRetryLogAction::Summary(stats) => {
                let reason = first_failure.reason().unwrap_or("-");
                let frame = first_failure.frame().unwrap_or_default();
                let (frame_hex, truncated) = format_hex_preview(frame, BAD_FRAME_LOG_MAX_BYTES);
                warn!(
                    "[串口查询补发持续] port={} device_id={} index={} role={:?} duration_s={} retries_succeeded={} first_timeouts={} first_bad_frames={} suppressed={} last_cause={} last_reason={:?} last_frame_len={} last_frame_hex={:?} truncated={} attempts=2 exchange_duration_ms={} action=keep_good_state",
                    self.serial_config.port_name,
                    device_id,
                    index,
                    self.current_run,
                    stats.duration.as_secs(),
                    stats.retries_succeeded,
                    stats.first_timeouts,
                    stats.first_bad_frames,
                    stats.suppressed,
                    first_failure.cause_name(),
                    reason,
                    frame.len(),
                    frame_hex,
                    truncated,
                    exchange_duration.as_millis(),
                );
            }
            QueryRetryLogAction::Suppressed => {}
        }
    }

    fn log_query_first_attempt_recovery(&mut self, index: usize, now: Instant) {
        let Some(stats) = self.device_states[index]
            .query_retry_log
            .recover_on_first_attempt(now)
        else {
            return;
        };
        let device_id = self.port_config.devices[index].config.device_id;

        info!(
            "[串口查询恢复单次成功] port={} device_id={} index={} role={:?} duration_ms={} retries_succeeded={} first_timeouts={} first_bad_frames={} suppressed={} attempts=1 action=single_attempt_success",
            self.serial_config.port_name,
            device_id,
            index,
            self.current_run,
            stats.duration.as_millis(),
            stats.retries_succeeded,
            stats.first_timeouts,
            stats.first_bad_frames,
            stats.suppressed,
        );
    }

    fn log_device_recovery(&mut self, now: Instant, frame_len: usize) {
        let index = self.index;
        let device_id = self.port_config.devices[index].config.device_id;
        let Some(stats) = self.device_states[index].failure_log.recover(now) else {
            return;
        };

        info!(
            "[串口通信恢复] port={} device_id={} index={} role={:?} duration_ms={} timeouts={} bad_frames={} suppressed={} valid_frame_len={} action=publish_good_value",
            self.serial_config.port_name,
            device_id,
            index,
            self.current_run,
            stats.duration.as_millis(),
            stats.timeouts,
            stats.bad_frames,
            stats.suppressed,
            frame_len,
        );
        self.log_record(format!(
            "设备通信恢复: device_id={} duration_ms={} timeouts={} bad_frames={}",
            device_id,
            stats.duration.as_millis(),
            stats.timeouts,
            stats.bad_frames
        ));
    }

    fn log_unattributed_recovery(&mut self, now: Instant, frame_len: usize) {
        let Some(stats) = self.unattributed_failure_log.recover(now) else {
            return;
        };

        info!(
            "[串口转发恢复] port={} role={:?} duration_ms={} bad_frames={} suppressed={} valid_frame_len={}",
            self.serial_config.port_name,
            self.current_run,
            stats.duration.as_millis(),
            stats.bad_frames,
            stats.suppressed,
            frame_len,
        );
        self.log_record(format!(
            "串口转发恢复: duration_ms={} bad_frames={}",
            stats.duration.as_millis(),
            stats.bad_frames
        ));
    }

    /// 执行主备角色切换，重置所有故障计数器并记录日志。
    fn switch_role(&mut self, new_role: RunLocation, reason: &str) {
        let old = if self.current_run == RunLocation::Primary {
            "主机"
        } else {
            "备机"
        };
        let new = if new_role == RunLocation::Primary {
            "主机"
        } else {
            "备机"
        };
        let msg = format!("角色切换 {} → {}：{}", old, new, reason);

        warn!("{}", msg);
        self.log_record(msg);

        self.current_run = new_role;

        // 重置所有故障计数器，避免切换后立即再次触发
        self.time_out_count = 0;
        self.parse_fail_count = 0;
        for state in &mut self.device_states {
            state.timeout_count = 0;
            state.parse_fail_count = 0;
        }
    }
}

pub(crate) fn contains_command_ack(data: &[u8]) -> bool {
    data.windows(COMMAND_ACK_BYTES.len())
        .any(|window| window == COMMAND_ACK_BYTES)
}

// ============================================================
//  帧长度计算
// ============================================================

/// 根据已读数据估算期望的完整帧长度。
///
/// 帧格式 `[Addr][CMD][Len][Status×(Len+1)][CRC]`，总长 = Len + 5。
fn expected_frame_len(buf: &[u8], current_run: &RunLocation, run_on: &RunLocation) -> usize {
    let total = buf.len();
    let accepts_forwarded_frame =
        *current_run == RunLocation::Secondary || *run_on == RunLocation::Secondary;

    if accepts_forwarded_frame {
        if let Some(data_start) = validated_forward_data_start(buf) {
            if data_start + 3 <= total {
                let len_byte = buf[data_start + 2] as usize;
                return data_start + len_byte + 5;
            }
            return total + 1;
        }

        // 在收齐固定长度的转发头前不能把头部第 3 字节误当成数据帧长度。
        if total < FORWARD_PREFIX_LEN {
            return FORWARD_PREFIX_LEN;
        }
    }

    // 普通直连帧：直接取 buf[2] 作为 Len。
    let len_byte = buf[2] as usize;
    len_byte + 5
}

fn is_complete_short_direct_candidate(
    buf: &[u8],
    current_run: &RunLocation,
    run_on: &RunLocation,
) -> bool {
    if *current_run != RunLocation::Primary || *run_on != RunLocation::Secondary || buf.len() < 3 {
        return false;
    }

    let expected = buf[2] as usize + 5;
    expected < FORWARD_PREFIX_LEN
        && buf.len() >= expected
        && protocol::verify_checksum(&buf[..expected])
}

/// 仅接受固定的 `[7字节有效校验头][00 00]` 转发前缀。
fn validated_forward_data_start(buf: &[u8]) -> Option<usize> {
    if buf.len() < FORWARD_PREFIX_LEN
        || buf[FORWARD_HEADER_LEN..FORWARD_PREFIX_LEN] != [0, 0]
        || !protocol::verify_checksum(&buf[..FORWARD_HEADER_LEN])
    {
        return None;
    }
    Some(FORWARD_PREFIX_LEN)
}

// ============================================================
//  日志辅助
// ============================================================

/// 将字节序列格式化为十六进制字符串（空格分隔）。
fn format_hex(data: &[u8]) -> String {
    data.iter()
        .map(|b| format!("{:02X}", b))
        .collect::<Vec<_>>()
        .join(" ")
}

fn format_hex_preview(data: &[u8], max_bytes: usize) -> (String, bool) {
    let preview_len = data.len().min(max_bytes);
    (format_hex(&data[..preview_len]), data.len() > preview_len)
}

#[derive(Debug, PartialEq, Eq)]
struct BadFrameLogDetails {
    reason: String,
    frame_len: usize,
    frame_hex: String,
    truncated: bool,
}

fn bad_frame_log_details(reason: &str, frame: &[u8]) -> BadFrameLogDetails {
    let (frame_hex, truncated) = format_hex_preview(frame, BAD_FRAME_LOG_MAX_BYTES);
    BadFrameLogDetails {
        reason: reason.to_string(),
        frame_len: frame.len(),
        frame_hex,
        truncated,
    }
}

fn device_failure_kind_name(kind: DeviceFailureKind) -> &'static str {
    match kind {
        DeviceFailureKind::ReadTimeout => "read_timeout",
        DeviceFailureKind::BadFrame => "bad_frame",
    }
}

// ============================================================
//  串口打开
// ============================================================

/// 尝试打开串口。
pub(crate) fn open_serial_port(
    port_name: &str,
    baud_rate: u32,
    data_bits: DataBits,
    stop_bits: StopBits,
    parity: Parity,
    read_timeout: Duration,
) -> Result<Box<dyn SerialPort>, serialport::Error> {
    serialport::new(port_name, baud_rate)
        .data_bits(data_bits)
        .stop_bits(stop_bits)
        .parity(parity)
        .timeout(read_timeout)
        .open()
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::HashMap;
    use std::io::Read;

    use crate::csv_parser::{Config as CsvConfig, DeviceConfig};
    use crate::model::Value;
    use serialport::FlowControl;

    fn test_manager(run_on: RunLocation, current_run: RunLocation) -> SerialManager {
        let device = DeviceConfig {
            config: CsvConfig {
                com: "COM1".to_string(),
                baud_rate: 9_600,
                is_special: true,
                com_index: 1,
                device_id: 1,
                data_len: 3,
                kks_prefix: String::new(),
            },
            records: Vec::new(),
        };

        SerialManager {
            port: None,
            command_queue: Arc::new(Mutex::new(VecDeque::new())),
            serial_config: SerialConfig::new(
                "COM1".to_string(),
                9_600,
                DataBits::Eight,
                StopBits::One,
                Parity::None,
                Duration::from_millis(100),
            ),
            port_config: SerialPortConfig::new("COM1".to_string(), 9_600, true, vec![device]),
            broadcaster: Broadcaster::new(),
            latest_status_cache: LatestStatusCache::new(),
            device_states: vec![DeviceRuntimeState::new()],
            index: 0,
            run_on,
            current_run,
            time_out_count: 0,
            parse_fail_count: 0,
            system_record: Arc::new(Mutex::new(BoundedVecDeque::new(10))),
            read_buf: vec![0u8; 1024],
            last_reconnect: Instant::now(),
            reconnect_failure_log: ReconnectFailureLogState::default(),
            unattributed_failure_log: DeviceFailureLogState::default(),
        }
    }

    fn with_checksum(mut bytes: Vec<u8>) -> Vec<u8> {
        let checksum = crate::model::get_sum(&bytes);
        bytes.push(checksum);
        bytes
    }

    #[derive(Debug)]
    enum ReadStep {
        Data(Vec<u8>),
        Timeout,
        IoError,
    }

    #[derive(Debug, Default)]
    struct SerialObservations {
        write_attempts: usize,
        writes: Vec<Vec<u8>>,
        input_clears: usize,
    }

    struct ScriptedSerialPort {
        reads: VecDeque<ReadStep>,
        write_results: VecDeque<bool>,
        observations: Arc<Mutex<SerialObservations>>,
        timeout: Duration,
    }

    impl Read for ScriptedSerialPort {
        fn read(&mut self, buffer: &mut [u8]) -> std::io::Result<usize> {
            match self.reads.pop_front().unwrap_or(ReadStep::Timeout) {
                ReadStep::Data(mut data) => {
                    let count = data.len().min(buffer.len());
                    buffer[..count].copy_from_slice(&data[..count]);
                    if count < data.len() {
                        let remaining = data.split_off(count);
                        self.reads.push_front(ReadStep::Data(remaining));
                    }
                    Ok(count)
                }
                ReadStep::Timeout => Err(std::io::Error::new(
                    std::io::ErrorKind::TimedOut,
                    "scripted timeout",
                )),
                ReadStep::IoError => Err(std::io::Error::new(
                    std::io::ErrorKind::BrokenPipe,
                    "scripted I/O error",
                )),
            }
        }
    }

    impl std::io::Write for ScriptedSerialPort {
        fn write(&mut self, buffer: &[u8]) -> std::io::Result<usize> {
            let succeeds = self.write_results.pop_front().unwrap_or(true);
            let mut observations = self.observations.lock().unwrap();
            observations.write_attempts += 1;
            observations.writes.push(buffer.to_vec());
            if succeeds {
                Ok(buffer.len())
            } else {
                Err(std::io::Error::new(
                    std::io::ErrorKind::BrokenPipe,
                    "scripted write failure",
                ))
            }
        }

        fn flush(&mut self) -> std::io::Result<()> {
            Ok(())
        }
    }

    impl SerialPort for ScriptedSerialPort {
        fn name(&self) -> Option<String> {
            Some("scripted".to_string())
        }

        fn baud_rate(&self) -> serialport::Result<u32> {
            Ok(9_600)
        }

        fn data_bits(&self) -> serialport::Result<DataBits> {
            Ok(DataBits::Eight)
        }

        fn flow_control(&self) -> serialport::Result<FlowControl> {
            Ok(FlowControl::None)
        }

        fn parity(&self) -> serialport::Result<Parity> {
            Ok(Parity::None)
        }

        fn stop_bits(&self) -> serialport::Result<StopBits> {
            Ok(StopBits::One)
        }

        fn timeout(&self) -> Duration {
            self.timeout
        }

        fn set_baud_rate(&mut self, _baud_rate: u32) -> serialport::Result<()> {
            Ok(())
        }

        fn set_data_bits(&mut self, _data_bits: DataBits) -> serialport::Result<()> {
            Ok(())
        }

        fn set_flow_control(&mut self, _flow_control: FlowControl) -> serialport::Result<()> {
            Ok(())
        }

        fn set_parity(&mut self, _parity: Parity) -> serialport::Result<()> {
            Ok(())
        }

        fn set_stop_bits(&mut self, _stop_bits: StopBits) -> serialport::Result<()> {
            Ok(())
        }

        fn set_timeout(&mut self, timeout: Duration) -> serialport::Result<()> {
            self.timeout = timeout;
            Ok(())
        }

        fn write_request_to_send(&mut self, _level: bool) -> serialport::Result<()> {
            Ok(())
        }

        fn write_data_terminal_ready(&mut self, _level: bool) -> serialport::Result<()> {
            Ok(())
        }

        fn read_clear_to_send(&mut self) -> serialport::Result<bool> {
            Ok(false)
        }

        fn read_data_set_ready(&mut self) -> serialport::Result<bool> {
            Ok(false)
        }

        fn read_ring_indicator(&mut self) -> serialport::Result<bool> {
            Ok(false)
        }

        fn read_carrier_detect(&mut self) -> serialport::Result<bool> {
            Ok(false)
        }

        fn bytes_to_read(&self) -> serialport::Result<u32> {
            Ok(0)
        }

        fn bytes_to_write(&self) -> serialport::Result<u32> {
            Ok(0)
        }

        fn clear(&self, buffer_to_clear: ClearBuffer) -> serialport::Result<()> {
            if matches!(buffer_to_clear, ClearBuffer::Input | ClearBuffer::All) {
                self.observations.lock().unwrap().input_clears += 1;
            }
            Ok(())
        }

        fn try_clone(&self) -> serialport::Result<Box<dyn SerialPort>> {
            Err(serialport::Error::new(
                serialport::ErrorKind::Unknown,
                "scripted port cannot be cloned",
            ))
        }

        fn set_break(&self) -> serialport::Result<()> {
            Ok(())
        }

        fn clear_break(&self) -> serialport::Result<()> {
            Ok(())
        }
    }

    fn install_scripted_port(
        manager: &mut SerialManager,
        reads: Vec<ReadStep>,
        write_results: Vec<bool>,
    ) -> Arc<Mutex<SerialObservations>> {
        let observations = Arc::new(Mutex::new(SerialObservations::default()));
        manager.port = Some(Box::new(ScriptedSerialPort {
            reads: reads.into(),
            write_results: write_results.into(),
            observations: observations.clone(),
            timeout: manager.serial_config.read_timeout,
        }));
        observations
    }

    fn status_query() -> SendData {
        let mut command = vec![
            1,
            cmd::READ_STATUS,
            cmd::READ_SUBCMD,
            cmd::STATUS_PAGE,
            cmd::STATUS_OFFSET,
            3,
        ];
        command.push(crate::model::get_sum(&command));
        SendData {
            device_id: 1,
            command,
        }
    }

    fn valid_status_frame(device_id: u8, command: u8) -> Vec<u8> {
        with_checksum(vec![device_id, command, 2, 7, 8, 9])
    }

    fn run_auto_query(manager: &mut SerialManager) {
        let query = status_query();
        assert!(manager.send_command(query.clone()));
        manager.receive_query_with_retry(query);
    }

    #[test]
    fn command_ack_detects_exact_reply() {
        assert!(contains_command_ack(&[0x00, 0x50, 0x50]));
    }

    #[test]
    fn command_ack_detects_embedded_reply() {
        assert!(contains_command_ack(&[0xAA, 0x00, 0x50, 0x50, 0xBB]));
    }

    #[test]
    fn command_ack_rejects_partial_or_reordered_reply() {
        assert!(!contains_command_ack(&[]));
        assert!(!contains_command_ack(&[0x00, 0x50]));
        assert!(!contains_command_ack(&[0x50, 0x50, 0x00]));
    }

    #[test]
    fn device_failure_log_records_first_event_suppresses_and_summarizes() {
        let mut state = DeviceFailureLogState::default();
        let started = Instant::now();

        assert!(matches!(
            state.record_failure(DeviceFailureKind::ReadTimeout, started),
            FailureLogAction::Immediate(FailureEpisodeStats {
                timeouts: 1,
                bad_frames: 0,
                ..
            })
        ));
        assert_eq!(
            state.record_failure(
                DeviceFailureKind::ReadTimeout,
                started + Duration::from_secs(1)
            ),
            FailureLogAction::Suppressed
        );

        let FailureLogAction::Summary(stats) = state.record_failure(
            DeviceFailureKind::ReadTimeout,
            started + FAILURE_LOG_SUMMARY_INTERVAL,
        ) else {
            panic!("60 秒后应生成持续异常汇总");
        };
        assert_eq!(stats.timeouts, 3);
        assert_eq!(stats.bad_frames, 0);
        assert_eq!(stats.suppressed, 1);
    }

    #[test]
    fn device_failure_log_records_first_bad_frame_during_timeout_episode() {
        let mut state = DeviceFailureLogState::default();
        let started = Instant::now();
        state.record_failure(DeviceFailureKind::ReadTimeout, started);

        let FailureLogAction::Immediate(stats) = state.record_failure(
            DeviceFailureKind::BadFrame,
            started + Duration::from_secs(1),
        ) else {
            panic!("同一故障周期内的首次坏帧应立即记录");
        };
        assert_eq!(stats.timeouts, 1);
        assert_eq!(stats.bad_frames, 1);
    }

    #[test]
    fn device_failure_log_recovery_returns_totals_and_resets_episode() {
        let mut state = DeviceFailureLogState::default();
        let started = Instant::now();
        state.record_failure(DeviceFailureKind::ReadTimeout, started);
        state.record_failure(
            DeviceFailureKind::BadFrame,
            started + Duration::from_secs(1),
        );

        let stats = state
            .recover(started + Duration::from_secs(5))
            .expect("故障后应生成恢复统计");
        assert_eq!(stats.duration, Duration::from_secs(5));
        assert_eq!(stats.timeouts, 1);
        assert_eq!(stats.bad_frames, 1);
        assert!(state.recover(started + Duration::from_secs(6)).is_none());
        assert!(matches!(
            state.record_failure(
                DeviceFailureKind::ReadTimeout,
                started + Duration::from_secs(7)
            ),
            FailureLogAction::Immediate(_)
        ));
    }

    #[test]
    fn query_retry_log_limits_repeated_successes_and_recovers() {
        let mut state = QueryRetryLogState::default();
        let started = Instant::now();

        assert!(matches!(
            state.record_retry_success(DeviceFailureKind::ReadTimeout, started),
            QueryRetryLogAction::Immediate(QueryRetryEpisodeStats {
                retries_succeeded: 1,
                first_timeouts: 1,
                first_bad_frames: 0,
                ..
            })
        ));
        assert_eq!(
            state.record_retry_success(
                DeviceFailureKind::ReadTimeout,
                started + Duration::from_secs(1)
            ),
            QueryRetryLogAction::Suppressed
        );
        assert!(matches!(
            state.record_retry_success(
                DeviceFailureKind::BadFrame,
                started + Duration::from_secs(2)
            ),
            QueryRetryLogAction::Immediate(QueryRetryEpisodeStats {
                retries_succeeded: 3,
                first_timeouts: 2,
                first_bad_frames: 1,
                ..
            })
        ));

        let QueryRetryLogAction::Summary(summary) = state.record_retry_success(
            DeviceFailureKind::BadFrame,
            started + Duration::from_secs(62),
        ) else {
            panic!("60 秒后应生成查询补发持续汇总");
        };
        assert_eq!(summary.retries_succeeded, 4);
        assert_eq!(summary.first_timeouts, 2);
        assert_eq!(summary.first_bad_frames, 2);
        assert_eq!(summary.suppressed, 1);

        let recovered = state
            .recover_on_first_attempt(started + Duration::from_secs(65))
            .expect("首发恢复成功应结束补发故障周期");
        assert_eq!(recovered.retries_succeeded, 4);
        assert!(state
            .recover_on_first_attempt(started + Duration::from_secs(66))
            .is_none());
    }

    #[test]
    fn reconnect_log_reports_error_changes_summaries_and_recovery() {
        let mut state = ReconnectFailureLogState::default();
        let started = Instant::now();

        assert!(matches!(
            state.record_failure("not found", started),
            ReconnectLogAction::Immediate(_)
        ));
        assert_eq!(
            state.record_failure("not found", started + Duration::from_secs(10)),
            ReconnectLogAction::Suppressed
        );
        assert!(matches!(
            state.record_failure("permission denied", started + Duration::from_secs(20)),
            ReconnectLogAction::Immediate(ReconnectEpisodeStats { attempts: 3, .. })
        ));

        let ReconnectLogAction::Summary(stats) =
            state.record_failure("permission denied", started + Duration::from_secs(80))
        else {
            panic!("错误类型不变且超过 60 秒后应生成汇总");
        };
        assert_eq!(stats.attempts, 4);
        assert_eq!(stats.suppressed, 1);
        assert_eq!(stats.last_error, "permission denied");

        let recovered = state
            .recover(started + Duration::from_secs(90))
            .expect("重连成功应返回故障周期统计");
        assert_eq!(recovered.attempts, 4);
        assert_eq!(recovered.duration, Duration::from_secs(90));
        assert!(state.recover(started + Duration::from_secs(91)).is_none());
    }

    #[test]
    fn bad_frame_details_preserve_reason_and_truncate_to_64_bytes() {
        let frame: Vec<u8> = (0..100).collect();
        let details = bad_frame_log_details("校验和不匹配", &frame);

        assert_eq!(details.reason, "校验和不匹配");
        assert_eq!(details.frame_len, 100);
        assert!(details.truncated);
        assert_eq!(details.frame_hex.split_whitespace().count(), 64);
        assert_eq!(details.frame_hex, format_hex(&frame[..64]));

        let short = bad_frame_log_details("数据帧过短", &[1, 2, 3]);
        assert!(!short.truncated);
        assert_eq!(short.frame_hex, "01 02 03");
    }

    #[test]
    fn should_broadcast_data_changed() {
        let mut state = DeviceRuntimeState::new();
        assert!(state.should_broadcast(&[1, 2, 3], true, true));
    }

    #[test]
    fn should_broadcast_same_data_below_threshold() {
        let mut state = DeviceRuntimeState::new();
        state.should_broadcast(&[1, 2, 3], true, true); // 第一次, 更新 last_data
                                                        // 连续发送相同数据, 次数不到阈值, 不推送
        for _ in 0..threshold::SAME_DATA_BROADCAST - 1 {
            assert!(!state.should_broadcast(&[1, 2, 3], true, true));
        }
    }

    #[test]
    fn should_broadcast_same_data_at_threshold() {
        let mut state = DeviceRuntimeState::new();
        state.should_broadcast(&[1, 2, 3], true, true); // 第一次
                                                        // 到达阈值时推送
        for _ in 0..threshold::SAME_DATA_BROADCAST - 1 {
            state.should_broadcast(&[1, 2, 3], true, true);
        }
        assert!(state.should_broadcast(&[1, 2, 3], true, true)); // 第 SAME_DATA_BROADCAST 次
    }

    #[test]
    fn should_broadcast_reset_on_change() {
        let mut state = DeviceRuntimeState::new();
        state.should_broadcast(&[1, 2, 3], true, true);
        // 发几次相同数据
        state.should_broadcast(&[1, 2, 3], true, true);
        state.should_broadcast(&[1, 2, 3], true, true);
        // 新数据 → 立即推送, 且计数器重置
        assert!(state.should_broadcast(&[4, 5, 6], true, true));
        // 再次发相同数据, 重新计数
        assert!(!state.should_broadcast(&[4, 5, 6], true, true));
    }

    #[test]
    fn cache_updates_even_when_same_data_is_not_broadcast() {
        let cache = LatestStatusCache::new();
        let mut state = DeviceRuntimeState::new();

        let first = DeviceStatus {
            id: 1,
            device_id: 1,
            com: "COM1".to_string(),
            com_status: true,
            device_status: true,
            value: HashMap::from([("a".to_string(), Value::UInt(1))]),
            raw_data: vec![1, 2, 3],
        };
        cache.update(first);
        assert!(state.should_broadcast(&[1, 2, 3], true, true));

        let second = DeviceStatus {
            id: 1,
            device_id: 1,
            com: "COM1".to_string(),
            com_status: true,
            device_status: true,
            value: HashMap::from([("a".to_string(), Value::UInt(2))]),
            raw_data: vec![1, 2, 3],
        };
        cache.update(second);
        assert!(!state.should_broadcast(&[1, 2, 3], true, true));

        let snapshot = cache.snapshot_statuses();
        assert_eq!(snapshot.len(), 1);
        assert_eq!(snapshot[0].value.get("a"), Some(&Value::UInt(2)));
    }

    #[test]
    fn status_change_broadcasts_even_when_payload_is_unchanged() {
        let mut state = DeviceRuntimeState::new();
        assert!(state.should_broadcast(&[0, 0, 0], true, true));
        assert!(!state.should_broadcast(&[0, 0, 0], true, true));
        assert!(state.should_broadcast(&[0, 0, 0], true, false));
    }

    #[test]
    fn direct_frame_with_double_zero_does_not_trigger_failback() {
        let mut manager = test_manager(RunLocation::Secondary, RunLocation::Primary);
        let frame = with_checksum(vec![1, 0x08, 2, 0, 0, 0]);
        manager.read_buf[..frame.len()].copy_from_slice(&frame);

        let (extracted, attributed) = manager.extract_frame(frame.len());

        assert_eq!(extracted, frame);
        assert!(attributed);
        assert_eq!(manager.current_run, RunLocation::Primary);
    }

    #[test]
    fn short_direct_frame_uses_fast_completion_only_for_temporary_primary() {
        let frame = with_checksum(vec![1, 0x08, 2, 0, 0, 0]);

        assert!(is_complete_short_direct_candidate(
            &frame,
            &RunLocation::Primary,
            &RunLocation::Secondary,
        ));
        assert!(!is_complete_short_direct_candidate(
            &frame,
            &RunLocation::Secondary,
            &RunLocation::Secondary,
        ));
        assert!(!is_complete_short_direct_candidate(
            &frame,
            &RunLocation::Primary,
            &RunLocation::Primary,
        ));
    }

    #[test]
    fn valid_forward_header_triggers_failback_and_is_removed() {
        let mut manager = test_manager(RunLocation::Secondary, RunLocation::Primary);
        let header = with_checksum(vec![1, 2, 3, 4, 5, 6]);
        let payload = with_checksum(vec![1, 0x08, 2, 7, 8, 9]);
        let mut frame = header;
        frame.extend_from_slice(&[0, 0]);
        frame.extend_from_slice(&payload);
        manager.read_buf[..frame.len()].copy_from_slice(&frame);

        let (extracted, attributed) = manager.extract_frame(frame.len());

        assert_eq!(extracted, payload);
        assert!(attributed);
        assert_eq!(manager.current_run, RunLocation::Secondary);
    }

    #[test]
    fn unattributed_secondary_frame_does_not_mark_previous_device_offline() {
        let mut manager = test_manager(RunLocation::Secondary, RunLocation::Secondary);
        let mut rx = manager.broadcaster.subscribe();
        let noise = vec![0xAA, 0xBB, 0xCC, 0xDD];
        manager.read_buf[..noise.len()].copy_from_slice(&noise);

        let (_, attributed) = manager.extract_frame(noise.len());
        assert!(!attributed);
        manager.handle_unattributed_frame(&noise);

        assert_eq!(manager.time_out_count, 1);
        assert_eq!(manager.device_states[0].timeout_count, 0);
        assert!(manager.latest_status_cache.is_empty());
        assert!(matches!(
            rx.try_recv(),
            Err(tokio::sync::broadcast::error::TryRecvError::Empty)
        ));
    }

    #[test]
    fn invalid_frame_marks_device_offline() {
        let mut manager = test_manager(RunLocation::Primary, RunLocation::Primary);
        let mut rx = manager.broadcaster.subscribe();

        manager.handle_invalid_frame_with_context("校验和不匹配", &[1, 2, 3, 4], None, true);

        assert_eq!(manager.time_out_count, 1);
        assert_eq!(manager.device_states[0].timeout_count, 1);
        let status = rx.try_recv().expect("离线状态应立即广播");
        assert!(!status.device_status);
        assert!(!manager.latest_status_cache.snapshot_statuses()[0].device_status);
    }

    #[test]
    fn first_timeout_still_marks_device_offline_immediately() {
        let mut manager = test_manager(RunLocation::Primary, RunLocation::Primary);
        let mut rx = manager.broadcaster.subscribe();

        manager.handle_timeout_with_context(None, true);

        assert_eq!(manager.time_out_count, 1);
        assert_eq!(manager.device_states[0].timeout_count, 1);
        let status = rx.try_recv().expect("首次超时仍应立即广播离线状态");
        assert!(status.com_status);
        assert!(!status.device_status);
        assert!(!manager.latest_status_cache.snapshot_statuses()[0].device_status);
    }

    #[test]
    fn automatic_query_timeout_then_retry_success_keeps_device_online() {
        let mut manager = test_manager(RunLocation::Primary, RunLocation::Primary);
        let observations = install_scripted_port(
            &mut manager,
            vec![
                ReadStep::Timeout,
                ReadStep::Data(valid_status_frame(1, cmd::READ_STATUS)),
            ],
            vec![true, true],
        );
        let mut rx = manager.broadcaster.subscribe();

        run_auto_query(&mut manager);

        let observations = observations.lock().unwrap();
        assert_eq!(observations.write_attempts, 2);
        assert_eq!(observations.writes[0], observations.writes[1]);
        assert_eq!(observations.input_clears, 1);
        drop(observations);
        assert_eq!(manager.time_out_count, 0);
        assert_eq!(manager.parse_fail_count, 0);
        assert_eq!(manager.device_states[0].timeout_count, 0);
        assert_eq!(manager.device_states[0].parse_fail_count, 0);
        assert!(manager.system_record.lock().unwrap().is_empty());

        let status = rx.try_recv().expect("补发成功应广播有效值");
        assert!(status.com_status);
        assert!(status.device_status);
        assert_eq!(status.raw_data, valid_status_frame(1, cmd::READ_STATUS));
        assert!(matches!(
            rx.try_recv(),
            Err(tokio::sync::broadcast::error::TryRecvError::Empty)
        ));
        assert!(manager.latest_status_cache.snapshot_statuses()[0].device_status);
        assert_eq!(
            manager.device_states[0]
                .query_retry_log
                .stats(Instant::now())
                .first_timeouts,
            1
        );
    }

    #[test]
    fn automatic_query_bad_frame_then_retry_success_does_not_commit_failure() {
        let mut bad_frame = valid_status_frame(1, cmd::READ_STATUS);
        *bad_frame.last_mut().unwrap() ^= 0xFF;
        let mut manager = test_manager(RunLocation::Primary, RunLocation::Primary);
        let observations = install_scripted_port(
            &mut manager,
            vec![
                ReadStep::Data(bad_frame),
                ReadStep::Data(valid_status_frame(1, cmd::READ_STATUS)),
            ],
            vec![true, true],
        );

        run_auto_query(&mut manager);

        assert_eq!(observations.lock().unwrap().write_attempts, 2);
        assert_eq!(manager.time_out_count, 0);
        assert_eq!(manager.parse_fail_count, 0);
        assert_eq!(manager.device_states[0].timeout_count, 0);
        assert_eq!(manager.device_states[0].parse_fail_count, 0);
        assert!(manager.system_record.lock().unwrap().is_empty());
        let retry_stats = manager.device_states[0]
            .query_retry_log
            .stats(Instant::now());
        assert_eq!(retry_stats.retries_succeeded, 1);
        assert_eq!(retry_stats.first_bad_frames, 1);
    }

    #[test]
    fn automatic_query_two_timeouts_commit_one_offline_failure() {
        let mut manager = test_manager(RunLocation::Primary, RunLocation::Primary);
        let observations = install_scripted_port(
            &mut manager,
            vec![ReadStep::Timeout, ReadStep::Timeout],
            vec![true, true],
        );
        let mut rx = manager.broadcaster.subscribe();

        run_auto_query(&mut manager);

        assert_eq!(observations.lock().unwrap().write_attempts, 2);
        assert_eq!(manager.time_out_count, 1);
        assert_eq!(manager.parse_fail_count, 0);
        assert_eq!(manager.device_states[0].timeout_count, 1);
        assert_eq!(manager.device_states[0].failure_log.timeouts, 1);
        assert_eq!(manager.device_states[0].failure_log.bad_frames, 0);
        assert_eq!(manager.system_record.lock().unwrap().len(), 1);
        let status = rx.try_recv().expect("两次超时后应只广播一次离线");
        assert!(status.com_status);
        assert!(!status.device_status);
        assert!(matches!(
            rx.try_recv(),
            Err(tokio::sync::broadcast::error::TryRecvError::Empty)
        ));
    }

    #[test]
    fn automatic_query_two_bad_frames_commit_one_parse_failure() {
        let mut bad_frame = valid_status_frame(1, cmd::READ_STATUS);
        *bad_frame.last_mut().unwrap() ^= 0xFF;
        let mut manager = test_manager(RunLocation::Primary, RunLocation::Primary);
        let observations = install_scripted_port(
            &mut manager,
            vec![ReadStep::Data(bad_frame.clone()), ReadStep::Data(bad_frame)],
            vec![true, true],
        );
        let mut rx = manager.broadcaster.subscribe();

        run_auto_query(&mut manager);

        assert_eq!(observations.lock().unwrap().write_attempts, 2);
        assert_eq!(manager.time_out_count, 1);
        assert_eq!(manager.parse_fail_count, 1);
        assert_eq!(manager.device_states[0].timeout_count, 1);
        assert_eq!(manager.device_states[0].parse_fail_count, 1);
        assert_eq!(manager.device_states[0].failure_log.timeouts, 0);
        assert_eq!(manager.device_states[0].failure_log.bad_frames, 1);
        assert!(!rx.try_recv().expect("两次坏帧后应广播离线").device_status);
        assert!(matches!(
            rx.try_recv(),
            Err(tokio::sync::broadcast::error::TryRecvError::Empty)
        ));
    }

    #[test]
    fn automatic_query_final_attempt_determines_failure_counters() {
        let mut bad_frame = valid_status_frame(1, cmd::READ_STATUS);
        *bad_frame.last_mut().unwrap() ^= 0xFF;

        let mut timeout_then_bad = test_manager(RunLocation::Primary, RunLocation::Primary);
        install_scripted_port(
            &mut timeout_then_bad,
            vec![ReadStep::Timeout, ReadStep::Data(bad_frame.clone())],
            vec![true, true],
        );
        run_auto_query(&mut timeout_then_bad);
        assert_eq!(timeout_then_bad.time_out_count, 1);
        assert_eq!(timeout_then_bad.parse_fail_count, 1);
        assert_eq!(timeout_then_bad.device_states[0].failure_log.timeouts, 0);
        assert_eq!(timeout_then_bad.device_states[0].failure_log.bad_frames, 1);

        let mut bad_then_timeout = test_manager(RunLocation::Primary, RunLocation::Primary);
        install_scripted_port(
            &mut bad_then_timeout,
            vec![ReadStep::Data(bad_frame), ReadStep::Timeout],
            vec![true, true],
        );
        run_auto_query(&mut bad_then_timeout);
        assert_eq!(bad_then_timeout.time_out_count, 1);
        assert_eq!(bad_then_timeout.parse_fail_count, 0);
        assert_eq!(bad_then_timeout.device_states[0].failure_log.timeouts, 1);
        assert_eq!(bad_then_timeout.device_states[0].failure_log.bad_frames, 0);
    }

    #[test]
    fn automatic_query_retry_send_failure_commits_first_receive_failure() {
        let mut manager = test_manager(RunLocation::Primary, RunLocation::Primary);
        let observations =
            install_scripted_port(&mut manager, vec![ReadStep::Timeout], vec![true, false]);
        let mut rx = manager.broadcaster.subscribe();

        run_auto_query(&mut manager);

        assert_eq!(observations.lock().unwrap().write_attempts, 2);
        assert_eq!(manager.time_out_count, 1);
        assert_eq!(manager.parse_fail_count, 0);
        assert_eq!(manager.device_states[0].failure_log.timeouts, 1);
        let status = rx.try_recv().expect("补发写入失败不能丢失离线通知");
        assert!(!status.com_status);
        assert!(!status.device_status);
        let cached = manager.latest_status_cache.snapshot_statuses();
        assert_eq!(cached.len(), 1);
        assert!(!cached[0].com_status);
        assert!(!cached[0].device_status);
        assert!(matches!(
            rx.try_recv(),
            Err(tokio::sync::broadcast::error::TryRecvError::Empty)
        ));
    }

    #[test]
    fn automatic_query_first_attempt_success_sends_only_once() {
        let mut manager = test_manager(RunLocation::Primary, RunLocation::Primary);
        let observations = install_scripted_port(
            &mut manager,
            vec![ReadStep::Data(valid_status_frame(1, cmd::READ_STATUS))],
            vec![true],
        );

        run_auto_query(&mut manager);

        let observations = observations.lock().unwrap();
        assert_eq!(observations.write_attempts, 1);
        assert_eq!(observations.input_clears, 0);
        drop(observations);
        assert_eq!(manager.time_out_count, 0);
        assert_eq!(manager.parse_fail_count, 0);
        assert!(manager.device_states[0]
            .query_retry_log
            .started_at
            .is_none());
    }

    #[test]
    fn automatic_query_io_error_does_not_trigger_active_retry() {
        let mut manager = test_manager(RunLocation::Primary, RunLocation::Primary);
        let observations = install_scripted_port(&mut manager, vec![ReadStep::IoError], vec![true]);

        run_auto_query(&mut manager);

        assert_eq!(observations.lock().unwrap().write_attempts, 1);
        assert_eq!(manager.time_out_count, 0);
        assert_eq!(manager.parse_fail_count, 0);
        assert_eq!(manager.device_states[0].timeout_count, 0);
        assert!(manager.device_states[0]
            .query_retry_log
            .started_at
            .is_none());
    }

    #[test]
    fn automatic_query_retries_address_and_command_mismatches() {
        for mismatched_frame in [
            valid_status_frame(2, cmd::READ_STATUS),
            valid_status_frame(1, 0x09),
        ] {
            let mut manager = test_manager(RunLocation::Primary, RunLocation::Primary);
            let observations = install_scripted_port(
                &mut manager,
                vec![
                    ReadStep::Data(mismatched_frame),
                    ReadStep::Data(valid_status_frame(1, cmd::READ_STATUS)),
                ],
                vec![true, true],
            );

            run_auto_query(&mut manager);

            assert_eq!(observations.lock().unwrap().write_attempts, 2);
            assert_eq!(manager.time_out_count, 0);
            assert_eq!(manager.parse_fail_count, 0);
            let statuses = manager.latest_status_cache.snapshot_statuses();
            assert_eq!(statuses.len(), 1);
            assert_eq!(statuses[0].device_id, 1);
            assert!(statuses[0].device_status);
        }
    }

    #[test]
    fn missing_port_during_reconnect_cooldown_does_not_spam_system_record() {
        let mut manager = test_manager(RunLocation::Secondary, RunLocation::Secondary);

        manager.receive_data();
        manager.receive_data();

        assert!(manager.system_record.lock().unwrap().is_empty());
    }
}
