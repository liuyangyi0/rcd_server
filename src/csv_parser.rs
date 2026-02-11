//! CSV 配置文件解析模块。
//!
//! 每个 CSV 文件描述一个设备的通信参数与数据点定义。
//! 文件前 7 行为设备配置头，之后为数据记录表。

use std::error::Error;
use std::fs::File;
use std::ops::RangeInclusive;
use std::path::Path;
use std::str::FromStr;

use csv::ReaderBuilder;
use serde::Deserialize;

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
pub fn parse_csv<P: AsRef<Path>>(file_path: P) -> Result<(Config, Vec<Record>), Box<dyn Error>> {
    let file = File::open(file_path)?;
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

    let config = Config {
        com: headers.get(0).cloned().unwrap_or_default(),
        baud_rate: headers.get(1).and_then(|s| s.parse().ok()).unwrap_or(0),
        is_special: headers.get(2).and_then(|s| s.parse().ok()).unwrap_or(false),
        com_index: headers.get(3).and_then(|s| s.parse().ok()).unwrap_or(0),
        device_id: headers.get(4).and_then(|s| s.parse().ok()).unwrap_or(0),
        data_len: headers.get(5).and_then(|s| s.parse().ok()).unwrap_or(0),
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
