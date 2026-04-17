//! 跨多次设备广播维护变量累加表，并按需执行公式的运行器。
//!
//! 将原先埋在 OPC UA 闭包里的 `global_vars` HashMap 提出来，使公式求值
//! 过程脱离 OPC UA 依赖，从而可独立单元测试。

use std::collections::HashMap;
use std::sync::Arc;

use crate::calc_engine::engine::CalcEngine;
use crate::model::{DeviceStatus, Value};

/// 持有 [`CalcEngine`] 与全局变量累加器的运行器。
///
/// 每次收到 [`DeviceStatus`] 时：
/// 1. 将该状态中的 `(kks, value)` 合并进累加表（后写覆盖先写）；
/// 2. 用累加表执行所有已编译公式，返回 `(kks_calc, Value)` 列表。
///
/// 累加表在 `CalcRunner` 生命周期内持续有效，使跨设备 / 跨串口的公式
/// （例如 `total = deviceA.x + deviceB.y`）可以引用不同时间点到达的值。
pub struct CalcRunner {
    engine: Arc<CalcEngine>,
    global_vars: HashMap<String, f64>,
}

impl CalcRunner {
    pub fn new(engine: Arc<CalcEngine>) -> Self {
        Self {
            engine,
            global_vars: HashMap::new(),
        }
    }

    /// 处理一次设备状态更新，返回本轮所有公式的计算结果。
    ///
    /// 空引擎情况下返回空 `Vec`，不进行任何累加，避免无谓分配。
    pub fn on_update(&mut self, status: &DeviceStatus) -> Vec<(String, Value)> {
        if self.engine.is_empty() {
            return Vec::new();
        }
        for (kks, val) in &status.value {
            self.global_vars.insert(kks.clone(), val.to_f64());
        }
        self.engine.run_cycle(&self.global_vars)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::calc_engine::config::CalcRule;

    fn make_rule(kks: &str, dtype: &str, expr: &str) -> CalcRule {
        CalcRule {
            kks_calc: kks.to_string(),
            data_type: dtype.to_string(),
            expression: expr.to_string(),
            max_limit: f64::MAX,
            min_limit: f64::MIN,
            default_value: 0.0,
        }
    }

    fn status(com: &str, device_id: u8, pairs: &[(&str, Value)]) -> DeviceStatus {
        DeviceStatus {
            id: 1,
            device_id,
            com: com.to_string(),
            com_status: true,
            device_status: true,
            value: pairs.iter().map(|(k, v)| (k.to_string(), v.clone())).collect(),
            raw_data: vec![],
        }
    }

    #[test]
    fn on_update_computes_simple_formula() {
        let engine = CalcEngine::new(vec![make_rule("out", "float", "a + 1")]).unwrap();
        let mut runner = CalcRunner::new(Arc::new(engine));

        let results = runner.on_update(&status("COM1", 1, &[("a", Value::Float(5.0))]));

        assert_eq!(results.len(), 1);
        assert_eq!(results[0], ("out".to_string(), Value::Float(6.0)));
    }

    #[test]
    fn empty_engine_returns_empty_no_accumulation() {
        let engine = CalcEngine::new(vec![]).unwrap();
        let mut runner = CalcRunner::new(Arc::new(engine));
        assert!(runner.on_update(&status("COM1", 1, &[("a", Value::UInt(10))])).is_empty());
    }

    #[test]
    fn cross_device_formula_accumulates_vars() {
        // total = a + b，a 和 b 来自不同设备的两次广播
        let engine = CalcEngine::new(vec![make_rule("total", "uint", "a + b")]).unwrap();
        let mut runner = CalcRunner::new(Arc::new(engine));

        // 第 1 次只带 a，b 缺失 → 公式 Fail-Fast 回退到 default_value(0)
        let r1 = runner.on_update(&status("COM1", 1, &[("a", Value::UInt(3))]));
        assert_eq!(r1[0].1, Value::UInt(0));

        // 第 2 次带 b，a 仍在累加表里 → total = 3 + 7 = 10
        let r2 = runner.on_update(&status("COM2", 2, &[("b", Value::UInt(7))]));
        assert_eq!(r2[0].1, Value::UInt(10));
    }

    #[test]
    fn later_update_overrides_earlier_same_kks() {
        let engine = CalcEngine::new(vec![make_rule("out", "uint", "a + 1")]).unwrap();
        let mut runner = CalcRunner::new(Arc::new(engine));

        runner.on_update(&status("COM1", 1, &[("a", Value::UInt(1))]));
        let r = runner.on_update(&status("COM1", 1, &[("a", Value::UInt(100))]));
        assert_eq!(r[0].1, Value::UInt(101));
    }
}
