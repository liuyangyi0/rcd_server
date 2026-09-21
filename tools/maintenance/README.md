# 目录维护工具

`layout-2026-09-19.json` 和 `layout-2026-09-20.json` 是已执行的迁移记录；`organize-layout.ps1` 默认预览、`-Apply` 才移动，且不覆盖、不删除。目标已存在时会停止，因此不能把历史计划当作日常清理任务重跑。脚本的 `-Plan` 参数用于指定新清单，缺省仍是第一次整理的历史清单。

`check_layout.py` 只读检查构建必要路径、文档相对链接和 Rust/Go 本地依赖。按项目约定在远端执行：

```sh
python3 tools/maintenance/check_layout.py
```

今后发布包按版本放入 `artifacts/releases/`，测试日志放入 `artifacts/runs/`，传输补丁放入 `artifacts/transfer/`。不要新增 `src_backup2`、`final-final` 或把历史源码打进发布包。
