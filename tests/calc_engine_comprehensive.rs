use std::collections::{HashMap, HashSet};
use std::path::Path;
use std::sync::Arc;

use rcd_server::calc_engine::config::{load_calc_config, CalcRule};
use rcd_server::calc_engine::engine::CalcEngine;
use rcd_server::calc_engine::evaluator::{evaluate, EvalError, EvalValue};
use rcd_server::calc_engine::parser::{extract_variables, parse};
use rcd_server::calc_engine::runner::CalcRunner;
use rcd_server::calc_engine::tokenizer::{tokenize, Token};
use rcd_server::model::{DeviceStatus, Value};

fn eval(input: &str, variables: &[(&str, EvalValue)]) -> Result<EvalValue, EvalError> {
    let tokens = tokenize(input).expect("tokenize expression");
    let ast = parse(tokens).expect("parse expression");
    let vars: HashMap<String, EvalValue> = variables
        .iter()
        .map(|(name, value)| ((*name).to_string(), value.clone()))
        .collect();
    evaluate(&ast, &vars)
}

fn rule(kks: &str, data_type: &str, expression: &str) -> CalcRule {
    CalcRule {
        kks_calc: kks.to_string(),
        data_type: data_type.to_string(),
        expression: expression.to_string(),
        max_limit: f64::MAX,
        min_limit: f64::MIN,
        default_value: 0.0,
    }
}

fn bounded_rule(
    kks: &str,
    data_type: &str,
    expression: &str,
    min_limit: f64,
    max_limit: f64,
    default_value: f64,
) -> CalcRule {
    CalcRule {
        kks_calc: kks.to_string(),
        data_type: data_type.to_string(),
        expression: expression.to_string(),
        min_limit,
        max_limit,
        default_value,
    }
}

fn vars(pairs: &[(&str, EvalValue)]) -> HashMap<String, EvalValue> {
    pairs
        .iter()
        .map(|(name, value)| ((*name).to_string(), value.clone()))
        .collect()
}

fn status(pairs: &[(&str, Value)]) -> DeviceStatus {
    DeviceStatus {
        id: 1,
        device_id: 1,
        com: "COM1".to_string(),
        com_status: true,
        device_status: true,
        value: pairs
            .iter()
            .map(|(name, value)| ((*name).to_string(), value.clone()))
            .collect(),
        raw_data: Vec::new(),
    }
}

fn result_value(results: &[(String, Value)], name: &str) -> Value {
    results
        .iter()
        .find_map(|(kks, value)| (kks == name).then(|| value.clone()))
        .unwrap_or_else(|| panic!("missing result {name}; got {results:?}"))
}

fn load_real_user_calc_engine() -> Option<CalcEngine> {
    let path = Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("test_data")
        .join("user_calc.toml");

    if !path.exists() {
        eprintln!("跳过真实 calc.toml 测试：{} 不存在", path.display());
        return None;
    }

    let cfg = load_calc_config(&path).expect("load real user calc.toml");
    Some(CalcEngine::new(cfg.rules).expect("compile real user calc.toml"))
}

fn real_calc_scenarios(engine: &CalcEngine) -> Vec<(String, HashMap<String, EvalValue>)> {
    let base_variables = engine.required_base_variables();
    let fill = |value: i64| -> HashMap<String, EvalValue> {
        base_variables
            .iter()
            .map(|name| (name.clone(), EvalValue::Int(value)))
            .collect()
    };

    let all_one = fill(1);
    let all_zero = fill(0);

    let mut ready_gate_true = fill(1);
    ready_gate_true.insert("9CYE91GH201_READY".to_string(), EvalValue::Int(1));
    ready_gate_true.insert("9CYE91GH201_SL3".to_string(), EvalValue::Int(13));

    let mut ready_gate_false = fill(1);
    ready_gate_false.insert("9CYE91GH201_READY".to_string(), EvalValue::Int(0));
    ready_gate_false.insert("9CYE91GH201_SL3".to_string(), EvalValue::Int(13));

    let patterned = base_variables
        .iter()
        .map(|name| {
            let sum: i64 = name.bytes().map(i64::from).sum();
            let value = if name.ends_with("_READY") {
                sum % 2
            } else {
                sum % 17
            };
            (name.clone(), EvalValue::Int(value))
        })
        .collect();

    vec![
        ("all_one".to_string(), all_one),
        ("all_zero".to_string(), all_zero),
        ("ready_gate_true".to_string(), ready_gate_true),
        ("ready_gate_false".to_string(), ready_gate_false),
        ("patterned".to_string(), patterned),
    ]
}

fn assert_value_matches_expected(kks: &str, actual: &Value, value_type: &str, expected: &str) {
    match actual {
        Value::UInt(value) => {
            assert_eq!(value_type, "uint", "{kks} expected type mismatch");
            assert_eq!(
                *value,
                expected.parse::<u32>().expect("parse expected uint"),
                "{kks} uint mismatch"
            );
        }
        Value::Bool(value) => {
            assert_eq!(value_type, "bool", "{kks} expected type mismatch");
            assert_eq!(*value, matches!(expected, "true"), "{kks} bool mismatch");
        }
        Value::Float(value) => {
            assert_eq!(value_type, "float", "{kks} expected type mismatch");
            let expected = expected.parse::<f64>().expect("parse expected float");
            assert!(
                ((*value as f64) - expected).abs() <= 1e-6,
                "{kks} float mismatch: actual={}, expected={}",
                value,
                expected
            );
        }
        Value::Double(value) => {
            assert_eq!(value_type, "double", "{kks} expected type mismatch");
            let expected = expected.parse::<f64>().expect("parse expected double");
            assert!(
                (*value - expected).abs() <= 1e-9,
                "{kks} double mismatch: actual={}, expected={}",
                value,
                expected
            );
        }
    }
}

#[test]
fn tokenizer_parser_preserve_kks_integer_float_bool_and_if_shape() {
    let tokens = tokenize("IF(9CYE91GH201_READY == 1, 9CYE91GH201_SL3, 4.5)").unwrap();

    assert_eq!(tokens[0], Token::Ident("IF".to_string()));
    assert!(tokens.contains(&Token::Ident("9CYE91GH201_READY".to_string())));
    assert!(tokens.contains(&Token::Int(1)));
    assert!(tokens.contains(&Token::Float(4.5)));
    assert_eq!(
        tokens
            .iter()
            .filter(|token| **token == Token::Comma)
            .count(),
        2
    );

    let ast = parse(tokens).unwrap();
    assert_eq!(
        extract_variables(&ast),
        vec![
            "9CYE91GH201_READY".to_string(),
            "9CYE91GH201_SL3".to_string()
        ]
    );

    assert_eq!(eval("true", &[]).unwrap(), EvalValue::Bool(true));
    assert_eq!(eval("FALSE", &[]).unwrap(), EvalValue::Bool(false));
}

#[test]
fn evaluator_uses_exact_int_and_bool_semantics_with_float_tolerance() {
    assert_eq!(
        eval("READY == 1", &[("READY", EvalValue::Int(1))]).unwrap(),
        EvalValue::Bool(true)
    );
    assert_eq!(
        eval("READY == true", &[("READY", EvalValue::Bool(true))]).unwrap(),
        EvalValue::Bool(true)
    );
    assert_eq!(eval("true == true", &[]).unwrap(), EvalValue::Bool(true));
    assert_eq!(eval("false != true", &[]).unwrap(), EvalValue::Bool(true));

    assert_eq!(
        eval("9007199254740993 == 9007199254740992", &[]).unwrap(),
        EvalValue::Bool(false)
    );
    assert_eq!(
        eval("0.1 + 0.2 == 0.3", &[]).unwrap(),
        EvalValue::Bool(true)
    );
}

#[test]
fn evaluator_keeps_existing_boolean_arithmetic_formula_compatible() {
    assert_eq!(
        eval(
            "(READY == 1) * SL3 + (READY != 1) * 4",
            &[("READY", EvalValue::Int(1)), ("SL3", EvalValue::Int(9))]
        )
        .unwrap(),
        EvalValue::Int(9)
    );
    assert_eq!(
        eval(
            "(READY == 1) * SL3 + (READY != 1) * 4",
            &[("READY", EvalValue::Int(0)), ("SL3", EvalValue::Int(9))]
        )
        .unwrap(),
        EvalValue::Int(4)
    );
}

#[test]
fn evaluator_short_circuits_logic_and_if_missing_branches() {
    assert_eq!(
        eval("0 && missing_var", &[]).unwrap(),
        EvalValue::Bool(false)
    );
    assert_eq!(
        eval("1 || missing_var", &[]).unwrap(),
        EvalValue::Bool(true)
    );
    assert_eq!(
        eval(
            "IF(ready == 0, 4, missing_var)",
            &[("ready", EvalValue::Int(0))]
        )
        .unwrap(),
        EvalValue::Int(4)
    );
    assert_eq!(
        eval(
            "IF(ready == 1, missing_var, 4)",
            &[("ready", EvalValue::Int(0))]
        )
        .unwrap(),
        EvalValue::Int(4)
    );
}

#[test]
fn evaluator_restricts_bitwise_to_integer_semantics() {
    assert_eq!(eval("true & 3", &[]).unwrap(), EvalValue::Int(1));
    assert_eq!(eval("4.0 << 1", &[]).unwrap(), EvalValue::Int(8));

    assert!(matches!(
        eval("1.2 & 1", &[]),
        Err(EvalError::TypeError(message)) if message.contains("不是整数")
    ));
}

#[test]
fn engine_if_avoids_missing_branch_but_legacy_boolean_multiply_does_not() {
    let engine = CalcEngine::new(vec![
        bounded_rule(
            "legacy",
            "uint",
            "(ready == 1) * sl3 + (ready != 1) * 4",
            0.0,
            100.0,
            88.0,
        ),
        bounded_rule(
            "if_gate",
            "uint",
            "IF(ready == 1, sl3, 4)",
            0.0,
            100.0,
            88.0,
        ),
    ])
    .unwrap();

    let results = engine.run_cycle(&vars(&[("ready", EvalValue::Int(0))]));

    assert_eq!(result_value(&results, "legacy"), Value::UInt(88));
    assert_eq!(result_value(&results, "if_gate"), Value::UInt(4));
}

#[test]
fn engine_converts_outputs_and_applies_limits_safely() {
    let engine = CalcEngine::new(vec![
        rule("uint_negative_saturates", "uint", "-5"),
        rule("uint_big_saturates", "uint32", "4294967296"),
        rule("bool_from_number", "boolean", "2"),
        rule("float_alias", "f32", "1.25"),
        rule("double_alias", "f64", "1.25 + 0.25"),
        bounded_rule("clamped", "float", "20", 0.0, 10.0, 0.0),
        bounded_rule("bad_bitwise_fallback", "uint", "1.2 & 1", 0.0, 100.0, 7.0),
    ])
    .unwrap();

    let results = engine.run_cycle(&HashMap::new());

    assert_eq!(
        result_value(&results, "uint_negative_saturates"),
        Value::UInt(0)
    );
    assert_eq!(
        result_value(&results, "uint_big_saturates"),
        Value::UInt(u32::MAX)
    );
    assert_eq!(
        result_value(&results, "bool_from_number"),
        Value::Bool(true)
    );
    assert_eq!(result_value(&results, "float_alias"), Value::Float(1.25));
    assert_eq!(result_value(&results, "double_alias"), Value::Double(1.5));
    assert_eq!(result_value(&results, "clamped"), Value::Float(10.0));
    assert_eq!(
        result_value(&results, "bad_bitwise_fallback"),
        Value::UInt(7)
    );
}

#[test]
fn engine_skips_invalid_rules_duplicates_and_cycles_without_stopping_valid_rules() {
    let rules = vec![
        rule("base_plus_one", "uint", "base + 1"),
        rule("duplicate", "uint", "1"),
        rule("duplicate", "uint", "2"),
        rule("unknown_type", "text", "1"),
        bounded_rule("bad_limits", "uint", "1", 10.0, 1.0, 0.0),
        bounded_rule("nan_default", "uint", "missing", 0.0, 10.0, f64::NAN),
        rule("cycle_a", "uint", "cycle_b + 1"),
        rule("cycle_b", "uint", "cycle_a + 1"),
        rule("after_valid", "uint", "base_plus_one + 1"),
    ];
    let engine = CalcEngine::new(rules).unwrap();

    assert_eq!(
        engine.output_names(),
        vec!["base_plus_one", "duplicate", "after_valid"]
    );
    assert_eq!(engine.required_base_variables(), vec!["base".to_string()]);

    let results = engine.run_cycle(&vars(&[("base", EvalValue::Int(5))]));
    assert_eq!(result_value(&results, "base_plus_one"), Value::UInt(6));
    assert_eq!(result_value(&results, "duplicate"), Value::UInt(1));
    assert_eq!(result_value(&results, "after_valid"), Value::UInt(7));
}

#[test]
fn runner_accumulates_mixed_model_values_across_updates() {
    let engine = Arc::new(
        CalcEngine::new(vec![
            rule("selected", "float", "IF(ready, analog, 4)"),
            rule("ready_as_uint", "uint", "ready"),
            rule("double_passthrough", "double", "precise"),
        ])
        .unwrap(),
    );
    let mut runner = CalcRunner::new(engine);

    let first = runner.on_update(&status(&[
        ("ready", Value::Bool(false)),
        ("precise", Value::Double(2.5)),
    ]));
    assert_eq!(result_value(&first, "selected"), Value::Float(4.0));
    assert_eq!(result_value(&first, "ready_as_uint"), Value::UInt(0));
    assert_eq!(
        result_value(&first, "double_passthrough"),
        Value::Double(2.5)
    );

    let second = runner.on_update(&status(&[("analog", Value::Float(9.5))]));
    assert_eq!(result_value(&second, "selected"), Value::Float(4.0));

    let third = runner.on_update(&status(&[("ready", Value::Bool(true))]));
    assert_eq!(result_value(&third, "selected"), Value::Float(9.5));
    assert_eq!(result_value(&third, "ready_as_uint"), Value::UInt(1));
}

#[test]
#[ignore]
fn real_user_calc_runs_all_rules_with_synthetic_inputs() {
    let Some(engine) = load_real_user_calc_engine() else {
        return;
    };
    let output_names = engine.output_names();
    let base_variables = engine.required_base_variables();
    let inputs: HashMap<String, EvalValue> = base_variables
        .iter()
        .map(|name| (name.clone(), EvalValue::Int(1)))
        .collect();

    let results = engine.run_cycle(&inputs);

    println!(
        "真实 calc.toml 合成输入烟测: outputs={}, base_variables={}",
        results.len(),
        base_variables.len()
    );
    assert_eq!(results.len(), output_names.len());
    assert!(
        results.len() > 1000,
        "真实配置输出数量异常偏少: {}",
        results.len()
    );
    for (name, value) in results {
        match value {
            Value::Float(f) => assert!(f.is_finite(), "{name} 输出非法 Float: {f}"),
            Value::Double(f) => assert!(f.is_finite(), "{name} 输出非法 Double: {f}"),
            Value::UInt(_) | Value::Bool(_) => {}
        }
    }
}

#[test]
#[ignore]
fn real_user_calc_known_ready_gate_outputs_expected_values() {
    let Some(engine) = load_real_user_calc_engine() else {
        return;
    };
    let mut inputs: HashMap<String, EvalValue> = engine
        .required_base_variables()
        .into_iter()
        .map(|name| (name, EvalValue::Int(1)))
        .collect();

    inputs.insert("9CYE91GH201_READY".to_string(), EvalValue::Int(1));
    inputs.insert("9CYE91GH201_SL3".to_string(), EvalValue::Int(13));
    let ready_results = engine.run_cycle(&inputs);
    assert_eq!(
        result_value(&ready_results, "9CYE91CB003XQ01"),
        Value::UInt(13)
    );

    inputs.insert("9CYE91GH201_READY".to_string(), EvalValue::Int(0));
    let not_ready_results = engine.run_cycle(&inputs);
    assert_eq!(
        result_value(&not_ready_results, "9CYE91CB003XQ01"),
        Value::UInt(4)
    );
}

#[test]
#[ignore]
fn real_user_calc_matches_python_generated_expected_table() {
    let Some(engine) = load_real_user_calc_engine() else {
        return;
    };
    let expected_path = Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("test_data")
        .join("calc_expected_outputs.csv");
    if !expected_path.exists() {
        eprintln!(
            "跳过 Python 期望表对比：{} 不存在。先运行: python tests\\calc_expected_oracle.py --calc test_data\\user_calc.toml --output test_data\\calc_expected_outputs.csv",
            expected_path.display()
        );
        return;
    }

    let mut reader = csv::Reader::from_path(&expected_path).expect("open Python expected CSV");
    let mut expected: HashMap<(String, String), (String, String)> = HashMap::new();
    for row in reader.records() {
        let row = row.expect("read expected CSV row");
        let scenario = row.get(0).expect("scenario column").to_string();
        let kks = row.get(1).expect("kks_calc column").to_string();
        let value_type = row.get(3).expect("value_type column").to_string();
        let expected_value = row.get(4).expect("expected column").to_string();
        assert!(
            expected
                .insert(
                    (scenario.clone(), kks.clone()),
                    (value_type, expected_value)
                )
                .is_none(),
            "duplicate expected row for scenario={scenario}, kks={kks}"
        );
    }

    let scenarios = real_calc_scenarios(&engine);
    let mut verified = HashSet::new();
    for (scenario, inputs) in scenarios {
        let results = engine.run_cycle(&inputs);
        let expected_count = expected
            .keys()
            .filter(|(expected_scenario, _)| expected_scenario == &scenario)
            .count();
        assert_eq!(
            results.len(),
            expected_count,
            "scenario {scenario} output count mismatch"
        );

        for (kks, value) in &results {
            let key = (scenario.clone(), kks.clone());
            let (value_type, expected_value) = expected.get(&key).unwrap_or_else(|| {
                panic!("missing expected row for scenario={scenario}, kks={kks}")
            });
            assert_value_matches_expected(kks, value, value_type, expected_value);
            verified.insert(key);
        }
    }

    assert_eq!(
        verified.len(),
        expected.len(),
        "Python expected table has rows that were not verified"
    );
}
