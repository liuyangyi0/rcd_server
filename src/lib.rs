//! RCD Server library crate。
//!
//! 把原本只在 binary 内部使用的模块以 `pub mod` 形式暴露出来，
//! 供 `tools/nesting-check` 等姊妹工具直接复用，
//! 避免重复实现 tokenizer / parser / CSV 解析等关键逻辑。
//!
//! binary 入口仍然在 `src/main.rs`。

pub mod broadcast;
pub mod build_info;
pub mod calc_engine;
pub mod command_tracking;
pub mod config;
pub mod csv_parser;
pub mod file_scanner;
mod log_push;
pub mod logging;
pub mod model;
pub mod operations;
pub mod protocol;
pub mod runtime_cache;
pub mod runtime_paths;
pub mod serial;
pub mod server;
