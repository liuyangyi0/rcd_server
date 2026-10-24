# 0.4.0 下级进程管理验证记录

版本：`0.4.0-process-control`。最终管理器构建标识：`cc14f5d+lifecycle-v3`。记录日期：2026-08-28。

结论：Rust、Go、Web 单元/构建及最终真实 OPC、REST、浏览器生命周期回归均通过；恢复和独立进程保护审计通过，最终只读核对 14/14。首轮浏览器的提前查询竞态已修复，并通过真实 POST 延迟后的复测；早期失败记录保留。

本文件按实际执行记录汇总，不把未跑项、忽略用例或旧版本结果计入本次通过。本次结论限下述隔离环境与覆盖范围，不等同 systemd、自启、生产硬件或满负载验收。

## 1. 环境与保护范围

- 远端：Linux x86_64 / Kylin V10 SP1，主机 `192.168.10.46`。
- 新管理器 Web / OPC：8086 / 4872，已运行。
- 新 Rust 下级：4870 / 4871；进程 agent：8091 / 8092，实例 `lifecycle-a` / `lifecycle-b`，来源 `lower-a` / `lower-b`。
- 运行根：`/home/lenovo/code/rcd_device_groups_20260828/runs/lifecycle-20260828-v1`；独立 PTY 夹具位于其 `fixture/`，程序、配置、PKI、日志、agent-data 按 A/B 分离。
- 日志根：`/home/lenovo/code/rcd_device_groups_20260828/logs/lifecycle-20260828-v1`。下文证据文件名均相对此目录。

最终运行组合为管理器 v3、两个 agent v2、Rust v1。管理器验收时 PID 为 666309；两个 agent 未更换 EXE，v2/v3 的进程管理后台代码相同，v3 仅调整 UI 与版本元数据。不能将这轮结果描述为“两个 agent 已热升级 v3”。升级前管理器 SQLite 备份为运行根下 `backups/manager-before-ui-v3.sqlite`。

旧 Web 8085 / OPC 4862 和原 4840 Rust 服务均独立保留。保护 PID 为 398713、612417、618222、614880；测试核对其启动 tick 和进程身份，不以“PID 数字仍存在”代替未重启判断。REST 只允许新 A child 和特定步骤的 A agent 变化，B child/agent、fixture 与四个旧进程另做最终逐项审计；该审计独立于恢复流程，即使恢复中断也会逐个记录。

所有构建、测试与真实浏览器执行在远端进行。浏览器使用远端 Playwright，原因是用户明确禁止本机测试；本机仅编辑与静态审阅，没有以本机浏览器或测试进程补充验收。

## 2. 已完成的测试与证据

| 范围 | 本次结果 | 证据 |
|---|---|---|
| Rust 全量测试 | 335 通过，4 忽略；忽略不计通过 | `rust-all-v1.log` |
| Web 单元 | 141/141 通过，17 个测试文件 | `web-unit-v3.log` |
| Web 生产构建 | 通过 | `web-build-v3.log` |
| Go 全模块 race | `go test -race ./...` 通过 | `manager-all-v3.log` |
| Go 静态检查 | `go vet ./...` 通过 | `manager-vet-v3.log` |
| 管理程序构建 | v3 通过，内嵌修复后的 Web 资源 | `manager-build-v3.log` |
| OPC 基线三大项 | 通过，含旧点表对照、既有安全连接子项、读/订阅/Web 选源/命令和 Session 恢复 | `opc-baseline-v1.log` |
| 新进程 OPC 生命周期首轮 | v2 通过，约 9.4 秒 | `process-opc-live-v2.log` |
| 最终合并 OPC 回归 | v3 四主测试通过，17.084 秒 | `process-opc-final-v3.log` |
| REST 首轮 | 16/16，恢复及保护审计通过 | `process-rest-v1.log`；`rest-20260828T114127Z-244dc199/report.json` |
| REST 最终复测 | v3 上再次 16/16，恢复及保护审计通过 | `process-rest-v2.log`；`rest-20260828T120050Z-76fb820e/report.json` |
| 真实浏览器最终复测 | ui-v2 六主流程、恢复及七个保护进程核对通过；错误/提前对账均为 0 | `process-ui-v2.log`；`ui-v2/report.json` 与同目录 PNG |
| 最终只读核对 | 14/14，exit 0；数据库、配置、数据与进程身份符合预期 | `lifecycle-final-v3.json`、`lifecycle-final-v3.log`、`lifecycle-final-v3.exit` |

这些是不同层次的验证记录，不相加为一个“总功能通过数”。`race`、单元用例、真实 PTY 和浏览器结果各自保留边界；构建通过不等于设备现场通过。

### OPC 基线与生命周期

OPC 基线包含旧点表黄金对比、六个既有安全连接子项，以及读、订阅、Web 选源、命令和 Session 恢复回归。最终 v3 合并执行四个主测试：旧点表/安全、ReadSubscribeWebCommands、进程生命周期、SessionResume，全部通过。本次沿用原 OPC 安全配置，没有新增或更换安全策略。

生命周期测试使用真实 Rust 下级与管理器，验证：

1. 实际 `_Send` 命令 pending 时进程操作返回 409，而非仅通过假客户端模拟互斥。
2. 选中 101 的来源停止后，该 Session 收到坏质量；另一 Session 仍从另一来源读取 11、质量 Good，没有自动切源。
3. 下级 start/restart 后，原上级 Session、Subscription 及各组路由保持，订阅恢复到所选来源。
4. 已完成 request_id 重放不再触发第二次进程重启。

### REST 16 项

首轮与最终复测均为 16/16；最终以 `rest-20260828T120050Z-76fb820e/report.json` 为准。完整请求、响应、时限、进程身份与每步断言保存在报告和同目录 `trace.jsonl`，原始 `calc.toml` 与初始夹具快照也在各自目录保留。通过项为：

1. 独立夹具、四个旧进程身份及固定来源绑定。
2. 进程状态、有限日志及真实 PTY 文件描述符。
3. 过期来源、绑定、agent 三种版本拒绝，PID 与状态不变。
4. 经地址别名重复绑定同一实例返回 409，原绑定不变。
5. 未存在请求经 resolve 封存为 `unconfirmed_cancelled`；迟到原请求不执行，同 ID 不同内容被拒绝。
6. stop A 确认实际进程与 FD 消失，点位变 `BadNoCommunication`，组选源未变化。
7. 下级离线时进程日志仍可读；start 后新 PID 与真实数据恢复。
8. 仅通过夹具重启 A agent；持久期望 stopped 不被首次启动参数覆盖，随后显式 start 恢复。
9. 仅在 A 已停止时临时去掉其二进制执行位；启动失败明确可见，恢复原权限后显式启动成功。
10. 仅把计算常量 7 改为 9 保存；PID 不变、运行值仍为 7，显示待重启。
11. 错误配置 hash 的 restart 返回 409，不派发进程重启。
12. 正确重启受理后，pending 窗口内配置写入和解绑均返回 409，进程日志仍可读取。
13. 重启产生新 PID，`active_revision` 等于保存 hash，待重启标记清除，`CALC_CONST=9`。
14. 同 ID 重放已完成 restart，只返回原作业，不产生第二个新 PID。
15. 通过 API 恢复原始配置并显式重启，回到 `CALC_CONST=7`。
16. 作业/管理审计、最终配置、B 真实采集及保护身份完整。

恢复流程有独立预算；HTTP 超时只查询/对账原 request_id，不盲目重发动作。最终结果还要求恢复成功和逐项保护审计成功，不能仅凭 16 个中间 PASS 就判定整轮成功。REST 不创建 OPC Session，其 Session/Subscription 保持结论来自前述独立 Go OPC 测试。

### 真实浏览器与最终核对

远端 Playwright 的 ui-v2 结果为 `ok=true`、六个主流程全部通过、恢复成功、七个保护进程核对通过。四次真实进程 POST 均注入 250 ms 延迟，`unexpectedHTTP`、`unexpectedConsole`、`prematureReconciliation` 均为 0；这验证了首次动作受理前不会提前查询/封存该请求，而非仅关闭 console 断言。

桌面 1600×1000、窄屏 390×844 均无横向溢出。主线程已人工查看初始页面、配置生效、离线日志、窄屏按钮四类截图。截图与报告保存在 `ui-v2/`，没有使用本机浏览器补测。

`lifecycle-final-v3` 首轮只读核对 14/14、退出码 0，包含 SQLite Schema 6 与完整性、作业终态且无 pending、配置磁盘/active 一致、双源采集 Good、四个旧进程及全部十个登记进程身份比较一致。它是验收结束时的状态核对，不是 24 小时运行或负载稳定性的替代证据。

最终目录并集有 7 个变量：6 个 Good，另一个 `serial_b_Send` 为预期的 `BadConfigurationError`——当前默认来源 `lower-a` 没有该串口节点，并非采集故障或自动切源失败。两物理来源的实际采集点均已核对为 Good，不能把“并集全部点都必须 Good”作为该配置的错误验收条件。

SQLite 作业统计为 32 个 succeeded、8 个 failed、非终态 0。failed 来自故意执行的负向用例和 resolve 封存，不是未解决的进程操作；该轮没有遗留 pending。应结合作业 request_id、动作和审计解释失败计数，不能只凭 failed 数量判断系统异常。

## 3. 历史失败与处理

历史失败日志保留，不删除、不覆盖，也不将其计为通过：

- 早期 `processagent-unit-v1` / `processagent-unit-v2` 受辅助测试进程启动 flag、测试宽限设置影响；完成测试辅助修正后，最终 v3 检查通过。最终采用 `manager-all-v3.log` 的全模块 race 结果，不以早期失败轮替代。
- OPC 生命周期 v1 假设下级一定提供非空 SourceTimestamp，但该真实 Rust 下级未提供；修正测试为使用管理器 ServerTimestamp 判定新通知后，v2 通过。没有为了测试去伪造/修改下级时间戳或改变产品转发语义。
- 浏览器 v1 虽完成六个主流程与恢复/保护检查，但有两次“动作尚未受理就查询作业”的 404 console 错误，因此该轮没有计为完整 UI 通过。v3 修复首次 Action pending 时的提前查询/resolve；ui-v2 增加四次真实 POST 延迟后复测，错误与提前对账均为 0。保留 v1 经过，不以操作看似完成掩盖竞态。

## 4. 不在本轮验收结论内

- systemd 用户服务实际安装、启动/停止、自启和未登录 linger；模板提供不等于服务验收。本轮未自动安装服务或申请现场系统权限。
- Windows agent 尚未实现；Windows 服务运行未验收。
- 真实串口硬件、电气时序、现场主备链路，以及生产场景下的强制停止/守护崩溃恢复矩阵；PTY/辅助进程不能等同真实硬件验证。
- 20 个来源、10 个全量上级订阅的性能指标、全规模延迟及 24 小时稳定性。配置上限不是性能达标报告。
- 未执行整机 shutdown/reboot；无自动 OPC 故障转移或高可用承诺。

## 5. 交付文档与后续核对

交付归档：`RcdManager-linux-x64-0.4.0-20260828.tar.gz`，包含管理器 v3 与 Rust v1；均动态链接，保留调试信息，未 strip。以下是两个**二进制文件**的 SHA256，不是归档 SHA256：

| 文件 | SHA256 |
|---|---|
| `manager/RcdManager`（v3） | `a95e013a4596b411bd8a2617e5251ddb2bddeb29ff5ed11bdd833124a6625966` |
| `lower/rcd_server`（Rust v1） | `bd96cd84d9ba562ef86af98d4ae8aab6b0a88177c026b0f20d1ac25546b7dbb0` |

包内 `docs/` 共五份：[使用说明](../guides/使用说明.md)、[功能说明](../guides/功能说明.md)、[下级 OPC 运维](../guides/lower-operations.md)、[下级进程管理](../guides/lower-process-management.md) 和本验证记录；`deploy/linux/` 放两份用户服务模板，保持详细文档的 `../deploy/` 相对链接。仓库对应 `manager/docs/` 与 `manager/deploy/linux/`。这五份文档的相对链接不依赖额外 `rest-api.md`，完整进程 REST 已在详细协议中列出。

随包 `licenses/` 收录四个本地 vendored 组件的许可文件，不改变文档与部署模板的相对布局。

核对入口时使用新版 Web 8086 / OPC 4872；查看进程时使用在线状态和当前夹具，不使用历史 PID 发信号。配置、agent 持久状态和原始现场 PKI 不包含在程序包中。后续测试仍只能对新隔离实例进行，并追加新的带版本日志，不覆盖本记录的早期失败与最终通过证据。
