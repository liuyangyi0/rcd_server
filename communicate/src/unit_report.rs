//! Unit-to-unit snapshots described by 01BAOWEIFENXI / 91BAOWEIFENXI.
//! Offsets refer to the uncompressed payload (worksheet row 8 = offset 0).
//! Opaque fields remain hexadecimal: the documents do not define their types.
use crate::config::{Config, ConfigError, PointDefinition, TaskConfig};
use crate::points::{DataType, PointUpdate, Quality, Value};
use crate::protocol::{DeviceProtocol, ProtocolError, Response};

const UNIT1_BUILDINGS: &[&str] = &[
    "11UBE", "11UBF", "11UBN", "11UBR", "11UBS", "11UBT", "11UBU", "11UBZ", "11UCB", "11UJA",
    "11UJE", "11UJG", "11UJZ", "11UKA", "11UKC", "11UKD", "11UKT", "11UMA", "11UMZ", "11UQA",
    "11UQB", "11USG", "11USP", "11USZ", "11UYZ", "12UBE", "12UBF", "12UBT", "12UBZ", "13UBF",
    "13UBZ", "14UBZ", "1JME34", "1JMF10", "1JMF34",
];
const UNIT9_BUILDINGS: &[&str] = &[
    "91UBH", "91UGM", "91UGQ", "91UKT", "91UKZ", "91UYA", "91UZD", "11UGS", "12UGS", "13UGS",
    "91UYZ",
];

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Layout {
    Unit1To9,
    Unit9To1,
}

impl Layout {
    /// One original document variable per field. Unit identity is fixed by
    /// layout; Fire inputs use the same enum published by the receiver.
    fn transmit_fields(self) -> Vec<Field> {
        self.fields()
            .into_iter()
            .filter(|f| f.name != "Header.nBlock.z")
            .collect()
    }
    pub fn transmit_definitions(self, _port: &str) -> Vec<PointDefinition> {
        self.transmit_fields()
            .into_iter()
            .map(|f| PointDefinition {
                name: format!("DATA.{}", f.name),
                task_id: _port.into(),
                data_type: if matches!(f.kind, Kind::Unsigned | Kind::Fire) {
                    DataType::UInt32
                } else {
                    DataType::String
                },
                description: f.description("发送"),
            })
            .collect()
    }
    pub fn empty_transmit_payload(self) -> Vec<u8> {
        let mut bytes = vec![0; self.payload_len()];
        bytes[..2].copy_from_slice(&self.block().to_le_bytes());
        bytes
    }
    pub fn write_transmit_field(
        self,
        _port: &str,
        name: &str,
        value: &Value,
        payload: &mut [u8],
    ) -> Result<(), ProtocolError> {
        self.validate_payload(payload)?;
        let field = self
            .transmit_fields()
            .into_iter()
            .find(|f| name == format!("DATA.{}", f.name))
            .ok_or_else(|| ProtocolError("unknown transmit input".into()))?;
        let bytes = match (&field.kind, value) {
            (Kind::Unsigned, Value::UInt32(v)) if *v <= 255 => vec![*v as u8],
            (Kind::Fire, Value::UInt32(v)) => fire_bytes(*v)
                .ok_or_else(|| {
                    ProtocolError("Fire 状态必须为 1..8；0 是接收未知状态，不能发送".into())
                })?
                .to_vec(),
            (Kind::Hex, Value::String(v)) if v.len() == field.len * 2 && v.is_ascii() => (0..v
                .len())
                .step_by(2)
                .map(|i| {
                    u8::from_str_radix(&v[i..i + 2], 16)
                        .map_err(|_| ProtocolError("invalid hexadecimal field".into()))
                })
                .collect::<Result<Vec<_>, _>>()?,
            _ => {
                return Err(ProtocolError(
                    "input type, range or exact field length mismatch".into(),
                ))
            }
        };
        payload[field.offset..field.offset + field.len].copy_from_slice(&bytes);
        Ok(())
    }
    pub fn from_protocol(name: &str) -> Option<Self> {
        match name {
            "unit1-to9-v1" => Some(Self::Unit1To9),
            "unit9-to1-v1" => Some(Self::Unit9To1),
            _ => None,
        }
    }
    pub fn payload_len(self) -> usize {
        match self {
            Self::Unit1To9 => 1156,
            Self::Unit9To1 => 105,
        }
    }
    fn block(self) -> u16 {
        match self {
            Self::Unit1To9 => 1,
            Self::Unit9To1 => 9,
        }
    }
    fn fields(self) -> Vec<Field> {
        let mut fields = vec![Field::new("Header.nBlock.z", 0, 2, Kind::Unsigned)];
        let (buildings, offset) = match self {
            Self::Unit1To9 => {
                fields.extend([
                    Field::new("Header.detectorTemp3", 2, 1, Kind::Unsigned),
                    Field::new("Header.detectorTemp6", 3, 1, Kind::Unsigned),
                    Field::new("AKONs.1CYE18GH204.HW", 4, 969, Kind::Hex),
                    Field::new("PumpStationCtrl", 973, 8, Kind::Hex),
                ]);
                (UNIT1_BUILDINGS, 981)
            }
            Self::Unit9To1 => {
                fields.extend([
                    Field::new("AES.SGC85.State", 2, 5, Kind::Hex),
                    Field::new("AES.GMM1", 7, 35, Kind::Hex),
                    Field::new("PumpStationCtrl[0]", 42, 8, Kind::Hex),
                ]);
                (UNIT9_BUILDINGS, 50)
            }
        };
        for (i, building) in buildings.iter().enumerate() {
            let base = format!("Buildings.{building}.Fire");
            fields.push(Field::new(&base, offset + i * 5, 5, Kind::Fire));
        }
        fields
    }
    pub fn definitions(self, task_id: &str) -> Vec<PointDefinition> {
        self.fields()
            .into_iter()
            .map(|f| PointDefinition {
                name: format!("DATA.{}", f.name),
                task_id: task_id.into(),
                data_type: if matches!(f.kind, Kind::Unsigned | Kind::Fire) {
                    DataType::UInt32
                } else {
                    DataType::String
                },
                description: f.description("接收"),
            })
            .collect()
    }
    /// A full known layout is required. Prefix demonstration frames in the
    /// workbooks are NOT complete business snapshots and cannot refresh Good.
    pub fn validate_payload(self, payload: &[u8]) -> Result<(), ProtocolError> {
        if payload.len() != self.payload_len() {
            return Err(ProtocolError(format!(
                "expected {} payload bytes, got {}",
                self.payload_len(),
                payload.len()
            )));
        }
        if u16::from_le_bytes([payload[0], payload[1]]) != self.block() {
            return Err(ProtocolError("unexpected unit block identity".into()));
        }
        Ok(())
    }
    /// Encode a complete caller-owned snapshot. No captured sample is used as
    /// a live outgoing default, and no hardware command is inferred here.
    pub fn encode(
        self,
        payload: &[u8],
        limits: crate::frame::Limits,
    ) -> Result<Vec<u8>, ProtocolError> {
        self.validate_payload(payload)?;
        crate::frame::encode(payload, limits).map_err(|e| ProtocolError(e.to_string()))
    }
    pub fn decode(self, _task_id: &str, payload: &[u8]) -> Result<Vec<PointUpdate>, ProtocolError> {
        self.validate_payload(payload)?;
        Ok(self
            .fields()
            .into_iter()
            .map(|f| {
                let bytes = &payload[f.offset..f.offset + f.len];
                let value = match f.kind {
                    Kind::Unsigned => Value::UInt32(if bytes.len() == 2 {
                        u16::from_le_bytes([bytes[0], bytes[1]]) as u32
                    } else {
                        bytes[0] as u32
                    }),
                    Kind::Hex => Value::String(hex(bytes)),
                    Kind::Fire => Value::UInt32(fire_code(bytes)),
                };
                PointUpdate {
                    name: format!("DATA.{}", f.name),
                    value,
                    quality: Quality::Good,
                    error: String::new(),
                }
            })
            .collect())
    }
}

pub fn hex(bytes: &[u8]) -> String {
    use std::fmt::Write;
    let mut output = String::with_capacity(bytes.len() * 2);
    for b in bytes {
        let _ = write!(output, "{b:02X}");
    }
    output
}

/// Public OPC values; the wire still uses exactly the original five bytes.
/// GZ/KL/DL/M workbook examples share bytes and cannot identify a unique cause.
pub const FIRE_DESCRIPTION: &str = "UInt32：0=未知（仅接收）；1=正常；2=红色报警；3=红色报警闪；4=黄色报警；5=黄色报警闪；6=故障报警；7=故障报警闪；8=闭锁。故障细因不能由现有报文区分。发送只接受1..8。";

pub const FIRE_STATES: &[(u32, &str, [u8; 5])] = &[
    (1, "正常", [0x0a, 0x85, 0, 6, 0]),
    (2, "红色报警", [1, 0x88, 0, 7, 0]),
    (3, "红色报警闪", [1, 0xc8, 0, 7, 0]),
    (4, "黄色报警", [5, 0x86, 0, 0x0c, 0]),
    (5, "黄色报警闪", [5, 0xa6, 0, 0x0c, 0]),
    (6, "故障报警（细分原因未定义）", [6, 0x82, 0, 0x12, 0]),
    (7, "故障报警闪（细分原因未定义）", [6, 0xc2, 0, 0x12, 0]),
    (8, "闭锁", [9, 0x85, 0, 4, 0]),
];

pub fn fire_code(bytes: &[u8]) -> u32 {
    FIRE_STATES
        .iter()
        .find(|(_, _, raw)| raw == bytes)
        .map_or(0, |(code, _, _)| *code)
}

pub fn fire_bytes(code: u32) -> Option<[u8; 5]> {
    FIRE_STATES
        .iter()
        .find(|(value, _, _)| *value == code)
        .map(|(_, _, raw)| *raw)
}

pub fn fire_state(bytes: &[u8]) -> &'static str {
    FIRE_STATES
        .iter()
        .find(|(_, _, raw)| raw == bytes)
        .map_or("未定义状态", |(_, label, _)| *label)
}

enum Kind {
    Unsigned,
    Hex,
    Fire,
}
struct Field {
    name: String,
    offset: usize,
    len: usize,
    kind: Kind,
}
impl Field {
    fn description(&self, direction: &str) -> String {
        let meaning = match self.kind {
            Kind::Fire => FIRE_DESCRIPTION,
            Kind::Hex => "String：原始字节的定长十六进制字符串，不加空格",
            Kind::Unsigned => "UInt32：协议原始整数；机组身份由通信方向固定",
        };
        format!(
            "{direction} DATA.{}；载荷偏移 {}，长度 {}；{meaning}",
            self.name, self.offset, self.len
        )
    }
    fn new(name: &str, offset: usize, len: usize, kind: Kind) -> Self {
        Self {
            name: name.into(),
            offset,
            len,
            kind,
        }
    }
}

pub struct ReportProtocol(pub Layout);
impl DeviceProtocol for ReportProtocol {
    fn unsolicited(&self) -> bool {
        true
    }
    fn request(&mut self, _: &TaskConfig) -> Result<Vec<u8>, ProtocolError> {
        Err(ProtocolError(
            "unit synchronization has no query request".into(),
        ))
    }
    fn response(&mut self, task: &TaskConfig, payload: &[u8]) -> Result<Response, ProtocolError> {
        if payload.len() < 2 {
            return Err(ProtocolError("missing unit identity".into()));
        }
        if u16::from_le_bytes([payload[0], payload[1]]) != self.0.block() {
            return Ok(Response::Ignore);
        }
        self.0.decode(&task.id, payload).map(Response::Matched)
    }
}

pub fn validate_points(config: &Config, points: &[PointDefinition]) -> Result<(), ConfigError> {
    let mut names: std::collections::HashSet<_> = points.iter().map(|p| p.name.clone()).collect();
    for port in &config.ports {
        for task in &port.tasks {
            let Some(layout) = Layout::from_protocol(&task.protocol) else {
                continue;
            };
            let expected = layout.definitions(&task.id);
            let actual: Vec<_> = points.iter().filter(|p| p.task_id == task.id).collect();
            if expected.len() != actual.len()
                || expected.iter().any(|e| {
                    !actual
                        .iter()
                        .any(|a| a.name == e.name && a.data_type == e.data_type)
                })
            {
                return Err(ConfigError::Invalid(format!(
                    "task {}: point map must match documented layout {} ({} points)",
                    task.id,
                    task.protocol,
                    expected.len()
                )));
            }
        }
        if let Some(layout) = port
            .synchronization
            .as_ref()
            .and_then(|s| Layout::from_protocol(&s.transmit_protocol))
        {
            for field in layout.transmit_definitions(&port.id) {
                if !names.insert(field.name) {
                    return Err(ConfigError::Invalid(
                        "原始变量名重复；不同方向/端口不得覆盖同一OPC节点".into(),
                    ));
                }
            }
            for name in [
                "TransmitSnapshot",
                "TransmitStatus",
                "InputRefreshIntervalMs",
                "InputTimeoutMs",
            ] {
                if !names.insert(name.into()) {
                    return Err(ConfigError::Invalid(
                        "同步辅助节点重复；请将不同同步端口部署为独立实例".into(),
                    ));
                }
            }
        }
    }
    Ok(())
}
