//! egui 窗口：加载 calc.toml，展示每条规则的依赖链深度和建议。

use std::path::PathBuf;

use eframe::{egui, App, CreationContext, Frame};
use serde::{Deserialize, Serialize};

use crate::metrics::{RuleReport, Severity, Thresholds};
use crate::rule_config::load_and_analyze;
use crate::suggest::suggest;

// ============================================================
//  持久化
// ============================================================

#[derive(Default, Serialize, Deserialize)]
struct PersistedState {
    last_file: Option<PathBuf>,
    thresholds: Thresholds,
}

// ============================================================
//  主 App
// ============================================================

pub struct RuleApp {
    persisted: PersistedState,
    rules: Vec<RuleReport>,
    selected: Option<usize>,
    search: String,
    only_issues: bool,
    load_status: Option<String>,
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
            selected: None,
            search: String::new(),
            only_issues: false,
            load_status: None,
        };

        if let Some(p) = app.persisted.last_file.clone() {
            if p.is_file() {
                app.load(&p);
            }
        }
        app
    }

    fn load(&mut self, path: &std::path::Path) {
        match load_and_analyze(path) {
            Ok(rules) => {
                self.rules = rules;
                self.selected = self.first_highest_severity();
                self.load_status = Some(format!(
                    "已加载 {} 条规则（{}）",
                    self.rules.len(),
                    path.display()
                ));
                self.persisted.last_file = Some(path.to_path_buf());
            }
            Err(e) => {
                self.rules.clear();
                self.selected = None;
                self.load_status = Some(format!("加载失败：{}", e));
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
            if best.as_ref().map_or(true, |(_, cur)| s > *cur) {
                best = Some((i, s));
            }
        }
        best.map(|(i, _)| i)
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
        // ---- 顶部工具栏 ----
        egui::TopBottomPanel::top("toolbar").show(ctx, |ui| {
            ui.horizontal(|ui| {
                if ui.button("📂 打开 calc.toml").clicked() {
                    if let Some(p) = rfd::FileDialog::new()
                        .add_filter("TOML", &["toml"])
                        .pick_file()
                    {
                        self.load(&p);
                    }
                }
                if ui.button("🔄 重新加载").clicked() {
                    if let Some(p) = self.persisted.last_file.clone() {
                        self.load(&p);
                    }
                }
                ui.separator();
                ui.label("深度阈值:");
                ui.add(
                    egui::DragValue::new(&mut self.persisted.thresholds.depth_medium)
                        .prefix("M:")
                        .speed(1)
                        .range(1..=10),
                );
                ui.add(
                    egui::DragValue::new(&mut self.persisted.thresholds.depth_high)
                        .prefix("H:")
                        .speed(1)
                        .range(1..=15),
                );
                ui.add(
                    egui::DragValue::new(&mut self.persisted.thresholds.depth_critical)
                        .prefix("C:")
                        .speed(1)
                        .range(1..=20),
                );
                ui.separator();
                ui.checkbox(&mut self.only_issues, "仅显示问题");
            });
        });

        // ---- 状态栏 ----
        egui::TopBottomPanel::bottom("status").show(ctx, |ui| {
            ui.horizontal(|ui| {
                let (ok, med, high, crit) = self.severity_counts();
                ui.label(format!(
                    "共 {} 条规则 · 🔴 {}  🟠 {}  🟡 {}  ✅ {}",
                    self.rules.len(),
                    crit,
                    high,
                    med,
                    ok,
                ));
                if let Some(s) = &self.load_status {
                    ui.separator();
                    ui.label(s);
                }
            });
        });

        // ---- 左侧规则列表 ----
        egui::SidePanel::left("rules")
            .resizable(true)
            .default_width(340.0)
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
            self.draw_detail(ui);
        });
    }

    fn save(&mut self, storage: &mut dyn eframe::Storage) {
        eframe::set_value(storage, eframe::APP_KEY, &self.persisted);
    }
}

impl RuleApp {
    fn draw_rule_list(&mut self, ui: &mut egui::Ui) {
        let t = self.persisted.thresholds.clone();
        let search = self.search.trim().to_ascii_lowercase();

        // 收集并按严重度 + 深度降序
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
            rows.push((i, sev));
        }
        rows.sort_by(|a, b| {
            b.1.cmp(&a.1)
                .then_with(|| self.rules[b.0].max_chain_depth.cmp(&self.rules[a.0].max_chain_depth))
        });

        for (i, sev) in rows {
            let (name, depth, deps_n, in_cycle, self_ref) = {
                let r = &self.rules[i];
                (
                    r.name.clone(),
                    r.max_chain_depth,
                    r.direct_deps.len(),
                    r.in_cycle,
                    r.is_self_ref,
                )
            };
            let selected = self.selected == Some(i);
            let flags = if self_ref {
                "[自引用]"
            } else if in_cycle {
                "[循环]"
            } else {
                ""
            };
            let label = format!(
                "{} {}  {}  (D:{} 依赖:{})",
                sev.label(),
                name,
                flags,
                depth,
                deps_n
            );
            let resp = ui.selectable_label(
                selected,
                egui::RichText::new(label).color(sev.color()),
            );
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
        let r: &RuleReport = &self.rules[i];
        let t = &self.persisted.thresholds;
        let sev = t.classify(r);

        ui.horizontal(|ui| {
            ui.heading(egui::RichText::new(format!("{} {}", sev.label(), r.name)).color(sev.color()));
            if r.is_self_ref {
                ui.label(egui::RichText::new("自引用").color(egui::Color32::from_rgb(220, 70, 70)));
            } else if r.in_cycle {
                ui.label(egui::RichText::new("循环依赖").color(egui::Color32::from_rgb(220, 70, 70)));
            }
        });

        ui.label(format!("表达式: {}", r.expression));
        ui.label(format!(
            "类型: {}  |  默认值: {}",
            if r.data_type.is_empty() { "(未指定)" } else { r.data_type.as_str() },
            r.default_value
        ));
        ui.separator();

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

                ui.label("其中基础变量:");
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

        ui.separator();

        // ---- 建议 ----
        ui.heading("改写建议");
        let suggestions = suggest(r, t);
        if suggestions.is_empty() {
            ui.label(egui::RichText::new("无触发规则，此规则结构良好 ✅").weak());
        } else {
            egui::ScrollArea::vertical()
                .auto_shrink([false; 2])
                .show(ui, |ui| {
                    for s in &suggestions {
                        ui.group(|ui| {
                            ui.label(egui::RichText::new(&s.title).strong());
                            ui.label(&s.detail);
                        });
                    }
                });
        }
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
