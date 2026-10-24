# 自动切源与来源差异告警验证（2026-09-20）

## 范围

在 `codex/opc-device-groups` 工作区增加组策略、Session 自动参与方式、故障确认/健康确认/冷却、独立默认源自动策略、双来源持续差异告警、Web 配置和 OPC 诊断节点。两个功能均默认关闭，差异不触发切源。操作说明见 [使用指南](../guides/redundancy.md)。

保留已有未提交修改；未提交或推送 Git。没有改 Rust 轮询逻辑、现场安全策略或现场端口。

## 验证环境与结果

所有构建、测试及 Chromium 浏览器运行均在 `192.168.10.46` 的隔离目录 `/home/lenovo/code/rcd_redundancy_20260920`，未在本机执行测试。

| 检查 | 命令或方式 | 结果 |
| --- | --- | --- |
| Go 全套回归 | `cd manager && go test ./...` | 全部通过；需要现场环境变量的 opt-in 测试默认跳过，不算现场验收 |
| 并发竞争 | `go test -race ./internal/domain ./internal/store ./internal/app ./internal/api ./internal/opc` | 五个包通过，无 race 报告；包含最后补充的候选 epoch 复查与冷却用例 |
| Web 全套 | `npm test -- --maxWorkers=2 --minWorkers=1` | 28 个文件、180 项通过 |
| Web 发布构建 | `npm run build` | TypeScript 检查和 Vite 构建通过 |
| 管理器构建 | `go build -o ../artifacts/runs/redundancy/rcd-manager ./cmd/rcd-manager` | 通过，包含新 Web 资源 |
| 浏览器 | 远端 Playwright + Chromium，独立管理器、独立数据库、随机 loopback 端口 | 登录、默认关闭、确认门槛、来源下拉、保存、关闭重开保留、无 Session 空态、桌面/窄屏截图通过；无 pageerror |
| OPC 协议 | `TestRedundancyIsolatedWire`，连接上述独立管理器 | Browse 新节点、两匿名 Session 读写参与方式隔离、订阅模式变化通过 |
| 格式 | 远端 gofmt；本机 `git diff --check` 只读差异检查 | 通过 |

首次并行 Web 全套测试出现旧 `SourceOperationsDrawer` 用例超过其 5 秒时限；限制 worker 数量后全部通过，没有放宽超时或修改该用例。既有 `CalculationEditor` 测试仍输出 React `act` 警告，测试通过，本次未扩大修改。

## 主要覆盖

- 整组故障延时、候选健康确认、切源冷却、无健康备用保持原源、恢复不自动回切。
- 未恢复的站点、过期状态和旧 epoch 样本不可作为健康备用；提交前重新检查目标连接。
- 手动 Session 隔离、手动选源退出自动、原 Session 重挂保留参与方式。
- 待处理命令阻止自动切源、模式和策略修改；默认源自动切换更新中央计算路由 revision。
- 数值容差、Boolean 对比、坏质量/时间差跳过、持续差异计时、告警不重复落库、不可比较不误恢复。
- 策略 revision/CAS、API 409/404/428/422、SQLite 持久化、告警重启恢复、删除组解除告警。
- 配置备份恢复参数但关闭自动控制开关；快照不可被调用方修改，并发读写无竞争。

## 证据及未覆盖范围

远端日志及截图：`/home/lenovo/code/rcd_redundancy_20260920/artifacts/runs/redundancy/`。

本机证据副本：仓库 `artifacts/runs/2026-09-20-redundancy/`，包含 `go-all.log`、`go-race.log`、`web-all-limited.log`、`web-build.log`、`browser-smoke.log`、`opc-wire.log`、桌面/窄屏 PNG；首次失败日志 `web-all.log` 也保留。

浏览器测试仅创建两个停用来源，未连接硬件，结束时已停止该独立管理器。纯逻辑测试使用确定性输入，不代表真实串口故障切源验收。

**未部署到现场运行服务，未对真实 OPC Session 或默认来源切源；未做 20×5300 点负载、延迟 SLA、长时间 soak 或 Windows 验收。** 现场启用前需要确认每组物理设备映射和候选优先级，并在明确授权的维护窗口验证真实故障、命令锁和恢复行为。

## 后续授权部署（2026-09-20 16:52，Asia/Shanghai）

上述“未部署”是初次开发交付时的状态。用户随后明确要求部署，现已更新 `192.168.10.46` 的 `rcd-manager.service`：

- 版本：`0.7.5-redundancy.1`，构建标识 `6f0dd6a-working-tree`。
- 固定入口：`/home/lenovo/.local/bin/RcdManager`；目标为 `/home/lenovo/code/rcd_device_groups_20260828/bin/RcdManager-0.7.5-redundancy.1-linux-amd64`。
- 二进制 SHA-256：`80743830cf497644883eb0033e7cea9b07f611b337bf98b90bf4f002e4dddba4`。与已复测的远端发布产物一致。
- 备份：`/home/lenovo/code/rcd_device_groups_20260828/backups/manager-redundancy-20260920-165242/`，包含旧二进制、停止管理器后的 SQLite 一致性备份（quick_check 通过）、服务及环境文件；只保存在远端，未纳入 Git。
- 首次部署因检查脚本未统一 API 列表包装格式，触发自动回退；脚本修复后第二次部署成功。不是新版程序启动失败；两次日志均保留。
- 管理端 `readyz` 正常，服务 active、开机自启 enabled、`NRestarts=0`。实际 Web 浏览器已打开新按钮及弹窗，无页面 JS 异常。
- 验证时 3/3 下级连接正常，5297 个点，3 个客户端 Session、24 个订阅。现有组默认源未变化，自动切源、默认源自动策略和差异告警均关闭。
- 只重启管理器；下级进程 PID 45254 及启动标识未变，未重启下级或 process agent，未发送串口命令。

证据增加 `deploy.log`（首次回退）、`deploy-retry.log`、`release-smoke.log`、`deployed-browser.log`、`deployed-summary.json`、`deployed-groups.png` 和 `deployed-dialog.png`，位于同一日志目录。仍未开启真实故障切源或开展负载/长稳验收。
