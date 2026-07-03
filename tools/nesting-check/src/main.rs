//! 计算规则引用链审查工具。
//!
//! 运行模式：
//! - 无参数 → 启动 egui GUI（默认）
//! - 带参数（如 `--calc path/to/calc.toml`）→ CLI / Headless 模式
//!
//! 加载 `calc.toml`（可选 `rcd*.csv` 目录），为每条规则计算：
//! - 最长引用链深度（到达基础变量之前经过的规则节点数）
//! - 直接依赖数
//! - 是否在循环 / 自引用里
//! - 是否被 calc_engine 拒绝（编译错误）
//! - data_type / max_limit-min_limit / kks_calc 重复 / 与 CSV 重名 / 运行期缺失变量等
//!
//! 对触发阈值的规则给出针对性改写建议。

mod app;
mod cli;
mod metrics;
mod report;
mod rule_config;
mod suggest;

use std::process::ExitCode;

use app::RuleApp;
use clap::Parser;

fn main() -> ExitCode {
    // 没有任何 CLI 参数 → 走 GUI；任何参数都走 CLI（包括 --help / --version）
    let argv: Vec<String> = std::env::args().collect();
    if argv.len() == 1 {
        return run_gui();
    }
    cli::run(cli::Cli::parse())
}

fn run_gui() -> ExitCode {
    let options = eframe::NativeOptions {
        viewport: egui::ViewportBuilder::default()
            .with_inner_size([1280.0, 800.0])
            .with_min_inner_size([900.0, 540.0])
            .with_title("calc.toml 引用链审查"),
        ..Default::default()
    };

    let result = eframe::run_native(
        "nesting-check",
        options,
        Box::new(|cc| Ok(Box::new(RuleApp::new(cc)))),
    );
    if let Err(e) = result {
        eprintln!("GUI 启动失败: {}", e);
        return ExitCode::from(2);
    }
    ExitCode::SUCCESS
}
