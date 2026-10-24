use std::collections::BTreeMap;
use std::env;
use std::error::Error;
use std::fs;
use std::io::{self, ErrorKind};
use std::path::Path;
use std::time::{Duration, Instant};

use rcd_server::csv_parser::{parse_csv, Config as DeviceConfig};
use rcd_server::model::get_sum;
use rcd_server::protocol::{self, cmd};
use serialport::{ClearBuffer, DataBits, Parity, SerialPort, StopBits};

const READ_TIMEOUT: Duration = Duration::from_millis(300);
const PRE_ADDRESS_DELAY: Duration = Duration::from_millis(25);
const SETTLE_MARGIN: Duration = Duration::from_millis(25);
const RAW_PREVIEW_LIMIT: usize = 64;
const DEFAULT_ROUNDS: usize = 10;
const DEFAULT_PORTS: [&str; 4] = ["/dev/ttyXR0", "/dev/ttyXR1", "/dev/ttyXR2", "/dev/ttyXR3"];

#[derive(Default)]
struct Counters {
    attempts: usize,
    valid: usize,
    timeouts: usize,
    bad_frames: usize,
    full_zero_frames: usize,
    send_errors: usize,
    read_errors: usize,
}

enum ReadResult {
    Frame(Vec<u8>),
    Timeout(Vec<u8>),
    IoError { error: io::Error, raw: Vec<u8> },
}

fn main() -> Result<(), Box<dyn Error>> {
    let args: Vec<String> = env::args().collect();
    let config_dir = args.get(1).map(String::as_str).unwrap_or("config");
    let rounds = args
        .get(2)
        .map(|value| value.parse::<usize>())
        .transpose()?
        .unwrap_or(DEFAULT_ROUNDS);
    let requested_ports: Vec<String> = if args.len() > 3 {
        args[3..].to_vec()
    } else {
        DEFAULT_PORTS
            .iter()
            .map(|port| (*port).to_string())
            .collect()
    };
    let clear_input = env_bool("SERIAL_CARD_TEST_CLEAR_INPUT", true);

    if rounds == 0 {
        return Err("rounds 必须大于 0".into());
    }

    let mut configured = load_devices(config_dir)?;
    println!(
        "TEST_START config_dir={config_dir:?} rounds={rounds} clear_input={clear_input} ports={requested_ports:?}"
    );

    for port_name in requested_ports {
        let Some(mut devices) = configured.remove(&port_name) else {
            println!("PORT_SUMMARY port={port_name} result=no_config");
            continue;
        };
        devices.sort_by_key(|device| (device.com_index, device.device_id));
        test_port(&port_name, &devices, rounds, clear_input);
    }

    println!("TEST_DONE");
    Ok(())
}

fn load_devices(
    config_dir: impl AsRef<Path>,
) -> Result<BTreeMap<String, Vec<DeviceConfig>>, Box<dyn Error>> {
    let mut devices_by_port = BTreeMap::<String, Vec<DeviceConfig>>::new();
    for entry in fs::read_dir(config_dir)? {
        let path = entry?.path();
        if !path.is_file()
            || !path
                .extension()
                .is_some_and(|extension| extension.eq_ignore_ascii_case("csv"))
        {
            continue;
        }
        match parse_csv(&path) {
            Ok((config, _)) => devices_by_port
                .entry(config.com.clone())
                .or_default()
                .push(config),
            Err(error) => eprintln!("CONFIG_ERROR path={path:?} error={error}"),
        }
    }
    Ok(devices_by_port)
}

fn test_port(port_name: &str, devices: &[DeviceConfig], rounds: usize, clear_input: bool) {
    let baud_rate = devices[0].baud_rate;
    if devices.iter().any(|device| device.baud_rate != baud_rate) {
        println!("PORT_SUMMARY port={port_name} result=inconsistent_baud_rate");
        return;
    }

    let mut port = match serialport::new(port_name, baud_rate)
        .data_bits(DataBits::Eight)
        .stop_bits(StopBits::One)
        .parity(Parity::None)
        .timeout(READ_TIMEOUT)
        .open()
    {
        Ok(port) => port,
        Err(error) => {
            println!("PORT_SUMMARY port={port_name} result=open_error error={error:?}");
            return;
        }
    };

    println!(
        "PORT_START port={port_name} baud_rate={baud_rate} devices={} rounds={rounds} clear_input={clear_input}",
        devices.len()
    );
    let mut port_total = Counters::default();
    let mut device_counters = devices
        .iter()
        .map(|_| Counters::default())
        .collect::<Vec<_>>();

    // 与正式 worker 一致：每轮依次查询各设备，而不是连续查询同一设备。
    for round in 1..=rounds {
        for (device, counters) in devices.iter().zip(&mut device_counters) {
            counters.attempts += 1;
            let frame = query_frame(device);
            if clear_input {
                if let Err(error) = port.clear(ClearBuffer::Input) {
                    eprintln!(
                        "CLEAR_WARNING port={port_name} device_id={} round={round} error={error:?}",
                        device.device_id
                    );
                }
            }

            if let Err(error) = send_query(port.as_mut(), device, &frame) {
                counters.send_errors += 1;
                println!(
                    "EVENT port={port_name} device_id={} round={round} result=send_error error_kind={:?} error={error:?}",
                    device.device_id,
                    error.kind()
                );
                continue;
            }

            match read_response(port.as_mut(), device.data_len as usize) {
                ReadResult::Frame(raw) => {
                    classify_frame(port_name, device.device_id, round, &raw, counters)
                }
                ReadResult::Timeout(raw) => {
                    counters.timeouts += 1;
                    println!(
                        "EVENT port={port_name} device_id={} round={round} result=timeout raw_len={} raw_hex={:?}",
                        device.device_id,
                        raw.len(),
                        hex_preview(&raw)
                    );
                }
                ReadResult::IoError { error, raw } => {
                    counters.read_errors += 1;
                    println!(
                        "EVENT port={port_name} device_id={} round={round} result=read_error error_kind={:?} error={error:?} raw_len={} raw_hex={:?}",
                        device.device_id,
                        error.kind(),
                        raw.len(),
                        hex_preview(&raw)
                    );
                }
            }
        }
    }

    for (device, counters) in devices.iter().zip(&device_counters) {
        println!(
            "DEVICE_SUMMARY port={port_name} device_id={} attempts={} valid={} timeouts={} bad_frames={} full_zero_frames={} send_errors={} read_errors={}",
            device.device_id,
            counters.attempts,
            counters.valid,
            counters.timeouts,
            counters.bad_frames,
            counters.full_zero_frames,
            counters.send_errors,
            counters.read_errors
        );
        add_counters(&mut port_total, counters);
    }

    println!(
        "PORT_SUMMARY port={port_name} attempts={} valid={} timeouts={} bad_frames={} full_zero_frames={} send_errors={} read_errors={}",
        port_total.attempts,
        port_total.valid,
        port_total.timeouts,
        port_total.bad_frames,
        port_total.full_zero_frames,
        port_total.send_errors,
        port_total.read_errors
    );
}

fn query_frame(device: &DeviceConfig) -> Vec<u8> {
    let mut frame = vec![
        device.device_id,
        cmd::READ_STATUS,
        cmd::READ_SUBCMD,
        cmd::STATUS_PAGE,
        cmd::STATUS_OFFSET,
        device.data_len,
    ];
    frame.push(get_sum(&frame));
    frame
}

fn send_query(port: &mut dyn SerialPort, device: &DeviceConfig, frame: &[u8]) -> io::Result<()> {
    if device.is_special {
        port.write_all(frame)?;
        return Ok(());
    }

    port.set_parity(Parity::Mark).map_err(serial_error_to_io)?;
    std::thread::sleep(PRE_ADDRESS_DELAY);
    port.write_all(&frame[..1])?;
    wait_for_wire(1, device.baud_rate);

    port.set_parity(Parity::Space).map_err(serial_error_to_io)?;
    port.write_all(&frame[1..])?;
    wait_for_wire(frame.len() - 1, device.baud_rate);
    Ok(())
}

fn wait_for_wire(byte_count: usize, baud_rate: u32) {
    let bits = byte_count as u128 * 12;
    let nanos = bits
        .saturating_mul(1_000_000_000)
        .div_ceil(u128::from(baud_rate.max(1)))
        .min(u128::from(u64::MAX));
    std::thread::sleep(Duration::from_nanos(nanos as u64).saturating_add(SETTLE_MARGIN));
}

fn read_response(port: &mut dyn SerialPort, configured_data_len: usize) -> ReadResult {
    let deadline = Instant::now() + READ_TIMEOUT;
    let mut raw = Vec::with_capacity(configured_data_len + 5);
    let mut buffer = [0u8; 512];

    loop {
        if let Some(expected_len) = expected_frame_len(&raw, configured_data_len) {
            if raw.len() >= expected_len {
                raw.truncate(expected_len);
                return ReadResult::Frame(raw);
            }
        }

        let remaining = deadline.saturating_duration_since(Instant::now());
        if remaining.is_zero() {
            return ReadResult::Timeout(raw);
        }
        if let Err(error) = port.set_timeout(remaining) {
            return ReadResult::IoError {
                error: serial_error_to_io(error),
                raw,
            };
        }

        match port.read(&mut buffer) {
            Ok(0) => continue,
            Ok(count) => raw.extend_from_slice(&buffer[..count]),
            Err(error) if matches!(error.kind(), ErrorKind::TimedOut | ErrorKind::WouldBlock) => {
                return ReadResult::Timeout(raw);
            }
            Err(error) => return ReadResult::IoError { error, raw },
        }
    }
}

fn expected_frame_len(raw: &[u8], configured_data_len: usize) -> Option<usize> {
    let declared_len = *raw.get(2)? as usize;
    let configured_len = if declared_len + 1 == configured_data_len {
        configured_data_len + 4
    } else {
        configured_data_len + 5
    };
    Some((declared_len + 5).max(configured_len))
}

fn classify_frame(
    port_name: &str,
    device_id: u8,
    round: usize,
    raw: &[u8],
    counters: &mut Counters,
) {
    if raw.iter().all(|byte| *byte == 0) {
        counters.full_zero_frames += 1;
        println!(
            "EVENT port={port_name} device_id={device_id} round={round} result=full_zero_frame raw_len={} raw_hex={:?}",
            raw.len(),
            hex_preview(raw)
        );
        return;
    }

    match protocol::parse_data_packet(raw) {
        Ok(packet) => {
            counters.valid += 1;
            println!(
                "EVENT port={port_name} device_id={device_id} round={round} result=valid raw_len={} response_addr={} response_cmd={} declared_len={} raw_hex={:?}",
                raw.len(),
                packet.addr,
                packet.cmd,
                packet.len,
                hex_preview(raw)
            );
        }
        Err(reason) => {
            counters.bad_frames += 1;
            println!(
                "EVENT port={port_name} device_id={device_id} round={round} result=bad_frame reason={reason:?} raw_len={} raw_hex={:?}",
                raw.len(),
                hex_preview(raw)
            );
        }
    }
}

fn hex_preview(data: &[u8]) -> String {
    let mut value = data
        .iter()
        .take(RAW_PREVIEW_LIMIT)
        .map(|byte| format!("{byte:02X}"))
        .collect::<Vec<_>>()
        .join(" ");
    if data.len() > RAW_PREVIEW_LIMIT {
        value.push_str(" ...");
    }
    value
}

fn serial_error_to_io(error: serialport::Error) -> io::Error {
    io::Error::other(error.to_string())
}

fn add_counters(total: &mut Counters, current: &Counters) {
    total.attempts += current.attempts;
    total.valid += current.valid;
    total.timeouts += current.timeouts;
    total.bad_frames += current.bad_frames;
    total.full_zero_frames += current.full_zero_frames;
    total.send_errors += current.send_errors;
    total.read_errors += current.read_errors;
}

fn env_bool(name: &str, default: bool) -> bool {
    match env::var(name) {
        Ok(value) => matches!(
            value.trim().to_ascii_lowercase().as_str(),
            "1" | "true" | "yes" | "on"
        ),
        Err(_) => default,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn expected_length_supports_both_deployed_data_len_meanings() {
        assert_eq!(expected_frame_len(&[0, 0, 29], 30), Some(34));
        assert_eq!(expected_frame_len(&[0, 0, 30], 30), Some(35));
    }

    #[test]
    fn query_matches_worker_format() {
        let config = DeviceConfig {
            com: "/dev/ttyXR0".to_string(),
            baud_rate: 9_600,
            is_special: false,
            com_index: 0,
            device_id: 13,
            data_len: 30,
            kks_prefix: String::new(),
        };
        assert_eq!(query_frame(&config), vec![13, 8, 2, 48, 16, 30, 117]);
    }
}
