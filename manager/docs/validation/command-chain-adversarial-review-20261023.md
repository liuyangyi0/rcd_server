# 命令队列、发送间隔及全链路对抗复查（2026-10-23）

最新后续：此前未发布的两个 Go 管理端修复已随 `0.6.16-config.20261023.1` 通过平台部署三台，见[配置页发布记录](configuration-workflow-deployment-20261023.md)。下方较早的未发布描述仅对应当时阶段。

后续部署：本记录中 Rust 的轮询槽及接收/准入期限修复已随 `0.6.15-csv.20261023.1` 发布三台采集程序；Go 管理端两项修复未随本次上线。见[采集发布记录](csv-legacy-deployment-20261023.md)，勿将下文隔离阶段的“未部署”或本次 Rust 发布外推为四项均已上线。

## 结果与边界

针对最近一批（2026-10-22）修改，由三路 agent 分别审查 Rust 串口执行、Go 命令队列、OPC 关联与反馈链路；主审交叉核对维护排空、结果通知、网页配置入口并集成回归。确认并修复 **4 处问题**，新增 **8 个测试入口**及其子场景，46 隔离验证通过。

**本轮未部署。** 未重启或修改 30、58、46 的现场程序、配置、串口开关、数据库、PKI 或安全策略，未发送现场硬件控制命令。上一轮维护授权已结束；本轮仅审查、修复和隔离测试，未提交或推送。

工作树为 `C:/Users/here/RustroverProjects/rcd_server_device_groups`、`codex/opc-device-groups`，基点 `769ef2a1f82e6e7bb5e77f9e79a3e88434005e42`。保留全部原有未提交内容，不编辑旧 `rcd_server` 工作树。上次已发布状态见[三台部署记录](command-chain-deployment-20261022.md)，不能把它当作本轮修复已上线。

## 已修复问题

| 问题 | 触发及影响 | 修复 |
| --- | --- | --- |
| 控制命令抢占后仍推进轮询下标 | 两站串口中控制与轮询交替，命令占用站点 1 的轮询槽后直接推进到站点 2，可能反复跳过站点 1。只测控制事务本身发现不了完整循环的错误。 | `src/serial/worker.rs` 仅在正常轮询槽推进下标；`CommandHandled` 不消耗该槽。正常 Query/Idle 推进、轮询休眠、发送间隔和 ACK 不变。 |
| 下级接收/准入等待后重新开始相对期限 | 管理端只给 5ms 剩余预算，持久化接收记录或等待 pending/维护互斥已耗尽它，下级仍可能重新创建期限并接受命令。 | `src/server/opcua/tracked_commands.rs` 在回调入口固化本地绝对截止点；`src/server/opcua.rs` 贯穿接收和准入等待，到期返回 `BadTimeout`、记录 `Expired`，不入队、不发布 Waiting、不遗留占用。同编号重试仍返回原快照，不能增加预算复活。 |
| 同步 Call 未在接受时固定物理串口 | 命令接受后进行能力发现/审计 I/O，期间目录热更新；稍后再解析实际串口，可能把这条已接受命令发到新串口。 | `manager/internal/app/opc_backend.go` 与 Write 一样，在接受事务中固化 `PhysicalPort`；派发和审计继续使用原目标，不随随后目录变化。 |
| `_Send` 被动复位绕过维护及物理占用 | 空值、IDLE、0/1/2/3 不是硬件命令，但原路径可在维护时写下级；检查空队列后到远端 Write 之间也可被新控制请求越过。 | 复位先检查管理端/来源维护，再通过现有 RouteTable 预占组和端口；同端口硬件请求、复位及切源明确忙拒绝，RPC 在锁外执行，结束后统一释放。复位不进入硬件 FIFO、不生成硬件发送审计；硬件超时扫描不提前释放仍在执行的复位。错误不会返回 Good。 |

最后一项复用原路由占用，不新增另一套 pending 状态。复位是受原调用超时限制的短操作，不是可积压重启/发布作业。不同串口不因它而全局阻塞。

## 新回归

Rust：

- `command_chain_worker_control_preserves_the_next_acquisition_slot`：直接运行真实 worker loop，覆盖有 ACK/无 ACK 的控制抢占、轮询下标不跳过、冷却中继续正常采集。
- `command_chain_worker_expired_control_does_not_skip_a_station`：过期 FIFO 头不写控制帧、不启动冷却，仍轮询原站点，只返回一次失败。
- `tracked_receipt_deadline_includes_journal_and_pending_lock_waits`：SQLite writer 锁与 pending mutex 屏障分别耗尽 5ms 预算；无入队、无 Waiting、发送/完整写出为 0，permit/pending 无残留；增大重复请求预算不能复活。

Go：

- `TestTrackedCallPinsPhysicalPortBeforeCapabilityAndAuditIO`：能力发现回调中改变目录，派发和审计仍指向接受时的原物理串口。
- `TestPassiveResetRejectsReleaseAndSourceMaintenance`：管理端维护、来源维护分别拒绝复位，远端没有收到 Write。
- `TestPassiveResetInFlightFencesFastCommandsAndSelection`：屏障暂停远端复位，验证快速 Write、Call、重复复位和切源不能越过；排空计数可见、超时扫描不误释放、无硬件审计，成功结束后占用释放。
- `TestPassiveResetReleasesReservationOnFailureAndCancellation`：拒绝、传输错误、上下文取消均不报 Good，并释放维护/端口/选源占用。
- `TestPassiveResetReservesPortWithoutJoiningHardwareQueueOrExpiry`：复位不成为 FIFO 头，超时扫描不替其完成 RPC，另一物理串口仍可开始，显式结束后释放。

已有快速连续写、64 条容量、FIFO 载荷固定、组选源锁、断线/超时取消队尾、迟到证据只补审计、首/二/三次 ACK 立即停止、Session 隔离与通知测试随相关范围一起回归。

## 46 测试结果

隔离目录：`/home/lenovo/code/rcd_command_adversarial_20261023_Bmuwha/`，`source/` 从上次已验证发布源码快照复制，再仅更新本轮文件。所有构建、格式化和可执行验证均在 46，本机只编辑/检查源码、传输及核对摘要。

| 检查 | 结果及证据 |
| --- | --- |
| 修改前既有回归基线 | `cargo test --offline --lib`：404 通过、1 既有忽略，`rust-baseline.log`；Go domain/app/opc race 通过，`go-baseline-race.log`。旧测试通过不代表不存在本轮新发现的问题。 |
| 新 Rust lib | 407 通过、1 既有忽略，`rust-reviewed-lib.log`。 |
| Rust 完整回归 | `cargo test --offline`：总计 **433 通过、4 既有忽略**，含库、二进制及集成测试，`rust-all.log`、`.exit=0`。 |
| 新 Go 回归 | `go test -race ./internal/domain ./internal/app -run 'TestPassiveReset\|TestTrackedCallPinsPhysicalPort' -count=1 -v` 通过，`go-new-regressions.log`。命令中的模式实际为正则或运算，不是 shell 管道。 |
| Go 完整相关 race + 真实 OPC 互操作 | 显式 `RCD_TRACKING_INTEROP_BINARY=<本轮 target/debug/rcd_server>`，执行 `go test -race ./internal/domain ./internal/app ./internal/opc -count=1`，三个包通过（约 1.0/131.8/87.8 秒），`go-final-race-interop.log`、`.exit=0`。没有将未设置二进制时的 Skip 算为实际互操作。 |
| 重复并发回归 | 新 Go 回归 `-count=5` 通过，`go-new-regressions-repeat.log`；Rust 接收期限测试连续 5 轮通过，`rust-deadline-repeat.log`。对应退出码均 0。 |
| 负向对照 | 独立 `before-manager/` 只换回上次部署的 `opc_backend.go`，保留新测试；Call 固定目标及管理端/来源维护拒绝复位均按预期失败（退出码 1），`go-before-regressions.log`。对照没有覆盖被测修复目录，也不是现场失败任务。 |
| 源码一致性 | 46 局部 rustfmt/gofmt；主审仅应用格式差异，10 份本地源码与46实际测试快照 SHA-256 逐字一致，`reviewed-sources.sha256`。涉及的 `git diff --check` 通过。 |

真实 Rust/Go OPC 测试使用临时数据目录、随机回环端口、停用且不存在的串口；不能访问现场串口。这证明协议、拒绝/恢复及配置链路，不证明设备动作成功。串口行为使用确定性 I/O 测试桩，不以模拟结果代替真实硬件验收。

格式化回传曾被防覆盖检查拦截，未覆盖工作树。随后先保留本地文件备份、在临时目录对比，再用局部补丁应用格式变化；其它文件逐字不变。

## 保留边界

- 当前有效 `00 50 50` 的判断、重试次数、串口保护及轮询间隔不变。ACK 只证明协议确认，不证明设备动作反馈或物理恰好一次。
- `command_interval_ms` 仍默认 0，网页保存后显式重启才生效；本轮没有代使用人填写现场值，过长等待不能延长期限。
- 相对 wire timeout 无法精确扣除请求到达下级之前的网络时间；本轮消除的是下级本地接收/准入耗时续期，不宣称跨主机严格同一绝对期限。
- 结果不明不回退旧 `_Send` 再发送，不自动补投；`_Send` 仍只表示本 Session 最近命令状态，逐条结果以独立审计核对。
- 旧 TCP 可选前端当前未启用，不在本轮启用或重做；未来启用前须另审其控制准入，不能由 OPC 路径通过就声称该路径也通过。
- 本轮不含新前端改动、现场硬件连续下发、性能压测、Windows、一小时/过夜长测或正式发布包。没有更改 OPC NodeId、Method 签名、端口、路由/计算语义。

上线仍须另获当次授权，通过运维平台发布涉及的管理端和采集程序。本记录不能作为已经部署或所有潜在问题均已消除的保证。
