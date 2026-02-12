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

use chrono::{DateTime, Local};
use bounded_vec_deque::BoundedVecDeque;
use log::{info, warn, error};
use serialport::{DataBits, Parity, SerialPort, StopBits};
use tokio::runtime::Handle;
use tokio::sync::Mutex as TokioMutex;

use crate::broadcast::Broadcaster;
use crate::config::RunLocation;
use crate::model::{DeviceStatus, SendData};
use crate::protocol::{self, threshold};
use super::port_config::SerialPortConfig;

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
}

impl SerialConfig {
    pub fn new(port_name: String, baud_rate: u32, data_bits: DataBits, stop_bits: StopBits, parity: Parity) -> Self {
        Self { port_name, baud_rate, data_bits, stop_bits, parity }
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
            last_data: vec![0],
            same_count: 0,
            is_enabled: true,
        }
    }

    /// 判断新数据是否应该推送给上层。
    ///
    /// 规则：如果与上次数据不同则立即推送；如果连续相同超过阈值也推送一次。
    pub(crate) fn should_broadcast(&mut self, data: &[u8]) -> bool {
        if data == self.last_data {
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
            true
        }
    }
}

// ============================================================
//  串口管理器
// ============================================================

/// 管理单个串口上所有设备的轮询与数据收发。
pub(crate) struct SerialManager {
    pub(crate) port: Option<Box<dyn SerialPort>>,
    pub(crate) command_queue: Arc<Mutex<VecDeque<SendData>>>,
    pub(crate) serial_config: SerialConfig,
    pub(crate) port_config: SerialPortConfig,
    pub(crate) broadcaster: Broadcaster,
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
    pub(crate) system_record: Arc<TokioMutex<BoundedVecDeque<String>>>,
    /// 主运行时句柄，用于在阻塞线程中执行异步操作。
    pub(crate) handle: Handle,
    /// 复用的读取缓冲区，避免每次 receive_data 堆分配。
    read_buf: Vec<u8>,
    /// 上次重连尝试的时间，用于冷却控制。
    last_reconnect: Instant,
}

impl SerialManager {
    /// 创建管理器并尝试打开串口。
    ///
    /// 必须在持有 tokio `Handle` 的上下文中调用（如 `spawn_blocking` 内部）。
    pub(crate) fn new(
        serial_config: SerialConfig,
        port_config: SerialPortConfig,
        broadcaster: Broadcaster,
        run_on: RunLocation,
        current_run: RunLocation,
        system_record: Arc<TokioMutex<BoundedVecDeque<String>>>,
        handle: Handle,
    ) -> Self {
        // 首次启动等待设备就绪
        std::thread::sleep(Duration::from_millis(2000));

        let port = open_serial_port(
            &serial_config.port_name,
            serial_config.baud_rate,
            serial_config.data_bits,
            serial_config.stop_bits,
            serial_config.parity,
        ).ok();

        if port.is_none() {
            warn!("串口 {} 打开失败", serial_config.port_name);
        }

        let device_states = port_config.devices.iter().map(|_| DeviceRuntimeState::new()).collect();

        Self {
            port,
            command_queue: Arc::new(Mutex::new(VecDeque::new())),
            serial_config,
            port_config,
            broadcaster,
            device_states,
            index: 0,
            run_on,
            current_run,
            time_out_count: 0,
            parse_fail_count: 0,
            system_record,
            handle,
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

        if self.device_states[self.index].should_broadcast(&zero_data) {
            let status = DeviceStatus {
                id: dev.config.com_index as u64,
                device_id: dev.config.device_id,
                com: self.port_config.port_number.clone(),
                com_status,
                device_status,
                value: parsed,
                raw_data: zero_data,
            };
            self.handle.block_on(self.broadcaster.broadcast(status));
        }
    }

    /// 记录一条系统日志。
    pub(crate) fn log_record(&self, msg: String) {
        let local: DateTime<Local> = Local::now();
        let mut record = self.handle.block_on(self.system_record.lock());
        record.push_back(format!(
            "串口 {} 时间 {} {}",
            self.serial_config.port_name,
            local.format("%Y-%m-%d %H:%M:%S"),
            msg,
        ));
    }

    // ---- 数据发送 ----

    /// 向设备发送命令帧。
    pub(crate) fn send_command(&mut self, mut command: SendData) {
        if command.command.is_empty() {
            return;
        }
        println!("串口 {} 发送数据: {}", self.serial_config.port_name, format_hex(&command.command));

        // 特殊串口：直接写入全部数据
        if self.port_config.is_special {
            match self.port {
                Some(ref mut port) => {
                    if let Err(e) = port.write(&command.command) {
                        error!("写入错误: {:?}", e);
                        self.port = None;
                        self.reconnect();
                    }
                    return;
                }
                None => {
                    self.reconnect();
                    return;
                }
            }
        }

        // 普通串口：地址字节使用 Mark 校验，数据部分使用 Space 校验
        let addr_byte = vec![command.command[0]];
        command.command.remove(0);

        // 发送地址字节（Mark 奇偶校验）
        match self.port {
            Some(ref mut port) => {
                if let Err(e) = port.set_parity(Parity::Mark) {
                    error!("设置 Mark 校验失败: {:?}", e);
                    self.port = None;
                    self.reconnect();
                    return;
                }
                if let Err(e) = port.write(&addr_byte) {
                    warn!("写入地址字节错误: {:?}", e);
                    self.restore_parity();
                    return;
                }
                 let _ = port.flush();
            }
            None => {
                self.reconnect();
                return;
            }
        }

        // 等待地址字节在物理线路上完全发出后再切换校验位
        std::thread::sleep(Duration::from_millis(2));

        // 发送数据部分（Space 奇偶校验）
        match self.port {
            Some(ref mut port) => {
                if let Err(e) = port.set_parity(Parity::Space) {
                    error!("设置 Space 校验失败: {:?}", e);
                    self.port = None;
                    self.reconnect();
                    return;
                }
                if let Err(e) = port.write(&command.command) {
                    error!("写入数据部分错误: {:?}", e);
                    self.port = None;
                    self.reconnect();
                    return;
                }
                let _ = port.flush();
            }
            None => {
                self.reconnect();
                return;
            }
        }

        // 等待数据在物理线路上完全发出后再恢复校验位
        std::thread::sleep(Duration::from_millis(2));

        // 恢复原始校验位，供后续 receive_data 使用
        //self.restore_parity();
    }

    /// 将串口校验位恢复为配置文件中指定的原始值。
    fn restore_parity(&mut self) {
        if let Some(ref mut port) = self.port {
            if let Err(e) = port.set_parity(Parity::None) {
                error!("恢复校验位失败: {:?}", e);
            }
        }
    }

    // ---- 数据接收与解析 ----

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
                    // 已读到帧头(3字节)时，可计算期望帧长
                    if total >= 3 {
                        let expected = expected_frame_len(
                            &self.read_buf[..total], &self.current_run,
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

        // 成功读到数据，重置超时计数
        self.device_states[self.index].timeout_count = 0;
        self.time_out_count = 0;

        // ---- 提取有效数据切片 ----
        let frame = self.extract_frame(total);
        println!("串口 {} 原始数据: {}", self.serial_config.port_name, format_hex(&frame));

        // ---- 解析数据帧 ----
        match protocol::parse_data_packet(&frame) {
            Ok(packet) => {
                self.device_states[self.index].parse_fail_count = 0;
                self.parse_fail_count = 0;

                let dev = &self.port_config.devices[self.index];
                let parsed = protocol::parse_status(&packet.status, &dev.records);
                println!("串口 {} 解析数据: {:?}", self.serial_config.port_name, parsed);
                if self.device_states[self.index].should_broadcast(&packet.status) {
                    let status = DeviceStatus {
                        id: dev.config.com_index as u64,
                        device_id: dev.config.device_id,
                        com: self.port_config.port_number.clone(),
                        com_status: true,
                        device_status: true,
                        value: parsed,
                        raw_data: frame,
                    };
                    self.handle.block_on(self.broadcaster.broadcast(status));
                }
            }
            Err(_) => {
                self.device_states[self.index].parse_fail_count += 1;
                self.parse_fail_count += 1;

                // 连续解析失败过多时尝试主备切换
                if self.parse_fail_count > threshold::PARSE_FAIL_FAILOVER
                    && self.current_run == RunLocation::Primary
                    && self.run_on == RunLocation::Secondary
                {
                    self.switch_role(RunLocation::Secondary, "解析失败过多，切换为备机");
                }
            }
        }
    }

    /// 从 `read_buf` 中提取有效数据帧。
    ///
    /// 备机模式下会解析转发头以更新 `index`，并剥离前缀。
    /// 如果当前由备机临时升级为主机，检测到有效转发头说明真正主机已恢复，
    /// 则自动切回备机。
    fn extract_frame(&mut self, total: usize) -> Vec<u8> {
        // 尝试检测转发头（含 00 00 分隔符）
        let has_forward_header = protocol::find_double_zero(&self.read_buf[..total]);

        // 回切检测：备机临时充当主机时，若收到转发头说明真正主机已恢复
        if self.current_run == RunLocation::Primary
            && self.run_on == RunLocation::Secondary
            && has_forward_header.is_some()
        {
            self.switch_role(RunLocation::Secondary, "检测到主机转发帧，切回备机");
        }

        if self.current_run == RunLocation::Secondary {
            if let Some(idx) = has_forward_header {
                // 校验转发头
                let header = &self.read_buf[..idx];
                if header.len() == 7 && protocol::verify_checksum(header) {
                    let device_addr = header[0];
                    for (i, dev) in self.port_config.devices.iter().enumerate() {
                        if dev.config.device_id == device_addr {
                            self.index = i;
                            break;
                        }
                    }
                }
                // 提取数据帧部分（跳过 00 00 分隔符）
                let data_start = idx + 2;
                let data = &self.read_buf[data_start..total];
                let effective_len = if data.len() >= 4 {
                    (data[2] as usize + 5).min(data.len())
                } else {
                    data.len()
                };
                data[..effective_len].to_vec()
            } else {
                self.read_buf[..total].to_vec()
            }
        } else {
            self.read_buf[..total].to_vec()
        }
    }

    /// 处理读取超时逻辑。
    pub(crate) fn handle_timeout(&mut self) {
        self.device_states[self.index].timeout_count += 1;
        self.time_out_count += 1;

        let dev = &self.port_config.devices[self.index];
        println!("串口 {} 设备 {} 读取超时 (连续{}次)",
            self.serial_config.port_name,
            dev.config.device_id,
            self.device_states[self.index].timeout_count,
        );

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
        let old = if self.current_run == RunLocation::Primary { "主机" } else { "备机" };
        let new = if new_role == RunLocation::Primary { "主机" } else { "备机" };
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

// ============================================================
//  帧长度计算
// ============================================================

/// 根据已读数据估算期望的完整帧长度。
///
/// 帧格式 `[Addr][CMD][Len][Status×(Len+1)][CRC]`，总长 = Len + 5。
fn expected_frame_len(buf: &[u8], current_run: &RunLocation) -> usize {
    let total = buf.len();
    if *current_run == RunLocation::Secondary {
        // 备机帧可能有前缀 header(7字节)+00 00，数据帧在后面
        if let Some(idx) = protocol::find_double_zero(buf) {
            let data_start = idx + 2; // 跳过 00 00
            if data_start + 3 <= total {
                let len_byte = buf[data_start + 2] as usize;
                return data_start + len_byte + 5;
            }
        }
        // 无法确定时返回较大值继续读
        total + 1
    } else {
        // 主机模式：直接取 buf[2] 作为 Len
        let len_byte = buf[2] as usize;
        len_byte + 5
    }
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
) -> Result<Box<dyn SerialPort>, serialport::Error> {
    serialport::new(port_name, baud_rate)
        .data_bits(data_bits)
        .stop_bits(stop_bits)
        .parity(parity)
        .timeout(Duration::from_millis(400))
        .open()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn should_broadcast_data_changed() {
        let mut state = DeviceRuntimeState::new();
        // 初始 last_data = [0], 新数据不同 → 推送
        assert!(state.should_broadcast(&[1, 2, 3]));
    }

    #[test]
    fn should_broadcast_same_data_below_threshold() {
        let mut state = DeviceRuntimeState::new();
        state.should_broadcast(&[1, 2, 3]); // 第一次, 更新 last_data
        // 连续发送相同数据, 次数不到阈值, 不推送
        for _ in 0..threshold::SAME_DATA_BROADCAST - 1 {
            assert!(!state.should_broadcast(&[1, 2, 3]));
        }
    }

    #[test]
    fn should_broadcast_same_data_at_threshold() {
        let mut state = DeviceRuntimeState::new();
        state.should_broadcast(&[1, 2, 3]); // 第一次
        // 到达阈值时推送
        for _ in 0..threshold::SAME_DATA_BROADCAST - 1 {
            state.should_broadcast(&[1, 2, 3]);
        }
        assert!(state.should_broadcast(&[1, 2, 3])); // 第 SAME_DATA_BROADCAST 次
    }

    #[test]
    fn should_broadcast_reset_on_change() {
        let mut state = DeviceRuntimeState::new();
        state.should_broadcast(&[1, 2, 3]);
        // 发几次相同数据
        state.should_broadcast(&[1, 2, 3]);
        state.should_broadcast(&[1, 2, 3]);
        // 新数据 → 立即推送, 且计数器重置
        assert!(state.should_broadcast(&[4, 5, 6]));
        // 再次发相同数据, 重新计数
        assert!(!state.should_broadcast(&[4, 5, 6]));
    }
}
