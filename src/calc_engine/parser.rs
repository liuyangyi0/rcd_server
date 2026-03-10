//! Pratt 表达式解析器。
//!
//! 将 Token 流解析为 AST（抽象语法树），支持完整的运算符优先级
//! 和括号嵌套。采用 Pratt 解析（自顶向下运算符优先级解析）算法。

use super::tokenizer::Token;

// ============================================================
//  AST 节点定义
// ============================================================

/// 二元运算符。
#[derive(Debug, Clone, Copy, PartialEq)]
pub enum BinOp {
    // 算术
    Add, Sub, Mul, Div, Mod,
    // 位运算
    BitAnd, BitOr, BitXor, ShiftLeft, ShiftRight,
    // 逻辑
    And, Or,
    // 比较
    Eq, NotEq, Lt, Gt, LtEq, GtEq,
}

/// 一元运算符。
#[derive(Debug, Clone, Copy, PartialEq)]
pub enum UnaryOp {
    /// 算术取反 `-`。
    Neg,
    /// 逻辑取反 `!`。
    Not,
    /// 按位取反 `~`。
    BitNot,
}

/// 表达式 AST 节点。
#[derive(Debug, Clone, PartialEq)]
pub enum Expr {
    /// 数字字面量。
    Literal(f64),
    /// 变量引用。
    Variable(String),
    /// 二元运算。
    Binary {
        op: BinOp,
        left: Box<Expr>,
        right: Box<Expr>,
    },
    /// 一元运算。
    Unary {
        op: UnaryOp,
        operand: Box<Expr>,
    },
}

/// 提取表达式中所有引用的变量名（去重）。
pub fn extract_variables(expr: &Expr) -> Vec<String> {
    let mut vars = Vec::new();
    collect_variables(expr, &mut vars);
    vars.sort();
    vars.dedup();
    vars
}

fn collect_variables(expr: &Expr, out: &mut Vec<String>) {
    match expr {
        Expr::Literal(_) => {}
        Expr::Variable(name) => out.push(name.clone()),
        Expr::Binary { left, right, .. } => {
            collect_variables(left, out);
            collect_variables(right, out);
        }
        Expr::Unary { operand, .. } => {
            collect_variables(operand, out);
        }
    }
}

// ============================================================
//  解析器
// ============================================================

/// Pratt 解析器。
struct Parser {
    tokens: Vec<Token>,
    pos: usize,
}

/// 将 Token 列表解析为 AST。
///
/// # Errors
/// 表达式不合法时返回描述性错误信息。
pub fn parse(tokens: Vec<Token>) -> Result<Expr, String> {
    let mut parser = Parser { tokens, pos: 0 };
    let expr = parser.expr_bp(0)?;
    if parser.peek() != &Token::Eof {
        return Err(format!("解析完成后仍有多余的 Token: {:?}", parser.peek()));
    }
    Ok(expr)
}

impl Parser {
    fn peek(&self) -> &Token {
        self.tokens.get(self.pos).unwrap_or(&Token::Eof)
    }

    fn advance(&mut self) -> Token {
        let tok = self.tokens.get(self.pos).cloned().unwrap_or(Token::Eof);
        self.pos += 1;
        tok
    }

    /// Pratt 解析核心：以 `min_bp` 为最低绑定力解析表达式。
    fn expr_bp(&mut self, min_bp: u8) -> Result<Expr, String> {
        // ---- 前缀部分 ----
        let mut lhs = match self.advance() {
            Token::Number(n) => Expr::Literal(n),
            Token::Ident(name) => Expr::Variable(name),
            Token::LParen => {
                let inner = self.expr_bp(0)?;
                if self.advance() != Token::RParen {
                    return Err("缺少右括号 ')'".to_string());
                }
                inner
            }
            // 一元运算符
            Token::Minus => {
                let ((), r_bp) = prefix_bp(&UnaryOp::Neg);
                let operand = self.expr_bp(r_bp)?;
                Expr::Unary { op: UnaryOp::Neg, operand: Box::new(operand) }
            }
            Token::Bang => {
                let ((), r_bp) = prefix_bp(&UnaryOp::Not);
                let operand = self.expr_bp(r_bp)?;
                Expr::Unary { op: UnaryOp::Not, operand: Box::new(operand) }
            }
            Token::Tilde => {
                let ((), r_bp) = prefix_bp(&UnaryOp::BitNot);
                let operand = self.expr_bp(r_bp)?;
                Expr::Unary { op: UnaryOp::BitNot, operand: Box::new(operand) }
            }
            tok => return Err(format!("期望表达式，但遇到 {:?}", tok)),
        };

        // ---- 中缀部分 ----
        loop {
            let op = match self.peek() {
                Token::Eof | Token::RParen => break,
                tok => match token_to_binop(tok) {
                    Some(op) => op,
                    None => break,
                },
            };

            let (l_bp, r_bp) = infix_bp(&op);
            if l_bp < min_bp {
                break;
            }

            self.advance(); // consume operator token
            let rhs = self.expr_bp(r_bp)?;
            lhs = Expr::Binary {
                op,
                left: Box::new(lhs),
                right: Box::new(rhs),
            };
        }

        Ok(lhs)
    }
}

// ============================================================
//  绑定力（优先级）
// ============================================================

/// 中缀运算符的左右绑定力。
///
/// 左绑定力决定何时"停止当前层"，右绑定力决定递归解析的最低要求。
/// 左结合：`r_bp = l_bp + 1`；右结合：`r_bp = l_bp`。
fn infix_bp(op: &BinOp) -> (u8, u8) {
    match op {
        BinOp::Or       => (2, 3),
        BinOp::And      => (4, 5),
        BinOp::BitOr    => (6, 7),
        BinOp::BitXor   => (8, 9),
        BinOp::BitAnd   => (10, 11),
        BinOp::Eq | BinOp::NotEq     => (12, 13),
        BinOp::Lt | BinOp::Gt | BinOp::LtEq | BinOp::GtEq => (14, 15),
        BinOp::ShiftLeft | BinOp::ShiftRight => (16, 17),
        BinOp::Add | BinOp::Sub => (18, 19),
        BinOp::Mul | BinOp::Div | BinOp::Mod => (20, 21),
    }
}

/// 前缀运算符的绑定力（返回 `((), right_bp)`）。
fn prefix_bp(_op: &UnaryOp) -> ((), u8) {
    // 所有一元运算符具有相同的高优先级
    ((), 23)
}

/// 将 Token 映射为 BinOp。
fn token_to_binop(tok: &Token) -> Option<BinOp> {
    match tok {
        Token::Plus       => Some(BinOp::Add),
        Token::Minus      => Some(BinOp::Sub),
        Token::Star       => Some(BinOp::Mul),
        Token::Slash      => Some(BinOp::Div),
        Token::Percent    => Some(BinOp::Mod),
        Token::Amp        => Some(BinOp::BitAnd),
        Token::Pipe       => Some(BinOp::BitOr),
        Token::Caret      => Some(BinOp::BitXor),
        Token::ShiftLeft  => Some(BinOp::ShiftLeft),
        Token::ShiftRight => Some(BinOp::ShiftRight),
        Token::And        => Some(BinOp::And),
        Token::Or         => Some(BinOp::Or),
        Token::Eq         => Some(BinOp::Eq),
        Token::NotEq      => Some(BinOp::NotEq),
        Token::Lt         => Some(BinOp::Lt),
        Token::Gt         => Some(BinOp::Gt),
        Token::LtEq       => Some(BinOp::LtEq),
        Token::GtEq       => Some(BinOp::GtEq),
        _ => None,
    }
}

// ============================================================
//  单元测试
// ============================================================

#[cfg(test)]
mod tests {
    use super::*;
    use crate::calc_engine::tokenizer::tokenize;

    /// 辅助函数：直接从字符串解析为 AST。
    fn parse_expr(input: &str) -> Result<Expr, String> {
        let tokens = tokenize(input)?;
        parse(tokens)
    }

    #[test]
    fn parse_literal() {
        assert_eq!(parse_expr("42").unwrap(), Expr::Literal(42.0));
    }

    #[test]
    fn parse_variable() {
        assert_eq!(parse_expr("abc").unwrap(), Expr::Variable("abc".into()));
    }

    #[test]
    fn parse_simple_add() {
        let expr = parse_expr("a + b").unwrap();
        assert_eq!(expr, Expr::Binary {
            op: BinOp::Add,
            left: Box::new(Expr::Variable("a".into())),
            right: Box::new(Expr::Variable("b".into())),
        });
    }

    #[test]
    fn parse_precedence_mul_over_add() {
        // a + b * c => a + (b * c)
        let expr = parse_expr("a + b * c").unwrap();
        assert_eq!(expr, Expr::Binary {
            op: BinOp::Add,
            left: Box::new(Expr::Variable("a".into())),
            right: Box::new(Expr::Binary {
                op: BinOp::Mul,
                left: Box::new(Expr::Variable("b".into())),
                right: Box::new(Expr::Variable("c".into())),
            }),
        });
    }

    #[test]
    fn parse_parentheses() {
        // (a + b) * c
        let expr = parse_expr("(a + b) * c").unwrap();
        assert_eq!(expr, Expr::Binary {
            op: BinOp::Mul,
            left: Box::new(Expr::Binary {
                op: BinOp::Add,
                left: Box::new(Expr::Variable("a".into())),
                right: Box::new(Expr::Variable("b".into())),
            }),
            right: Box::new(Expr::Variable("c".into())),
        });
    }

    #[test]
    fn parse_unary_neg() {
        let expr = parse_expr("-a").unwrap();
        assert_eq!(expr, Expr::Unary {
            op: UnaryOp::Neg,
            operand: Box::new(Expr::Variable("a".into())),
        });
    }

    #[test]
    fn parse_unary_not() {
        let expr = parse_expr("!flag").unwrap();
        assert_eq!(expr, Expr::Unary {
            op: UnaryOp::Not,
            operand: Box::new(Expr::Variable("flag".into())),
        });
    }

    #[test]
    fn parse_unary_bitnot() {
        let expr = parse_expr("~mask").unwrap();
        assert_eq!(expr, Expr::Unary {
            op: UnaryOp::BitNot,
            operand: Box::new(Expr::Variable("mask".into())),
        });
    }

    #[test]
    fn parse_logic_and_comparison() {
        // (x == 1) && (y != 0)
        let expr = parse_expr("(x == 1) && (y != 0)").unwrap();
        assert_eq!(expr, Expr::Binary {
            op: BinOp::And,
            left: Box::new(Expr::Binary {
                op: BinOp::Eq,
                left: Box::new(Expr::Variable("x".into())),
                right: Box::new(Expr::Literal(1.0)),
            }),
            right: Box::new(Expr::Binary {
                op: BinOp::NotEq,
                left: Box::new(Expr::Variable("y".into())),
                right: Box::new(Expr::Literal(0.0)),
            }),
        });
    }

    #[test]
    fn parse_bitwise_and_shift() {
        // a & 0xFF => Binary(BitAnd, a, 0xFF)
        // 实际上 0xFF 会被 tokenizer 识别为标识符 "0xFF"
        // 使用纯数字测试
        let expr = parse_expr("a & 255").unwrap();
        assert_eq!(expr, Expr::Binary {
            op: BinOp::BitAnd,
            left: Box::new(Expr::Variable("a".into())),
            right: Box::new(Expr::Literal(255.0)),
        });
    }

    #[test]
    fn parse_left_associativity() {
        // a - b - c => (a - b) - c
        let expr = parse_expr("a - b - c").unwrap();
        assert_eq!(expr, Expr::Binary {
            op: BinOp::Sub,
            left: Box::new(Expr::Binary {
                op: BinOp::Sub,
                left: Box::new(Expr::Variable("a".into())),
                right: Box::new(Expr::Variable("b".into())),
            }),
            right: Box::new(Expr::Variable("c".into())),
        });
    }

    #[test]
    fn parse_complex_kks_expression() {
        let expr = parse_expr("9CYE91GH201_SL1 * 1000").unwrap();
        assert_eq!(expr, Expr::Binary {
            op: BinOp::Mul,
            left: Box::new(Expr::Variable("9CYE91GH201_SL1".into())),
            right: Box::new(Expr::Literal(1000.0)),
        });
    }

    #[test]
    fn parse_unmatched_paren() {
        assert!(parse_expr("(a + b").is_err());
    }

    #[test]
    fn parse_empty() {
        assert!(parse_expr("").is_err());
    }

    #[test]
    fn extract_variables_complex() {
        let expr = parse_expr("(a + b) * c - a").unwrap();
        let vars = extract_variables(&expr);
        assert_eq!(vars, vec!["a", "b", "c"]);
    }

    #[test]
    fn parse_or_precedence() {
        // a || b && c => a || (b && c)
        let expr = parse_expr("a || b && c").unwrap();
        assert_eq!(expr, Expr::Binary {
            op: BinOp::Or,
            left: Box::new(Expr::Variable("a".into())),
            right: Box::new(Expr::Binary {
                op: BinOp::And,
                left: Box::new(Expr::Variable("b".into())),
                right: Box::new(Expr::Variable("c".into())),
            }),
        });
    }

    #[test]
    fn parse_neg_literal() {
        // -42 => Unary(Neg, Literal(42))
        let expr = parse_expr("-42").unwrap();
        assert_eq!(expr, Expr::Unary {
            op: UnaryOp::Neg,
            operand: Box::new(Expr::Literal(42.0)),
        });
    }

    #[test]
    fn parse_nested_unary() {
        // !!a => !(!(a))
        let expr = parse_expr("!!a").unwrap();
        assert_eq!(expr, Expr::Unary {
            op: UnaryOp::Not,
            operand: Box::new(Expr::Unary {
                op: UnaryOp::Not,
                operand: Box::new(Expr::Variable("a".into())),
            }),
        });
    }
}
