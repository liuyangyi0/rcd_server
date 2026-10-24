//! 基于规则报告生成针对性改写建议。

use crate::metrics::{RuleReport, Thresholds};

pub struct Suggestion {
    pub title: String,
    pub detail: String,
}

pub fn suggest(r: &RuleReport, t: &Thresholds) -> Vec<Suggestion> {
    let mut out = Vec::new();

    // ---- 编译错误 ----
    if let Some(err) = &r.compile_error {
        out.push(Suggestion {
            title: "表达式无法被引擎编译".into(),
            detail: format!(
                "tokenize/parse 阶段失败：{}\n\
                 calc_engine 会在启动时记录 error 日志并跳过此规则。\n\
                 常见原因：\n\
                 • 用了单个 `=` 而不是 `==`；\n\
                 • 括号不匹配；\n\
                 • 表达式为空或只有空白；\n\
                 • 出现非法字符（@、#、$ 等）。",
                err
            ),
        });
    }

    // ---- data_type 取值非法 ----
    if r.data_type_invalid {
        out.push(Suggestion {
            title: "data_type 取值非法".into(),
            detail: format!(
                "当前值 `{}`。calc_engine 仅识别 `uint` / `bool` / `float`，\n\
                 其他值（包括拼写错误，如 `Uint`、`int`、`f32`）会按 `float` 处理且不报错，\n\
                 容易把语义带偏。请改为这三个值之一。",
                r.data_type
            ),
        });
    }

    // ---- max_limit < min_limit ----
    if r.limits_inconsistent {
        out.push(Suggestion {
            title: "上下限设置矛盾".into(),
            detail: format!(
                "max_limit = {}，min_limit = {}。\n\
                 calc_engine 用 `clamp(min_limit, max_limit)` 钳制结果，当 max < min 时\n\
                 行为退化（结果会被钳到 min_limit 上）。请检查配置，或省略两者使用默认无限制。",
                r.max_limit, r.min_limit
            ),
        });
    }

    // ---- kks_calc 在 calc.toml 中重复 ----
    if r.duplicate_kks {
        out.push(Suggestion {
            title: "kks_calc 在文件中重复".into(),
            detail: format!(
                "`{}` 出现在多条 [[rules]] 块里。calc_engine 会把它们都当作独立节点编译，\n\
                 但拓扑排序与 OPC UA 节点空间会以最后一条覆盖前面，造成调试困难。\n\
                 请合并表达式，或改名一条。",
                r.name
            ),
        });
    }

    // ---- 与 CSV 数据点重名 ----
    if r.collides_with_csv {
        out.push(Suggestion {
            title: "kks_calc 与 CSV 数据点同名".into(),
            detail: format!(
                "`{}` 既是计算输出，也是某个 CSV 设备数据点。\n\
                 在 OPC UA 节点空间里两者会注册到相同 BrowseName，运行时后写覆盖前写，\n\
                 客户端读到的值是无序的。请给计算输出换一个不同的名字。",
                r.name
            ),
        });
    }

    // ---- 表达式变量既不是有效计算输出，也未匹配到 CSV 点位 ----
    if !r.missing_in_csv.is_empty() {
        out.push(Suggestion {
            title: format!("{} 个运行期缺失变量", r.missing_in_csv.len()),
            detail: format!(
                "缺失变量 / 疑似写错：{}\n\
                 这些名字不是当前可输出的计算结果，也没有匹配到已加载 CSV 点位。\n\
                 运行期 calc_engine 会因 `MissingVariable` 短路，输出 default_value（{}）。\n\
                 修复路径：\n\
                 • 如果它来自下位机采集点，在对应 CSV 中补上数据点定义；\n\
                 • 如果它是中间计算量，新增或修复同名 `kks_calc` 的计算规则；\n\
                 • 或改写表达式，引用已有且有效的变量名。",
                r.missing_in_csv.join(", "),
                r.default_value
            ),
        });
    }

    // ---- 自引用 / 循环 ----
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
    if r.blocked_by_cycle {
        out.push(Suggestion {
            title: "依赖循环节点，规则会被跳过".into(),
            detail: format!(
                "规则 `{}` 本身不在环里，但它依赖的上游规则最终落入循环依赖。\n\
                 calc_engine 的拓扑排序会把这条规则和循环节点一起跳过，因此 OPC UA\n\
                 不会发布这个计算输出。请先修复上游循环，再检查这条规则是否还能正常求值。",
                r.name
            ),
        });
    }

    // ---- 链条过深 ----
    if !r.in_cycle && !r.is_self_ref && r.compile_error.is_none() {
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
    !expr.contains('(')
        && expr.len() <= 30
        && expr.split(|c: char| "+-*/%".contains(c)).count() <= 3
}
