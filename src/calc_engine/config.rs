//! 计算规则 TOML 配置文件加载模块。

use std::fmt;
use std::path::Path;

use log::info;
use serde::Deserialize;

// ============================================================
//  配置结构
// ============================================================

/// 计算规则配置文件顶层结构。
#[derive(Deserialize, Debug)]
pub struct CalcConfig {
    /// 计算规则列表。
    #[serde(default)]
    pub rules: Vec<CalcRule>,
}

/// 单条计算规则定义。
#[derive(Deserialize, Debug, Clone)]
pub struct CalcRule {
    /// 输出变量名（OPC UA BrowseName，全局唯一）。
    pub kks_calc: String,
    /// 输出数据类型：`"uint"` / `"bool"` / `"float"`。
    pub data_type: String,
    /// 表达式字符串。
    pub expression: String,
    /// 结果上限（超出则 clamp）。
    #[serde(default = "default_max")]
    pub max_limit: f64,
    /// 结果下限。
    #[serde(default = "default_min")]
    pub min_limit: f64,
    /// 异常回退默认值。
    #[serde(default)]
    pub default_value: f64,
}

fn default_max() -> f64 {
    f64::MAX
}

fn default_min() -> f64 {
    f64::MIN
}

impl fmt::Display for CalcRule {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(
            f,
            "CalcRule({} [{}] = \"{}\")",
            self.kks_calc, self.data_type, self.expression
        )
    }
}

// ============================================================
//  加载函数
// ============================================================

/// 从指定路径加载计算规则。
///
/// 文件不存在时静默返回空规则列表（info 日志）。
/// TOML 格式不正确时返回错误。
pub fn load_calc_config(path: &Path) -> Result<CalcConfig, String> {
    if !path.exists() {
        info!("计算规则文件不存在: {:?}，跳过加载", path);
        return Ok(CalcConfig { rules: vec![] });
    }

    let content =
        std::fs::read_to_string(path).map_err(|e| format!("读取计算规则文件失败: {}", e))?;

    let config: CalcConfig =
        toml::from_str(&content).map_err(|e| format!("解析计算规则 TOML 失败: {}", e))?;

    info!("已加载 {} 条计算规则", config.rules.len());
    Ok(config)
}

// ============================================================
//  单元测试
// ============================================================

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn deserialize_calc_config() {
        let toml_str = r#"
[[rules]]
kks_calc = "rate_usv"
data_type = "float"
expression = "rate_msv * 1000"
max_limit = 999999.0
min_limit = 0.0
default_value = 0.0

[[rules]]
kks_calc = "alarm"
data_type = "bool"
expression = "(status_A == 1) && (status_B == 0)"
default_value = 0.0
"#;
        let config: CalcConfig = toml::from_str(toml_str).unwrap();
        assert_eq!(config.rules.len(), 2);
        assert_eq!(config.rules[0].kks_calc, "rate_usv");
        assert_eq!(config.rules[0].data_type, "float");
        assert_eq!(config.rules[0].max_limit, 999999.0);
        assert_eq!(config.rules[1].kks_calc, "alarm");
        assert_eq!(config.rules[1].data_type, "bool");
    }

    #[test]
    fn deserialize_defaults() {
        let toml_str = r#"
[[rules]]
kks_calc = "test"
data_type = "uint"
expression = "a + b"
"#;
        let config: CalcConfig = toml::from_str(toml_str).unwrap();
        assert_eq!(config.rules[0].max_limit, f64::MAX);
        assert_eq!(config.rules[0].min_limit, f64::MIN);
        assert_eq!(config.rules[0].default_value, 0.0);
    }

    #[test]
    fn deserialize_empty() {
        let config: CalcConfig = toml::from_str("").unwrap();
        assert!(config.rules.is_empty());
    }

    #[test]
    fn load_nonexistent_file() {
        let result = load_calc_config(Path::new("nonexistent_file.toml"));
        assert!(result.is_ok());
        assert!(result.unwrap().rules.is_empty());
    }
}
