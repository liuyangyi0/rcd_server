//! egui 窗口：加载 calc.toml + CSV 目录，展示规则报告与建议。

use std::collections::{HashMap, HashSet};
use std::path::{Path, PathBuf};
use std::sync::mpsc;
use std::time::Duration;

use eframe::{egui, App, CreationContext, Frame};
use notify_debouncer_mini::notify::{RecommendedWatcher, RecursiveMode};
use notify_debouncer_mini::{new_debouncer, DebounceEventResult, Debouncer};
use rcd_server::calc_engine::config::load_calc_config;
use rcd_server::calc_engine::engine::CalcEngine;
use rcd_server::model::Value;
use serde::{Deserialize, Serialize};

use crate::metrics::{GlobalSummary, RuleReport, Severity, Thresholds};
use crate::report::{export_json, export_markdown};
use crate::rule_config::{
    load_and_analyze, load_and_analyze_with_csv, scan_csv_dir, AnalyzeResult, CsvCrossCheckIndex,
};
use crate::suggest::suggest;

// ============================================================
//  持久化
// ============================================================

#[derive(Default, Serialize, Deserialize)]
struct PersistedState {
    last_calc_file: Option<PathBuf>,
    last_csv_dir: Option<PathBuf>,
    thresholds: Thresholds,
}

// ============================================================
//  主 App
// ============================================================

pub struct RuleApp {
    persisted: PersistedState,
    rules: Vec<RuleReport>,
    /// 名称 → rules 索引列表；rules 变更时与 rules 同步刷新。
    /// 消除多处「按名查规则」的 O(N) 线性扫描（详情跳转、反向追溯、DAG 等）。
    name_to_idx: HashMap<String, Vec<usize>>,
    summary: GlobalSummary,
    csv_kks: Option<CsvCrossCheckIndex>,
    selected: Option<usize>,
    search: String,
    only_issues: bool,
    /// Chip 过滤：非空时仅显示触发了任一指定 chip 的规则。
    /// 元素值与 `RuleReport::flag_labels()` 返回的英文标签一致。
    chip_filters: HashSet<&'static str>,
    load_status: Option<String>,
    /// Watch 模式：监听 calc.toml + CSV 目录变化自动重新加载。
    watch_enabled: bool,
    watcher: Option<Debouncer<RecommendedWatcher>>,
    watch_rx: Option<mpsc::Receiver<()>>,
    /// 试算窗口状态。
    simulator: SimulatorState,
    /// DAG 视图开关与最大层数。
    dag_open: bool,
    dag_max_depth: usize,
}

#[derive(Default)]
struct SimulatorState {
    open: bool,
    inputs: HashMap<String, f64>,
    outputs: Vec<(String, Value)>,
    last_error: Option<String>,
}

impl RuleApp {
    pub fn new(cc: &CreationContext<'_>) -> Self {
        install_cjk_font(&cc.egui_ctx);

        let persisted: PersistedState = cc
            .storage
            .and_then(|s| eframe::get_value(s, eframe::APP_KEY))
            .unwrap_or_default();

        let mut app = Self {
            persisted,
            rules: Vec::new(),
            name_to_idx: HashMap::new(),
            summary: GlobalSummary::default(),
            csv_kks: None,
            selected: None,
            search: String::new(),
            only_issues: false,
            chip_filters: HashSet::new(),
            load_status: None,
            watch_enabled: false,
            watcher: None,
            watch_rx: None,
            simulator: SimulatorState::default(),
            dag_open: false,
            dag_max_depth: 3,
        };

        // 自动加载历史记录。CSV 加载函数会在成功/失败后负责同步 calc 分析，
        // 避免启动时用旧 CSV 状态重复分析。
        let mut handled_calc = false;
        if let Some(p) = app.persisted.last_csv_dir.clone() {
            if p.is_dir() {
                app.load_csv(&p);
                handled_calc = true;
            }
        }
        if !handled_calc {
            if let Some(p) = app.persisted.last_calc_file.clone() {
                if p.is_file() {
                    app.load_calc(&p);
                }
            }
        }
        app
    }

    /// 设置 rules 同时刷新 name_to_idx，保持两者同步的唯一入口。
    fn set_rules(&mut self, rules: Vec<RuleReport>) {
        self.name_to_idx.clear();
        for (i, r) in rules.iter().enumerate() {
            self.name_to_idx.entry(r.name.clone()).or_default().push(i);
        }
        self.rules = rules;
    }

    fn first_rule_index(&self, name: &str) -> Option<usize> {
        self.name_to_idx
            .get(name)
            .and_then(|indices| indices.first())
            .copied()
    }

    fn load_calc(&mut self, path: &std::path::Path) {
        let result: anyhow::Result<AnalyzeResult> = match self.csv_kks.as_ref() {
            Some(csv) => load_and_analyze_with_csv(path, csv),
            None => load_and_analyze(path),
        };
        match result {
            Ok(res) => {
                self.summary = res.summary;
                self.persisted.last_calc_file = Some(path.to_path_buf());
                self.set_rules(res.reports);
                self.selected = self.first_highest_severity();
                self.load_status = Some(format!(
                    "已加载 {} 条规则（{}）",
                    self.rules.len(),
                    path.display()
                ));
            }
            Err(e) => {
                self.set_rules(Vec::new());
                self.summary = GlobalSummary::default();
                self.selected = None;
                self.load_status = Some(format!("加载失败：{}", e));
            }
        }
    }

    fn load_csv(&mut self, dir: &std::path::Path) -> bool {
        match scan_csv_dir(dir) {
            Ok(index) => {
                let total = index.total_records;
                let real = index.real_kks.len();
                let match_names = index.match_names.len();
                self.csv_kks = Some(index);
                self.persisted.last_csv_dir = Some(dir.to_path_buf());
                self.load_status = Some(format!(
                    "CSV 目录已加载：{} 条数据点，{} 个真实点位，{} 个可匹配变量名（只扫描当前目录：{}）",
                    total,
                    real,
                    match_names,
                    dir.display()
                ));
                // 已加载 calc.toml 时立即重算
                if let Some(p) = self.persisted.last_calc_file.clone() {
                    if p.is_file() {
                        self.load_calc(&p);
                    }
                }
                true
            }
            Err(e) => {
                self.csv_kks = None;
                self.persisted.last_csv_dir = Some(dir.to_path_buf());
                let status = format!("CSV 目录加载失败：{}；已清除 CSV 交叉校验结果", e);
                if let Some(p) = self.persisted.last_calc_file.clone() {
                    if p.is_file() {
                        self.load_calc(&p);
                    }
                }
                self.load_status = Some(status);
                false
            }
        }
    }

    /// 一次性扫描 CSV 目录 + 加载 calc.toml 并完整分析。
    /// 比 `load_csv` + `load_calc` 顺序调用少一次重复分析。
    fn load_combined(&mut self, csv_dir: &Path, calc_path: &Path) {
        let index = match scan_csv_dir(csv_dir) {
            Ok(v) => v,
            Err(e) => {
                self.load_status = Some(format!("CSV 加载失败：{}", e));
                return;
            }
        };
        let total = index.total_records;
        let real = index.real_kks.len();
        let match_names = index.match_names.len();
        self.csv_kks = Some(index);
        self.persisted.last_csv_dir = Some(csv_dir.to_path_buf());

        let csv_ref = self.csv_kks.as_ref().expect("just set above");
        match load_and_analyze_with_csv(calc_path, csv_ref) {
            Ok(res) => {
                self.summary = res.summary;
                self.persisted.last_calc_file = Some(calc_path.to_path_buf());
                self.set_rules(res.reports);
                self.selected = self.first_highest_severity();
                self.load_status = Some(format!(
                    "已加载：{} 条规则 + {} 个 CSV 真实点位 + {} 个可匹配变量名（{} 条数据点，只扫描 CSV 当前目录）",
                    self.rules.len(),
                    real,
                    match_names,
                    total,
                ));
            }
            Err(e) => {
                self.set_rules(Vec::new());
                self.summary = GlobalSummary::default();
                self.selected = None;
                self.load_status = Some(format!("calc.toml 加载失败：{}", e));
            }
        }
    }

    fn first_highest_severity(&self) -> Option<usize> {
        let t = &self.persisted.thresholds;
        let mut best: Option<(usize, Severity)> = None;
        for (i, r) in self.rules.iter().enumerate() {
            let s = t.classify(r);
            if s == Severity::Ok {
                continue;
            }
            if best.as_ref().is_none_or(|(_, cur)| s > *cur) {
                best = Some((i, s));
            }
        }
        best.map(|(i, _)| i)
    }

    /// 返回每类问题 chip 的 (英文 key, 中文 label, 触发条数)。
    /// 顺序与标签来源于 `metrics::ISSUE_LABELS`。
    fn chip_counts(&self) -> Vec<(&'static str, &'static str, usize)> {
        crate::metrics::ISSUE_LABELS
            .iter()
            .map(|(key, label)| {
                let count = self.rules.iter().filter(|r| r.has_issue(key)).count();
                (*key, *label, count)
            })
            .collect()
    }

    /// 收集"谁递归地依赖了 `name`"：BFS 展开，含层级。
    /// 用于详情面板的"反向影响"树。
    /// 返回 Vec<(name, depth_from_self)>，self 不含。
    fn collect_upstream(&self, name: &str) -> Vec<(String, usize)> {
        let mut out = Vec::new();
        let mut visited: HashSet<String> = HashSet::new();
        let mut queue: std::collections::VecDeque<(String, usize)> =
            std::collections::VecDeque::new();
        queue.push_back((name.to_string(), 0));
        visited.insert(name.to_string());

        while let Some((cur, depth)) = queue.pop_front() {
            if let Some(indices) = self.name_to_idx.get(cur.as_str()) {
                for idx in indices {
                    let r = &self.rules[*idx];
                    for dep in &r.dependents {
                        if visited.insert(dep.clone()) {
                            out.push((dep.clone(), depth + 1));
                            queue.push_back((dep.clone(), depth + 1));
                        }
                    }
                }
            }
        }
        out
    }

    /// 启停 watch：监听 last_calc_file 与 last_csv_dir，文件变更时通过 mpsc 通知主循环。
    /// 切换状态调用：开 → 关释放 watcher；关 → 开新建。
    fn toggle_watch(&mut self, ctx: &egui::Context) {
        if self.watch_enabled {
            self.watcher = None;
            self.watch_rx = None;
            self.watch_enabled = false;
            self.load_status = Some("Watch 已关闭".into());
            return;
        }
        let (tx, rx) = mpsc::channel::<()>();
        let ctx_clone = ctx.clone();
        let mut debouncer = match new_debouncer(
            Duration::from_millis(400),
            move |res: DebounceEventResult| {
                if res.is_ok() {
                    let _ = tx.send(());
                    ctx_clone.request_repaint();
                }
            },
        ) {
            Ok(d) => d,
            Err(e) => {
                self.load_status = Some(format!("Watch 启动失败：{}", e));
                return;
            }
        };
        let mut watched: Vec<String> = Vec::new();
        if let Some(p) = &self.persisted.last_calc_file {
            if let Err(e) = debouncer.watcher().watch(p, RecursiveMode::NonRecursive) {
                self.load_status = Some(format!("Watch calc.toml 失败：{}", e));
                return;
            }
            watched.push(p.display().to_string());
        }
        if let Some(d) = &self.persisted.last_csv_dir {
            if let Err(e) = debouncer.watcher().watch(d, RecursiveMode::NonRecursive) {
                self.load_status = Some(format!("Watch CSV 目录失败：{}", e));
                return;
            }
            watched.push(d.display().to_string());
        }
        if watched.is_empty() {
            self.load_status = Some("尚未选择 calc.toml / CSV 目录，无法启动 Watch".into());
            return;
        }
        self.watcher = Some(debouncer);
        self.watch_rx = Some(rx);
        self.watch_enabled = true;
        self.load_status = Some(format!("Watch 已启动：{}", watched.join("，")));
    }

    /// 收集当前 reports 的所有基础变量到 simulator.inputs（保留已有值，新变量初始 0）。
    fn refresh_simulator_inputs(&mut self) {
        let mut keys: HashSet<String> = HashSet::new();
        for r in &self.rules {
            for v in &r.base_deps {
                keys.insert(v.clone());
            }
        }
        // 移除不再相关的输入
        self.simulator.inputs.retain(|k, _| keys.contains(k));
        // 加新变量
        for k in keys {
            self.simulator.inputs.entry(k).or_insert(0.0);
        }
    }

    /// 跑一次 CalcEngine 并把结果存入 simulator.outputs。
    fn run_simulation(&mut self) {
        let Some(path) = self.persisted.last_calc_file.clone() else {
            self.simulator.last_error = Some("尚未加载 calc.toml".into());
            return;
        };
        let cfg = match load_calc_config(&path) {
            Ok(c) => c,
            Err(e) => {
                self.simulator.last_error = Some(format!("重读 calc.toml 失败：{}", e));
                return;
            }
        };
        let engine = match CalcEngine::new(cfg.rules) {
            Ok(e) => e,
            Err(e) => {
                self.simulator.last_error = Some(format!("引擎构建失败：{}", e));
                return;
            }
        };
        let outputs = engine.run_cycle(&self.simulator.inputs);
        self.simulator.outputs = outputs;
        self.simulator.last_error = None;
    }

    /// 用 toml_edit 把指定规则的 data_type 字段改为 `new_type`，写回文件并重新加载。
    /// 保留所有注释和格式。
    fn quickfix_data_type(&mut self, rule_name: &str, source_line: usize, new_type: &str) {
        let Some(path) = self.persisted.last_calc_file.clone() else {
            self.load_status = Some("尚未加载 calc.toml".into());
            return;
        };
        let result = (|| -> anyhow::Result<()> {
            let content = std::fs::read_to_string(&path)?;
            let rule_lines = rule_table_lines(&content);
            let mut doc: toml_edit::DocumentMut = content.parse()?;
            let arr = doc
                .get_mut("rules")
                .and_then(|x| x.as_array_of_tables_mut())
                .ok_or_else(|| anyhow::anyhow!("calc.toml 中找不到 [[rules]] 数组"))?;
            let mut found = false;
            let mut same_name_count = 0usize;
            for (idx, tbl) in arr.iter_mut().enumerate() {
                if tbl.get("kks_calc").and_then(|x| x.as_str()) == Some(rule_name) {
                    same_name_count += 1;
                    let line_matches =
                        source_line == 0 || rule_lines.get(idx).copied() == Some(source_line);
                    if !line_matches {
                        continue;
                    }
                    tbl["data_type"] = toml_edit::value(new_type);
                    found = true;
                    break;
                }
            }
            if !found {
                if same_name_count > 0 {
                    anyhow::bail!("找到同名规则 {}，但未匹配到源行 {}", rule_name, source_line);
                } else {
                    anyhow::bail!("未找到规则 {}", rule_name);
                }
            }
            std::fs::write(&path, doc.to_string())?;
            Ok(())
        })();
        match result {
            Ok(()) => {
                self.load_status = Some(format!(
                    "已把 {} 的 data_type 改为 \"{}\"",
                    rule_name, new_type
                ));
                self.load_calc(&path);
            }
            Err(e) => {
                self.load_status = Some(format!("Quick fix 失败：{}", e));
            }
        }
    }

    /// 主循环每帧调用：若 watcher 有事件，重新加载。
    fn poll_watch(&mut self) {
        let triggered = match self.watch_rx.as_ref() {
            Some(rx) => {
                let mut any = false;
                while rx.try_recv().is_ok() {
                    any = true;
                }
                any
            }
            None => false,
        };
        if !triggered {
            return;
        }
        // CSV 先于 calc 重算，与"一起加载"逻辑保持一致
        let mut handled_calc = false;
        if let Some(p) = self.persisted.last_csv_dir.clone() {
            handled_calc = true;
            if !self.load_csv(&p) {
                return;
            }
        }
        if !handled_calc {
            if let Some(p) = self.persisted.last_calc_file.clone() {
                self.load_calc(&p);
            }
        }
    }

    fn severity_counts(&self) -> (usize, usize, usize, usize) {
        let t = &self.persisted.thresholds;
        let mut ok = 0;
        let mut med = 0;
        let mut high = 0;
        let mut crit = 0;
        for r in &self.rules {
            match t.classify(r) {
                Severity::Ok => ok += 1,
                Severity::Medium => med += 1,
                Severity::High => high += 1,
                Severity::Critical => crit += 1,
            }
        }
        (ok, med, high, crit)
    }
}

impl App for RuleApp {
    fn update(&mut self, ctx: &egui::Context, _frame: &mut Frame) {
        // 处理 watcher 触发的事件（若开启）
        self.poll_watch();

        // ---- 顶部工具栏 ----
        egui::TopBottomPanel::top("toolbar").show(ctx, |ui| {
            ui.horizontal_wrapped(|ui| {
                if ui
                    .button("📦 选择 CSV 目录 + calc.toml")
                    .on_hover_text("第 1 步选择直接包含 rcd*.csv 的目录；不会扫描子目录，与主程序一致。第 2 步选择 calc.toml")
                    .clicked()
                {
                    match rfd::FileDialog::new()
                        .set_title("第 1 步：选择直接包含 rcd*.csv 的 CSV 目录")
                        .pick_folder()
                    {
                        Some(dir) => match rfd::FileDialog::new()
                            .set_title("第 2 步：选择 calc.toml 文件")
                            .add_filter("TOML", &["toml"])
                            .pick_file()
                        {
                            Some(file) => self.load_combined(&dir, &file),
                            None => self.load_status = Some("已取消（calc.toml 未选）".into()),
                        },
                        None => self.load_status = Some("已取消（CSV 目录未选）".into()),
                    }
                }
                if ui.button("📂 打开 calc.toml").clicked() {
                    if let Some(p) = rfd::FileDialog::new()
                        .set_title("选择 calc.toml 文件")
                        .add_filter("TOML", &["toml"])
                        .pick_file()
                    {
                        self.load_calc(&p);
                    }
                }
                if ui.button("📁 打开 CSV 目录").clicked() {
                    if let Some(p) = rfd::FileDialog::new()
                        .set_title("选择直接包含 rcd*.csv 的 CSV 目录")
                        .pick_folder()
                    {
                        self.load_csv(&p);
                    }
                }
                if ui.button("🔄 重新加载").clicked() {
                    let mut handled_calc = false;
                    let mut csv_failed = false;
                    if let Some(p) = self.persisted.last_csv_dir.clone() {
                        handled_calc = true;
                        csv_failed = !self.load_csv(&p);
                    }
                    if !csv_failed && !handled_calc {
                        if let Some(p) = self.persisted.last_calc_file.clone() {
                            self.load_calc(&p);
                        }
                    }
                }
                let watch_label = if self.watch_enabled {
                    "👁 Watch (开)"
                } else {
                    "👁 Watch"
                };
                if ui
                    .button(watch_label)
                    .on_hover_text("文件变化时自动重新加载分析")
                    .clicked()
                {
                    self.toggle_watch(ctx);
                }
                if ui
                    .button("🧪 试算")
                    .on_hover_text("输入运行期输入变量值，跑一次 CalcEngine 看每条规则输出")
                    .clicked()
                {
                    self.simulator.open = !self.simulator.open;
                    if self.simulator.open {
                        // 首次打开自动 collect 基础变量并初始化 inputs（保留旧值）
                        self.refresh_simulator_inputs();
                    }
                }
                if ui
                    .button("📊 DAG")
                    .on_hover_text("选中规则的依赖图（向下到运行期输入变量）")
                    .clicked()
                {
                    self.dag_open = !self.dag_open;
                }
                ui.separator();
                ui.label("深度阈值:")
                    .on_hover_text("按最长引用链深度分级：M=MEDIUM，H=HIGH，C=CRITICAL");
                ui.add(
                    egui::DragValue::new(&mut self.persisted.thresholds.depth_medium)
                        .prefix("M:")
                        .speed(1)
                        .range(1..=10),
                )
                .on_hover_text("达到该深度后标记为 MEDIUM。默认 2。");
                ui.add(
                    egui::DragValue::new(&mut self.persisted.thresholds.depth_high)
                        .prefix("H:")
                        .speed(1)
                        .range(1..=15),
                )
                .on_hover_text("达到该深度后标记为 HIGH。默认 3。");
                ui.add(
                    egui::DragValue::new(&mut self.persisted.thresholds.depth_critical)
                        .prefix("C:")
                        .speed(1)
                        .range(1..=20),
                )
                .on_hover_text("达到该深度后标记为 CRITICAL。默认 5。");
                ui.separator();
                ui.checkbox(&mut self.only_issues, "仅显示问题");
                ui.separator();
                ui.menu_button("📤 导出报告", |ui| {
                    if ui.button("Markdown (.md)").clicked() {
                        if let Some(p) = rfd::FileDialog::new()
                            .add_filter("Markdown", &["md"])
                            .set_file_name("calc_report.md")
                            .save_file()
                        {
                            match export_markdown(
                                &self.rules,
                                &self.summary,
                                &self.persisted.thresholds,
                                &p,
                            ) {
                                Ok(_) => {
                                    self.load_status =
                                        Some(format!("已导出 Markdown：{}", p.display()))
                                }
                                Err(e) => self.load_status = Some(format!("导出失败：{}", e)),
                            }
                        }
                        ui.close_menu();
                    }
                    if ui.button("JSON (.json)").clicked() {
                        if let Some(p) = rfd::FileDialog::new()
                            .add_filter("JSON", &["json"])
                            .set_file_name("calc_report.json")
                            .save_file()
                        {
                            match export_json(
                                &self.rules,
                                &self.summary,
                                &self.persisted.thresholds,
                                &p,
                            ) {
                                Ok(_) => {
                                    self.load_status = Some(format!("已导出 JSON：{}", p.display()))
                                }
                                Err(e) => self.load_status = Some(format!("导出失败：{}", e)),
                            }
                        }
                        ui.close_menu();
                    }
                });
            });

            // ---- 第二行：问题类型 chip 过滤 ----
            ui.horizontal_wrapped(|ui| {
                ui.label("过滤:");
                let counts = self.chip_counts();
                for (label, label_cn, count) in counts {
                    let on = self.chip_filters.contains(label);
                    let text = if count > 0 {
                        format!("{} ({})", label_cn, count)
                    } else {
                        label_cn.to_string()
                    };
                    let resp = ui.selectable_label(on, text);
                    if resp.clicked() {
                        if on {
                            self.chip_filters.remove(label);
                        } else {
                            self.chip_filters.insert(label);
                        }
                    }
                }
                if !self.chip_filters.is_empty() && ui.button("✖ 清空").clicked() {
                    self.chip_filters.clear();
                }
            });
        });

        // ---- 状态栏 ----
        egui::TopBottomPanel::bottom("status").show(ctx, |ui| {
            ui.horizontal_wrapped(|ui| {
                let (ok, med, high, crit) = self.severity_counts();
                ui.label(format!(
                    "共 {} 条 · CRITICAL {} | HIGH {} | MEDIUM {} | OK {}",
                    self.rules.len(),
                    crit,
                    high,
                    med,
                    ok,
                ))
                .on_hover_text(
                    "CRITICAL：必须修复；HIGH：优先确认；MEDIUM：复杂度偏高；OK：未触发问题。",
                );
                ui.separator();
                let s = &self.summary;
                if s.csv_loaded {
                    ui.label(format!(
                        "CSV: {} 真实点位 · {} 可匹配变量名 · 变量缺失 {} · 与真实 CSV 重名 {}",
                        s.csv_real_kks,
                        s.csv_match_names,
                        s.base_vars_missing,
                        s.csv_collisions
                    ));
                    if s.base_vars_missing > 0
                        && ui
                            .button("📋 缺失变量转 CSV 模板")
                            .on_hover_text("若这些变量应来自采集点，可复制为可粘到 rcd*.csv 的行模板")
                            .clicked()
                    {
                        let mut all: Vec<String> = self
                            .rules
                            .iter()
                            .flat_map(|r| r.missing_in_csv.iter().cloned())
                            .collect();
                        all.sort();
                        all.dedup();
                        ui.ctx().copy_text(csv_template(&all));
                    }
                } else {
                    ui.label("CSV: 未加载（无法判断运行期输入是否来自采集点）");
                }
                if s.compile_failed
                    + s.in_cycle
                    + s.blocked_by_cycle
                    + s.self_ref
                    + s.data_type_invalid
                    + s.duplicate_kks
                    + s.limits_inconsistent
                    > 0
                {
                    ui.separator();
                    ui.label(format!(
                        "编译失败 {} · 循环 {} · 依赖循环 {} · 自引用 {} · data_type 错 {} · KKS 重复 {} · max<min {}",
                        s.compile_failed,
                        s.in_cycle,
                        s.blocked_by_cycle,
                        s.self_ref,
                        s.data_type_invalid,
                        s.duplicate_kks,
                        s.limits_inconsistent
                    ));
                }
                if let Some(s) = &self.load_status {
                    ui.separator();
                    ui.label(s);
                }
            });
        });

        // ---- 左侧规则列表 ----
        egui::SidePanel::left("rules")
            .resizable(true)
            .default_width(360.0)
            .show(ctx, |ui| {
                ui.heading("规则列表");
                ui.horizontal(|ui| {
                    ui.label("🔍");
                    ui.text_edit_singleline(&mut self.search);
                });
                ui.separator();
                egui::ScrollArea::vertical().show(ui, |ui| {
                    self.draw_rule_list(ui);
                });
            });

        // ---- 中央详情 ----
        egui::CentralPanel::default().show(ctx, |ui| {
            egui::ScrollArea::vertical()
                .id_source("detail_scroll")
                .auto_shrink([false; 2])
                .show(ui, |ui| {
                    self.draw_detail(ui);
                });
        });

        // ---- 试算浮动窗口 ----
        self.draw_simulator(ctx);

        // ---- DAG 视图 ----
        self.draw_dag(ctx);
    }

    fn save(&mut self, storage: &mut dyn eframe::Storage) {
        eframe::set_value(storage, eframe::APP_KEY, &self.persisted);
    }
}

impl RuleApp {
    fn draw_rule_list(&mut self, ui: &mut egui::Ui) {
        let t = self.persisted.thresholds.clone();
        let search = self.search.trim().to_ascii_lowercase();

        let mut rows: Vec<(usize, Severity)> = Vec::new();
        for (i, r) in self.rules.iter().enumerate() {
            let sev = t.classify(r);
            if self.only_issues && sev == Severity::Ok {
                continue;
            }
            if !search.is_empty() {
                let hay = format!("{} {}", r.name, r.expression).to_ascii_lowercase();
                if !hay.contains(&search) {
                    continue;
                }
            }
            // chip 过滤：非空时仅留下触发了任一指定 chip 的规则
            if !self.chip_filters.is_empty()
                && !r
                    .flag_labels()
                    .iter()
                    .any(|l| self.chip_filters.contains(l))
            {
                continue;
            }
            rows.push((i, sev));
        }
        rows.sort_by(|a, b| {
            b.1.cmp(&a.1).then_with(|| {
                self.rules[b.0]
                    .max_chain_depth
                    .cmp(&self.rules[a.0].max_chain_depth)
            })
        });

        for (i, sev) in rows {
            let r = &self.rules[i];
            let flags = r.flag_labels_cn();
            let flag_str = if flags.is_empty() {
                String::new()
            } else {
                format!("[{}]", flags.join(","))
            };
            let label = format!(
                "{}  {}  {}  深度:{}  依赖:{}",
                sev.label(),
                r.name,
                flag_str,
                r.max_chain_depth,
                r.direct_deps.len()
            );
            let selected = self.selected == Some(i);
            let resp = ui
                .selectable_label(selected, egui::RichText::new(label).color(sev.color()))
                .on_hover_text(sev.description());
            if resp.clicked() {
                self.selected = Some(i);
            }
        }
    }

    fn draw_detail(&mut self, ui: &mut egui::Ui) {
        let Some(i) = self.selected else {
            ui.vertical_centered(|ui| {
                ui.add_space(80.0);
                ui.label(
                    egui::RichText::new("← 选择一条规则查看依赖链与建议")
                        .size(18.0)
                        .weak(),
                );
            });
            return;
        };
        // Clone 一次：详情视图里需要可变借用 self（quickfix），与 r 引用冲突。
        // 选中规则只 clone 一份，2000+ 条规则规模下开销可忽略。
        let r: RuleReport = self.rules[i].clone();
        let r = &r;
        let t = self.persisted.thresholds.clone();
        let t = &t;
        let sev = t.classify(r);

        let crit_color = Severity::Critical.color();
        let high_color = Severity::High.color();

        ui.horizontal(|ui| {
            ui.heading(
                egui::RichText::new(format!("[{}] {}", sev.label(), r.name)).color(sev.color()),
            );
            if r.is_self_ref {
                ui.label(egui::RichText::new("自引用").color(crit_color));
            } else if r.in_cycle {
                ui.label(egui::RichText::new("循环依赖").color(crit_color));
            } else if r.blocked_by_cycle {
                ui.label(egui::RichText::new("依赖循环").color(crit_color));
            }
            if r.compile_error.is_some() {
                ui.label(egui::RichText::new("编译失败").color(crit_color));
            }
        });

        // 文件位置（点击复制 path:line 到剪贴板，便于粘到 IDE）
        if r.source_line > 0 {
            if let Some(p) = &self.persisted.last_calc_file {
                let file_label = p
                    .file_name()
                    .and_then(|s| s.to_str())
                    .unwrap_or("calc.toml");
                let location = format!("{}:{}", p.display(), r.source_line);
                ui.horizontal(|ui| {
                    ui.label(format!("📍 {}:{}", file_label, r.source_line));
                    if ui.button("📋 复制路径").on_hover_text(&location).clicked() {
                        ui.ctx().copy_text(location);
                    }
                });
            }
        }

        ui.label(format!("表达式: {}", r.expression));
        ui.label(format!(
            "类型: {}  |  默认值: {}  |  上限: {}  |  下限: {}",
            if r.data_type.is_empty() {
                "(未指定)"
            } else {
                r.data_type.as_str()
            },
            r.default_value,
            fmt_limit(r.max_limit),
            fmt_limit(r.min_limit)
        ));
        ui.separator();

        if let Some(err) = &r.compile_error {
            ui.colored_label(crit_color, format!("⚠ 编译错误：{}", err));
        }
        if r.data_type_invalid {
            ui.colored_label(
                crit_color,
                format!(
                    "⚠ data_type 不合法：{}（仅允许 uint/bool/float）",
                    r.data_type
                ),
            );
            // 大小写常错（如 "Uint"）→ 推荐对应小写；否则给三选一
            let lc = r.data_type.to_ascii_lowercase();
            let suggested = ["uint", "bool", "float"]
                .iter()
                .find(|t| **t == lc.as_str())
                .copied();
            let mut chosen: Option<&'static str> = None;
            ui.horizontal(|ui| {
                ui.label("Quick fix:");
                if let Some(t) = suggested {
                    if ui.button(format!("📝 修正为 \"{}\"", t)).clicked() {
                        chosen = Some(t);
                    }
                } else {
                    for t in ["uint", "bool", "float"] {
                        if ui.button(format!("📝 改为 {}", t)).clicked() {
                            chosen = Some(t);
                        }
                    }
                }
            });
            if let Some(t) = chosen {
                let name = r.name.clone();
                self.quickfix_data_type(&name, r.source_line, t);
            }
        }
        if r.limits_inconsistent {
            ui.colored_label(crit_color, "⚠ max_limit < min_limit，运行期会被钳制到边界");
        }
        if r.duplicate_kks {
            ui.colored_label(
                crit_color,
                "⚠ kks_calc 在 calc.toml 中重复，引擎将以最后一条为准",
            );
        }
        if r.blocked_by_cycle {
            ui.colored_label(crit_color, "⚠ 依赖循环节点，主引擎拓扑排序时会跳过此规则");
        }
        if r.collides_with_csv {
            ui.colored_label(crit_color, "⚠ kks_calc 与 CSV 数据点重名，OPC 节点将冲突");
        }
        if !r.missing_in_csv.is_empty() {
            ui.colored_label(
                high_color,
                format!(
                    "⚠ 变量缺失 / 疑似写错（既不是有效计算输出，也未匹配到 CSV 点位）：{}",
                    r.missing_in_csv.join(", ")
                ),
            );
            if ui
                .button("📋 复制为 CSV 模板")
                .on_hover_text(
                    "仅当这些变量应来自采集点时使用；type/byte_index 为占位值，需手工调整",
                )
                .clicked()
            {
                ui.ctx().copy_text(csv_template(&r.missing_in_csv));
            }
        }

        // ---- 指标面板 ----
        egui::Grid::new("metrics")
            .num_columns(2)
            .spacing([24.0, 6.0])
            .show(ui, |ui| {
                ui.label("最长链深度:");
                ui.label(format!("{}", r.max_chain_depth));
                ui.end_row();

                ui.label("最长链示例:");
                ui.label(r.chain_example.join(" → "));
                ui.end_row();

                ui.label("直接依赖:");
                ui.label(format!("{} 个", r.direct_deps.len()));
                ui.end_row();

                ui.label("其中规则引用:");
                ui.label(if r.rule_deps.is_empty() {
                    "无".to_string()
                } else {
                    r.rule_deps.join(", ")
                });
                ui.end_row();

                ui.label("其中运行期输入:");
                ui.label(if r.base_deps.is_empty() {
                    "无".to_string()
                } else {
                    r.base_deps.join(", ")
                });
                ui.end_row();

                ui.label("被谁引用:");
                ui.label(if r.dependents.is_empty() {
                    "无".to_string()
                } else {
                    r.dependents.join(", ")
                });
                ui.end_row();
            });

        // ---- 反向影响（递归展开 dependents 树）----
        let upstream = self.collect_upstream(&r.name);
        if !upstream.is_empty() {
            egui::CollapsingHeader::new(format!("🔝 反向影响：递归 {} 条上游规则", upstream.len()))
                .default_open(false)
                .show(ui, |ui| {
                    let mut clicked: Option<String> = None;
                    for (name, depth) in &upstream {
                        let indent = "  ".repeat(*depth);
                        if ui
                            .selectable_label(false, format!("{}↳ {}", indent, name))
                            .on_hover_text("点击跳转到该规则")
                            .clicked()
                        {
                            clicked = Some(name.clone());
                        }
                    }
                    if let Some(target) = clicked {
                        if let Some(idx) = self.first_rule_index(target.as_str()) {
                            self.selected = Some(idx);
                        }
                    }
                });
        }

        ui.separator();

        // ---- 建议 ----
        ui.heading("改写建议");
        let suggestions = suggest(r, t);
        if suggestions.is_empty() {
            ui.label(egui::RichText::new("无触发规则，此规则结构良好 ✓").weak());
        } else {
            for s in &suggestions {
                ui.group(|ui| {
                    ui.label(egui::RichText::new(&s.title).strong());
                    ui.label(&s.detail);
                });
            }
        }
    }
}

impl RuleApp {
    fn draw_simulator(&mut self, ctx: &egui::Context) {
        if !self.simulator.open {
            return;
        }
        let mut open = self.simulator.open;
        let mut do_run = false;
        let mut do_clear = false;
        egui::Window::new("🧪 试算（运行期输入 → 公式输出）")
            .open(&mut open)
            .resizable(true)
            .default_size([520.0, 480.0])
            .show(ctx, |ui| {
                if self.persisted.last_calc_file.is_none() {
                    ui.label("尚未加载 calc.toml");
                    return;
                }
                ui.horizontal(|ui| {
                    if ui
                        .button("▶ 运行")
                        .on_hover_text("用当前输入跑 CalcEngine::run_cycle")
                        .clicked()
                    {
                        do_run = true;
                    }
                    if ui
                        .button("🔄 刷新输入列表")
                        .on_hover_text("重新从规则收集基础变量")
                        .clicked()
                    {
                        self.refresh_simulator_inputs();
                    }
                    if ui.button("🧹 清零").clicked() {
                        do_clear = true;
                    }
                });
                if let Some(err) = &self.simulator.last_error {
                    ui.colored_label(Severity::Critical.color(), err);
                }
                ui.separator();

                egui::ScrollArea::vertical()
                    .id_source("sim_scroll")
                    .max_height(360.0)
                    .show(ui, |ui| {
                        ui.heading("运行期输入变量");
                        let mut keys: Vec<String> = self.simulator.inputs.keys().cloned().collect();
                        keys.sort();
                        for k in &keys {
                            let v = self.simulator.inputs.entry(k.clone()).or_insert(0.0);
                            ui.horizontal(|ui| {
                                ui.add_sized([260.0, 20.0], egui::Label::new(k));
                                ui.add(egui::DragValue::new(v).speed(0.1));
                            });
                        }

                        if !self.simulator.outputs.is_empty() {
                            ui.add_space(8.0);
                            ui.separator();
                            ui.heading(format!("公式输出（{} 条）", self.simulator.outputs.len()));
                            for (kks, val) in &self.simulator.outputs {
                                ui.horizontal(|ui| {
                                    ui.add_sized([260.0, 20.0], egui::Label::new(kks));
                                    ui.label(format_value(val));
                                });
                            }
                        }
                    });
            });
        self.simulator.open = open;
        if do_clear {
            for v in self.simulator.inputs.values_mut() {
                *v = 0.0;
            }
        }
        if do_run {
            self.run_simulation();
        }
    }

    /// 绘制选中规则的下游依赖图（向下到运行期输入变量），简单层次布局。
    fn draw_dag(&mut self, ctx: &egui::Context) {
        if !self.dag_open {
            return;
        }
        let mut open = self.dag_open;
        let Some(idx) = self.selected else {
            egui::Window::new("📊 DAG 视图")
                .open(&mut open)
                .show(ctx, |ui| {
                    ui.label("先在左侧列表选中一条规则");
                });
            self.dag_open = open;
            return;
        };
        let root_name = self.rules[idx].name.clone();
        let max_depth = self.dag_max_depth;
        let layers = self.compute_dag_layers(idx, max_depth);
        let mut clicked: Option<usize> = None;
        let mut new_depth = max_depth;
        egui::Window::new(format!("📊 DAG: {}", root_name))
            .open(&mut open)
            .resizable(true)
            .default_size([720.0, 480.0])
            .show(ctx, |ui| {
                ui.horizontal(|ui| {
                    ui.label("最大层数:");
                    ui.add(egui::DragValue::new(&mut new_depth).speed(1).range(1..=8));
                });
                ui.separator();
                egui::ScrollArea::both()
                    .id_source("dag_scroll")
                    .show(ui, |ui| {
                        clicked = paint_dag(ui, &layers);
                    });
            });
        self.dag_open = open;
        if new_depth != max_depth {
            self.dag_max_depth = new_depth;
        }
        if let Some(target) = clicked {
            self.selected = Some(target);
        }
    }

    /// BFS 展开依赖：每层节点列表（含运行期输入叶子）。
    /// 重复节点会重复出现（DAG 当 tree 画，简化布局）。
    /// 每个 child 节点记录所属父节点在上一层的 idx，画线时直接对位。
    fn compute_dag_layers(&self, root_idx: usize, max_depth: usize) -> Vec<Vec<DagNode>> {
        let mut layers: Vec<Vec<DagNode>> = vec![vec![DagNode {
            name: self.rules[root_idx].name.clone(),
            is_base: false,
            rule_idx: Some(root_idx),
            parent_idx: None,
        }]];
        for _ in 0..max_depth {
            let prev = &layers[layers.len() - 1];
            let mut next: Vec<DagNode> = Vec::new();
            for (pi, parent) in prev.iter().enumerate() {
                if parent.is_base {
                    continue;
                }
                if let Some(idx) = parent.rule_idx {
                    let r = &self.rules[idx];
                    for d in &r.rule_deps {
                        if let Some(indices) = self.name_to_idx.get(d.as_str()) {
                            for dep_idx in indices {
                                next.push(DagNode {
                                    name: d.clone(),
                                    is_base: false,
                                    rule_idx: Some(*dep_idx),
                                    parent_idx: Some(pi),
                                });
                            }
                        }
                    }
                    for d in &r.base_deps {
                        next.push(DagNode {
                            name: d.clone(),
                            is_base: true,
                            rule_idx: None,
                            parent_idx: Some(pi),
                        });
                    }
                }
            }
            if next.is_empty() {
                break;
            }
            layers.push(next);
        }
        layers
    }
}

#[derive(Clone)]
struct DagNode {
    name: String,
    is_base: bool,
    /// 对应 `RuleApp.rules` 的索引；基础变量为 None。
    rule_idx: Option<usize>,
    /// 父节点在上一层的 index；root 节点为 None。
    parent_idx: Option<usize>,
}

/// 把分层节点画到 ui 中：每层水平等距分布、相邻层之间画连线。
/// 返回被点击的规则索引（若有）。
fn paint_dag(ui: &mut egui::Ui, layers: &[Vec<DagNode>]) -> Option<usize> {
    let layer_height = 84.0;
    let node_w = 160.0;
    let node_h = 32.0;
    let max_per_layer = layers.iter().map(|l| l.len()).max().unwrap_or(1) as f32;
    let canvas_w = (max_per_layer * (node_w + 16.0)).max(400.0);
    let canvas_h = layers.len() as f32 * layer_height + 16.0;
    let (rect, _) = ui.allocate_exact_size(egui::vec2(canvas_w, canvas_h), egui::Sense::hover());

    let painter = ui.painter_at(rect);
    let mut positions: Vec<Vec<egui::Pos2>> = Vec::with_capacity(layers.len());
    for (li, layer) in layers.iter().enumerate() {
        let n = layer.len() as f32;
        let row_y = rect.top() + 16.0 + li as f32 * layer_height + node_h * 0.5;
        let mut row = Vec::with_capacity(layer.len());
        for (ci, _node) in layer.iter().enumerate() {
            let cx = rect.left() + canvas_w * (ci as f32 + 0.5) / n;
            row.push(egui::pos2(cx, row_y));
        }
        positions.push(row);
    }
    for (li, layer) in layers.iter().enumerate().skip(1) {
        for (ci, node) in layer.iter().enumerate() {
            let Some(pi) = node.parent_idx else { continue };
            let p = positions[li - 1][pi];
            let c = positions[li][ci];
            painter.line_segment(
                [
                    egui::pos2(p.x, p.y + node_h * 0.5),
                    egui::pos2(c.x, c.y - node_h * 0.5),
                ],
                egui::Stroke::new(1.0, egui::Color32::from_gray(120)),
            );
        }
    }

    let mut clicked_idx: Option<usize> = None;
    for (li, layer) in layers.iter().enumerate() {
        for (ci, node) in layer.iter().enumerate() {
            let center = positions[li][ci];
            let r = egui::Rect::from_center_size(center, egui::vec2(node_w, node_h));
            let fill = if node.is_base {
                egui::Color32::from_rgb(80, 110, 80)
            } else {
                egui::Color32::from_rgb(60, 80, 130)
            };
            painter.rect(
                r,
                4.0,
                fill,
                egui::Stroke::new(1.0, egui::Color32::from_gray(200)),
            );
            painter.text(
                center,
                egui::Align2::CENTER_CENTER,
                truncate_label(&node.name, 22),
                egui::FontId::proportional(12.0),
                egui::Color32::WHITE,
            );
            // 点击检测
            let resp = ui.allocate_rect(r, egui::Sense::click());
            if resp.clicked() {
                if let Some(idx) = node.rule_idx {
                    clicked_idx = Some(idx);
                }
            }
            if resp.hovered() {
                resp.on_hover_text(&node.name);
            }
        }
    }
    clicked_idx
}

fn truncate_label(s: &str, max: usize) -> String {
    if s.chars().count() <= max {
        s.to_string()
    } else {
        let mut out: String = s.chars().take(max - 1).collect();
        out.push('…');
        out
    }
}

fn rule_table_lines(content: &str) -> Vec<usize> {
    content
        .lines()
        .enumerate()
        .filter_map(|(i, line)| {
            let trimmed = line.trim_start();
            if !trimmed.starts_with('#') && trimmed.starts_with("[[rules]]") {
                Some(i + 1)
            } else {
                None
            }
        })
        .collect()
}

fn format_value(v: &Value) -> String {
    match v {
        Value::UInt(n) => format!("UInt: {}", n),
        Value::Bool(b) => format!("Bool: {}", b),
        Value::Float(f) => format!("Float: {}", f),
    }
}

/// 生成可粘贴到 `rcd*.csv` 数据区的行模板。
/// type/byte_index/bit_index 为占位值，使用方需要按实际下位机协议调整。
fn csv_template(vars: &[String]) -> String {
    let mut s =
        String::with_capacity(rcd_server::csv_parser::RECORD_CSV_HEADER.len() + vars.len() * 32);
    s.push_str(rcd_server::csv_parser::RECORD_CSV_HEADER);
    s.push('\n');
    for v in vars {
        s.push_str(v);
        s.push_str(",uint,DI,1,0,0,0,0,0\n");
    }
    s
}

fn fmt_limit(v: f64) -> String {
    if v == f64::MAX {
        "f64::MAX".into()
    } else if v == f64::MIN {
        "f64::MIN".into()
    } else {
        format!("{}", v)
    }
}

// ============================================================
//  CJK 字体
// ============================================================

fn install_cjk_font(ctx: &egui::Context) {
    use egui::{FontData, FontDefinitions, FontFamily};

    let mut fonts = FontDefinitions::default();
    for candidate in [
        r"C:\Windows\Fonts\msyh.ttc",
        r"C:\Windows\Fonts\simhei.ttf",
        r"C:\Windows\Fonts\simsun.ttc",
    ] {
        if let Ok(bytes) = std::fs::read(candidate) {
            fonts
                .font_data
                .insert("cjk".to_owned(), FontData::from_owned(bytes));
            fonts
                .families
                .get_mut(&FontFamily::Proportional)
                .unwrap()
                .insert(0, "cjk".to_owned());
            fonts
                .families
                .get_mut(&FontFamily::Monospace)
                .unwrap()
                .push("cjk".to_owned());
            ctx.set_fonts(fonts);
            return;
        }
    }
}
