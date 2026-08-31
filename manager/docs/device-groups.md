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
单设备计算自动归属该设备，多设备/常量计算留在默认组并展示依赖，不由管理端重新计算。
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
   └─ Ports/<port_id>/Devices/<device_id>
      └─ DeviceAddress / AddressKnown / DeviceName / KKSPrefix
         PointCount / RawPointCount / CalculationPointCount
         Configured / ExplicitlyBound / PresentInTopology
```

客户端先 Browse 成员树确认 source、实际串口和子站关系，再将目标 `source_id` 作为 String
写入同组的 `SelectedSourceId`。写入与业务点 Read/Subscribe 必须使用同一个 OPC Session；
切换的是该 Session 的整个逻辑设备组，不是整台下级，也不是一个 KKS。成员树没有写接口，
串口/子站映射只能通过经过 revision 校验的 Web/REST 配置修改。

`Configured` 表示设备已有效归入该组（包括继承来源默认组），`ExplicitlyBound` 只表示存在人工
绑定；`PresentInTopology` 表示设备存在于最后一份已验证的拓扑快照，来源是否在线另看
`ConnectionState`。未发现的配置占位设备用 `AddressKnown=false`，其 `DeviceAddress` 返回
`BadWaitingForInitialData`，不会把未知地址伪装成合法的站址 0。`Selectable` 只表示允许选中；
若 `CompleteCoverage=false`，选中后缺失点仍按协议返回 `BadConfigurationError`。

物理树热更新采用稳定 NodeId 的差异化增删：名称、Endpoint、点数等变化只更新原变量读取器，
不会删除再创建未变化节点，因此已有监控项保持有效；真正被配置删除的分支才会移除。

## 命令语义

共享的旧 `_Send` / `_SendCommand` 节点不能按其默认组直接发送。先从 hex payload
首字节解析设备地址，结合公开端口及拓扑确定逻辑组，再固定 Session 选中的 source、
实际串口和下级命令 NodeId。不可重写原始协议字节；目标地址必须与选中设备匹配。
无法唯一确定设备、端口、选源或成员关系时返回 `BadConfigurationError`，不派发命令。
无新拓扑/无设备 bindings 的旧配置仍按原单组规则处理。
pending 保持 `source_id + physical port`，同一物理串口跨设备仍互斥；选源锁针对本次命中的逻辑组。
`_Send` 写入保持 pending 至设备 ACK 或超时；ACK 绑定已固定的 source/port/epoch，
Session 关闭不重定向正在执行的命令。`SendCommand` 保留原下级的同步 RPC 语义，
锁在 RPC 返回/超时后释放；其 `Received` / `1` 仅表示代理调用完成，不代表设备执行成功。
需要跟踪设备实际回执时使用 `_Send` 写入链路。
旧共享 `_Send` 读取优先显示该 Session 最近一次该节点命令的结果，按 command ID
独立保存 `2`（等待）与 `1` / `3`（成功 / 失败）；其他 Session 或同串口另一设备的
后续 ACK 不得覆盖它。历史仅在进程内保存，Session 关闭后清除。原本 Good 的历史会叠加
当前离线/退役等坏质量；已失败的命令历史保留原失败质量，可同时查看来源状态获知当前连接情况。
无历史且端口内设备选源分裂时返回坏质量，不能任意展示某个下级的 ACK。
新增管理命名空间的 `Groups/<group>/Send` 与 `SendCommand` 可直接指定逻辑组；
它们也通过 payload 地址验证唯一设备/端口，保持相同的物理端口互斥及原始命令协议。

## 验证要求

所有构建和测试在远端 `192.168.10.46` 完成。使用独立源码/运行/数据/日志目录和端口，
不使两个采集实例同时打开现有真实串口。新增 Rust PTY 下级用于两设备、两来源互操作。
验证旧黄金点表、拓扑发现、命名空间 URI 解析、双 Session 独立选源、缺点/离线/缓存、
同值切源通知、不同下级串口名、共享串口命令互斥/ACK、配置迁移、API 和 Web 流程。
保留测试日志与截图；24 小时稳定性和目标 Windows 服务验收必须单独明确执行状态。
本次已执行结果与未覆盖项见 [远端验证记录](device-groups-validation.md)。
