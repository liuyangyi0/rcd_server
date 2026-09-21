# 本地产物与可恢复归档

此目录不放有效源码。除本说明外默认不入 Git，避免将二进制、测试日志、现场资料及依赖缓存混进源码提交。

2026-09-19 已完整归档：

| 现在的位置 | 原位置 / 用途 |
|---|---|
| `archive/2026-09-19/manager-output/` | 原 `manager/output/`：一次性部署脚本、源码传输包、测试日志、SSH 依赖、Word/PDF 及排版中间文件 |
| `archive/2026-09-19/manager-dist/` | 原 `manager/dist/`：0.3～0.4 系列历史 Linux 包及校验文件 |
| `archive/2026-09-19/manager-tmp/` | 原 `manager/tmp/`，空目录也保留 |
| `maintenance/layout-*.json` | 每个移动文件的旧/新路径、大小和移动前 SHA-256 |

原 Word 说明位于 `archive/2026-09-19/manager-output/word-manual-20260915/`；用户确认的朴素样式版及其它原稿都保留，没有重写文档内容。

原 SSH 辅助脚本仍与 `ssh-deps/` 放在同一归档目录，可用于追溯当时的执行环境。它及历史部署脚本可能引用旧的绝对工作路径，**不可把归档脚本直接当作当前部署入口运行**。

今后新产物建议按用途存放：`releases/<版本>/`、`runs/<日期-任务>/`、`transfer/<日期-任务>/`。`manager/dist/` 可由现有构建脚本重新生成，但不作为长期发布包仓库。不得仅凭日期自动删除；需要清理空间时先确认保留版本和备份。

2026-09-20 已将传输根目录的两个散包分别移入 `transfer/2026-09-19-layout/` 与 `transfer/2026-09-20-instruction-audit/`。第二轮工具整理的传输包与检查证据分别使用 `transfer/2026-09-20-layout/`、`runs/2026-09-20-layout/`。原归档包、Word 原稿和 SSH 辅助环境仍保留在第一次归档位置。
