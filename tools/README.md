# 开发与诊断工具

所有可执行检查按 [远端验证约定](../docs/development/agent-validation.md) 进行。下表用于查找工具，不是运行授权；读数据报文同样会占用真实串口，不能与采集程序争用。

| 目录 | 内容 | 使用边界 |
| --- | --- | --- |
| [serial/](serial/) | 校验位探针、矩阵实验、日志分析及离线测试 | `serial_parity_analyze.py` / `serial_parity_report.py` 分析已有日志；`run` / `matrix` / C 探针会访问指定串口，须单独核对目标 |
| [opc/](opc/) | OPC 客户端检查、串口端到端测试、只读观察 | `serial_opc_observe.py` 不写 OPC 值；另外两个工具含写入/Call 或启动测试实例的能力 |
| [fuzz-device/](fuzz-device/) | 设备模拟工具及 Windows/Kylin 打包入口 | 仅供隔离回归，不能代替真实设备验收 |
| [nesting-check/](nesting-check/) | 现有独立 Rust 辅助工具 | 保持自身 Cargo 根目录 |
| [maintenance/](maintenance/README.md) | 目录检查、迁移清单及可恢复整理工具 | 整理脚本默认预览；历史清单不可重复执行 |
| [release/](release/README.md) | 显式目标发布、配置保留回滚、目录迁移及实际运行版本检查 | live 操作须有当次授权与维护窗口；不默认升级 58 |
| [ansible/](ansible/README.md) | 复用 release 的巡检、分发、串行部署、验收及显式恢复 | 发布/回滚须当次授权；不接管用户配置，不自动重试未知结果 |

管理端专用工具仍在 [manager/tools](../manager/tools/README.md)，不复制第二套。

## 路径与依赖

- 上述诊断脚本原本平铺于 `tools/`，现已归类；文件名不变。外部调用请更新为 `tools/serial/...`、`tools/opc/...` 或 `tools/fuzz-device/...`。
- OPC 客户端及串口工具的基础依赖文件仍为 [requirements.txt](requirements.txt)，命令中的相对路径以仓库根为准。`serial_opc_observe.py` 另需 `asyncua`；模拟器需 Tk，打包另需 PyInstaller。未借本次整理升级依赖。
- `serial_parity_run.py` 仍在脚本同级查找 `probe` 并写实验日志，使用它时将整组 serial 工具复制到隔离运行目录；不要只复制一个 runner 而漏掉 report 模块。矩阵工具可显式指定 `--probe` 和 `--output`。
- `fuzz-device/` 的 Dockerfile 与 `fuzz_device.py` 保持同级，两个 batch 入口定位自己的目录，生成 `fuzz-device/build/`、`fuzz-device/output/`。本次未重新验收 Windows 或 Docker 打包。
- 新测试证据放 `artifacts/runs/<日期-任务>/`；发布包放 `artifacts/releases/<版本>/`，不存入工具源码目录。
