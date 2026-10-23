# P2 第二批：Agent 维护作业与下级握手

日期：2026-09-28。开发线：`codex/opc-device-groups`。本批完成源码及 46 隔离验证，**未现场部署**；完整发布恢复和 Web 编排尚未完成。

## 实现

- agent 在 process-state.json 登记维护作业、目标版本、发起者、物理绑定、令牌、阶段及下级证据。先持久登记作业和 token，再调用下级维护接口。
- 本地参数固定回环 OPC 端点和 gateway_instance_id，不接受 HTTP 提供任意端点、PID 或执行参数。按 Namespace URI 解析运维节点，匿名身份使用下级发布的真实 policy ID，禁用自动重连/重发。
- 增加 maintenance 查询及 enter/reconcile/complete，使用实例检查、If-Match/revision。reconcile 只读补查，不重发设备或进程操作。
- 维护期间普通进程动作返回 409；持有同一作业和 token 才能操作。stop/restart 前重新核对下级持久化、所有者和排空，不使用缓存 ready 代替实时确认。
- agent 重启保留 unknown，暂停自动启动/重试，不自动重放 restart。对已确认维护的下级可显式 start，启动后重新对账。发现下级证据矛盾时撤销缓存启动许可。
- 关闭 agent 时取消并等待维护握手结束，之后才释放实例锁，防止迟到结果覆盖新 agent 的文件。
- 首次登记维护作业将 agent **状态文件**升为 schema v2，旧 agent 会拒绝读取。公共 HTTP/Operations 仍为 v1；从未建立维护作业的旧状态保持 v1。活动维护中不能通过去掉参数或改变固定启动路径绕开绑定。
- 现有采集、OPC 点表、命令报文、ACK 和重试规则不变；未修改任何现场配置或服务参数。

完整协议、例子及边界见 [维护接口契约](../../../contracts/maintenance/README.md)。新参数默认不启用，旧运行方式继续兼容；发布器和 Web 尚未接入，因此现在不向现场添加新参数。

## 环境与验证

全部构建、格式化和测试在 `192.168.10.46`：

`/home/lenovo/code/rcd_agent_maintenance_20260928_kHC6PH`

本地只编辑、检查差异、传输和取回格式化源码。未调用现场 agent 进程动作，未打开真实串口，未下发硬件报文。

| 检查 | 结果 |
| --- | --- |
| `go test -race ./... -count=1 -json`（同时开启隔离维护互操作） | 12 个有测试的包全部通过；包含 API/app/OPC/store 等回归，无 race 报告 |
| 最后补强后的 `go test -race ./internal/processagent ./internal/domain -count=1 -v` | 通过，包含 13 项维护相关测试及两个包原有测试 |
| `cargo build --bin rcd_server` | 当前下级源码构建成功，仅用于隔离联调 |
| `go test -race ./internal/processagent -run TestMaintenanceRealRustInterop -count=1 -v` | 实际 Rust 程序与 Go agent 的 OPC/进程握手通过 |
| `gofmt`、`git diff --check` | 涉及文件通过，格式化源码已取回 |

全量回归后的小改动为：维护进程请求强制 If-Match、状态阶段日志、连接失败资源释放、证据矛盾撤销缓存许可及相应测试。最终对实际改动的 processagent/domain 包重跑 race 和真实 Rust 互操作；没有把修改前日志冒充最终测试。

新增覆盖：固定回环配置、先持久化后远程修改、未持令牌/过期令牌/竞争作业拒绝、enter/leave 回复丢失、只读恢复、待执行事务未结束拒绝停机、维护重启保留/抑制自动重试、agent 存储失败、错误身份与矛盾证据、HTTP CAS/未知字段、关闭时等待写入者、活动绑定不得被移除或重定向。

## 实际 Rust 联调方式及发现

`TestMaintenanceRealRustInterop` 只在显式设置 `RCD_MAINTENANCE_INTEROP_BINARY` 时运行。复制指定 Rust 程序到 TempDir，建立临时配置、状态和 PKI，使用动态端口，**不配置任何串口**。由真正的 agent Supervisor 创建/停止自己拥有的 Rust child，通过真正的 OPC Operations 验证进入、重启后持久化、旧 boot 拒绝、agent 重启不重放、显式恢复和释放。

这不是串口模拟器，但也不是现场硬件验收。故障注入单元测试使用内存对端，分别证明确定的并发/存储/回复丢失分支。

首轮互操作发现未设置正确匿名 policy ID，Rust 返回 BadIdentityTokenInvalid；已改为发现并选取现有 None 端点的真实身份策略，没有修改安全配置。临时 PKI 首次生成偶尔超过小型测试 helper 的 8 秒等待，已为真实 Rust 测试单独使用有界启动等待；未修改现场或串口超时。

## 证据与未完成项

- 隔离目录：`go-race-final.jsonl`、`go-race-final.stderr`、`agent-final.log`、`maintenance-interop.log`、`interop-build.log`、`rust-binary.sha256`。
- 本地归档：`artifacts/runs/2026-09-28-agent-maintenance/agent-formatted-evidence.tar.gz`，包含测试日志、对应 Go 文件和联调用 Rust 文件。
- 未提交、未推送、未部署、未重启现场。没有新增现场设备状态/配置一致性的验收要求。
- 未完成：发布器逐阶段持久日志与中断恢复、完整 rollback、产物哈希/目标版本验收与维护收尾的编排、Web 维护 UI/动作串联、三语言黄金样例。
- 未做：现场双管理端竞争、物理掉电/磁盘满、硬件控制、性能 P95、一小时/过夜长测。关闭后保留 unknown 不等于所有未知状态均可自动修复；在首次栅栏确认之前崩溃等情形仍需发布恢复方案，不允许凭猜测启动/解除维护。
- 旧 agent 不能直接读取 v2 状态；不得简单换旧二进制或恢复旧作业代次。下一批发布工具必须识别这一回滚边界。

下一批接入发布器的维护阶段、产物/备份校验和可对账恢复，再接 Web。完整联调与部署窗口确认前，现场继续使用现有人工命令暂停流程。
