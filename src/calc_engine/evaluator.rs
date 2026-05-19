//! AST 求值器。
//!
//! 表达式内部保留整数、布尔、浮点三类值。比较和逻辑运算返回真正的
//! `Bool`，只有在算术或整数语义需要时才把布尔值兼容转换为 `1/0`。

use std::collections::HashMap;
use std::fmt;

use super::parser::{BinOp, Expr, Literal, UnaryOp};
use crate::model::Value;

const FLOAT_EQ_EPSILON: f64 = 1e-9;

// ============================================================
//  值与错误类型
// ============================================================

/// 表达式求值过程中的内部类型。
#[derive(Debug, Clone, PartialEq)]
pub enum EvalValue {
    Int(i64),
    Bool(bool),
    Float(f64),
}

impl EvalValue {
    pub fn from_model_value(value: &Value) -> Self {
        match value {
            Value::UInt(n) => EvalValue::Int(*n as i64),
            Value::Bool(b) => EvalValue::Bool(*b),
            Value::Float(f) => EvalValue::Float(*f as f64),
            Value::Double(f) => EvalValue::Float(*f),
        }
    }

    pub fn as_bool(&self) -> bool {
        match self {
            EvalValue::Bool(b) => *b,
            EvalValue::Int(n) => *n != 0,
            EvalValue::Float(f) => *f != 0.0,
        }
    }

    pub fn as_f64(&self) -> f64 {
        match self {
            EvalValue::Int(n) => *n as f64,
            EvalValue::Bool(b) => {
                if *b {
                    1.0
                } else {
                    0.0
                }
            }
            EvalValue::Float(f) => *f,
        }
    }

    pub fn is_invalid_float(&self) -> bool {
        matches!(self, EvalValue::Float(f) if f.is_nan() || f.is_infinite())
    }

    fn is_float(&self) -> bool {
        matches!(self, EvalValue::Float(_))
    }

    fn as_int_for_integer_op(&self) -> Result<i64, EvalError> {
        match self {
            EvalValue::Int(n) => Ok(*n),
            EvalValue::Bool(b) => Ok(if *b { 1 } else { 0 }),
            EvalValue::Float(f) => {
                if !f.is_finite() {
                    return Err(EvalError::TypeError(format!(
                        "浮点值 {} 不是有限整数，不能用于整数运算",
                        f
                    )));
                }
                if f.fract() != 0.0 {
                    return Err(EvalError::TypeError(format!(
                        "浮点值 {} 不是整数，不能用于整数运算",
                        f
                    )));
                }
                if *f < i64::MIN as f64 || *f > i64::MAX as f64 {
                    return Err(EvalError::TypeError(format!(
                        "浮点值 {} 超出 i64 范围，不能用于整数运算",
                        f
                    )));
                }
                Ok(*f as i64)
            }
        }
    }
}

/// 求值过程中可能出现的错误。
#[derive(Debug)]
pub enum EvalError {
    /// 表达式引用的变量在当前上下文中不存在（丢包 / 未采集到）。
    MissingVariable(String),
    /// 除法或取模运算的除数为零。
    DivisionByZero,
    /// 表达式类型不适合当前运算。
    TypeError(String),
}

impl fmt::Display for EvalError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            EvalError::MissingVariable(name) => write!(f, "变量 '{}' 缺失", name),
            EvalError::DivisionByZero => write!(f, "除以零"),
            EvalError::TypeError(msg) => write!(f, "类型错误: {}", msg),
        }
    }
}

// ============================================================
//  求值入口
// ============================================================

/// 对表达式 AST 求值。
///
/// `&&`、`||` 和 `IF()` 会短路：未命中的分支不会求值，也不会触发缺失变量。
pub fn evaluate(
    expr: &Expr,
    variables: &HashMap<String, EvalValue>,
) -> Result<EvalValue, EvalError> {
    match expr {
        Expr::Literal(lit) => Ok(eval_literal(lit)),

        Expr::Variable(name) => variables
            .get(name.as_str())
            .cloned()
            .ok_or_else(|| EvalError::MissingVariable(name.clone())),

        Expr::Unary { op, operand } => {
            let val = evaluate(operand, variables)?;
            eval_unary(*op, val)
        }

        Expr::Binary { op, left, right } => match op {
            BinOp::And => {
                let l = evaluate(left, variables)?;
                if !l.as_bool() {
                    return Ok(EvalValue::Bool(false));
                }
                let r = evaluate(right, variables)?;
                Ok(EvalValue::Bool(r.as_bool()))
            }
            BinOp::Or => {
                let l = evaluate(left, variables)?;
                if l.as_bool() {
                    return Ok(EvalValue::Bool(true));
                }
                let r = evaluate(right, variables)?;
                Ok(EvalValue::Bool(r.as_bool()))
            }
            _ => {
                let l = evaluate(left, variables)?;
                let r = evaluate(right, variables)?;
                eval_binary(*op, l, r)
            }
        },

        Expr::If {
            condition,
            then_branch,
            else_branch,
        } => {
            let condition = evaluate(condition, variables)?;
            if condition.as_bool() {
                evaluate(then_branch, variables)
            } else {
                evaluate(else_branch, variables)
            }
        }
    }
}

fn eval_literal(lit: &Literal) -> EvalValue {
    match lit {
        Literal::Int(n) => EvalValue::Int(*n),
        Literal::Float(f) => EvalValue::Float(*f),
        Literal::Bool(b) => EvalValue::Bool(*b),
    }
}

// ============================================================
//  运算
// ============================================================

fn eval_unary(op: UnaryOp, val: EvalValue) -> Result<EvalValue, EvalError> {
    match op {
        UnaryOp::Neg => match val {
            EvalValue::Float(f) => Ok(EvalValue::Float(-f)),
            _ => val
                .as_int_for_integer_op()?
                .checked_neg()
                .map(EvalValue::Int)
                .ok_or_else(|| EvalError::TypeError("整数取反溢出".to_string())),
        },
        UnaryOp::Not => Ok(EvalValue::Bool(!val.as_bool())),
        UnaryOp::BitNot => Ok(EvalValue::Int(!val.as_int_for_integer_op()?)),
    }
}

fn eval_binary(op: BinOp, l: EvalValue, r: EvalValue) -> Result<EvalValue, EvalError> {
    match op {
        // ---- 算术 ----
        BinOp::Add => eval_add(l, r),
        BinOp::Sub => eval_sub(l, r),
        BinOp::Mul => eval_mul(l, r),
        BinOp::Div => eval_div(l, r),
        BinOp::Mod => eval_mod(l, r),

        // ---- 位运算 ----
        BinOp::BitAnd => Ok(EvalValue::Int(
            l.as_int_for_integer_op()? & r.as_int_for_integer_op()?,
        )),
        BinOp::BitOr => Ok(EvalValue::Int(
            l.as_int_for_integer_op()? | r.as_int_for_integer_op()?,
        )),
        BinOp::BitXor => Ok(EvalValue::Int(
            l.as_int_for_integer_op()? ^ r.as_int_for_integer_op()?,
        )),
        BinOp::ShiftLeft => {
            let shift = checked_shift(r.as_int_for_integer_op()?)?;
            Ok(EvalValue::Int(l.as_int_for_integer_op()? << shift))
        }
        BinOp::ShiftRight => {
            let shift = checked_shift(r.as_int_for_integer_op()?)?;
            Ok(EvalValue::Int(l.as_int_for_integer_op()? >> shift))
        }

        // 逻辑运算在 evaluate() 中短路处理。
        BinOp::And | BinOp::Or => unreachable!("logical ops are handled before eval_binary"),

        // ---- 比较 ----
        BinOp::Eq => Ok(EvalValue::Bool(eq_values(&l, &r))),
        BinOp::NotEq => Ok(EvalValue::Bool(!eq_values(&l, &r))),
        BinOp::Lt => Ok(EvalValue::Bool(l.as_f64() < r.as_f64())),
        BinOp::Gt => Ok(EvalValue::Bool(l.as_f64() > r.as_f64())),
        BinOp::LtEq => Ok(EvalValue::Bool(l.as_f64() <= r.as_f64())),
        BinOp::GtEq => Ok(EvalValue::Bool(l.as_f64() >= r.as_f64())),
    }
}

fn eval_add(l: EvalValue, r: EvalValue) -> Result<EvalValue, EvalError> {
    if l.is_float() || r.is_float() {
        Ok(EvalValue::Float(l.as_f64() + r.as_f64()))
    } else {
        l.as_int_for_integer_op()?
            .checked_add(r.as_int_for_integer_op()?)
            .map(EvalValue::Int)
            .ok_or_else(|| EvalError::TypeError("整数加法溢出".to_string()))
    }
}

fn eval_sub(l: EvalValue, r: EvalValue) -> Result<EvalValue, EvalError> {
    if l.is_float() || r.is_float() {
        Ok(EvalValue::Float(l.as_f64() - r.as_f64()))
    } else {
        l.as_int_for_integer_op()?
            .checked_sub(r.as_int_for_integer_op()?)
            .map(EvalValue::Int)
            .ok_or_else(|| EvalError::TypeError("整数减法溢出".to_string()))
    }
}

fn eval_mul(l: EvalValue, r: EvalValue) -> Result<EvalValue, EvalError> {
    if l.is_float() || r.is_float() {
        Ok(EvalValue::Float(l.as_f64() * r.as_f64()))
    } else {
        l.as_int_for_integer_op()?
            .checked_mul(r.as_int_for_integer_op()?)
            .map(EvalValue::Int)
            .ok_or_else(|| EvalError::TypeError("整数乘法溢出".to_string()))
    }
}

fn eval_div(l: EvalValue, r: EvalValue) -> Result<EvalValue, EvalError> {
    if r.as_f64() == 0.0 {
        Err(EvalError::DivisionByZero)
    } else {
        Ok(EvalValue::Float(l.as_f64() / r.as_f64()))
    }
}

fn eval_mod(l: EvalValue, r: EvalValue) -> Result<EvalValue, EvalError> {
    if r.as_f64() == 0.0 {
        return Err(EvalError::DivisionByZero);
    }
    if l.is_float() || r.is_float() {
        Ok(EvalValue::Float(l.as_f64() % r.as_f64()))
    } else {
        Ok(EvalValue::Int(
            l.as_int_for_integer_op()? % r.as_int_for_integer_op()?,
        ))
    }
}

fn checked_shift(shift: i64) -> Result<u32, EvalError> {
    if shift < 0 {
        return Err(EvalError::TypeError(format!(
            "移位位数 {} 不能为负数",
            shift
        )));
    }
    Ok((shift as u32).min(63))
}

fn eq_values(l: &EvalValue, r: &EvalValue) -> bool {
    match (l, r) {
        (EvalValue::Bool(a), EvalValue::Bool(b)) => a == b,
        (EvalValue::Int(a), EvalValue::Int(b)) => a == b,
        (EvalValue::Float(a), EvalValue::Float(b)) => (a - b).abs() <= FLOAT_EQ_EPSILON,
        _ => (l.as_f64() - r.as_f64()).abs() <= FLOAT_EQ_EPSILON,
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

    fn eval(input: &str, vars: &HashMap<String, EvalValue>) -> EvalValue {
        let tokens = tokenize(input).unwrap();
        let ast = parse(tokens).unwrap();
        evaluate(&ast, vars).unwrap()
    }

    fn eval_num(input: &str, vars: &HashMap<String, EvalValue>) -> f64 {
        eval(input, vars).as_f64()
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

    #[test]
    fn eval_add() {
        assert_eq!(eval("3 + 4", &HashMap::new()), int(7));
    }

    #[test]
    fn eval_sub() {
        assert_eq!(eval("10 - 3", &HashMap::new()), int(7));
    }

    #[test]
    fn eval_mul() {
        assert_eq!(eval("3 * 4", &HashMap::new()), int(12));
    }

    #[test]
    fn eval_div() {
        assert_eq!(eval("10 / 4", &HashMap::new()), float(2.5));
    }

    #[test]
    fn eval_mod() {
        assert_eq!(eval("10 % 3", &HashMap::new()), int(1));
    }

    #[test]
    fn eval_div_by_zero_returns_error() {
        let ast = parse(tokenize("10 / 0").unwrap()).unwrap();
        let result = evaluate(&ast, &HashMap::new());
        assert!(matches!(result, Err(EvalError::DivisionByZero)));
    }

    #[test]
    fn eval_mod_by_zero_returns_error() {
        let ast = parse(tokenize("10 % 0").unwrap()).unwrap();
        let result = evaluate(&ast, &HashMap::new());
        assert!(matches!(result, Err(EvalError::DivisionByZero)));
    }

    #[test]
    fn eval_variable() {
        let v = vars(&[("rate_msv", int(5))]);
        assert_eq!(eval("rate_msv * 1000", &v), int(5000));
    }

    #[test]
    fn eval_missing_variable_returns_error() {
        let ast = parse(tokenize("missing_var + 1").unwrap()).unwrap();
        let result = evaluate(&ast, &HashMap::new());
        assert!(
            matches!(result, Err(EvalError::MissingVariable(ref name)) if name == "missing_var")
        );
    }

    #[test]
    fn eval_missing_variable_still_errors_for_arithmetic() {
        let ast = parse(tokenize("missing_var + 10").unwrap()).unwrap();
        let result = evaluate(&ast, &HashMap::new());
        assert!(result.is_err());
    }

    #[test]
    fn eval_unary_neg() {
        assert_eq!(eval("-5", &HashMap::new()), int(-5));
    }

    #[test]
    fn eval_unary_not_zero() {
        assert_eq!(eval("!0", &HashMap::new()), bool_val(true));
    }

    #[test]
    fn eval_unary_not_nonzero() {
        assert_eq!(eval("!1", &HashMap::new()), bool_val(false));
    }

    #[test]
    fn eval_unary_bitnot() {
        assert_eq!(eval("~0", &HashMap::new()), int(-1));
    }

    #[test]
    fn eval_and_true() {
        assert_eq!(eval("1 && 1", &HashMap::new()), bool_val(true));
    }

    #[test]
    fn eval_and_false() {
        assert_eq!(eval("1 && 0", &HashMap::new()), bool_val(false));
    }

    #[test]
    fn eval_or_true() {
        assert_eq!(eval("0 || 1", &HashMap::new()), bool_val(true));
    }

    #[test]
    fn eval_or_false() {
        assert_eq!(eval("0 || 0", &HashMap::new()), bool_val(false));
    }

    #[test]
    fn eval_and_short_circuits_missing_right() {
        assert_eq!(eval("0 && missing_var", &HashMap::new()), bool_val(false));
    }

    #[test]
    fn eval_or_short_circuits_missing_right() {
        assert_eq!(eval("1 || missing_var", &HashMap::new()), bool_val(true));
    }

    #[test]
    fn eval_eq() {
        assert_eq!(eval("5 == 5", &HashMap::new()), bool_val(true));
        assert_eq!(eval("5 == 6", &HashMap::new()), bool_val(false));
    }

    #[test]
    fn eval_neq() {
        assert_eq!(eval("5 != 6", &HashMap::new()), bool_val(true));
        assert_eq!(eval("5 != 5", &HashMap::new()), bool_val(false));
    }

    #[test]
    fn eval_bool_comparison() {
        assert_eq!(eval("true == true", &HashMap::new()), bool_val(true));
        assert_eq!(eval("false != true", &HashMap::new()), bool_val(true));
        assert_eq!(eval("true == 1", &HashMap::new()), bool_val(true));
    }

    #[test]
    fn eval_float_eq_uses_tolerance() {
        assert_eq!(eval("0.1 + 0.2 == 0.3", &HashMap::new()), bool_val(true));
    }

    #[test]
    fn eval_lt_gt() {
        assert_eq!(eval("3 < 5", &HashMap::new()), bool_val(true));
        assert_eq!(eval("5 < 3", &HashMap::new()), bool_val(false));
        assert_eq!(eval("5 > 3", &HashMap::new()), bool_val(true));
        assert_eq!(eval("3 > 5", &HashMap::new()), bool_val(false));
    }

    #[test]
    fn eval_lteq_gteq() {
        assert_eq!(eval("3 <= 3", &HashMap::new()), bool_val(true));
        assert_eq!(eval("3 >= 3", &HashMap::new()), bool_val(true));
        assert_eq!(eval("4 <= 3", &HashMap::new()), bool_val(false));
        assert_eq!(eval("2 >= 3", &HashMap::new()), bool_val(false));
    }

    #[test]
    fn eval_bit_and() {
        assert_eq!(eval("12 & 10", &HashMap::new()), int(8));
    }

    #[test]
    fn eval_bit_or() {
        assert_eq!(eval("12 | 10", &HashMap::new()), int(14));
    }

    #[test]
    fn eval_bit_xor() {
        assert_eq!(eval("12 ^ 10", &HashMap::new()), int(6));
    }

    #[test]
    fn eval_shift_left() {
        assert_eq!(eval("1 << 4", &HashMap::new()), int(16));
    }

    #[test]
    fn eval_shift_right() {
        assert_eq!(eval("16 >> 2", &HashMap::new()), int(4));
    }

    #[test]
    fn eval_bitwise_rejects_non_integer_float() {
        let ast = parse(tokenize("1.2 & 1").unwrap()).unwrap();
        let result = evaluate(&ast, &HashMap::new());
        assert!(matches!(result, Err(EvalError::TypeError(_))));
    }

    #[test]
    fn eval_precedence() {
        assert_eq!(eval("2 + 3 * 4", &HashMap::new()), int(14));
    }

    #[test]
    fn eval_parentheses_override() {
        assert_eq!(eval("(2 + 3) * 4", &HashMap::new()), int(20));
    }

    #[test]
    fn eval_unit_conversion() {
        let v = vars(&[("rate_msv", float(0.5))]);
        assert_eq!(eval_num("rate_msv * 1000", &v), 500.0);
    }

    #[test]
    fn eval_logic_combination() {
        let v = vars(&[("status_A", int(1)), ("status_B", int(0))]);
        assert_eq!(
            eval("(status_A == 1) && (status_B == 0)", &v),
            bool_val(true)
        );
    }

    #[test]
    fn eval_kks_cross_device() {
        let v = vars(&[("9CYE91GH201_SL1", int(100)), ("9CYE92GH209_SL1", int(200))]);
        assert_eq!(eval("9CYE91GH201_SL1 + 9CYE92GH209_SL1", &v), int(300));
    }

    #[test]
    fn eval_boolean_arithmetic_compatibility() {
        let v = vars(&[("READY", int(1)), ("SL3", int(7))]);
        assert_eq!(eval("(READY == 1) * SL3", &v), int(7));

        let v = vars(&[("READY", int(0)), ("SL3", int(7))]);
        assert_eq!(eval("(READY == 1) * SL3", &v), int(0));
    }

    #[test]
    fn eval_if_short_circuits_then_branch() {
        let v = vars(&[("ready", int(0))]);
        assert_eq!(eval("IF(ready == 0, 4, missing_var)", &v), int(4));
    }

    #[test]
    fn eval_if_short_circuits_else_branch() {
        let v = vars(&[("ready", int(0))]);
        assert_eq!(eval("IF(ready == 1, sl3, 4)", &v), int(4));
    }

    #[test]
    fn eval_if_uses_then_branch() {
        let v = vars(&[("ready", int(1)), ("sl3", int(9))]);
        assert_eq!(eval("IF(ready == 1, sl3, 4)", &v), int(9));
    }
}
