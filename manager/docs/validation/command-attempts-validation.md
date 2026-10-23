# 命令发送次数记录验证（2026-09-22）

基于 `769ef2a` 增加下级命令执行摘要及 Web“命令记录 → 串口发送明细”。使用方式和保留边界见 [集中日志](../guides/central-lower-logs.md)。未改变发送次数配置、ACK 字节、读取超时、选源策略或旧 OPC 节点。

## 实现边界

- 摘要由实际带 ACK 命令循环生成，不从相同报文或相近时间猜测次数。下级命令编号与管理端命令 ID 不同，不自动跨来源/Session/管理器拼接记录。
- 每条已执行命令结束后上报一次，完整发送次数与发送尝试次数分开；成功后保留先前超时/不匹配原因。首次、第二次、第三次收到 ACK 都立即退出循环。
- 复用既有 HTTP 日志管道、来源校验、落盘、补传去重、SSE 与保留策略。下级 HTTP 摘要不受普通日志等级影响，仍为有界非阻塞投递；不承诺队列满、断网超容量或强杀时完整保留。
- 新增 `command_attempts=all|retried|three` 在后端计数分页之前过滤，旧数据缺少摘要时不假定只发送一次。日志推送到 46 不会自动复制给 58。

## 验证结果

全部执行于 `192.168.10.46` 的隔离目录 `/home/lenovo/code/rcd_command_audit_20260922`，本机只编辑、检查差异和接收证据。

| 检查 | 结果 |
| --- | --- |
| `cargo test --lib command_audit -j 2` | 6 项通过：第 1/2/3 次 ACK（含分片）、三次耗尽、原因分类、次数/截断、非阻塞推送 |
| `cargo test --lib command -j 2` | 21 项通过，包括既有命令入队、超时、ACK、串口读取异常、通信状态不污染 |
| `cargo test --lib log_push -j 2` | 5 项通过，包括 spool、HTTP ACK 解析和 JSON 串口/站点上下文 |
| `cargo build -j 2` | 下级开发构建通过 |
| `go test ./internal/lowerlogs ./internal/api ./internal/app` | 三个包全套通过 |
| `go test -race ./internal/api ./internal/app -run LowerLog` 及 `go test -race ./internal/lowerlogs` | 通过，无竞争报告 |
| 发送明细与下级日志页面前端测试 | 2 文件、5 项通过；含次数含义、原因、大整数 ID、过滤重置分页、来源切换、畸形记录 |
| `npm run build`、`go build ... ./cmd/rcd-manager` | 管理端和内嵌 Web 构建通过 |
| 远端 Playwright/Chromium | 独立进程/数据库/随机 loopback 端口：登录、空态、HTTP 摘要写入、SSE 自动更新、重复批次去重、服务端分页、来源隔离、桌面/窄屏显示通过 |

浏览器测试数据为明确构造的诊断摘要；Rust 使用脚本化串口单元测试。**这些不代替真实设备验收。本次未下发现场设备命令，未部署或重启 46/58/30 正在运行的程序，未提交或推送 Git。** 必须同时更新下级与管理端后，现场页面才会出现新命令明细。

证据目录：远端 `artifacts/command-audit/`，本机副本 `artifacts/runs/2026-09-22-command-audit/`。包含 Rust/Go/Web 测试与构建日志、浏览器输出、桌面/窄屏截图。未测试 Windows 发布包、硬件实际 ACK 时序、长时间日志容量或断电恢复。

## 后续授权部署（2026-09-22 10:54–10:58，Asia/Shanghai）

用户随后要求部署。以上“未部署”为初次开发交付时状态；本次更新如下：

- **46 管理端**升级为 `0.7.6-command-audit.1`，构建标识 `769ef2a-command-audit-20260922`（基于该提交的未提交功能改动）。固定入口仍为 `/home/lenovo/.local/bin/RcdManager`；版本化目标位于 `/home/lenovo/code/rcd_device_groups_20260828/bin/`。
- **30、58、46 三台下级**依次替换各自 `~/code/rcd/rcd_server`；使用现有进程 agent 明确停止/启动，全部 `forced=false`，保留 agent 原进程。下级运行状态通过 OPC 运维接口确认均返回新构建标识。
- **58 第二管理端未升级**，待用户确认是否同步更新。日志目标不变，三台下级仍只推送到 46。
- 下级配置 TOML/CSV 哈希前后一致；未修改 50 ms 超时、5 ms 轮询间隔、总共 3 次命令上限、校验切换参数、计算配置、默认源、端口或安全策略。

发布产物均在 46 构建。管理端发布二进制重新通过隔离浏览器/API 验收后部署；Rust 使用 `cargo build --release --locked -j 2`。

| 产物 | SHA-256 |
| --- | --- |
| 管理端 | `e947feb17d05007529ccb30e7cc89da9098496b9c96de02a579c8854a899a746` |
| 三台下级（相同二进制） | `fb8ef50d5b535937e49046199fbbb6c0ef9fefcff8304d03945a843540772153` |

备份保存在各自机器，不纳入 Git：

- 46 管理端：`/home/lenovo/code/rcd_device_groups_20260828/backups/command-audit-20260922-105429/`，含旧二进制、服务/环境配置、SQLite 一致性备份（quick_check 通过）和前后状态。
- 30 下级：`/home/RCD93/code/backups/command-audit-rcd93-30-20260922-105520/`。
- 58 下级：`/home/lenovo/code/backups/command-audit-rcd92-58-20260922-105548/`。
- 46 下级：`/home/lenovo/code/backups/command-audit-real-lower-20260922-105603/`。
- 每个下级备份含旧二进制、配置/站点状态，以及停止后的第二份配置副本；未改动 PKI。

现场只读验收通过：46 `readyz`、服务 active/running、`NRestarts=0`；3/3 来源 Connected、订阅 Active、目录 Ready，5297 点和 1997 条中央计算规则保留。真实浏览器已看到“串口发送明细”，无页面 JS 异常；三个来源重启后的 HTTP 日志均已到达 46。OPC `TestLiveManagerAnonymousNone` 和 `TestLiveManagerCentralCalculations` 通过，包括命名空间、计算节点 Read/Subscribe。既有设备坏质量仍保留，没有伪造为 Good。

本次**没有发送设备控制命令或向运行库注入示例摘要**，检查时三个来源新增命令摘要均为 0。次数及成功/失败展示已由隔离测试覆盖；现场实际命令到来后才能核对真实次数。证据新增 `release-browser.log`、`deploy-manager46.log`、`deploy-lower46.log`、30/58 部署日志、`deployed-browser.log`、`deployed-opc-read.log`、`deployed-summary.json` 和 `deployed-command-attempts.png`。未执行 Git 提交/推送。
