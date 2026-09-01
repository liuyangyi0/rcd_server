//! 串口通信管理器。
//!
//! 负责串口的打开、重连、数据收发、协议解析以及将解析结果分发给上层订阅者。
//!
//! 串口 I/O 是同步阻塞操作，运行在 `tokio::task::spawn_blocking` 线程池中。
//! 需要执行异步操作（广播、日志、延时）时，通过持有的 `tokio::runtime::Handle`
//! 调用 `block_on` 桥接到主运行时。

use std::collections::VecDeque;
use std::io::{self, ErrorKind};
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
use crate::protocol::{self, threshold};
use crate::runtime_cache::LatestStatusCache;

const COMMAND_ACK_BYTES: [u8; 3] = [0x00, 0x50, 0x50];
const FORWARD_HEADER_LEN: usize = 7;
const FORWARD_SEPARATOR_LEN: usize = 2;
const FORWARD_PREFIX_LEN: usize = FORWARD_HEADER_LEN + FORWARD_SEPARATOR_LEN;
// 协议帧总长为 Len + 地址、命令、长度、状态和校验共 5 个固定字节。
const RESPONSE_FRAME_OVERHEAD: usize = 5;
// 部分历史配置把 data_len 写成实际状态区长度（协议 Len + 1）。
const RESPONSE_FRAME_OVERHEAD_WITH_STATUS_LEN: usize = 4;
const FORWARD_PREFIX_GRACE: Duration = Duration::from_millis(5);
const FAILURE_LOG_SUMMARY_INTERVAL: Duration = Duration::from_secs(60);
const BAD_FRAME_LOG_MAX_BYTES: usize = 64;
const OUTPUT_QUEUE_POLL_INTERVAL: Duration = Duration::from_micros(250);
const MIN_TRANSMIT_IO_SLACK: Duration = Duration::from_millis(50);
const MAX_TRANSMIT_TIMEOUT: Duration = Duration::from_secs(5);
const RESPONSE_COMPLETION_GUARD: Duration = Duration::from_millis(5);
const STATION_VERSION_RESPONSE_LEN: usize = 9;
const STATION_VERSION_LATE_FRAME_QUARANTINE: Duration = Duration::from_millis(50);

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
    last_retry_at: Option<Instant>,
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
        // 单次查询成功不再立即结束日志周期。现场的间歇性超时通常表现为
        // “补发成功 -> 下一轮首发成功 -> 再次补发”，若每次首发成功都重置，
        // 限频会失效并为每次补发产生两条日志。只有连续 60 秒没有补发时，
        // 才在下一次补发到来时开始新的诊断周期。
        if self
            .last_retry_at
            .is_some_and(|last| now.saturating_duration_since(last) >= FAILURE_LOG_SUMMARY_INTERVAL)
        {
            *self = Self::default();
        }

        if self.started_at.is_none() {
            self.started_at = Some(now);
            self.last_log_at = Some(now);
        }
        self.last_retry_at = Some(now);
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
    pub parity_switch_delay: Duration,
    pub transmit_settle_guard: Duration,
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
            parity_switch_delay: Duration::from_millis(1),
            transmit_settle_guard: Duration::from_millis(1),
        }
    }

    pub fn with_transmit_timing(
        mut self,
        parity_switch_delay: Duration,
        transmit_settle_guard: Duration,
    ) -> Self {
        self.parity_switch_delay = parity_switch_delay;
        self.transmit_settle_guard = transmit_settle_guard;
        self
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
    /// Acquisition-only gate, independent of legacy whole-device disable.
    pub(crate) read_enabled: bool,
    last_data_at: Option<String>,
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
            read_enabled: true,
            last_data_at: None,
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
    pub(crate) expires_at: Option<Instant>,
    pub(crate) _permit: Option<crate::operations::CommandPermit>,
}

pub(crate) struct SerialManager {
    pub(crate) port: Option<Box<dyn SerialPort>>,
    pub(crate) operations: Option<crate::operations::Operations>,
    pub(crate) command_queue: Arc<Mutex<VecDeque<QueuedCommand>>>,
    /// 启动/重新启用/改为国产后的一次性版本查询；同一设备去重。
    station_version_query_queue: VecDeque<u8>,
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
    /// 尚未消费的串口输入。备机一次 read 收到多帧时保留后续完整帧。
    rx_pending: Vec<u8>,
    /// 上次重连尝试的时间，用于冷却控制。
    last_reconnect: Instant,
    /// 当前端口掉线状态是否已向全部设备发布，避免重连冷却期间重复广播。
    port_offline_published: bool,
    /// 串口打开/重连故障周期的限频日志状态。
    reconnect_failure_log: ReconnectFailureLogState,
    /// 备机无法归属帧的端口级限频日志状态。
    unattributed_failure_log: DeviceFailureLogState,
    /// 串口接收尝试序号，仅用于把交错的多端口诊断日志关联到同一次 read/组帧。
    receive_sequence: u64,
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
        // A persistently closed port must never be opened even momentarily at
        // startup (another application may legitimately own the hardware).
        let opened = if port_config.status {
            std::thread::sleep(Duration::from_millis(2000));
            Some(open_serial_port(
                &serial_config.port_name,
                serial_config.baud_rate,
                serial_config.data_bits,
                serial_config.stop_bits,
                serial_config.parity,
                serial_config.read_timeout,
            ))
        } else {
            None
        };

        let mut reconnect_failure_log = ReconnectFailureLogState::default();
        let (port, initial_open_error) = match opened {
            Some(Ok(port)) => (Some(port), None),
            Some(Err(error)) => {
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
            None => (None, None),
        };

        let device_states = port_config
            .devices
            .iter()
            .map(|_| DeviceRuntimeState::new())
            .collect();

        let mut manager = Self {
            port,
            operations: None,
            command_queue: Arc::new(Mutex::new(VecDeque::new())),
            station_version_query_queue: VecDeque::new(),
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
            rx_pending: Vec::new(),
            last_reconnect: Instant::now(),
            port_offline_published: false,
            reconnect_failure_log,
            unattributed_failure_log: DeviceFailureLogState::default(),
            receive_sequence: 0,
        };

        if let Some(error) = initial_open_error {
            manager.log_record(format!("串口打开失败: error={error:?}"));
            manager.publish_port_offline_once();
        } else if !manager.port_config.status {
            manager.publish_port_disabled();
        }

        manager
    }

    // ---- 连接管理 ----

    /// 进入重连路径时先将整口设备发布为离线零值，再尝试重新打开串口。
    ///
    /// 内置 2 秒冷却：距上次重连不足 2 秒时仅跳过打开操作；整口离线状态仍会
    /// 在当前故障周期首次进入时发布。串口打开成功只清除离线发布标记，各设备
    /// 必须收到有效帧后才恢复在线状态。
    pub(crate) fn reconnect(&mut self) {
        const RECONNECT_COOLDOWN: Duration = Duration::from_secs(2);

        if !self.port_config.status {
            self.port = None;
            self.rx_pending.clear();
            return;
        }

        // 连接恢复前，端口下所有设备都已经失去可信数据。该动作必须发生在
        // 冷却判断之前，否则冷却期间会继续向 OPC 暴露旧值。
        self.publish_port_offline_once();

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
                self.port_offline_published = false;
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
                self.port = None;
            }
        }
    }

    /// 将当前设备的全零状态发送给所有订阅者。
    pub(crate) fn send_zero_status(&mut self, com_status: bool, device_status: bool) {
        self.send_zero_status_for_index(self.index, com_status, device_status);
    }

    /// 将指定设备的全零状态写入缓存并按状态变化规则广播。
    fn send_zero_status_for_index(&mut self, index: usize, com_status: bool, device_status: bool) {
        let (zero_data, status) = {
            let Some(dev) = self.port_config.devices.get(index) else {
                return;
            };
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
            (zero_data, status)
        };
        self.latest_status_cache.update(status.clone());

        if self
            .device_states
            .get_mut(index)
            .is_some_and(|state| state.should_broadcast(&zero_data, com_status, device_status))
        {
            self.broadcaster.broadcast(status);
        }
    }

    /// 首次检测到整口不可用时，将该端口下全部设备发布为离线零值。
    fn publish_port_offline_once(&mut self) {
        if self.port_offline_published {
            return;
        }
        self.port_offline_published = true;

        for index in 0..self.port_config.devices.len() {
            self.send_zero_status_for_index(index, false, false);
        }
    }

    /// 上层主动禁用端口时立即撤销该端口全部 OPC 有效值。
    /// 这是管理状态变化，不占用物理串口掉线周期的去重标记。
    pub(crate) fn publish_port_disabled(&mut self) {
        for index in 0..self.port_config.devices.len() {
            self.send_zero_status_for_index(index, false, false);
        }
        info!(
            "[串口已禁用] port={} affected_devices={} action=publish_all_zero_bad_no_communication",
            self.serial_config.port_name,
            self.port_config.devices.len(),
        );
        self.log_record(format!(
            "串口已禁用: affected_devices={} action=publish_all_zero_bad_no_communication",
            self.port_config.devices.len()
        ));
    }

    /// Called on the worker between serial transactions; disabling really
    /// releases the descriptor, unlike the former polling-only flag.
    pub(crate) fn set_port_enabled(&mut self, enabled: bool) -> Result<(), String> {
        self.port_config.status = enabled;
        if !enabled {
            self.port = None;
            self.rx_pending.clear();
            self.publish_port_disabled();
            return Ok(());
        }
        if self.port.is_none() {
            self.last_reconnect = Instant::now() - Duration::from_secs(2);
            self.reconnect();
        }
        if self.port.is_none() {
            return Err("port enable is applied, but opening the serial device failed; automatic connection recovery remains active".into());
        }
        Ok(())
    }

    pub(crate) fn set_device_read_enabled(
        &mut self,
        device_id: u8,
        enabled: bool,
    ) -> Result<(), String> {
        let index = self
            .port_config
            .devices
            .iter()
            .position(|d| d.config.device_id == device_id)
            .ok_or_else(|| "unknown device".to_string())?;
        self.device_states[index].read_enabled = enabled;
        // Never claim recovery until a new valid frame is accepted. Retain
        // control command permission and all other devices' receive buffers.
        self.send_zero_status_for_index(index, self.port.is_some(), false);
        if enabled {
            self.device_states[index].timeout_count = 0;
            self.device_states[index].current_round = 0;
        }
        self.log_record(format!(
            "设备读取设置: device_id={device_id} read_enabled={enabled}"
        ));
        Ok(())
    }

    pub(crate) fn refresh_operations_state(&self) {
        if let Some(operations) = &self.operations {
            operations.report_runtime(
                &self.port_config.port_number,
                self.port_config.status,
                self.port.is_some(),
                self.port_config
                    .devices
                    .iter()
                    .zip(&self.device_states)
                    .map(|(device, runtime)| {
                        (
                            device.config.device_id,
                            runtime.read_enabled,
                            self.port_config.status
                                && self.port.is_some()
                                && runtime.is_enabled
                                && runtime.read_enabled
                                && runtime.last_com_status == Some(true)
                                && runtime.last_device_status == Some(true),
                            runtime.last_data_at.clone(),
                        )
                    }),
            );
        }
    }

    /// 上层主动禁用单个设备时立即撤销该设备的 OPC 有效值。
    pub(crate) fn publish_device_disabled(&mut self, index: usize) {
        let Some(device_id) = self
            .port_config
            .devices
            .get(index)
            .map(|device| device.config.device_id)
        else {
            return;
        };
        self.send_zero_status_for_index(index, self.port.is_some(), false);
        info!(
            "[串口设备已禁用] port={} device_id={} index={} action=publish_zero_bad_no_communication",
            self.serial_config.port_name, device_id, index,
        );
        self.log_record(format!(
            "串口设备已禁用: device_id={} index={} action=publish_zero_bad_no_communication",
            device_id, index
        ));
    }

    /// worker panic 后释放串口并向全部设备发布离线，随后由监督循环重新进入重连。
    pub(crate) fn prepare_worker_restart(&mut self) {
        self.port = None;
        self.rx_pending.clear();
        self.publish_port_offline_once();
    }

    /// 主机自动查询发送前清除上一轮迟到数据和驱动接收状态。
    ///
    /// 该操作只清输入队列；当前查询尚未发送，因此不会丢弃本轮响应，也不会影响
    /// 输出缓冲、命令下发或备机被动监听。
    pub(crate) fn prepare_automatic_query(&mut self, device_id: u8, index: usize) {
        let frame_diagnostics = logging::serial_frame_logs_enabled();
        let pending_len = self.rx_pending.len();
        let (pending_hex, pending_truncated) =
            format_hex_preview(&self.rx_pending, BAD_FRAME_LOG_MAX_BYTES);
        let mut queued_before = None;
        let mut queued_after = None;
        let mut clear_result = "port_not_open";

        if let Some(port) = self.port.as_ref() {
            if frame_diagnostics {
                queued_before = port.bytes_to_read().ok();
            }
            if let Err(error) = port.clear(ClearBuffer::Input) {
                clear_result = "failed";
                warn!(
                    "[串口查询前清理失败] port={} device_id={} index={} error={:?} action=continue_query",
                    self.serial_config.port_name, device_id, index, error,
                );
            } else {
                clear_result = "success";
            }
            if frame_diagnostics {
                queued_after = port.bytes_to_read().ok();
            }
        }
        self.rx_pending.clear();

        if frame_diagnostics {
            info!(
                "[串口查询前清理诊断] port={} device_id={} index={} result={} kernel_bytes_before={:?} kernel_bytes_after={:?} pending_len={} pending_hex={:?} pending_truncated={} action=clear_input_before_first_send",
                self.serial_config.port_name,
                device_id,
                index,
                clear_result,
                queued_before,
                queued_after,
                pending_len,
                pending_hex,
                pending_truncated,
            );
        }
    }

    pub(crate) fn schedule_station_version_query(&mut self, device_id: u8) {
        if !self
            .station_version_query_queue
            .iter()
            .any(|queued| *queued == device_id)
        {
            self.station_version_query_queue.push_back(device_id);
        }
    }

    pub(crate) fn cancel_scheduled_station_version_query(&mut self, device_id: u8) {
        self.station_version_query_queue
            .retain(|queued| *queued != device_id);
    }

    pub(crate) fn schedule_configured_domestic_version_queries(&mut self) {
        let Some(operations) = self.operations.clone() else {
            return;
        };
        for device_id in operations.startup_version_query_devices(&self.port_config.port_number) {
            self.schedule_station_version_query(device_id);
        }
    }

    /// 每轮完整的普通站点扫描后最多执行一个启动/重新启用查询。
    pub(crate) fn execute_one_scheduled_station_version_query(&mut self) {
        let Some(device_id) = self.station_version_query_queue.pop_front() else {
            return;
        };
        let Some(operations) = self.operations.clone() else {
            return;
        };
        match operations.begin_automatic_station_query(&self.port_config.port_number, device_id) {
            Ok(true) => {
                let result = self.perform_station_version_query(device_id);
                if let Err(error) = operations.report_station_query_result(
                    &self.port_config.port_number,
                    device_id,
                    result,
                ) {
                    error!(
                        "[版本查询状态保存失败] port={} device_id={} error={:?}",
                        self.serial_config.port_name, device_id, error
                    );
                }
            }
            Ok(false) => {}
            Err(error) => error!(
                "[版本查询启动失败] port={} device_id={} error={:?}",
                self.serial_config.port_name, device_id, error
            ),
        }
    }

    /// 执行专用版本寄存器事务。该路径不修改普通轮询 index、故障计数或 KKS 缓存。
    pub(crate) fn perform_station_version_query(
        &mut self,
        device_id: u8,
    ) -> Result<protocol::StationVersionResponse, String> {
        let index = self
            .port_config
            .devices
            .iter()
            .position(|device| device.config.device_id == device_id)
            .ok_or_else(|| "unknown device".to_string())?;
        if !self.port_config.status
            || !self.device_states[index].is_enabled
            || !self.device_states[index].read_enabled
        {
            return Err("port or device acquisition is disabled".into());
        }
        if self.current_run != RunLocation::Primary {
            return Err("standby gateway must not actively query station version".into());
        }
        if self.port.is_none() {
            self.reconnect();
        }
        if self.port.is_none() {
            return Err("serial port is not open".into());
        }

        self.prepare_automatic_query(device_id, index);
        let request = protocol::build_station_version_query(device_id);
        info!(
            "[版本查询发送] port={} device_id={} frame_hex={:?}",
            self.serial_config.port_name,
            device_id,
            format_hex(&request),
        );
        if !self.send_command(SendData {
            device_id: u32::from(device_id),
            command: request.to_vec(),
        }) {
            // A transport error can occur after the driver accepted part or all
            // of the frame.  If reconnect succeeded, isolate a possible late
            // response before ordinary acquisition resumes on the new handle.
            if self.port.is_some() {
                self.quarantine_late_station_version_response();
            }
            return Err("could not send station version query".into());
        }

        let mut received = Vec::with_capacity(STATION_VERSION_RESPONSE_LEN);
        let mut deadline = Instant::now() + self.serial_config.read_timeout;
        let mut completion_deadline_extended = false;
        let read_result: io::Result<()> = loop {
            if received.len() >= STATION_VERSION_RESPONSE_LEN {
                break Ok(());
            }
            let result = {
                let port = self.port.as_mut().expect("version query checked open port");
                read_before_deadline(port.as_mut(), &mut self.read_buf, deadline)
            };
            match result {
                Ok(0) => {
                    break Err(io::Error::new(
                        ErrorKind::TimedOut,
                        "empty version response",
                    ))
                }
                Ok(count) => {
                    received.extend_from_slice(&self.read_buf[..count]);
                    if !completion_deadline_extended {
                        deadline = Instant::now()
                            + wire_duration(
                                STATION_VERSION_RESPONSE_LEN,
                                self.serial_config.baud_rate,
                            )
                            .saturating_add(RESPONSE_COMPLETION_GUARD);
                        completion_deadline_extended = true;
                    }
                }
                Err(error) => break Err(error),
            }
        };

        match read_result {
            Ok(()) => {
                if received.len() != STATION_VERSION_RESPONSE_LEN {
                    let message = format!(
                        "station version response length must be exactly {STATION_VERSION_RESPONSE_LEN} bytes, got {}",
                        received.len()
                    );
                    if logging::serial_frame_logs_enabled() || !received.is_empty() {
                        info!(
                            "[版本查询响应] port={} device_id={} frame_len={} frame_hex={:?} valid=false",
                            self.serial_config.port_name,
                            device_id,
                            received.len(),
                            format_hex(&received),
                        );
                    }
                    self.quarantine_late_station_version_response();
                    return Err(message);
                }
                let frame = &received[..STATION_VERSION_RESPONSE_LEN];
                let result =
                    protocol::parse_station_version_response(frame).map_err(str::to_string);
                info!(
                    "[版本查询响应] port={} device_id={} frame_len={} frame_hex={:?} valid={}",
                    self.serial_config.port_name,
                    device_id,
                    received.len(),
                    format_hex(&received),
                    result.is_ok(),
                );
                if result.is_err() {
                    self.quarantine_late_station_version_response();
                } else {
                    self.rx_pending.clear();
                }
                result
            }
            Err(error) if error.kind() == ErrorKind::TimedOut => {
                self.quarantine_late_station_version_response();
                Err(format!(
                    "station version query timed out after {} ms",
                    self.serial_config.read_timeout.as_millis()
                ))
            }
            Err(error) => {
                let message = format!("station version read failed: {error}");
                self.port = None;
                self.rx_pending.clear();
                self.reconnect();
                if self.port.is_some() {
                    self.quarantine_late_station_version_response();
                }
                Err(message)
            }
        }
    }

    fn quarantine_late_station_version_response(&mut self) {
        self.rx_pending.clear();
        std::thread::sleep(STATION_VERSION_LATE_FRAME_QUARANTINE);
        if let Some(port) = self.port.as_ref() {
            if let Err(error) = port.clear(ClearBuffer::Input) {
                warn!(
                    "[版本查询迟到帧清理失败] port={} error={:?}",
                    self.serial_config.port_name, error
                );
            }
        }
        self.rx_pending.clear();
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
            warn!(
                "[串口发送失败] port={} device_id={} phase=validate cause=empty_command action=drop_command",
                self.serial_config.port_name, command.device_id
            );
            return false;
        }
        let required_timeout =
            minimum_transmit_timeout(command.command.len(), self.serial_config.baud_rate);
        if required_timeout > MAX_TRANSMIT_TIMEOUT {
            warn!(
                "[串口发送失败] port={} device_id={} phase=validate cause=frame_too_long frame_len={} estimated_timeout_ms={} limit_ms={} action=drop_command",
                self.serial_config.port_name,
                command.device_id,
                command.command.len(),
                required_timeout.as_millis(),
                MAX_TRANSMIT_TIMEOUT.as_millis(),
            );
            return false;
        }
        let frame_diagnostics = logging::serial_frame_logs_enabled();
        let send_started = Instant::now();
        if frame_diagnostics {
            info!(
                "[串口发送诊断开始] port={} device_id={} index={} role={:?} special={} baud_rate={} frame_len={} frame_hex={:?}",
                self.serial_config.port_name,
                command.device_id,
                self.index,
                self.current_run,
                self.port_config.is_special,
                self.serial_config.baud_rate,
                command.command.len(),
                format_hex(&command.command),
            );
        }

        // 特殊串口：直接写入全部数据
        if self.port_config.is_special {
            let result = self.write_segment(None, &command.command, Duration::ZERO, false);
            if let Err(error) = result {
                return self.handle_send_transport_error(
                    command.device_id,
                    "write_special_frame",
                    error,
                );
            }
            if frame_diagnostics {
                let queued_after = self
                    .port
                    .as_ref()
                    .and_then(|port| port.bytes_to_write().ok());
                info!(
                    "[串口发送诊断结束] port={} device_id={} index={} result=success mode=special elapsed_us={} queued_after={:?}",
                    self.serial_config.port_name,
                    command.device_id,
                    self.index,
                    send_started.elapsed().as_micros(),
                    queued_after,
                );
            }
            return true;
        }

        // 普通串口：地址字节使用 Mark 校验，数据部分使用 Space 校验
        let addr_byte = [command.command[0]];
        let payload = &command.command[1..];

        // 发送地址字节（Mark 奇偶校验）
        let address_started = Instant::now();
        if let Err(error) = self.write_segment(
            Some(Parity::Mark),
            &addr_byte,
            self.serial_config.parity_switch_delay,
            true,
        ) {
            return self.handle_send_transport_error(
                command.device_id,
                "write_address_mark",
                error,
            );
        }
        if frame_diagnostics {
            let queued_after = self
                .port
                .as_ref()
                .and_then(|port| port.bytes_to_write().ok());
            info!(
                "[串口发送诊断阶段] port={} device_id={} index={} phase=address_mark bytes={} elapsed_us={} queued_after={:?}",
                self.serial_config.port_name,
                command.device_id,
                self.index,
                addr_byte.len(),
                address_started.elapsed().as_micros(),
                queued_after,
            );
        }

        // 发送数据部分（Space 奇偶校验）
        let payload_started = Instant::now();
        if let Err(error) = self.write_segment(Some(Parity::Space), payload, Duration::ZERO, true) {
            return self.handle_send_transport_error(
                command.device_id,
                "write_payload_space",
                error,
            );
        }
        if frame_diagnostics {
            let queued_after = self
                .port
                .as_ref()
                .and_then(|port| port.bytes_to_write().ok());
            info!(
                "[串口发送诊断结束] port={} device_id={} index={} result=success mode=mark_space rx_parity=Space payload_bytes={} payload_elapsed_us={} total_elapsed_us={} queued_after={:?}",
                self.serial_config.port_name,
                command.device_id,
                self.index,
                payload.len(),
                payload_started.elapsed().as_micros(),
                send_started.elapsed().as_micros(),
                queued_after,
            );
        }

        true
    }

    /// 在有限总时限内写入一段数据；普通串口在每段写入后按波特率和字节数等待
    /// 线速发送时间，并增加驱动缓冲余量。XR17 驱动的 `tcdrain` 可能永久阻塞，
    /// 因此这里不能调用 `flush`。
    fn write_segment(
        &mut self,
        parity: Option<Parity>,
        data: &[u8],
        pre_write_delay: Duration,
        wait_for_settle: bool,
    ) -> io::Result<()> {
        let segment_started = Instant::now();
        let timeout = transmit_timeout(
            data.len(),
            self.serial_config.baud_rate,
            self.serial_config.read_timeout,
        );
        let deadline = Instant::now() + timeout;
        let port = self
            .port
            .as_mut()
            .ok_or_else(|| io::Error::new(ErrorKind::NotConnected, "serial port is not open"))?;

        if let Some(parity) = parity {
            port.set_parity(parity).map_err(|error| {
                io::Error::other(format!(
                    "stage=set_parity target_parity={parity:?} serial_error_kind={:?} error={error}",
                    error.kind()
                ))
            })?;
        }
        if !pre_write_delay.is_zero() {
            std::thread::sleep(pre_write_delay);
        }
        write_all_before_deadline(port.as_mut(), data, deadline).map_err(|error| {
            io_error_with_context(
                error,
                format!(
                    "stage=write data_len={} elapsed_us={}",
                    data.len(),
                    segment_started.elapsed().as_micros()
                ),
            )
        })?;
        if wait_for_settle {
            wait_for_output_settle(
                port.as_ref(),
                data.len(),
                self.serial_config.baud_rate,
                self.serial_config.transmit_settle_guard,
                deadline,
            )
            .map_err(|error| {
                io_error_with_context(
                    error,
                    format!(
                        "stage=wait_output_settle data_len={} elapsed_us={}",
                        data.len(),
                        segment_started.elapsed().as_micros()
                    ),
                )
            })?;
        }
        Ok(())
    }

    fn handle_send_transport_error(
        &mut self,
        device_id: u32,
        phase: &str,
        error: io::Error,
    ) -> bool {
        let error_kind = error.kind();
        if error_kind == ErrorKind::NotConnected {
            // 首次打开/上一次 I/O 失败已经由重连故障周期记录；冷却期间不为每台
            // 设备重复打印同一个“端口未打开”。
            self.reconnect();
            return false;
        }
        let error_text = error.to_string();
        let (port_timeout_ms, queued_rx, queued_tx, current_parity) = self
            .port
            .as_ref()
            .map(|port| {
                (
                    Some(port.timeout().as_millis()),
                    port.bytes_to_read().ok(),
                    port.bytes_to_write().ok(),
                    port.parity().ok(),
                )
            })
            .unwrap_or((None, None, None, None));
        error!(
            "[串口发送失败] port={} device_id={} index={} role={:?} phase={} error_kind={:?} error={:?} baud_rate={} configured_read_timeout_ms={} port_timeout_ms={:?} current_parity={:?} queued_rx={:?} queued_tx={:?} rx_pending_len={} action=close_publish_all_offline_reconnect",
            self.serial_config.port_name,
            device_id,
            self.index,
            self.current_run,
            phase,
            error_kind,
            error_text,
            self.serial_config.baud_rate,
            self.serial_config.read_timeout.as_millis(),
            port_timeout_ms,
            current_parity,
            queued_rx,
            queued_tx,
            self.rx_pending.len(),
        );
        self.log_record(format!(
            "串口发送失败: device_id={} phase={} error_kind={:?} error={:?}",
            device_id, phase, error_kind, error_text
        ));
        self.port = None;
        self.rx_pending.clear();
        self.reconnect();
        false
    }

    // ---- 数据接收与解析 ----

    /// 读取命令 ACK 回复。
    ///
    /// 该路径只用于客户端下发命令后的 ACK 判断，不进入采集帧解析，也不更新
    /// 设备超时或解析失败计数。普通 ACK 超时和内容不匹配不改变缓存或广播状态；
    /// 串口未打开或发生非超时 I/O 错误时会进入重连流程并发布整口离线状态。
    pub(crate) fn receive_command_ack(&mut self) -> bool {
        if self.port.is_none() {
            warn!(
                "[串口ACK读取失败] port={} phase=before_read cause=port_not_open action=publish_all_offline_reconnect",
                self.serial_config.port_name
            );
            self.log_record("命令 ACK 读取失败：串口未打开".into());
            self.reconnect();
            return false;
        }

        let deadline = Instant::now() + self.serial_config.read_timeout;
        let mut received = Vec::with_capacity(self.read_buf.len());
        let read_result = loop {
            let result = {
                let port = self.port.as_mut().expect("端口已在读取前检查");
                read_before_deadline(port.as_mut(), &mut self.read_buf, deadline)
            };
            match result {
                Ok(0) => break Ok(false),
                Ok(n) => {
                    received.extend_from_slice(&self.read_buf[..n]);
                    if contains_command_ack(&received) {
                        break Ok(true);
                    }
                    if received.len() >= self.read_buf.len() {
                        break Ok(false);
                    }
                }
                Err(error) if error.kind() == ErrorKind::TimedOut => break Ok(false),
                Err(error) => break Err(error),
            }
        };

        match read_result {
            Ok(true) => {
                if logging::serial_frame_logs_enabled() {
                    info!(
                        "串口 {} 命令 ACK: {}",
                        self.serial_config.port_name,
                        format_hex(&received)
                    );
                }
                return true;
            }
            Ok(false) => {}
            Err(error) => {
                let error_kind = error.kind();
                let error_text = error.to_string();
                error!(
                    "[串口ACK读取错误] port={} phase=read_ack error_kind={:?} error={:?} action=close_publish_all_offline_reconnect",
                    self.serial_config.port_name,
                    error_kind,
                    error_text,
                );
                self.log_record(format!(
                    "读取命令 ACK 错误: error_kind={:?} error={:?}",
                    error_kind, error_text
                ));
                self.port = None;
                self.rx_pending.clear();
                self.reconnect();
                return false;
            }
        }

        if received.is_empty() {
            warn!(
                "[串口ACK超时] port={} timeout_ms={} received_len=0 action=retry_command_if_configured",
                self.serial_config.port_name,
                self.serial_config.read_timeout.as_millis(),
            );
        } else {
            warn!(
                "[串口ACK不匹配] port={} timeout_ms={} received_len={} received_hex={:?} action=retry_command_if_configured",
                self.serial_config.port_name,
                self.serial_config.read_timeout.as_millis(),
                received.len(),
                format_hex(&received),
            );
        }
        false
    }

    /// 从串口读取并解析一帧数据；超时或坏帧立即按原有语义提交故障。
    pub(crate) fn receive_data(&mut self) {
        if let ReceiveDataOutcome::Retryable(failure) = self.receive_data_attempt(1) {
            self.commit_query_failure(failure, None);
        }
    }

    /// 接收主机自动查询响应；仅在显式配置时对首轮超时或坏帧补发一次。
    pub(crate) fn receive_query_with_retry(&mut self, query: SendData, retry_count: u32) {
        let query_index = self.index;
        let Some(device) = self.port_config.devices.get(query_index) else {
            return;
        };
        let device_id = device.config.device_id;
        let started_at = Instant::now();

        let first_failure = match self.receive_data_attempt(1) {
            ReceiveDataOutcome::Success => return,
            ReceiveDataOutcome::Retryable(failure) => failure,
            ReceiveDataOutcome::TransportHandled | ReceiveDataOutcome::UnattributedHandled => {
                return
            }
        };

        if retry_count == 0 {
            self.commit_query_failure(first_failure, None);
            return;
        }

        // 临时主机在接收期间可能因检测到有效转发头切回备机，此时禁止主动补发。
        if self.current_run != RunLocation::Primary || self.index != query_index {
            self.commit_query_failure(first_failure, None);
            return;
        }

        let frame_diagnostics = logging::serial_frame_logs_enabled();
        let (pending_hex, pending_truncated) =
            format_hex_preview(&self.rx_pending, BAD_FRAME_LOG_MAX_BYTES);
        if let Some(port) = self.port.as_ref() {
            let queued_before = frame_diagnostics
                .then(|| port.bytes_to_read().ok())
                .flatten();
            let clear_result = port.clear(ClearBuffer::Input);
            let queued_after = frame_diagnostics
                .then(|| port.bytes_to_read().ok())
                .flatten();
            if let Err(error) = &clear_result {
                warn!(
                    "[串口查询补发清理失败] port={} device_id={} index={} error={:?} action=continue_resend",
                    self.serial_config.port_name,
                    device_id,
                    query_index,
                    error,
                );
            }
            if frame_diagnostics {
                info!(
                    "[串口补发清理诊断] port={} device_id={} index={} first_receive_seq={} retry_receive_seq={} result={} kernel_bytes_before={:?} kernel_bytes_after={:?} pending_len={} pending_hex={:?} pending_truncated={} action=clear_kernel_and_memory_before_retry",
                    self.serial_config.port_name,
                    device_id,
                    query_index,
                    self.receive_sequence,
                    self.receive_sequence.wrapping_add(1),
                    if clear_result.is_ok() { "success" } else { "failed" },
                    queued_before,
                    queued_after,
                    self.rx_pending.len(),
                    pending_hex,
                    pending_truncated,
                );
            }
        }
        // 首次失败后内存缓冲中可能还留有该次响应的尾部或粘连帧。补发前必须和
        // 内核输入队列一起清掉，避免把旧响应当成补发响应。
        self.rx_pending.clear();

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

        match self.receive_data_attempt(2) {
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
    fn receive_data_attempt(&mut self, attempt: u8) -> ReceiveDataOutcome {
        self.receive_sequence = self.receive_sequence.wrapping_add(1);
        let sequence = self.receive_sequence;
        let receive_started = Instant::now();
        let frame_diagnostics = logging::serial_frame_logs_enabled();
        let device_id = self
            .port_config
            .devices
            .get(self.index)
            .map(|device| device.config.device_id)
            .unwrap_or_default();
        let configured_frame_len_range = self.port_config.devices.get(self.index).map(|device| {
            (
                device.config.data_len as usize + RESPONSE_FRAME_OVERHEAD_WITH_STATUS_LEN,
                device.config.data_len as usize + RESPONSE_FRAME_OVERHEAD,
            )
        });
        let (pending_hex, pending_truncated) =
            format_hex_preview(&self.rx_pending, BAD_FRAME_LOG_MAX_BYTES);
        let kernel_bytes_at_start = frame_diagnostics
            .then(|| {
                self.port
                    .as_ref()
                    .and_then(|port| port.bytes_to_read().ok())
            })
            .flatten();
        if frame_diagnostics {
            info!(
                "[串口接收诊断开始] port={} seq={} attempt={} device_id={} index={} role={:?} timeout_ms={} rx_parity={} configured_frame_len_range={:?} kernel_bytes={:?} pending_len={} pending_hex={:?} pending_truncated={}",
                self.serial_config.port_name,
                sequence,
                attempt,
                device_id,
                self.index,
                self.current_run,
                self.serial_config.read_timeout.as_millis(),
                if self.port_config.is_special { format!("{:?}", self.serial_config.parity) } else { "Space".to_string() },
                configured_frame_len_range,
                kernel_bytes_at_start,
                self.rx_pending.len(),
                pending_hex,
                pending_truncated,
            );
        }
        if self.port.is_none() {
            self.reconnect();
            return ReceiveDataOutcome::TransportHandled;
        }

        // 50 ms 是“等待响应开始”的截止时间。若截止前已收到首字节，只按当前
        // 波特率和最大合法帧长追加一次收帧预算，避免晚到的合法帧被截断，同时
        // 防止设备持续零星发送字节时不断刷新完整超时窗口。
        let completion_budget =
            response_completion_budget(&self.port_config, self.index, self.serial_config.baud_rate);
        let mut deadline = Instant::now() + self.serial_config.read_timeout;
        let mut completion_deadline_extended = false;
        if !self.rx_pending.is_empty() {
            let completion_deadline = Instant::now() + completion_budget;
            if completion_deadline > deadline {
                deadline = completion_deadline;
            }
            completion_deadline_extended = true;
        }
        let mut read_call = 0u32;
        let raw_frame = loop {
            if let Some(frame_len) = complete_frame_len(
                &self.rx_pending,
                &self.current_run,
                &self.run_on,
                configured_response_frame_len(
                    &self.rx_pending,
                    &self.port_config,
                    self.index,
                    &self.current_run,
                    &self.run_on,
                ),
            ) {
                break self.rx_pending.drain(..frame_len).collect::<Vec<_>>();
            }

            if is_complete_short_direct_candidate(&self.rx_pending, &self.current_run, &self.run_on)
            {
                // 临时主机的短直连帧可能与转发头前半段重合。只在驱动队列确实为空时
                // 提前结束，否则继续读取完整转发前缀。
                std::thread::sleep(FORWARD_PREFIX_GRACE);
                let no_more_bytes = self
                    .port
                    .as_ref()
                    .is_some_and(|port| matches!(port.bytes_to_read(), Ok(0)));
                if no_more_bytes {
                    let frame_len = self.rx_pending[2] as usize + 5;
                    break self.rx_pending.drain(..frame_len).collect::<Vec<_>>();
                }
            }

            if self.rx_pending.len() >= self.read_buf.len() {
                break std::mem::take(&mut self.rx_pending);
            }

            read_call = read_call.saturating_add(1);
            let pending_before = self.rx_pending.len();
            let kernel_bytes_before = frame_diagnostics
                .then(|| {
                    self.port
                        .as_ref()
                        .and_then(|port| port.bytes_to_read().ok())
                })
                .flatten();
            let read_started = Instant::now();
            let read_result = {
                let port = self.port.as_mut().expect("端口已在读取前检查");
                read_before_deadline(port.as_mut(), &mut self.read_buf, deadline)
            };
            let read_elapsed = read_started.elapsed();
            let kernel_bytes_after = frame_diagnostics
                .then(|| {
                    self.port
                        .as_ref()
                        .and_then(|port| port.bytes_to_read().ok())
                })
                .flatten();
            match read_result {
                Ok(0) => {
                    if frame_diagnostics {
                        info!(
                            "[串口读取块诊断] port={} seq={} attempt={} read_call={} result=empty read_len=0 elapsed_us={} kernel_bytes_before={:?} kernel_bytes_after={:?} pending_before={} pending_after={} deadline_remaining_ms={}",
                            self.serial_config.port_name,
                            sequence,
                            attempt,
                            read_call,
                            read_elapsed.as_micros(),
                            kernel_bytes_before,
                            kernel_bytes_after,
                            pending_before,
                            self.rx_pending.len(),
                            deadline.saturating_duration_since(Instant::now()).as_millis(),
                        );
                    }
                    if self.rx_pending.is_empty() {
                        if frame_diagnostics {
                            info!(
                                "[串口接收诊断结束] port={} seq={} attempt={} device_id={} index={} result=read_timeout read_calls={} elapsed_ms={} pending_len=0 kernel_bytes_after={:?}",
                                self.serial_config.port_name,
                                sequence,
                                attempt,
                                device_id,
                                self.index,
                                read_call,
                                receive_started.elapsed().as_millis(),
                                kernel_bytes_after,
                            );
                        }
                        return ReceiveDataOutcome::Retryable(QueryFailure::ReadTimeout);
                    }
                    break std::mem::take(&mut self.rx_pending);
                }
                Ok(n) => {
                    self.rx_pending.extend_from_slice(&self.read_buf[..n]);
                    if !completion_deadline_extended {
                        let completion_deadline = Instant::now() + completion_budget;
                        if completion_deadline > deadline {
                            deadline = completion_deadline;
                        }
                        completion_deadline_extended = true;
                    }
                    if frame_diagnostics {
                        let (chunk_hex, chunk_truncated) =
                            format_hex_preview(&self.read_buf[..n], BAD_FRAME_LOG_MAX_BYTES);
                        info!(
                            "[串口读取块诊断] port={} seq={} attempt={} read_call={} result=data read_len={} elapsed_us={} kernel_bytes_before={:?} kernel_bytes_after={:?} pending_before={} pending_after={} chunk_zero_bytes={} chunk_all_zero={} chunk_hex={:?} chunk_truncated={} deadline_remaining_ms={}",
                            self.serial_config.port_name,
                            sequence,
                            attempt,
                            read_call,
                            n,
                            read_elapsed.as_micros(),
                            kernel_bytes_before,
                            kernel_bytes_after,
                            pending_before,
                            self.rx_pending.len(),
                            self.read_buf[..n].iter().filter(|byte| **byte == 0).count(),
                            self.read_buf[..n].iter().all(|byte| *byte == 0),
                            chunk_hex,
                            chunk_truncated,
                            deadline.saturating_duration_since(Instant::now()).as_millis(),
                        );
                    }
                }
                Err(error) if error.kind() == ErrorKind::TimedOut => {
                    if frame_diagnostics {
                        info!(
                            "[串口读取块诊断] port={} seq={} attempt={} read_call={} result=timeout read_len=0 elapsed_us={} kernel_bytes_before={:?} kernel_bytes_after={:?} pending_before={} pending_after={} deadline_remaining_ms={}",
                            self.serial_config.port_name,
                            sequence,
                            attempt,
                            read_call,
                            read_elapsed.as_micros(),
                            kernel_bytes_before,
                            kernel_bytes_after,
                            pending_before,
                            self.rx_pending.len(),
                            deadline.saturating_duration_since(Instant::now()).as_millis(),
                        );
                    }
                    if self.rx_pending.is_empty() {
                        if frame_diagnostics {
                            info!(
                                "[串口接收诊断结束] port={} seq={} attempt={} device_id={} index={} result=read_timeout read_calls={} elapsed_ms={} pending_len=0 kernel_bytes_after={:?}",
                                self.serial_config.port_name,
                                sequence,
                                attempt,
                                device_id,
                                self.index,
                                read_call,
                                receive_started.elapsed().as_millis(),
                                kernel_bytes_after,
                            );
                        }
                        return ReceiveDataOutcome::Retryable(QueryFailure::ReadTimeout);
                    }
                    break std::mem::take(&mut self.rx_pending);
                }
                Err(error) => {
                    let error_kind = error.kind();
                    let error_text = error.to_string();
                    error!(
                        "[串口读取错误] port={} seq={} attempt={} device_id={} index={} role={:?} phase=read_frame read_call={} elapsed_ms={} pending_len={} error_kind={:?} error={:?} action=close_publish_all_offline_reconnect",
                        self.serial_config.port_name,
                        sequence,
                        attempt,
                        device_id,
                        self.index,
                        self.current_run,
                        read_call,
                        receive_started.elapsed().as_millis(),
                        self.rx_pending.len(),
                        error_kind,
                        error_text,
                    );
                    self.log_record(format!(
                        "串口读取错误: index={} error_kind={:?} error={:?}",
                        self.index, error_kind, error_text
                    ));
                    self.port = None;
                    self.rx_pending.clear();
                    self.reconnect();
                    return ReceiveDataOutcome::TransportHandled;
                }
            }
        };

        if raw_frame.is_empty() {
            if frame_diagnostics {
                info!(
                    "[串口接收诊断结束] port={} seq={} attempt={} device_id={} index={} result=read_timeout reason=empty_raw_frame read_calls={} elapsed_ms={}",
                    self.serial_config.port_name,
                    sequence,
                    attempt,
                    device_id,
                    self.index,
                    read_call,
                    receive_started.elapsed().as_millis(),
                );
            }
            return ReceiveDataOutcome::Retryable(QueryFailure::ReadTimeout);
        }

        // ---- 提取有效数据切片 ----
        let (frame, device_attributed) = self.extract_frame(&raw_frame);
        let discarded_tail_len = self.rx_pending.len();
        let (discarded_tail_hex, discarded_tail_truncated) =
            format_hex_preview(&self.rx_pending, BAD_FRAME_LOG_MAX_BYTES);
        // 主机发送查询时只接受本轮的一帧响应。任何粘连尾部都属于迟到/重复数据，
        // 必须丢弃，防止下一台设备的轮询错误消费；备机被动监听则保留后续帧。
        if self.current_run == RunLocation::Primary {
            self.rx_pending.clear();
        }
        if frame_diagnostics {
            let (raw_hex, raw_truncated) = format_hex_preview(&raw_frame, BAD_FRAME_LOG_MAX_BYTES);
            let (frame_hex, frame_truncated) = format_hex_preview(&frame, BAD_FRAME_LOG_MAX_BYTES);
            let leading_zero_bytes = frame.iter().take_while(|byte| **byte == 0).count();
            let zero_bytes = frame.iter().filter(|byte| **byte == 0).count();
            info!(
                "[串口组帧诊断] port={} seq={} attempt={} device_id={} index={} read_calls={} raw_len={} raw_hex={:?} raw_truncated={} frame_len={} frame_hex={:?} frame_truncated={} declared_len={:?} expected_from_header={:?} configured_frame_len_range={:?} attributed={} zero_bytes={} all_zero={} leading_zero_bytes={} discarded_tail_len={} discarded_tail_hex={:?} discarded_tail_truncated={} elapsed_ms={}",
                self.serial_config.port_name,
                sequence,
                attempt,
                device_id,
                self.index,
                read_call,
                raw_frame.len(),
                raw_hex,
                raw_truncated,
                frame.len(),
                frame_hex,
                frame_truncated,
                frame.get(2),
                frame.get(2).map(|len| *len as usize + 5),
                configured_frame_len_range,
                device_attributed,
                zero_bytes,
                !frame.is_empty() && zero_bytes == frame.len(),
                leading_zero_bytes,
                discarded_tail_len,
                discarded_tail_hex,
                discarded_tail_truncated,
                receive_started.elapsed().as_millis(),
            );
        }

        // 备机监听到的无有效转发头数据无法归属到具体设备，不能沿用旧 index。
        if !device_attributed {
            if frame_diagnostics {
                info!(
                    "[串口接收诊断结束] port={} seq={} attempt={} device_id={} index={} result=unattributed_frame frame_len={} read_calls={} elapsed_ms={}",
                    self.serial_config.port_name,
                    sequence,
                    attempt,
                    device_id,
                    self.index,
                    frame.len(),
                    read_call,
                    receive_started.elapsed().as_millis(),
                );
            }
            self.handle_unattributed_frame(&frame);
            return ReceiveDataOutcome::UnattributedHandled;
        }

        // ---- 解析数据帧 ----
        match protocol::parse_data_packet(&frame) {
            Ok(packet) => {
                if frame_diagnostics {
                    info!(
                        "[串口接收诊断结束] port={} seq={} attempt={} device_id={} index={} result=success frame_len={} response_addr={:?} response_cmd={:?} read_calls={} elapsed_ms={} action=publish_good_value",
                        self.serial_config.port_name,
                        sequence,
                        attempt,
                        device_id,
                        self.index,
                        frame.len(),
                        frame.first(),
                        frame.get(1),
                        read_call,
                        receive_started.elapsed().as_millis(),
                    );
                }
                self.handle_valid_packet(packet, frame);
                ReceiveDataOutcome::Success
            }
            Err(reason) => {
                if frame_diagnostics {
                    info!(
                        "[串口接收诊断结束] port={} seq={} attempt={} device_id={} index={} result=bad_frame reason={:?} frame_len={} response_addr={:?} response_cmd={:?} read_calls={} elapsed_ms={}",
                        self.serial_config.port_name,
                        sequence,
                        attempt,
                        device_id,
                        self.index,
                        reason,
                        frame.len(),
                        frame.first(),
                        frame.get(1),
                        read_call,
                        receive_started.elapsed().as_millis(),
                    );
                }
                ReceiveDataOutcome::Retryable(QueryFailure::BadFrame {
                    reason: reason.to_string(),
                    frame,
                })
            }
        }
    }

    fn handle_valid_packet(&mut self, packet: protocol::DataPacket, frame: Vec<u8>) {
        // This gate covers both active polling and passive secondary frames.
        // A stopped station must not become Good because its peer still polls.
        if !self.port_config.status
            || !self.device_states[self.index].is_enabled
            || !self.device_states[self.index].read_enabled
        {
            self.send_zero_status_for_index(
                self.index,
                self.port_config.status && self.port.is_some(),
                false,
            );
            return;
        }
        // 只有长度与校验和均有效的完整帧才能证明通信恢复。
        let now = Instant::now();
        self.log_device_recovery(now, frame.len());
        self.log_unattributed_recovery(now, frame.len());
        self.device_states[self.index].timeout_count = 0;
        self.time_out_count = 0;
        self.device_states[self.index].parse_fail_count = 0;
        self.parse_fail_count = 0;
        self.device_states[self.index].last_data_at = Some(chrono::Utc::now().to_rfc3339());

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

    /// 从一段完整原始帧中提取协议数据帧。
    ///
    /// 备机模式下会解析转发头以更新 `index`，并剥离前缀。
    /// 如果当前由备机临时升级为主机，检测到有效转发头说明真正主机已恢复，
    /// 则自动切回备机。
    fn extract_frame(&mut self, raw_frame: &[u8]) -> (Vec<u8>, bool) {
        let accepts_forwarded_frame =
            self.current_run == RunLocation::Secondary || self.run_on == RunLocation::Secondary;
        let forwarded = if accepts_forwarded_frame {
            validated_forward_data_start(raw_frame).and_then(|data_start| {
                let device_addr = raw_frame[0];
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
                let data = &raw_frame[data_start..];
                let effective_len = if data.len() >= 4 {
                    (data[2] as usize + 5).min(data.len())
                } else {
                    data.len()
                };
                (data[..effective_len].to_vec(), true)
            } else {
                (raw_frame.to_vec(), false)
            }
        } else {
            (raw_frame.to_vec(), true)
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
            .unwrap_or("-");
        let first_reason = context
            .and_then(|value| value.first_failure.reason())
            .unwrap_or("-");
        let first_frame = context
            .and_then(|value| value.first_failure.frame())
            .unwrap_or_default();
        let first_frame_len = first_frame.len();
        let (first_frame_hex, first_frame_truncated) =
            format_hex_preview(first_frame, BAD_FRAME_LOG_MAX_BYTES);
        let action = self.device_states[index]
            .failure_log
            .record_failure(kind, now);

        match action {
            FailureLogAction::Immediate(stats) => {
                match kind {
                    DeviceFailureKind::ReadTimeout => warn!(
                        "[串口通信异常] port={} device_id={} index={} role={:?} cause=read_timeout timeout_ms={} attempts={} first_cause={} first_reason={:?} first_frame_len={} first_frame_hex={:?} first_frame_truncated={} final_cause=read_timeout consecutive={} affected_points={} action=publish_zero_bad_no_communication",
                        self.serial_config.port_name,
                        device_id,
                        index,
                        self.current_run,
                        self.serial_config.read_timeout.as_millis(),
                        attempts,
                        first_cause,
                        first_reason,
                        first_frame_len,
                        first_frame_hex,
                        first_frame_truncated,
                        consecutive,
                        affected_points,
                    ),
                    DeviceFailureKind::BadFrame => {
                        let frame = frame.unwrap_or_default();
                        let details =
                            bad_frame_log_details(reason.unwrap_or("未知解析错误"), frame);
                        warn!(
                            "[串口坏帧] port={} device_id={} index={} role={:?} reason={:?} frame_len={} frame_hex={:?} truncated={} attempts={} first_cause={} first_reason={:?} first_frame_len={} first_frame_hex={:?} first_frame_truncated={} final_cause=bad_frame consecutive={} parse_failures={} affected_points={} action=publish_zero_bad_no_communication",
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
                            first_frame_hex,
                            first_frame_truncated,
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

fn serial_error_to_io(error: serialport::Error) -> io::Error {
    io::Error::other(error.to_string())
}

fn io_error_with_context(error: io::Error, context: String) -> io::Error {
    let kind = error.kind();
    io::Error::new(kind, format!("{context} error={error}"))
}

/// 在统一截止时间内执行一次 read，并在结束后恢复端口原始超时配置。
fn read_before_deadline(
    port: &mut dyn SerialPort,
    buffer: &mut [u8],
    deadline: Instant,
) -> io::Result<usize> {
    let remaining = deadline.saturating_duration_since(Instant::now());
    if remaining.is_zero() {
        return Err(io::Error::new(
            ErrorKind::TimedOut,
            "frame read deadline elapsed",
        ));
    }

    let original_timeout = port.timeout();
    port.set_timeout(remaining).map_err(serial_error_to_io)?;
    let result = port.read(buffer);
    let restore_result = port
        .set_timeout(original_timeout)
        .map_err(serial_error_to_io);
    match (result, restore_result) {
        (_, Err(error)) => Err(error),
        (result, Ok(())) => result,
    }
}

/// 在统一截止时间内写完一段数据，避免 `write_all` 的逐次超时被零星写入反复续期。
fn write_all_before_deadline(
    port: &mut dyn SerialPort,
    mut data: &[u8],
    deadline: Instant,
) -> io::Result<()> {
    while !data.is_empty() {
        let remaining = deadline.saturating_duration_since(Instant::now());
        if remaining.is_zero() {
            return Err(io::Error::new(
                ErrorKind::TimedOut,
                "serial write deadline elapsed",
            ));
        }

        let original_timeout = port.timeout();
        port.set_timeout(remaining).map_err(serial_error_to_io)?;
        let result = port.write(data);
        let restore_result = port
            .set_timeout(original_timeout)
            .map_err(serial_error_to_io);
        let written = match (result, restore_result) {
            (_, Err(error)) => return Err(error),
            (result, Ok(())) => result?,
        };
        if written == 0 {
            return Err(io::Error::new(
                ErrorKind::WriteZero,
                "serial write returned zero bytes",
            ));
        }
        data = &data[written..];
    }
    Ok(())
}

fn wire_duration(byte_count: usize, baud_rate: u32) -> Duration {
    if byte_count == 0 || baud_rate == 0 {
        return Duration::ZERO;
    }
    // 起始位 + 8 数据位 + 校验位 + 最多 2 个停止位，使用 12 bit/byte 的保守值。
    let bit_count = (byte_count as u128).saturating_mul(12);
    let nanos = bit_count
        .saturating_mul(1_000_000_000)
        .div_ceil(u128::from(baud_rate))
        .min(u128::from(u64::MAX));
    Duration::from_nanos(nanos as u64)
}

/// 首字节已经到达后，用线速计算一次性的完整收帧预算。
///
/// 始终包含转发前缀，既覆盖直连帧，也覆盖主备切换期间可能出现的转发帧。
fn response_completion_budget(
    port_config: &SerialPortConfig,
    device_index: usize,
    baud_rate: u32,
) -> Duration {
    let maximum_frame_len = port_config
        .devices
        .get(device_index)
        .map(|device| {
            usize::from(device.config.data_len)
                .saturating_add(RESPONSE_FRAME_OVERHEAD)
                .saturating_add(FORWARD_PREFIX_LEN)
        })
        .unwrap_or(RESPONSE_FRAME_OVERHEAD + FORWARD_PREFIX_LEN);
    wire_duration(maximum_frame_len, baud_rate).saturating_add(RESPONSE_COMPLETION_GUARD)
}

fn transmit_wire_budget(byte_count: usize, baud_rate: u32) -> Duration {
    wire_duration(byte_count, baud_rate)
        .saturating_add(wire_duration(byte_count.min(16), baud_rate))
}

fn minimum_transmit_timeout(byte_count: usize, baud_rate: u32) -> Duration {
    transmit_wire_budget(byte_count, baud_rate).saturating_add(MIN_TRANSMIT_IO_SLACK)
}

fn transmit_timeout(byte_count: usize, baud_rate: u32, configured: Duration) -> Duration {
    let wire_budget = transmit_wire_budget(byte_count, baud_rate);
    // 读取超时只是一个可复用的 I/O 余量，不得因为它大于发送保护上限就把所有
    // 短命令误判为“帧过长”。余量限制在剩余的 5 秒总预算内。
    let remaining_budget = MAX_TRANSMIT_TIMEOUT.saturating_sub(wire_budget);
    let io_slack = configured.max(MIN_TRANSMIT_IO_SLACK).min(remaining_budget);
    wire_budget.saturating_add(io_slack)
}

/// 等待驱动发送队列清空，再按完整段线速时间和短保护时间等待 UART 发完。
/// `bytes_to_write` 不受驱动支持时仍使用线速时间兜底；不调用可能永久阻塞的 flush/tcdrain。
fn wait_for_output_settle(
    port: &dyn SerialPort,
    byte_count: usize,
    baud_rate: u32,
    settle_guard: Duration,
    deadline: Instant,
) -> io::Result<()> {
    while matches!(port.bytes_to_write(), Ok(queued) if queued > 0) {
        let remaining = deadline.saturating_duration_since(Instant::now());
        if remaining.is_zero() {
            return Err(io::Error::new(
                ErrorKind::TimedOut,
                "serial output queue drain deadline elapsed",
            ));
        }
        std::thread::sleep(OUTPUT_QUEUE_POLL_INTERVAL.min(remaining));
    }

    // 即使驱动报告队列为 0，字节仍可能已经进入 UART FIFO/移位寄存器；完整
    // 线速时间是安全上界，额外保护时间用于校验位切换和 USB/XR17 调度抖动。
    let settle = wire_duration(byte_count, baud_rate).saturating_add(settle_guard);
    let remaining = deadline.saturating_duration_since(Instant::now());
    if settle > remaining {
        return Err(io::Error::new(
            ErrorKind::TimedOut,
            "serial output settle deadline elapsed",
        ));
    }
    if !settle.is_zero() {
        std::thread::sleep(settle);
    }
    Ok(())
}

// ============================================================
//  帧长度计算
// ============================================================

fn complete_frame_len(
    buf: &[u8],
    current_run: &RunLocation,
    run_on: &RunLocation,
    configured_minimum: Option<usize>,
) -> Option<usize> {
    if buf.len() < 3 {
        return None;
    }
    let expected = configured_minimum
        .map(|minimum| expected_frame_len(buf, current_run, run_on).max(minimum))
        .unwrap_or_else(|| expected_frame_len(buf, current_run, run_on));
    (buf.len() >= expected).then_some(expected)
}

fn configured_response_frame_len(
    buf: &[u8],
    port_config: &SerialPortConfig,
    current_index: usize,
    current_run: &RunLocation,
    run_on: &RunLocation,
) -> Option<usize> {
    let accepts_forwarded_frame =
        *current_run == RunLocation::Secondary || *run_on == RunLocation::Secondary;
    if accepts_forwarded_frame {
        let Some(data_start) = validated_forward_data_start(buf) else {
            return (*current_run == RunLocation::Primary)
                .then(|| {
                    port_config.devices.get(current_index).map(|device| {
                        configured_protocol_frame_len(buf, 0, device.config.data_len as usize)
                    })
                })
                .flatten();
        };
        let device_id = *buf.first()?;
        return port_config
            .devices
            .iter()
            .find(|device| device.config.device_id == device_id)
            .map(|device| {
                data_start
                    + configured_protocol_frame_len(
                        buf,
                        data_start,
                        device.config.data_len as usize,
                    )
            });
    }

    (*current_run == RunLocation::Primary)
        .then(|| {
            port_config.devices.get(current_index).map(|device| {
                configured_protocol_frame_len(buf, 0, device.config.data_len as usize)
            })
        })
        .flatten()
}

/// 同时兼容两种已投入使用的 `data_len` 含义：
/// - 等于协议 Len 字段：总长为 `data_len + 5`；
/// - 等于实际状态区长度（Len + 1）：总长为 `data_len + 4`。
fn configured_protocol_frame_len(
    buf: &[u8],
    data_start: usize,
    configured_data_len: usize,
) -> usize {
    let overhead = match buf.get(data_start + 2) {
        Some(declared_len) if *declared_len as usize + 1 == configured_data_len => {
            RESPONSE_FRAME_OVERHEAD_WITH_STATUS_LEN
        }
        _ => RESPONSE_FRAME_OVERHEAD,
    };
    configured_data_len + overhead
}

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
        && buf.len() == expected
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
    use crate::protocol::cmd;
    use std::collections::HashMap;
    use std::io::Read;

    use crate::csv_parser::{BitIndex, Config as CsvConfig, DeviceConfig, Record};
    use crate::model::Value;
    use serialport::FlowControl;

    fn test_device(device_id: u8) -> DeviceConfig {
        DeviceConfig {
            config: CsvConfig {
                com: "COM1".to_string(),
                baud_rate: 28_800,
                is_special: true,
                com_index: device_id,
                device_id,
                data_len: 3,
                kks_prefix: String::new(),
            },
            records: vec![Record {
                kks: format!("POINT_{device_id}"),
                type_: "uint".to_string(),
                _f_type: String::new(),
                byte_index: 1,
                bit_index: BitIndex::Range(0..=7),
                _def: 0,
                _max: 0,
                _min: 0,
                byte_order: 0,
            }],
        }
    }

    fn test_manager(run_on: RunLocation, current_run: RunLocation) -> SerialManager {
        let device = test_device(1);

        SerialManager {
            port: None,
            operations: None,
            command_queue: Arc::new(Mutex::new(VecDeque::new())),
            station_version_query_queue: VecDeque::new(),
            serial_config: SerialConfig::new(
                "COM1".to_string(),
                28_800,
                DataBits::Eight,
                StopBits::One,
                Parity::None,
                Duration::from_millis(100),
            ),
            port_config: SerialPortConfig::new("COM1".to_string(), 28_800, true, vec![device]),
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
            rx_pending: Vec::new(),
            last_reconnect: Instant::now(),
            port_offline_published: false,
            reconnect_failure_log: ReconnectFailureLogState::default(),
            unattributed_failure_log: DeviceFailureLogState::default(),
            receive_sequence: 0,
        }
    }

    fn test_manager_with_devices(device_count: u8) -> SerialManager {
        let mut manager = test_manager(RunLocation::Primary, RunLocation::Primary);
        manager.port_config.devices = (1..=device_count).map(test_device).collect();
        manager.device_states = (0..device_count)
            .map(|_| DeviceRuntimeState::new())
            .collect();
        manager
    }

    fn with_checksum(mut bytes: Vec<u8>) -> Vec<u8> {
        let checksum = crate::model::get_sum(&bytes);
        bytes.push(checksum);
        bytes
    }

    #[derive(Debug)]
    enum ReadStep {
        Data(Vec<u8>),
        DelayedData(Duration, Vec<u8>),
        Timeout,
        IoError,
    }

    #[derive(Debug, Default)]
    struct SerialObservations {
        write_attempts: usize,
        writes: Vec<Vec<u8>>,
        input_clears: usize,
        flush_attempts: usize,
        drops: usize,
    }

    struct ScriptedSerialPort {
        reads: VecDeque<ReadStep>,
        write_results: VecDeque<bool>,
        observations: Arc<Mutex<SerialObservations>>,
        timeout: Duration,
    }

    impl Drop for ScriptedSerialPort {
        fn drop(&mut self) {
            self.observations
                .lock()
                .unwrap_or_else(|e| e.into_inner())
                .drops += 1;
        }
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
                ReadStep::DelayedData(delay, mut data) => {
                    if delay > self.timeout {
                        std::thread::sleep(self.timeout);
                        self.reads
                            .push_front(ReadStep::DelayedData(delay - self.timeout, data));
                        return Err(std::io::Error::new(
                            std::io::ErrorKind::TimedOut,
                            "scripted delayed timeout",
                        ));
                    }
                    std::thread::sleep(delay);
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
            self.observations.lock().unwrap().flush_attempts += 1;
            Ok(())
        }
    }

    impl SerialPort for ScriptedSerialPort {
        fn name(&self) -> Option<String> {
            Some("scripted".to_string())
        }

        fn baud_rate(&self) -> serialport::Result<u32> {
            Ok(28_800)
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

    fn forwarded_status_frame(device_id: u8) -> Vec<u8> {
        let mut frame = with_checksum(vec![device_id, 2, 3, 4, 5, 6]);
        frame.extend_from_slice(&[0, 0]);
        frame.extend_from_slice(&valid_status_frame(device_id, cmd::READ_STATUS));
        frame
    }

    fn run_auto_query(manager: &mut SerialManager) {
        run_auto_query_with_retries(manager, 1);
    }

    fn run_auto_query_with_retries(manager: &mut SerialManager, retry_count: u32) {
        let query = status_query();
        manager.prepare_automatic_query(query.device_id as u8, manager.index);
        assert!(manager.send_command(query.clone()));
        manager.receive_query_with_retry(query, retry_count);
    }

    #[test]
    fn station_version_transaction_accepts_fragmented_valid_response_without_poll_side_effects() {
        let mut manager = test_manager(RunLocation::Primary, RunLocation::Primary);
        let observations = install_scripted_port(
            &mut manager,
            vec![
                ReadStep::Data(vec![0x00, 0x00, 0x04, 0x08]),
                ReadStep::Data(vec![0x01, 0x04, 0x00, 0x05, 0x16]),
            ],
            vec![true],
        );
        manager.device_states[0].timeout_count = 2;
        manager.device_states[0].parse_fail_count = 3;
        let original_index = manager.index;

        let result = manager.perform_station_version_query(1).unwrap();

        assert_eq!(
            result,
            protocol::StationVersionResponse::Domestic {
                payload: [1, 4, 0, 5],
                communication_module_version: 0x0104,
                plc_version: 0x0005,
            }
        );
        assert_eq!(manager.index, original_index);
        assert_eq!(manager.device_states[0].timeout_count, 2);
        assert_eq!(manager.device_states[0].parse_fail_count, 3);
        let observations = observations.lock().unwrap();
        assert_eq!(
            observations.writes,
            vec![protocol::build_station_version_query(1).to_vec()]
        );
    }

    #[test]
    fn station_version_timeout_is_isolated_from_normal_device_failure_counters() {
        let mut manager = test_manager(RunLocation::Primary, RunLocation::Primary);
        manager.serial_config.read_timeout = Duration::from_millis(1);
        let observations = install_scripted_port(&mut manager, vec![ReadStep::Timeout], vec![true]);
        manager.device_states[0].timeout_count = 2;
        manager.device_states[0].parse_fail_count = 3;

        let error = manager.perform_station_version_query(1).unwrap_err();

        assert!(error.contains("timed out"));
        assert_eq!(manager.device_states[0].timeout_count, 2);
        assert_eq!(manager.device_states[0].parse_fail_count, 3);
        assert!(observations.lock().unwrap().input_clears >= 2);
    }

    #[test]
    fn station_version_transaction_rejects_extra_bytes_as_non_strict_frame() {
        let mut manager = test_manager(RunLocation::Primary, RunLocation::Primary);
        let mut response = vec![0x00, 0x00, 0x04, 0x08, 0x01, 0x04, 0x00, 0x05, 0x16];
        response.push(0x00);
        let observations =
            install_scripted_port(&mut manager, vec![ReadStep::Data(response)], vec![true]);

        let error = manager.perform_station_version_query(1).unwrap_err();

        assert!(error.contains("exactly 9 bytes"));
        assert!(observations.lock().unwrap().input_clears >= 2);
    }

    #[test]
    fn configured_response_length_supports_both_data_len_meanings_and_modes() {
        let direct_manager = test_manager(RunLocation::Primary, RunLocation::Primary);
        assert_eq!(
            configured_response_frame_len(
                &[0, 0, 3],
                &direct_manager.port_config,
                0,
                &direct_manager.current_run,
                &direct_manager.run_on,
            ),
            Some(3 + RESPONSE_FRAME_OVERHEAD)
        );
        assert_eq!(
            configured_response_frame_len(
                &[0, 0, 2],
                &direct_manager.port_config,
                0,
                &direct_manager.current_run,
                &direct_manager.run_on,
            ),
            Some(3 + RESPONSE_FRAME_OVERHEAD_WITH_STATUS_LEN)
        );

        let forwarded_manager = test_manager(RunLocation::Secondary, RunLocation::Secondary);
        let mut forwarded = forwarded_status_frame(1);
        assert_eq!(
            configured_response_frame_len(
                &forwarded,
                &forwarded_manager.port_config,
                0,
                &forwarded_manager.current_run,
                &forwarded_manager.run_on,
            ),
            Some(FORWARD_PREFIX_LEN + 3 + RESPONSE_FRAME_OVERHEAD_WITH_STATUS_LEN)
        );

        forwarded[FORWARD_PREFIX_LEN + 2] = 3;
        assert_eq!(
            configured_response_frame_len(
                &forwarded,
                &forwarded_manager.port_config,
                0,
                &forwarded_manager.current_run,
                &forwarded_manager.run_on,
            ),
            Some(FORWARD_PREFIX_LEN + 3 + RESPONSE_FRAME_OVERHEAD)
        );

        // 配置在备机、当前临时切为主机时仍可能接收转发帧。
        let temporary_primary = test_manager(RunLocation::Secondary, RunLocation::Primary);
        assert_eq!(
            configured_response_frame_len(
                &forwarded,
                &temporary_primary.port_config,
                0,
                &temporary_primary.current_run,
                &temporary_primary.run_on,
            ),
            Some(FORWARD_PREFIX_LEN + 3 + RESPONSE_FRAME_OVERHEAD)
        );
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
    fn query_retry_log_limits_intermittent_successes_until_quiet_period() {
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

        assert_eq!(
            state.record_retry_success(
                DeviceFailureKind::ReadTimeout,
                started + Duration::from_secs(30)
            ),
            QueryRetryLogAction::Suppressed
        );

        let QueryRetryLogAction::Summary(summary) = state.record_retry_success(
            DeviceFailureKind::BadFrame,
            started + Duration::from_secs(62),
        ) else {
            panic!("60 秒后应生成查询补发持续汇总");
        };
        assert_eq!(summary.retries_succeeded, 5);
        assert_eq!(summary.first_timeouts, 3);
        assert_eq!(summary.first_bad_frames, 2);
        assert_eq!(summary.suppressed, 2);

        // 距离上次补发满 60 秒后，新的补发应开始全新的周期并立即记录。
        assert!(matches!(
            state.record_retry_success(
                DeviceFailureKind::ReadTimeout,
                started + Duration::from_secs(122)
            ),
            QueryRetryLogAction::Immediate(QueryRetryEpisodeStats {
                retries_succeeded: 1,
                first_timeouts: 1,
                first_bad_frames: 0,
                suppressed: 0,
                ..
            })
        ));
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

        let (extracted, attributed) = manager.extract_frame(&frame);

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

        let (extracted, attributed) = manager.extract_frame(&frame);

        assert_eq!(extracted, payload);
        assert!(attributed);
        assert_eq!(manager.current_run, RunLocation::Secondary);
    }

    #[test]
    fn secondary_preserves_and_processes_two_forwarded_frames_from_one_read() {
        let mut manager = test_manager_with_devices(2);
        manager.run_on = RunLocation::Secondary;
        manager.current_run = RunLocation::Secondary;
        let mut combined = forwarded_status_frame(1);
        combined.extend_from_slice(&forwarded_status_frame(2));
        install_scripted_port(&mut manager, vec![ReadStep::Data(combined)], vec![]);
        let mut rx = manager.broadcaster.subscribe();

        manager.receive_data();
        manager.receive_data();

        let first = rx.try_recv().expect("第一帧应广播");
        let second = rx.try_recv().expect("粘连的第二帧应在下一轮广播");
        assert_eq!((first.device_id, second.device_id), (1, 2));
        assert!(first.device_status && second.device_status);
        assert!(manager.rx_pending.is_empty());
    }

    #[test]
    fn primary_consumes_only_one_frame_and_discards_stale_tail() {
        let mut manager = test_manager(RunLocation::Primary, RunLocation::Primary);
        let first = valid_status_frame(1, cmd::READ_STATUS);
        let mut combined = first.clone();
        combined.extend_from_slice(&valid_status_frame(1, cmd::READ_STATUS));
        let observations =
            install_scripted_port(&mut manager, vec![ReadStep::Data(combined)], vec![true]);

        run_auto_query(&mut manager);

        assert_eq!(observations.lock().unwrap().write_attempts, 1);
        assert!(manager.rx_pending.is_empty());
        let cached = manager.latest_status_cache.snapshot_statuses();
        assert_eq!(cached.len(), 1);
        assert_eq!(cached[0].raw_data, first);
        assert!(cached[0].device_status);
    }

    #[test]
    fn frame_read_uses_one_total_deadline_across_trickled_bytes() {
        let mut manager = test_manager(RunLocation::Primary, RunLocation::Primary);
        manager.serial_config.read_timeout = Duration::from_millis(50);
        let frame = valid_status_frame(1, cmd::READ_STATUS);
        let reads = frame
            .into_iter()
            .map(|byte| ReadStep::DelayedData(Duration::from_millis(20), vec![byte]))
            .collect();
        install_scripted_port(&mut manager, reads, vec![]);

        let started = Instant::now();
        let outcome = manager.receive_data_attempt(1);

        assert!(started.elapsed() < Duration::from_millis(120));
        assert!(matches!(
            outcome,
            ReceiveDataOutcome::Retryable(QueryFailure::BadFrame { .. })
        ));
    }

    #[test]
    fn response_start_before_timeout_gets_one_wire_time_completion_budget() {
        let mut manager = test_manager(RunLocation::Primary, RunLocation::Primary);
        manager.serial_config.read_timeout = Duration::from_millis(50);
        let frame = valid_status_frame(1, cmd::READ_STATUS);
        install_scripted_port(
            &mut manager,
            vec![
                ReadStep::DelayedData(Duration::from_millis(45), frame[..3].to_vec()),
                ReadStep::DelayedData(Duration::from_millis(4), frame[3..].to_vec()),
            ],
            vec![],
        );

        let started = Instant::now();
        let outcome = manager.receive_data_attempt(1);
        let elapsed = started.elapsed();

        assert!(matches!(outcome, ReceiveDataOutcome::Success));
        assert!(elapsed >= Duration::from_millis(45));
        assert!(
            elapsed < Duration::from_millis(80),
            "首字节后的收帧预算不应变成第二个完整超时: {elapsed:?}"
        );
    }

    #[test]
    fn response_completion_budget_matches_28800_baud_maximum_fixture_frame() {
        let mut manager = test_manager(RunLocation::Primary, RunLocation::Primary);
        manager.port_config.devices[0].config.data_len = 35;

        let budget = response_completion_budget(&manager.port_config, 0, 28_800);

        // 35 数据字节 + 5 固定字节 + 9 转发前缀，以 12 bit/byte 保守计算，
        // 再加 5 ms 调度保护，理论预算约 25.42 ms。
        assert!(budget >= Duration::from_micros(25_400));
        assert!(budget < Duration::from_millis(26));
    }

    #[test]
    fn normal_send_uses_bounded_settle_without_flush() {
        let mut manager = test_manager(RunLocation::Primary, RunLocation::Primary);
        manager.port_config.is_special = false;
        let observations = install_scripted_port(&mut manager, vec![], vec![true, true]);

        let started = Instant::now();
        assert!(manager.send_command(status_query()));
        let elapsed = started.elapsed();

        let observations = observations.lock().unwrap();
        assert_eq!(observations.write_attempts, 2);
        assert_eq!(observations.flush_attempts, 0);
        assert_eq!(observations.writes[0], vec![1]);
        assert_eq!(observations.writes[1].len(), 6);
        println!(
            "ordinary Mark/Space transmit elapsed_us={}",
            elapsed.as_micros()
        );
        assert!(
            elapsed < Duration::from_millis(45),
            "普通串口发送仍包含过长固定等待: {elapsed:?}"
        );
    }

    #[test]
    fn send_diagnostic_context_preserves_io_error_kind() {
        let error = io_error_with_context(
            io::Error::new(ErrorKind::TimedOut, "driver write timed out"),
            "stage=write data_len=7 elapsed_us=123".to_string(),
        );

        assert_eq!(error.kind(), ErrorKind::TimedOut);
        assert_eq!(
            error.to_string(),
            "stage=write data_len=7 elapsed_us=123 error=driver write timed out"
        );
    }

    #[test]
    fn long_read_timeout_does_not_reject_a_short_transmit() {
        let mut manager = test_manager(RunLocation::Primary, RunLocation::Primary);
        manager.serial_config.read_timeout = Duration::from_secs(10);
        let observations = install_scripted_port(&mut manager, vec![], vec![true]);

        assert!(manager.send_command(status_query()));

        let observations = observations.lock().unwrap();
        assert_eq!(observations.write_attempts, 1);
        assert_eq!(observations.writes[0].len(), 7);
    }

    #[test]
    fn configured_length_prevents_zero_noise_from_becoming_a_valid_short_frame() {
        let mut manager = test_manager(RunLocation::Primary, RunLocation::Primary);
        install_scripted_port(&mut manager, vec![ReadStep::Data(vec![0; 7])], vec![]);

        let outcome = manager.receive_data_attempt(1);

        match outcome {
            ReceiveDataOutcome::Retryable(QueryFailure::BadFrame { reason, frame }) => {
                assert_eq!(reason, "数据帧长度不匹配");
                assert_eq!(frame, vec![0; 7]);
            }
            _ => panic!("零噪声必须作为坏帧返回"),
        }
        assert!(manager.latest_status_cache.is_empty());
    }

    #[test]
    fn unattributed_secondary_frame_does_not_mark_previous_device_offline() {
        let mut manager = test_manager(RunLocation::Secondary, RunLocation::Secondary);
        let mut rx = manager.broadcaster.subscribe();
        let noise = vec![0xAA, 0xBB, 0xCC, 0xDD];

        let (_, attributed) = manager.extract_frame(&noise);
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
    fn read_io_error_marks_every_device_offline_once_during_reconnect_cooldown() {
        let mut manager = test_manager_with_devices(3);
        let mut rx = manager.broadcaster.subscribe();

        // 先为每台设备发布非零有效值，模拟运行中整口掉线。
        for index in 0..manager.port_config.devices.len() {
            manager.index = index;
            let device_id = manager.port_config.devices[index].config.device_id;
            let frame = valid_status_frame(device_id, cmd::READ_STATUS);
            let packet = protocol::parse_data_packet(&frame).expect("测试有效帧应能解析");
            manager.handle_valid_packet(packet, frame);
        }
        while rx.try_recv().is_ok() {}

        manager.index = 1;
        manager.last_reconnect = Instant::now();
        install_scripted_port(&mut manager, vec![ReadStep::IoError], vec![]);

        manager.receive_data();

        assert!(manager.port.is_none(), "读取错误后应先释放失效句柄");
        assert!(manager.port_offline_published);
        assert_eq!(manager.index, 1, "整口置零不应改变当前轮询索引");
        assert!(manager
            .device_states
            .iter()
            .all(|state| state.timeout_count == 0 && state.parse_fail_count == 0));

        let mut offline = Vec::new();
        while let Ok(status) = rx.try_recv() {
            offline.push(status);
        }
        offline.sort_by_key(|status| status.device_id);
        assert_eq!(offline.len(), 3);
        for status in &offline {
            assert!(!status.com_status);
            assert!(!status.device_status);
            assert_eq!(
                status.value.get(&format!("POINT_{}", status.device_id)),
                Some(&Value::UInt(0))
            );
        }

        let cached = manager.latest_status_cache.snapshot_statuses();
        assert_eq!(cached.len(), 3);
        assert!(cached
            .iter()
            .all(|status| !status.com_status && !status.device_status));

        // 冷却期再次进入重连路径，不应重复广播整口离线。
        manager.receive_data();
        assert!(matches!(
            rx.try_recv(),
            Err(tokio::sync::broadcast::error::TryRecvError::Empty)
        ));
    }

    #[test]
    fn initial_query_write_failure_marks_every_device_offline() {
        let mut manager = test_manager_with_devices(3);
        let mut rx = manager.broadcaster.subscribe();
        manager.last_reconnect = Instant::now();
        install_scripted_port(&mut manager, vec![], vec![false]);

        assert!(!manager.send_command(status_query()));

        let mut device_ids = Vec::new();
        while let Ok(status) = rx.try_recv() {
            assert!(!status.com_status);
            assert!(!status.device_status);
            device_ids.push(status.device_id);
        }
        device_ids.sort_unstable();
        assert_eq!(device_ids, vec![1, 2, 3]);
    }

    #[test]
    fn command_ack_io_error_releases_port_and_marks_every_device_offline() {
        let mut manager = test_manager_with_devices(2);
        let mut rx = manager.broadcaster.subscribe();
        manager.last_reconnect = Instant::now();
        install_scripted_port(&mut manager, vec![ReadStep::IoError], vec![]);

        assert!(!manager.receive_command_ack());
        assert!(manager.port.is_none());

        let mut device_ids = Vec::new();
        while let Ok(status) = rx.try_recv() {
            assert!(!status.com_status);
            assert!(!status.device_status);
            device_ids.push(status.device_id);
        }
        device_ids.sort_unstable();
        assert_eq!(device_ids, vec![1, 2]);
    }

    #[test]
    fn valid_frame_recovers_only_the_responding_device_after_port_offline() {
        let mut manager = test_manager_with_devices(2);
        manager.publish_port_offline_once();
        manager.port_offline_published = false; // 模拟串口已经重新打开

        manager.index = 0;
        let frame = valid_status_frame(1, cmd::READ_STATUS);
        let packet = protocol::parse_data_packet(&frame).expect("测试有效帧应能解析");
        manager.handle_valid_packet(packet, frame);

        let cached = manager.latest_status_cache.snapshot_statuses();
        let device_1 = cached
            .iter()
            .find(|status| status.device_id == 1)
            .expect("设备1应存在缓存");
        let device_2 = cached
            .iter()
            .find(|status| status.device_id == 2)
            .expect("设备2应存在缓存");
        assert!(device_1.com_status && device_1.device_status);
        assert!(!device_2.com_status && !device_2.device_status);
    }

    #[test]
    fn active_port_disable_publishes_every_device_offline() {
        let mut manager = test_manager_with_devices(2);
        let mut rx = manager.broadcaster.subscribe();
        for index in 0..2 {
            manager.index = index;
            let frame = valid_status_frame((index + 1) as u8, cmd::READ_STATUS);
            let packet = protocol::parse_data_packet(&frame).unwrap();
            manager.handle_valid_packet(packet, frame);
        }
        while rx.try_recv().is_ok() {}

        manager.publish_port_disabled();

        let statuses: Vec<_> = std::iter::from_fn(|| rx.try_recv().ok()).collect();
        assert_eq!(statuses.len(), 2);
        assert!(statuses
            .iter()
            .all(|status| !status.com_status && !status.device_status));
        assert!(manager
            .latest_status_cache
            .snapshot_statuses()
            .iter()
            .all(|status| !status.com_status && !status.device_status));
    }

    #[test]
    fn active_device_disable_only_invalidates_selected_device() {
        let mut manager = test_manager_with_devices(2);
        install_scripted_port(&mut manager, vec![], vec![]);
        for index in 0..2 {
            manager.index = index;
            let frame = valid_status_frame((index + 1) as u8, cmd::READ_STATUS);
            let packet = protocol::parse_data_packet(&frame).unwrap();
            manager.handle_valid_packet(packet, frame);
        }

        manager.publish_device_disabled(0);

        let cached = manager.latest_status_cache.snapshot_statuses();
        let first = cached.iter().find(|status| status.device_id == 1).unwrap();
        let second = cached.iter().find(|status| status.device_id == 2).unwrap();
        assert!(first.com_status);
        assert!(!first.device_status);
        assert!(second.com_status && second.device_status);
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
        assert_eq!(observations.input_clears, 2);
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
    fn automatic_query_fast_mode_commits_after_one_timeout_without_resend() {
        let mut manager = test_manager(RunLocation::Primary, RunLocation::Primary);
        manager.serial_config.read_timeout = Duration::from_millis(50);
        let observations = install_scripted_port(
            &mut manager,
            vec![
                ReadStep::DelayedData(Duration::from_millis(100), Vec::new()),
                ReadStep::Data(valid_status_frame(1, cmd::READ_STATUS)),
            ],
            vec![true, true],
        );
        let mut rx = manager.broadcaster.subscribe();

        let started = Instant::now();
        run_auto_query_with_retries(&mut manager, 0);
        let elapsed = started.elapsed();

        let observations = observations.lock().unwrap();
        assert_eq!(observations.write_attempts, 1);
        assert_eq!(observations.input_clears, 1);
        drop(observations);
        assert_eq!(manager.time_out_count, 1);
        assert_eq!(manager.device_states[0].timeout_count, 1);
        assert!(!rx.try_recv().expect("单次超时应立即发布离线").device_status);
        println!(
            "single-attempt 50ms query elapsed_ms={}",
            elapsed.as_millis()
        );
        assert!(
            elapsed >= Duration::from_millis(45),
            "脚本化读取没有实际等待约 50ms: {elapsed:?}"
        );
        assert!(
            elapsed < Duration::from_millis(90),
            "快速模式不应再等待第二个 50ms 超时: {elapsed:?}"
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
        assert_eq!(observations.input_clears, 1);
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
    fn automatic_query_preserves_existing_frame_validation_rules() {
        for accepted_frame in [
            valid_status_frame(0, cmd::READ_STATUS),
            valid_status_frame(2, cmd::READ_STATUS),
            valid_status_frame(1, 0x09),
        ] {
            let mut manager = test_manager(RunLocation::Primary, RunLocation::Primary);
            let observations = install_scripted_port(
                &mut manager,
                vec![ReadStep::Data(accepted_frame.clone())],
                vec![true],
            );

            run_auto_query(&mut manager);

            assert_eq!(observations.lock().unwrap().write_attempts, 1);
            assert_eq!(manager.time_out_count, 0);
            assert_eq!(manager.parse_fail_count, 0);
            let statuses = manager.latest_status_cache.snapshot_statuses();
            assert_eq!(statuses.len(), 1);
            assert_eq!(statuses[0].device_id, 1);
            assert!(statuses[0].device_status);
            assert_eq!(statuses[0].raw_data, accepted_frame);
        }
    }

    #[test]
    fn missing_port_during_reconnect_cooldown_does_not_spam_system_record() {
        let mut manager = test_manager(RunLocation::Secondary, RunLocation::Secondary);
        let mut rx = manager.broadcaster.subscribe();

        manager.receive_data();
        manager.receive_data();

        assert!(manager.system_record.lock().unwrap().is_empty());
        let offline = rx.try_recv().expect("缺失串口应立即发布离线状态");
        assert!(!offline.com_status);
        assert!(!offline.device_status);
        assert!(matches!(
            rx.try_recv(),
            Err(tokio::sync::broadcast::error::TryRecvError::Empty)
        ));
    }

    #[test]
    fn acquisition_port_close_releases_handle_and_blocks_reconnect() {
        let mut manager = test_manager_with_devices(2);
        let observations = install_scripted_port(&mut manager, vec![], vec![]);
        manager.rx_pending.extend_from_slice(&[1, 2, 3]);
        manager.set_port_enabled(false).unwrap();
        assert!(manager.port.is_none());
        assert_eq!(observations.lock().unwrap().drops, 1);
        assert!(manager.rx_pending.is_empty());
        assert!(!manager.port_config.status);
        manager.last_reconnect = Instant::now() - Duration::from_secs(5);
        manager.reconnect();
        assert!(manager.port.is_none());
        assert_eq!(manager.reconnect_failure_log.attempts, 0);
        assert!(manager
            .latest_status_cache
            .snapshot_statuses()
            .iter()
            .all(|s| !s.com_status && !s.device_status));
    }

    #[test]
    fn acquisition_closed_startup_never_attempts_to_open_port() {
        let seed = test_manager_with_devices(1);
        let mut port_config = seed.port_config;
        port_config.status = false;
        let mut serial_config = seed.serial_config;
        serial_config.port_name = "__TEST_CLOSED_NONEXISTENT_SERIAL_PORT__".into();
        let manager = SerialManager::new(
            serial_config,
            port_config,
            Broadcaster::new(),
            LatestStatusCache::new(),
            RunLocation::Primary,
            RunLocation::Primary,
            seed.system_record,
        );
        assert!(manager.port.is_none());
        assert_eq!(
            manager.reconnect_failure_log.attempts, 0,
            "disabled startup must not even attempt open"
        );
        assert!(!manager.latest_status_cache.snapshot_statuses()[0].device_status);
    }

    #[test]
    fn acquisition_stopped_device_ignores_primary_and_secondary_valid_frames() {
        for role in [RunLocation::Primary, RunLocation::Secondary] {
            let mut manager = test_manager_with_devices(2);
            manager.current_run = role;
            install_scripted_port(&mut manager, vec![], vec![]);
            for index in 0..2 {
                manager.index = index;
                let frame = valid_status_frame((index + 1) as u8, cmd::READ_STATUS);
                manager.handle_valid_packet(protocol::parse_data_packet(&frame).unwrap(), frame);
            }
            let last_received = manager.device_states[0].last_data_at.clone();
            manager.set_device_read_enabled(1, false).unwrap();
            assert!(
                manager.device_states[0].is_enabled,
                "stop-read must not disable control commands"
            );
            manager.index = 0;
            let frame = valid_status_frame(1, cmd::READ_STATUS);
            manager.handle_valid_packet(protocol::parse_data_packet(&frame).unwrap(), frame);
            let samples = manager.latest_status_cache.snapshot_statuses();
            assert!(
                !samples[0].device_status,
                "passive/late valid frame cannot revive stopped station"
            );
            assert!(samples[1].device_status, "other station must stay online");
            assert_eq!(manager.device_states[0].last_data_at, last_received);
            manager.set_device_read_enabled(1, true).unwrap();
            assert!(
                !manager.latest_status_cache.snapshot_statuses()[0].device_status,
                "enable is not evidence of a new frame"
            );
            let frame = valid_status_frame(1, cmd::READ_STATUS);
            manager.handle_valid_packet(protocol::parse_data_packet(&frame).unwrap(), frame);
            assert!(manager.latest_status_cache.snapshot_statuses()[0].device_status);
        }
    }

    #[test]
    fn acquisition_stop_read_skips_polling_but_preserves_control_commands() {
        let mut manager = test_manager_with_devices(2);
        let observations = install_scripted_port(&mut manager, vec![], vec![]);
        manager.set_device_read_enabled(1, false).unwrap();
        let (ack_tx, _ack_rx) = std::sync::mpsc::channel();
        assert!(super::super::worker::test_primary_cycle_is_idle(
            &mut manager,
            &ack_tx
        ));
        assert_eq!(observations.lock().unwrap().write_attempts, 0);
        let (tx, rx) = std::sync::mpsc::channel();
        tx.send(crate::model::Command {
            com: manager.port_config.port_number.clone(),
            command: crate::model::CommandType::SendData(SendData {
                device_id: 1,
                command: vec![1, 9, 2, 3],
            }),
            ack_id: None,
            expires_at: None,
            control_receipt: None,
            permit: None,
        })
        .unwrap();
        super::super::worker::process_test_commands(
            &mut manager,
            &rx,
            &crate::model::SystemState::new(),
            &ack_tx,
        );
        assert_eq!(manager.command_queue.lock().unwrap().len(), 1);
        assert!(!super::super::worker::test_primary_cycle_is_idle(
            &mut manager,
            &ack_tx
        ));
        assert!(observations.lock().unwrap().write_attempts > 0);
        assert!(!manager.latest_status_cache.snapshot_statuses()[0].device_status);
    }

    #[test]
    fn acquisition_worker_receipt_follows_actual_stop_read_and_port_close() {
        let mut manager = test_manager_with_devices(2);
        let observations = install_scripted_port(&mut manager, vec![], vec![]);
        let root = tempfile::tempdir().unwrap();
        std::fs::create_dir(root.path().join("config")).unwrap();
        std::fs::create_dir(root.path().join("logs")).unwrap();
        let operations = crate::operations::Operations::new(
            root.path().join("config"),
            root.path().join("logs"),
            "test".into(),
            &[manager.port_config.clone()],
        )
        .unwrap();
        let port_id = manager.port_config.port_number.clone();
        let (tx, rx) = std::sync::mpsc::channel();
        operations.register_worker(&port_id, tx);
        manager.operations = Some(operations.clone());
        manager.refresh_operations_state();
        let system_state = crate::model::SystemState::new();
        system_state.add_port(manager.port_config.to_runtime_state());
        let (ack_tx, _ack_rx) = std::sync::mpsc::channel();
        for (action, revision) in [("set_device_read_enabled", "1"), ("set_port_enabled", "2")] {
            let caller = operations.clone();
            let request = serde_json::json!({"action":action,"port_id":port_id,"device_id":"1","enabled":false,"expected_revision":revision});
            let task = std::thread::spawn(move || caller.execute(&request));
            let command = rx.recv_timeout(Duration::from_secs(1)).unwrap();
            assert_eq!(operations.status()["ports"][0]["control_pending"], true);
            let (forward, receiver) = std::sync::mpsc::channel();
            forward.send(command).unwrap();
            super::super::worker::process_test_commands(
                &mut manager,
                &receiver,
                &system_state,
                &ack_tx,
            );
            let response = task.join().unwrap().unwrap();
            assert_eq!(response["status"], "Applied");
            assert_eq!(response["ports"][0]["control_pending"], false);
        }
        assert!(!manager.device_states[0].read_enabled);
        assert!(manager.device_states[0].is_enabled);
        assert!(manager.device_states[1].read_enabled);
        assert!(manager.port.is_none());
        assert_eq!(observations.lock().unwrap().drops, 1);
        let restarted = crate::operations::Operations::new(
            root.path().join("config"),
            root.path().join("logs"),
            "test".into(),
            &[manager.port_config.clone()],
        )
        .unwrap();
        assert!(!restarted.initial_port_enabled(&port_id));
        assert!(!restarted.initial_device_read_enabled(&port_id, 1));
        assert!(restarted.initial_device_read_enabled(&port_id, 2));
        assert_eq!(restarted.status()["revision"], "3");
    }
}
