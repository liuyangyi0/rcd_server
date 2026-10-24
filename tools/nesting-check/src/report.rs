//! 报告导出：Markdown / JSON。

use std::fs::File;
use std::io::Write;
use std::path::Path;

use serde::Serialize;

use crate::metrics::{GlobalSummary, RuleReport, Severity, Thresholds};
use crate::suggest::suggest;

// ============================================================
//  Markdown
// ============================================================

pub fn export_markdown(
    rules: &[RuleReport],
    summary: &GlobalSummary,
    t: &Thresholds,
    path: &Path,
) -> std::io::Result<()> {
    let mut f = File::create(path)?;
    writeln!(f, "# calc.toml 审计报告")?;
    writeln!(f)?;
    writeln!(f, "## 总览")?;
    writeln!(f)?;
    writeln!(f, "- 规则总数：{}", summary.total_rules)?;
    writeln!(f, "- 编译失败：{}", summary.compile_failed)?;
    writeln!(f, "- 循环依赖：{}", summary.in_cycle)?;
    writeln!(f, "- 依赖循环被跳过：{}", summary.blocked_by_cycle)?;
    writeln!(f, "- 自引用：{}", summary.self_ref)?;
    writeln!(f, "- data_type 取值非法：{}", summary.data_type_invalid)?;
    writeln!(
        f,
        "- max_limit < min_limit：{}",
        summary.limits_inconsistent
    )?;
    writeln!(f, "- kks_calc 重复：{}", summary.duplicate_kks)?;
    if summary.csv_loaded {
        writeln!(f, "- CSV 真实点位数：{}", summary.csv_real_kks)?;
        writeln!(f, "- CSV 可匹配变量名数：{}", summary.csv_match_names)?;
        writeln!(f, "- 与真实 CSV 点位重名：{}", summary.csv_collisions)?;
        writeln!(
            f,
            "- 变量缺失 / 疑似写错（不是有效计算输出，且未匹配到 CSV 点位）：{}",
            summary.base_vars_missing
        )?;
    } else {
        writeln!(f, "- CSV：未加载（无法做基础变量交叉校验）")?;
    }
    writeln!(f)?;

    let mut crit: Vec<&RuleReport> = Vec::new();
    let mut high: Vec<&RuleReport> = Vec::new();
    let mut med: Vec<&RuleReport> = Vec::new();
    for r in rules {
        match t.classify(r) {
            Severity::Critical => crit.push(r),
            Severity::High => high.push(r),
            Severity::Medium => med.push(r),
            Severity::Ok => {}
        }
    }

    write_section(&mut f, "🔴 Critical", &crit, t)?;
    write_section(&mut f, "🟠 High", &high, t)?;
    write_section(&mut f, "🟡 Medium", &med, t)?;

    Ok(())
}

fn write_section<W: Write>(
    f: &mut W,
    title: &str,
    rules: &[&RuleReport],
    t: &Thresholds,
) -> std::io::Result<()> {
    writeln!(f, "## {} ({} 条)", title, rules.len())?;
    writeln!(f)?;
    if rules.is_empty() {
        writeln!(f, "_无_")?;
        writeln!(f)?;
        return Ok(());
    }
    for r in rules {
        writeln!(f, "### {}", r.name)?;
        writeln!(f)?;
        writeln!(f, "- 表达式：`{}`", r.expression)?;
        writeln!(f, "- data_type：{}", r.data_type)?;
        writeln!(f, "- 最长链深度：{}", r.max_chain_depth)?;
        if r.max_chain_depth > 0 {
            writeln!(f, "- 链路示例：{}", r.chain_example.join(" → "))?;
        }
        if let Some(err) = &r.compile_error {
            writeln!(f, "- ⚠ 编译错误：{}", err)?;
        }
        if r.is_self_ref {
            writeln!(f, "- ⚠ 自引用")?;
        }
        if r.in_cycle {
            writeln!(f, "- ⚠ 循环依赖")?;
        }
        if r.blocked_by_cycle {
            writeln!(f, "- ⚠ 依赖循环节点，主引擎会跳过此规则")?;
        }
        if r.data_type_invalid {
            writeln!(f, "- ⚠ data_type 非法")?;
        }
        if r.limits_inconsistent {
            writeln!(f, "- ⚠ max_limit < min_limit")?;
        }
        if r.duplicate_kks {
            writeln!(f, "- ⚠ kks_calc 重复")?;
        }
        if r.collides_with_csv {
            writeln!(f, "- ⚠ 与 CSV 数据点重名")?;
        }
        if !r.missing_in_csv.is_empty() {
            writeln!(
                f,
                "- ⚠ 变量缺失 / 疑似写错（不是有效计算输出，且未匹配到 CSV 点位）：{}",
                r.missing_in_csv.join(", ")
            )?;
        }
        let suggestions = suggest(r, t);
        if !suggestions.is_empty() {
            writeln!(f)?;
            writeln!(f, "**建议：**")?;
            writeln!(f)?;
            for s in suggestions {
                writeln!(f, "- **{}** —— {}", s.title, s.detail.replace('\n', " "))?;
            }
        }
        writeln!(f)?;
    }
    Ok(())
}

// ============================================================
//  JSON
// ============================================================

/// JSON 报告 schema 版本。字段语义改动时递增。
pub const JSON_SCHEMA_VERSION: u32 = 3;

#[derive(Serialize)]
struct JsonReport<'a> {
    schema_version: u32,
    summary: &'a GlobalSummary,
    thresholds: &'a Thresholds,
    rules: Vec<JsonRule<'a>>,
}

#[derive(Serialize)]
struct JsonRule<'a> {
    name: &'a str,
    expression: &'a str,
    data_type: &'a str,
    default_value: f64,
    max_limit: f64,
    min_limit: f64,
    severity: &'static str,
    direct_deps: &'a [String],
    rule_deps: &'a [String],
    base_deps: &'a [String],
    max_chain_depth: usize,
    chain_example: &'a [String],
    dependents: &'a [String],
    in_cycle: bool,
    blocked_by_cycle: bool,
    is_self_ref: bool,
    compile_error: Option<&'a str>,
    data_type_invalid: bool,
    limits_inconsistent: bool,
    duplicate_kks: bool,
    collides_with_csv: bool,
    missing_in_csv: &'a [String],
}

pub fn export_json(
    rules: &[RuleReport],
    summary: &GlobalSummary,
    t: &Thresholds,
    path: &Path,
) -> std::io::Result<()> {
    let json_rules: Vec<JsonRule> = rules
        .iter()
        .map(|r| JsonRule {
            name: &r.name,
            expression: &r.expression,
            data_type: &r.data_type,
            default_value: r.default_value,
            max_limit: r.max_limit,
            min_limit: r.min_limit,
            severity: severity_label(t.classify(r)),
            direct_deps: &r.direct_deps,
            rule_deps: &r.rule_deps,
            base_deps: &r.base_deps,
            max_chain_depth: r.max_chain_depth,
            chain_example: &r.chain_example,
            dependents: &r.dependents,
            in_cycle: r.in_cycle,
            blocked_by_cycle: r.blocked_by_cycle,
            is_self_ref: r.is_self_ref,
            compile_error: r.compile_error.as_deref(),
            data_type_invalid: r.data_type_invalid,
            limits_inconsistent: r.limits_inconsistent,
            duplicate_kks: r.duplicate_kks,
            collides_with_csv: r.collides_with_csv,
            missing_in_csv: &r.missing_in_csv,
        })
        .collect();
    let payload = JsonReport {
        schema_version: JSON_SCHEMA_VERSION,
        summary,
        thresholds: t,
        rules: json_rules,
    };
    let s = serde_json::to_string_pretty(&payload)
        .map_err(|e| std::io::Error::new(std::io::ErrorKind::Other, e))?;
    let mut f = File::create(path)?;
    f.write_all(s.as_bytes())?;
    Ok(())
}

fn severity_label(s: Severity) -> &'static str {
    match s {
        Severity::Ok => "ok",
        Severity::Medium => "medium",
        Severity::High => "high",
        Severity::Critical => "critical",
    }
}
