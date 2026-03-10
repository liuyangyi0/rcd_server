//! AST 求值器。
//!
//! 对解析后的表达式 AST 进行求值，所有中间计算使用 `f64`。
//! 内置除零保护、NaN/Infinity 检测、变量缺失回退等安全机制。

use std::collections::HashMap;

use super::parser::{BinOp, Expr, UnaryOp};

/// 对表达式 AST 求值。
///
/// - `variables`：当前全局变量存储。
/// - `default_value`：当变量缺失时使用的回退值。
///
/// 返回 `f64` 结果，调用方负责后续的 NaN/clamp/类型转换。
pub fn evaluate(expr: &Expr, variables: &HashMap<String, f64>, default_value: f64) -> f64 {
    match expr {
        Expr::Literal(n) => *n,

        Expr::Variable(name) => *variables.get(name.as_str()).unwrap_or(&default_value),

        Expr::Unary { op, operand } => {
            let val = evaluate(operand, variables, default_value);
            match op {
                UnaryOp::Neg => -val,
                UnaryOp::Not => {
                    if val == 0.0 {
                        1.0
                    } else {
                        0.0
                    }
                }
                UnaryOp::BitNot => {
                    let i = val as i64;
                    (!i) as f64
                }
            }
        }

        Expr::Binary { op, left, right } => {
            let l = evaluate(left, variables, default_value);
            let r = evaluate(right, variables, default_value);
            eval_binary(*op, l, r)
        }
    }
}

/// 二元运算求值，包含完整的安全防护。
fn eval_binary(op: BinOp, l: f64, r: f64) -> f64 {
    match op {
        // ---- 算术 ----
        BinOp::Add => l + r,
        BinOp::Sub => l - r,
        BinOp::Mul => l * r,
        BinOp::Div => {
            if r == 0.0 {
                f64::NAN // 由调用方统一处理 NaN → default_value
            } else {
                l / r
            }
        }
        BinOp::Mod => {
            if r == 0.0 {
                f64::NAN
            } else {
                l % r
            }
        }

        // ---- 位运算（转 i64 操作再转回 f64）----
        BinOp::BitAnd => ((l as i64) & (r as i64)) as f64,
        BinOp::BitOr => ((l as i64) | (r as i64)) as f64,
        BinOp::BitXor => ((l as i64) ^ (r as i64)) as f64,
        BinOp::ShiftLeft => {
            let shift = (r as u32).min(63); // 防止移位过大 panic
            ((l as i64) << shift) as f64
        }
        BinOp::ShiftRight => {
            let shift = (r as u32).min(63);
            ((l as i64) >> shift) as f64
        }

        // ---- 逻辑（非零为 true）----
        BinOp::And => {
            if l != 0.0 && r != 0.0 {
                1.0
            } else {
                0.0
            }
        }
        BinOp::Or => {
            if l != 0.0 || r != 0.0 {
                1.0
            } else {
                0.0
            }
        }

        // ---- 比较 ----
        BinOp::Eq => {
            if (l - r).abs() < f64::EPSILON {
                1.0
            } else {
                0.0
            }
        }
        BinOp::NotEq => {
            if (l - r).abs() >= f64::EPSILON {
                1.0
            } else {
                0.0
            }
        }
        BinOp::Lt => {
            if l < r {
                1.0
            } else {
                0.0
            }
        }
        BinOp::Gt => {
            if l > r {
                1.0
            } else {
                0.0
            }
        }
        BinOp::LtEq => {
            if l <= r {
                1.0
            } else {
                0.0
            }
        }
        BinOp::GtEq => {
            if l >= r {
                1.0
            } else {
                0.0
            }
        }
    }
}

// ============================================================
//  单元测试
// ============================================================

#[cfg(test)]
mod tests {
    use super::*;
    use crate::calc_engine::parser::parse;
    use crate::calc_engine::tokenizer::tokenize;

    /// 辅助：解析并求值表达式。
    fn eval(input: &str, vars: &HashMap<String, f64>) -> f64 {
        let tokens = tokenize(input).unwrap();
        let ast = parse(tokens).unwrap();
        evaluate(&ast, vars, 0.0)
    }

    fn vars(pairs: &[(&str, f64)]) -> HashMap<String, f64> {
        pairs.iter().map(|(k, v)| (k.to_string(), *v)).collect()
    }

    // ---- 基础算术 ----

    #[test]
    fn eval_add() {
        assert_eq!(eval("3 + 4", &HashMap::new()), 7.0);
    }

    #[test]
    fn eval_sub() {
        assert_eq!(eval("10 - 3", &HashMap::new()), 7.0);
    }

    #[test]
    fn eval_mul() {
        assert_eq!(eval("3 * 4", &HashMap::new()), 12.0);
    }

    #[test]
    fn eval_div() {
        assert_eq!(eval("10 / 4", &HashMap::new()), 2.5);
    }

    #[test]
    fn eval_mod() {
        assert_eq!(eval("10 % 3", &HashMap::new()), 1.0);
    }

    // ---- 除零保护 ----

    #[test]
    fn eval_div_by_zero() {
        let result = eval("10 / 0", &HashMap::new());
        assert!(result.is_nan());
    }

    #[test]
    fn eval_mod_by_zero() {
        let result = eval("10 % 0", &HashMap::new());
        assert!(result.is_nan());
    }

    // ---- 变量 ----

    #[test]
    fn eval_variable() {
        let v = vars(&[("rate_msv", 5.0)]);
        assert_eq!(eval("rate_msv * 1000", &v), 5000.0);
    }

    #[test]
    fn eval_missing_variable_uses_default() {
        let tokens = tokenize("missing_var + 1").unwrap();
        let ast = parse(tokens).unwrap();
        let result = evaluate(&ast, &HashMap::new(), 42.0);
        assert_eq!(result, 43.0); // default=42.0 + 1
    }

    // ---- 一元运算 ----

    #[test]
    fn eval_unary_neg() {
        assert_eq!(eval("-5", &HashMap::new()), -5.0);
    }

    #[test]
    fn eval_unary_not_zero() {
        assert_eq!(eval("!0", &HashMap::new()), 1.0);
    }

    #[test]
    fn eval_unary_not_nonzero() {
        assert_eq!(eval("!1", &HashMap::new()), 0.0);
    }

    #[test]
    fn eval_unary_bitnot() {
        // ~0 = -1 (所有位取反)
        assert_eq!(eval("~0", &HashMap::new()), -1.0);
    }

    // ---- 逻辑运算 ----

    #[test]
    fn eval_and_true() {
        assert_eq!(eval("1 && 1", &HashMap::new()), 1.0);
    }

    #[test]
    fn eval_and_false() {
        assert_eq!(eval("1 && 0", &HashMap::new()), 0.0);
    }

    #[test]
    fn eval_or_true() {
        assert_eq!(eval("0 || 1", &HashMap::new()), 1.0);
    }

    #[test]
    fn eval_or_false() {
        assert_eq!(eval("0 || 0", &HashMap::new()), 0.0);
    }

    // ---- 比较运算 ----

    #[test]
    fn eval_eq() {
        assert_eq!(eval("5 == 5", &HashMap::new()), 1.0);
        assert_eq!(eval("5 == 6", &HashMap::new()), 0.0);
    }

    #[test]
    fn eval_neq() {
        assert_eq!(eval("5 != 6", &HashMap::new()), 1.0);
        assert_eq!(eval("5 != 5", &HashMap::new()), 0.0);
    }

    #[test]
    fn eval_lt_gt() {
        assert_eq!(eval("3 < 5", &HashMap::new()), 1.0);
        assert_eq!(eval("5 < 3", &HashMap::new()), 0.0);
        assert_eq!(eval("5 > 3", &HashMap::new()), 1.0);
        assert_eq!(eval("3 > 5", &HashMap::new()), 0.0);
    }

    #[test]
    fn eval_lteq_gteq() {
        assert_eq!(eval("3 <= 3", &HashMap::new()), 1.0);
        assert_eq!(eval("3 >= 3", &HashMap::new()), 1.0);
        assert_eq!(eval("4 <= 3", &HashMap::new()), 0.0);
        assert_eq!(eval("2 >= 3", &HashMap::new()), 0.0);
    }

    // ---- 位运算 ----

    #[test]
    fn eval_bit_and() {
        // 0b1100 & 0b1010 = 0b1000 = 8
        assert_eq!(eval("12 & 10", &HashMap::new()), 8.0);
    }

    #[test]
    fn eval_bit_or() {
        // 0b1100 | 0b1010 = 0b1110 = 14
        assert_eq!(eval("12 | 10", &HashMap::new()), 14.0);
    }

    #[test]
    fn eval_bit_xor() {
        // 0b1100 ^ 0b1010 = 0b0110 = 6
        assert_eq!(eval("12 ^ 10", &HashMap::new()), 6.0);
    }

    #[test]
    fn eval_shift_left() {
        assert_eq!(eval("1 << 4", &HashMap::new()), 16.0);
    }

    #[test]
    fn eval_shift_right() {
        assert_eq!(eval("16 >> 2", &HashMap::new()), 4.0);
    }

    // ---- 优先级综合 ----

    #[test]
    fn eval_precedence() {
        // 2 + 3 * 4 = 2 + 12 = 14
        assert_eq!(eval("2 + 3 * 4", &HashMap::new()), 14.0);
    }

    #[test]
    fn eval_parentheses_override() {
        // (2 + 3) * 4 = 20
        assert_eq!(eval("(2 + 3) * 4", &HashMap::new()), 20.0);
    }

    // ---- 典型业务场景 ----

    #[test]
    fn eval_unit_conversion() {
        let v = vars(&[("rate_msv", 0.5)]);
        assert_eq!(eval("rate_msv * 1000", &v), 500.0);
    }

    #[test]
    fn eval_logic_combination() {
        let v = vars(&[("status_A", 1.0), ("status_B", 0.0)]);
        assert_eq!(eval("(status_A == 1) && (status_B == 0)", &v), 1.0);
    }

    #[test]
    fn eval_kks_cross_device() {
        let v = vars(&[
            ("9CYE91GH201_SL1", 100.0),
            ("9CYE92GH209_SL1", 200.0),
        ]);
        assert_eq!(eval("9CYE91GH201_SL1 + 9CYE92GH209_SL1", &v), 300.0);
    }
}
