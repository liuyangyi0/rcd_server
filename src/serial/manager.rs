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
use serialport::{DataBits, Parity, SerialPort, StopBits};

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
const FORWARD_PREFIX_GRACE: Duration = Duration::from_millis(5);

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

        let port = open_serial_port(
            &serial_config.port_name,
            serial_config.baud_rate,
            serial_config.data_bits,
            serial_config.stop_bits,
            serial_config.parity,
            serial_config.read_timeout,
        )
        .ok();

        if port.is_none() {
            warn!("串口 {} 打开失败", serial_config.port_name);
        }

        let device_states = port_config
            .devices
            .iter()
            .map(|_| DeviceRuntimeState::new())
            .collect();

        Self {
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
        }
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
        self.last_reconnect = Instant::now();

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
                info!("串口 {} 重新连接成功", self.serial_config.port_name);
            }
            Err(e) => {
                error!("{:?}: 重连串口 {} 失败", e, self.serial_config.port_name);
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
        std::thread::sleep(Duration::from_millis(100));

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

    /// 从串口读取并解析一帧数据。
    ///
    /// 采用循环读取策略：根据帧头中的长度字段判断帧是否完整，
    /// 避免因一次 `read` 未返回完整帧而解析失败。
    pub(crate) fn receive_data(&mut self) {
        let port = match self.port {
            Some(ref mut p) => p,
            None => {
                self.log_record("串口未打开".into());
                self.reconnect();
                return;
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
                    self.handle_timeout();
                    return;
                }
                Err(e) => {
                    error!("读取错误: {:?}", e);
                    self.log_record("读取错误".into());
                    self.reconnect();
                    return;
                }
            }
        }

        if total == 0 {
            self.handle_timeout();
            return;
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
            self.handle_unattributed_frame();
            return;
        }

        // ---- 解析数据帧 ----
        match protocol::parse_data_packet(&frame) {
            Ok(packet) => {
                // 只有长度与校验和均有效的完整帧才能证明通信恢复。
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
            Err(_) => self.handle_invalid_frame(),
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
    fn handle_unattributed_frame(&mut self) {
        self.parse_fail_count += 1;
        self.time_out_count += 1;

        if self.time_out_count > threshold::PORT_FAILOVER_TIMEOUT
            && self.current_run == RunLocation::Secondary
            && self.run_on == RunLocation::Secondary
        {
            self.switch_role(RunLocation::Primary, "无法归属的坏帧过多，备机升级为主机");
        }
    }

    /// 将收到但无法通过长度或校验和验证的帧视为本轮没有有效响应。
    fn handle_invalid_frame(&mut self) {
        self.device_states[self.index].parse_fail_count += 1;
        self.parse_fail_count += 1;

        // 坏帧不能证明设备在线；复用超时路径更新离线状态和故障计数。
        self.handle_timeout();

        // 连续解析失败过多时尝试主备切换。
        if self.parse_fail_count > threshold::PARSE_FAIL_FAILOVER
            && self.current_run == RunLocation::Primary
            && self.run_on == RunLocation::Secondary
        {
            self.switch_role(RunLocation::Secondary, "解析失败过多，切换为备机");
        }
    }

    /// 处理读取超时逻辑。
    pub(crate) fn handle_timeout(&mut self) {
        self.device_states[self.index].timeout_count += 1;
        self.time_out_count += 1;

        // let dev = &self.port_config.devices[self.index];
        // warn!(
        //     "串口 {} 设备 {} 读取超时 (连续{}次)",
        //     self.serial_config.port_name,
        //     dev.config.device_id,
        //     self.device_states[self.index].timeout_count,
        // );

        self.send_zero_status(true, false);

        // 超时过多时尝试主备切换
        if self.time_out_count > threshold::PORT_FAILOVER_TIMEOUT
            && self.current_run == RunLocation::Secondary
            && self.run_on == RunLocation::Secondary
        {
            self.switch_role(RunLocation::Primary, "超时过多，备机升级为主机");
        }
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

    use crate::csv_parser::{Config as CsvConfig, DeviceConfig};
    use crate::model::Value;

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
        }
    }

    fn with_checksum(mut bytes: Vec<u8>) -> Vec<u8> {
        let checksum = crate::model::get_sum(&bytes);
        bytes.push(checksum);
        bytes
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
        manager.handle_unattributed_frame();

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

        manager.handle_invalid_frame();

        assert_eq!(manager.time_out_count, 1);
        assert_eq!(manager.device_states[0].timeout_count, 1);
        let status = rx.try_recv().expect("离线状态应立即广播");
        assert!(!status.device_status);
        assert!(!manager.latest_status_cache.snapshot_statuses()[0].device_status);
    }
}
