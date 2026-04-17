//! 读取 calc.toml、解析表达式变量、构建规则引用图并计算最长链条。

use std::collections::{HashMap, HashSet};
use std::fs;
use std::path::Path;

use anyhow::Context;
use regex::Regex;
use serde::Deserialize;

use crate::metrics::RuleReport;

// ============================================================
//  TOML 文件结构
// ============================================================

#[derive(Debug, Deserialize)]
struct TomlDoc {
    #[serde(default)]
    rules: Vec<TomlRule>,
}

#[derive(Debug, Deserialize)]
struct TomlRule {
    kks_calc: String,
    #[serde(default)]
    data_type: String,
    expression: String,
    #[serde(default)]
    default_value: f64,
}

// ============================================================
//  入口
// ============================================================

/// 从指定路径加载 calc.toml，返回每条规则的分析报告。
pub fn load_and_analyze(path: &Path) -> anyhow::Result<Vec<RuleReport>> {
    let content =
        fs::read_to_string(path).with_context(|| format!("读取 {:?} 失败", path))?;
    let doc: TomlDoc = toml::from_str(&content)
        .with_context(|| format!("解析 TOML 失败: {:?}", path))?;
    Ok(analyze_rules(doc.rules))
}

// ============================================================
//  分析
// ============================================================

fn analyze_rules(raw: Vec<TomlRule>) -> Vec<RuleReport> {
    // 1) 提取每条规则的直接依赖变量
    let var_re = variable_regex();
    let name_set: HashSet<String> = raw.iter().map(|r| r.kks_calc.clone()).collect();

    let mut prelim: Vec<(TomlRule, Vec<String>)> = raw
        .into_iter()
        .map(|r| {
            let deps = extract_variables(&r.expression, &var_re);
            (r, deps)
        })
        .collect();

    // 2) 识别自引用
    for (rule, deps) in &mut prelim {
        if deps.iter().any(|v| v == &rule.kks_calc) {
            // 保留在 direct_deps；self-ref 标志通过比较判断，在后面组装 RuleReport 时设置
        }
    }

    // 3) 构建反向依赖与规则依赖
    let mut dependents_map: HashMap<String, Vec<String>> = HashMap::new();
    for (rule, deps) in &prelim {
        for d in deps {
            if name_set.contains(d) && d != &rule.kks_calc {
                dependents_map
                    .entry(d.clone())
                    .or_default()
                    .push(rule.kks_calc.clone());
            }
        }
    }

    // 4) 检测循环：对每条规则跑 DFS 看能否回到自身
    let adj: HashMap<String, Vec<String>> = prelim
        .iter()
        .map(|(r, deps)| {
            let rule_deps: Vec<String> = deps
                .iter()
                .filter(|d| name_set.contains(*d) && *d != &r.kks_calc)
                .cloned()
                .collect();
            (r.kks_calc.clone(), rule_deps)
        })
        .collect();

    let cyclic: HashSet<String> = find_cyclic_nodes(&adj);

    // 5) 计算每条规则的最长链条（跳过循环节点以免死循环）
    let mut depth_cache: HashMap<String, (usize, Vec<String>)> = HashMap::new();
    for (rule, _) in &prelim {
        if cyclic.contains(&rule.kks_calc) {
            continue;
        }
        longest_chain(&rule.kks_calc, &adj, &cyclic, &mut depth_cache);
    }

    // 6) 组装 RuleReport
    prelim
        .into_iter()
        .map(|(rule, deps)| {
            let is_self_ref = deps.iter().any(|v| v == &rule.kks_calc);
            let in_cycle = cyclic.contains(&rule.kks_calc);
            let rule_deps: Vec<String> = deps
                .iter()
                .filter(|d| name_set.contains(*d) && *d != &rule.kks_calc)
                .cloned()
                .collect();
            let base_deps: Vec<String> = deps
                .iter()
                .filter(|d| !name_set.contains(*d))
                .cloned()
                .collect();
            let (max_chain_depth, chain_example) = if in_cycle {
                (0, vec![rule.kks_calc.clone(), "…[循环]".into()])
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

            RuleReport {
                name: rule.kks_calc,
                expression: rule.expression,
                data_type: rule.data_type,
                default_value: rule.default_value,
                direct_deps: deps,
                rule_deps,
                base_deps,
                max_chain_depth,
                chain_example,
                dependents,
                in_cycle,
                is_self_ref,
            }
        })
        .collect()
}

/// 正则匹配 C 风格标识符；表达式中除变量外的其他 token 被忽略。
fn variable_regex() -> Regex {
    Regex::new(r"[A-Za-z_][A-Za-z0-9_]*").expect("variable regex")
}

/// 从表达式字符串中抽出变量名，过滤掉布尔字面量和数字前缀。
fn extract_variables(expr: &str, re: &Regex) -> Vec<String> {
    let mut seen = HashSet::new();
    let mut out = Vec::new();
    for m in re.find_iter(expr) {
        let s = m.as_str();
        if is_reserved(s) {
            continue;
        }
        if seen.insert(s.to_string()) {
            out.push(s.to_string());
        }
    }
    out.sort();
    out
}

fn is_reserved(s: &str) -> bool {
    matches!(s, "true" | "false" | "and" | "or" | "not")
}

// ============================================================
//  图算法
// ============================================================

/// 用三色 DFS 找出所有落在某个循环里或在循环后裔里的节点。
/// 这里简化为：只返回"本身在循环里"的节点（真 SCC 成员）。
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
        // 迭代式 DFS
        let mut stack: Vec<(String, usize)> = vec![(start.clone(), 0)];
        let mut path: Vec<String> = vec![start.clone()];
        color.insert(start, Color::Gray);

        while let Some((node, idx)) = stack.last().cloned() {
            let children = adj.get(&node).cloned().unwrap_or_default();
            if idx < children.len() {
                // 推进到下一个孩子
                *stack.last_mut().unwrap() = (node.clone(), idx + 1);
                let child = children[idx].clone();
                match color.get(&child).copied().unwrap_or(Color::White) {
                    Color::White => {
                        color.insert(child.clone(), Color::Gray);
                        path.push(child.clone());
                        stack.push((child, 0));
                    }
                    Color::Gray => {
                        // 发现回边 → path 中从 child 到当前都在循环里
                        if let Some(pos) = path.iter().position(|n| n == &child) {
                            for n in &path[pos..] {
                                in_cycle.insert(n.clone());
                            }
                        }
                    }
                    Color::Black => {}
                }
            } else {
                // 该节点所有孩子已处理
                color.insert(node.clone(), Color::Black);
                path.pop();
                stack.pop();
            }
        }
    }
    in_cycle
}

/// 计算节点的最长链条深度与示例路径。带备忘化。
///
/// depth 定义：到基础变量之前经过的"规则节点"数。
/// 例如 A→B→C, C 只依赖基础变量：
///   - depth(C) = 1 (C 本身是一层规则)
///   - depth(B) = 2
///   - depth(A) = 3
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

    fn rule(kks: &str, expr: &str) -> TomlRule {
        TomlRule {
            kks_calc: kks.into(),
            data_type: "float".into(),
            expression: expr.into(),
            default_value: 0.0,
        }
    }

    #[test]
    fn depth_linear_chain() {
        // A→B→C; C 只依赖 base
        let reports = analyze_rules(vec![
            rule("A", "B + 1"),
            rule("B", "C * 2"),
            rule("C", "base + 1"),
        ]);
        let m: HashMap<_, _> = reports.iter().map(|r| (r.name.as_str(), r)).collect();
        assert_eq!(m["C"].max_chain_depth, 1);
        assert_eq!(m["B"].max_chain_depth, 2);
        assert_eq!(m["A"].max_chain_depth, 3);
        assert_eq!(m["A"].chain_example, vec!["A", "B", "C"]);
    }

    #[test]
    fn extracts_base_vs_rule_deps() {
        let reports = analyze_rules(vec![
            rule("A", "B + x"),
            rule("B", "y * 2"),
        ]);
        let a = reports.iter().find(|r| r.name == "A").unwrap();
        assert_eq!(a.rule_deps, vec!["B"]);
        assert_eq!(a.base_deps, vec!["x"]);
        assert!(a.direct_deps.contains(&"B".to_string()));
        assert!(a.direct_deps.contains(&"x".to_string()));
    }

    #[test]
    fn self_reference_flagged() {
        let reports = analyze_rules(vec![rule("C", "B + C")]);
        let c = &reports[0];
        assert!(c.is_self_ref);
    }

    #[test]
    fn simple_cycle_detected() {
        let reports = analyze_rules(vec![
            rule("A", "B + 1"),
            rule("B", "A + 1"),
        ]);
        assert!(reports.iter().all(|r| r.in_cycle));
    }

    #[test]
    fn dependents_populated() {
        let reports = analyze_rules(vec![
            rule("A", "B + 1"),
            rule("X", "B * 2"),
            rule("B", "base"),
        ]);
        let b = reports.iter().find(|r| r.name == "B").unwrap();
        assert!(b.dependents.contains(&"A".to_string()));
        assert!(b.dependents.contains(&"X".to_string()));
    }

    #[test]
    fn reserved_words_ignored() {
        let reports = analyze_rules(vec![rule("A", "x and true")]);
        let a = &reports[0];
        assert_eq!(a.direct_deps, vec!["x"]);
    }

    #[test]
    fn smoke_fixture_file() {
        let path = std::path::Path::new("test_fixtures/sample_calc.toml");
        if !path.exists() {
            eprintln!("跳过：test_fixtures/sample_calc.toml 不存在");
            return;
        }
        let reports = load_and_analyze(path).expect("加载 fixture 失败");
        let by_name: HashMap<&str, &RuleReport> =
            reports.iter().map(|r| (r.name.as_str(), r)).collect();

        // 深链条：total → sum → mid
        assert_eq!(by_name["mid"].max_chain_depth, 1);
        assert_eq!(by_name["sum"].max_chain_depth, 2);
        assert_eq!(by_name["total"].max_chain_depth, 3);

        // 循环与自引用
        assert!(by_name["C"].is_self_ref, "C 应被标为自引用");
        assert!(by_name["B"].in_cycle, "B 应被标为循环");

        // rate_usv 仅被 alarm_rate 引用
        assert_eq!(by_name["rate_usv"].dependents, vec!["alarm_rate"]);
    }
}
