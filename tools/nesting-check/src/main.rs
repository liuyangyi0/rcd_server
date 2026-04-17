//! 计算规则引用链审查工具（egui GUI）。
//!
//! 加载 `calc.toml`，为每条规则计算：
//! - 最长引用链深度（到达基础变量之前经过的规则节点数）
//! - 直接依赖数
//! - 是否在循环 / 自引用里
//! - 被谁引用
//!
//! 对触发阈值的规则给出针对性改写建议。

mod app;
mod metrics;
mod rule_config;
mod suggest;

use app::RuleApp;

fn main() -> eframe::Result<()> {
    let options = eframe::NativeOptions {
        viewport: egui::ViewportBuilder::default()
            .with_inner_size([1200.0, 760.0])
            .with_min_inner_size([800.0, 500.0])
            .with_title("calc.toml 引用链审查"),
        ..Default::default()
    };

    eframe::run_native(
        "nesting-check",
        options,
        Box::new(|cc| Ok(Box::new(RuleApp::new(cc)))),
    )
}
