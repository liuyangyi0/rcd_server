# 对抗审查修复记录（2026-10-09，已部署，长测待完成）

范围：上一轮多 Agent 审查确认的六项问题，以及回归中暴露的两个相关问题。前半部分记录隔离修复；另获本次控制暂停及重启授权后的现场部署见末节。保留其它未提交功能，没有提交、推送、业务配置替换或硬件控制报文操作。

## 修复内容

| 问题 | 修复与保护边界 |
| --- | --- |
| Agent 已接受的启停作业变为 unknown 后，管理端只 GET，迟到退出无法完成对账 | 管理端对同一个已持久化的 lifecycle 请求调用 Resolve；只确认原结果，不重新 Action，不重新停止或启动 |
| 原始 POST 尚未登记，浏览器先来对账时误用旧远程接口 | 在管理端登记该编号的失败终态封存记录；延迟到达的原 POST、正在进行的 Prepare 及管理端重启都不能再次派发；不猜测新身份 |
| Agent 对账成功却返回旧的 desired=running 快照 | 更新派生状态后再冻结终态快照；通过现有严格协议校验，并验证持久记录可以重新加载 |
| 审计数据库读取游标失败，阻断已接受命令的 ACK 补查 | 实时 pending 使用内存身份和独立只读 Get；审计失败继续告警，不伪装记录完整、不重发控制命令 |
| 慢历史同步仍占用同一来源的执行槽 | 实时补查、历史同步采用独立的每来源执行槽和超时；历史读取已开始后新来的命令也可以及时查询 ACK，两路错误分别保留 |
| 管理端重启后只有 pending 预占，没有完成协调状态 | 仅恢复未过期、身份/启动代次/物理端口/报文摘要齐全的 tracked 命令协调状态；不恢复旧 RPC waiter，不重新派发，不接受新连接的无关联 ACK |
| 管理网页未监听 source.process，Agent 绑定/状态变化不刷新 | 监听具名 SSE，按物理来源刷新绑定、状态和作业查询；刷新来源 revision 与机器台账，不连带刷新配置草稿、点表和日志 |
| Semaphore 详情依赖当前页列表，筛选后保留旧“检查成功” | 抽屉独立读取设备详情；关闭、切设备/项目及乱序响应有隔离；失败保留标明过期的历史值，禁用依赖新鲜状态的启停，恢复读取后消除警告 |

## 验证环境与边界

全部构建、可执行测试和浏览器位于 `192.168.10.46`：

`/home/lenovo/code/rcd_review_fixes_20261009_Vt0AZp/`

没有在本机构建或运行测试。隔离目录从此前已验证源码复制，再覆盖本轮修复文件；Go 文件在该目录 gofmt 后回传。Semaphore 原生源码基线仍为 2.19.16，没有新增门户、登录或执行队列。

关键测试不是只检查字符串：

- 使用实际管理端 HTTP 客户端和 Agent Handler，远程地址设为非 loopback，验证远程 ACL、unknown 原请求对账、回复丢失、resolve-first、Prepare 并发以及管理端重建。
- Agent 测试验证迟到退出的终态快照符合协议，并可在 Agent 重建后读取；不放宽成功状态校验。
- 命令测试关闭 Runtime 与 audit 共用的真实 Repository，证明游标读取失败后仍能接收内存 ACK；原始审计缺口仍可见。
- 实际 commandTrackingWorker 用通道固定“历史读取已阻塞”的时点，再提交 Call 和注入 ACK；要求历史未返回时 Call 已成功，发送次数为一次。
- 使用同一个 SQLite 构建新 Runtime，验证未超时的关联命令可完成，错误 manager/gateway/boot/port/hash、legacy 和已超时记录不可借新 ACK 改写结论。
- 管理网页浏览器使用持续具名 SSE；不用重连刷新掩盖 source.process 缺失。平台浏览器使用隔离原生登录和前端，RCD 状态响应为测试样例，不执行现场服务任务。

这些结果只覆盖本轮源码及隔离行为，不等同于真实串口、现场新版部署、一小时或过夜稳定性验收。现场原版本的观察记录不能证明本修复版本。

## 验证结果

| 检查 | 实际结果 / 证据 |
| --- | --- |
| lifecycle、命令恢复定向回归 | 通过；`go-targeted-final.log` |
| 慢历史同步与实时 ACK 隔离补强 | 通过；`go-command-final.log`，包含实际 worker 屏障用例 |
| 管理端 `GOMAXPROCS=2 go test -p 2 ./...` | 通过；最终源码 `go-all-final.log`，未变更包复用 Go 测试缓存 |
| `go test -race -p 2 ./internal/app ./internal/processagent ./internal/store ./internal/opc` | 通过；`go-race.log` 首轮四包执行，最后调度改动再次执行 app，其他未变更包复用缓存，见 `go-race-final.log` |
| 管理 Web `npm test` | 35 个文件、241 项通过；`web-all.log` |
| 管理 Web `npm run build` / Go 程序构建 | 通过；`web-build.log`、`manager-go-build.log` |
| Semaphore 页面状态测试 | 24 项通过；`platform-ui-state-final.log` |
| Semaphore 原生 Vue 构建（含 ESLint）/ Go 单程序构建 | 通过；`platform-web-build-verified.log`、`platform-go-build.log` |
| 管理网页浏览器回归 | 11 组检查通过；`manager-browser.log`、`source/artifacts/runs/process-maintenance-browser/result.json`；具名 SSE 独立验证绑定刷新，零修改请求 |
| Semaphore 浏览器回归 | 5 组流程通过；`platform-browser.log`、`platform-browser/result.json`；1366/1920 布局，详情脱离列表仍刷新、详情错误及恢复、原请求回复丢失；真实服务任务为零 |

初次跨组件测试暴露了 Agent 成功快照仍含旧意愿状态的问题，修复后重新通过。原生 Vue 构建也先报告新代码的行长检查，修正后通过；失败日志保留，没有绕过协议校验或关闭 ESLint。

本轮隔离构建产物（不是现场运行版本，也不是已签发发布包）：

- `rcd-manager-review`：SHA-256 `6d52a81ad45f2e5c0774a72b75b2fd410de6693addfe3c6100974cb4075aff05`。
- `semaphore-isolated`：SHA-256 `2585d0c5a70a283e3ac9838517174b3f7db7553c27905a53d59510335b9588b7`。

浏览器使用的原生 Semaphore 临时实例位于 `/home/lenovo/code/rcd_unified_review_fixes_20261009_Vt0AZp`，仅监听 loopback 随机端口，验收后已正常退出，保留证据与隔离数据库。没有停止或更改正式 `46:3000` 及三台业务服务。

以上隔离阶段未覆盖现场重启/实际硬件控制、一小时/过夜运行和 Windows 执行。后续另获当次维护授权完成下述部署；实际硬件控制、长测及 Windows 仍未验收。

## 当次授权部署与平台实操

用户本次明确确认暂停三台管理端及直连客户端的控制，并允许重启平台、管理端和 Agent。所有构建、测试与浏览器仍在 46 隔离目录执行；本机只编辑、检查源码和传输。

### 版本与证据

- Semaphore `v2.19.16-rcd.4`：SHA-256 `7c55dd474a1c0f1d463a3bdb9d01d8e57ec890053317aa855df8f6607422f683`。通过平台自身安装器更新，不冒充平台内业务组件发布；首次因定时任务未结束在停服前拒绝，队列空闲后才执行。备份 `/var/lib/semaphore/backups/native-platform-1791525820423660722`，含一致性数据库、配置、软件包及原程序。
- 三台管理端 / Agent `0.6.3-unified.20261009.2`，发布编号 `review-fixes-20261009-2`，二进制 SHA-256 `d2e5018d2db84b76e5c2b4094d29756271037e87c9c4cb9b7f11a3f1d4539c3d`。
- 基点提交 `769ef2a1f82e6e7bb5e77f9e79a3e88434005e42`，实际脏快照摘要 `7643f96e91519336180b346306455314600c7610425591adc948da829b3782ff`，不冒充干净提交。再次完成 npm ci、完整 Web 测试/构建、Go 全包测试和身份核对，见 `release-build.log`、`build-artifacts/build-1230abc60b474acba076c2343108ed7e/manifest.json`。
- Agent 与管理端来自同一个支持两种入口的 Go 二进制；Agent 包另执行 `agent version --json` 验证组件身份并记录派生关系，不是只改包名。两个包均不含业务配置、数据库或 PKI。
- Rust 三机保持 `0.6.2-ops.20261008.1`，实际 SHA-256 `f300d7ae3b55ab93261e4c1a70edaaae412bbba72cf3f56664308de006d5815a`，没有替换 Rust 程序或调整采集时序。

### 六次真实网页升级

原生 Semaphore 登录和页面实际完成上传、选择包、选机器、当次暂停确认、升级及查看执行过程；没有 mock，也没有用 SSH 手工替换业务程序代替平台发布。

| 目标 | Agent 任务 | 管理端任务 | 实际核对 |
| --- | --- | --- | --- |
| 30 | 3677 | 3681 | 成功，新进程摘要匹配，未选择组件摘要不变 |
| 58 | 3683 | 3687 | 成功，本次无 KYSEC 拒绝，不改安全标签 |
| 46 | 3691 | 3695 | 成功，最后一次维护于北京时间 14:15:04 完成 |

均经过原生队列 → Ansible → 维护排空 → 备份 → 替换 → 运行身份确认 → 维护收尾。每次网页发布只产生一个 POST，没有重复提交。各目标机 `/var/lib/rcd-release-platform/artifacts/{agent,manager}/deployments/` 保留备份与原操作记录；平台任务保留关联，不删除原作业。

证据：`live-browser-v5/{agent,manager}-{30,58,46}/` 的原请求、结果、设备快照和 1366/1920 截图。两个上传均返回 201，之后按归档摘要只读确认入库，未重复上传。初版验收脚本遇到登录导航竞争、大上传回执缓存淘汰及 Vuetify 控件定位问题；已修正并保留 `live-browser*` 原证据。这些不是业务发布失败，不用重复发布验证脚本问题。

### 发布后只读验收

- 每台管理端均连接三个来源，全部 `Connected / Active / Active`；九条运维连接均 running，来源稳定 ID、固定 IP 和名称不变。三台 `pending_commands=0`、`catalog_conflicts=0`，无 Agent pending/maintenance 残留，台账同步无错误。见 `managers-after.json`。
- 三台 `/healthz` 全部检查为 ok，包括命令审计、日志持久化和数据库；平台更新后 journal 无新增 error 级记录。
- 六个 OPC 入口 Browse/Read/Subscribe 均通过并收到业务点通知，Write/Call 为零，见 `opc-readonly-postdeploy.jsonl`。30 管理端点数 3300、58/46 为 5297，未复制使用人的计算配置。
- 部分下级样本仍为 `BadNoCommunication`，30 管理端某样本带 Good 信息位 `0x00000080`；未清空质量标志，不将接口通过说成全部物理设备正常。
- 本次未重复历史恢复、九组启停矩阵或发送硬件控制；已有现场证据与本轮隔离回归分开保存。一小时/过夜及本版 Windows 仍未通过。

既有观察服务未重启、日志未清理。以北京时间 14:16 后本版本记录建立新观察窗口，`postdeploy-observation-baseline.json` 保留三台 14:18 样本。旧脚本仍保留 `process_identity_or_restart_count_changed`，这是相对 13:15 旧基线的已授权变化；仅在实际 PID/SHA/重启计数与新基线一致时才能归为本次部署。新异常不得被笼统忽略。三台最新 health_warnings 均为空、systemd NRestarts 均为 0。已通知恢复控制，后续仅只读观察。

## 首次一小时复核：进程稳定，OPC 连接稳定性未通过

2026-10-09 北京时间 15:24–15:27，只读读取三机完整 JSONL，再按 **14:16 后**的新版本窗口汇总；没有将旧版本区间合并。没有构建、重启、发布、配置/标签修改、硬件控制或日志清理。

| 机器 | 本次完整采样区间（北京时间） | 样本数 | 最大相邻间隔 | 健康告警样本 |
| --- | --- | --- | --- | --- |
| 30 | 14:16:13–15:23:13 | 68 | 60.090 秒 | 0 |
| 58 | 14:16:13–15:23:13 | 68 | 60.099 秒 | 1 |
| 46 | 14:16:21–15:23:21 | 68 | 60.097 秒 | 2 |

共 204 个新版本样本，每机跨度约 67 分钟，无倒序或漏分钟。逐条 PID、实际 SHA、NRestarts 与 `postdeploy-observation-baseline.json` 一致，才将旧基线引起的身份变化标记归为已授权部署；没有笼统忽略该标记。九个业务进程和平台未发现崩溃/自动重启，NRestarts 均 0；HTTP 均 200，4840/4841 TCP 端口检查未失败。端口存活不等于已建立 OPC 会话持续正常。

### 发现的异常

- 46 管理端 14:25:21 样本：`command_tracking/rcd92-58 = warning: lower OPC source is unavailable`。
- 46 管理端 14:51:21 样本：`command_tracking/real-lower = warning: audit_unavailable: journal lookup timeout`。
- 58 管理端 15:03:13 样本：`command_tracking/rcd92-58 = warning: lower OPC source is unavailable`。
- 上述告警前后相邻分钟样本正常，15:25:54 三机健康明细也恢复正常；每分钟采样不能确定精确持续时间。审计超时来自下级历史记录查询等待，不凭告警推定数据库损坏或命令已丢失，也不能据恢复后的 HTTP 200 声称全区间无异常。

继续只读核对来源状态，发现重连是持续问题。下表为发布后的 `managers-after.json` → 15:25:54 的 `reconnect_count`，不是 systemd 重启数：

| 管理端 | 下级 30 | 下级 58 | 下级 46 |
| --- | --- | --- | --- |
| 30 | 1 → 29 | 2 → 26 | 2 → 7 |
| 58 | 1 → 28 | 0 → 24 | 2 → 7 |
| 46 | 0 → 28 | 0 → 24 | 0 → 5 |

三份下级当前日志在 **14:16:00–15:24:00（不含末端）**分别有 53 / 35 / 6 条 `Fatal transport error: BadSequenceNumberInvalid`。例如 58 的 14:25:21.671、15:03:14.721 与告警采样相邻。该错误是 OPC 传输层报文序号无效并结束连接，不是采集进程崩溃，也不是物理站点 `BadNoCommunication`；尚未完成根因定位，不能断言由本次修复引入或归咎串口卡。

同区间每台另有 136 条建链前 `BadCommunicationError`，其中多条时间与只建 TCP 随即关闭的分钟探测相符；这类记录单独统计，不全部算成真实业务会话掉线，也不据此忽略上述序号错误。46 下级 14:35:59 的一条现场命令记录显示三次写出后 ACK 超时；只读取原记录，未发送、重发或修改该命令，不能仅凭该记录断定与 OPC 重连存在因果关系。

### 平台与证据

- Semaphore 自 14:16 至本次读取的 1435 条 journal 全为 info，无 warning/error；原生数据库只读查询有 204 个新检查任务成功，当前分钟 3903/3904/3905 为一个运行、两个等待，无旧任务积压，发布占用表为空。
- 全区间平台三设备版本快照最大年龄小于 81 秒，没有超过 180 秒；最新九组件身份匹配、无磁盘/运行版本差异、无发布作业占用。三台观察服务均 active、NRestarts=0。
- 15:25:54 三个管理端均显示三个来源 Connected/Active/Active、pending_commands=0、catalog_conflicts=0；瞬时恢复不抹去重连历史。
- 原证据：30 `/home/RCD93/code/rcd_unified_20261009_5rgqWC/observation.jsonl`；58 `/home/lenovo/code/rcd_unified_20261009_u3NS1o/observation.jsonl`；46 `/home/lenovo/code/rcd_unified_20261009_iuB7vj/observation.jsonl`。读取时均 129 行，其中 68 行属于新窗口；之后仍自然追加。
- 相关下级日志：各机 `/var/log/rcd/lower/<rcd93-30|rcd92-58|real-lower>/rcd_server_rCURRENT.log`。不删除或改写异常证据。

**结论：一小时复核已执行；进程、端口和平台任务检查通过，但 OPC 连接稳定性及命令追踪全程无告警未通过，整体不能验收为全部完成。** 当前只读权限不包含修改或现场重启，需用户决定进入针对性修复。暂停自动验收，保留远端日志与 24 小时观察服务自然退出；过夜尚未验收。

后续：2026-10-11 经用户要求开展针对性修复，代码及隔离回归结果见 [OPC 传输序号与审计查询修复](opc-transport-audit-fixes-20261011.md)。该修复尚未部署，不改写本次线上稳定性未通过的结论。
