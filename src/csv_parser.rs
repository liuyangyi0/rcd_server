use csv::{ReaderBuilder};
use serde::Deserialize;
use std::error::Error;
use std::fs::File;
use std::path::Path;
use std::ops::RangeInclusive;
use std::str::FromStr;

// 定义 BitIndex 枚举
// 定义一个名为 `BitIndex` 的枚举，用于存储单个位索引或者位索引范围
#[derive(Debug, Deserialize, Clone)]
pub enum BitIndex {
    Single(u32),                // 单个位索引
    Range(RangeInclusive<u32>), // 位索引范围
}

// 为 `BitIndex` 实现 `FromStr` 特性，允许从字符串解析
impl FromStr for BitIndex {
    type Err = String;  // 定义错误类型为字符串，用于描述解析错误

    // 定义字符串解析方法
    fn from_str(s: &str) -> Result<Self, Self::Err> {
        // 检查字符串中是否存在'.'，用以区分是单个索引还是索引范围
        if let Some(dot_pos) = s.find('.') {
            // 解析'.'之前的部分作为起始索引
            let start = s[..dot_pos].parse::<u32>()
                .map_err(|e| e.to_string())?; // 转换错误信息为字符串
            // 解析'.'之后的部分作为结束索引
            let end = s[dot_pos + 1..].parse::<u32>()
                .map_err(|e| e.to_string())?; // 转换错误信息为字符串
            // 如果成功解析，返回一个表示范围的 `BitIndex`
            Ok(BitIndex::Range(start..=end))
        } else {
            // 如果没有找到'.'，则视为单个位索引
            let index = s.parse::<u32>()
                .map_err(|e| e.to_string())?; // 转换错误信息为字符串
            // 返回一个表示单个位索引的 `BitIndex`
            Ok(BitIndex::Single(index))
        }
    }
}


// 定义一个枚举来表示数据的大小端模式
#[derive(Debug, Deserialize)]
enum Endian {
    Big,
    Little,
}

//为 Endian 实现 FromStr 特性
impl FromStr for Endian {
    type Err = String;

    fn from_str(s: &str) -> Result<Self, Self::Err> {
        match s {
            "big" => Ok(Endian::Big),
            "little" => Ok(Endian::Little),
            _ => Err(format!("Invalid endian value: {}", s)),
        }
    }
}



// 定义 Record 结构体，使用 BitIndex 枚举
#[derive(Debug, Deserialize , Clone)]
pub struct Record {
    pub kks: String,
    pub type_: String,
    pub f_type: String,
    pub byte_index: u32,
    #[serde(deserialize_with = "deserialize_bit_index")]
    pub bit_index: BitIndex,
    pub def: u32,
    pub max: u32,
    pub min: u32,
    pub lh: u32,
}

#[derive(Debug, Deserialize, Clone)]
pub struct DeviceConfiguration {
    pub config: Config,
    //超时次数
    pub timeout_count: u32,
    //当前轮询次数
    pub current_round: u32,
    pub records: Vec<Record>, // 使用 Vec 来存储多个 Record 实例
    //解析失败次数
    pub parse_fail_count: u32,
    //站点是否通讯
    pub site_status: bool,
}

//DeviceConfiguration new
// impl DeviceConfiguration {
//     pub fn new(config: Config, records: Vec<Record>) -> Self {
//         DeviceConfiguration {
//             config,
//             records,
//         }
//     }
// }

// 自定义反序列化函数
fn deserialize_bit_index<'de, D>(deserializer: D) -> Result<BitIndex, D::Error>
    where
        D: serde::Deserializer<'de>,
{
    use serde::de::Error;
    let s = String::deserialize(deserializer)?;
    s.parse().map_err(Error::custom)
}

// 定义 Config 结构体
#[derive(Debug, Deserialize, Clone)]
pub struct Config {
    pub com: String,
    pub com_index: u8,
    pub device_id: u8,
    pub data_len: u8,
    pub pre: String,
}

// 函数 parse_csv，用于解析 CSV 文件
pub fn parse_csv<P: AsRef<Path>>(file_path: P) -> Result<(Config, Vec<Record>), Box<dyn Error>> {
    let file = File::open(file_path)?;
    let mut rdr = ReaderBuilder::new()
        .delimiter(b',')
        .has_headers(false)
        .from_reader(file);

    let mut headers = Vec::new();
    for _ in 0..5 {
        if let Some(result) = rdr.records().next() {
            let record = result?;
            headers.push(record.get(1).unwrap_or_default().to_string());
        }
    }

    let config = Config {
        com: headers.get(0).cloned().unwrap_or_default(),
        com_index: headers.get(1).cloned().unwrap_or_default().parse().unwrap_or(0),
        device_id: headers.get(2).cloned().unwrap_or_default().parse().unwrap_or(0),
        data_len: headers.get(3).cloned().unwrap_or_default().parse().unwrap_or(0),
        pre: headers.get(4).cloned().unwrap_or_default(),
    };


    // 跳过配置后的行
    rdr.records().next();
    let mut records = Vec::new();
    for result in rdr.deserialize() {
        let mut record: Record = result?;
        // 拼接前缀到 kks 字段
        record.kks = format!("{}{}", config.pre, record.kks);
        records.push(record);
    }
    Ok((config, records))
}
