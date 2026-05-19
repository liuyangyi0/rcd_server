//! 表达式词法分析器。
//!
//! 将表达式字符串拆分为 Token 流，支持算术、逻辑、位运算、
//! 比较运算符以及数字字面量和变量标识符。

/// 词法单元。
#[derive(Debug, Clone, PartialEq)]
pub enum Token {
    /// 整数字面量。
    Int(i64),
    /// 浮点数字面量。
    Float(f64),
    /// 变量标识符。
    Ident(String),

    // 算术
    Plus,
    Minus,
    Star,
    Slash,
    Percent,

    // 位运算
    Amp,        // & (单独出现时为按位与)
    Pipe,       // | (单独出现时为按位或)
    Caret,      // ^
    Tilde,      // ~
    ShiftLeft,  // <<
    ShiftRight, // >>

    // 逻辑
    And,  // &&
    Or,   // ||
    Bang, // !

    // 比较
    Eq,    // ==
    NotEq, // !=
    Lt,    // <
    Gt,    // >
    LtEq,  // <=
    GtEq,  // >=

    // 分组
    LParen,
    RParen,
    Comma,

    // 结束
    Eof,
}

/// 将表达式字符串解析为 Token 列表。
///
/// # Errors
/// 遇到非法字符或格式错误时返回描述性错误信息。
pub fn tokenize(input: &str) -> Result<Vec<Token>, String> {
    let chars: Vec<char> = input.chars().collect();
    let mut tokens = Vec::new();
    let mut pos = 0;

    while pos < chars.len() {
        let ch = chars[pos];

        // 跳过空白
        if ch.is_ascii_whitespace() {
            pos += 1;
            continue;
        }

        // 数字字面量 或 以数字开头的标识符（如 KKS "9CYE91GH201_SL1"）
        if ch.is_ascii_digit()
            || (ch == '.' && pos + 1 < chars.len() && chars[pos + 1].is_ascii_digit())
        {
            let start = pos;
            // 先尝试消费数字（含小数点）
            let mut has_dot = false;
            while pos < chars.len() && (chars[pos].is_ascii_digit() || chars[pos] == '.') {
                if chars[pos] == '.' {
                    if has_dot {
                        break;
                    }
                    has_dot = true;
                }
                pos += 1;
            }
            // 如果数字后面紧跟字母或下划线，说明这是以数字开头的标识符
            if pos < chars.len() && (chars[pos].is_ascii_alphabetic() || chars[pos] == '_') {
                while pos < chars.len() && (chars[pos].is_ascii_alphanumeric() || chars[pos] == '_')
                {
                    pos += 1;
                }
                let ident: String = chars[start..pos].iter().collect();
                tokens.push(Token::Ident(ident));
            } else {
                let num_str: String = chars[start..pos].iter().collect();
                if has_dot {
                    let num = num_str
                        .parse::<f64>()
                        .map_err(|e| format!("无法解析浮点数 '{}': {}", num_str, e))?;
                    tokens.push(Token::Float(num));
                } else {
                    let num = num_str
                        .parse::<i64>()
                        .map_err(|e| format!("无法解析整数 '{}': {}", num_str, e))?;
                    tokens.push(Token::Int(num));
                }
            }
            continue;
        }

        // 标识符
        if ch.is_ascii_alphabetic() || ch == '_' {
            let start = pos;
            while pos < chars.len() && (chars[pos].is_ascii_alphanumeric() || chars[pos] == '_') {
                pos += 1;
            }
            let ident: String = chars[start..pos].iter().collect();
            tokens.push(Token::Ident(ident));
            continue;
        }

        // 运算符与标点
        match ch {
            '+' => {
                tokens.push(Token::Plus);
                pos += 1;
            }
            '-' => {
                tokens.push(Token::Minus);
                pos += 1;
            }
            '*' => {
                tokens.push(Token::Star);
                pos += 1;
            }
            '/' => {
                tokens.push(Token::Slash);
                pos += 1;
            }
            '%' => {
                tokens.push(Token::Percent);
                pos += 1;
            }
            '^' => {
                tokens.push(Token::Caret);
                pos += 1;
            }
            '~' => {
                tokens.push(Token::Tilde);
                pos += 1;
            }
            '(' => {
                tokens.push(Token::LParen);
                pos += 1;
            }
            ')' => {
                tokens.push(Token::RParen);
                pos += 1;
            }
            ',' => {
                tokens.push(Token::Comma);
                pos += 1;
            }

            '&' => {
                if pos + 1 < chars.len() && chars[pos + 1] == '&' {
                    tokens.push(Token::And);
                    pos += 2;
                } else {
                    tokens.push(Token::Amp);
                    pos += 1;
                }
            }
            '|' => {
                if pos + 1 < chars.len() && chars[pos + 1] == '|' {
                    tokens.push(Token::Or);
                    pos += 2;
                } else {
                    tokens.push(Token::Pipe);
                    pos += 1;
                }
            }
            '!' => {
                if pos + 1 < chars.len() && chars[pos + 1] == '=' {
                    tokens.push(Token::NotEq);
                    pos += 2;
                } else {
                    tokens.push(Token::Bang);
                    pos += 1;
                }
            }
            '=' => {
                if pos + 1 < chars.len() && chars[pos + 1] == '=' {
                    tokens.push(Token::Eq);
                    pos += 2;
                } else {
                    return Err(format!("位置 {} 处出现单独的 '='，请使用 '=='", pos));
                }
            }
            '<' => {
                if pos + 1 < chars.len() && chars[pos + 1] == '=' {
                    tokens.push(Token::LtEq);
                    pos += 2;
                } else if pos + 1 < chars.len() && chars[pos + 1] == '<' {
                    tokens.push(Token::ShiftLeft);
                    pos += 2;
                } else {
                    tokens.push(Token::Lt);
                    pos += 1;
                }
            }
            '>' => {
                if pos + 1 < chars.len() && chars[pos + 1] == '=' {
                    tokens.push(Token::GtEq);
                    pos += 2;
                } else if pos + 1 < chars.len() && chars[pos + 1] == '>' {
                    tokens.push(Token::ShiftRight);
                    pos += 2;
                } else {
                    tokens.push(Token::Gt);
                    pos += 1;
                }
            }

            _ => return Err(format!("位置 {} 处出现非法字符 '{}'", pos, ch)),
        }
    }

    tokens.push(Token::Eof);
    Ok(tokens)
}

// ============================================================
//  单元测试
// ============================================================

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn tokenize_arithmetic() {
        let tokens = tokenize("a + b * 3.14").unwrap();
        assert_eq!(
            tokens,
            vec![
                Token::Ident("a".into()),
                Token::Plus,
                Token::Ident("b".into()),
                Token::Star,
                Token::Float(3.14),
                Token::Eof,
            ]
        );
    }

    #[test]
    fn tokenize_logical_and_comparison() {
        let tokens = tokenize("(x == 1) && (y != 0)").unwrap();
        assert_eq!(
            tokens,
            vec![
                Token::LParen,
                Token::Ident("x".into()),
                Token::Eq,
                Token::Int(1),
                Token::RParen,
                Token::And,
                Token::LParen,
                Token::Ident("y".into()),
                Token::NotEq,
                Token::Int(0),
                Token::RParen,
                Token::Eof,
            ]
        );
    }

    #[test]
    fn tokenize_bitwise() {
        let tokens = tokenize("a & b | c ^ ~d << 2 >> 1").unwrap();
        assert_eq!(
            tokens,
            vec![
                Token::Ident("a".into()),
                Token::Amp,
                Token::Ident("b".into()),
                Token::Pipe,
                Token::Ident("c".into()),
                Token::Caret,
                Token::Tilde,
                Token::Ident("d".into()),
                Token::ShiftLeft,
                Token::Int(2),
                Token::ShiftRight,
                Token::Int(1),
                Token::Eof,
            ]
        );
    }

    #[test]
    fn tokenize_comparison_operators() {
        let tokens = tokenize("a > b < c >= d <= e").unwrap();
        assert_eq!(
            tokens,
            vec![
                Token::Ident("a".into()),
                Token::Gt,
                Token::Ident("b".into()),
                Token::Lt,
                Token::Ident("c".into()),
                Token::GtEq,
                Token::Ident("d".into()),
                Token::LtEq,
                Token::Ident("e".into()),
                Token::Eof,
            ]
        );
    }

    #[test]
    fn tokenize_unary_operators() {
        let tokens = tokenize("!a + -b").unwrap();
        assert_eq!(
            tokens,
            vec![
                Token::Bang,
                Token::Ident("a".into()),
                Token::Plus,
                Token::Minus,
                Token::Ident("b".into()),
                Token::Eof,
            ]
        );
    }

    #[test]
    fn tokenize_kks_identifier() {
        let tokens = tokenize("9CYE91GH201_SL1 * 1000").unwrap();
        assert_eq!(
            tokens,
            vec![
                Token::Ident("9CYE91GH201_SL1".into()),
                Token::Star,
                Token::Int(1000),
                Token::Eof,
            ]
        );
    }

    #[test]
    fn tokenize_error_single_equal() {
        let result = tokenize("a = b");
        assert!(result.is_err());
    }

    #[test]
    fn tokenize_error_illegal_char() {
        let result = tokenize("a @ b");
        assert!(result.is_err());
    }

    #[test]
    fn tokenize_modulo() {
        let tokens = tokenize("a % 3").unwrap();
        assert_eq!(
            tokens,
            vec![
                Token::Ident("a".into()),
                Token::Percent,
                Token::Int(3),
                Token::Eof,
            ]
        );
    }

    #[test]
    fn tokenize_empty() {
        let tokens = tokenize("").unwrap();
        assert_eq!(tokens, vec![Token::Eof]);
    }

    #[test]
    fn tokenize_integer() {
        let tokens = tokenize("42").unwrap();
        assert_eq!(tokens, vec![Token::Int(42), Token::Eof]);
    }

    #[test]
    fn tokenize_or_operator() {
        let tokens = tokenize("a || b").unwrap();
        assert_eq!(
            tokens,
            vec![
                Token::Ident("a".into()),
                Token::Or,
                Token::Ident("b".into()),
                Token::Eof,
            ]
        );
    }

    #[test]
    fn tokenize_if_commas() {
        let tokens = tokenize("IF(a == 1, b, 4)").unwrap();
        assert_eq!(
            tokens,
            vec![
                Token::Ident("IF".into()),
                Token::LParen,
                Token::Ident("a".into()),
                Token::Eq,
                Token::Int(1),
                Token::Comma,
                Token::Ident("b".into()),
                Token::Comma,
                Token::Int(4),
                Token::RParen,
                Token::Eof,
            ]
        );
    }
}
