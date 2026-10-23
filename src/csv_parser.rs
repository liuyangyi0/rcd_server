//! CSV 配置文件解析模块。
//!
//! 每个 CSV 文件描述一个设备的通信参数与数据点定义。
//! 文件前 7 行为设备配置头，之后为数据记录表。

use std::fs::File;
use std::ops::RangeInclusive;
use std::path::{Path, PathBuf};
use std::str::FromStr;

use csv::ReaderBuilder;
use log::warn;
use serde::Deserialize;
use thiserror::Error;

// ============================================================
//  错误类型
// ============================================================

/// CSV 解析过程中的结构化错误。
#[derive(Debug, Error)]
pub enum CsvParseError {
    #[error("打开 CSV 文件失败: {path:?}: {source}")]
    Io {
        path: PathBuf,
        #[source]
        source: std::io::Error,
    },

    #[error("读取 CSV 记录失败: {0}")]
    Csv(#[from] csv::Error),

    #[error("CSV 头缺少必填字段 '{field}' (第 {row} 行第 2 列)")]
    MissingHeader { field: &'static str, row: usize },

    #[error("CSV 头字段 '{field}' 解析失败 (值=\"{value}\"): {reason}")]
    BadHeaderValue {
        field: &'static str,
        value: String,
        reason: String,
    },
}

/// 解析 CSV 头区域的必填数值字段；缺失或不合法时返回带字段名的错误。
fn parse_required<T: FromStr>(
    headers: &[String],
    idx: usize,
    field: &'static str,
) -> Result<T, CsvParseError>
where
    T::Err: std::fmt::Display,
{
    let raw = headers
        .get(idx)
        .map(|s| s.trim())
        .filter(|s| !s.is_empty())
        .ok_or(CsvParseError::MissingHeader {
            field,
            row: idx + 1,
        })?;
    raw.parse::<T>().map_err(|e| CsvParseError::BadHeaderValue {
        field,
        value: raw.to_string(),
        reason: e.to_string(),
    })
}

// ============================================================
//  位索引类型
// ============================================================

/// 数据字节中的位定位方式：单个位 或 连续位范围。
#[derive(Debug, Deserialize, Clone)]
pub enum BitIndex {
    /// 单个位（如第 3 位）。
    Single(u32),
    /// 连续位范围（如第 0..7 位）。
    Range(RangeInclusive<u32>),
}

impl FromStr for BitIndex {
    type Err = String;

    fn from_str(s: &str) -> Result<Self, Self::Err> {
        if let Some(dot_pos) = s.find('.') {
            let start = s[..dot_pos].parse::<u32>().map_err(|e| e.to_string())?;
            let end = s[dot_pos + 1..].parse::<u32>().map_err(|e| e.to_string())?;
            Ok(BitIndex::Range(start..=end))
        } else {
            let index = s.parse::<u32>().map_err(|e| e.to_string())?;
            Ok(BitIndex::Single(index))
        }
    }
}

/// serde 自定义反序列化：将字符串解析为 [`BitIndex`]。
fn deserialize_bit_index<'de, D>(deserializer: D) -> Result<BitIndex, D::Error>
where
    D: serde::Deserializer<'de>,
{
    let s = String::deserialize(deserializer)?;
    s.parse().map_err(serde::de::Error::custom)
}

// ============================================================
//  数据记录
// ============================================================

/// 数据点表的列头（与 [`Record`] 字段顺序一致）。
/// 工具生成 csv 模板时复用此常量，避免字段顺序漂移。
pub const RECORD_CSV_HEADER: &str = "kks,type_,f_type,byte_index,bit_index,def,max,min,lh";

/// CSV 中一行数据点描述。
#[derive(Debug, Deserialize, Clone)]
pub struct Record {
    /// KKS 标识符（已拼接前缀）。
    pub kks: String,
    /// 值类型字符串（`"uint"` / `"bool"` / `"float"`）。
    pub type_: String,
    /// 功能类型（预留字段）。
    #[serde(rename = "f_type")]
    pub _f_type: String,
    /// 数据在响应帧中的字节偏移（1-based）。
    pub byte_index: u32,
    /// 位偏移。
    #[serde(deserialize_with = "deserialize_bit_index")]
    pub bit_index: BitIndex,
    /// 默认值（预留）。
    #[serde(rename = "def")]
    pub _def: u32,
    /// 最大值（预留）。
    #[serde(rename = "max")]
    pub _max: u32,
    /// 最小值（预留）。
    #[serde(rename = "min")]
    pub _min: u32,
    /// 字节序标记（`1` = 大端，`0` = 小端）。
    #[serde(rename = "lh")]
    pub byte_order: u32,
}

/// 校验一个数据点是否能在配置声明的状态数据区内安全取值。
fn record_bounds_error(record: &Record, data_len: usize) -> Option<String> {
    let byte_index = match record.byte_index.checked_sub(1) {
        Some(value) => value as usize,
        None => return Some("byte_index=0，但该字段从 1 开始计数".to_string()),
    };

    if byte_index >= data_len {
        return Some(format!(
            "byte_index={} 超出状态数据长度 1..={}",
            record.byte_index, data_len
        ));
    }

    match &record.bit_index {
        BitIndex::Single(bit) if *bit >= 8 => {
            Some(format!("bit_index={} 超出单字节位范围 0..=7", bit))
        }
        BitIndex::Single(_) => None,
        BitIndex::Range(range) => {
            let start_bit = u64::from(*range.start());
            let end_bit = u64::from(*range.end());
            if start_bit > end_bit {
                return Some(format!("位范围 {}..{} 起点大于终点", start_bit, end_bit));
            }

            let num_bits = end_bit - start_bit + 1;
            if num_bits > 32 {
                return Some(format!(
                    "位范围 {}..{} 宽度为 {}，超过 u32 的 32 位上限",
                    start_bit, end_bit, num_bits
                ));
            }

            let end_byte = byte_index as u64 + end_bit / 8;
            if end_byte >= data_len as u64 {
                return Some(format!(
                    "位范围 {}..{} 需要访问第 {} 字节，但状态数据只有 {} 字节",
                    start_bit,
                    end_bit,
                    end_byte + 1,
                    data_len
                ));
            }
            None
        }
    }
}

// ============================================================
//  设备配置头
// ============================================================

/// CSV 文件前 7 行中提取的设备通信配置。
#[derive(Debug, Deserialize, Clone)]
pub struct Config {
    /// 串口号，如 `"COM3"`。
    pub com: String,
    /// 波特率。
    pub baud_rate: u32,
    /// 是否为特殊串口（Mark/Space 奇偶校验模式）。
    pub is_special: bool,
    /// 串口索引编号。
    pub com_index: u8,
    /// 设备站地址。
    pub device_id: u8,
    /// 期望数据长度。
    pub data_len: u8,
    /// KKS 前缀。
    pub kks_prefix: String,
}

// ============================================================
//  设备运行时状态
// ============================================================

/// 设备静态配置：通信参数 + 数据点定义（从 CSV 解析，运行期不可变）。
#[derive(Debug, Clone)]
pub struct DeviceConfig {
    /// 通信配置。
    pub config: Config,
    /// 数据点定义列表。
    pub records: Vec<Record>,
}

impl DeviceConfig {
    /// 创建新的设备配置实例。
    pub fn new(config: Config, records: Vec<Record>) -> Self {
        Self { config, records }
    }
}

// ============================================================
//  CSV 解析入口
// ============================================================

// Older point tables use COM/BaudRate/.../Pre and KKS/Type/.../LH.
// Accept those labels without rewriting data or weakening column order checks.
fn csv_label_matches(actual: &str, expected: &str) -> bool {
    let actual = actual.trim().replace('_', "").to_ascii_lowercase();
    let expected = expected.replace('_', "").to_ascii_lowercase();
    actual == expected || (expected == "kksprefix" && actual == "pre")
}

/// 运维保存前的严格校验。与启动解析器分开，避免改变已有采集行为；
/// 不把非法布尔值、越界点、未知类型或重复 KKS 静默当成有效配置。
pub fn validate_csv_content(content: &str) -> Result<(Config, Vec<Record>), String> {
    validate_csv_content_inner(content, None)
}

/// 保存配置时保留点地址/位范围诊断，但不让可恢复的点表问题阻止配置更新。
/// 文件结构、通信头部、类型及身份冲突仍严格校验；不修复或丢弃任何记录。
pub fn validate_csv_content_with_point_warnings(
    content: &str,
    mut warning: impl FnMut(String),
) -> Result<(Config, Vec<Record>), String> {
    validate_csv_content_inner(content, Some(&mut warning))
}

fn validate_csv_content_inner(
    content: &str,
    mut warning: Option<&mut dyn FnMut(String)>,
) -> Result<(Config, Vec<Record>), String> {
    let mut reader = ReaderBuilder::new()
        .has_headers(false)
        .from_reader(content.trim_start_matches('\u{feff}').as_bytes());
    let mut rows = reader.records();
    let fields = [
        "com",
        "baud_rate",
        "is_special",
        "com_index",
        "device_id",
        "data_len",
        "kks_prefix",
    ];
    let mut headers = Vec::with_capacity(fields.len());
    for (index, field) in fields.iter().enumerate() {
        let row = rows
            .next()
            .ok_or_else(|| format!("第 {} 行缺少配置字段 {field}", index + 1))?
            .map_err(|error| error.to_string())?;
        if !row
            .get(0)
            .is_some_and(|actual| csv_label_matches(actual, field))
        {
            return Err(format!("第 {} 行应为配置字段 {field}", index + 1));
        }
        headers.push(row.get(1).unwrap_or_default().to_string());
    }
    let com = headers[0].trim().to_string();
    if com.is_empty() || com.chars().any(char::is_control) {
        return Err("com 必须为非空串口名且不含控制字符".to_string());
    }
    if headers[6].chars().any(char::is_control) {
        return Err("kks_prefix 不能包含控制字符".to_string());
    }
    // FALSE is the deployed spelling of the runtime's existing false value.
    // Do not case-fold TRUE: the old startup parser treats it as false, so
    // accepting it as true here would silently change special-port semantics.
    let is_special = if headers[2].trim().eq_ignore_ascii_case("false") {
        false
    } else {
        parse_required(&headers, 2, "is_special").map_err(|e| e.to_string())?
    };
    let config = Config {
        com,
        baud_rate: parse_required(&headers, 1, "baud_rate").map_err(|e| e.to_string())?,
        is_special,
        com_index: parse_required(&headers, 3, "com_index").map_err(|e| e.to_string())?,
        device_id: parse_required(&headers, 4, "device_id").map_err(|e| e.to_string())?,
        data_len: parse_required(&headers, 5, "data_len").map_err(|e| e.to_string())?,
        kks_prefix: headers[6].clone(),
    };
    if config.baud_rate == 0 {
        return Err("baud_rate 必须大于 0".to_string());
    }
    let columns = rows
        .next()
        .ok_or_else(|| "第 8 行缺少点表列头".to_string())?
        .map_err(|error| error.to_string())?;
    let expected_columns: Vec<_> = RECORD_CSV_HEADER.split(',').collect();
    if columns.len() != expected_columns.len()
        || !columns
            .iter()
            .zip(expected_columns)
            .all(|(actual, expected)| csv_label_matches(actual, expected))
    {
        return Err(format!("第 8 行列头必须为 {RECORD_CSV_HEADER}"));
    }
    let mut records = Vec::new();
    let mut names = std::collections::HashSet::new();
    for (offset, row) in rows.enumerate() {
        let row_number = offset + 9;
        let row = row.map_err(|error| format!("第 {row_number} 行: {error}"))?;
        let mut record: Record = row
            .deserialize(None)
            .map_err(|error| format!("第 {row_number} 行: {error}"))?;
        if record.kks.trim().is_empty() || record.kks.chars().any(char::is_control) {
            return Err(format!("第 {row_number} 行 KKS 为空或含控制字符"));
        }
        record.kks = format!("{}{}", config.kks_prefix, record.kks);
        if !names.insert(record.kks.clone()) {
            return Err(format!("第 {row_number} 行 KKS 重复: {}", record.kks));
        }
        if !matches!(record.type_.as_str(), "uint" | "bool" | "float") {
            return Err(format!(
                "第 {row_number} 行 {}: type_ 仅支持 uint/bool/float",
                record.kks
            ));
        }
        if record.byte_order > 1 {
            return Err(format!(
                "第 {row_number} 行 {}: lh 必须为 0 或 1",
                record.kks
            ));
        }
        // Deployed files use either the wire Len field or the actual status
        // byte count (Len + 1); the serial receiver already accepts both.
        // Without a frame, validation must use their common protocol upper
        // bound, including Len=0 (one status byte) and Len=255 (256 bytes).
        // Keep the startup parser's existing warning behaviour unchanged.
        let max_status_bytes = usize::from(config.data_len) + 1;
        if let Some(reason) = record_bounds_error(&record, max_status_bytes) {
            let message = format!("第 {row_number} 行 {}: {reason}", record.kks);
            match warning.as_mut() {
                Some(report) => report(message),
                None => return Err(message),
            }
        }
        records.push(record);
    }
    Ok((config, records))
}

/// 解析一个设备 CSV 文件，返回 `(设备配置, 数据点列表)`。
///
/// 文件格式：
/// - 第 1~7 行：配置项（第 2 列为值）。
/// - 第 8 行：表头分隔行（跳过）。
/// - 第 9 行起：数据记录。
pub fn parse_csv<P: AsRef<Path>>(file_path: P) -> Result<(Config, Vec<Record>), CsvParseError> {
    let path_ref = file_path.as_ref();
    let file = File::open(path_ref).map_err(|e| CsvParseError::Io {
        path: path_ref.to_path_buf(),
        source: e,
    })?;
    let mut rdr = ReaderBuilder::new()
        .delimiter(b',')
        .has_headers(false)
        .from_reader(file);

    // 读取前 7 行配置头
    let mut headers = Vec::with_capacity(7);
    for _ in 0..7 {
        if let Some(result) = rdr.records().next() {
            let record = result?;
            headers.push(record.get(1).unwrap_or_default().to_string());
        }
    }

    // 必填字段缺失/不可解析时立即报错，避免默认 0 在 serialport::new() 里晚崩
    let com = headers
        .first()
        .map(|s| s.trim().to_string())
        .filter(|s| !s.is_empty())
        .ok_or(CsvParseError::MissingHeader {
            field: "com",
            row: 1,
        })?;

    let config = Config {
        com,
        baud_rate: parse_required(&headers, 1, "baud_rate")?,
        is_special: headers
            .get(2)
            .and_then(|s| s.trim().parse().ok())
            .unwrap_or(false),
        com_index: parse_required(&headers, 3, "com_index")?,
        device_id: parse_required(&headers, 4, "device_id")?,
        data_len: parse_required(&headers, 5, "data_len")?,
        kks_prefix: headers.get(6).cloned().unwrap_or_default(),
    };

    // 跳过表头分隔行
    rdr.records().next();

    // 读取数据记录并拼接 KKS 前缀
    let mut records = Vec::new();
    for (offset, result) in rdr.deserialize().enumerate() {
        let mut record: Record = result?;
        record.kks = format!("{}{}", config.kks_prefix, record.kks);
        if let Some(reason) = record_bounds_error(&record, config.data_len as usize) {
            warn!(
                "CSV 数据点配置越界: 文件={}, 行={}, KKS={}, data_len={}, 原因={}",
                path_ref.display(),
                offset + 9,
                record.kks,
                config.data_len,
                reason,
            );
        }
        records.push(record);
    }

    Ok((config, records))
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::Write;

    /// 写入一个临时 CSV 文件并返回路径；测试结束由 tempdir drop 自动清理。
    fn write_tmp(content: &str) -> (tempfile::TempDir, PathBuf) {
        let dir = tempfile::tempdir().expect("创建临时目录");
        let path = dir.path().join("test.csv");
        let mut f = File::create(&path).expect("创建临时文件");
        f.write_all(content.as_bytes()).expect("写入内容");
        (dir, path)
    }

    const VALID_HEADERS: &str = "\
com,COM3,
baud_rate,9600,
is_special,false,
com_index,1,
device_id,2,
data_len,8,
kks_prefix,PX_,
kks,type_,f_type,byte_index,bit_index,def,max,min,lh
";

    #[test]
    fn parse_valid_file_headers_only() {
        let (_d, p) = write_tmp(VALID_HEADERS);
        let (cfg, recs) = parse_csv(&p).unwrap();
        assert_eq!(cfg.com, "COM3");
        assert_eq!(cfg.baud_rate, 9600);
        assert!(!cfg.is_special);
        assert_eq!(cfg.device_id, 2);
        assert_eq!(cfg.data_len, 8);
        assert_eq!(cfg.kks_prefix, "PX_");
        assert!(recs.is_empty());
    }

    #[test]
    fn missing_com_returns_missing_header() {
        let content = VALID_HEADERS.replacen("com,COM3,", "com,,", 1);
        let (_d, p) = write_tmp(&content);
        let err = parse_csv(&p).unwrap_err();
        assert!(
            matches!(err, CsvParseError::MissingHeader { field: "com", .. }),
            "got {err:?}"
        );
    }

    #[test]
    fn bad_baud_rate_returns_bad_header_value() {
        let content = VALID_HEADERS.replacen("baud_rate,9600,", "baud_rate,abc,", 1);
        let (_d, p) = write_tmp(&content);
        let err = parse_csv(&p).unwrap_err();
        assert!(
            matches!(
                err,
                CsvParseError::BadHeaderValue {
                    field: "baud_rate",
                    ..
                }
            ),
            "got {err:?}"
        );
    }

    #[test]
    fn nonexistent_file_returns_io_error() {
        let err = parse_csv(Path::new("./_does_not_exist_xyz.csv")).unwrap_err();
        assert!(matches!(err, CsvParseError::Io { .. }), "got {err:?}");
    }

    fn test_record(byte_index: u32, bit_index: BitIndex) -> Record {
        Record {
            kks: "TEST".to_string(),
            type_: "uint".to_string(),
            _f_type: String::new(),
            byte_index,
            bit_index,
            _def: 0,
            _max: 0,
            _min: 0,
            byte_order: 0,
        }
    }

    #[test]
    fn record_bounds_validation_reports_config_errors_at_load_time() {
        assert!(record_bounds_error(&test_record(1, BitIndex::Single(7)), 1).is_none());
        assert!(record_bounds_error(&test_record(0, BitIndex::Single(0)), 1).is_some());
        assert!(record_bounds_error(&test_record(2, BitIndex::Single(0)), 1).is_some());
        assert!(record_bounds_error(&test_record(1, BitIndex::Single(8)), 1).is_some());
        assert!(record_bounds_error(&test_record(1, BitIndex::Range(0..=8)), 1).is_some());
        assert!(record_bounds_error(&test_record(1, BitIndex::Range(0..=32)), 8).is_some());
    }

    fn strict_fixture(data_len: u8, byte_index: u32, bit_index: &str) -> String {
        format!(
            "com,COM1,,,,,,,\n\
             baud_rate,9600,,,,,,,\n\
             is_special,false,,,,,,,\n\
             com_index,1,,,,,,,\n\
             device_id,1,,,,,,,\n\
             data_len,{data_len},,,,,,,\n\
             kks_prefix,PX_,,,,,,,\n\
             {RECORD_CSV_HEADER}\n\
             VALUE,uint,status,{byte_index},{bit_index},0,255,0,0\n"
        )
    }

    fn legacy_strict_fixture(data_len: u8, byte_index: u32, bit_index: &str) -> String {
        let mut content = strict_fixture(data_len, byte_index, bit_index);
        for (canonical, legacy) in [
            ("com,", "COM,"),
            ("baud_rate,", "BaudRate,"),
            ("is_special,false,", "IsSpecial,FALSE,"),
            ("com_index,", "ComIndex,"),
            ("device_id,", "DeviceID,"),
            ("data_len,", "DataLen,"),
            ("kks_prefix,", "Pre,"),
            (
                RECORD_CSV_HEADER,
                "KKS,Type,F_Type,byte_index,bit_index,Def,Max,Min,LH",
            ),
        ] {
            content = content.replace(canonical, legacy);
        }
        content
    }

    #[test]
    fn strict_csv_legacy_labels_and_false_match_runtime_values() {
        let content = legacy_strict_fixture(8, 1, "0.7");
        let (_directory, path) = write_tmp(&content);
        let (running, running_records) = parse_csv(&path).unwrap();
        let (validated, validated_records) = validate_csv_content(&content).unwrap();
        assert_eq!(validated.com, running.com);
        assert_eq!(validated.baud_rate, running.baud_rate);
        assert_eq!(validated.is_special, running.is_special);
        assert_eq!(validated.com_index, running.com_index);
        assert_eq!(validated.device_id, running.device_id);
        assert_eq!(validated.data_len, running.data_len);
        assert_eq!(validated.kks_prefix, running.kks_prefix);
        assert_eq!(validated_records[0].type_, running_records[0].type_);
        assert_eq!(validated_records[0].kks, "PX_VALUE");
        assert_eq!(
            parsed_wire_values(&validated_records, &[0xaa, 0xbb]),
            parsed_wire_values(&running_records, &[0xaa, 0xbb])
        );
        assert!(
            content.contains("IsSpecial,FALSE"),
            "validation must not rewrite the input"
        );
    }

    #[test]
    fn strict_csv_legacy_bom_crlf_and_label_spacing_are_accepted() {
        let content = format!(
            "\u{feff}{}",
            legacy_strict_fixture(8, 1, "0.7")
                .replace("COM,COM1", " CoM ,COM1")
                .replace("BaudRate,", " BAUD_RATE ,")
                .replace("KKS,Type,F_Type,", " KKS , Type , F_Type ,")
                .replace('\n', "\r\n")
        );
        let (_, records) = validate_csv_content(&content).unwrap();
        assert_eq!(records[0].kks, "PX_VALUE");
    }

    #[test]
    fn strict_csv_legacy_keeps_header_order_and_column_validation() {
        for content in [
            legacy_strict_fixture(8, 1, "0.7").replacen("COM,COM1", "Unknown,COM1", 1),
            legacy_strict_fixture(8, 1, "0.7").replace("BaudRate,9600", "DeviceID,9600"),
            legacy_strict_fixture(8, 1, "0.7").replace("KKS,Type,F_Type", "Type,KKS,F_Type"),
            legacy_strict_fixture(8, 1, "0.7").replace("KKS,Type,F_Type", "KKS,Unknown,F_Type"),
        ] {
            assert!(
                validate_csv_content(&content).is_err(),
                "accepted invalid legacy labels"
            );
        }
    }

    #[test]
    fn strict_csv_legacy_still_rejects_invalid_data_and_duplicates() {
        let valid = legacy_strict_fixture(1, 1, "0.7");
        for content in [
            valid.replace("FALSE", "maybe"),
            valid.replace("VALUE,uint", "VALUE,unknown"),
            valid.replace("VALUE,uint,status,1", "VALUE,uint,status,3"),
            valid.replace("status,1,0.7", "status,1,8"),
            format!("{valid}VALUE,uint,status,1,0.7,0,255,0,0\n"),
        ] {
            assert!(
                validate_csv_content(&content).is_err(),
                "accepted invalid legacy data"
            );
        }
    }

    #[test]
    fn point_warnings_keep_invalid_addresses_and_report_each_row_without_rewriting() {
        for (byte, bits, expected) in [
            (20, "0.15", "byte_index=20"),
            (0, "0", "byte_index=0"),
            (1, "8", "单字节位范围"),
            (1, "9.2", "起点大于终点"),
            (1, "0.32", "32 位上限"),
            (2, "0.15", "需要访问第 3 字节"),
        ] {
            let content = legacy_strict_fixture(1, byte, bits);
            let mut warnings = Vec::new();
            let (_, records) = validate_csv_content_with_point_warnings(&content, |message| {
                warnings.push(message)
            })
            .unwrap();
            assert_eq!(records.len(), 1);
            assert_eq!(records[0].byte_index, byte);
            assert_eq!(records[0].kks, "PX_VALUE");
            assert_eq!(warnings.len(), 1);
            assert!(warnings[0].contains("第 9 行 PX_VALUE"));
            assert!(warnings[0].contains(expected), "{}", warnings[0]);
            assert!(validate_csv_content(&content).is_err());
        }
        let content = format!(
            "{}OTHER,uint,status,22,0,0,255,0,0\n",
            legacy_strict_fixture(1, 20, "0")
        );
        let mut warnings = Vec::new();
        let (_, records) =
            validate_csv_content_with_point_warnings(&content, |message| warnings.push(message))
                .unwrap();
        assert_eq!(records.len(), 2);
        assert_eq!(warnings.len(), 2);
        assert!(warnings[1].contains("第 10 行 PX_OTHER"));
    }

    #[test]
    fn point_warnings_do_not_relax_file_structure_types_or_duplicate_identities() {
        let valid = legacy_strict_fixture(8, 1, "0");
        for content in [
            valid.replace("FALSE", "maybe"),
            valid.replace("VALUE,uint", "VALUE,unknown"),
            valid.replace("BaudRate,9600", "BaudRate,invalid"),
            valid.replace("KKS,Type", "Unknown,Type"),
            format!("{valid}VALUE,uint,status,20,0,0,255,0,0\n"),
        ] {
            assert!(validate_csv_content_with_point_warnings(&content, |_| {}).is_err());
        }
    }

    #[test]
    fn strict_csv_does_not_silently_enable_legacy_uppercase_true() {
        let content = legacy_strict_fixture(8, 1, "0.7").replace("FALSE", "TRUE");
        let (_directory, path) = write_tmp(&content);
        assert!(!parse_csv(path).unwrap().0.is_special);
        assert!(validate_csv_content(&content).is_err());
        // Explicit canonical true remains supported, exactly as at startup.
        let explicit = content.replace("TRUE", "true");
        assert!(validate_csv_content(&explicit).unwrap().0.is_special);
    }

    fn parsed_wire_values(records: &[Record], status: &[u8]) -> crate::model::Value {
        assert!(!status.is_empty() && status.len() <= 256);
        let mut frame = vec![
            1,
            crate::protocol::cmd::READ_STATUS,
            (status.len() - 1) as u8,
        ];
        frame.extend_from_slice(status);
        frame.push(crate::model::get_sum(&frame));
        let packet = crate::protocol::parse_data_packet(&frame).expect("valid real protocol frame");
        assert_eq!(packet.status, status);
        crate::protocol::parse_status(&packet.status, records)
            .remove("PX_VALUE")
            .expect("configured point is present in the actual frame")
    }

    #[test]
    fn strict_csv_matches_runtime_for_both_data_length_conventions() {
        // The same two-byte response has wire Len=1. Both data_len=1 and
        // data_len=2 are supported by configured_protocol_frame_len.
        for data_len in [1, 2] {
            let content = strict_fixture(data_len, 2, "0.7");
            let (_directory, path) = write_tmp(&content);
            let (runtime_config, runtime_records) = parse_csv(&path).unwrap();
            let actual = parsed_wire_values(&runtime_records, &[0xaa, 0xbb]);
            assert_eq!(actual, crate::model::Value::UInt(0xbb));
            let (validated_config, validated_records) = validate_csv_content(&content)
                .unwrap_or_else(|error| panic!("working data_len={data_len} rejected: {error}"));
            assert_eq!(validated_config.data_len, runtime_config.data_len);
            assert_eq!(
                parsed_wire_values(&validated_records, &[0xaa, 0xbb]),
                actual
            );
        }
    }

    #[test]
    fn strict_csv_accepts_cross_byte_ranges_at_protocol_boundary() {
        let content = strict_fixture(1, 1, "8.15");
        let (_directory, path) = write_tmp(&content);
        let (_, runtime_records) = parse_csv(&path).unwrap();
        let actual = parsed_wire_values(&runtime_records, &[0xaa, 0xbb]);
        assert_eq!(actual, crate::model::Value::UInt(0xbb));
        let (_, validated_records) = validate_csv_content(&content).unwrap();
        assert_eq!(
            parsed_wire_values(&validated_records, &[0xaa, 0xbb]),
            actual
        );
    }

    #[test]
    fn strict_csv_allows_protocol_length_endpoints() {
        for data_len in [0, u8::MAX] {
            let status_len = usize::from(data_len) + 1;
            let content = strict_fixture(data_len, status_len as u32, "0.7");
            let mut status = vec![0; status_len];
            status[status_len - 1] = 0xbb;
            let (_directory, path) = write_tmp(&content);
            let (_, runtime_records) = parse_csv(&path).unwrap();
            assert_eq!(
                parsed_wire_values(&runtime_records, &status),
                crate::model::Value::UInt(0xbb)
            );
            let (_, validated_records) = validate_csv_content(&content).unwrap();
            assert_eq!(
                parsed_wire_values(&validated_records, &status),
                crate::model::Value::UInt(0xbb)
            );
        }
    }

    #[test]
    fn strict_csv_rejects_real_byte_and_bit_overruns() {
        for (data_len, byte_index, bit_index) in [
            (0, 2, "0.7"),
            (1, 3, "0.7"),
            (255, 257, "0.7"),
            (1, 0, "0.7"),
            (1, 2, "0.8"),
            (1, 1, "8"),
            (1, 1, "7.0"),
            (8, 1, "0.32"),
        ] {
            let content = strict_fixture(data_len, byte_index, bit_index);
            assert!(
                validate_csv_content(&content).is_err(),
                "accepted true overrun data_len={data_len} byte_index={byte_index} bit_index={bit_index}"
            );
        }
        let zero_baud = strict_fixture(0, 1, "0.7").replace("baud_rate,9600", "baud_rate,0");
        assert!(validate_csv_content(&zero_baud).is_err());
        let too_long = strict_fixture(255, 256, "0.7").replace("data_len,255", "data_len,256");
        assert!(validate_csv_content(&too_long).is_err());
    }
}
