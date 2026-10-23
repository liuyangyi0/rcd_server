# 架构整理与命令追踪：实施记录

最新状态：2026-09-28 用户另行授权并确认暂停控制下发，30/58 下级及 58 管理端已升级到 46 的已验证阶段版本。46 程序此次不替换或重启。部署详情见文末；前述各批“未部署”是当时状态，不代表当前状态。

## 基线与边界

- 开发 worktree：`rcd_server_device_groups`，分支 `codex/opc-device-groups`。
- 起点提交：`769ef2a1f82e6e7bb5e77f9e79a3e88434005e42`，包含上一轮尚未提交的串口补发审计功能；这些改动完整保留。
- 编辑前源码快照：`artifacts/runs/2026-09-22-architecture/baseline-source.tar.gz`；SHA-256：`bb0d87205e824c84e37f67c93a583a30c11081ef2a6d610ba728213f3ab9d98b`。
- 远端隔离源码：`192.168.10.46:/home/lenovo/code/rcd_architecture_20260922`。不与真实程序共用配置、数据库或串口。
- 本次没有部署、重启、现场设备命令、Git 提交或推送授权。

## 本轮实现

- 普通日志接收状态、来源锁及健康状态迁出 Runtime；文件快照固定有效字节边界，扫描时释放写锁，只保留当前页正文和去重键。增加取消、5 秒查询预算和 Windows 可删除共享句柄（Windows 只做交叉编译）。
- 命令执行元数据、独立审计缓存/补存、下级关联能力/同步分别由组件拥有。RouteTable 仍独占物理端口 pending/组选源锁，没有第二套占用表。命令审计写入移出共享配置锁。
- 下级 Operations v1 增加 command_tracking、submit/get/list 和 CommandRevision；rusqlite 固定 0.37.0 bundled，Cargo.lock 已由远端解析并同步。
- 下级 SQLite 独立写线程、100 ms 接收持久化预算、降级继续控制、内存有界、重启未知不重投、恢复序号重排、重复编号/内容冲突处理；串口控制事务移到 command_transaction。旧直接 Method 不等 ACK 的行为保留，独立记录为 SentUnconfirmed，不冒充 ACK 成功。
- 审计库明确使用 schema 1。降级期间首次创建的审计库沿用当前实例标识，恢复再重启后保持一致；写入失败时尽可能只读获取已有实例标识。恢复时发现不同实例或较新 schema 则保持降级并告警，不混写、不改写旧身份、不降级数据库结构，控制仍继续。
- 管理端持久化 manager_instance_id、固定目标、变化记录和游标；含不确定新接口响应不回退旧协议、迟到结果只补充证据、审计故障不中断控制/不保留已结束命令的端口锁。
- 各来源独立补查，进行中命令短周期补查，空闲来源 revision 通知配合较慢兜底，慢来源不串行阻塞其他来源。补存每批有时间预算，避免恢复积压阻塞结果查询。
- 新详情/事件 REST、列表轻量摘要、Web 执行过程抽屉/审计告警及旧协议提示；减少无关 SSE 对概览的失效。Rust、Go、TypeScript 共用黄金快照。
- 增加显式目标 Linux 发布工具，包含源码/依赖摘要、隔离构建、产物校验、SQLite 一致性备份和配套回滚；本轮未实际运行部署功能。

## 已执行验证

全部可执行验证在 192.168.10.46 隔离源码目录执行。本机只修改、检查、传输源码和查看远端生成的截图。

| 验证 | 实际结果 | 远端证据 |
| --- | --- | --- |
| `cargo test --locked --lib -j2` | 359 通过，0 失败，1 忽略（load_user_calc_real）；含审计恢复后再次重启、不同实例拒绝混写及新 schema 保护 | architecture-rust.log |
| `cargo build --locked -j2` | Rust 二进制 debug 构建成功；不是发布版性能结果 | architecture-rust-build.log |
| `go test -race ./...` | 全包通过；最终命令状态组件拆分后重新执行，app 66.101 秒，其余无变化包使用测试缓存 | architecture-race.log |
| `go test -race ./internal/lowerlogs`（最后补测） | 通过；取消的查询不再等待来源写锁，1.221 秒 | architecture-log-followup.log |
| `go build -trimpath -o ../RcdManager.architecture ./cmd/rcd-manager` | 成功 | 下述产物摘要 |
| `GOOS=windows GOARCH=amd64 go build ./internal/lowerlogs` | 通过；没有 Windows 运行验收 | architecture-windows-build.log |
| `npm test`、`npm run build` | 30 文件、185 项测试通过；TypeScript/Vite 构建成功 | architecture-web.log |
| `node tools/opc/command-tracking-browser.cjs` | 降级告警、迟到 ACK、64 位 ID、分页、旧协议、桌面/手机抽屉通过；无 JavaScript 异常 | architecture-browser.log、artifacts/runs/architecture-browser/ |
| `python3 -m unittest discover -s tools/release` | 4 项通过；校验摘要、目标隔离和 EXE/SQLite 配套恢复，控制器是隔离测试替身 | architecture-release-tools.log |

管理端隔离构建产物：`/home/lenovo/code/rcd_architecture_20260922/RcdManager.architecture`。
SHA-256：`10e946229d6800666b6e5c6a3d89aa59f2f00c67dde19c8bfdd1c9d6d6424d2f`。
它没有替换任何现场程序。

新增回归涉及：重复提交/内容冲突、两管理身份的端口竞争、原 `_Send` 与关联入口互斥、回复不明确禁止旧协议重发、旧 ACK 不能结束关联命令、审计失效仍能执行/解锁、恢复只补存、重启未知、worker 异常中断、迟到 ACK 保留原超时、事件去重与游标事务、旧 TCP 字段不变、日志追加/扫描/轮转并发。
此前已有的首次/第二次/第三次 ACK 立即停止测试随 Rust 全库回归通过；没有调整实际重试次数或串口时序。

## 尚未完成，不作为已验收

这是阶段性交付，**并非整份架构计划已经完成**：

1. Runtime 的来源状态汇总、目录索引/隔离状态和站点快照缓存等尚需迁移；已完成日志、命令执行、审计关联、运维占用、计算运行状态、冗余策略/检测状态、目录落盘队列/发布恢复，以及来源监督器句柄/generation 边界。计算配置持久化、目录/路由一致性提交仍由 Runtime 协调；来源工厂创建、冗余检测、目录发布/保存仍保留配置事务范围，短事务优化尚未完成。
2. SerialManager 中普通轮询、连接恢复和错误统计仍需后续拆分；第二批增加版本事务与传输原语拆分，未改写采集时序。
3. 已执行第二批所列的实际 Rust/Go OPC 网络互操作；尚未完成成功设备 ACK、真实双管理端控制竞争及全部新旧版本组合，不能将停用串口的拒绝测试当成现场控制验收。
4. 已测审计不可用、恢复、异常中断、10,000 条缓存边界及保留清理逻辑；真实磁盘满、只读目录、慢写饱和及进程强杀的完整故障矩阵仍未跑完。清理测试使用确定时间，不是实际等待 7 天。
5. 浏览器验证是隔离响应样例，不代表真实设备动作或跨端最终一致性已验收。
6. 同负载 P95（通知 ≤500 ms、计算 ≤50 ms）、一小时稳定性和过夜真实运行未完成，不报告达标数字。
7. 发布工具仅单元验证，实际预检/部署/回滚流程仍须在获授权的明确目标上演练；Windows 服务与句柄轮转仍需真实 Windows 验收。

未提交、未推送、未部署、未重启现场；46/30/58 既有程序与配置未被本轮测试替换。

## 第二批：运维边界、版本事务与网络互操作

延续同一未提交开发快照及 46 隔离目录，不替换第一批产物或现场二进制。

实现：

- `maintenanceService` 拥有运维修改、进程栅栏和执行租约；Runtime、冗余判断及后台补查不再直接修改这三张 map。按请求编号/租约释放，避免旧请求结束误清新请求。未知进程结果继续保留栅栏。
- 运维进程入参和结果类型迁到 domain，HTTP 保留类型别名，原 JSON 不变。
- Rust 分离同步传输原语，以及版本查询调度/响应/隔离；串口仍只有唯一 worker。没有调整 50 ms 超时、版本迟到隔离窗口、线速预算、校验位保护或命令重试配置。
- 修复审计内存满且全为进行中记录时，容量拒绝错误地消耗命令编号的问题；拒绝后可以在容量释放后重新提交原编号。真正已接收、后被淘汰的记录仍禁止重投。
- 审计保留清理使用单独 SQLite 事务；清理失败告警，不再静默忽略。清理后游标 head 不回退，运行中记录不删除。

新增验证（全部远端）：

| 检查 | 结果 | 证据 |
| --- | --- | --- |
| `cargo test --locked --lib -j2` | 363 通过、0 失败、1 忽略；含原串口时序/ACK 回归及新增队列、缓存淘汰、期限/数量清理测试 | architecture-phase2-rust.log |
| `cargo build --locked -j2` | debug 构建成功 | architecture-phase2-rust-build.log |
| `go test -race ./internal/app ./internal/api ./internal/domain` | 通过，app 61.863 秒、api 24.233 秒、domain 1.025 秒 | architecture-phase2-go.log |
| 实际 Rust 二进制 + Go LowerSupervisor 回环 OPC 测试 | 通过，含 race；约 16.81 秒 | architecture-phase2-interop.log |
| `go test -race ./...` | 全包通过；opc 61.384 秒、store 10.852 秒，先前已测未变化包使用测试缓存 | architecture-phase2-all-go.log |

第二批管理端构建：`RcdManager.architecture-phase2`，SHA-256 `67edb47c90f9bec50f20f56d187071fa807d54796f2869a45e3bdf398a6a0217`，仍保存在远端隔离源码根目录，未部署。
本批涉及的已格式化、已验证源码增量存档：`artifacts/runs/2026-09-22-architecture/phase2-verified.tar.gz`，SHA-256 `4c4e967ce2eee8e8c39afa8977d26a0487d292adca019fe079ab8dd8d6019360`。这是本批增量文件，不冒充完整仓库快照。
证据已取回 `artifacts/runs/2026-09-22-architecture/evidence/architecture-phase2-*.log`。

网络测试通过显式环境变量选择本轮隔离构建的 Rust 二进制，再复制到测试临时目录启动。测试自行生成配置/数据库/PKI，使用回环临时端口；唯一配置串口位于临时目录，既不存在又明确停用。没有模拟器、真实串口、现场配置或服务管理操作。

已验证 None/Anonymous、Basic256Sha256/SignAndEncrypt + Anonymous/Username 三种连接，跟踪能力读取、停用串口拒绝（零次发送）、重复编号去重、内容冲突、错误 boot 拒绝、CommandRevision 订阅、事件拉取、进程重启后实例身份稳定与历史记录可查。

仅在隔离环境执行：

```sh
RCD_TRACKING_INTEROP_BINARY=/absolute/isolated-build/rcd_server \
  go test -race ./internal/opc -run TestCommandTrackingRealRustInterop -count=1 -v
```

未设置该变量时跳过此测试。测试不接受外部 OPC 地址或串口参数，不能用于向现场设备发送测试控制命令。

## 第三批：计算服务状态所有权与并发边界

继续在 46 同一隔离源码目录验证，不改现场部署和配置；Rust/Web 源码本批未修改。

- Runtime 的计算可变字段移入 `calculationService`：活动/待重启版本、Runner、结果与退役点、脏输入、默认路由 revision、状态输入签名、事件节流及唤醒队列。
- HTTP/OPC 入口仅转换查询结果；状态 DTO 定义移到 calculation 包，HTTP 使用兼容别名，JSON、NodeId、数据类型不变。
- 单独串行锁保护 Runner；计算期间查询状态/结果不等待整个批次。每批输入读取仍受短配置读事务保护，但表达式求值不再占用 Runtime 配置读锁。
- 配置激活/停用、默认源切换及来源/目录失效会使旧 generation 作废。旧批次不能覆盖新结果；批次运行期间到达的脏输入保留给下一批。输入快照失败会重新安排全算。
- 结果集合及通信中断依赖列表返回副本，不向调用者暴露组件内部可变切片。
- 保留默认源共享计算、Session 独立选源不影响计算、通信中断输入清零后继续求值、按实际执行路径传播质量、热退役及待重启类型变更语义。未引入计算历史库。

新增并发测试覆盖旧批次在切源/目录失效/激活/停用后被丢弃、读者不等待求值、输入不丢失、快照失败重算、返回集合隔离及多调用者 Runner 串行化。

首次定向 race：`architecture-phase3-focused.log`，2.787 秒通过。首次全包运行发现站点测试的手工 Runtime 未初始化新组件；已补齐测试初始化，保留失败日志 `architecture-phase3-race.log` 供追溯。修正后的计算/站点定向 race：`architecture-phase3-followup.log`，1.096 秒通过。

最终 `go test -race ./...` 全包通过，证据为 `architecture-phase3-race-final.log`；app 61.232 秒，其余本轮已验证、无新增改动的包使用缓存。`go build -trimpath` 成功。

- 隔离产物：`/home/lenovo/code/rcd_architecture_20260922/RcdManager.architecture-phase3`。
- 二进制 SHA-256：`a9383797fa72f0abde265055c10a3ee0edf3673f12110002e13c7cec526faa32`。
- 已验证源码增量：`artifacts/runs/2026-09-22-architecture/phase3-verified.tar.gz`，SHA-256：`ddc2150083a9b0b2e62f5f4d055761af184d9f15eb2e342cfcbd0a91233e9498`。
- 本批日志已取回同目录 `evidence/architecture-phase3-*.log`；本机 `git diff --check` 通过。

没有重复宣称本批执行 Rust 或浏览器验收；相关源码未改，前两批证据仅作历史记录。尚未测出当前完整部署负载的 P95，也未完成一小时/过夜测试。未部署、未重启现场、未提交或推送。

## 第四批：自动切源与差异告警状态边界

继续使用 46 的隔离目录，不修改现场服务、配置或物理串口；本批只修改 Go 管理端及说明文档。

- `redundancyService` 独占策略、健康确认计时、差异确认计时、冷却及告警状态，Runtime 不再直接读写这些 map。
- 候选判定、成员确认与差异比较使用独立检测副本；组件锁仅用于取快照和提交。网络、缓存扫描、路由事务和 SQLite 不在该组件锁内执行。
- 提交检查 generation；策略更新、组删除后重建、告警恢复或新检测已提交时，旧检测不得覆盖当前状态。外部选源/告警副作用仍由 Runtime 配置事务串行协调，generation 只保护状态提交，不能代替命令锁和路由 revision。
- 保持原规则：故障/正常持续确认、切源冷却、命令等待 ACK 时锁定、不自动回切、不补空选源；差异不可比较时保持告警，换连接或中断后重新确认；策略修改清空确认计时，但保留切源证据和冷却。
- 修复删除组清理重试仍受上一次 `LastError` 阻挡的问题；重试前清空旧错误，本次持久化失败仍保留状态等待下次重试。
- 状态查询仅复制公开摘要，不复制按点保存的差异确认计时表，避免 OPC/HTTP 诊断读取的内存分配随内部计时点数增长。

新增 9 个测试函数（含旧检测失效的多个子场景），覆盖副本隔离、策略/删除/重建/告警/新检测的 generation、计时重置、慢扫描不持有组件锁、Unknown 告警保留、切源阈值与命令锁、清理重试和并发访问。既有 Runtime 自动切源回归继续执行。

| 检查 | 结果 | 证据 |
| --- | --- | --- |
| 首轮 `go test -race ./internal/app -run Redundancy -count=1` | 通过，5.231 秒 | architecture-phase4-focused.log |
| `go test -race ./...` | 全包通过，app 60.967 秒；未修改的其它包使用缓存 | architecture-phase4-race.log |
| 增加清理重试回归后，再次运行全部 Redundancy 测试 | 通过，5.686 秒 | architecture-phase4-final-focused.log |
| 优化摘要复制后，最终 `go test -race ./...` | 全包通过，app 61.682 秒；未修改的其它包使用缓存 | architecture-phase4-race-final.log |
| 诊断摘要微基准，`-benchtime=100ms` | 0 / 10,000 条内部计时记录均为 64 B/op、1 alloc/op；192.9 / 198.1 ns/op | architecture-phase4-benchmark.log |
| 最终 `go build -trimpath` | 成功 | architecture-phase4-build-final.log |

- 最终隔离产物：`/home/lenovo/code/rcd_architecture_20260922/RcdManager.architecture-phase4-final`。
- 二进制 SHA-256：`e4218b2e144646bf9f51257eeb29bb8ce7753287f6c350fc53186ed7b947760e`。
- 最终已验证源码增量：`artifacts/runs/2026-09-22-architecture/phase4-final-verified.tar.gz`，SHA-256：`bb74f2c4005721f794f8872b184d2ce7a52e56c063af6eb5bf7f6f10db9cdbc8`。只包含本批 8 个 Go 文件，不是完整仓库快照。此前 `phase4-verified.tar.gz` 及中间产物保留用于追溯，不作为最终版本。
- 远端格式化源码与日志已取回，证据位于 `artifacts/runs/2026-09-22-architecture/evidence/architecture-phase4-*.log`；本机只执行源码检查，`git diff --check` 通过。

本批尚未缩短整个 Runtime 冗余检测的配置锁范围，不能声称已完成来源/目录组件或全局锁优化。摘要微基准不是完整负载下的通知/计算 P95 验收。没有重跑 Rust、浏览器、现场控制或长时性能验收；一小时/过夜和 P95 仍未验收。未部署、未重启现场、未提交、未推送。

## 第五批：目录落盘与上级发布恢复

继续在 46 原隔离源码目录验证，仅修改 Go 管理端及说明；没有访问真实串口、现场数据库或服务管理接口。

- `catalogPersistence` 拥有每来源待落盘目录副本、内部递增版本和保存错误；Runtime 不再直接修改待落盘 map 或全局目录保存错误。写入者串行执行，但数据库 I/O 不持有组件状态锁，状态查询和入队不等待慢写入。
- 保存完成按内部版本确认，而不是 `DiscoveredAt`。相同发现时间的新目录、删除后重新入队的目录不会被旧结果清除；失败保留最新待写快照，各来源错误分别恢复。入队和传给写入者的目录均为独立副本。
- `catalogPublication` 拥有目录校验/发布回调及失败重试状态。新发现、配置变更、计算激活和停用统一走发布入口；回调替换后，旧调用结果不能覆盖新一代发布健康状态。
- 修复中央计算停用已提交、上级点表发布失败却没有标记重试的问题。恢复时按最新下级计算目录重试，而不是再次执行停用操作。
- 初次 `SetCatalogHandlers` 与热目录提交使用同一配置事务，避免初始旧快照在新目录之后发布。给校验/发布分别提供目录和元数据副本，校验不能修改随后发布的数据。
- 发布失败与来源目录隔离分别保留。成功发布不掩盖仍在隔离的来源；隔离解除后也不会留下已经恢复的旧发布错误。
- 发布副本在取出不可变索引指针并释放 Runtime 状态锁后构造。目录写库及发布仍保留配置事务协调，未宣称已完全移除共享配置锁内的 I/O。

新增 10 个测试函数，覆盖快照所有权、同时间戳替换与删除重建后的旧结果、慢写入期间状态访问、来源级错误、取消与写入串行、回调 generation、初始校验失败不进入发布重试、初次注册与热更新次序、回调输入隔离、隔离/发布故障分别恢复，以及中央计算停用的发布恢复。

首轮专项测试失败于新测试夹具：仅手工设置节点 `Kind`，没有提供真实协议要求的 Topology 计算声明，因而被识别为原始点冲突，未进入目标测试场景。已补齐 Topology；保留 `architecture-phase5-focused.log`，没有为绕过测试放宽业务目录校验。

| 检查 | 结果 | 证据 |
| --- | --- | --- |
| 修正后 `go test -race ./internal/app -run "Catalog\|Quarantine\|Calculation\|SourceReset" -count=1` | 通过，11.909 秒 | architecture-phase5-focused-final.log |
| `go test -race ./...` | 全包通过，app 63.307 秒；未修改的其它包使用缓存 | architecture-phase5-race.log |
| `go build -trimpath` | 成功 | architecture-phase5-build.log |

- 隔离产物：`/home/lenovo/code/rcd_architecture_20260922/RcdManager.architecture-phase5`。
- 二进制 SHA-256：`7c31206034ab9347c8ade33b9b15ffbb41ca58b7c3f20fa2eac7aa3cac8dd647`。
- 已验证源码增量：`artifacts/runs/2026-09-22-architecture/phase5-verified.tar.gz`；SHA-256：`60fb1786aeeca9e1cc6bbe56b243dee2081658d1e87073ec065fd025d9377b1d`。仅本批 10 个 Go 文件，不是完整仓库快照。
- 远端格式化源码和全部本批测试日志已取回；证据位于同目录 `evidence/architecture-phase5-*.log`；本机 `git diff --check` 通过。

未执行 Rust/Web 新验收、完整负载性能或一小时/过夜测试。来源连接、目录索引整体迁移及共享配置锁优化仍待完成。未部署、未重启现场、未提交、未推送。

## 第六批：来源监督器生命周期与退出状态

继续只在 46 隔离目录构建和执行测试；不使用现场连接、串口、数据库或配置。

- `sourceConnections` 独占每来源句柄和 generation；Runtime、命令派发、下级运维、缺失连接重试等入口通过组件方法访问，不再直接修改连接 map。
- `source_lifecycle.go` 负责工厂创建、启动/停止和回调适配。Runtime 状态锁继续协调“代次检查 + 状态/目录/缓存写入”，防止只检查后解锁所产生的旧回调竞态；组件状态锁中没有网络、凭据解密或取消调用。
- 工厂失败（包括返回 nil client）、监督器退出均立即废弃生产者代次。旧 worker 结束不能清掉新 worker；关闭除了已经挂接的连接，还会废弃尚在创建中的预留代次。ID 删除再创建不复用旧代次。
- 修复异常退出报告使用 local epoch 0，被已有较新 epoch 拒绝、可能继续显示在线的问题。现在先废弃旧生产者，再保留最后全局 epoch 上报断开；终态使用单独内部令牌，新启动/停止会使它失效，不能覆盖后来连接。
- 退出时释放子 context；所有连接取消函数移到 Runtime/组件状态锁外执行。连接运行失败不再借用已有 `CatalogConflict` 字段触发新的拓扑隔离原因。
- 保持独立物理来源监督器、原重连退避、epoch 编码、命令目标固定、Session 选源及计算质量规则；未调整串口时序和重试次数。

新增 7 个测试函数（包含成功返回/错误返回、工厂错误/nil 返回等子场景）。覆盖替换与旧 worker 结束、重复挂接拒绝、创建中关闭、ID 重建、并发访问、异常退出离线状态与旧状态/目录/样本拒绝、子 context 释放、工厂失败后的回调拒绝、取消锁，以及连接失败不变成拓扑错误。已有关闭 deadline、动态创建与关闭、重连计数、命令/运维测试随全包回归执行。

| 检查 | 结果 | 证据 |
| --- | --- | --- |
| 生命周期/退出/工厂/迟到回调专项 `go test -race ./internal/app -run ... -count=1` | 通过，6.157 秒 | architecture-phase6-focused.log |
| `go test -race ./...` | 全包通过，app 65.833 秒；未修改的其它包使用缓存 | architecture-phase6-race.log |
| `go build -trimpath` | 成功 | architecture-phase6-build.log |

- 隔离产物：`/home/lenovo/code/rcd_architecture_20260922/RcdManager.architecture-phase6`。
- 二进制 SHA-256：`085a09ceb453f9719ce264ba52e6d803e186826923a9d91f6226f6ce2f186009`。
- 已验证源码增量：`artifacts/runs/2026-09-22-architecture/phase6-verified.tar.gz`；SHA-256：`42fc2a622b10fb9c95bbbe3bb3a2f617a85c39c717b59c5536236cb2f613f1e9`。仅本批 13 个 Go 文件，不是完整仓库快照。
- 格式化源码和本批日志已取回；证据在同目录 `evidence/architecture-phase6-*.log`；本机 `git diff --check` 通过。

本批是隔离的逻辑/并发验收，没有新增现场 OPC 连接或物理控制验收。来源状态汇总、目录索引、站点缓存及共享配置锁优化仍待完成；未执行 Rust/Web 新验收、整机 P95、一小时或过夜测试。未部署、未重启现场、未提交、未推送。

## 第七批：站点刷新隔离与版本缓存恢复

继续在 46 的同一隔离源码目录验证，只修改 Go 管理端；没有连接现场 OPC、访问真实串口或操作现场数据库。

- 新增 `stationSync`，拥有刷新票据、合并失效标记、能力缓存、各来源保存错误和写入协调锁。保留 64 项队列、4 个刷新 worker、2 秒状态扫描和 3 秒刷新期限；不调整串口轮询、版本请求、命令重试或业务选源语义。
- 用唯一票据代替按来源 ID 清除 pending。来源删除/重建、重复通知和旧刷新结束不会清掉新任务；队列满释放本次预留，后续周期扫描可以重试，不留下永久 pending。
- 能力查询在锁外执行，返回后在 Runtime 状态锁内复核来源存在、连接 generation、epoch、在线状态与拓扑。重连、拓扑变化、停止或删除期间的旧回复不再覆盖新能力缓存，也不继续发起后续状态查询。
- 修复版本写库失败后，相同内容的状态刷新被提前去重、无法重试保存的问题。失败时继续提供内存快照；后续有效刷新重试。恢复后，纯运行标志变化仍只更新内存，不写成实时 SQLite 数据流。
- 按来源保留保存错误，另一来源成功不掩盖失败来源；修复整组级联删除未清理站点快照、epoch 和能力/刷新状态的问题。
- 来源清理与站点写入/迟到回滚共用写入协调锁，防止删除完成后旧写入恢复记录。此锁仍跨来源串行，且删除仍保留配置事务协调；没有宣称全局锁优化或整个站点快照组件迁移已完成。

新增 6 个测试函数，包含 5 种迟到能力回复子场景和单来源/整组删除子场景；同步调整原刷新合并测试。覆盖删除重建票据、并发合并、队列满恢复、来源级错误隔离、epoch/拓扑/generation/离线/删除栅栏、SQLite 写入故障后相同快照重试，以及恢复后纯运行状态刷新不重复写库。数据库故障由隔离临时 SQLite 触发器注入，不修改现场数据库；不是整机磁盘满或硬件验收。

| 检查 | 结果 | 证据 |
| --- | --- | --- |
| `go test -race ./internal/app -run "Station\|SourceReset\|DeleteGroup\|DeleteSource" -count=1` | 通过，6.675 秒 | architecture-phase7-focused.log |
| `go test -race ./...` | 全包通过，app 69.432 秒；未修改的其它包使用缓存 | architecture-phase7-race.log |
| `go build -trimpath` | 成功 | architecture-phase7-build.log |

- 隔离产物：`/home/lenovo/code/rcd_architecture_20260922/RcdManager.architecture-phase7`。
- 二进制 SHA-256：`ba6efa865bd93b288f4c67bceabcade118cbe61ae848d99f309538d7c57db977`。
- 已验证源码增量：`artifacts/runs/2026-09-22-architecture/phase7-verified.tar.gz`；SHA-256：`fbf350d49df5a481bf5d023235678bc9a19176d1bfa3cf9b7853bab0288baa3a`。仅本批 7 个 Go 文件，不是完整仓库快照。
- 格式化源码和本批日志已取回，压缩包 SHA-256 一致；证据在同目录 `evidence/architecture-phase7-*.log`。本机仅检查/编辑/传输源码，`git diff --check` 通过。

Runtime 仍持有站点实时快照与 epoch、来源状态汇总和目录索引，后续继续拆分并保持连接/拓扑的一致性。未新增 Rust/Web 验收，整机 P95、一小时/过夜、完整故障矩阵和真实设备验收仍未完成。未部署、未重启现场、未提交、未推送。

## 第八批：不可变站点快照与通信状态查找

继续仅在 46 隔离目录执行 Go 测试、基准和构建，没有连接现场 OPC 或访问串口。

- 新增 `stationSnapshots`，独占每物理来源的站点快照、接收 epoch、有效标记及串口/站点查找索引。Runtime 不再持有或直接修改站点快照和 epoch map；仍负责将连接/拓扑检查与缓存提交放在同一个一致性边界内。
- 快照写入时深拷贝嵌套切片和可选版本指针；对外返回独立副本。心跳与失效只替换不可变快照头，旧读者持有的视图不会随新数据或删除变化。
- `StationVersions` 不再为复制整个来源站点列表持有 Runtime 状态锁。Web 版本列表在一致性锁内固定快照视图，与状态/拓扑对应，再在锁外复制站点正文；拓扑本身的复制仍保留原协调方式。
- 通信状态使用 `(port_id, device_id)` 二元键索引，不再扫描所有站点。索引不拼接分隔符，避免特殊 ID 冲突；状态数值、15 秒过期、离线、未知及不支持语义保持不变。索引优化不取消 Runtime 的当前拓扑指纹校验。
- 启动读取 SQLite 的快照只作为缓存，不获得有效连接 epoch；重连或拓扑失效后仍须等新的状态回执，不能把旧的“正常”用于计算状态输入或自动切源。
- 来源删除、级联删除、拓扑变更、状态刷新、计算状态输入和冗余测试统一改走组件方法，没有为了兼容旧测试保留第二套状态 map。

新增 6 个测试函数，覆盖嵌套数据所有权、固定视图在心跳/失效/删除后的稳定性、13 种通信状态场景、物理站点索引与原遍历结果一致、并发读写/删除，以及从真实临时 SQLite 恢复后必须等待新连接回执。相关计算状态输入、自动切源及上一批恢复测试随专项与全包执行。

| 检查 | 结果 | 证据 |
| --- | --- | --- |
| `go test -race ./internal/app -run "Station\|StateInput\|CalculationState\|Redundancy\|SourceReset\|DeleteGroup\|DeleteSource" -count=1` | 通过，13.829 秒 | architecture-phase8-focused.log |
| `go test -race ./...` | 全包通过，app 70.356 秒；未修改的其它包使用缓存 | architecture-phase8-race.log |
| `go test ./internal/app -run "^$" -bench BenchmarkStationSnapshotsCommunication -benchmem -benchtime=100ms` | 通过，组件查询均为 0 B/op、0 allocs/op | architecture-phase8-benchmark.log |
| `go build -trimpath` | 成功 | architecture-phase8-build.log |

基准查询每个快照的最后一个站点，比较组件索引与原 `Device` 遍历；数据规模为站点数，不是业务点数：

| 站点数 | 索引查询 | 原遍历 |
| --- | --- | --- |
| 16 | 129.6 ns/op | 267.0 ns/op |
| 1,024 | 142.3 ns/op | 10,426 ns/op |
| 5,300 | 126.1 ns/op | 57,443 ns/op |

该基准不包含 Runtime 拓扑指纹计算、OPC 读订阅、网络、快照构造或真实采集负载，不作为整机通知/计算 P95 验收，也不表示现场有 5,300 个站点。

- 隔离产物：`/home/lenovo/code/rcd_architecture_20260922/RcdManager.architecture-phase8`。
- 二进制 SHA-256：`19034d0b376c0d881cb1281b3ae02aa1b3db3abd673a0f5278c996157a56dfb6`。
- 已验证源码增量：`artifacts/runs/2026-09-22-architecture/phase8-verified.tar.gz`；SHA-256：`3b1fdc285f6912d83062fb6999b464a435adef71b3ae3eca535759879a9943cf`。只含本批 11 个 Go 文件，不是完整仓库快照。
- 格式化源码和日志已取回，压缩包校验一致；证据在同目录 `evidence/architecture-phase8-*.log`。本机 `git diff --check` 通过。

来源状态汇总、目录索引/隔离状态、共享配置锁范围和 Rust 剩余职责拆分仍待完成；站点落盘协调仍跨来源串行。未新增 Rust/Web、真实硬件或长时验收，整机 P95、一小时/过夜及完整故障矩阵仍未完成。未部署、未重启现场、未提交、未推送。

## 第九批：来源事件与告警重试组件

继续只在 46 隔离目录构建和测试；没有接入现场 OPC、串口或现场数据库。

- 新增 `statusHistory`，独占来源事件 FIFO、按来源合并的告警目标、内部工作版本、保存错误与写入协调锁。Runtime 不再直接维护这些队列或状态保存错误，健康检查和系统待写数量统一从组件读取；管理审计及来源实时状态尚未迁移。
- 修复历史事件保存失败时跳过整轮告警的问题。事件和告警分别尝试并报告错误，一个来源的告警失败也不阻止其它来源；告警成功不能把仍未恢复的历史写入错误清掉。
- 事件按入队 FIFO 重试，每轮只处理开始时已有的事件，执行中新增事件留到下轮，避免新到事件不断延长本轮。没有引入事件丢弃、磁盘队列或新的数据库 schema。
- 告警成功按内部递增工作版本确认。即使后来排队的状态内容、时间戳或 epoch 完全相同，旧写入也不能清掉新工作。
- 状态通知可能在提交后迟到：保留原历史转换，但入队时在 Runtime 状态锁内重新读取权威当前告警目标，避免旧断开通知把已恢复来源再次设为故障目标。写库过程与实时状态仍不是跨组件原子事务；处理中发生的新变化由保留的后续工作更新。
- 每轮 Runtime 状态补写使用 2 秒 context 期限并服从更短上层期限；取消后不继续尝试其余数据库写入，未完成任务留队重试。不改变串口、命令或 OPC 超时。
- 数据库调用不持有组件状态锁，慢写入时入队和健康查询仍可进行；多个补写调用使用单写入者串行处理。沿用现有后台/关闭路径，不新增轮询线程或外部服务。

新增 9 个测试函数，覆盖分项故障与独立恢复、同值新工作确认、固定事件批次、取消与数据所有权、取消后停止后续来源尝试、慢写期间入队/查询、并发写入者与事件数量、迟到通知保留历史但不恢复旧告警，以及真实临时 SQLite 的事件表故障不阻塞连接恢复告警。SQLite 故障由隔离触发器注入，不是现场磁盘满或硬件故障验收。

| 检查 | 结果 | 证据 |
| --- | --- | --- |
| 首轮状态/告警/删除/关闭专项 race | 通过，7.442 秒 | architecture-phase9-focused.log |
| 首轮 `go test -race ./...` 与构建 | 通过，app 71.322 秒 | architecture-phase9-race.log、architecture-phase9-build.log |
| 增加轮次期限后，再次专项 race | 通过，7.362 秒 | architecture-phase9-focused-final.log |
| 最终 `go test -race ./...` | 全包通过，app 71.149 秒；未修改的其它包使用缓存 | architecture-phase9-race-final.log |
| 最终 `go build -trimpath` | 成功 | architecture-phase9-build-final.log |

专项命令：`go test -race ./internal/app -run "StatusHistory\|StatusNotification\|EventTableFailure\|StatusEvents\|Alarms\|DeleteGroup\|Close" -count=1`。

- 最终隔离产物：`/home/lenovo/code/rcd_architecture_20260922/RcdManager.architecture-phase9-final`；首轮无 final 后缀产物仅用于追溯。
- 最终二进制 SHA-256：`c73ebb8857a41d88db86d2cf9e7f545b634903e39343678734f8925149f0d83f`。
- 已验证源码增量：`artifacts/runs/2026-09-22-architecture/phase9-final-verified.tar.gz`；SHA-256：`3bb4653e8bb7122dce3c4bebb5a5f3708d3d58a579a30423ef85a65454721daa`。仅本批 7 个 Go 文件，不是完整仓库快照。
- 格式化源码和两轮日志已取回，压缩包校验一致；证据在同目录 `evidence/architecture-phase9-*.log`。本机仅源码检查/编辑/传输，`git diff --check` 通过。

管理审计队列、来源实时状态和目录索引/隔离状态仍有 Runtime 所有权；共享配置锁和 Rust 剩余职责整理尚未完成。历史重试仍在进程内，未新增恰好一次历史写入或长期存储故障下的容量保障。整机 P95、一小时/过夜、完整故障矩阵、Rust/Web 新验收及真实设备验收仍未完成。未部署、未重启现场、未提交、未推送。

## 第十批：管理审计所有权与补写协调

继续只在 46 隔离目录构建和测试；没有访问现场 OPC、串口或现场数据库。

- 新增 `managementAudit`，独占待保存记录、内部编号、不可变详情及逐记录失败状态。Runtime 不再直接维护管理审计 map 或保存错误，健康和待写数量改从组件获取。
- 接收时固定时间戳并冻结 JSON 详情，重试及 SSE 使用独立解码副本。修复调用方或保存函数修改嵌套详情后，待补写内容跟随变化的问题；解码使用 `UseNumber`，避免 UInt64 经 float64 丢精度。Runtime 添加请求元数据时也不修改调用方顶层 map。
- 首次保存和补写共用逐记录占用，同一记录不会同时被多个写入者保存；独立记录可以并发。写数据库不持有组件状态锁，慢写时健康查询和其它记录接收仍可进行。已有 SQLite 去重键与保留规则不变，不声称数据库全局恰好一次。
- 一条保存成功不能清除其它记录的失败；序列化失败的记录保留元数据和错误，但不保留不安全的原详情引用，不静默省略详情后假装保存成功，也不将其无限重复提交。
- 每轮补写只固定开始时符合条件的工作，新增失败留到下轮；Runtime 给整轮设置 2 秒期限并服从更短上层期限，取消后不启动后续写入。只重试审计保存，不重做配置操作或设备命令。
- 关闭流程在原有共享期限内等待已经接收的在途写入，再尝试失败记录。组件不封闭新接收，应用仍需先停止请求生产者；不是无期限等待或保证关闭时全部落盘。

新增 9 个测试函数，覆盖嵌套详情和大整数、独立失败、并发补写单占用、首次写入与后台补写竞争、非法详情可见性、关闭等待及补写、取消保留任务、补写中新到记录，以及真实临时 SQLite 触发器故障后的原始详情恢复与并发去重。测试只对隔离数据库注入故障，不代表磁盘满/异常断电已验收。

| 检查 | 结果 | 证据 |
| --- | --- | --- |
| `go test -race ./internal/app -run "Audit\|Close\|Operations\|Process" -count=1` | 通过，18.242 秒 | architecture-phase10-focused.log |
| `go test -race ./...` | 全包通过，app 71.823 秒；未修改的其它包使用缓存 | architecture-phase10-race.log |
| `go build -trimpath -o ../RcdManager.architecture-phase10 ./cmd/rcd-manager` | 成功 | architecture-phase10-build.log |

- 隔离产物：`/home/lenovo/code/rcd_architecture_20260922/RcdManager.architecture-phase10`。
- 二进制 SHA-256：`ab508dc4d9495f97074d0f083c63b932f639c0814da21965c7066ffa7e7310bb`。
- 已验证源码增量：`artifacts/runs/2026-09-22-architecture/phase10-verified.tar.gz`；SHA-256：`ffef51e5d62f2f50a8bc10ffa86144df52872ee43ef7e2b19dd0761841fc232f`。仅本批 7 个 Go 文件，不是完整仓库快照。
- 远端格式化源码和日志已取回，压缩包校验一致；证据在同目录 `evidence/architecture-phase10-*.log`。本机只做源码/文档检查、编辑及传输。

管理审计重试仍是进程内缓存，未新增持久队列、容量上限或永久失败记录的人工处理接口。来源实时状态、目录索引/隔离状态、共享配置锁范围和 Rust 剩余职责整理尚未完成。整机 P95、一小时/过夜、完整故障矩阵、Rust/Web 新验收及真实设备验收仍未完成。未部署、未重启现场、未提交、未推送。

## 46 现场阶段部署（2026-09-22）

用户本次明确要求部署测试，并确认 46、58 管理端和直连客户端已暂停控制下发。部署范围仅 46 下级和管理端；未升级 30/58 下级或 58 管理端，未发送真实设备命令，未修改安全、端口、选源或采集配置。未提交、未推送。

### 发布与回滚证据

- 46 隔离源码 `/home/lenovo/code/rcd_architecture_20260922` 的 728 个清单文件与本地内容一致（允许 CRLF/LF 差异）。实际构建源码摘要：`19242f9b428233114b71ece340fda190c72a31495ac00f4961bbb748994922d8`。manifest 明确记录为未提交源码快照，不冒充干净 Git 提交。
- 发布目录：`/home/lenovo/code/rcd_architecture_release_20260922`。发布工具在停止目标后备份，再原子替换，并校验 `/proc/<PID>/exe` 摘要、健康与端口。下级于 19:47:50 启动，管理端于 19:48:27 启动。
- 下级运行入口仍为 `/home/lenovo/code/rcd/rcd_server`；release 构建 SHA-256：`bad04f605cd36f7632d5b482fa4d4f55f66295ba8d29221a1552a6aa617c6eef`。
- 管理端固定入口仍为 `/home/lenovo/.local/bin/RcdManager`，本次工具原子替换该入口的符号链接为普通可执行文件；旧链接目标保留、备份记录含原链接。运行中的进程代理未重启，仍运行旧映像，新下级通过其现有进程控制 API 启动。管理端 SHA-256：`ab508dc4d9495f97074d0f083c63b932f639c0814da21965c7066ffa7e7310bb`，与第十批已通过 race 的产物相同。
- 下级备份：发布目录下 `lower/build-720bf84b618e412da851030b20ebaa49/backup-f32209ebb45d4868b883b17358ece467`，含旧二进制和 98 个配置/站点文件。新建命令审计库此前不存在；原日志仍保留，启动时正常轮转，未清理日志。TOML/CSV 摘要复核未变。
- 管理端备份：发布目录下 `manager/build-f9c29de0751c4275a81a60dc0ab5db10/backup-720a9418d9574dbab81b9d4dd5987287`，含旧二进制与一致性 SQLite 备份。数据库从 schema 13 迁移到 14；新下级命令库 schema 1。两库 `quick_check=ok`。
- 回滚管理端必须恢复该备份的 schema 13 数据库与旧二进制，不能只替换二进制。下级回滚前须另存新建命令审计库，不因回滚删除新产生的审计证据。
- 当前管理端页面版本显示 `dev/unknown`，下级构建提交亦未注入；本次发布身份以产物 SHA-256、源码摘要和 manifest 为准。发布工具的应用内版本注入仍需补齐，不能将默认显示当作正式版本号。

### 实际测试

| 项目 | 结果与范围 |
| --- | --- |
| 下级发布构建 | 发布工具执行 `cargo test --locked --lib -j2`、`cargo build --locked --release -j2` 成功；工具首版未保留成功步骤的逐条 stdout，不另编造测试数量 |
| 管理端发布构建 | 最终 npm ci/test/build、Go 全包测试及构建成功，前端 30 文件/185 测试通过；最终各步骤 stdout 单独保存 |
| 首轮前端失败 | 首轮 `npm test` 失败，原工具仅留下异常未保留具体测试输出，不能确定根因；单独复跑与最终完整构建均通过，未使用失败产物 |
| 发布版 Rust/Go OPC 互操作 | `TestCommandTrackingRealRustInterop` 含 race 通过，4.334 秒；None/匿名、安全端点匿名/用户名、能力、拒绝与去重、revision、历史恢复；仅隔离不存在且停用串口，不代表现场 ACK 成功 |
| 真实 OPC 点表 | 升级前后管理端 5,297 变量+6 Method，下级 3,300 变量+6 Method；NamespaceArray、NodeId、BrowseName、类型、值秩、访问级别、父引用及方法参数逐项一致 |
| 真实 OPC 订阅 | 管理端抽样 13 节点，下级 12 节点，分别订阅 15 秒并收到所有首值，无订阅错误；未测全量 P95 |
| 真实采集/计算 | 46 管理端 3/3 来源连接、无目录冲突、无待处理命令；29 个正常站点时间戳持续前进；1,997 条计算规则 revision 6/hash 保持不变 |
| 旧管理端兼容 | 58 未升级，3/3 来源继续在线、29 个站点时间戳前进、点数不变，健康检查正常；未测试其真实控制命令 |
| 新追踪能力 | 46 管理端读到 real-lower 的 `command_tracking` feature，新独立审计库创建正常；没有现场命令，故未验收实际发送次数/真实 ACK 全链路 |
| Web 真实页面 | 46 上已有 Playwright：登录→历史命令详情→结果筛选→计算中心→下级日志→手机详情；1440×1000、390×844，无 JS 错误、控制台警告或 HTTP 4xx/5xx；未执行配置修改/控制动作 |
| 短时运行 | 19:54:28 复核下级 PID 476700、管理端 PID 477167 未变，管理端 NRestarts=0，启动后无 WARN/ERROR，下级无 panic 日志；仅约 6 分钟，不是一小时稳定性/过夜验收 |

部署前已有 16 个通信异常站点，部署后名单完全相同：WCH2/61、WCH3/62–69、WCH5/61–67；日志继续报告这些站点超时，没有新增异常站点。不能仅据此断定设备或串口硬件根因。

验收脚本最初有 NodeID 订阅重复计数、匿名 token 未按 endpoint policy 初始化、Web 标签/导航等待定位问题，修正脚本后通过，未据此改业务协议。浏览器截图需等待抽屉动画完成；手机抽屉在可视范围内，但顶部标题/账号区较拥挤，未在本次部署中扩大 UI 改动。

证据包：`artifacts/runs/2026-09-22-architecture/deployment-evidence.tar.gz`，SHA-256 `b015c29f3147fd82328fe43d3e37d49d639e84f1b96ab917a28046cf63fe772e`。含发布日志、点表黄金对比、短时日志、验收摘要与源码清单，不含运行数据库或配置正文。Web 图片/脚本在远端 `/home/lenovo/rcd-deploy-qa-20260922`、`/home/lenovo/rcd-deploy-web-20260922.cjs`，不纳入源码。原始日志持续正常记录；未另建无人值守监控或宣称完成长测。

## 30/58 阶段升级（2026-09-28）

用户明确授权升级 30、58，确认暂停控制下发；网络恢复后逐个检查并升级。30 没有部署新的管理端，仅升级已有下级；58 升级已有下级和管理端。46 只作为检查客户端/产物来源，程序不替换或重启。全部 OPC/API 验收从 46 发起，本机仅编辑、传输、摘要和文档检查；没有发送设备控制报文，也没有改串口/站点开关、配置、默认源、安全或端口。

- 预检证实 30/58 已有系统级 `rcd-lower.service`，enabled/active、on-failure 守护；58 用户级 `rcd-manager.service` 亦 enabled。无需重复安装或再启动第二份采集实例。原进程守护 agent 及其服务文件保留，不将未重启的 agent 宣称已更新。
- 复用 46 的确切发布产物和原始源码清单，不在 30/58 重新构建：下级 `bad04f605cd36f7632d5b482fa4d4f55f66295ba8d29221a1552a6aa617c6eef`；管理端 `ab508dc4d9495f97074d0f083c63b932f639c0814da21965c7066ffa7e7310bb`。本机、目标及运行中 `/proc/<PID>/exe` 摘要一致；每个目标 manifest 记录 build_host=46、imported_without_rebuild，保留原 manifest。应用内版本仍为此前说明的 dev/unknown。
- 30 下级 11:02:12 启动，PID 1112183；58 下级 11:02:46 启动，PID 2897395；58 管理端 11:03:34 启动，PID 2897961。检查时 PID 保持、服务自动重启计数为 0，下级当前日志无 panic；这仅是短时检查，并非重启整机或长测。
- 两下级各备份旧二进制和 143 个配置/站点文件，TOML/CSV 摘要未变；新建命令审计库 schema 1。58 管理端备份旧二进制、manager.env 和一致性数据库，数据库 schema 13→14。三库 quick_check 均为 ok。

| 目标 | 回滚备份目录 |
| --- | --- |
| 30 下级 | `/home/RCD93/code/rcd_release_20260928/lower/imported-build/backup-360b2828b1124978b64dd6f7ac19dee7` |
| 58 下级 | `/home/lenovo/code/rcd_release_20260928/lower/imported-build/backup-791b152eed814a25865848259e23aff2` |
| 58 管理端 | `/home/lenovo/code/rcd_release_20260928/manager/imported-build/backup-654f13a5b411417bb528fa25eff08c9e` |

回滚管理端须成套恢复 schema 13 数据库与旧二进制；下级回滚前另外保全此次新增命令审计库，不能删除新的命令证据。未执行回滚。

### 检查结果与限制

- 部署前后 NamespaceArray、业务 NodeId/BrowseName、类型、值秩、访问级别、引用与方法参数逐项一致。30、58 下级分别 3,300 个变量+6 Method；58 管理端 5,297 个变量+6 Method。
- 各下级抽样 12 节点、58 管理端 13 节点，分别 15 秒订阅收到全部首值，无订阅错误。未做全量负载或通知 P95 测试。
- 46/58 两管理端 health 正常，均 3/3 来源在线、0 目录冲突、0 待处理命令；两管理端均可发现全部下级的 command_tracking 能力。分组、默认源及计算活动 revision/hash/规则数未变，站点开关未变。
- 58 的 29 个正常通信站点时间戳持续前进；其原有 16 个异常站点名单未变。46 仍有 29 正常/16 异常。
- **30 在升级前后都是 45 个站点全部通信异常，串口 open=true。OPC 连接、进程和订阅正常不代表物理采集成功。未推断设备/接线/角色或串口卡根因，30 的有效数据采集未验收通过。**
- 58 Web 首页、静态 JS/CSS、登录及来源/计算/运维状态 API 可读。本次未重新做浏览器视觉回归；同一产物在 46 的前述浏览器验证不冒充 58 本次浏览器实测。
- 没有真实命令 ACK 验收、整机重启、一小时/过夜或完整性能测试；日志保留并继续正常记录。没有 Git 提交/推送，未改业务源码。

本地证据：`artifacts/runs/2026-09-28-upgrade/`，含各目标发布日志、verification.json、原 manifest 和产物摘要；运行配置及数据库正文仍留在目标备份中。46 的黄金点表/API 快照与对比结果在 `/home/lenovo/code/rcd_upgrade_20260928_checks/`。取回 `evidence.tar.gz`，SHA-256 `fcc3bab770347595f2e2f62de28cf12a0501175c8d6c612f058dcba16f709e74`，本机核对一致。
