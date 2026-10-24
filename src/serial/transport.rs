//! Bounded synchronous transport primitives shared by poll, ACK and version transactions.
//! These functions borrow the sole worker's port; no handle cloning or extra reader.
use serialport::SerialPort;
use std::io::{self, ErrorKind};
use std::time::{Duration, Instant};

const OUTPUT_QUEUE_POLL_INTERVAL: Duration = Duration::from_micros(250);
const MIN_TRANSMIT_IO_SLACK: Duration = Duration::from_millis(50);
pub(super) const MAX_TRANSMIT_TIMEOUT: Duration = Duration::from_secs(5);

fn serial_error_to_io(error: serialport::Error) -> io::Error {
    io::Error::other(error.to_string())
}

/// 在统一截止时间内执行一次 read，并在结束后恢复端口原始超时配置。
pub(super) fn read_before_deadline(
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
pub(super) fn write_all_before_deadline(
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

pub(super) fn wire_duration(byte_count: usize, baud_rate: u32) -> Duration {
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

fn transmit_wire_budget(byte_count: usize, baud_rate: u32) -> Duration {
    wire_duration(byte_count, baud_rate)
        .saturating_add(wire_duration(byte_count.min(16), baud_rate))
}

pub(super) fn minimum_transmit_timeout(byte_count: usize, baud_rate: u32) -> Duration {
    transmit_wire_budget(byte_count, baud_rate).saturating_add(MIN_TRANSMIT_IO_SLACK)
}

pub(super) fn transmit_timeout(
    byte_count: usize,
    baud_rate: u32,
    configured: Duration,
) -> Duration {
    let wire_budget = transmit_wire_budget(byte_count, baud_rate);
    // 读取超时只是一个可复用的 I/O 余量，不得因为它大于发送保护上限就把所有
    // 短命令误判为“帧过长”。余量限制在剩余的 5 秒总预算内。
    let remaining_budget = MAX_TRANSMIT_TIMEOUT.saturating_sub(wire_budget);
    let io_slack = configured.max(MIN_TRANSMIT_IO_SLACK).min(remaining_budget);
    wire_budget.saturating_add(io_slack)
}

/// 等待驱动发送队列清空，再按完整段线速时间和短保护时间等待 UART 发完。
/// `bytes_to_write` 不受驱动支持时仍使用线速时间兜底；不调用可能永久阻塞的 flush/tcdrain。
pub(super) fn wait_for_output_settle(
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
