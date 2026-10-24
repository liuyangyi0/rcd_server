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

### 开发线中的组件边界（已阶段发布到 46、30、58）

当前拆分采用单进程内部组件，未新增远程服务：

本节对应的阶段版本于 2026-09-22 在 46 下级和管理端部署，2026-09-28 升级到 30/58 下级及 58 管理端。现有进程守护 agent 保留原版本。完整架构拆分和长时验收尚未完成，发布/备份/验证边界见[实施记录](../validation/architecture-tracking-validation.md)。

- `app/command_execution.go`：执行元数据与完成协调；实际端口占用仍只由 `domain.RouteTable` 管理。
- `app/command_audit_service.go`、`command_tracking.go`：有界审计缓存、持久化补存及按来源同步下级证据；不是命令重投队列。
- `app/maintenance_service.go`：独占运维修改租约、进程操作持久栅栏与补查租约。远程调用超时只结束本次调用租约，不能解除未确认结果的栅栏；终态持久化后才解除。旧请求结束不能释放新请求的租约。
- `app/calculation_service.go`：独占活动定义、Runner、计算结果/退役点、脏输入、状态输入签名及事件节流。Runner 由单独求值锁串行访问，结果查询只使用短状态锁；输入快照取完即释放配置读锁，再执行表达式。配置、默认源、来源/目录失效递增内部 generation，已过期批次不得提交结果。草稿/活动版本的数据库事务及目录发布仍由 Runtime 协调。
- `app/redundancy_service.go`：独占组策略、健康/差异确认计时、切源冷却及告警状态；负责候选判断与差异比较。检测使用脱离组件内部存储的副本，提交校验 generation，防止旧检测覆盖策略变更、组删除/重建或更新的检测。缓存扫描、路由提交、数据库及事件发布不持有组件状态锁；外部副作用仍由 Runtime 配置事务协调，不能只凭 generation 校验就执行切源。
- `app/catalog_delivery.go`：`catalogPersistence` 独占待落盘目录副本、内部版本及各来源保存错误；`catalogPublication` 独占 OPC 目录回调、发布错误与重试状态。写数据库/发布回调不持有组件状态锁；保存确认按内部版本而非发现时间清除队列。计算激活、停用、新发现和配置变更共用发布路径，初次注册也进入配置事务，避免旧初始快照覆盖热更新。目录内容和路由的权威状态仍在 Runtime，组件不自行改变来源或路由。
- `app/source_connections.go`：独占每物理来源的监督器句柄与 generation；预留、挂接、停止、退出通过组件方法操作。`source_lifecycle.go` 协调创建/运行/关闭及带代次回调。生产代码在 Runtime 状态锁内完成代次检查及相关状态/目录/缓存提交，组件不反向调用 Runtime。取消函数在状态锁外执行；旧 worker 结束不删除新 worker，创建失败、退出及关闭均废弃旧代次，关闭还覆盖尚未挂接的创建预留。
- `app/station_sync.go`：独占站点刷新租约、合并失效标记、能力缓存及按来源记录的保存错误。队列仍为 64 项、4 个只读刷新 worker；刷新结束按唯一票据释放，不按来源 ID 直接删除新任务。能力响应提交前复核连接代次、epoch、在线状态和拓扑；能力查询不修改站点类型或触发硬件版本命令。
- `app/station_snapshots.go`：独占物理来源的不可变站点快照、接收 epoch 与有效标记，以及按串口/站点二元键构建的查找索引。深拷贝输入和对外返回的数据，不暴露内部切片或版本数值指针；心跳与失效替换快照头，不改动正在被读取的旧快照。Runtime 继续协调连接/拓扑检查与提交，组件不自行改变来源或路由。版本列表先在一致性锁内固定快照视图，再在锁外复制站点正文；通信状态查询按索引读取，不遍历全部站点。
- `app/status_history.go`：独占来源历史事件 FIFO、按来源合并的待更新告警、内部工作版本和分项保存错误。数据库操作由单个写入者串行完成，但不持有组件状态锁。每轮事件只处理开始时已有的部分，新增事件进入下一轮；事件写入失败仍尝试告警，一个来源的告警失败不阻止其它来源。写入成功按工作版本确认，不以状态内容相等删除新工作。历史队列仍是进程内重试缓存，并非持久消息队列。
- `app/management_audit.go`：独占管理审计待保存记录、不可变详情和分项错误。接收时固定时间与 JSON 详情，解码保留大整数精度；首次保存和补写共用逐记录占用，不重复并发保存同一记录，数据库 I/O 不持有组件状态锁。单条成功不能清除其它记录的错误；非法详情保留失败标记而非静默丢弃。补写只保存审计，不重新执行管理操作或设备命令；关闭时在既有期限内等待已接收写入，再尝试失败记录。该队列仍在进程内，沿用数据库去重/保留规则，不新增持久消息队列或容量保证。
- `lowerlogs.Service`：按来源拥有接收序号/锁与健康状态；查询固定文件句柄和有效字节边界后不再持有写锁。
- Rust `serial/transport.rs`：有截止时间的读写、线速预算与输出队列等待；只借用唯一串口 worker 的句柄，不新增串口读取者。
- Rust `serial/version_transaction.rs`：版本查询去重队列、独立响应组帧与迟到帧隔离；不持有普通轮询下标、KKS 或通信失败计数。`command_transaction.rs` 单独负责控制命令重试与 ACK 过程。

运维准入锁顺序保持“Runtime 配置协调锁 → 运维组件内部短锁”；运维组件持锁期间不调用 Runtime、其它组件或进行网络/数据库操作。命令完成与 RouteTable 占用释放仍通过短事务协调，不维护第二套 pending。运维进程请求/快照类型移到 `domain`，HTTP 层保留兼容别名，JSON 字段和接口不变。

来源运行状态汇总、目录索引/隔离原因等仍有 Runtime 状态。目录发布构造副本前只短暂读取索引指针，不再在 Runtime 状态锁内复制整个并集；但配置事务仍协调目录提交、发布及缓存落盘，来源工厂创建及冗余检测也保留原配置事务范围，尚未把全组扫描与持久化都改成短事务。站点快照已迁移，但与状态/拓扑一致的提交和物理拓扑指纹校验仍由 Runtime 协调；不能将组件内部短锁等同于整体配置锁已完成优化。实际验证及未完成项见[架构整理记录](../validation/architecture-tracking-validation.md)。

站点版本保存失败后，内存快照继续供查询，后续有效状态刷新即使内容相同也重试保存；另一来源保存成功不清除该来源错误。恢复后，只有串口/站点运行标志变化的周期刷新仍不写库。站点保存/迟到回滚与来源清理共用写入协调锁；来源重置、删除和整组级联删除一并清除站点快照与刷新状态，避免旧写入恢复已删除记录。该锁当前仍跨来源串行，并非已完成按来源并发持久化改造。

## 稳定点标识与目录

内部不保存易变化的 namespace index，而使用：

```text
NodeKey { NamespaceURI, Identifier }
```

每次下级连接后，监督器读取 NamespaceArray，以 URI 重新解析 namespace index，再浏览：

- `Objects/Devices` 下的变量；
- `Objects/Methods` 下的方法。

当前支持的变量类型为 `Boolean`、`UInt32`、`Float`、`Double` 和 `String`。`_Send` 必须是可写标量 String；`_SendCommand` 必须严格为 `HexData String -> Result String` 的标量方法。目录属性按最多 100 个节点一批读取；每个来源只创建一个 Subscription，再按最多 500 个一批加入 MonitoredItem，避免真实大点表同时触发消息尺寸和下级 Subscription 数量限制。默认 sampling/publishing interval 为 50 ms。

同组目录采用并集。同名节点只有 class、data type、value rank，以及方法签名一致时才能合并。以下情况会使涉及的来源进入 `CatalogConflict` 并不可选：

- 同一来源重复 identifier；
- 同组同名节点形状不一致；
- `_Send` 类型、秩或访问级别非法；
- `_SendCommand` 参数签名非法；
- 同一个 identifier 出现在不同组。

首次发现完成前来源处于 `Discovering`，不可作为默认源或 Session 选源。数据库中已有有效目录时，进程离线启动会将来源标记为 `Cached`，允许选择。

热更新加入的新点立即加入地址空间。当前进程内消失的点保留并标记为 retired，读取返回 `BadOutOfService`；重启加载目录时会丢弃 retired 点。

配置或目录版本提交后，如果向上级 OPC 地址空间应用最新快照时发生临时错误，Runtime 会保留 dirty 状态、令 `/healthz` 的 `upstream_catalog` 检查失败，并由后台在配置串行门内按最新完整快照重试。成功收敛后自动清除 dirty 健康错误；确定性的点表形状错误仍按 `CatalogConflict` 隔离来源，不进入无意义重试。

中央计算停用后切回下级计算点表也使用该重试流程。来源目录隔离与临时发布故障分别保留：发布重试成功不能清除尚未恢复的来源隔离，隔离解除也不能隐藏仍未成功的发布。全部问题解除后不保留过期的健康错误。

## Session 级选源

每个 OPC UA Session 都有独立的不可变路由快照语义：每个分组记录一个 `source_id` 和递增的 selection revision。

1. 新 Session 激活时复制各组当时的默认源；
2. 修改默认源只影响之后创建的新 Session，不改已有 Session；
3. OPC Session 可以调用组下 `SelectSource(SourceId String, ExpectedRevision UInt64)`，原样校验客户端提供的路由版本，成功输出来源和新版本；冲突返回`BadInvalidState`。旧 `SelectedSourceId` String写入继续保留，但不含客户端期望版本，不能当端到端CAS。Web也可以按Session修改；
4. 切源成功后递增该 Session/组的 revision，并分批强制重发该 Session 在该组的既有监控项；即使新旧来源的值、质量和时间戳相同，也会产生一次目标 Session 专属通知；
5. SecureChannel 短暂断开并重新附着原 Session 时保留路由；
6. Session Close/Timeout 或服务关闭后删除路由，新 Session 再从默认源开始。

自动故障转移默认关闭。手动固定的 Session 选择离线来源时保持选择并返回坏质量；显式开启组策略且参与自动的 Session 可按故障/健康确认与冷却规则切到完整健康成员，不自动回切。默认源自动切换另有开关，中央计算只随默认源变化。详见 [自动切源与差异告警](../guides/redundancy.md)。

`MemberSourceIds`只列已配置成员，不承诺在线、可选或点表完整；通过成员树分别检查状态。SelectSource已于2026-10-21随`0.6.12-state-names.20261021.1`部署三台，旧服务可能不存在该Method；三个原有变量的NodeId、类型及权限不变。选择版本、下级发送快照revision和实时数据质量是不同维度。

原 Session 重附着还须保持订阅的协议状态：旧 SecureChannel 的 Publish 请求不能消费新通道通知；通知进入重传队列时即占用唯一序号，发送失败也不能让下一条不同数据复用该序号。客户端恢复原 Session 后，应恢复既有订阅的重传及 Publish 循环，而不是另建空订阅来唤醒发布。两个固定依赖的具体补丁及边界测试记录在各自的 `FORK.md` 中。

管理程序下级仍由独立连接监督器管理，保持 `AutoReconnect(false)`，每次新连接重新发现并订阅。客户端补丁不改变这一策略，也不改变安全端点、身份认证或默认选源。

## 固定 namespace 与地址空间

### 通信程序同步发送输入

新通信程序在 `urn:SerialServer` 的 Devices 下按原文档发布 `DATA.*` 变量：接收点只读，发送输入为可写的UInt32/String标量，不添加串口或方向前缀；Fire使用UInt32状态编号。管理端仅对当前所选来源目录中实际存在且可写的输入转发写入，不把它们当 `_Send` 控制命令或创建 ACK 等待。来源和连接 epoch 在写入开始时固定，错误/不明结果不重投、不换源。每个在途字段通过路由预占阻止组选源和维护竞态，不同字段允许批量并行写；完成下级 Write 后释放，不逐点持久化高频控制命令审计。

通信程序承担输入完整性、单字段新鲜度和组包。默认客户端每 5000 ms 刷新，15000 ms 未写入的必需字段导致停止发送；值相同的 Write 也刷新时间，管理端订阅不得刷新。刷新周期和超时由通信来源配置并通过只读节点公布，不自动改外部客户端定时器。TX 设置/状态与输入均排除来源冗余差异判断。OPC 写入成功仅表示输入缓存接收，串口完成写出及对端业务确认是不同层次；协议无 ACK 时不声称对端确认。

整包入口为 `ns=2;s=TransmitSnapshot` 可写String，状态为 `ns=2;s=TransmitStatus`，刷新/过期参数为 `InputRefreshIntervalMs` / `InputTimeoutMs`。管理端转发完整JSON字段集（上限64KiB），固定Session选源和epoch，保持维护栅栏，不创建ACK等待或自动重投。下级在单次临界区校验revision、完整字段集和类型后整体替换发送缓存。首次整包提交成功后，本进程该端口拒绝逐点写入，避免另一客户端拆改原子快照；未切换前保留逐点模式。选择路由不会撤回旧来源已接受的缓存，也不授予独占发送权。管理端保留对历史`RcdSync.*`来源的写入识别，但新通信程序不再发布这些前缀节点，原名升级需同步替换点表。普通同步字段仍受8192字节限制；非`_Send`数据写入不读取ACK时间基线。完整字段、状态及类型见[通信客户端说明](../../../communicate/docs/opc-client-guide.md)。

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
- `Groups/<group_id>/SelectSource`：Method，父对象`ns=3;s=Groups/<group_id>`；输入`SourceId String, ExpectedRevision UInt64`，输出`SelectedSourceId String, SelectionRevision UInt64`；
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

`DefaultSourceId` 是全局持久配置，写入空字符串会清除默认源；`SelectedSourceId`的读写、只读`SelectionRevision`及`SelectSource`调用均以当前OPC Session为上下文，不使用全局 backing value。
成员来源节点同时提供 `Selectable`、当前 Session 的 `IsSelected`、`ConnectionState`、
`CatalogState` 和并集点位的期望/提供/缺失覆盖统计，并通过 `CoverageKnown`、
`CoverageFingerprint`、`CoverageRevision`、`ProvidedPointNodeIds`、`MissingPointNodeIds` 公开相对当前可发布并集的
覆盖快照。每组或每成员的 revision、fingerprint、count 和数组由同一个原子不可变快照提供；客户端
在单个数组分页前后校验自身 revision；跨点表与成员读取时还要在最外层校验相同的非零偶数
`CatalogRevision`，不一致或为奇数时丢弃重读。串口节点提供 `PortId`、显示名和命令
NodeId；设备节点提供 `DeviceId`、地址是否已知、名称、KKS 前缀、原始/计算/合计点数，
以及有效归组 `Configured`、人工绑定 `ExplicitlyBound`、拓扑快照存在性
`PresentInTopology`。这棵树完全只读；选源应调用组下的带revision `SelectSource` Method，旧客户端仍可向
`Groups/<group_id>/SelectedSourceId` 写入 String 类型的 source_id。两者共用同一Session路由事务，成员树没有选源写接口。
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
接口见 [下级运维](../guides/lower-operations.md)。

## 数据值规则

下级样本在写入分片内存缓存后视为不可变。SQLite 不参与实时读路径。

| 场景 | 上级输出 |
|---|---|
| 有正常样本且已连接 | 保留下级值、StatusCode、SourceTimestamp；ServerTimestamp 取当前管理程序时间 |
| 已连接但没有首值 | 对应类型零值 + `BadWaitingForInitialData` |
| 下级断开或采集点返回通信中断 | 对应类型零值，保留可用的 SourceTimestamp，StatusCode 为 `BadNoCommunication` |
| 缓存目录离线启动且无内存样本 | 对应类型零值 + `BadNoCommunication` |
| 所选源缺少组并集中的点 | 对应类型零值 + `BadConfigurationError` |
| 当前进程内热删除点 | 对应类型零值 + `BadOutOfService` |

除 `BadNoCommunication` 必须把不可用的采集值归零外，管理程序不会把下级自身的 Good 状态“修正”为其他质量；连接正常时忠实转发下级质量。中央计算使用归零后的输入继续执行公式，输出公式所得值，同时沿实际执行路径传播 `BadNoCommunication`。

## 下级连接与 epoch

每个来源拥有独立监督器。断线后以 1 秒到 30 秒指数退避，并加入约 ±20% 抖动重新连接；每次新连接都重新解析 namespace、重新发现目录并重建订阅。连接和运行时还叠加 generation/epoch，旧连接迟到的状态、目录或数据不会覆盖新连接。若 DPAPI 凭据读取或监督器工厂本身临时失败，来源 `LastError` 会立即更新，后台继续尝试创建该来源的监督器。仅修改来源显示名称等不影响连接的元数据不会中断监督器；endpoint、group、安全参数、用户名、密码或启用状态变化才会重启对应来源。

监督器异常返回后，先废弃该生产者代次，再以保留的最后连接 epoch 上报离线，不使用可能被判为旧消息的 local epoch 0。内部终态令牌只允许本次离线报告；后续启动/停止使其失效，不能覆盖新连接。退出时释放子 context，迟到回调不再进入缓存；连接失败不被误写成拓扑拒绝原因。此处不改变监督器自身的重连退避、采样周期或串口命令重试规则。

除订阅 Publish 外，监督器默认每 20 秒读取一次下级 `NamespaceArray` 作为 Session 活性探针。该探针用于兼容不会把 Publish 计入 Session 活动、且服务端 SessionTimeout 较短的下级；探针失败会结束当前连接并进入统一重连流程。探针间隔必须短于协商出的下级 SessionTimeout，当前对已知 Rust 下级的 60 秒上限留有足够余量。

## 命令路由

写 `<port>_Send` 或 Call `<port>_SendCommand` 时，管理程序在操作开始时固化 Session、组、来源和端口。pending key 为 `source_id + port`。变量写入采用每物理端口 FIFO，最多 64 条（含正在执行的一条）；队满返回 `BadWaitingForResponse`，不覆盖已接收命令。同步 Method 不进入队列，有执行或排队命令时仍返回占用。

队列仅位于管理端；下级继续一次执行一个事务，不改变串口时序、重试或 ACK 规则。每条接受的请求保存独立载荷、物理目标及连接代次，相同载荷不合并；只有取得上一条可信终态才派发下一条。下级明确报告 ACK 超时等失败后可以继续；管理端等待超时、通信中断或执行结果不明时终止该端口剩余队列并记明原因，不盲目发送。命令总期限从接受时开始（沿用 `CommandPendingTTL`，默认 30 秒），包括排队时间。进程重启不重放待发命令；持久化的 `Queued` 审计改记为中断失败。

`Groups/<group>/PendingCommandCount` 包含排队与执行；新增 `QueuedCommandCount`、`ExecutingCommandCount`、`CommandQueueCapacityPerPort` 供客户端观察。OPC Write 的 `Good` 只表示已接受，不能作为硬件执行成功。队列不是跨管理端的全局队列；其它管理端的竞争仍由下级物理端口互斥拒绝。新通信程序的`DATA.*`发送输入与`TransmitSnapshot`保持快照同步语义，不进入此命令队列。

任一端口命令 pending 时，其所在组的默认源和所有 Session 选源均被锁定：OPC 返回 `BadWaitingForResponse`，REST 返回 HTTP 409。Session 中途关闭或来源随后断线不会将命令改投其他来源。

`_Send` 写成功后等待同一来源同一端口的 ACK：`2` 表示仍在等待，`1` 完成，`3` 失败。每次建立或重建订阅后，该端口必须先有一个当前 connection epoch 的 `_Send` 样本通过运行时，写命令才允许下发。旧协议来源另须确认写前 ServerTimestamp 基线；写前保留值和暂存终态不得完成新命令。正常情况下观察本次 `2 → 1/3`，快速完成被采样合并或派发立即失败时，也可接受同 epoch、严格晚于写前基线的直接 `1/3`。

为处理下级在一个 50 ms 采样周期内快速完成 `2 → 1/3`、且最终字符串与上一条命令相同而导致订阅合并通知的情况，写前会读取 `_Send` 的 ServerTimestamp 作为基线，写成功后启动短时、仅针对该端口的 50 ms Read 探针。只有 ServerTimestamp 严格晚于写前基线且 connection epoch 仍匹配的 `1/2/3` 才会送入 ACK 状态机；旧终态不会被复用。探针遇到终态或上下文结束即停止，不替代常规订阅。

2026-10-13 开发修复（尚未现场部署）：旧协议的订阅通知和 Read 探针共用写前时间基线；取得基线时清除尚未派发命令的暂存终态。读取和写入过程中只为该命令节点暂存最新通知，不持有跨节点锁执行网络调用。基线读取失败、时间缺失或倒退、下级仍为 `2`、预占已失效或 epoch 已改变时不发送；不具备基线保护的客户端适配器返回 `BadNotSupported`，不得退回无保护写入。快速完成时允许经过严格时间基线验证的直接 `1/3`，不要求采样一定看到中间 `2`。带 `command_tracking` 的关联协议不变；此保护不解决物理 ACK 没有命令编号的限制，也不承诺设备恰好执行一次。

上级调用 `_SendCommand` 时，管理程序保留原 `HexData String -> Result String`、NodeId 和路由方式，共用命令派发与ACK等待流程。下级声明`command_tracking`时使用`submit_command`关联接口，明确不支持时才兼容`_Send`；新接口超时或结果不明绝不回退再发一次。只有设备 ACK `1` 才返回 `Good` 和 Result `"1"`，只表示协议确认；`3`、下级 OPC 拒绝、串口停用/不可服务以及等待超时保留相应坏状态，失败时没有 Result。Method在执行期间、变量命令在排队及执行期间持有pending锁，不把“管理端已入队”记作设备执行成功。

## 持久化边界

SQLite 使用 WAL、foreign keys、5 秒 busy timeout 和 `synchronous=NORMAL`。schema migration 通过 `PRAGMA user_version` 顺序执行。当前表包括：

- `groups`、`sources`；
- `catalog_nodes`；
- `command_audit`；
- `management_audit`；
- `source_events`、`alarms`。

持久化内容包括组、来源、默认源、目录缓存、revision、命令/管理审计、控制面状态变化和告警。告警只记录连接、目录和订阅等低频状态，不保存点值；热数据路径仍不写 SQLite。Session 选源及实时样本不持久化。下级密码通过 Windows DPAPI machine scope 加密后才进入数据库；API 只返回 `has_password`，不回传明文。目录、命令终态、管理审计、来源状态事件和告警在瞬时 SQLite 失败后都会保留 dirty 工作并由后台重试；`/api/v1/system` 暴露各类待写数量，`/healthz` 在未恢复前保持不健康。优雅关闭会在停止后台工作后再次尝试刷新这些记录；若持续存储故障超过关闭期限，关闭仍按配置的 deadline 返回错误。

来源状态发生变化时写入 `source_events`，异常条件按 `source_id + kind` 合并为一个打开告警并累计次数；恢复后保留历史行，后续再次发生会新开一行。确认和静默均使用 revision 乐观并发。禁用或删除来源会恢复其打开告警。历史默认保留 90 天并设置单表行数上限，`Waiting` 命令和未恢复告警不参与过期删除。

来源状态通知可能在释放 Runtime 状态锁后才进入历史队列。历史保留该次原始转换，而告警目标在入队时重新读取权威当前状态，防止迟到通知把已恢复的旧故障重新设为目标。正在写库时出现更新，旧结果不得清除新工作，新目标由后续轮次更新；这不是数据库与内存状态跨组件的原子事务。每轮补写使用 2 秒 context 期限（若上层期限更短则服从上层），取消后不再发起后续数据库尝试，未完成工作保留至下一轮；该期限不改变命令或串口超时。来源事件和各来源告警分别报告保存错误，告警恢复成功不掩盖尚未补齐的历史。队列仍沿用内存保留策略，未新增磁盘故障下的无限时长保障或恰好一次历史写入承诺。
