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
    /// 配置中的上限。
    pub max_limit: f64,
    /// 配置中的下限。
    pub min_limit: f64,
    /// 表达式里出现的所有变量名（去重）。
    pub direct_deps: Vec<String>,
    /// direct_deps 中属于 calc.toml 中其他规则输出的部分。
    pub rule_deps: Vec<String>,
    /// direct_deps 中不是引擎最终可输出规则的部分 → 运行期基础变量（来自 CSV 采集）。
    pub base_deps: Vec<String>,
    /// 到达基础变量的最长引用链深度，按链条中的规则节点数计数，包含自身。
    /// 例如 `A = base_x + 1` 的 depth = 1；A→B→C 且 C 引用基础变量时 depth = 3。
    pub max_chain_depth: usize,
    /// 最长链条示例，按从 self 到叶子的顺序，如 ["A", "B", "C"]。
    pub chain_example: Vec<String>,
    /// 被哪些规则直接引用（反向依赖）。
    pub dependents: Vec<String>,
    /// 是否在某个循环里。
    pub in_cycle: bool,
    /// 不在循环本身，但依赖循环节点，主引擎拓扑排序时会一起跳过。
    pub blocked_by_cycle: bool,
    /// 表达式是否引用了自身。
    pub is_self_ref: bool,
    /// 表达式编译错误（tokenize / parse 阶段）。`None` 表示正常。
    pub compile_error: Option<String>,
    /// data_type 字段是否合法（仅允许 `"uint"` / `"bool"` / `"float"`）。
    pub data_type_invalid: bool,
    /// `max_limit < min_limit` 配置错误（始终触发 clamp 到边界）。
    pub limits_inconsistent: bool,
    /// kks_calc 在整个 calc.toml 中出现 >1 次。
    pub duplicate_kks: bool,
    /// kks_calc 与 CSV 中的某条数据点 KKS 重名（OPC 节点会冲突）。
    pub collides_with_csv: bool,
    /// `base_deps` 中既不是有效计算输出，也未匹配到 CSV 点位的变量。
    /// 加载 CSV 之前为空。
    pub missing_in_csv: Vec<String>,
    /// 该规则在 calc.toml 中的起始行号（`[[rules]]` 那一行，1-based）。
    /// 0 表示未知。
    pub source_line: usize,
}

/// 所有可能的问题类型：(英文 key, 中文 label)。
/// 顺序决定 CLI/GUI 显示顺序、chip 排列与 JSON 输出。
/// 单一来源——`flag_pairs` 与 `chip_counts` 都基于此迭代。
pub const ISSUE_LABELS: &[(&str, &str)] = &[
    ("self_ref", "自引用"),
    ("cycle", "循环"),
    ("cycle_dep", "依赖循环"),
    ("compile_err", "编译错"),
    ("data_type", "data_type"),
    ("max<min", "max<min"),
    ("dup_kks", "KKS 重复"),
    ("csv_collide", "CSV 重名"),
    ("base_missing", "变量缺失"),
];

impl RuleReport {
    /// 返回触发的所有警告短标签（英文，用于 CLI 输出与 JSON）。
    pub fn flag_labels(&self) -> Vec<&'static str> {
        ISSUE_LABELS
            .iter()
            .filter(|(k, _)| self.has_issue(k))
            .map(|(k, _)| *k)
            .collect()
    }

    /// 返回触发的所有警告短标签（中文，用于 GUI 列表）。
    pub fn flag_labels_cn(&self) -> Vec<&'static str> {
        ISSUE_LABELS
            .iter()
            .filter(|(k, _)| self.has_issue(k))
            .map(|(_, l)| *l)
            .collect()
    }

    /// 是否触发指定 issue。`key` 应该是 `ISSUE_LABELS` 里某个英文键。
    /// 自引用与循环互斥（self_ref 优先），其它独立。
    pub fn has_issue(&self, key: &str) -> bool {
        match key {
            "self_ref" => self.is_self_ref,
            "cycle" => !self.is_self_ref && self.in_cycle,
            "cycle_dep" => self.blocked_by_cycle,
            "compile_err" => self.compile_error.is_some(),
            "data_type" => self.data_type_invalid,
            "max<min" => self.limits_inconsistent,
            "dup_kks" => self.duplicate_kks,
            "csv_collide" => self.collides_with_csv,
            "base_missing" => !self.missing_in_csv.is_empty(),
            _ => false,
        }
    }
}

/// 整个 calc.toml 的全局校验摘要。
#[derive(Debug, Clone, Default, Serialize)]
pub struct GlobalSummary {
    pub total_rules: usize,
    pub compile_failed: usize,
    pub in_cycle: usize,
    pub blocked_by_cycle: usize,
    pub self_ref: usize,
    pub data_type_invalid: usize,
    pub limits_inconsistent: usize,
    pub duplicate_kks: usize,
    /// 是否已加载 CSV（决定下面字段是否有意义）。
    pub csv_loaded: bool,
    /// CSV 真实数据点数量，仅包含 `Pre + KKS`。
    pub csv_real_kks: usize,
    /// CSV 可匹配变量名数量：包含 `Pre + KKS` 和 `COM_DeviceID_PreKKS` 派生别名。
    pub csv_match_names: usize,
    pub csv_collisions: usize,
    pub base_vars_missing: usize,
}

/// 严重度分级。变体顺序代表大小：`Ok < Medium < High < Critical`。
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, PartialOrd, Ord)]
pub enum Severity {
    Ok,
    Medium,
    High,
    Critical,
}

impl Severity {
    pub fn label(self) -> &'static str {
        match self {
            Severity::Ok => "OK",
            Severity::Medium => "MEDIUM",
            Severity::High => "HIGH",
            Severity::Critical => "CRITICAL",
        }
    }

    pub fn description(self) -> &'static str {
        match self {
            Severity::Ok => "OK：未触发当前阈值或结构问题。",
            Severity::Medium => {
                "MEDIUM：结构复杂度偏高，例如引用链较深或直接依赖较多，通常不影响运行，但建议关注。"
            }
            Severity::High => {
                "HIGH：运行期可能回退到 default_value，或结构风险较高，需要优先确认。"
            }
            Severity::Critical => {
                "CRITICAL：主引擎会跳过、OPC 节点会冲突，或配置本身明显错误，需要修复。"
            }
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
        // 编译失败 / 循环 / 自引用 / 字段错误 / 命名冲突 → 直接 Critical
        if r.compile_error.is_some()
            || r.in_cycle
            || r.blocked_by_cycle
            || r.is_self_ref
            || r.data_type_invalid
            || r.limits_inconsistent
            || r.duplicate_kks
            || r.collides_with_csv
        {
            return Severity::Critical;
        }
        // 表达式变量既不是有效计算输出，也未匹配到 CSV 点位 → High。
        if !r.missing_in_csv.is_empty() {
            return Severity::High;
        }

        let depth_sev = bucket3(
            r.max_chain_depth,
            self.depth_medium,
            self.depth_high,
            self.depth_critical,
        );
        let deps_sev = bucket2(r.direct_deps.len(), self.deps_medium, self.deps_high);
        [depth_sev, deps_sev]
            .into_iter()
            .max()
            .unwrap_or(Severity::Ok)
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
