# OPC 传输序号与审计查询修复（2026-10-11）

## 范围和状态

针对 [10 月 9 日稳定性复核](adversarial-review-fixes-20261009.md) 中的 `BadSequenceNumberInvalid`、持续重连和 `journal lookup timeout` 开展源码修复与隔离验证。

工作树为 `rcd_server_device_groups` / `codex/opc-device-groups`，保留已有未提交修改。所有构建和可执行测试在 46 的隔离目录进行，第一轮为 `/home/lenovo/code/rcd_opc_transport_20261011_40Br9u`，后续目录分别在各节列明；本机只编辑、读取源码及传输文件。真实 Rust 互操作使用临时目录、独占随机 OPC 端口和禁用的不存在串口，不连接现场串口、不发送硬件命令。

**最新状态（2026-10-11 13:34，北京时间）：已复现并修复 151 秒空闲订阅重连，三台管理端经 Semaphore 升至 `0.6.4-opc.20261011.2`，共同约 11 分钟现场观察通过，九条来源零重连。** 下级保持前一轮的 `0.6.4-opc.20261011.1`，本次未重启下级、Agent 或平台。已通知恢复控制，此后只记录证据，不再执行维护操作。业务配置、KYSEC 标签未改，未清理日志、提交或推送。新版一小时及过夜尚未验收，不能把本次问题专项验收等同长期稳定性保证。下文保留前一版失败区间及各阶段原始结果。

## 已复现的代码问题

1. Go OPC 客户端在持有通道实例锁后先分配序号，随后才检查取消；请求没有写到 TCP，序号却已消耗。编码失败也有同类问题，并留下响应 handler。旧代码五项 wire 回归均失败；真实 Rust 下级在一次已取消 Read 后拒绝下一次正常 Read，记录 `BadSequenceNumberInvalid`，客户端返回 EOF。
2. 修复取消路径后，真实 Rust 的并发 Read / 只读 Call / Subscribe / Renew 仍失败。三轮 None 和 Basic256Sha256 均发生序号错误；race 报告 `pendingReq.Wait` 与 `Add` 竞争。普通请求选择旧实例与登记发送不是原子操作；旧续期计时器也可能再次使用过时实例。
3. Rust 的冷历史 Lookup 与落盘、批量保存、保留期清理共用写入队列，容易耗尽 100 ms 查询预算。改为独立有界只读线程，事务内核对 schema 和 gateway 身份；串口 worker 仍不做数据库 I/O。确定性测试暂停并填满 writer 队列，验证历史查询、去重、内容冲突与新命令降级准入。

前两项有真实协议复现，第三项证明了队列阻塞机制；没有据此断言 10 月 9 日每一次现场异常都已逐条归因，也没有把物理设备通信故障归入软件传输问题。

## 证据目录

上述 46 隔离目录保留源码基线归档、测试日志和构建结果。源码基线归档 SHA-256：`d334b090cb0480131176910a88523cf5fe7880261ac961e676448f293e427153`，不包含现场配置、数据库、密码或 PKI。

- `go-sequence-before.log`：旧 Go 代码五项回归失败。
- `rust-go-sequence-before.log`：旧 Go 客户端与真实 Rust 的取消路径复现。
- `rust-go-sequence-after.log`：仅取消修复后的中间结果，取消通过、并发续期失败及 race；不是最终通过证据。
- `rust-journal-after.log`：命令追踪 12 项通过。
- `rust-lib-after.log`：383 项通过、1 项跳过。
- `rust-build-after.log`：新 Rust 隔离二进制构建完成。
- `tracking-interop-after.log`：新 Rust 的能力、禁用端口拒绝、幂等/内容冲突、旧启动身份拒绝、revision 订阅、事件补查和重启历史查询通过；含 None、加密匿名及加密用户名。

## 最终实现与验证结果

Go 传输层在分配序号前检查取消，未尝试写入的失败在原实例锁内恢复序号，响应 handler 只清理当前注册项；分片已写出后失败则关闭整个通道，不回退序号、不重发。新增可取消的通道发送门，将“选择 token、写完整报文”与续期互斥；普通响应等待不占门，旧 token 的延迟续期跳过。首次 Open 或 Renew 结果不明时关闭通道，Close 不排队等待未返回的 OPN。请求的写入 deadline（包括非正值）与响应路径的原行为保留。

Rust 只拆出历史单条查询；命令准入、100 ms 持久化降级策略、事件游标边界、保留策略、串口时序、ACK 和重试次数未改。独立交叉审查未发现新增阻断项；保留真实 I/O 故障仍可能超时这一边界。

以下全部在上述 46 隔离目录执行，源码经远端 gofmt / rustfmt 后回传当前工作树：

| 范围 | 命令 / 覆盖 | 结果及证据 |
| --- | --- | --- |
| Rust 审计 | `cargo test --offline --locked --lib command_tracking -- --nocapture` | 12 项通过，含 writer 饱和、WAL 读隔离、身份与 schema 校验 |
| Rust 库回归 | `cargo test --offline --locked --lib` | 383 项通过、1 项按原标记跳过（`load_user_calc_real`，不加载现场计算配置） |
| Rust 构建 | `cargo build --offline --locked` | 成功，仅隔离 debug 二进制 |
| Go fork | `GOMAXPROCS=2 go test -race -p 2 -count=3 -timeout=3m ./...` | 全包通过，新增 14 项发送/续期回归；`go-fork-verified.log` |
| Go 管理端 | `GOMAXPROCS=2 go test -race -p 2 -count=1 -timeout=4m ./...` | 全包通过；`manager-race-final.log` |
| 真实 Rust 传输互操作 | 设置 `RCD_SEQUENCE_INTEROP_BINARY` 为本轮 Rust 二进制，`go test -race -p 2 -v -count=3 -timeout=4m ./internal/opc -run '^TestTransportSequenceRealRustInterop$'` | 三轮、None 与 Basic256Sha256 均通过；`rust-go-sequence-final.log` |
| 真实 Rust 审计互操作 | 设置 `RCD_TRACKING_INTEROP_BINARY` 为本轮 Rust 二进制，`go test -race -p 2 -v -count=1 -timeout=3m ./internal/opc -run '^TestCommandTrackingRealRustInterop$'` | 通过，包含重启后的冷历史查询及安全端点；`tracking-interop-final.log` |
| Go 实际构建 | `go build -p 2 -o ../../RcdManager-isolated ./cmd/rcd-manager` | 成功；`manager-build-final.log` |

传输互操作每轮每种安全策略执行 8 次取消及其后的 8 次正常 Read，以及 450 Read、450 只读 Operations Call、3 次显式续期与实际订阅。三轮总计 48 次取消后连接仍可用、2700 次并发 Read、2700 次只读 Call、18 次显式续期；客户端另有短寿命 token 的自动续期。禁用自动重连，不能用重连掩盖断链。最终三轮没有 race 或 `BadSequenceNumberInvalid`。这不是现场控制命令验收。

新增单元 fixture 曾因未完整初始化 UA 响应头、未设置 HEL/ACK 后的接收限额失败；仅修正测试初始化，没有放宽生产协议限制。两份中间日志 `go-fork-final.log`、`go-fork-complete.log` 原样保留，最终依据为 `go-fork-verified.log`。原先取消修复不足以解决续期的失败日志也保留。

隔离构建产物（不是标准现场发布包）：

- `RcdManager-isolated` SHA-256：`e22ad186eade209c6ec23639c2c517c0b520e821349b309c1fbd2ba4e3f743a7`。
- `source/target/debug/rcd_server` SHA-256：`a643df2b26ca83b5779e702ea523af64262b58c02410d6267d85d39ea6498e3c`。
- 修复生产源码 SHA-256：`secure_channel.go` 为 `34f334126b8afd7a0adff6864dfc4b2a455dac72af3fac69076d007c867cba2e`，`command_tracking.rs` 为 `e5f43d798c223a0c6886236a078ca0b3f07a663880ae01ffdd4f4dfeab731980`，本地与远端相同。

## 隔离验证完成时的边界（部署前历史记录）

该阶段代码修复及隔离回归完成，尚未部署。隔离 Rust 子进程均已由测试清理，未遗留测试采集实例。没有修改 Web、Semaphore、Agent 或现有业务配置；当时旧线上版本仍可能发生原有异常。

后续需另取当次部署及控制暂停授权，制作记录源码与版本的正式包，按现场维护流程升级管理端和下级，再重新建立一小时和过夜观察窗口。Windows、真实串口控制、现场三管理端长时间并发、端到端 P95 性能及长期稳定性本轮均未验收，不把几十秒的互操作回归写成一小时或过夜通过。

## 正式包与部署预检（2026-10-11 上午）

收到“开始部署”后，先完成隔离正式构建和只读预检；该准备阶段尚未收到本次控制暂停确认，没有上传发布任务、替换程序或重启现场服务。发布范围为 30 → 58 → 46 各自的下级和管理端，Agent、Semaphore 不升级。原控制暂停授权不沿用；后续获得当次确认的实际部署见下一节。

证据目录为 46 的 `/home/lenovo/code/rcd_transport_release_20261011_CEzn25/`。正式构建使用 `tools/release/release.py`，包使用 `rcd_package.py`；基线提交 `769ef2a1f82e6e7bb5e77f9e79a3e88434005e42`，明确记录 dirty 快照。输入与前轮经过验证的源码逐文件比对，唯一既有文件差异为补充后的 `FORK.md`；另外补齐正式构建所需的 Web、发布工具等源文件。

- 两个组件版本均为 `0.6.4-opc.20261011.1`，发布编号 `opc-transport-20261011-1`。
- 源码归档 SHA-256 为 `a4d7352354632087b4b8d3f0cb003dde0084f118925239812a8ffa34398926ae`；939 个源文件清单摘要为 `e620beaa604bcb48d7d69a534627dfd7183b9a9e9e859c552c7b89ae9ea58ddd`，两者用途不同。
- 管理端二进制 SHA-256 为 `8402d6381b8642c1d1f882fdcb3633705ea91fcfe224912dffbbe26cbad8585a`；发布包 SHA-256 为 `06e5c3c7ad263f8de18372574a6943eea12d52a1d48b1b95b4c093219d436889`。
- 下级二进制 SHA-256 为 `269b2aaace51bed8541a0de53ac814d57458d0d963207f5dd658b21adfc94dd8`；发布包 SHA-256 为 `ef7104aa28d5c796657cb6d107ca9ae870dbe9aaf058e109bcbb87665bbfd71b`。
- 正式构建的 Web 35 文件、241 项测试通过；Web 生产构建、管理端 `go test ./...` 和正式编译通过。Rust 383 项库测试通过、1 项按原标记跳过，Release 编译及运行身份核对通过。日志为 `manager-build.log`、`lower-build.log`；清单为 `manager-package.json`、`lower-package.json`。
- 包仅包含单组件二进制和公开清单；兼容范围固定为管理端 `manager_db=16`，下级 `command_journal=1`、`maintenance=0`，最低适配器 `1.1.0`。maintenance 的数值来自 SQLite `PRAGMA user_version`，不是维护记录内 JSON schema。
- `platform-before.json` 证实三机仍为原版本，快照新鲜、无发布占用，新发布编号未占用；三机实际数据格式均满足上述精确范围。
- `preflight-confirmed.json` 核对九条来源/Agent 路径全部运行且空闲、三机待处理命令为零、目录冲突为零、健康明细无告警，实际进程 SHA 与磁盘一致。初次 `preflight.json` 因 46 自连 SSH 缺少本机 host key 产生证据缺项；保留原报告，改为在已认证的 46 上执行同一只读 sudo 探针后补齐，没有放宽 SSH 校验或修改 known_hosts。
- `opc-before/` 与 `opc-before.log` 保存六个入口的 Browse/Read/Subscribe 和节点属性：30 管理端及三个下级各 3300 个变量，58/46 管理端各 5297 个变量；未 Write/Call 或发送硬件命令。这些是升级前基线，不是新版现场验收。

准备阶段没有启动新的长测或修改自动验收任务；正式发布后须重新建立与实际 PID、摘要及重连计数匹配的观察窗口。

## 正式部署与即时验收（2026-10-11 中午）

用户本次明确回复“可以重启”，已提醒维持控制暂停。通过 46 的 Semaphore 原生页面上传两个包，并经原生队列、Ansible 和既有发布器按下表串行完成；没有绕过平台直接替换安装目录二进制，没有发送硬件控制报文。

| 目标 | 下级升级任务 | 管理端升级任务 | 结果 |
| --- | --- | --- | --- |
| 30 | 12027 | 12031 | 均成功，实际运行版本及摘要匹配目标包 |
| 58 | 12036 | 12043 | 均成功，实际运行版本及摘要匹配目标包 |
| 46 | 12050 | 12054 | 均成功，实际运行版本及摘要匹配目标包 |

每次浏览器操作只有一次发布 POST；1366/1920 宽度无页面横向溢出、无 pageerror，证据在 `browser/upgrade-<host>-<component>/`。上传不会自动部署。六个任务及原始失败历史全部保留，没有重投旧任务。

- `postdeploy.json`：北京时间 12:42 完成三台实际进程、九条来源/Agent 运维路径、健康明细、目录、待处理命令和发布占用核查，无 findings；Agent 均空闲且运行，三台均连接三个来源。
- `backup-evidence.json`：六份原程序和文件备份摘要、身份、路径均通过，九份 SQLite 备份 `quick_check` 和 schema 校验通过。普通业务配置摘要未变；58/46 的 `state/station_versions.json` 是采集进程维护的运行状态快照，摘要随运行更新，未将旧状态恢复覆盖。
- `opc-after/`：六个 OPC 入口完成 Browse/Read/Subscribe，全部业务节点属性和 NamespaceArray 与 `opc-before/` 一致。30 管理端及各下级为 3300 个变量，58/46 管理端为 5297 个变量；每个入口抽取 12–13 个变量实际订阅并收到通知，无订阅错误。不是全量通知性能测试，也没有调用业务 Write/Call。
- `platform-after.json`：三设备版本检查成功，快照新鲜、无活动发布占用。`final-verification.json` 汇总 65 项即时核对全部通过；不把这个结果称为一小时稳定性通过。
- 三台 Agent 的 PID 和摘要均未变化，仍为 `0.6.3-unified.20261009.2`；平台 PID `1879278` 未变，仍为 `v2.19.16-rcd.4`，SHA-256 `7c55dd474a1c0f1d463a3bdb9d01d8e57ec890053317aa855df8f6607422f683`。本次没有重启 Agent 或 Semaphore。

新实际 PID 基线（所属服务 `NRestarts=0`）：

| 机器 | 管理端 | Agent（未重启） | 下级 |
| --- | --- | --- | --- |
| 30 | 3335957 | 1878131 | 3334805 |
| 58 | 4129629 | 1493834 | 4127284 |
| 46 | 1077883 | 1892088 | 1075596 |

北京时间 12:46 已通知恢复控制，此后维护授权结束；不再发布、重启、改配置、修标签或发送硬件命令。本轮未保证历史审计缺口已经恢复，也没有用物理站点的 `BadNoCommunication` 判断软件发布失败。

### 新版只读观察（已发现异常，保留采样）

三台各启动一个独立的 `rcd-transport-observe-20261011.service`，只读每分钟记录 24 小时后自然退出，不修改业务自启动。46 另启动有限时的 HTTP 观察器，记录三台管理端九路来源的重连计数、来源 revision、管理端启动时间和完整健康明细；密码只通过交互 stdin 传入内存，不落盘。原有观察日志未清理。

- 30：`/home/RCD93/code/rcd_transport_observe_20261011_pREUIa/runtime-observation.jsonl`。
- 58：`/home/lenovo/code/rcd_transport_observe_20261011_gJk3dR/runtime-observation.jsonl`。
- 46：本轮证据目录中的 `runtime-observation.jsonl`、`transport-http.jsonl`；启动记录为 `runtime-observers.json`、`transport-http-process.json`。

长测必须核对首条完整记录与上述实际 PID/SHA，读取全区间的 `issues`、`health_warnings`、健康字段和采样连续性；脚本 active、退出 0 或 HTTP 200 均不能代替验收。九路重连须结合启动时间、来源 revision 和计数重置判断，不能把不可比较计数记成零增长。既有 runtime 探针每分钟裸 TCP 连通检查可能产生连接关闭日志；应与 `BadSequenceNumberInvalid`、审计查询超时分别统计，不能无条件忽略全部 `BadCommunicationError`。

四份日志共同起点为北京时间 `2026-10-11 12:44:42.191`，以 `observer-baseline.json` 保存首样本、当前样本及与 postdeploy 的身份比对。最初核查的三台各三个 runtime 样本均无进程/端口/健康异常，PID、实际摘要、NRestarts 与新基线一致，但 HTTP 来源重连计数已增加，不能标为稳定。

### 即时检查之后发现的未解决问题

`transport-http.jsonl` 在 12:43:52–12:48:52 的五分钟内记录如下（顺序为连接下级 30 / 58 / 46）：

| 管理端 | 起始重连计数 | 结束重连计数 | 区间增量 |
| --- | --- | --- | --- |
| 30 | 4 / 5 / 2 | 6 / 7 / 2 | 2 / 2 / 0 |
| 58 | 3 / 3 / 2 | 5 / 5 / 2 | 2 / 2 / 0 |
| 46 | 1 / 1 / 0 | 3 / 3 / 0 | 2 / 2 / 0 |

- 各计数在同一 manager `started_at` 和来源 revision 下可比较。`source-events-after.json` 保存三个管理端最新 100 条来源历史（不是整个历史数据库），确认发生 `Connected → Disconnected`，随后恢复；不是单纯刷新页面或正常 Browse 导致计数变化。
- 示例：46 管理端连接 30 下级在 12:44:18.376、12:46:50.125、12:49:21.575 断开，连接 58 下级在 12:44:18.925、12:46:50.624、12:49:22.276 断开，间隔约 151 秒。事件仅给出 `source ... connection state Disconnected`，尚不足以确定底层原因。
- 12:46:52 的 46 健康明细出现 `command_tracking/rcd92-58: warning: lower OPC source is unavailable`，后续样本恢复；HTTP 仍是 200，不能据此忽略告警。其他健康检查在该样本为 ok。
- `transport-log-check.json` 检查 12:40 至约 12:48 的三台下级当前相关轮转文件和管理端/Agent/平台 journal，所查区间未匹配 `BadSequenceNumberInvalid`、审计查询超时、panic/fatal/segfault/OOM 关键词；这不证明所有历史错误已消除。三台各五条 `BadCommunicationError` 均发生于新 runtime 观察器每分钟裸 TCP 探测时刻、内容为握手前连接关闭，单独保留，不用于解释另外约 151 秒的真实来源重连。

结论为 **部署成功、瞬时功能检查通过，但连接稳定性未通过**。没有根据此次异常恢复旧配置、重投任务、重复升级或再次重启，也未将其归咎于串口物理站点。原自动验收任务继续保持暂停；三台 24 小时只读采样服务及 HTTP 观察器保留自然退出，全部证据保留。需另行定位周期性断链后再启动有效的通过性验收，不能等待一小时后自动抹去本段异常。

只读代码复核发现后续定位候选：管理端请求 50 ms publishing × 3000 keepalive，Go `publishTimeout` 得到 150 秒；Rust 过期 Publish 可能返回 `BadTimeout`，Go Publish 处理与 SecureChannel 全局错误监控对该状态的处理可能不一致，后者可能先置为 Disconnected，再加约一秒重连退避。该机制与 151 秒周期吻合，但本次尚未捕获现场 Publish ServiceFault，**只是待验证假设，不是已确认根因**；没有据此再次改动或上线。后续应在 46 隔离复现空闲订阅超过 150 秒的行为，并增加回归，不能用短于此周期的 Read/Subscribe 验收代替。

一小时及过夜均未验收；Windows、真实串口控制行为、全量端到端 P95 性能也不在本轮即时验收范围内。

## 151 秒重连的定位与修复（2026-10-11 后续）

本节为独立后续修复，不把前一版的异常区间算作通过。测试和构建全部在 46 的 `/home/lenovo/code/rcd_publish_timeout_20261011_lZuaZh/` 执行。本机只编辑、传输和核对源码。

### 已确认的机制

管理端请求 `50 ms publishing × 3000 keepalive`，下级返回同样的修订参数。旧 Go 客户端把 Publish 的超时也定为 150 秒，恰好与保活截止时间重合。真实 Rust 服务端先处理过期 Publish，再推进订阅 tick；调度偏差可使空闲 Publish 返回 `BadTimeout`。

Publish 自身处理允许补交下一次请求，但 secure-channel dispatcher 同时把这个 ServiceFault 发到连接监控；后者将其作为断链，管理端监督器约一秒后重连。频繁变值会提前完成 Publish，因此空闲/无变值来源更容易暴露问题。这不是串口卡每 151 秒停顿，也不是正常 token 续期。

`idle-before.log`：未修补客户端连接本轮已部署的真实 Rust `0.6.4-opc.20261011.1` 二进制；使用隔离配置、随机端口、禁用的不存在串口和常量 NamespaceArray，None 与 Basic256Sha256 均在约 151 秒变为 Closed，协议错误计数包含 `ua.StatusBadTimeout`。`timeout-unit-before.log` 进一步用真实 UA wire ServiceFault 确认错误升级路径；没有用协议测试替代真实 Rust 复现，也没有声称已抓到现场每一条超时报文。

依据 [OPC UA Part 4 RequestHeader](https://reference.opcfoundation.org/specs/OPC-10000-4/7.32)，过期 Publish 的 Bad_Timeout 结束的是单次请求，不是 Session。订阅生命周期到期的 `StatusChangeNotification(BadTimeout)` 是不同路径，不能吞掉。

### 改动与隔离验证

- 仅改 Go 客户端 fork：解码成功的响应头 `ServiceResult=BadTimeout` 仍交付原调用者，不再作为连接故障上报。Call 仍返回超时，不重发；真正的通道、传输和订阅到期错误保持原行为。
- Publish 请求增加保活余量：当前参数为 300 秒，并保留 UA 上限与溢出保护。不是降低采样速度，也不延长串口命令超时；sampling/publishing、ACK、重试和业务配置均未改。
- `idle-after.log`：两种安全模式都在原 Session/channel 连续 330 秒存活，无自动重连，期间 Read 成功，未再出现 `ua.StatusBadTimeout`。关闭测试连接时的通用字符串错误计数不计为存活阶段故障。
- `fork-race-after.log`：fork 全包 `go test -race -p 2 -count=3 -timeout=3m ./...` 通过，覆盖重复 Publish 超时、Call 超时不重发、真实通道故障、订阅生命周期超时通知、余量及溢出。
- `sequence-after.log`：真实 Rust 两种安全模式的取消、并发 Read/只读 Call/Subscribe/Renew 三轮回归通过，无硬件命令。
- 首次 `manager-race-after.log` 因隔离源码缺少生成的 `web/dist`，三个包 setup failed；原日志保留，不算通过。标准构建随后生成 Web 资源；最终 `manager-race-final.log` 中管理端 `go test -race -p 2 -count=1 -timeout=4m ./...` 全包通过、退出 0。
- 标准发布器生成版本 `0.6.4-opc.20261011.2` / 发布编号 `opc-publish-timeout-20261011-2`，`manager-build.log` 保留 Web 测试、生产资源构建、Go 全包测试和正式编译记录。包仅含管理端及公开清单，管理端数据库兼容范围仍为 16。
- 二进制 SHA-256：`167a286d0a0df0dfa5afc887d0d92027b652bb296d561b2f11701a331de5cce8`；发布包 SHA-256：`2009e9c19a45f6f0712b514a214e0951e351b8e3472c17aec63b1e9c5e05a48d`。
- 实际源码清单摘要：`cf3055b59ac6b2ac7261c4c246c6ac40dcdbc616433352d73f885bc45874f46d`；明确记录 dirty 快照、基线提交不冒充已提交修复。`source-verified.tar.gz` SHA-256 为 `48cb50c141e3f26864af6e36aac040566e15b13e5ada6064e5de554897a0335a`，不含 node_modules、生成 Web 资源、现场数据或凭据。
- 当前工作树与远端已测试的六份源码逐文件 SHA 一致；生产文件 `subscription.go` 为 `e924e85d35b9ef1b248e73ece21f8d890caa055ddd93046ee6f62cfdad1d99d1`，`uasc/secure_channel.go` 为 `c4ae770123218cc84895ed193723b9ab1193fde6b584849b063440e103f8aa01`。

本节编写时已取得当次三台管理端部署及控制暂停确认。现场部署结果另记；不重启下级、Agent 或平台，不覆盖配置、不改变安全标签，不重投旧任务。构建依赖安装报告已有 Web 依赖安全告警，本次未自动升级依赖或改变安全策略。

### 修复版现场发布

用户明确确认已暂停控制后，复查九条 Agent 运维路径空闲、无在途命令，按 30 → 58 → 46 通过 Semaphore 原生页面发布管理端 `0.6.4-opc.20261011.2`：

| 机器 | 平台任务 | 结果 |
| --- | --- | --- |
| 30 | 12163 | 成功，实际运行版本及摘要匹配目标包 |
| 58 | 12170 | 成功，实际运行版本及摘要匹配目标包 |
| 46 | 12174 | 成功，实际运行版本及摘要匹配目标包 |

上传只登记包，没有自动发布；每台只提交一次操作，三次均保存原生页面确认、操作编号和结果，未通过 SSH 替换安装目录文件。46 最后一项维护在北京时间 13:18:22 完成。`postdeploy.json` 九条运维路径、来源连接、健康明细和实际进程核对无 findings；这仍是即时结果，下面的观察窗口必须另外核实。

浏览器在 46 使用既有 Playwright（本会话 Browser 插件不可用），证据位于 `browser/upload-manager/` 和 `browser/upgrade-<host>-manager/`。实际登录→包上传→选择单台→控制暂停确认→创建原生任务→执行过程→实际版本核对均通过，1366/1920 确认页无横向溢出、无 pageerror，未改动前端。结果截图已人工核对，未把页面 Success 当作连接稳定性通过。

`backup-evidence.json` 三份管理端备份中的原二进制、文件摘要、SQLite `quick_check` 和 schema 校验通过，当前普通配置文件与备份一致。`opc-before/` 与 `opc-after/` 保留三台管理端升级前后点表；后者另含三个未重启下级的 Browse/Read/Subscribe，共六个入口实际读取订阅通过。节点属性等交叉比对和观察窗口结果以最终报告为准。

新观察基线为 `postdeploy.json`，完成于北京时间 13:19:57。30/58/46 管理端 PID 分别为 `3360247/4172518/1132380`，九条来源重连计数均为 0。下级 PID 保持 `3334805/4127284/1075596`；Agent 保持 `1878131/1493834/1892088`；平台 PID 仍为 `1879278`。沿用前节已存在的 24 小时采样服务和 HTTP 观察器，不清日志、不重启探针；旧探针会因本次授权的管理端 PID 更换写入身份变更标记，只有逐样本 PID、SHA、NRestarts 全部匹配新基线时才能解释为该次发布，不能无条件忽略。

### 修复版现场观察结论

`observation-result.json`、`runtime-window-{30,58,46}.json`、`http-window.json` 保留新基线之后的完整逐分钟区间：

| 范围 | 北京时间 | 样本 | 结果 |
| --- | --- | --- | --- |
| 30 实际进程/端口/健康 | 13:20:41–13:31:41 | 12 | 约 660 秒，无缺样、无新增故障 |
| 58 实际进程/端口/健康 | 13:20:42–13:31:42 | 12 | 约 660 秒，无缺样、无新增故障 |
| 46 实际进程/端口/健康 | 13:20:42–13:31:42 | 12 | 约 660 秒，无缺样、无新增故障 |
| 三管理端九路来源 HTTP | 13:20:52–13:31:52 | 12 轮 | 全程 Connected/Active，九路重连计数均为 0 |

最大采样间隔 60.087 秒。所有进程的实际 SHA、PID 和 `NRestarts=0` 逐项与新基线相符；每台原观察器的 12 次身份变化提示仅对应已授权的管理端升级，没有无条件忽略未知变化。完整健康字段无告警，HTTP 200 不作为唯一依据。

`final-verification.json` 共 46 项交叉核对通过：三台管理端运行/磁盘摘要匹配目标包、来源 revision 与启动时间稳定；下级和 Agent 的 PID/摘要与升级前一致，平台 PID/摘要/重启计数也一致；三份备份完整，原普通配置和发布绑定未变；管理端 NamespaceArray、业务节点属性与升级前一致，六个入口实际 Browse/Read/Subscribe 成功。三台来源事件按固定区间 13:19:57–13:32:36 分页查询，事件总数均为 0，不只查看最后状态。

`post-release-logs-final.json` 扫描 13:19:57 至约 13:33 的下级相关轮转文件及管理端、Agent、Semaphore journal，没有匹配新的 `BadTimeout`、`BadSequenceNumberInvalid`、审计查询超时、panic/fatal/segfault/OOM 等诊断项。各下级 13 条 `BadCommunicationError` 都是每分钟探针时刻的“等待建立通道时连接关闭”，与既有只读裸 TCP 检查对应，单独保留；没有用此归因掩盖已建立 Session 的断链。管理端日志文件该区间没有新写入，不伪称扫描到了新的内容。

`platform-after.json` 三设备检查成功、实际版本更新、发布占用为空，版本快照新鲜；本次平台未重启。三项 UI 操作只有各一次发布 POST，没有重投历史失败任务。本次是后端客户端修复；平台上传、确认和执行结果页面按前端技能验收，未进行新的前端改造或全面移动端复测。后续深入前端开发可使用 Browser 插件；本轮按远端执行约束使用现有 Playwright。

结论：**151 秒问题已复现、修复、远端回归通过并部署；当前专项现场验证通过**。已通知恢复控制。原 24 小时日志采样保留自然退出，旧自动验收任务仍暂停，未擅自恢复旧版本的验收计划。一小时、过夜、真实控制命令和全量 P95 压测未在本轮完成，不声称通过。
