use csv::{ReaderBuilder};
use serde::Deserialize;
use std::error::Error;
use std::fs::File;
use std::path::Path;
use std::ops::RangeInclusive;
use std::str::FromStr;

// 定义 BitIndex 枚举
#[derive(Debug, Deserialize)]
pub enum BitIndex {
    Single(u32),
    Range(RangeInclusive<u32>),
}

// 为 BitIndex 实现从字符串的转换
// impl FromStr for BitIndex {
//     type Err = ParseIntError;
//
//     fn from_str(s: &str) -> Result<Self, Self::Err> {
//         if let Some(dot_pos) = s.find('.') {
//             let start = s[..dot_pos].parse::<u32>()?;
//             let end = s[dot_pos + 1..].parse::<u32>()?;
//             Ok(BitIndex::Range(start..=end))
//         } else {
//             s.parse::<u32>().map(BitIndex::Single)
//         }
//     }
// }

impl FromStr for BitIndex {
    type Err = String;  // 使用字符串直接描述错误

    fn from_str(s: &str) -> Result<Self, Self::Err> {
        if let Some(dot_pos) = s.find('.') {
            let start = s[..dot_pos].parse::<u32>()
                .map_err(|e| e.to_string())?;
                //.checked_sub(1)
                //.ok_or("Index underflow, input should be 1 or greater")?;
            let end = s[dot_pos + 1..].parse::<u32>()
                .map_err(|e| e.to_string())?;
                //.checked_sub(1)
                //.ok_or("Index underflow, input should be 1 or greater")?;
            Ok(BitIndex::Range(start..=end))
        } else {
            let index = s.parse::<u32>()
                .map_err(|e| e.to_string())?;
                //.checked_sub(1)
                //.ok_or("Index underflow, input should be 1 or greater")?;
            Ok(BitIndex::Single(index))
        }
    }
}

// 定义一个枚举来表示数据的大小端模式
enum Endian {
    Big,
    Little,
}



// 定义 Record 结构体，使用 BitIndex 枚举
#[derive(Debug, Deserialize)]
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
#[derive(Debug, Deserialize)]
pub struct Config {
    pub com: String,
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
    for _ in 0..4 {
        if let Some(result) = rdr.records().next() {
            let record = result?;
            headers.push(record.get(1).unwrap_or_default().to_string());
        }
    }

    let config = Config {
        com: headers.get(0).cloned().unwrap_or_default(),
        device_id: headers.get(1).cloned().unwrap_or_default().parse().unwrap_or(0),
        data_len: headers.get(2).cloned().unwrap_or_default().parse().unwrap_or(0),
        pre: headers.get(3).cloned().unwrap_or_default(),
    };


    // 跳过配置后的行
    rdr.records().next();
    let mut records = Vec::new();
    for result in rdr.deserialize() {
        let record: Record = result?;
        records.push(record);
    }
    Ok((config, records))
}