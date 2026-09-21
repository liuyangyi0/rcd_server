//! 串口协议常量与纯解析函数。
//!
//! 将协议相关的常量定义和帧解析逻辑集中于此，
//! 方便独立测试和复用。

use std::collections::HashMap;

use crate::csv_parser::{BitIndex, Record};
use crate::model::Value;

// ============================================================
//  协议常量
// ============================================================

/// 串口轮询命令码。
pub mod cmd {
    /// 读状态命令。
    pub const READ_STATUS: u8 = 0x08;
    /// 读状态子命令。
    pub const READ_SUBCMD: u8 = 0x02;
    /// 状态页面。
    pub const STATUS_PAGE: u8 = 0x30;
    /// 状态偏移。
    pub const STATUS_OFFSET: u8 = 0x10;
    /// 国产通信模块版本寄存器页。
    pub const VERSION_PAGE: u8 = 0x50;
    /// 国产通信模块版本寄存器偏移。
    pub const VERSION_OFFSET: u8 = 0x10;
    /// 通信模块版本 + PLC 版本共四字节。
    pub const VERSION_LENGTH: u8 = 0x04;
}

/// 各类阈值常量。
pub mod threshold {
    /// 单设备超时次数达到此值后开始跳过轮询。
    pub const DEVICE_TIMEOUT_SKIP: u32 = 3;
    /// 设备超时后每跳过此数轮次重新尝试。
    pub const DEVICE_SKIP_ROUNDS: u32 = 10;
    /// 端口级超时次数达到此值后触发主备切换。
    pub const PORT_FAILOVER_TIMEOUT: u32 = 20;
    /// 解析失败次数达到此值后触发主备切换。
    pub const PARSE_FAIL_FAILOVER: u32 = 100;
    /// 连续相同数据达到此值后强制推送一次。
    pub const SAME_DATA_BROADCAST: u32 = 10;
}

// ============================================================
//  数据帧
// ============================================================

/// 解析后的串口响应数据包。
#[derive(Debug, Clone)]
pub struct DataPacket {
    #[allow(unused)]
    pub addr: u8,
    #[allow(unused)]
    pub cmd: u8,
    #[allow(unused)]
    pub len: u8,
    pub status: Vec<u8>,
    #[allow(unused)]
    pub crc: u8,
}

impl DataPacket {
    pub fn new(addr: u8, cmd: u8, len: u8, status: Vec<u8>, crc: u8) -> Self {
        Self {
            addr,
            cmd,
            len,
            status,
            crc,
        }
    }
}

/// 严格校验后的通信模块版本响应。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum StationVersionResponse {
    /// 国产模块：前两字节为通信模块版本，后两字节为 PLC 版本，均为大端。
    Domestic {
        payload: [u8; 4],
        communication_module_version: u16,
        plc_version: u16,
    },
    /// 俄供模块以四字节全 `0xFF` 响应该寄存器。
    LegacyRussian { payload: [u8; 4] },
}

// ============================================================
//  协议解析函数
// ============================================================

/// 累加和校验（最后一个字节为校验字节）。
pub fn verify_checksum(data: &[u8]) -> bool {
    if data.is_empty() {
        return false;
    }
    let sum: u8 = data[..data.len() - 1]
        .iter()
        .fold(0u8, |acc, &x| acc.wrapping_add(x));
    sum == data[data.len() - 1]
}

/// 构造通信模块版本查询：`[Addr,08,02,50,10,04,Checksum]`。
pub fn build_station_version_query(device_address: u8) -> [u8; 7] {
    let mut frame = [
        device_address,
        cmd::READ_STATUS,
        cmd::READ_SUBCMD,
        cmd::VERSION_PAGE,
        cmd::VERSION_OFFSET,
        cmd::VERSION_LENGTH,
        0,
    ];
    frame[6] = frame[..6]
        .iter()
        .fold(0u8, |sum, byte| sum.wrapping_add(*byte));
    frame
}

/// 严格解析固定 9 字节版本响应：
/// `00 00 04 <variable-byte> <module-hi> <module-lo> <plc-hi> <plc-lo> <checksum>`。
/// 第四字节允许变化，不限定取值，但仍参与整帧累加和校验。
///
/// 只有完整帧头、长度和累加和均正确时，四字节全 `FF` 才会被识别为俄供模块。
pub fn parse_station_version_response(
    frame: &[u8],
) -> Result<StationVersionResponse, &'static str> {
    if frame.len() != 9 {
        return Err("版本响应长度必须为 9 字节");
    }
    if frame[..3] != [0x00, 0x00, 0x04] {
        return Err("版本响应帧头不匹配");
    }
    if !verify_checksum(frame) {
        return Err("版本响应校验和不匹配");
    }
    let payload = [frame[4], frame[5], frame[6], frame[7]];
    if payload == [0xFF; 4] {
        return Ok(StationVersionResponse::LegacyRussian { payload });
    }
    Ok(StationVersionResponse::Domestic {
        payload,
        communication_module_version: u16::from_be_bytes([payload[0], payload[1]]),
        plc_version: u16::from_be_bytes([payload[2], payload[3]]),
    })
}

/// 解析响应帧：`[Addr][CMD][Len][Status...][CRC]`。
pub fn parse_data_packet(data: &[u8]) -> Result<DataPacket, &'static str> {
    if data.len() < 4 {
        return Err("数据帧过短");
    }

    let len = data[2] as usize;
    if len + 2 != data.len() - 3 {
        return Err("数据帧长度不匹配");
    }
    if !verify_checksum(data) {
        return Err("校验和不匹配");
    }

    Ok(DataPacket::new(
        data[0],
        data[1],
        data[2],
        data[3..data.len() - 1].to_vec(),
        data[data.len() - 1],
    ))
}

/// 根据数据点定义解析状态字节为业务值。
pub fn parse_status(data: &[u8], records: &[Record]) -> HashMap<String, Value> {
    let mut results = HashMap::new();

    for record in records {
        // byte_index 为 1-based，0 视为非法，避免 0 - 1 下溢
        let byte_index = match record.byte_index.checked_sub(1) {
            Some(v) => v as usize,
            None => continue,
        };
        if byte_index >= data.len() {
            continue;
        }

        let raw_value = match &record.bit_index {
            BitIndex::Single(bit) => {
                // u8 移位宽度必须 < 8，否则 debug 下 panic、release 下行为未定
                if *bit >= 8 {
                    continue;
                }
                ((data[byte_index] >> (*bit as usize)) & 1) as u32
            }
            BitIndex::Range(range) => {
                let total_bits = (data.len() * 8) as u32;
                let start_bit = *range.start();
                let end_bit = *range.end();

                if start_bit > end_bit || end_bit >= total_bits {
                    continue;
                }

                let start_byte = byte_index + (start_bit / 8) as usize;
                let end_byte = byte_index + (end_bit / 8) as usize;

                if end_byte >= data.len() {
                    continue;
                }

                let num_bits = end_bit - start_bit + 1;
                if num_bits > 32 {
                    continue;
                }

                let mut combined = 0u64;
                for (i, byte) in data.iter().enumerate().take(end_byte + 1).skip(start_byte) {
                    let byte = *byte as u64;
                    if record.byte_order == 1 {
                        combined = (combined << 8) | byte; // 大端
                    } else {
                        combined |= byte << (8 * (i - start_byte)); // 小端
                    }
                }

                let bit_offset = start_bit % 8;
                let mask = if num_bits == 32 {
                    u32::MAX as u64
                } else {
                    (1u64 << num_bits) - 1
                };
                ((combined >> bit_offset) & mask) as u32
            }
        };

        let value = match record.type_.as_str() {
            "uint" => Value::UInt(raw_value),
            "bool" => Value::Bool(raw_value != 0),
            "float" => Value::Float(raw_value as f32 / 100.0),
            _ => continue,
        };

        results.insert(record.kks.clone(), value);
    }

    results
}

/// 在字节流中查找连续两个 `0x00` 的位置。
pub fn find_double_zero(buffer: &[u8]) -> Option<usize> {
    buffer.windows(2).position(|w| w == [0, 0])
}

/// 十六进制字符串转字节数组。
pub fn hex_str_to_bytes(hex_str: &str) -> Result<Vec<u8>, String> {
    // 后续按两个字节切片；先拒绝非 ASCII，既保证切片一定落在 UTF-8
    // 字符边界上，也避免把全角数字等看起来相似的字符当成十六进制输入。
    if !hex_str.is_ascii() {
        return Err("Hex 字符串只能包含 ASCII 字符".to_string());
    }
    if hex_str.len() % 2 != 0 {
        return Err("Hex 字符串长度不是偶数".to_string());
    }
    (0..hex_str.len())
        .step_by(2)
        .map(|i| u8::from_str_radix(&hex_str[i..i + 2], 16).map_err(|e| e.to_string()))
        .collect()
}

// ============================================================
//  单元测试
// ============================================================

#[cfg(test)]
mod tests {
    use super::*;

    // ---- verify_checksum ----

    #[test]
    fn verify_checksum_correct() {
        // 0x01 + 0x02 + 0x03 = 0x06
        assert!(verify_checksum(&[0x01, 0x02, 0x03, 0x06]));
    }

    #[test]
    fn verify_checksum_incorrect() {
        assert!(!verify_checksum(&[0x01, 0x02, 0x03, 0xFF]));
    }

    #[test]
    fn verify_checksum_empty() {
        assert!(!verify_checksum(&[]));
    }

    #[test]
    fn verify_checksum_single_byte() {
        // 空前缀和为 0
        assert!(verify_checksum(&[0x00]));
    }

    #[test]
    fn verify_checksum_wrapping() {
        // 0xFF + 0x02 = 0x01 (wrapping)
        assert!(verify_checksum(&[0xFF, 0x02, 0x01]));
    }

    #[test]
    fn station_version_query_matches_protocol_example_with_full_sum() {
        assert_eq!(
            build_station_version_query(0x44),
            [0x44, 0x08, 0x02, 0x50, 0x10, 0x04, 0xB2]
        );
    }

    #[test]
    fn station_version_response_parses_domestic_versions() {
        assert_eq!(
            parse_station_version_response(&[0x00, 0x00, 0x04, 0x08, 0x01, 0x04, 0x00, 0x05, 0x16])
                .unwrap(),
            StationVersionResponse::Domestic {
                payload: [0x01, 0x04, 0x00, 0x05],
                communication_module_version: 0x0104,
                plc_version: 0x0005,
            }
        );
    }

    #[test]
    fn station_version_response_accepts_real_device_80_header() {
        assert_eq!(
            parse_station_version_response(&[0x00, 0x00, 0x04, 0x80, 0x01, 0x03, 0x00, 0x00, 0x88])
                .unwrap(),
            StationVersionResponse::Domestic {
                payload: [0x01, 0x03, 0x00, 0x00],
                communication_module_version: 0x0103,
                plc_version: 0x0000,
            }
        );
    }

    #[test]
    fn station_version_response_accepts_81_example() {
        assert_eq!(
            parse_station_version_response(&[0x00, 0x00, 0x04, 0x81, 0x01, 0x03, 0x00, 0x0A, 0x93])
                .unwrap(),
            StationVersionResponse::Domestic {
                payload: [0x01, 0x03, 0x00, 0x0A],
                communication_module_version: 0x0103,
                plc_version: 0x000A,
            }
        );
    }

    #[test]
    fn station_version_response_variable_byte_still_requires_checksum() {
        for variable_byte in 0u8..=u8::MAX {
            for payload in [[0x01, 0x03, 0x00, 0x0A], [0xFF; 4]] {
                let mut frame = [0x00, 0x00, 0x04, variable_byte, 0, 0, 0, 0, 0];
                frame[4..8].copy_from_slice(&payload);
                frame[8] = frame[..8].iter().fold(0u8, |sum, byte| sum.wrapping_add(*byte));
                let expected = if payload == [0xFF; 4] {
                    StationVersionResponse::LegacyRussian { payload }
                } else {
                    StationVersionResponse::Domestic {
                        payload,
                        communication_module_version: 0x0103,
                        plc_version: 0x000A,
                    }
                };
                assert_eq!(parse_station_version_response(&frame), Ok(expected));
                frame[8] ^= 1;
                assert_eq!(
                    parse_station_version_response(&frame),
                    Err("版本响应校验和不匹配")
                );
            }
        }
    }

    #[test]
    fn station_version_response_recognizes_only_valid_all_ff_payload() {
        assert_eq!(
            parse_station_version_response(&[0x00, 0x00, 0x04, 0x08, 0xFF, 0xFF, 0xFF, 0xFF, 0x08])
                .unwrap(),
            StationVersionResponse::LegacyRussian { payload: [0xFF; 4] }
        );
        assert!(parse_station_version_response(&[
            0x00, 0x00, 0x04, 0x08, 0xFF, 0xFF, 0xFF, 0xFE, 0x07
        ])
        .is_ok());
    }

    #[test]
    fn station_version_response_rejects_bad_shape_and_checksum() {
        assert!(parse_station_version_response(&[0; 8]).is_err());
        assert!(parse_station_version_response(&[
            0x00, 0x00, 0x05, 0x08, 0x01, 0x04, 0x00, 0x05, 0x17
        ])
        .is_err());
        assert!(parse_station_version_response(&[
            0x00, 0x00, 0x04, 0x08, 0x01, 0x04, 0x00, 0x05, 0x14
        ])
        .is_err());
    }

    // ---- parse_data_packet ----

    #[test]
    fn parse_data_packet_normal() {
        // Addr=0x01, CMD=0x08, Len=0x02, Status=[0x10, 0x20, 0x30], CRC
        // 帧格式要求: len + 2 == data.len() - 3
        // len=2, data.len()-3=4 → 需要 data.len()=7
        let mut data = vec![0x01, 0x08, 0x02, 0x10, 0x20, 0x30];
        let crc: u8 = data.iter().fold(0u8, |a, &b| a.wrapping_add(b));
        data.push(crc);

        let pkt = parse_data_packet(&data).unwrap();
        assert_eq!(pkt.addr, 0x01);
        assert_eq!(pkt.cmd, 0x08);
        assert_eq!(pkt.len, 0x02);
        assert_eq!(pkt.status, vec![0x10, 0x20, 0x30]);
        assert_eq!(pkt.crc, crc);
    }

    #[test]
    fn parse_data_packet_too_short() {
        let err = parse_data_packet(&[0x01, 0x02, 0x03]).unwrap_err();
        assert_eq!(err, "数据帧过短");
    }

    #[test]
    fn parse_data_packet_length_mismatch() {
        // Len=0x05 但实际 status 只有 2 字节
        let mut data = vec![0x01, 0x08, 0x05, 0x10, 0x20];
        let crc: u8 = data.iter().fold(0u8, |a, &b| a.wrapping_add(b));
        data.push(crc);

        let err = parse_data_packet(&data).unwrap_err();
        assert_eq!(err, "数据帧长度不匹配");
    }

    #[test]
    fn parse_data_packet_bad_checksum() {
        // Len=0x02, data.len()=7, len+2==4==7-3 → 长度正确
        // 但 CRC 故意错误
        let data = vec![0x01, 0x08, 0x02, 0x10, 0x20, 0x30, 0xFF];
        let err = parse_data_packet(&data).unwrap_err();
        assert_eq!(err, "校验和不匹配");
    }

    // ---- parse_status ----

    fn make_record(
        kks: &str,
        type_: &str,
        byte_index: u32,
        bit_index: BitIndex,
        byte_order: u32,
    ) -> Record {
        Record {
            kks: kks.to_string(),
            type_: type_.to_string(),
            _f_type: String::new(),
            byte_index,
            bit_index,
            _def: 0,
            _max: 0,
            _min: 0,
            byte_order,
        }
    }

    #[test]
    fn parse_status_single_bit() {
        let data = [0b0000_0100]; // bit 2 = 1
        let records = vec![
            make_record("K1", "bool", 1, BitIndex::Single(2), 0),
            make_record("K2", "bool", 1, BitIndex::Single(0), 0),
        ];
        let result = parse_status(&data, &records);
        assert_eq!(result.get("K1"), Some(&Value::Bool(true)));
        assert_eq!(result.get("K2"), Some(&Value::Bool(false)));
    }

    #[test]
    fn parse_status_range_little_endian() {
        // 2 bytes little-endian: 0x34, 0x12 → 0x1234
        // 取 bits 0..=15 → 0x1234 = 4660
        let data = [0x34, 0x12];
        let records = vec![make_record("K1", "uint", 1, BitIndex::Range(0..=15), 0)];
        let result = parse_status(&data, &records);
        assert_eq!(result.get("K1"), Some(&Value::UInt(0x1234)));
    }

    #[test]
    fn parse_status_range_big_endian() {
        // 2 bytes big-endian: 0x12, 0x34 → 0x1234
        let data = [0x12, 0x34];
        let records = vec![make_record("K1", "uint", 1, BitIndex::Range(0..=15), 1)];
        let result = parse_status(&data, &records);
        assert_eq!(result.get("K1"), Some(&Value::UInt(0x1234)));
    }

    #[test]
    fn parse_status_range_32_bits_little_endian() {
        let data = [0x78, 0x56, 0x34, 0x12];
        let records = vec![make_record("K1", "uint", 1, BitIndex::Range(0..=31), 0)];
        let result = parse_status(&data, &records);
        assert_eq!(result.get("K1"), Some(&Value::UInt(0x1234_5678)));
    }

    #[test]
    fn parse_status_range_32_bits_big_endian() {
        let data = [0x12, 0x34, 0x56, 0x78];
        let records = vec![make_record("K1", "uint", 1, BitIndex::Range(0..=31), 1)];
        let result = parse_status(&data, &records);
        assert_eq!(result.get("K1"), Some(&Value::UInt(0x1234_5678)));
    }

    #[test]
    fn parse_status_range_32_bits_unaligned_little_endian() {
        let data = [0x00, 0x00, 0x00, 0x00, 0x01];
        let records = vec![make_record("K1", "uint", 1, BitIndex::Range(1..=32), 0)];
        let result = parse_status(&data, &records);
        assert_eq!(result.get("K1"), Some(&Value::UInt(0x8000_0000)));
    }

    #[test]
    fn parse_status_range_wider_than_u32_skipped() {
        let data = [0xFF, 0xFF, 0xFF, 0xFF, 0xFF];
        let records = vec![make_record("bad", "uint", 1, BitIndex::Range(0..=32), 0)];
        let result = parse_status(&data, &records);
        assert!(!result.contains_key("bad"));
    }

    #[test]
    fn parse_status_out_of_bounds() {
        let data = [0xFF];
        let records = vec![
            make_record("K1", "uint", 5, BitIndex::Single(0), 0), // byte_index=5 超出
        ];
        let result = parse_status(&data, &records);
        assert!(result.is_empty());
    }

    #[test]
    fn parse_status_byte_index_zero_skipped() {
        // byte_index=0 1-based 不合法，不应导致 panic
        let data = [0xFF];
        let records = vec![
            make_record("bad", "bool", 0, BitIndex::Single(0), 0),
            make_record("ok", "bool", 1, BitIndex::Single(0), 0),
        ];
        let result = parse_status(&data, &records);
        assert!(!result.contains_key("bad"));
        assert_eq!(result.get("ok"), Some(&Value::Bool(true)));
    }

    #[test]
    fn parse_status_single_bit_out_of_range_skipped() {
        // bit=8 超出 u8 范围，必须跳过而非 panic
        let data = [0xFF];
        let records = vec![
            make_record("bad", "bool", 1, BitIndex::Single(8), 0),
            make_record("ok", "bool", 1, BitIndex::Single(3), 0),
        ];
        let result = parse_status(&data, &records);
        assert!(!result.contains_key("bad"));
        assert_eq!(result.get("ok"), Some(&Value::Bool(true)));
    }

    #[test]
    fn parse_status_float() {
        // raw_value = 0x01F4 = 500 → float = 500 / 100.0 = 5.0
        let data = [0xF4, 0x01]; // little-endian
        let records = vec![make_record("K1", "float", 1, BitIndex::Range(0..=15), 0)];
        let result = parse_status(&data, &records);
        assert_eq!(result.get("K1"), Some(&Value::Float(5.0)));
    }

    // ---- find_double_zero ----

    #[test]
    fn find_double_zero_found() {
        assert_eq!(find_double_zero(&[0x01, 0x00, 0x00, 0x02]), Some(1));
    }

    #[test]
    fn find_double_zero_not_found() {
        assert_eq!(find_double_zero(&[0x01, 0x00, 0x02, 0x00]), None);
    }

    #[test]
    fn find_double_zero_at_start() {
        assert_eq!(find_double_zero(&[0x00, 0x00, 0x01]), Some(0));
    }

    #[test]
    fn find_double_zero_at_end() {
        assert_eq!(find_double_zero(&[0x01, 0x00, 0x00]), Some(1));
    }

    #[test]
    fn find_double_zero_too_short() {
        assert_eq!(find_double_zero(&[0x00]), None);
        assert_eq!(find_double_zero(&[]), None);
    }

    // ---- hex_str_to_bytes ----

    #[test]
    fn hex_str_to_bytes_normal() {
        assert_eq!(hex_str_to_bytes("0A1BFF").unwrap(), vec![0x0A, 0x1B, 0xFF]);
    }

    #[test]
    fn hex_str_to_bytes_empty() {
        assert_eq!(hex_str_to_bytes("").unwrap(), Vec::<u8>::new());
    }

    #[test]
    fn hex_str_to_bytes_odd_length() {
        assert!(hex_str_to_bytes("0A1").is_err());
    }

    #[test]
    fn hex_str_to_bytes_invalid_chars() {
        assert!(hex_str_to_bytes("ZZZZ").is_err());
    }

    #[test]
    fn hex_str_to_bytes_lowercase() {
        assert_eq!(hex_str_to_bytes("ff00").unwrap(), vec![0xFF, 0x00]);
    }

    #[test]
    fn hex_str_to_bytes_rejects_multibyte_unicode_without_panicking() {
        assert!(hex_str_to_bytes("中0").is_err());
        assert!(hex_str_to_bytes("🙂").is_err());
    }

    #[test]
    fn hex_str_to_bytes_rejects_fullwidth_hex_characters() {
        assert!(hex_str_to_bytes("ＦＦ").is_err());
        assert!(hex_str_to_bytes("１２").is_err());
    }
}
