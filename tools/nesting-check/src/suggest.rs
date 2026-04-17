//! 基于规则报告生成针对性改写建议。

use crate::metrics::{RuleReport, Thresholds};

pub struct Suggestion {
    pub title: String,
    pub detail: String,
}

pub fn suggest(r: &RuleReport, t: &Thresholds) -> Vec<Suggestion> {
    let mut out = Vec::new();

    // ---- 自引用 / 循环（致命）----
    if r.is_self_ref {
        out.push(Suggestion {
            title: "表达式引用了自身".into(),
            detail: format!(
                "规则 `{name} = {expr}` 里出现了 `{name}` 自身，无法求值。\n\
                 calc_engine 会在编译阶段跳过此规则。修正方式：\n\
                 • 改名：若你想引用的是基础变量（如来自 CSV 采集），改用不同的名字避开 kks_calc；\n\
                 • 上一轮值：如果确实需要回馈（IIR 滤波式），需要在引擎侧额外支持，当前不支持。",
                name = r.name,
                expr = r.expression
            ),
        });
    } else if r.in_cycle {
        out.push(Suggestion {
            title: "循环依赖".into(),
            detail: format!(
                "规则 `{name}` 与其他规则形成循环引用链，calc_engine 会把循环节点\n\
                 以及依赖循环节点的下游规则全部跳过。\n\
                 修正方式：打破环。通常是把其中某条规则的依赖替换为基础变量，\n\
                 或将两个规则合并为一条表达式。",
                name = r.name
            ),
        });
    }

    // ---- 链条过深 ----
    if !r.in_cycle && !r.is_self_ref {
        if r.max_chain_depth >= t.depth_critical {
            out.push(Suggestion {
                title: format!(
                    "引用链过深（深度 {}，阈值 {}）",
                    r.max_chain_depth, t.depth_critical
                ),
                detail: format!(
                    "最长链条：{}。\n\
                     建议：\n\
                     • 用内联展开把中间规则合并到顶层，减少层级；\n\
                     • 或者重新组织变量命名，让 `{}` 直接引用基础变量而非链式中转。\n\
                     深层链条不影响求值正确性，但维护成本高、读图难。",
                    r.chain_example.join(" → "),
                    r.name
                ),
            });
        } else if r.max_chain_depth >= t.depth_high {
            out.push(Suggestion {
                title: format!("引用链偏深（深度 {}）", r.max_chain_depth),
                detail: format!(
                    "当前链条：{}。\n\
                     可考虑把这条链里最简单的中间规则（如只做单位换算的）展开回来。",
                    r.chain_example.join(" → ")
                ),
            });
        }
    }

    // ---- 单规则直接依赖过多 ----
    if r.direct_deps.len() >= t.deps_high {
        out.push(Suggestion {
            title: format!(
                "依赖变量过多（{} 个，阈值 {}）",
                r.direct_deps.len(),
                t.deps_high
            ),
            detail: format!(
                "规则 `{}` 的表达式里引用了 {} 个变量。高耦合往往意味着这条规则\n\
                 在做多个不相关的聚合，可拆成多条规则后再求和。\n\
                 依赖列表：{}",
                r.name,
                r.direct_deps.len(),
                r.direct_deps.join(", ")
            ),
        });
    } else if r.direct_deps.len() >= t.deps_medium {
        out.push(Suggestion {
            title: format!("依赖变量较多（{} 个）", r.direct_deps.len()),
            detail: format!(
                "依赖列表：{}。当前尚可接受，但如果继续增加请考虑拆分。",
                r.direct_deps.join(", ")
            ),
        });
    }

    // ---- 悬空中间规则（没人引用，自己也是衍生量） ----
    if r.dependents.is_empty() && !r.rule_deps.is_empty() {
        // 顶层规则：自己是链条顶端，就是正常的
    }

    // ---- 可内联：只有一个调用者且表达式短 ----
    if r.dependents.len() == 1 && expression_is_trivial(&r.expression) {
        out.push(Suggestion {
            title: "可考虑内联到唯一调用处".into(),
            detail: format!(
                "规则 `{}` 仅被 `{}` 引用，且表达式较简单 (`{}`)。\n\
                 若没有独立的业务含义（比如需要单独发布到 OPC UA），可直接把表达式\n\
                 嵌入 `{}` 的公式里，减少 calc.toml 规则条数。",
                r.name,
                r.dependents[0],
                r.expression.trim(),
                r.dependents[0]
            ),
        });
    }

    out
}

fn expression_is_trivial(expr: &str) -> bool {
    // 启发式：无括号 + 单个 token（操作数数量 ≤ 2） → 很可能只是单位换算
    !expr.contains('(')
        && expr.len() <= 30
        && expr.split(|c: char| "+-*/%".contains(c)).count() <= 3
}
