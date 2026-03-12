//! 计算引擎模块。
//!
//! 提供基于公式表达式的衍生变量计算能力：
//! - `tokenizer`：词法分析，将表达式字符串拆分为 Token 流。
//! - `parser`：Pratt 解析，将 Token 流构建为 AST。
//! - `evaluator`：AST 求值，Fail-Fast 短路错误处理。
//! - `config`：TOML 计算规则配置加载。
//! - `engine`：运行引擎，负责 DAG 拓扑排序、求值编排、Clamping 与类型转换。

pub mod config;
pub mod engine;
pub mod evaluator;
pub mod parser;
pub mod tokenizer;
