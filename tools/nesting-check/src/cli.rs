//! CLI / Headless 入口。
//!
//! 不启动 GUI，从命令行加载 calc.toml（可选 CSV 目录），输出汇总到 stdout，
//! 可选导出 Markdown / JSON 报告。适合 CI 预检。

use std::path::PathBuf;
use std::process::ExitCode;

use clap::Parser;

use crate::metrics::{Severity, Thresholds};
use crate::report::{export_json, export_markdown};
use crate::rule_config::{load_and_analyze, load_and_analyze_with_csv, scan_csv_dir};

/// 退出码常量。脚本可按这些值路由告警通道。
pub const EXIT_OK: u8 = 0;
pub const EXIT_TOOL_ERROR: u8 = 2;
pub const EXIT_HAS_MEDIUM: u8 = 3;
pub const EXIT_HAS_HIGH: u8 = 4;
pub const EXIT_HAS_CRITICAL: u8 = 5;

#[derive(Parser, Debug)]
#[command(
    name = "nesting-check",
    version,
    about = "calc.toml 引用链审查工具（CLI 模式）"
)]
pub struct Cli {
    /// calc.toml 路径
    #[arg(short, long)]
    calc: PathBuf,

    /// CSV 目录（当前目录直接包含 rcd*.csv，不扫描子目录），可选；提供后会做基础变量交叉校验
    #[arg(short = 'd', long)]
    csv_dir: Option<PathBuf>,

    /// 导出 Markdown 报告路径
    #[arg(long)]
    md: Option<PathBuf>,

    /// 导出 JSON 报告路径
    #[arg(long)]
    json: Option<PathBuf>,

    /// 有 CRITICAL 时退出码 = 5（CI 友好）
    #[arg(long)]
    fail_on_critical: bool,

    /// 有 HIGH 或 CRITICAL 时退出码分别为 4 / 5
    #[arg(long)]
    fail_on_high: bool,

    /// 有 MEDIUM/HIGH/CRITICAL 时退出码分别为 3 / 4 / 5
    #[arg(long)]
    fail_on_medium: bool,

    /// 引用链深度阈值 medium
    #[arg(long, default_value_t = 2)]
    depth_medium: usize,
    /// 引用链深度阈值 high
    #[arg(long, default_value_t = 3)]
    depth_high: usize,
    /// 引用链深度阈值 critical
    #[arg(long, default_value_t = 5)]
    depth_critical: usize,
    /// 单条规则依赖数阈值 medium
    #[arg(long, default_value_t = 4)]
    deps_medium: usize,
    /// 单条规则依赖数阈值 high
    #[arg(long, default_value_t = 8)]
    deps_high: usize,
}

pub fn run(cli: Cli) -> ExitCode {
    let thresholds = Thresholds {
        depth_medium: cli.depth_medium,
        depth_high: cli.depth_high,
        depth_critical: cli.depth_critical,
        deps_medium: cli.deps_medium,
        deps_high: cli.deps_high,
    };

    // ---- 加载 CSV（可选）----
    let csv_kks = match cli.csv_dir.as_deref() {
        Some(dir) => match scan_csv_dir(dir) {
            Ok(index) => {
                println!(
                    "CSV 目录：{}\n  数据点总数：{}\n  真实 CSV 点位：{}\n  可匹配变量名：{}\n  扫描方式：只扫描当前目录，不扫描子目录（与主程序一致）",
                    dir.display(),
                    index.total_records,
                    index.real_kks.len(),
                    index.match_names.len()
                );
                Some(index)
            }
            Err(e) => {
                eprintln!("CSV 加载失败：{}", e);
                return ExitCode::from(EXIT_TOOL_ERROR);
            }
        },
        None => None,
    };

    // ---- 加载 calc.toml ----
    let result = match csv_kks {
        Some(csv) => load_and_analyze_with_csv(&cli.calc, &csv),
        None => load_and_analyze(&cli.calc),
    };
    let res = match result {
        Ok(r) => r,
        Err(e) => {
            eprintln!("calc.toml 加载失败：{}", e);
            return ExitCode::from(EXIT_TOOL_ERROR);
        }
    };

    // ---- 汇总打印 ----
    let s = &res.summary;
    println!("\n=== calc.toml 汇总 ===");
    println!("规则总数：{}", s.total_rules);
    println!("编译失败：{}", s.compile_failed);
    println!("循环依赖：{}", s.in_cycle);
    println!("依赖循环被跳过：{}", s.blocked_by_cycle);
    println!("自引用：{}", s.self_ref);
    println!("data_type 取值非法：{}", s.data_type_invalid);
    println!("max_limit < min_limit：{}", s.limits_inconsistent);
    println!("kks_calc 重复：{}", s.duplicate_kks);
    if s.csv_loaded {
        println!("\n=== CSV 交叉校验 ===");
        println!("CSV 真实点位：{}", s.csv_real_kks);
        println!("CSV 可匹配变量名：{}", s.csv_match_names);
        println!("与真实 CSV 点位重名：{}", s.csv_collisions);
        println!("变量缺失 / 疑似写错：{}", s.base_vars_missing);
    }

    // ---- 严重度分布 ----
    let (mut crit, mut high, mut med, mut ok) = (0usize, 0usize, 0usize, 0usize);
    for r in &res.reports {
        match thresholds.classify(r) {
            Severity::Critical => crit += 1,
            Severity::High => high += 1,
            Severity::Medium => med += 1,
            Severity::Ok => ok += 1,
        }
    }
    println!(
        "\n严重度分布：CRITICAL {} | HIGH {} | MEDIUM {} | OK {}",
        crit, high, med, ok
    );

    // ---- 列出 CRITICAL 与 HIGH 详情 ----
    if crit > 0 {
        println!("\n--- CRITICAL ---");
        for r in res
            .reports
            .iter()
            .filter(|r| thresholds.classify(r) == Severity::Critical)
        {
            print_brief(r);
        }
    }
    if high > 0 {
        println!("\n--- HIGH ---");
        for r in res
            .reports
            .iter()
            .filter(|r| thresholds.classify(r) == Severity::High)
        {
            print_brief(r);
        }
    }

    // ---- 导出 ----
    if let Some(p) = &cli.md {
        if let Err(e) = export_markdown(&res.reports, &res.summary, &thresholds, p) {
            eprintln!("导出 Markdown 失败：{}", e);
            return ExitCode::from(EXIT_TOOL_ERROR);
        }
        println!("\n已导出 Markdown：{}", p.display());
    }
    if let Some(p) = &cli.json {
        if let Err(e) = export_json(&res.reports, &res.summary, &thresholds, p) {
            eprintln!("导出 JSON 失败：{}", e);
            return ExitCode::from(EXIT_TOOL_ERROR);
        }
        println!("已导出 JSON：{}", p.display());
    }

    // ---- 退出码 ----
    // 优先级 critical > high > medium，最严重者决定退出码
    if (cli.fail_on_critical || cli.fail_on_high || cli.fail_on_medium) && crit > 0 {
        return ExitCode::from(EXIT_HAS_CRITICAL);
    }
    if (cli.fail_on_high || cli.fail_on_medium) && high > 0 {
        return ExitCode::from(EXIT_HAS_HIGH);
    }
    if cli.fail_on_medium && med > 0 {
        return ExitCode::from(EXIT_HAS_MEDIUM);
    }
    ExitCode::from(EXIT_OK)
}

fn print_brief(r: &crate::metrics::RuleReport) {
    println!(
        "  {:<28}  D={:<2} 依赖={:<2} [{}]",
        r.name,
        r.max_chain_depth,
        r.direct_deps.len(),
        r.flag_labels().join(",")
    );
    if let Some(err) = &r.compile_error {
        println!("    └── {}", err);
    }
    if !r.missing_in_csv.is_empty() {
        println!("    └── 运行期缺失变量: {}", r.missing_in_csv.join(", "));
    }
}
