# 下级维护准入协议 v1（开发中）

Rust 下级准入/排空、agent 维护作业握手、下级发布主流程、已停止作业的显式继续及已启动作业的收尾已接入开发版并完成隔离联调，尚未现场部署。不自动续跑；准入/原请求仍不明、agent 自身升级、协调回滚及 Web 编排尚未全部接入，不能宣称已完成整个安全发布计划。现场仍使用原人工维护窗口。agent 接口见下文。

沿用 OPC Operations schema v1 和既有端点、安全策略，追加 feature `maintenance_gate_v1`；未声明该 feature 的下级不支持软件维护栅栏。没有新 HTTP 端口，不更改 ns=2 命令节点和 Method 参数。

## 动作

`maintenance_status` 无副作用，`status` 同时增加 `maintenance` 对象。进入和退出示例：

```json
{
  "schema_version": 1,
  "action": "enter_maintenance",
  "deployment_id": "release-example-001",
  "fencing_token": "1",
  "expected_gateway_instance_id": "从当前 command_tracking 获取的稳定身份",
  "expected_boot_id": "从当前 command_tracking 获取的启动身份"
}
```

退出仅将 action 改为 `leave_maintenance`，必须使用相同 deployment_id/token 和**当前** boot_id。重启后的旧 boot_id 必须拒绝，不能盲目重发退出请求。示例不是现场执行授权。

- deployment_id：1–128 个 ASCII 字母、数字、`-_.`。
- fencing_token：大于 0 的规范十进制 **String**，不接受前导零/正号；首次为 `"1"`，以后必须为上一次加一。内部保存 u64，不能在 JavaScript 中先转 Number。
- 活动期间，相同所有者重复进入幂等；其他作业不能抢占。退出后同一请求重复退出幂等，但旧进入请求不能重新打开已经结束的作业。
- 新一代维护启动后，旧 token 不能退出它。无自动超时释放、无 TTL；断网不会打开准入。
- 请求成功包裹仍为 `{"ok":true,"result":...}`，错误仍为 `{"ok":false,"error":{"code":...,"message":...}}`。

状态字段：

| 字段 | 含义 |
| --- | --- |
| gateway_instance_id / deployment_id / fencing_token | 下级稳定身份、维护作业及代次 |
| active | 当前持有维护作业；不是“已停止采集” |
| admission_blocked | 新控制/配置修改被阻止，包括状态不明时的保守阻止 |
| durable | 当前状态已确认持久化；失败时不得继续停机替换 |
| pending_admissions | 尚未释放的准入凭证，含命令、修改请求、自动版本事务；不是发送次数 |
| pending_port_operations | 仍有控制/命令的物理端口数量；已返回 Pending 的异步操作仍计入 |
| drained | active、durable 均成立，且两项 pending 均为 0 |
| error | 维护存储/身份错误；空字符串表示无错误 |

`drained=true` 只表示下级接受的副作用事务已结束，不表示普通采集停止；发布协调者仍须确认 agent 作业、绑定、token 等条件。状态读与事务释放使用固定锁序，防止把“RPC 结束、串口尚未处理”误判为排空。

## 入口与兼容性

- 新 `_Send` 和 Method 请求、`submit_command` 最终共用命令准入。维护期间忙碌映射为 `BadWaitingForResponse`；旧 Method 其他结果字符串保持原行为。
- `submit_command` 对已存在编号仍可只读查重；新请求被拒绝后不会入队。维护前已接受的命令按原有 ACK/重试/超时规则结束。
- 所有当前 Operations 写动作被阻止：采集开关、类型修改、手动版本查询、计算模式、日志等级、配置写入。
- 自动版本查询持有事务凭证；维护期间保留待查询队列，结束后按原“一轮最多一个”调度恢复。不会改变常规轮询、失败计数、ACK 或串口报文。
- Read/Subscribe、Operations 状态、文件读取、配置校验、普通采集不被维护主动停止。
- 未启用的可选 TCP 控制前端不在本批兼容保证内，不得启用它绕过准入；TCP 序列化格式未修改。

## 状态文件和故障

下级 state 目录新增 `maintenance.sqlite`（WAL、FULL 同步）。它是维护控制状态，不是命令审计库，不可作为缓存删除。必须与设备实例身份一起迁移、纳入一致性备份；回退旧维护代次不可作为普通软件回滚步骤。

进入先在内存原子阻止新准入，磁盘确认后才返回成功；退出磁盘确认后才恢复准入。磁盘 I/O 不持有 Operations 配置锁，也不由串口 worker 执行。存储失败返回 `persistence_failed`，不伪装成功、不自动解除维护。当前进程已知所有者可重复进入，恢复其未确认的落盘；启动时状态缺行、损坏、身份不符则保持控制阻止，保留只读/采集能力，须修复存储并显式重启核对。

独立命令审计的“存储失败仍执行控制并告警”策略不变；这里阻止的是**维护状态无法确认**，两类故障不能混为一谈。

## Agent 维护作业接口（开发版，未部署）

agent 增加两个**本地启动参数**，不允许 HTTP 请求修改：

```text
--maintenance-endpoint=opc.tcp://127.0.0.1:4841
--gateway-instance-id=<核对过的下级 command_tracking.gateway_instance_id>
```

两者必须同时指定。端点只接受明确回环 IP/端口，以现有 None/Anonymous 端点握手，读取服务端真实匿名策略 ID，不修改服务端策略、不尝试其它安全模式降级。按 Namespace URI 找 Operations，不固定下级 namespace index；每次连接禁用自动重连，修改请求绑定当前 boot_id。

首次启用前须核对是该 agent 的固定下级；维护中的 endpoint、gateway、程序路径、工作目录、启动参数不能悄悄修改。可以在同一固定程序路径更新二进制，但产物哈希和软件版本验收由后续发布工具负责。IP、source_id、显示名不作为 gateway 身份。

保留 `X-RCD-Instance-ID` 目标检查及原同源限制，不把它当认证。`initiator` 是发起者声明的审计标签，不是认证用户名。

### 查询与动作

- `GET /api/v1/maintenance` 返回 supported、revision、固定 gateway_instance_id/endpoint、最近作业 job（无作业为 null）；ETag 等于 revision。发布器在首次 enter 前核对这两个固定字段，避免先封锁错误下级再发现绑定不符。它只读 agent 当前记录，不自动请求下级刷新。
- `POST /api/v1/maintenance/actions` 接受 enter/reconcile/complete，必须携带匹配的 `If-Match` 及 body.expected_revision。未知字段拒绝，不接受路径、PID、任意端点或执行参数。
- `GET /api/v1/process` 追加 maintenance_deployment_id/maintenance_phase；不改变原进程状态语义。

```json
{
  "action": "enter",
  "instance_id": "example-lower",
  "deployment_id": "release-example-001",
  "expected_revision": "从GET取得的当前revision",
  "target_version": "1.0.1",
  "initiator": "operator"
}
```

agent 先持久保存作业，再读取下级状态、分配下一个 token 并保存，然后请求下级进入。返回 draining 表示仍有事务未结束，不停止它们；重新 GET 获取 CAS 版本后提交同作业 reconcile，**只读取**下级状态并更新 agent 记录，不重发设备/进程动作。reply 丢失或网络失败保留 unknown；不得因超时自动开放准入。

`job` 保存实例/绑定、deployment_id、目标版本、发起者、fencing_token（String）、阶段、下级证据、创建/更新时间及错误。agent 状态文件保存最近一份作业，阶段变更另记 agent.log；它不是后续发布器完整的逐版本归档。

### 维护期间启停

已有 `POST /api/v1/process/actions` 增加可选 deployment_id、fencing_token；维护请求必须携带 If-Match。仍使用原 action、request_id、instance_id、expected_revision，不允许请求改变可执行文件或 PID。

```json
{
  "request_id": "release-example-001.stop",
  "instance_id": "example-lower",
  "action": "stop",
  "expected_revision": "当前进程revision",
  "deployment_id": "release-example-001",
  "fencing_token": "1"
}
```

- 一旦启用维护绑定，普通 stop/restart 返回 409，要求先登记维护；无维护作业时普通 start 和原首次启动行为仍可用。
- 维护期间所有普通进程动作被阻止；只有匹配作业/token 的动作可执行。stop/restart 执行前重新读取下级，确认所有者、代次、durable 和 drained，不能只用缓存 ready。
- 已停止下级可凭**此前持久确认**的栅栏显式 start，启动后必须重新读取下级核对；不能因为 OPC 已停止不可连接，就清除维护或声称验收通过。
- agent 重启后活动作业变为 unknown，保留 token 和已确认事实，暂停自动启动/自动重试；不重放未确认 restart。操作者核对原作业后才显式启动/恢复。
- 收尾 complete 会确认下级仍持有相同作业且已排空，先保存 releasing，再请求下级退出，并重新读取确认。退出回复丢失后用 reconcile 补查；未经确认不自动重发退出。
- 显式 reconcile 即使遇到缓存 completed 也重新读取 OPC，不把历史解除成功当作当前下级证据；读失败会保留 unknown。GET maintenance 仍只读缓存，不触发 OPC 或修改状态。
- complete 只结束维护准入，并不验证目标版本、配置迁移、点表或 SLA。发布器必须完成这些验收后才能调用它。
- 旧 token/已结束作业不能操控新维护；CAS 过期、忙碌、目标不匹配均拒绝。未支持的旧下级、下级无法确认或 agent 存储故障不会自动降级到普通 stop/restart。

agent 关闭会取消并等待未结束握手后才释放实例锁，防止旧回复覆盖新 agent 状态。root/systemd 直接停 agent 及父进程死亡仍可导致 child 被终止，这不是操作系统级防绕过机制。

### Agent 状态版本和发布限制

从未建立维护作业的 process-state.json 保持 schema v1。首次持久登记维护时升至 **schema v2**，公共 HTTP/Operations 协议仍为 v1；新版读取 v1/v2，旧 agent 会拒绝 v2，避免忽略维护记录后自动启动。不可删除维护字段、改 schema、恢复旧 token 或直接换旧 agent 进行普通回滚。数据库/状态回退必须单独制定保全当前数据的恢复方案。

本批不自动修改现场服务参数，不启用新绑定。[发布工具](../../tools/release/README.md)已有显式 `--maintenance` 下级发布主流程；Web 只展示维护状态并禁用不适用的普通动作，不编排发布。旧 Web 的 stop/restart 在新参数启用后会得到 409，这是保护性拒绝，不能靠去掉校验绕开。

### Web 只读维护视图（P2 第七批）

后续进程快照还可包含 `agent_identity`：`build` 为 schema v1 的 agent 构建信息（与 agent `/api/v1/version` 同一来源），另含 `boot_id`、`started_at`、`reported_at`。身份来自运行中的守护实例，不是其 child。启动/报告时间为 RFC3339，启动标识稳定于本次守护运行；字段缺失表示旧代理未报告，不影响旧协议控制。构建元数据在初始化时读取，快照独立复制；只读查询不改变控制 revision。不以自报版本替代发布映像摘要验收。

`GET /api/v1/process` 和管理端来源 process/status 继续使用 schema v1，追加可选 `maintenance_enabled` Boolean。缺失表示旧代理未报告，不能等同 false。新版 agent 每次明确返回开关；若有未结束的持久作业，即使绑定配置被移除也报告 true。

活动作业同时返回 `maintenance_deployment_id`、`maintenance_phase`，均为 1–128 字符的稳定标识（字母、数字、冒号、下划线、连字符、点）。两字段必须同时有效；false 与活动字段矛盾时拒绝快照。未知将来阶段仍视为活动维护，不因枚举扩展解锁。完成作业不再附加活动字段；这只是 agent 当前缓存，不是独立 OPC 验收证明。

Web 在下级运维 → 进程展示发布编号、阶段和禁用原因；刷新失败保留旧维护信息并禁止普通进程控制。活动维护阻止普通启停及绑定保存，同一抽屉采集/配置写入也锁定，读取与草稿保留。无活动维护但开关为 true 时，只允许原有普通 start，stop/restart 使用发布工具。前端只是使用提示，服务端维护令牌和准入检查仍是最终约束。

尚未完成：完整中断恢复与新版已运行后的回退、agent 自身发布、管理 Web 发布编排、三语言黄金样例、真实断电/磁盘满、双管理端现场竞争及硬件/性能长测。现有工具支持特定确认状态下的显式 resume/finish 和新版启动前的只回退程序，不是任意恢复。隔离联调不等于整个 P2 或现场设备验收。
