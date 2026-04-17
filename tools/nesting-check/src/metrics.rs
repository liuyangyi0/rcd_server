//! 规则报告数据模型与严重度分级。

use serde::{Deserialize, Serialize};

/// 单条计算规则的分析报告。
#[derive(Debug, Clone)]
pub struct RuleReport {
    /// 输出变量名 (kks_calc)。
    pub name: String,
    /// 原始表达式字符串。
    pub expression: String,
    /// 数据类型标签 ("uint" / "bool" / "float")。
    pub data_type: String,
    /// 异常回退默认值。
    pub default_value: f64,
    /// 表达式里出现的所有变量名（去重）。
    pub direct_deps: Vec<String>,
    /// direct_deps 中属于其他规则输出的部分。
    pub rule_deps: Vec<String>,
    /// direct_deps 中不是任何规则输出 → 即基础变量（来自 CSV 采集）。
    pub base_deps: Vec<String>,
    /// 到达基础变量的最长引用链深度。
    /// - 自身就是基础变量链条 → 0（如 `A = base_x + 1`，depth = 1）
    /// - 实际值 = 最长引用链中的"规则节点数"。例如 A→B→C, C 引用基础变量：depth = 2（B 和 C 是规则）。
    pub max_chain_depth: usize,
    /// 最长链条示例，按从 self 到叶子的顺序，如 ["A", "B", "C"]。
    pub chain_example: Vec<String>,
    /// 被哪些规则直接引用（反向依赖）。
    pub dependents: Vec<String>,
    /// 是否在某个循环里。
    pub in_cycle: bool,
    /// 表达式是否引用了自身。
    pub is_self_ref: bool,
}

/// 严重度分级。
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum Severity {
    Ok,
    Medium,
    High,
    Critical,
}

impl Severity {
    pub fn label(self) -> &'static str {
        match self {
            Severity::Ok => "✅",
            Severity::Medium => "🟡",
            Severity::High => "🟠",
            Severity::Critical => "🔴",
        }
    }

    pub fn color(self) -> egui::Color32 {
        match self {
            Severity::Ok => egui::Color32::from_rgb(120, 180, 120),
            Severity::Medium => egui::Color32::from_rgb(230, 200, 80),
            Severity::High => egui::Color32::from_rgb(230, 140, 60),
            Severity::Critical => egui::Color32::from_rgb(220, 70, 70),
        }
    }
}

impl PartialOrd for Severity {
    fn partial_cmp(&self, other: &Self) -> Option<std::cmp::Ordering> {
        Some(self.cmp(other))
    }
}

impl Ord for Severity {
    fn cmp(&self, other: &Self) -> std::cmp::Ordering {
        fn rank(s: Severity) -> u8 {
            match s {
                Severity::Ok => 0,
                Severity::Medium => 1,
                Severity::High => 2,
                Severity::Critical => 3,
            }
        }
        rank(*self).cmp(&rank(*other))
    }
}

/// UI 可调的阈值。
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Thresholds {
    /// 引用链深度：达到即 Medium。
    pub depth_medium: usize,
    /// 深度 High。
    pub depth_high: usize,
    /// 深度 Critical。
    pub depth_critical: usize,
    /// 单条规则的依赖变量数阈值（Medium）。
    pub deps_medium: usize,
    /// 依赖数 High。
    pub deps_high: usize,
}

impl Default for Thresholds {
    fn default() -> Self {
        Self {
            depth_medium: 2,
            depth_high: 3,
            depth_critical: 5,
            deps_medium: 4,
            deps_high: 8,
        }
    }
}

impl Thresholds {
    /// 综合所有维度给出严重度。
    pub fn classify(&self, r: &RuleReport) -> Severity {
        // 循环 / 自引用直接 Critical
        if r.in_cycle || r.is_self_ref {
            return Severity::Critical;
        }
        let depth_sev = bucket3(
            r.max_chain_depth,
            self.depth_medium,
            self.depth_high,
            self.depth_critical,
        );
        let deps_sev = bucket2(
            r.direct_deps.len(),
            self.deps_medium,
            self.deps_high,
        );
        [depth_sev, deps_sev].into_iter().max().unwrap_or(Severity::Ok)
    }
}

fn bucket3(v: usize, m: usize, h: usize, c: usize) -> Severity {
    if v >= c {
        Severity::Critical
    } else if v >= h {
        Severity::High
    } else if v >= m {
        Severity::Medium
    } else {
        Severity::Ok
    }
}

fn bucket2(v: usize, m: usize, h: usize) -> Severity {
    if v >= h {
        Severity::High
    } else if v >= m {
        Severity::Medium
    } else {
        Severity::Ok
    }
}
