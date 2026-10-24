# 历史资料，不参与当前构建

- `pre-modularization/src_backup/`：原根目录 `src_backup/`，保留八个旧 Rust 文件。当前有效实现仅在根目录 `src/`。
- `pre-modularization/REFACTORING.md`：原始重构方案，描述的是当时的七模块结构，不是当前架构。
- `calculation-reference/jisuan.txt`：原根目录计算表达式参考文本。用途不能完全确认为废弃，因此保留原内容；不自动导入、不替换 `calc.toml`。

这些文件仍可纳入 Git 历史；不是编译缓存，不做自动删除。
