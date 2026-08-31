# 架构与 OPC 行为

## 进程结构

```text
下级 RCD OPC UA Server（最多 20 个）
              │
              │ gopcua v0.9.1 本地补丁：发现、订阅、读、写、Call
              ▼
┌────────────────────── RcdManager.exe ──────────────────────┐
│ 每来源连接监督器 → 目录合并 → 分片热缓存                 │
│                         │                                  │
│ SQLite WAL              ├→ Session 路由表/命令锁          │
│ 配置、目录、审计、告警  │                                  │
│                         ├→ awcullen/opcua v1.4.0 本地 fork │
│                         │       上级 OPC UA Server          │
│                         └→ REST + SSE + 内嵌 React Web      │
└────────────────────────────────────────────────────────────┘
              │                             │
              ▼                             ▼
       上级 OPC 客户端（最多 10 Session）   浏览器/运维客户端
```

核心包的职责：

- `internal/domain`：不依赖 OPC 库的点键、样本、目录、Session 路由和命令状态；
- `internal/opc`：下级 `gopcua` 适配器和上级服务端适配器；
- `third_party/awcullen-opcua`：固定 v1.4.0 的服务端 fork；
- `third_party/gopcua-opcua`：固定 v0.9.1 的客户端依赖闭包及原 Session 订阅恢复补丁；
- `internal/app`：配置提交、目录合并、选源、热缓存、命令代理和 HTTP/OPC 协调；
- `internal/store`：SQLite WAL、迁移、目录缓存、命令/管理审计、来源状态历史和持久告警；
- `web`：React + TypeScript + Vite 管理界面及嵌入资源。

## 稳定点标识与目录

内部不保存易变化的 namespace index，而使用：

```text
NodeKey { NamespaceURI, Identifier }
```

每次下级连接后，监督器读取 NamespaceArray，以 URI 重新解析 namespace index，再浏览：

- `Objects/Devices` 下的变量；
- `Objects/Methods` 下的方法。

当前支持的变量类型为 `Boolean`、`UInt32`、`Float`、`Double` 和 `String`。`_Send` 必须是可写标量 String；`_SendCommand` 必须严格为 `HexData String -> Result String` 的标量方法。目录属性按最多 100 个节点一批读取；每个来源只创建一个 Subscription，再按最多 500 个一批加入 MonitoredItem，避免真实大点表同时触发消息尺寸和下级 Subscription 数量限制。默认 sampling/publishing interval 为 250 ms。

同组目录采用并集。同名节点只有 class、data type、value rank，以及方法签名一致时才能合并。以下情况会使涉及的来源进入 `CatalogConflict` 并不可选：

- 同一来源重复 identifier；
- 同组同名节点形状不一致；
- `_Send` 类型、秩或访问级别非法；
- `_SendCommand` 参数签名非法；
- 同一个 identifier 出现在不同组。

首次发现完成前来源处于 `Discovering`，不可作为默认源或 Session 选源。数据库中已有有效目录时，进程离线启动会将来源标记为 `Cached`，允许选择。

热更新加入的新点立即加入地址空间。当前进程内消失的点保留并标记为 retired，读取返回 `BadOutOfService`；重启加载目录时会丢弃 retired 点。

配置或目录版本提交后，如果向上级 OPC 地址空间应用最新快照时发生临时错误，Runtime 会保留 dirty 状态、令 `/healthz` 的 `upstream_catalog` 检查失败，并由后台在配置串行门内按最新完整快照重试。成功收敛后自动清除 dirty 健康错误；确定性的点表形状错误仍按 `CatalogConflict` 隔离来源，不进入无意义重试。

## Session 级选源

每个 OPC UA Session 都有独立的不可变路由快照语义：每个分组记录一个 `source_id` 和递增的 selection revision。

1. 新 Session 激活时复制各组当时的默认源；
2. 修改默认源只影响之后创建的新 Session，不改已有 Session；
3. OPC Session 可以写 `SelectedSourceId`，Web 也可以按 Session 修改；
4. 切源成功后递增该 Session/组的 revision，并分批强制重发该 Session 在该组的既有监控项；即使新旧来源的值、质量和时间戳相同，也会产生一次目标 Session 专属通知；
5. SecureChannel 短暂断开并重新附着原 Session 时保留路由；
6. Session Close/Timeout 或服务关闭后删除路由，新 Session 再从默认源开始。

系统不执行自动故障转移。选择离线来源时保持选择，向该 Session 返回坏质量，绝不静默改到其他来源。

原 Session 重附着还须保持订阅的协议状态：旧 SecureChannel 的 Publish 请求不能消费新通道通知；通知进入重传队列时即占用唯一序号，发送失败也不能让下一条不同数据复用该序号。客户端恢复原 Session 后，应恢复既有订阅的重传及 Publish 循环，而不是另建空订阅来唤醒发布。两个固定依赖的具体补丁及边界测试记录在各自的 `FORK.md` 中。

管理程序下级仍由独立连接监督器管理，保持 `AutoReconnect(false)`，每次新连接重新发现并订阅。客户端补丁不改变这一策略，也不改变安全端点、身份认证或默认选源。

## 固定 namespace 与地址空间

上级服务端要求 namespace 顺序固定：

| Index | URI | 用途 |
|---:|---|---|
| 0 | OPC UA 标准 namespace | 标准节点 |
| 1 | `urn:RcdManager:rcd_manager` | 应用 namespace |
| 2 | `urn:SerialServer` | 旧 RCD 点表兼容 |
| 3 | `urn:RcdManager:management` | 管理节点 |

`ns=2` 保留下级目录发现出的 string NodeId、BrowseName、数据类型、value rank、访问级别和父子关系：

- `Objects/Devices/<KKS>`；
- `Objects/Devices/<port>_Send`；
- `Objects/Methods/<port>_SendCommand`；
- 方法属性 `InputArguments` 和 `OutputArguments`。

`ns=3` 的 string NodeId 以以下形式提供：

- `Groups/<group_id>/DefaultSourceId`；
- `Groups/<group_id>/SelectedSourceId`；
- `Groups/<group_id>/SelectionRevision`；
- `Groups/<group_id>/MemberSourceIds`；
- `Groups/<group_id>/PointNodeIds`：该组当前可发布、非热退役的 `urn:SerialServer` 数据变量 identifier；
- `Groups/<group_id>/PointNodeCount`：清单元素数，配合标准 OPC `IndexRange` 分页；
- `Groups/<group_id>/PointSetFingerprint`：点集变化指纹；
- `Groups/<group_id>/PointSetRevision`：进程内单调点集版本；
- `Groups/<group_id>/CatalogRevision`：覆盖组点表和全部成员的目录 seqlock；
- `Groups/<group_id>/PendingCommandCount`；
- `Groups/<group_id>/Members/<source_id>`：该逻辑组的候选物理来源；
- `Groups/<group_id>/Members/<source_id>/Ports/<port_id>/Devices/<device_id>`：来源内实际串口、子站和点数的只读树；
- `Sources/<source_id>/ConnectionState`、`SessionState`、`SubscriptionState`、`CatalogState`、`LastDataAt`、`LastProbeAt`、`LastError`、`ReconnectCount`；
- `Sources/<source_id>/PointCount`（兼容别名）、`MonitoredPointCount`、`SubscriptionCount`、`TotalPointCount`、`GoodPointCount`、`BadPointCount`。

`DefaultSourceId` 是全局持久配置，写入空字符串会清除默认源；`SelectedSourceId` 和 `SelectionRevision` 的读写上下文来自当前 OPC Session，不使用全局 backing value。
成员来源节点同时提供 `Selectable`、当前 Session 的 `IsSelected`、`ConnectionState`、
`CatalogState` 和并集点位的期望/提供/缺失覆盖统计，并通过 `CoverageKnown`、
`CoverageFingerprint`、`CoverageRevision`、`ProvidedPointNodeIds`、`MissingPointNodeIds` 公开相对当前可发布并集的
覆盖快照。每组或每成员的 revision、fingerprint、count 和数组由同一个原子不可变快照提供；客户端
在单个数组分页前后校验自身 revision；跨点表与成员读取时还要在最外层校验相同的非零偶数
`CatalogRevision`，不一致或为奇数时丢弃重读。串口节点提供 `PortId`、显示名和命令
NodeId；设备节点提供 `DeviceId`、地址是否已知、名称、KKS 前缀、原始/计算/合计点数，
以及有效归组 `Configured`、人工绑定 `ExplicitlyBound`、拓扑快照存在性
`PresentInTopology`。这棵树完全只读，客户端仍只向
`Groups/<group_id>/SelectedSourceId` 写入 String 类型的 source_id，从而保持唯一的选源入口。
含 `/`、空格等字符的物理 ID 会在管理 NodeId 路径段中进行 URL 转义，BrowseName 和只读值
始终保留原始 ID；客户端应 Browse 这些节点，不应自行拼接转义后的物理 NodeId。
树的热更新按稳定 NodeId 做差异化增删，并原位更新变量读取器；未变化节点不会经历删除窗口，
现有监控项可继续使用。缓存 JSON 或拓扑语义损坏只隔离对应来源并清除不可发布快照，不阻止
其他来源和管理服务启动；点表冲突来源虽不可选择，仍保留已验证物理拓扑供排障。

## 独立的下级运维通道

Rust 下级在已有拓扑 namespace 之后追加 `urn:SerialServer:operations`，不改变业务节点，
也不改变管理 OPC 的四个固定 namespace。Go 通过现有下级 OPC 连接按 URI 发现
`Operations/Capabilities` 和 `Operations/Execute(Request String) -> Response String`。
运维目标为物理 `source_id/port_id/device_id`，不经过 Session 选源路由。

Runtime 在短配置锁内固定来源配置版本、连接实例和 epoch；网络请求在锁外执行。
每个来源只允许一个运维修改；执行期间拒绝该来源更新、删除或重连，其他来源、选源和采样
不等待这次 RPC。命令锁通过已发现的拓扑把原始物理 port_id 映射到旧 OPC 端口名称；Rust
还在命令从入队到 ACK 结束的整个生命周期持有物理端口许可，与采集控制原子互斥。

采集控制先原子持久化期望状态，再由原串口 worker 在安全边界应用并确认。等待超时返回
`Pending`，实际打开和通信状态由 worker 回报，不由期望开关推断。已有配置文件采用完整
配置集校验、内容 SHA256 CAS、备份、原子替换；当前进程继续使用启动配置，重启才应用。
日志读取有代次和字节上限。运维错误不误报为 UA 通道失败；修改只派发一次，超时提示核对。

文件正文不写 SQLite 或审计；管理审计只保存目标、版本、动作、内容哈希、时延与结果。
Web 面板按来源和版本缓存，按标签按需请求，退出后释放配置/日志查询缓存。协议与 REST
接口见 [下级运维](lower-operations.md)。

## 数据值规则

下级样本在写入分片内存缓存后视为不可变。SQLite 不参与实时读路径。

| 场景 | 上级输出 |
|---|---|
| 有正常样本且已连接 | 保留下级值、StatusCode、SourceTimestamp；ServerTimestamp 取当前管理程序时间 |
| 已连接但没有首值 | 对应类型零值 + `BadWaitingForInitialData` |
| 下级断开且进程内有末值 | 保留末值和 SourceTimestamp，StatusCode 改为 `BadNoCommunication` |
| 缓存目录离线启动且无内存样本 | 对应类型零值 + `BadNoCommunication` |
| 所选源缺少组并集中的点 | 对应类型零值 + `BadConfigurationError` |
| 当前进程内热删除点 | 对应类型零值 + `BadOutOfService` |

管理程序不会把下级计算点错误地标为 Bad，也不会把下级自身不准确的 Good 状态“修正”为其他质量；连接正常时忠实转发下级质量。

## 下级连接与 epoch

每个来源拥有独立监督器。断线后以 1 秒到 30 秒指数退避，并加入约 ±20% 抖动重新连接；每次新连接都重新解析 namespace、重新发现目录并重建订阅。连接和运行时还叠加 generation/epoch，旧连接迟到的状态、目录或数据不会覆盖新连接。若 DPAPI 凭据读取或监督器工厂本身临时失败，来源 `LastError` 会立即更新，后台继续尝试创建该来源的监督器。仅修改来源显示名称等不影响连接的元数据不会中断监督器；endpoint、group、安全参数、用户名、密码或启用状态变化才会重启对应来源。

除订阅 Publish 外，监督器默认每 20 秒读取一次下级 `NamespaceArray` 作为 Session 活性探针。该探针用于兼容不会把 Publish 计入 Session 活动、且服务端 SessionTimeout 较短的下级；探针失败会结束当前连接并进入统一重连流程。探针间隔必须短于协商出的下级 SessionTimeout，当前对已知 Rust 下级的 60 秒上限留有足够余量。

## 命令路由

写 `<port>_Send` 或 Call `<port>_SendCommand` 时，管理程序在操作开始时固化 Session、组、来源和端口。pending key 为 `source_id + port`，同键不排队；冲突返回 `BadWaitingForResponse`。

任一端口命令 pending 时，其所在组的默认源和所有 Session 选源均被锁定：OPC 返回 `BadWaitingForResponse`，REST 返回 HTTP 409。Session 中途关闭或来源随后断线不会将命令改投其他来源。

`_Send` 写成功后等待同一来源同一端口的 ACK：`2` 表示仍在等待，`1` 完成，`3` 失败。每次建立或重建订阅后，该端口必须先有一个当前 connection epoch 的 `_Send` 样本通过运行时，写命令才允许下发；这使异步到达的保留值只成为 sequence baseline，而不会被认作新命令的 ACK。成功 `1` 必须跟随本次命令的新 `2`；真实下级在派发立即失败时可能直接给出 `3`，管理程序会接受同 epoch、post-baseline 的直接失败 ACK。

为处理下级在一个 250 ms 采样周期内快速完成 `2 → 1/3`、且最终字符串与上一条命令相同而导致订阅合并通知的情况，写前会读取 `_Send` 的 ServerTimestamp 作为基线，写成功后启动短时、仅针对该端口的 50 ms Read 探针。只有 ServerTimestamp 严格晚于写前基线且 connection epoch 仍匹配的 `1/2/3` 才会送入 ACK 状态机；旧终态不会被复用。探针遇到终态或上下文结束即停止，不替代常规订阅。超过 pending timeout 后记为 `TimedOut`。`_SendCommand` 在短超时内同步 Call，并在返回时结束命令记录。

## 持久化边界

SQLite 使用 WAL、foreign keys、5 秒 busy timeout 和 `synchronous=NORMAL`。schema migration 通过 `PRAGMA user_version` 顺序执行。当前表包括：

- `groups`、`sources`；
- `catalog_nodes`；
- `command_audit`；
- `management_audit`；
- `source_events`、`alarms`。

持久化内容包括组、来源、默认源、目录缓存、revision、命令/管理审计、控制面状态变化和告警。告警只记录连接、目录和订阅等低频状态，不保存点值；热数据路径仍不写 SQLite。Session 选源及实时样本不持久化。下级密码通过 Windows DPAPI machine scope 加密后才进入数据库；API 只返回 `has_password`，不回传明文。目录、命令终态、管理审计、来源状态事件和告警在瞬时 SQLite 失败后都会保留 dirty 工作并由后台重试；`/api/v1/system` 暴露各类待写数量，`/healthz` 在未恢复前保持不健康。优雅关闭会在停止后台工作后再次尝试刷新这些记录；若持续存储故障超过关闭期限，关闭仍按配置的 deadline 返回错误。

来源状态发生变化时写入 `source_events`，异常条件按 `source_id + kind` 合并为一个打开告警并累计次数；恢复后保留历史行，后续再次发生会新开一行。确认和静默均使用 revision 乐观并发。禁用或删除来源会恢复其打开告警。历史默认保留 90 天并设置单表行数上限，`Waiting` 命令和未恢复告警不参与过期删除。
