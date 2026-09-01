//! 通信模块/PLC 版本管理的稳定领域类型。

use serde::{Deserialize, Serialize};

/// 人工配置的物理通信模块类型。
#[derive(Debug, Clone, Copy, PartialEq, Eq, Deserialize, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum ModuleType {
    Unknown,
    Domestic,
    LegacyRussian,
}

impl Default for ModuleType {
    fn default() -> Self {
        Self::Unknown
    }
}

impl ModuleType {
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Unknown => "unknown",
            Self::Domestic => "domestic",
            Self::LegacyRussian => "legacy_russian",
        }
    }
}

impl std::str::FromStr for ModuleType {
    type Err = &'static str;

    fn from_str(value: &str) -> Result<Self, Self::Err> {
        match value {
            "unknown" => Ok(Self::Unknown),
            "domestic" => Ok(Self::Domestic),
            "legacy_russian" => Ok(Self::LegacyRussian),
            _ => Err("module_type must be unknown, domestic, or legacy_russian"),
        }
    }
}

/// 可持久化、可直接平铺到 Operations device JSON 的版本快照。
#[derive(Debug, Clone, PartialEq, Eq, Deserialize, Serialize)]
#[serde(default, deny_unknown_fields)]
pub struct StationVersionRecord {
    pub configured_module_type: ModuleType,
    pub detected_module_type: ModuleType,
    pub type_mismatch: bool,
    pub communication_module_version: String,
    pub communication_module_version_raw: Option<u16>,
    pub plc_version: String,
    pub plc_version_raw: Option<u16>,
    /// 大写、无空格的四字节十六进制载荷。
    pub version_payload: String,
    pub version_state: String,
    pub last_version_attempt_at: Option<String>,
    pub last_version_success_at: Option<String>,
    pub last_version_error: String,
    pub consecutive_failures: u32,
    pub stale: bool,
}

impl Default for StationVersionRecord {
    fn default() -> Self {
        Self {
            configured_module_type: ModuleType::Unknown,
            detected_module_type: ModuleType::Unknown,
            type_mismatch: false,
            communication_module_version: String::new(),
            communication_module_version_raw: None,
            plc_version: String::new(),
            plc_version_raw: None,
            version_payload: String::new(),
            version_state: "never_queried".into(),
            last_version_attempt_at: None,
            last_version_success_at: None,
            last_version_error: String::new(),
            consecutive_failures: 0,
            stale: false,
        }
    }
}

impl StationVersionRecord {
    pub fn has_last_version(&self) -> bool {
        // A strictly validated all-FF legacy marker is also a successful station
        // identification even though it has no numeric version fields.  Keep that
        // successful observation usable (and mark it stale) if a later query fails.
        self.last_version_success_at.is_some()
            || self.communication_module_version_raw.is_some()
            || self.plc_version_raw.is_some()
    }

    pub fn reset_for_configured_type(&mut self, configured: ModuleType) {
        *self = Self {
            configured_module_type: configured,
            version_state: match configured {
                ModuleType::LegacyRussian => "unsupported".into(),
                ModuleType::Unknown | ModuleType::Domestic => "never_queried".into(),
            },
            ..Self::default()
        };
    }
}

pub fn display_version(raw: u16) -> String {
    format!("V{}.{}", raw >> 8, raw & 0x00FF)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn version_display_uses_big_endian_major_minor_bytes() {
        assert_eq!(display_version(0x0104), "V1.4");
        assert_eq!(display_version(0x0005), "V0.5");
    }
}
