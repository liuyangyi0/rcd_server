//! Opt-in exporter used by the manager migration acceptance test.
//!
//! The test is inert during an ordinary `cargo test`. On the deployment host,
//! set `RCD_CALC_PARITY_CONFIG` and `RCD_CALC_PARITY_OUTPUT` to produce the
//! Rust engine's deterministic result set for the exact deployed calc.toml.

use std::collections::HashMap;
use std::path::Path;

use rcd_server::calc_engine::config::load_calc_config;
use rcd_server::calc_engine::evaluator::EvalValue;
use rcd_server::calc_engine::engine::CalcEngine;
use rcd_server::model::Value;
use serde::Serialize;

#[derive(Serialize)]
struct ParityResult {
    kind: &'static str,
    number: f64,
    boolean: bool,
    valid: bool,
    dependencies: usize,
}

#[test]
fn export_deployed_calc_results_for_go_parity() {
    let Ok(config_path) = std::env::var("RCD_CALC_PARITY_CONFIG") else {
        eprintln!("RCD_CALC_PARITY_CONFIG is unset; skipping opt-in parity export");
        return;
    };
    let output_path = std::env::var("RCD_CALC_PARITY_OUTPUT")
        .expect("RCD_CALC_PARITY_OUTPUT must accompany RCD_CALC_PARITY_CONFIG");

    let config = load_calc_config(Path::new(&config_path)).expect("load deployed calc.toml");
    let engine = CalcEngine::new(config.rules).expect("compile deployed calc.toml");
    let inputs: HashMap<String, EvalValue> = engine
        .required_base_variables()
        .into_iter()
        .map(|name| {
            let value = deterministic_input(&name);
            (name, EvalValue::Int(value))
        })
        .collect();
    let validity = inputs.keys().map(|name| (name.clone(), true)).collect();
    let dependencies: HashMap<String, usize> = engine
        .output_dependencies()
        .into_iter()
        .map(|(name, values)| (name.to_owned(), values.len()))
        .collect();

    let mut results = HashMap::new();
    for (name, value, valid) in engine.run_cycle_with_quality(&inputs, &validity) {
        let item = match value {
            Value::UInt(value) => ParityResult {
                kind: "UInt32",
                number: value as f64,
                boolean: false,
                valid,
                dependencies: dependencies[&name],
            },
            Value::Bool(value) => ParityResult {
                kind: "Boolean",
                number: if value { 1.0 } else { 0.0 },
                boolean: value,
                valid,
                dependencies: dependencies[&name],
            },
            Value::Float(value) => ParityResult {
                kind: "Float",
                number: value as f64,
                boolean: false,
                valid,
                dependencies: dependencies[&name],
            },
            Value::Double(value) => ParityResult {
                kind: "Double",
                number: value,
                boolean: false,
                valid,
                dependencies: dependencies[&name],
            },
        };
        results.insert(name, item);
    }
    let encoded = serde_json::to_vec(&results).expect("encode parity results");
    std::fs::write(&output_path, encoded).expect("write parity results");
    assert_eq!(results.len(), engine.output_names().len());
}

fn deterministic_input(name: &str) -> i64 {
    let hash = name
        .as_bytes()
        .iter()
        .fold(2_166_136_261_u32, |value, byte| {
            (value ^ u32::from(*byte)).wrapping_mul(16_777_619)
        });
    i64::from(hash % 17 + 1)
}
