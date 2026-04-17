//! CSV 配置文件解析模块。
//!
//! 每个 CSV 文件描述一个设备的通信参数与数据点定义。
//! 文件前 7 行为设备配置头，之后为数据记录表。

use std::fs::File;
use std::ops::RangeInclusive;
use std::path::{Path, PathBuf};
use std::str::FromStr;

use csv::ReaderBuilder;
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
        .ok_or(CsvParseError::MissingHeader { field, row: idx + 1 })?;
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
        .get(0)
        .map(|s| s.trim().to_string())
        .filter(|s| !s.is_empty())
        .ok_or(CsvParseError::MissingHeader { field: "com", row: 1 })?;

    let config = Config {
        com,
        baud_rate: parse_required(&headers, 1, "baud_rate")?,
        is_special: headers.get(2).and_then(|s| s.trim().parse().ok()).unwrap_or(false),
        com_index: parse_required(&headers, 3, "com_index")?,
        device_id: parse_required(&headers, 4, "device_id")?,
        data_len: parse_required(&headers, 5, "data_len")?,
        kks_prefix: headers.get(6).cloned().unwrap_or_default(),
    };

    // 跳过表头分隔行
    rdr.records().next();

    // 读取数据记录并拼接 KKS 前缀
    let mut records = Vec::new();
    for result in rdr.deserialize() {
        let mut record: Record = result?;
        record.kks = format!("{}{}", config.kks_prefix, record.kks);
        records.push(record);
    }

    Ok((config, records))
}
