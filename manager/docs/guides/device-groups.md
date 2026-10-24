# 设备归属与多逻辑组选源（v2）

## 兼容性与范围

基线提交为 `70586a3`。旧 Rust `urn:SerialServer` 命名空间、`Objects/Devices/<KKS>`、
`<port>_Send` 和 `Objects/Methods/<port>_SendCommand` 不移动、不重命名。
Rust 新增拓扑命名空间 `urn:SerialServer:topology`；管理端仍以一个 physical source
建立一条下级连接，允许该来源提供多个逻辑设备组。安全配置保持基线行为。

## 下级拓扑协议 v1

新增 `Objects/Topology` 浏览树，包含 Ports / Devices / Points（引用原 KKS 节点）和
Calculations。新命名空间中的 String 节点 `Topology/Manifest` 返回以下 JSON：

```json
{
  "schema_version": 1,
  "ports": [{
    "port_id": "/dev/ttyS0",
    "port_name": "ttyS0",
    "send_node_id": "ttyS0_Send",
    "send_method_id": "ttyS0_SendCommand",
    "devices": [{
      "device_id": "1",
      "device_address": 1,
      "name": "设备 1",
      "kks_prefix": "UNIT1",
      "node_ids": ["UNIT1_TEMP"]
    }]
  }],
  "calculations": [{
    "node_id": "UNIT1_CALC",
    "dependency_node_ids": ["UNIT1_TEMP"],
    "owners": [{"port_id": "/dev/ttyS0", "device_id": "1"}]
  }]
}
```

`device_id` 是设备地址的十进制字符串；`port_id` 是配置中的完整串口名。
源内唯一身份为 `(port_id, device_id)`，不是跨下级的逻辑设备标识。
JSON 中的业务节点标识均是 `urn:SerialServer` 中的 String NodeId identifier。
所有集合确定性排序；同源设备/点位归属重复、非法地址、无法序列化等错误明确拒绝。
空计算 owner 数组表示无已知设备依赖；多 owner 表示跨设备计算。
未激活中央计算时，单设备下级计算点自动归属该设备，多设备/常量计算留在默认组并展示依赖，管理端仅转发。激活中央计算后，由管理端按各组全局默认来源求值，结果由所有 Session 共享；Session 选源不改变中央计算结果。
老下级无此 URI 时维持原单组行为；存在 URI 但 manifest 非法时不能悄悄退回单组。

## 管理端配置及存储

`Source.group_id` 保留为兼容默认组，用于未映射点、跨设备计算和共享端口命令节点。
新增 `Source.group_bindings`：

```json
[{"group_id": "device-a", "port_id": "/dev/ttyS0", "device_id": "1"}]
```

一个设备只能绑定一次；允许多个设备映射到同一逻辑组，但命令解析必须唯一。
只要该来源实际发现了组内至少一个映射设备即可选用；缺少的其他设备/点仍按并集规则返回
`BadConfigurationError`。若整组映射均不存在，或旧下级完全没有拓扑，则该设备组不可选用该来源。
`Source.HasGroup(groupID)` 统一处理默认组和 bindings；`SourceCatalog.Topology` 保存原始拓扑。
`CatalogNode.GroupID/PortID/DeviceID/Kind` 保存解析后的归属，Kind 为
`point` / `calculation` / `command`（旧缓存空值保持兼容）。
同组同名同类型点合并，跨组重名和归属冲突继续隔离来源。
拓扑及 bindings 进入 SQLite 增量 migration；旧库、旧缓存和配置备份继续可读。
API Source 返回 bindings；另提供 `GET /api/v1/sources/{id}/topology`，只含结构/点数，
不把全量实时样本推入前端状态。点位分页行增加归属字段，支持 port/device 过滤。
改变现有绑定关系时要求没有在线 Session 和相关待处理命令；普通 Session 选源不受此限制。
删除组不能级联误删仍服务其他组的来源，必须先解除关联。
冗余来源的同名 KKS 必须绑定到同一逻辑组。依次保存多来源映射时，第一次保存可能暂时触发
跨组目录冲突；完成其他来源的对应映射后自动重新校验恢复，不需要手工修改数据库。
冗余来源应使用相同的兼容默认组：旧串口命令节点仍归该组，若两个来源有同名
`COMx_Send` 等节点却配置了不同默认组，仍会按跨组重名规则报告目录冲突。

## 上级 OPC 客户端浏览与选源

管理命名空间在每个逻辑组下公开只读物理成员树：

```text
Groups/<group_id>/Members
└─ <source_id>
   ├─ Selectable / IsSelected / ConnectionState / CatalogState
   ├─ ExpectedPointCount / ProvidedPointCount / MissingPointCount / CompleteCoverage
   ├─ CoverageKnown / CoverageFingerprint / CoverageRevision
   ├─ ProvidedPointNodeIds / MissingPointNodeIds
   └─ Ports/<port_id>/Devices/<device_id>
      └─ DeviceAddress / AddressKnown / DeviceName / KKSPrefix
         PointCount / RawPointCount / CalculationPointCount
         Configured / ExplicitlyBound / PresentInTopology
```

组节点的 `PointNodeCount` / `PointNodeIds` 公开该组当前可发布、非热退役的
`urn:SerialServer` 数据变量 identifier，并以 `PointSetFingerprint` 标识内容、`PointSetRevision`
标识本进程内的每次变化；客户端分页前后必须校验 Revision 相同。组级 `CatalogRevision` 是覆盖
点表和全部成员的 seqlock：整套读取前后必须是同一个非零偶数。客户端可先用它从
KKS 精确反查逻辑组，再通过成员的 `ProvidedPointNodeIds` / `MissingPointNodeIds` 判断该来源是否
提供兼容的此点。`CoverageKnown=false` 时清单尚未完成；冲突来源的清单只相对当前可发布组并集，
用于诊断而不能表示该来源全部原始目录。`MemberSourceIds` 是已配置成员列表，包含离线、停用和不可选来源，不是健康列表；须另查成员的 `Selectable`、`ConnectionState`、`CatalogState` 和 `CompleteCoverage`。允许选择有效缓存的离线成员，`Selectable=true` 不保证在线或点值为Good。

客户端 Browse 成员树确认 source、实际串口和子站关系。推荐在同一OPC Session内读取 `SelectionRevision`，调用组对象下的 `SelectSource(SourceId String, ExpectedRevision UInt64) -> (SelectedSourceId String, SelectionRevision UInt64)`。例如ObjectId为`ns=3;s=Groups/device-a`，MethodId为`ns=3;s=Groups/device-a/SelectSource`。只有客户端提供的期望版本匹配才提交；版本冲突返回`BadInvalidState`且不修改选择。成功输出来自本次已提交路由，不通过另一次读取得到；返回后仍可能有后续合法操作。

旧客户端仍可将目标 `source_id` 作为String写入同组 `SelectedSourceId`，NodeId、类型和语义保持；它没有客户端期望版本参数，先读Revision再普通Write也不能防止陈旧意图覆盖。新Method与Web/旧Write共用路由事务、命令锁、审计和订阅刷新；不允许另设旁路。以上操作与业务点 Read/Subscribe 必须使用同一个 OPC Session；
切换的是该 Session 的整个逻辑设备组，不是整台下级，也不是一个 KKS。成员树没有写接口，
串口/子站映射只能通过经过 revision 校验的 Web/REST 配置修改。

新增Method已于2026-10-21随`0.6.12-state-names.20261021.1`部署到30、58、46，其他旧服务须先确认能力。参数数量缺少/多余为`BadArgumentsMissing`/`BadTooManyArguments`，类型不符为`BadTypeMismatch`，目标不可选为`BadConfigurationError`，组有在途事务为`BadWaitingForResponse`。失败不返回成功输出，不用最新revision自动重试旧选择。手动Method与旧Write都会将本Session本组改为手动固定；`SelectionRevision`不是通信程序`TransmitStatus.revision`。切源不撤回旧下级已接受的快照，也不建立独占发送权。

`Configured` 表示设备已有效归入该组（包括继承来源默认组），`ExplicitlyBound` 只表示存在人工
绑定；`PresentInTopology` 表示设备存在于最后一份已验证的拓扑快照，来源是否在线另看
`ConnectionState`。未发现的配置占位设备用 `AddressKnown=false`，其 `DeviceAddress` 返回
`BadWaitingForInitialData`，不会把未知地址伪装成合法的站址 0。`Selectable` 只表示允许选中；
若 `CompleteCoverage=false`，选中后缺失点仍按协议返回 `BadConfigurationError`。

物理树热更新采用稳定 NodeId 的差异化增删：名称、Endpoint、点数等变化只更新原变量读取器，
不会删除再创建未变化节点，因此已有监控项保持有效；真正被配置删除的分支才会移除。

## 站点通信状态（数值变量）

客户端可在 `ns=3` 浏览并订阅：

`Groups/<group_id>/Members/<source_id>/Ports/<escaped_port>/Devices/<device_id>/CommunicationState`

这是只读 `UInt32` **变量节点**，不是目录。`Sources/<source_id>/Ports/.../Devices/...` 下也提供相同状态。

| Value | 含义 |
| --- | --- |
| 0 | 未知：尚无当前连接的状态、状态超过 15 秒未更新，或启用操作尚未应用 |
| 1 | 通信正常 |
| 2 | 通信异常：超时、坏帧或尚无有效采集数据；不能仅凭此值判定设备损坏 |
| 3 | 已配置启用，但串口未打开 |
| 4 | 串口或站点读取已配置停用 |
| 5 | 管理器与下级 OPC 连接/Session 不可用 |

配套 `CommunicationStateText`（String）给出文字解释，`CommunicationObservedAt`（DateTime）
给出管理器收到这份下级状态的时间，**不是设备最后正常采集时间，也不是故障开始时间**。
尚无状态时，时间节点质量为 `BadWaitingForInitialData`。

`CommunicationState` 的 OPC DataValue 质量为 `Good` 表示“这个状态判断有效”，不表示设备正常。
例如 `Value=5, StatusCode=Good` 的含义是“已知下级离线”。客户端判断设备好坏请读取 Value，
不能只看这个诊断节点自身的质量。业务 KKS 的数值、质量规则不变。

状态按物理来源独立，不跟随任何 Session 选源；可以同时订阅三个来源再决定选哪一个。
管理器每约 2 秒读取下级运维状态（不额外发送串口请求），收到新状态后由 OPC 监控项采样通知。
下级离线优先显示 5；恢复连接后必须收到新状态才恢复正常，不使用旧连接或磁盘缓存假装在线。
未知/过期显示 0；仅已启用的自动切源策略会使用这些状态做延时判断，默认仍由人工选源。状态刷新在内存完成，普通通信变化不写入版本 SQLite 缓存。策略说明见 [自动切源与差异告警](redundancy.md)。

例如实际串口 ID 为 `/dev/ttyWCH0`，路径中使用 `%2Fdev%2FttyWCH0`。推荐用 Browse 获取 NodeId，
不要根据机器 IP 或显示名称猜路径。组与来源的显示名称均可在 Web 编辑；固定 ID 不随改名变化。

## 命令路由与反馈

共享的旧 `_Send` / `_SendCommand` 节点不能按其默认组直接发送。先从 hex payload
首字节解析设备地址，结合公开端口及拓扑确定逻辑组，再固定 Session 选中的 source、
实际串口和下级命令 NodeId。不可重写原始协议字节；目标地址必须与选中设备匹配。
无法唯一确定设备、端口、选源或成员关系时返回 `BadConfigurationError`，不派发命令。
无新拓扑/无设备 bindings 的旧配置仍按原单组规则处理。
pending 保持 `source_id + physical port`，同一物理串口跨设备仍只能执行一条；选源锁针对本次命中的逻辑组。
连续写 `_Send` 或组 `Send` 时，管理端为每条命令保存独立载荷并按该端口的接受顺序发送，
队列最多 64 条（包含正在执行的一条）。队满明确返回 `BadWaitingForResponse`；不会覆盖旧命令。
接受成功不等于设备成功。排队命令也锁定所在组选源，并计入维护排空数量。
总期限包含排队时间，沿用管理端命令 pending 超时（默认 30 秒）。下级明确返回失败后继续下一条；
如果管理端失联、等待超时或不能确认上一条执行结果，则终止剩余队列并记录原因，不自动重发。
管理端重启也不会重放这些命令。同步 `SendCommand` 方法保持忙时拒绝，不参与排队。

`_Send` 的空值、IDLE、0/1/2/3 是被动复位/状态写，不是硬件控制命令。它不进硬件 FIFO、没有发送次数审计，但仍受管理端/来源维护及同端口互斥保护；复位 RPC 正在执行时新的同端口命令明确返回 `BadWaitingForResponse`，不会假装入队成功。复位结束才释放占用。这项保护随 2026-10-23 审查修复，当前为隔离验证版，见[复查记录](../validation/command-chain-adversarial-review-20261023.md)，不能据本文假定旧现场版本已具备。

关联协议的 `Expired` 可能由下级独立看门狗提前报告，不能证明串口 worker 已退出交换或释放占用。因此管理端保留超时结果并取消该端口未派发的队列，不因稍后收到该条的成功证据自动恢复队列。明确的完成 ACK/失败则在下级释放物理占用后发布，才允许接续下一条；同一条命令 ACK 超时重试耗尽与执行结果未知是不同情况。补查只补审计，不重投、不追溯更改已经返回给客户端的结论。

新版采集下级可设置 `settings.command_interval_ms`，在同一串口上一控制事务完成后延迟下一条首次发送；默认 0，保存配置后重启下级生效，配置步骤见[下级配置说明](lower-operations.md#控制命令最小发送间隔)。等待期间不阻塞普通采集，不改变同条命令重试。不同管理端没有共享 FIFO，但连接同一下级时都受该下级的物理串口间隔约束。间隔等待不会更新管理端的总期限，关联请求仅传递当时剩余的执行预算；到期不补发。

Web 命令记录显示“排队待发送”，每条命令仍保存独立审计。OPC 可读取
`Groups/<group>/QueuedCommandCount`、`ExecutingCommandCount`、`CommandQueueCapacityPerPort`；
原 `PendingCommandCount` 是排队与执行的合计。`_Send` 结果仍代表该 Session 最近接受的一条，
不要求订阅者收到所有中间状态；要核对每条命令请看审计。不同管理端之间没有共享 FIFO。
`_Send` 写入保持 pending 至设备 ACK 或超时；ACK 绑定已固定的 source/port/epoch，
Session 关闭不重定向正在执行的命令。上级`SendCommand`保留原方法签名，共用命令派发与ACK等待；下级支持`command_tracking`时使用关联`submit_command`，明确不支持时才兼容`_Send`写入。新接口超时或结果不明不退回旧接口重发。只有ACK `1`返回`Good`和Result `"1"`，仅表示协议确认；ACK `3`
返回坏状态，串口停用/不可服务保留下级状态，等待超时返回 `BadTimeout`。其 pending 锁同样保持到
设备终态，不再把下级 OPC 入队成功当作设备执行成功。
旧共享 `_Send` 读取优先显示该 Session 最近一次该节点命令的结果，按 command ID
独立保存 `2`（等待）与 `1` / `3`（成功 / 失败）；其他 Session 或同串口另一设备的
后续 ACK 不得覆盖它。历史仅在进程内保存，Session 关闭后清除。原本 Good 的历史会叠加
当前离线/退役等坏质量；已失败的命令历史保留原失败质量，可同时查看来源状态获知当前连接情况。
无历史且端口内设备选源分裂时返回坏质量，不能任意展示某个下级的 ACK。
新增管理命名空间的 `Groups/<group>/Send` 与 `SendCommand` 可直接指定逻辑组；
它们也通过 payload 地址验证唯一设备/端口，保持相同的物理端口互斥及原始命令协议。

## 验证要求

所有构建和测试在远端 `192.168.10.46` 完成。使用独立源码/运行/数据/日志目录和端口，
不使两个采集实例同时打开现有真实串口。Rust PTY 下级可用于隔离的协议回归，不替代真实设备验收；读取或执行测试工具前须核对副作用与本次授权。
验证旧黄金点表、拓扑发现、命名空间 URI 解析、双 Session 独立选源、缺点/离线/缓存、
同值切源通知、不同下级串口名、共享串口命令互斥/ACK、配置迁移、API 和 Web 流程。
保留测试日志与截图；24 小时稳定性和目标 Windows 服务验收必须单独明确执行状态。
本次已执行结果与未覆盖项见 [远端验证记录](../validation/device-groups-validation.md)。
