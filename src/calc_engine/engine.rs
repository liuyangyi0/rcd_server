//! 计算引擎运行器。
//!
//! 负责：
//! 1. 启动期编译：将 TOML 配置中的表达式字符串预编译为 AST。
//! 2. DAG 拓扑排序：若公式 A 引用了公式 B 的输出，确保 B 先于 A 求值。
//! 3. 运行期求值：按拓扑顺序依次求值，Fail-Fast 短路 + 默认值回退。
//! 4. 后处理：NaN/Infinity 安全检查、上下限 Clamping、数据类型强转。

use std::collections::{HashMap, HashSet, VecDeque};

use log::{error, info, warn};

use super::config::{normalize_data_type, CalcDataType, CalcRule};
use super::evaluator::{evaluate, EvalError, EvalValue};
use super::parser::{extract_variables, parse, Expr};
use super::tokenizer::tokenize;
use crate::model::Value;

// ============================================================
//  编译后的规则
// ============================================================

/// 预编译的计算规则：包含 AST 和提取的变量列表。
struct CompiledRule {
    /// 输出变量名（OPC UA BrowseName）。
    kks_calc: String,
    /// 输出数据类型。
    data_type: CalcDataType,
    /// 原始表达式字符串（用于日志）。
    expression: String,
    /// 预编译的 AST。
    ast: Expr,
    /// 表达式引用的变量名列表（去重、排序）。
    variables: Vec<String>,
    /// 结果上限。
    max_limit: f64,
    /// 结果下限。
    min_limit: f64,
    /// 异常时的回退默认值。
    default_value: f64,
}

// ============================================================
//  计算引擎
// ============================================================

/// 计算引擎：持有预编译、拓扑排序后的规则列表。
///
/// 构造后不可变，线程安全（可用 `Arc` 共享）。
pub struct CalcEngine {
    /// 按拓扑顺序排列的编译后规则。
    rules: Vec<CompiledRule>,
}

impl CalcEngine {
    /// 从配置规则列表构建引擎。
    ///
    /// 1. 逐条编译表达式（tokenize → parse → extract_variables）。
    /// 2. 编译失败（含自引用）的规则会记录错误日志并跳过。
    /// 3. 对成功编译的规则进行 DAG 拓扑排序。
    /// 4. 参与循环依赖或依赖循环节点的规则会被跳过（warn 日志），其余规则正常加载。
    /// 5. 始终返回 `Ok`；`Err` 签名保留以备未来其他失败场景使用。
    pub fn new(config_rules: Vec<CalcRule>) -> Result<Self, String> {
        if config_rules.is_empty() {
            return Ok(CalcEngine { rules: Vec::new() });
        }

        // ---- 编译阶段 ----
        let mut compiled = Vec::with_capacity(config_rules.len());

        for rule in config_rules {
            match Self::compile_rule(&rule) {
                Ok(cr) => {
                    if compiled
                        .iter()
                        .any(|existing: &CompiledRule| existing.kks_calc == cr.kks_calc)
                    {
                        error!(
                            "[CalcEngine] 编译失败，跳过重复输出变量 '{}': 已存在同名 kks_calc",
                            cr.kks_calc
                        );
                        continue;
                    }
                    info!(
                        "[CalcEngine] 编译成功: {} = \"{}\" (依赖: {:?})",
                        cr.kks_calc, cr.expression, cr.variables
                    );
                    compiled.push(cr);
                }
                Err(e) => {
                    error!("[CalcEngine] 编译失败，跳过规则 '{}': {}", rule.kks_calc, e);
                }
            }
        }

        if compiled.is_empty() {
            info!("[CalcEngine] 无有效规则，引擎为空");
            return Ok(CalcEngine { rules: Vec::new() });
        }

        // ---- 拓扑排序 ----
        let sorted = Self::topological_sort(compiled)?;

        info!(
            "[CalcEngine] 引擎初始化完成，共 {} 条规则，求值顺序: [{}]",
            sorted.len(),
            sorted
                .iter()
                .map(|r| r.kks_calc.as_str())
                .collect::<Vec<_>>()
                .join(" → ")
        );

        Ok(CalcEngine { rules: sorted })
    }

    /// 返回所有规则的输出变量名。
    pub fn output_names(&self) -> Vec<&str> {
        self.rules.iter().map(|r| r.kks_calc.as_str()).collect()
    }

    /// 返回所有规则需要的基础变量名（排除引擎自身产出的变量）。
    pub fn required_base_variables(&self) -> Vec<String> {
        let outputs: HashSet<&str> = self.rules.iter().map(|r| r.kks_calc.as_str()).collect();
        let mut base_vars: Vec<String> = self
            .rules
            .iter()
            .flat_map(|r| r.variables.iter())
            .filter(|v| !outputs.contains(v.as_str()))
            .cloned()
            .collect();
        base_vars.sort();
        base_vars.dedup();
        base_vars
    }

    /// 返回所有规则的 `(kks_calc, data_type)` 对。
    ///
    /// 供 OPC UA 节点创建时确定变量类型。
    pub fn output_definitions(&self) -> Vec<(&str, &str)> {
        self.rules
            .iter()
            .map(|r| (r.kks_calc.as_str(), r.data_type.as_str()))
            .collect()
    }

    /// 是否为空引擎（无规则）。
    pub fn is_empty(&self) -> bool {
        self.rules.is_empty()
    }

    // ============================================================
    //  核心求值
    // ============================================================

    /// 执行一轮完整的公式求值。
    ///
    /// - `base_variables`：本轮从串口采集到的全部基础 KKS 值。
    /// - 返回值：每条规则的计算结果 `(kks_calc, Value)`。
    ///
    /// 对每条规则：
    /// 1. 调用 `evaluate`，若变量缺失或除零则 Fail-Fast 短路。
    /// 2. 捕获错误后记录 `warn!` 日志，输出 `default_value`。
    /// 3. 对合法结果进行 NaN/Infinity 检测。
    /// 4. 执行 `[min_limit, max_limit]` 区间钳制。
    /// 5. 按 `data_type` 转换为 `Value::UInt` / `Value::Bool` / `Value::Float`。
    /// 6. 将结果写入变量表，供下游公式引用。
    pub fn run_cycle(&self, base_variables: &HashMap<String, EvalValue>) -> Vec<(String, Value)> {
        let mut variables = base_variables.clone();
        let mut results = Vec::with_capacity(self.rules.len());

        for rule in &self.rules {
            // ---- 求值 ----
            let raw_value = match evaluate(&rule.ast, &variables) {
                Ok(val) => val,
                Err(e) => {
                    match &e {
                        EvalError::MissingVariable(var) => {
                            warn!(
                                "[CalcEngine] 公式 '{}' (\"{}\") 求值失败: 变量 '{}' 缺失，输出默认值 {}",
                                rule.kks_calc, rule.expression, var, rule.default_value
                            );
                        }
                        EvalError::DivisionByZero => {
                            warn!(
                                "[CalcEngine] 公式 '{}' (\"{}\") 求值失败: 除以零，输出默认值 {}",
                                rule.kks_calc, rule.expression, rule.default_value
                            );
                        }
                        EvalError::TypeError(msg) => {
                            warn!(
                                "[CalcEngine] 公式 '{}' (\"{}\") 求值失败: 类型错误: {}，输出默认值 {}",
                                rule.kks_calc, rule.expression, msg, rule.default_value
                            );
                        }
                    }
                    EvalValue::Float(rule.default_value)
                }
            };

            // ---- NaN / Infinity 安全网 ----
            let safe_value = if raw_value.is_invalid_float() {
                warn!(
                    "[CalcEngine] 公式 '{}' 产生非法浮点值 ({})，输出默认值 {}",
                    rule.kks_calc,
                    raw_value.as_f64(),
                    rule.default_value
                );
                EvalValue::Float(rule.default_value)
            } else {
                raw_value
            };

            // ---- 上下限钳制 ----
            let clamped = safe_value.as_f64().clamp(rule.min_limit, rule.max_limit);

            // ---- 类型转换 ----
            let typed_value = match rule.data_type {
                CalcDataType::UInt => {
                    if clamped > u32::MAX as f64 {
                        warn!(
                            "[CalcEngine] 公式 '{}' 结果 {} 超出 u32::MAX，饱和到 {}",
                            rule.kks_calc,
                            clamped,
                            u32::MAX
                        );
                    }
                    Value::UInt(clamped.max(0.0) as u32)
                }
                CalcDataType::Bool => Value::Bool(match &safe_value {
                    EvalValue::Bool(b) if clamped == safe_value.as_f64() => *b,
                    _ => clamped != 0.0,
                }),
                CalcDataType::Float => {
                    let f32_max = f32::MAX as f64;
                    let value = if clamped > f32_max {
                        warn!(
                            "[CalcEngine] 公式 '{}' 结果 {} 超出 f32::MAX，饱和到 {}",
                            rule.kks_calc,
                            clamped,
                            f32::MAX
                        );
                        f32::MAX
                    } else if clamped < -f32_max {
                        warn!(
                            "[CalcEngine] 公式 '{}' 结果 {} 小于 -f32::MAX，饱和到 {}",
                            rule.kks_calc,
                            clamped,
                            -f32::MAX
                        );
                        -f32::MAX
                    } else {
                        clamped as f32
                    };
                    Value::Float(value)
                }
                CalcDataType::Double => Value::Double(clamped),
            };

            // ---- 写回变量表，供下游公式引用 ----
            variables.insert(
                rule.kks_calc.clone(),
                EvalValue::from_model_value(&typed_value),
            );

            results.push((rule.kks_calc.clone(), typed_value));
        }

        results
    }

    // ============================================================
    //  内部方法
    // ============================================================

    /// 编译单条规则：tokenize → parse → extract_variables。
    ///
    /// 同时检测自引用（kks_calc 出现在自身表达式的变量列表里）。
    fn compile_rule(rule: &CalcRule) -> Result<CompiledRule, String> {
        let data_type = normalize_data_type(&rule.data_type).ok_or_else(|| {
            format!(
                "未知 data_type '{}': 仅支持 uint/u32/uint32、bool/boolean、float/f32、double/f64",
                rule.data_type
            )
        })?;

        if !rule.max_limit.is_finite()
            || !rule.min_limit.is_finite()
            || !rule.default_value.is_finite()
        {
            return Err(format!(
                "上下限或默认值必须为有限数: min={}, max={}, default={}",
                rule.min_limit, rule.max_limit, rule.default_value
            ));
        }

        if rule.min_limit > rule.max_limit {
            return Err(format!(
                "min_limit ({}) 不能大于 max_limit ({})",
                rule.min_limit, rule.max_limit
            ));
        }

        let tokens = tokenize(&rule.expression)?;
        let ast = parse(tokens)?;
        let variables = extract_variables(&ast);

        if variables.iter().any(|v| v == &rule.kks_calc) {
            return Err(format!(
                "公式自引用: {} 的表达式 \"{}\" 中引用了自身",
                rule.kks_calc, rule.expression
            ));
        }

        Ok(CompiledRule {
            kks_calc: rule.kks_calc.clone(),
            data_type,
            expression: rule.expression.clone(),
            ast,
            variables,
            max_limit: rule.max_limit,
            min_limit: rule.min_limit,
            default_value: rule.default_value,
        })
    }

    /// DAG 拓扑排序（Kahn 算法）。
    ///
    /// 保证依赖的规则优先求值。若检测到循环依赖，参与循环或依赖循环节点的
    /// 规则会被跳过并记录 warn 日志，其余规则正常返回（服务器不会崩溃）。
    fn topological_sort(rules: Vec<CompiledRule>) -> Result<Vec<CompiledRule>, String> {
        let n = rules.len();

        // 构建名称 → 索引映射
        let name_to_idx: HashMap<&str, usize> = rules
            .iter()
            .enumerate()
            .map(|(i, r)| (r.kks_calc.as_str(), i))
            .collect();

        // 构建邻接表和入度
        let mut adj: Vec<Vec<usize>> = vec![Vec::new(); n];
        let mut in_degree: Vec<usize> = vec![0; n];

        for (i, rule) in rules.iter().enumerate() {
            for var in &rule.variables {
                if let Some(&dep_idx) = name_to_idx.get(var.as_str()) {
                    // rule[i] 依赖 rule[dep_idx]，dep_idx 必须先算
                    adj[dep_idx].push(i);
                    in_degree[i] += 1;
                }
            }
        }

        // Kahn BFS
        let mut queue: VecDeque<usize> = VecDeque::new();
        for (i, degree) in in_degree.iter().enumerate() {
            if *degree == 0 {
                queue.push_back(i);
            }
        }

        let mut order: Vec<usize> = Vec::with_capacity(n);
        while let Some(idx) = queue.pop_front() {
            order.push(idx);
            for &next in &adj[idx] {
                in_degree[next] -= 1;
                if in_degree[next] == 0 {
                    queue.push_back(next);
                }
            }
        }

        if order.len() != n {
            let skipped: Vec<&str> = (0..n)
                .filter(|i| in_degree[*i] > 0)
                .map(|i| rules[i].kks_calc.as_str())
                .collect();
            warn!(
                "[CalcEngine] 检测到循环依赖或依赖循环节点，以下规则被跳过: [{}]",
                skipped.join(", ")
            );
        }

        // 只取 order 中的规则，剩余的（参与循环 / 依赖循环）丢弃
        let mut rules_opt: Vec<Option<CompiledRule>> = rules.into_iter().map(Some).collect();
        let sorted: Vec<CompiledRule> = order
            .into_iter()
            .map(|i| rules_opt[i].take().unwrap())
            .collect();

        Ok(sorted)
    }
}

// ============================================================
//  单元测试
// ============================================================

#[cfg(test)]
mod tests {
    use super::*;
    use crate::calc_engine::config::CalcRule;

    fn make_rule(kks: &str, dtype: &str, expr: &str, default: f64) -> CalcRule {
        CalcRule {
            kks_calc: kks.to_string(),
            data_type: dtype.to_string(),
            expression: expr.to_string(),
            max_limit: f64::MAX,
            min_limit: f64::MIN,
            default_value: default,
        }
    }

    fn vars(pairs: &[(&str, EvalValue)]) -> HashMap<String, EvalValue> {
        pairs
            .iter()
            .map(|(k, v)| (k.to_string(), v.clone()))
            .collect()
    }

    fn int(n: i64) -> EvalValue {
        EvalValue::Int(n)
    }

    fn float(n: f64) -> EvalValue {
        EvalValue::Float(n)
    }

    fn bool_val(v: bool) -> EvalValue {
        EvalValue::Bool(v)
    }

    // ---- 基础求值 ----

    #[test]
    fn engine_simple_eval() {
        let rules = vec![make_rule("rate_usv", "float", "rate_msv * 1000", 0.0)];
        let engine = CalcEngine::new(rules).unwrap();

        let results = engine.run_cycle(&vars(&[("rate_msv", float(0.5))]));
        assert_eq!(results.len(), 1);
        assert_eq!(results[0].0, "rate_usv");
        assert_eq!(results[0].1, Value::Float(500.0));
    }

    // ---- 变量缺失 → 默认值 ----

    #[test]
    fn engine_missing_variable_uses_default() {
        let rules = vec![make_rule("output", "float", "sensor_a + 10", -99.0)];
        let engine = CalcEngine::new(rules).unwrap();

        // sensor_a 不在 base_variables 中
        let results = engine.run_cycle(&HashMap::new());
        assert_eq!(results[0].1, Value::Float(-99.0));
    }

    // ---- 除以零 → 默认值 ----

    #[test]
    fn engine_div_by_zero_uses_default() {
        let rules = vec![make_rule("output", "float", "10 / 0", -1.0)];
        let engine = CalcEngine::new(rules).unwrap();

        let results = engine.run_cycle(&HashMap::new());
        assert_eq!(results[0].1, Value::Float(-1.0));
    }

    // ---- Clamping ----

    #[test]
    fn engine_clamp_max() {
        let mut rule = make_rule("output", "float", "a", 0.0);
        rule.max_limit = 100.0;
        rule.min_limit = 0.0;
        let engine = CalcEngine::new(vec![rule]).unwrap();

        let results = engine.run_cycle(&vars(&[("a", int(999))]));
        assert_eq!(results[0].1, Value::Float(100.0));
    }

    #[test]
    fn engine_clamp_min() {
        let mut rule = make_rule("output", "float", "a", 0.0);
        rule.max_limit = 100.0;
        rule.min_limit = 0.0;
        let engine = CalcEngine::new(vec![rule]).unwrap();

        let results = engine.run_cycle(&vars(&[("a", int(-50))]));
        assert_eq!(results[0].1, Value::Float(0.0));
    }

    // ---- 类型转换 ----

    #[test]
    fn engine_type_uint() {
        let rules = vec![make_rule("output", "uint", "a", 0.0)];
        let engine = CalcEngine::new(rules).unwrap();

        let results = engine.run_cycle(&vars(&[("a", float(42.7))]));
        assert_eq!(results[0].1, Value::UInt(42));
    }

    #[test]
    fn engine_type_bool_true() {
        let rules = vec![make_rule("output", "bool", "a", 0.0)];
        let engine = CalcEngine::new(rules).unwrap();

        let results = engine.run_cycle(&vars(&[("a", bool_val(true))]));
        assert_eq!(results[0].1, Value::Bool(true));
    }

    #[test]
    fn engine_type_bool_false() {
        let rules = vec![make_rule("output", "bool", "a", 0.0)];
        let engine = CalcEngine::new(rules).unwrap();

        let results = engine.run_cycle(&vars(&[("a", bool_val(false))]));
        assert_eq!(results[0].1, Value::Bool(false));
    }

    #[test]
    fn engine_type_uint_alias() {
        let rules = vec![make_rule("output", "uint32", "a", 0.0)];
        let engine = CalcEngine::new(rules).unwrap();

        let results = engine.run_cycle(&vars(&[("a", int(42))]));
        assert_eq!(results[0].1, Value::UInt(42));
    }

    #[test]
    fn engine_type_double_alias() {
        let rules = vec![make_rule("output", "f64", "a / 4", 0.0)];
        let engine = CalcEngine::new(rules).unwrap();

        let results = engine.run_cycle(&vars(&[("a", int(10))]));
        assert_eq!(results[0].1, Value::Double(2.5));
    }

    #[test]
    fn engine_type_float_saturates_above_f32_max() {
        let rules = vec![make_rule("output", "float", "a", 0.0)];
        let engine = CalcEngine::new(rules).unwrap();

        let results = engine.run_cycle(&vars(&[("a", float(f32::MAX as f64 * 2.0))]));
        assert_eq!(results[0].1, Value::Float(f32::MAX));
    }

    #[test]
    fn engine_type_float_saturates_below_negative_f32_max() {
        let rules = vec![make_rule("output", "float", "a", 0.0)];
        let engine = CalcEngine::new(rules).unwrap();

        let results = engine.run_cycle(&vars(&[("a", float(-(f32::MAX as f64) * 2.0))]));
        assert_eq!(results[0].1, Value::Float(-f32::MAX));
    }

    #[test]
    fn engine_if_avoids_missing_branch() {
        let rules = vec![make_rule("output", "uint", "IF(ready == 1, sl3, 4)", 0.0)];
        let engine = CalcEngine::new(rules).unwrap();

        let results = engine.run_cycle(&vars(&[("ready", int(0))]));
        assert_eq!(results[0].1, Value::UInt(4));
    }

    #[test]
    fn engine_boolean_arithmetic_formula_still_works() {
        let rules = vec![make_rule(
            "output",
            "uint",
            "(READY==1)*SL3+(READY!=1)*4",
            0.0,
        )];
        let engine = CalcEngine::new(rules).unwrap();

        let results = engine.run_cycle(&vars(&[("READY", int(1)), ("SL3", int(7))]));
        assert_eq!(results[0].1, Value::UInt(7));

        let results = engine.run_cycle(&vars(&[("READY", int(0)), ("SL3", int(7))]));
        assert_eq!(results[0].1, Value::UInt(4));
    }

    // ---- DAG 拓扑排序 ----

    #[test]
    fn engine_dag_dependency_order() {
        // B = a * 2
        // A = B + 10  （A 依赖 B）
        let rules = vec![
            make_rule("A", "float", "B + 10", 0.0),
            make_rule("B", "float", "a * 2", 0.0),
        ];
        let engine = CalcEngine::new(rules).unwrap();

        // 拓扑排序后 B 应在 A 前面
        assert_eq!(engine.rules[0].kks_calc, "B");
        assert_eq!(engine.rules[1].kks_calc, "A");

        let results = engine.run_cycle(&vars(&[("a", int(5))]));
        // B = 5 * 2 = 10, A = 10 + 10 = 20
        assert_eq!(results.len(), 2);
        let result_map: HashMap<&str, &Value> =
            results.iter().map(|(k, v)| (k.as_str(), v)).collect();
        assert_eq!(result_map["B"], &Value::Float(10.0));
        assert_eq!(result_map["A"], &Value::Float(20.0));
    }

    #[test]
    fn engine_dag_three_level() {
        // C = base * 3
        // B = C + 1
        // A = B * 2
        let rules = vec![
            make_rule("A", "float", "B * 2", 0.0),
            make_rule("B", "float", "C + 1", 0.0),
            make_rule("C", "float", "base * 3", 0.0),
        ];
        let engine = CalcEngine::new(rules).unwrap();

        // 排序后应该是 C → B → A
        assert_eq!(engine.rules[0].kks_calc, "C");
        assert_eq!(engine.rules[1].kks_calc, "B");
        assert_eq!(engine.rules[2].kks_calc, "A");

        let results = engine.run_cycle(&vars(&[("base", int(2))]));
        let result_map: HashMap<&str, &Value> =
            results.iter().map(|(k, v)| (k.as_str(), v)).collect();
        // C = 2*3 = 6, B = 6+1 = 7, A = 7*2 = 14
        assert_eq!(result_map["C"], &Value::Float(6.0));
        assert_eq!(result_map["B"], &Value::Float(7.0));
        assert_eq!(result_map["A"], &Value::Float(14.0));
    }

    #[test]
    fn engine_dag_cycle_detection() {
        // A = B + 1, B = A + 1 → 循环，两条规则都应被跳过，引擎为空
        let rules = vec![
            make_rule("A", "float", "B + 1", 0.0),
            make_rule("B", "float", "A + 1", 0.0),
        ];
        let engine = CalcEngine::new(rules).expect("循环依赖不应导致构建失败");
        assert!(engine.is_empty(), "循环中的所有规则都应被跳过");
    }

    #[test]
    fn engine_self_reference_skipped() {
        // C = B + C 属于自引用，应在编译阶段被跳过
        let rules = vec![
            make_rule("C", "uint", "B + C", 0.0),
            make_rule("X", "float", "sensor + 1", 0.0),
        ];
        let engine = CalcEngine::new(rules).unwrap();
        let names: Vec<&str> = engine.output_names();
        assert!(!names.contains(&"C"), "自引用规则 C 应被跳过");
        assert!(names.contains(&"X"), "正常规则 X 应保留");
    }

    #[test]
    fn engine_cycle_preserves_other_rules() {
        // B ↔ C 构成循环，A = x + 1 独立，应只保留 A
        let rules = vec![
            make_rule("A", "float", "x + 1", 0.0),
            make_rule("B", "float", "C + 1", 0.0),
            make_rule("C", "float", "B + 1", 0.0),
        ];
        let engine = CalcEngine::new(rules).unwrap();
        let names: Vec<&str> = engine.output_names();
        assert_eq!(names, vec!["A"], "循环规则应被跳过，独立规则保留");

        let results = engine.run_cycle(&vars(&[("x", int(10))]));
        assert_eq!(results.len(), 1);
        assert_eq!(results[0].1, Value::Float(11.0));
    }

    #[test]
    fn engine_user_crash_scenario() {
        // 用户提供的原始崩溃场景：C 自引用 + B↔C 循环 + A/D 依赖循环节点
        // 期望：不 panic，CalcEngine::new 返回 Ok
        let rules = vec![
            make_rule("C", "uint", "B+C", 0.0),
            make_rule("D", "uint", "B+C", 0.0),
            make_rule("A", "uint", "B+C", 0.0),
            make_rule("B", "uint", "C+D", 0.0),
        ];
        let engine = CalcEngine::new(rules).expect("用户配置不应导致崩溃");
        // 所有规则要么自引用(C)要么参与/依赖循环(A B D)，引擎应为空
        assert!(engine.is_empty());
    }

    // ---- 编译失败跳过 ----

    #[test]
    fn engine_skip_bad_expression() {
        let rules = vec![
            make_rule("good", "float", "a + 1", 0.0),
            make_rule("bad", "float", "a = b", 0.0), // 非法表达式
        ];
        let engine = CalcEngine::new(rules).unwrap();
        assert_eq!(engine.rules.len(), 1);
        assert_eq!(engine.rules[0].kks_calc, "good");
    }

    #[test]
    fn engine_skip_bad_limits() {
        let mut bad = make_rule("bad", "float", "a", 0.0);
        bad.min_limit = 10.0;
        bad.max_limit = 0.0;

        let engine = CalcEngine::new(vec![bad]).unwrap();
        assert!(engine.is_empty());
    }

    #[test]
    fn engine_skip_duplicate_output_name() {
        let rules = vec![
            make_rule("out", "uint", "a", 0.0),
            make_rule("out", "uint", "b", 0.0),
        ];
        let engine = CalcEngine::new(rules).unwrap();

        assert_eq!(engine.output_names(), vec!["out"]);
        let results = engine.run_cycle(&vars(&[("a", int(1)), ("b", int(2))]));
        assert_eq!(results[0].1, Value::UInt(1));
    }

    // ---- 空引擎 ----

    #[test]
    fn engine_empty() {
        let engine = CalcEngine::new(vec![]).unwrap();
        assert!(engine.is_empty());
        assert!(engine.run_cycle(&HashMap::new()).is_empty());
    }

    // ---- 辅助方法 ----

    #[test]
    fn engine_output_names() {
        let rules = vec![
            make_rule("x", "float", "a + 1", 0.0),
            make_rule("y", "uint", "b + 2", 0.0),
        ];
        let engine = CalcEngine::new(rules).unwrap();
        let names = engine.output_names();
        assert!(names.contains(&"x"));
        assert!(names.contains(&"y"));
    }

    #[test]
    fn engine_required_base_variables() {
        let rules = vec![
            make_rule("B", "float", "a * 2", 0.0),
            make_rule("A", "float", "B + c", 0.0),
        ];
        let engine = CalcEngine::new(rules).unwrap();
        let base = engine.required_base_variables();
        // a 和 c 是基础变量，B 是引擎自身产出的
        assert!(base.contains(&"a".to_string()));
        assert!(base.contains(&"c".to_string()));
        assert!(!base.contains(&"B".to_string()));
    }
}
