//! 读取 calc.toml、解析表达式变量、构建规则引用图并计算最长链条。
//!
//! 复用 `rcd_server::calc_engine` 的 tokenizer / parser，确保表达式语义
//! 与主项目 100% 一致（包括对数字开头的 KKS 标识符的支持）。

use std::collections::{HashMap, HashSet, VecDeque};
use std::fs;
use std::path::Path;

use anyhow::Context;
use rcd_server::calc_engine::config::{load_calc_config, CalcRule};
use rcd_server::calc_engine::parser::{extract_variables as parser_extract, parse};
use rcd_server::calc_engine::tokenizer::tokenize;
use rcd_server::csv_parser::Config as CsvConfig;

use crate::metrics::{GlobalSummary, RuleReport};

/// 加载结果：每条规则的报告 + 全局摘要。
pub struct AnalyzeResult {
    pub reports: Vec<RuleReport>,
    pub summary: GlobalSummary,
}

/// CSV 交叉校验索引。
///
/// - `real_kks` 只包含 CSV 真实数据点名 (`Pre + KKS`)，用于判断 `kks_calc` 是否与采集点重名。
/// - `match_names` 包含真实数据点名和测试工具支持的派生别名，用于判断表达式变量是否能匹配到 CSV。
#[derive(Debug, Clone, Default)]
pub struct CsvCrossCheckIndex {
    pub real_kks: HashSet<String>,
    pub match_names: HashSet<String>,
    pub total_records: usize,
}

/// 从指定路径加载 calc.toml，返回每条规则的分析报告。
pub fn load_and_analyze(path: &Path) -> anyhow::Result<AnalyzeResult> {
    if !path.is_file() {
        anyhow::bail!("calc.toml 文件不存在或不是文件: {}", path.display());
    }
    let cfg = load_calc_config(path)
        .map_err(|e| anyhow::anyhow!(e))
        .with_context(|| format!("读取 {:?} 失败", path))?;
    let lines = read_rule_lines(path).unwrap_or_default();
    Ok(analyze_rules(cfg.rules, None, &lines))
}

/// 加载 calc.toml + CSV 目录，返回每条规则的分析报告（含交叉校验）。
pub fn load_and_analyze_with_csv(
    calc_path: &Path,
    csv_index: &CsvCrossCheckIndex,
) -> anyhow::Result<AnalyzeResult> {
    if !calc_path.is_file() {
        anyhow::bail!("calc.toml 文件不存在或不是文件: {}", calc_path.display());
    }
    let cfg = load_calc_config(calc_path)
        .map_err(|e| anyhow::anyhow!(e))
        .with_context(|| format!("读取 {:?} 失败", calc_path))?;
    let lines = read_rule_lines(calc_path).unwrap_or_default();
    Ok(analyze_rules(cfg.rules, Some(csv_index), &lines))
}

/// 扫描 calc.toml 文本，返回每个 `[[rules]]` 块的起始行号（1-based）。
/// 顺序与 toml 反序列化得到的 `rules` 数组一致。
fn read_rule_lines(path: &Path) -> Option<Vec<usize>> {
    let content = fs::read_to_string(path).ok()?;
    let lines = content
        .lines()
        .enumerate()
        .filter_map(|(i, line)| {
            let trimmed = line.trim_start();
            // 排除注释行；`[[rules]]` 必须为一行的有效起始（toml 数组表头格式）
            if !trimmed.starts_with('#') && trimmed.starts_with("[[rules]]") {
                Some(i + 1)
            } else {
                None
            }
        })
        .collect();
    Some(lines)
}

/// 扫描指定目录第一层中的 `rcd*.csv`，返回 CSV 交叉校验索引。
///
/// 这里必须与主程序 `read_and_process_files` 保持一致：不递归扫描子目录。
pub fn scan_csv_dir(dir: &Path) -> anyhow::Result<CsvCrossCheckIndex> {
    use rcd_server::csv_parser::parse_csv;
    use rcd_server::file_scanner::read_and_process_files;

    let files = read_and_process_files(dir, r"^rcd.*\.csv$")
        .with_context(|| format!("扫描目录失败: {:?}", dir))?;
    if files.is_empty() {
        anyhow::bail!(
            "未找到 rcd*.csv（只扫描当前目录，不扫描子目录；与主程序一致）: {}",
            dir.display()
        );
    }

    let mut index = CsvCrossCheckIndex::default();
    let mut errors = Vec::new();
    for f in files {
        match parse_csv(&f) {
            Ok((conf, recs)) => {
                for r in recs {
                    let kks = r.kks;
                    if let Some(scoped) = scoped_csv_variable_name(&conf, &kks) {
                        index.match_names.insert(scoped);
                    }
                    index.real_kks.insert(kks.clone());
                    index.match_names.insert(kks);
                    index.total_records += 1;
                }
            }
            Err(e) => errors.push(format!("{:?}: {}", f, e)),
        }
    }
    if !errors.is_empty() {
        anyhow::bail!("CSV 解析失败：{}", errors.join("；"));
    }
    Ok(index)
}

fn scoped_csv_variable_name(conf: &CsvConfig, kks: &str) -> Option<String> {
    let port = normalize_port_name(&conf.com)?;
    Some(format!("{}_{}_{}", port, conf.device_id, kks))
}

fn normalize_port_name(com: &str) -> Option<String> {
    let trimmed = com.trim();
    if trimmed.is_empty() {
        return None;
    }
    let last = trimmed
        .rsplit(['/', '\\'])
        .find(|part| !part.is_empty())
        .unwrap_or(trimmed);
    let normalized: String = last
        .chars()
        .filter_map(|ch| {
            if ch.is_ascii_alphanumeric() || ch == '_' {
                Some(ch)
            } else if ch == '-' {
                Some('_')
            } else {
                None
            }
        })
        .collect();
    (!normalized.is_empty()).then_some(normalized)
}

fn analyze_rules(
    raw: Vec<CalcRule>,
    csv_index: Option<&CsvCrossCheckIndex>,
    source_lines: &[usize],
) -> AnalyzeResult {
    // 1) 用真 tokenizer / parser 抽取直接依赖；记录编译错误。
    let raw_name_set: HashSet<String> = raw.iter().map(|r| r.kks_calc.clone()).collect();

    // 重复 kks_calc 检测
    let mut seen_count: HashMap<String, usize> = HashMap::new();
    for r in &raw {
        *seen_count.entry(r.kks_calc.clone()).or_default() += 1;
    }

    let prelim: Vec<(CalcRule, Vec<String>, Option<String>)> = raw
        .into_iter()
        .map(|r| {
            let (deps, err) = extract_variables_via_engine(&r.expression);
            (r, deps, err)
        })
        .collect();
    let self_ref_by_idx: Vec<bool> = prelim
        .iter()
        .map(|(r, deps, _)| deps.iter().any(|v| v == &r.kks_calc))
        .collect();
    let topology_skipped = find_topology_skipped(&prelim, &self_ref_by_idx);

    // 主引擎会先丢弃编译失败/自引用规则，再做拓扑排序。后续“基础变量”
    // 判断必须基于最终仍会输出的规则集合，否则会漏报依赖已跳过规则的表达式。
    let surviving_names: HashSet<String> = prelim
        .iter()
        .enumerate()
        .filter(|(idx, (_, _, err))| {
            err.is_none() && !self_ref_by_idx[*idx] && !topology_skipped.contains(idx)
        })
        .map(|(_, (r, _, _))| r.kks_calc.clone())
        .collect();
    let compilable_names: HashSet<String> = prelim
        .iter()
        .enumerate()
        .filter(|(idx, (_, _, err))| err.is_none() && !self_ref_by_idx[*idx])
        .map(|(_, (r, _, _))| r.kks_calc.clone())
        .collect();

    // 2) 反向依赖映射
    let mut dependents_map: HashMap<String, Vec<String>> = HashMap::new();
    for (rule, deps, _) in &prelim {
        for d in deps {
            if raw_name_set.contains(d) && d != &rule.kks_calc {
                dependents_map
                    .entry(d.clone())
                    .or_default()
                    .push(rule.kks_calc.clone());
            }
        }
    }

    // 3) 构建邻接表（仅规则之间），检测循环
    let adj: HashMap<String, Vec<String>> = prelim
        .iter()
        .enumerate()
        .filter(|(idx, _)| {
            let (_, _, err) = &prelim[*idx];
            err.is_none() && !self_ref_by_idx[*idx]
        })
        .map(|(_, (r, deps, _))| {
            let rule_deps: Vec<String> = deps
                .iter()
                .filter(|d| compilable_names.contains(*d) && *d != &r.kks_calc)
                .cloned()
                .collect();
            (r.kks_calc.clone(), rule_deps)
        })
        .collect();
    let cyclic: HashSet<String> = find_cyclic_nodes(&adj);

    // 4) 深度备忘
    let mut depth_cache: HashMap<String, (usize, Vec<String>)> = HashMap::new();
    for (idx, (rule, _, err)) in prelim.iter().enumerate() {
        if err.is_some()
            || self_ref_by_idx[idx]
            || cyclic.contains(&rule.kks_calc)
            || topology_skipped.contains(&idx)
        {
            continue;
        }
        longest_chain(&rule.kks_calc, &adj, &cyclic, &mut depth_cache);
    }

    // 5) 组装 RuleReport
    let valid_data_types: &[&str] = &["uint", "bool", "float"];
    let mut reports: Vec<RuleReport> = prelim
        .into_iter()
        .enumerate()
        .map(|(idx, (rule, deps, compile_err))| {
            let is_self_ref = self_ref_by_idx[idx];
            let in_cycle = cyclic.contains(&rule.kks_calc);
            let blocked_by_cycle = topology_skipped.contains(&idx) && !in_cycle;
            let can_run = compile_err.is_none() && !is_self_ref && !topology_skipped.contains(&idx);
            let rule_deps: Vec<String> = deps
                .iter()
                .filter(|d| raw_name_set.contains(*d) && *d != &rule.kks_calc)
                .cloned()
                .collect();
            let base_deps: Vec<String> = deps
                .iter()
                .filter(|d| {
                    if can_run {
                        !surviving_names.contains(*d)
                    } else {
                        !raw_name_set.contains(*d)
                    }
                })
                .cloned()
                .collect();
            let (max_chain_depth, chain_example) = if compile_err.is_some() {
                (0, vec![rule.kks_calc.clone()])
            } else if is_self_ref {
                (0, vec![rule.kks_calc.clone(), "…[自引用]".into()])
            } else if in_cycle {
                (0, vec![rule.kks_calc.clone(), "…[循环]".into()])
            } else if blocked_by_cycle {
                (0, vec![rule.kks_calc.clone(), "…[依赖循环]".into()])
            } else {
                depth_cache
                    .get(&rule.kks_calc)
                    .cloned()
                    .unwrap_or((0, vec![rule.kks_calc.clone()]))
            };
            let dependents = dependents_map
                .get(&rule.kks_calc)
                .cloned()
                .unwrap_or_default();

            let dt_lc = rule.data_type.to_ascii_lowercase();
            let data_type_invalid = !valid_data_types.contains(&dt_lc.as_str());
            let limits_inconsistent = rule.max_limit < rule.min_limit;
            let duplicate_kks = seen_count.get(&rule.kks_calc).copied().unwrap_or(0) > 1;

            let (collides_with_csv, missing_in_csv) = if let Some(csv) = csv_index {
                let collides = csv.real_kks.contains(&rule.kks_calc);
                let missing: Vec<String> = if can_run {
                    base_deps
                        .iter()
                        .filter(|v| !csv.match_names.contains(v.as_str()))
                        .cloned()
                        .collect()
                } else {
                    Vec::new()
                };
                (collides, missing)
            } else {
                (false, Vec::new())
            };

            let source_line = source_lines.get(idx).copied().unwrap_or(0);
            RuleReport {
                name: rule.kks_calc,
                expression: rule.expression,
                data_type: rule.data_type,
                default_value: rule.default_value,
                max_limit: rule.max_limit,
                min_limit: rule.min_limit,
                direct_deps: deps,
                rule_deps,
                base_deps,
                max_chain_depth,
                chain_example,
                dependents,
                in_cycle,
                blocked_by_cycle,
                is_self_ref,
                compile_error: compile_err,
                data_type_invalid,
                limits_inconsistent,
                duplicate_kks,
                collides_with_csv,
                missing_in_csv,
                source_line,
            }
        })
        .collect();

    // 排序：先按名字稳定（GUI 还会按严重度重排）
    reports.sort_by(|a, b| a.name.cmp(&b.name));

    // 6) 全局摘要
    let mut summary = GlobalSummary {
        total_rules: reports.len(),
        ..Default::default()
    };
    for r in &reports {
        if r.compile_error.is_some() {
            summary.compile_failed += 1;
        }
        if r.in_cycle {
            summary.in_cycle += 1;
        }
        if r.blocked_by_cycle {
            summary.blocked_by_cycle += 1;
        }
        if r.is_self_ref {
            summary.self_ref += 1;
        }
        if r.data_type_invalid {
            summary.data_type_invalid += 1;
        }
        if r.limits_inconsistent {
            summary.limits_inconsistent += 1;
        }
        if r.duplicate_kks {
            summary.duplicate_kks += 1;
        }
    }
    if let Some(csv) = csv_index {
        summary.csv_loaded = true;
        summary.csv_real_kks = csv.real_kks.len();
        summary.csv_match_names = csv.match_names.len();
        summary.csv_collisions = reports.iter().filter(|r| r.collides_with_csv).count();
        summary.base_vars_missing = reports
            .iter()
            .flat_map(|r| r.missing_in_csv.iter())
            .collect::<HashSet<_>>()
            .len();
    }

    AnalyzeResult { reports, summary }
}

/// 用 rcd_server 的 tokenizer + parser 抽取变量，并捕获编译错误。
///
/// 与原始正则方案的区别：
/// - 支持以数字开头的标识符（如 `9CYE91GH201_SL1`），这是真实业务表达式的常态。
/// - 真正 parse 一遍，能检测语法错误（缺括号、`a = b` 这种非法 token 等）。
fn extract_variables_via_engine(expr: &str) -> (Vec<String>, Option<String>) {
    let trimmed = expr.trim();
    if trimmed.is_empty() {
        return (Vec::new(), Some("表达式为空".into()));
    }
    let tokens = match tokenize(trimmed) {
        Ok(t) => t,
        Err(e) => return (Vec::new(), Some(format!("tokenize 失败：{}", e))),
    };
    let ast = match parse(tokens) {
        Ok(a) => a,
        Err(e) => return (Vec::new(), Some(format!("parse 失败：{}", e))),
    };
    let mut vars = parser_extract(&ast);
    vars.sort();
    vars.dedup();
    (vars, None)
}

// ============================================================
//  图算法
// ============================================================

fn find_cyclic_nodes(adj: &HashMap<String, Vec<String>>) -> HashSet<String> {
    #[derive(Clone, Copy, PartialEq)]
    enum Color {
        White,
        Gray,
        Black,
    }
    let mut color: HashMap<String, Color> = adj.keys().map(|k| (k.clone(), Color::White)).collect();
    let mut in_cycle: HashSet<String> = HashSet::new();
    let keys: Vec<String> = adj.keys().cloned().collect();
    for start in keys {
        if color[&start] != Color::White {
            continue;
        }
        let mut stack: Vec<(String, usize)> = vec![(start.clone(), 0)];
        let mut path: Vec<String> = vec![start.clone()];
        color.insert(start, Color::Gray);

        while let Some((node, idx)) = stack.last().cloned() {
            let children = adj.get(&node).cloned().unwrap_or_default();
            if idx < children.len() {
                *stack.last_mut().unwrap() = (node.clone(), idx + 1);
                let child = children[idx].clone();
                match color.get(&child).copied().unwrap_or(Color::White) {
                    Color::White => {
                        color.insert(child.clone(), Color::Gray);
                        path.push(child.clone());
                        stack.push((child, 0));
                    }
                    Color::Gray => {
                        if let Some(pos) = path.iter().position(|n| n == &child) {
                            for n in &path[pos..] {
                                in_cycle.insert(n.clone());
                            }
                        }
                    }
                    Color::Black => {}
                }
            } else {
                color.insert(node.clone(), Color::Black);
                path.pop();
                stack.pop();
            }
        }
    }
    in_cycle
}

/// 复刻 `CalcEngine::topological_sort` 的跳过规则：编译成功且非自引用的规则
/// 进入 Kahn 排序，最终仍有入度的规则都会被主引擎丢弃。这些规则包括循环
/// 本身，也包括依赖循环节点的下游规则。
fn find_topology_skipped(
    prelim: &[(CalcRule, Vec<String>, Option<String>)],
    self_ref_by_idx: &[bool],
) -> HashSet<usize> {
    let compiled: Vec<(usize, &CalcRule, &[String])> = prelim
        .iter()
        .enumerate()
        .filter(|(idx, (_, _, err))| err.is_none() && !self_ref_by_idx[*idx])
        .map(|(idx, (rule, deps, _))| (idx, rule, deps.as_slice()))
        .collect();
    let n = compiled.len();
    if n == 0 {
        return HashSet::new();
    }

    let name_to_idx: HashMap<&str, usize> = compiled
        .iter()
        .enumerate()
        .map(|(i, (_, rule, _))| (rule.kks_calc.as_str(), i))
        .collect();
    let mut adj: Vec<Vec<usize>> = vec![Vec::new(); n];
    let mut in_degree = vec![0usize; n];
    for (i, (_, _, deps)) in compiled.iter().enumerate() {
        for dep in *deps {
            if let Some(&dep_idx) = name_to_idx.get(dep.as_str()) {
                adj[dep_idx].push(i);
                in_degree[i] += 1;
            }
        }
    }

    let mut queue = VecDeque::new();
    for (i, degree) in in_degree.iter().enumerate() {
        if *degree == 0 {
            queue.push_back(i);
        }
    }
    while let Some(idx) = queue.pop_front() {
        for &next in &adj[idx] {
            in_degree[next] -= 1;
            if in_degree[next] == 0 {
                queue.push_back(next);
            }
        }
    }

    in_degree
        .iter()
        .enumerate()
        .filter_map(|(i, degree)| (*degree > 0).then_some(compiled[i].0))
        .collect()
}

fn longest_chain(
    node: &str,
    adj: &HashMap<String, Vec<String>>,
    cyclic: &HashSet<String>,
    cache: &mut HashMap<String, (usize, Vec<String>)>,
) -> (usize, Vec<String>) {
    if let Some(cached) = cache.get(node) {
        return cached.clone();
    }
    let children = adj.get(node).cloned().unwrap_or_default();
    let mut best_depth = 1;
    let mut best_path = vec![node.to_string()];

    for child in &children {
        if cyclic.contains(child) {
            continue;
        }
        let (d, p) = longest_chain(child, adj, cyclic, cache);
        if d + 1 > best_depth {
            best_depth = d + 1;
            let mut new_path = vec![node.to_string()];
            new_path.extend(p);
            best_path = new_path;
        }
    }

    cache.insert(node.to_string(), (best_depth, best_path.clone()));
    (best_depth, best_path)
}

// ============================================================
//  单元测试
// ============================================================

#[cfg(test)]
mod tests {
    use super::*;

    fn rule(kks: &str, expr: &str) -> CalcRule {
        CalcRule {
            kks_calc: kks.into(),
            data_type: "float".into(),
            expression: expr.into(),
            default_value: 0.0,
            max_limit: f64::MAX,
            min_limit: f64::MIN,
        }
    }

    fn csv_index(real: &[&str], aliases: &[&str]) -> CsvCrossCheckIndex {
        let mut index = CsvCrossCheckIndex::default();
        for name in real {
            index.real_kks.insert((*name).to_string());
            index.match_names.insert((*name).to_string());
            index.total_records += 1;
        }
        for name in aliases {
            index.match_names.insert((*name).to_string());
        }
        index
    }

    /// 测试辅助：用 `analyze_rules` 的简化签名（不关心 source_line）。
    fn analyze(rules: Vec<CalcRule>, csv: Option<&CsvCrossCheckIndex>) -> AnalyzeResult {
        analyze_rules(rules, csv, &[])
    }

    #[test]
    fn depth_linear_chain() {
        let res = analyze(
            vec![
                rule("A", "B + 1"),
                rule("B", "C * 2"),
                rule("C", "base + 1"),
            ],
            None,
        );
        let m: HashMap<_, _> = res.reports.iter().map(|r| (r.name.as_str(), r)).collect();
        assert_eq!(m["C"].max_chain_depth, 1);
        assert_eq!(m["B"].max_chain_depth, 2);
        assert_eq!(m["A"].max_chain_depth, 3);
        assert_eq!(m["A"].chain_example, vec!["A", "B", "C"]);
    }

    #[test]
    fn extracts_base_vs_rule_deps() {
        let res = analyze(vec![rule("A", "B + x"), rule("B", "y * 2")], None);
        let a = res.reports.iter().find(|r| r.name == "A").unwrap();
        assert_eq!(a.rule_deps, vec!["B"]);
        assert_eq!(a.base_deps, vec!["x"]);
    }

    #[test]
    fn valid_calc_output_is_legal_reference_with_csv_loaded() {
        let csv = CsvCrossCheckIndex::default();
        let res = analyze(vec![rule("A", "B + 1"), rule("B", "base")], Some(&csv));
        let a = res.reports.iter().find(|r| r.name == "A").unwrap();
        let b = res.reports.iter().find(|r| r.name == "B").unwrap();
        assert!(a.missing_in_csv.is_empty());
        assert_eq!(b.missing_in_csv, vec!["base"]);
    }

    #[test]
    fn self_reference_flagged() {
        let res = analyze(vec![rule("C", "B + C")], None);
        let c = &res.reports[0];
        assert!(c.is_self_ref);
    }

    #[test]
    fn simple_cycle_detected() {
        let res = analyze(vec![rule("A", "B + 1"), rule("B", "A + 1")], None);
        assert!(res.reports.iter().all(|r| r.in_cycle));
    }

    #[test]
    fn downstream_of_cycle_is_flagged_as_skipped() {
        let res = analyze(
            vec![rule("A", "B + 1"), rule("B", "C + 1"), rule("C", "B + 1")],
            None,
        );
        let a = res.reports.iter().find(|r| r.name == "A").unwrap();
        let b = res.reports.iter().find(|r| r.name == "B").unwrap();
        let c = res.reports.iter().find(|r| r.name == "C").unwrap();
        assert!(a.blocked_by_cycle);
        assert!(!a.in_cycle);
        assert!(b.in_cycle);
        assert!(c.in_cycle);
        assert_eq!(res.summary.blocked_by_cycle, 1);
    }

    #[test]
    fn dependency_on_compile_failed_rule_is_runtime_base_var() {
        let csv = CsvCrossCheckIndex::default();
        let res = analyze(vec![rule("A", "B + 1"), rule("B", "x = y")], Some(&csv));
        let a = res.reports.iter().find(|r| r.name == "A").unwrap();
        let b = res.reports.iter().find(|r| r.name == "B").unwrap();
        assert!(b.compile_error.is_some());
        assert!(a.base_deps.contains(&"B".to_string()));
        assert_eq!(a.missing_in_csv, vec!["B"]);
    }

    #[test]
    fn dependents_populated() {
        let res = analyze(
            vec![rule("A", "B + 1"), rule("X", "B * 2"), rule("B", "base")],
            None,
        );
        let b = res.reports.iter().find(|r| r.name == "B").unwrap();
        assert!(b.dependents.contains(&"A".to_string()));
        assert!(b.dependents.contains(&"X".to_string()));
    }

    /// 关键修复：以数字开头的 KKS（业务真实命名）现在能被正确识别。
    #[test]
    fn kks_with_leading_digit_is_recognized() {
        let res = analyze(
            vec![rule(
                "91UGM91110_Fire",
                "9CYE91CB001XQ01 + 9CYE91GH201_READY",
            )],
            None,
        );
        let r = &res.reports[0];
        assert!(
            r.direct_deps.contains(&"9CYE91CB001XQ01".to_string()),
            "应识别为单一标识符，而非被旧正则切成 'CYE91CB001XQ01'"
        );
        assert!(r.direct_deps.contains(&"9CYE91GH201_READY".to_string()));
        assert!(r.compile_error.is_none(), "合法表达式不应有编译错误");
    }

    #[test]
    fn syntax_error_recorded_as_compile_error() {
        // `a = b` 在主项目 tokenizer 里会报错（必须用 `==`）
        let res = analyze(vec![rule("X", "a = b")], None);
        let x = &res.reports[0];
        assert!(x.compile_error.is_some());
    }

    #[test]
    fn empty_expression_recorded_as_compile_error() {
        let res = analyze(vec![rule("X", "")], None);
        let x = &res.reports[0];
        assert!(x.compile_error.is_some());
    }

    #[test]
    fn pure_literal_expression_is_valid() {
        // 用户提到的"表达式直接写 2 4 不认"——本工具应能识别为合法
        let res = analyze(vec![rule("X", "2"), rule("Y", "4")], None);
        for r in &res.reports {
            assert!(
                r.compile_error.is_none(),
                "纯数字字面量应是合法表达式：{}",
                r.expression
            );
            // 没有依赖任何变量
            assert!(r.direct_deps.is_empty());
        }
    }

    #[test]
    fn duplicate_kks_flagged() {
        let res = analyze(
            vec![rule("X", "a + 1"), rule("X", "b + 2"), rule("Y", "c + 3")],
            None,
        );
        let xs: Vec<_> = res.reports.iter().filter(|r| r.name == "X").collect();
        assert!(xs.len() == 2 && xs.iter().all(|r| r.duplicate_kks));
        let y = res.reports.iter().find(|r| r.name == "Y").unwrap();
        assert!(!y.duplicate_kks);
    }

    #[test]
    fn invalid_data_type_flagged() {
        let mut r = rule("X", "a + 1");
        r.data_type = "string".into();
        let res = analyze(vec![r], None);
        assert!(res.reports[0].data_type_invalid);
    }

    #[test]
    fn inconsistent_limits_flagged() {
        let mut r = rule("X", "a + 1");
        r.max_limit = 0.0;
        r.min_limit = 100.0;
        let res = analyze(vec![r], None);
        assert!(res.reports[0].limits_inconsistent);
    }

    #[test]
    fn csv_cross_check_missing_and_collision() {
        let csv = csv_index(&["sensor_a", "X"], &[]);
        let res = analyze(
            vec![
                rule("X", "sensor_a + 1"),        // X 与 CSV 重名 → collides
                rule("Y", "sensor_a + sensor_b"), // sensor_b 不在 CSV → missing
            ],
            Some(&csv),
        );
        let x = res.reports.iter().find(|r| r.name == "X").unwrap();
        let y = res.reports.iter().find(|r| r.name == "Y").unwrap();
        assert!(x.collides_with_csv);
        assert!(!y.collides_with_csv);
        assert_eq!(y.missing_in_csv, vec!["sensor_b"]);
        assert!(x.missing_in_csv.is_empty());
    }

    #[test]
    fn scoped_csv_alias_matches_but_does_not_collide() {
        let csv = csv_index(
            &["9CYE94GH254_PUMP1_M_TEST_04KV"],
            &["ttyAP3_64_9CYE94GH254_PUMP1_M_TEST_04KV"],
        );
        let alias = "ttyAP3_64_9CYE94GH254_PUMP1_M_TEST_04KV";
        let res = analyze(vec![rule("OUT", alias)], Some(&csv));
        let r = res.reports.iter().find(|r| r.name == "OUT").unwrap();
        assert!(r.missing_in_csv.is_empty());

        let alias_output = analyze(vec![rule(alias, "1")], Some(&csv));
        let r = alias_output
            .reports
            .iter()
            .find(|r| r.name == alias)
            .unwrap();
        assert!(!r.collides_with_csv);

        let real_output = analyze(vec![rule("9CYE94GH254_PUMP1_M_TEST_04KV", "1")], Some(&csv));
        assert!(real_output.reports[0].collides_with_csv);
    }

    #[test]
    fn missing_calc_file_returns_error() {
        let path = std::env::temp_dir().join(format!(
            "nesting-check-missing-{}-{}.toml",
            std::process::id(),
            "calc"
        ));
        let _ = std::fs::remove_file(&path);
        assert!(load_and_analyze(&path).is_err());
    }

    #[test]
    fn scan_csv_dir_adds_scoped_com_device_alias() {
        let dir =
            std::env::temp_dir().join(format!("nesting-check-scoped-csv-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).expect("创建临时 CSV 目录");
        std::fs::write(
            dir.join("rcd_alias.csv"),
            "\
COM,/dev/ttyAP3,,,,,,,
BaudRate,28800,,,,,,,
IsSpecial,FALSE,,,,,,,
ComIndex,1,,,,,,,
DeviceID,64,,,,,,,
DataLen,18,,,,,,,
Pre,9CYE94GH254_,,,,,,,
KKS,Type,F_Type,byte_index,bit_index,Def,Max,Min,LH
PUMP1_M_TEST_04KV,bool,I,5,3,0,1,0,1
",
        )
        .expect("写入 CSV");

        let csv = scan_csv_dir(&dir).expect("扫描 CSV 目录");
        assert_eq!(csv.total_records, 1);
        assert!(
            csv.real_kks.contains("9CYE94GH254_PUMP1_M_TEST_04KV"),
            "got: {:?}",
            csv.real_kks
        );
        assert!(
            csv.match_names
                .contains("ttyAP3_64_9CYE94GH254_PUMP1_M_TEST_04KV"),
            "got: {:?}",
            csv.match_names
        );
        let res = analyze(
            vec![rule("OUT", "ttyAP3_64_9CYE94GH254_PUMP1_M_TEST_04KV")],
            Some(&csv),
        );
        assert!(res.reports[0].missing_in_csv.is_empty());
        let alias_collision = analyze(
            vec![rule("ttyAP3_64_9CYE94GH254_PUMP1_M_TEST_04KV", "1")],
            Some(&csv),
        );
        assert!(!alias_collision.reports[0].collides_with_csv);
        let real_collision = analyze(vec![rule("9CYE94GH254_PUMP1_M_TEST_04KV", "1")], Some(&csv));
        assert!(real_collision.reports[0].collides_with_csv);

        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn scan_csv_dir_does_not_scan_child_dirs() {
        let dir = std::env::temp_dir().join(format!(
            "nesting-check-nonrecursive-csv-{}",
            std::process::id()
        ));
        let nested = dir.join("nested");
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&nested).expect("创建临时 CSV 子目录");
        std::fs::write(
            nested.join("rcd_child.csv"),
            "\
com,COM3,,,,,,,
baud_rate,9600,,,,,,,
is_special,false,,,,,,,
com_index,1,,,,,,,
device_id,2,,,,,,,
data_len,8,,,,,,,
kks_prefix,PX_,,,,,,,
kks,type_,f_type,byte_index,bit_index,def,max,min,lh
A,bool,,1,0,0,1,0,1
",
        )
        .expect("写入 CSV");

        let err = scan_csv_dir(&dir).unwrap_err().to_string();
        assert!(err.contains("不扫描子目录"), "got: {err}");

        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn scan_csv_dir_ignores_bad_child_dir_when_direct_files_exist() {
        let dir =
            std::env::temp_dir().join(format!("nesting-check-direct-csv-{}", std::process::id()));
        let nested = dir.join("backup");
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&nested).expect("创建临时 CSV 子目录");
        std::fs::write(
            dir.join("rcd_root.csv"),
            "\
com,COM3,,,,,,,
baud_rate,9600,,,,,,,
is_special,false,,,,,,,
com_index,1,,,,,,,
device_id,2,,,,,,,
data_len,8,,,,,,,
kks_prefix,ROOT_,,,,,,,
kks,type_,f_type,byte_index,bit_index,def,max,min,lh
A,bool,,1,0,0,1,0,1
",
        )
        .expect("写入根目录 CSV");
        std::fs::write(nested.join("rcd_bad.csv"), "not,enough\n").expect("写入子目录坏 CSV");

        let csv = scan_csv_dir(&dir).expect("扫描 CSV 目录");
        assert_eq!(csv.total_records, 1);
        assert!(csv.real_kks.contains("ROOT_A"), "got: {:?}", csv.real_kks);
        assert!(
            csv.match_names.contains("ROOT_A"),
            "got: {:?}",
            csv.match_names
        );

        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn bad_csv_file_returns_error() {
        let dir =
            std::env::temp_dir().join(format!("nesting-check-bad-csv-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).expect("创建临时 CSV 目录");
        std::fs::write(dir.join("rcd_bad.csv"), "not,enough\n").expect("写入坏 CSV");

        let err = scan_csv_dir(&dir).unwrap_err().to_string();
        assert!(err.contains("CSV 解析失败"), "got: {err}");

        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn smoke_fixture_file() {
        let path = std::path::Path::new("test_fixtures/sample_calc.toml");
        if !path.exists() {
            eprintln!("跳过：test_fixtures/sample_calc.toml 不存在");
            return;
        }
        let res = load_and_analyze(path).expect("加载 fixture 失败");
        let by_name: HashMap<&str, &RuleReport> =
            res.reports.iter().map(|r| (r.name.as_str(), r)).collect();

        assert_eq!(by_name["mid"].max_chain_depth, 1);
        assert_eq!(by_name["sum"].max_chain_depth, 2);
        assert_eq!(by_name["total"].max_chain_depth, 3);
        assert!(by_name["C"].is_self_ref);
        assert!(by_name["B"].in_cycle);
        assert_eq!(by_name["rate_usv"].dependents, vec!["alarm_rate"]);
    }
}
