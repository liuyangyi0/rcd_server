# 下级 OPC 运维协议与操作边界

管理程序通过现有 OPC UA 连接管理**指定物理下级**的配置、串口采集和日志，不使用 SSH，不在 Rust 下级中增加 HTTP 端口，也不沿 Session 的分组选源路由操作。旧下级没有本协议时继续正常采集，Web 显示“未提供运维能力”。原有端点、安全策略、业务 NodeId 和拓扑命名空间不变。

新增的 Linux 进程 start/stop/restart 使用**另一个独立运行的 agent HTTP 接口**，不扩充下面的 OPC action 或 Capabilities；下级停止后仍可经 agent 启动。部署、版本和结果未知处理见 [下级进程管理](lower-process-management.md)，未绑定 agent 时不能远程启停进程。

## 首版语义

- 串口关闭：在 worker 安全边界停止该口所有站点的采集并释放串口句柄；OPC 服务继续运行。再次开启会尝试打开串口，开启配置不等同于硬件通信正常。
- 站点停读：独立于原有整站禁用，仅停止该站自动查询/被动采集的数据发布，其他站点及允许的控制命令不受影响。主、备机模式均执行门禁；停读期间输出坏质量。
- 采集开关持久化到下级 `config/operations.json`。分别展示期望值、worker 已应用值、物理句柄状态和通信状态。异步执行未确认时显示 `Pending`，不能报告已生效。
- 配置管理限于下级配置目录的 `config.toml`、`calc.toml`、`rcd*.csv`。保存前进行全配置预检和版本比较，自动备份并原子替换文件。CSV/计算规则及全局配置**重启下级后才生效**，本协议不自动重启进程。
- 备份先载入编辑草稿，再由用户显式校验、保存恢复。查看备份不修改当前文件。
- 日志正文只读；按文件代次和字节游标分页，最多返回 64 KiB 文本。筛选针对当前扫描页，不声称检索了整个历史日志。下载按钮下载当前显示页。日志等级可通过专用动作修改配置，但只在明确重启下级后生效。

## OPC UA

在现有命名空间末尾追加 `urn:SerialServer:operations`；管理客户端必须按 URI 解析索引。

| NodeId（String） | 类型 | 含义 |
|---|---|---|
| `Operations` | Object | 运维方法所属对象 |
| `Operations/Capabilities` | String，只读 | JSON 能力描述 |
| `Operations/Execute` | Method | `(Request String) -> (Response String)` |

能力描述：

```json
{"schema_version":1,"features":["acquisition","configuration","logs","station_versions","calculation_mode","logging_configuration"],"max_config_bytes":2097152,"max_log_bytes":65536,"configuration_apply":"restart"}
```

请求为有界 JSON 对象。业务错误放在正常方法结果中，不将文件不存在、版本冲突或日志轮转误报为 OPC 通道断开：

```json
{"action":"set_device_read_enabled","port_id":"COM1","device_id":"1","enabled":false,"expected_revision":"3"}
```

```json
{"ok":true,"result":{"status":"Applied","revision":"4","ports":[]}}
```

```json
{"ok":false,"error":{"code":"conflict","message":"配置版本已变化，请刷新后重试","current_revision":"4"}}
```

`expected_revision` 是不透明 String：采集状态使用下级持久化版本；配置文件使用内容 SHA256；两者都不同于管理程序自身的来源配置 revision。写操作不得在传输失败后自动重试，应刷新状态核对实际结果。

### 动作与结果

| action | 参数 | result |
|---|---|---|
| `status` | 无 | `schema_version, revision, ports` |
| `set_port_enabled` | `port_id, enabled, expected_revision` | 状态快照及 `status: Applied/Pending` |
| `set_device_read_enabled` | 上述字段及 `device_id` | 同上 |
| `list_configs` | 无 | `{items: ConfigFile[]}` |
| `read_config` | `name, backup_id?` | `ConfigDocument` |
| `validate_config` | `name, content` | `{valid, errors: string[], warnings: string[], restart_required}` |
| `write_config` | `name, content, expected_revision` | 保存后的 `ConfigDocument` |
| `list_logs` | 无 | `{items: LogFile[]}` |
| `read_log` | `name, generation?, offset?, limit?, query?, level?, tail?` | `LogChunk` |
| `get_logging` | 无 | 当前运行/已保存等级、配置 revision、HTTP 推送状态 |
| `set_logging_level` | `log_level, expected_revision` | 保存后的等级状态及 `restart_required` |

状态中的端口包含 `port_id, port_name, enabled, applied_enabled, open, devices, pending_commands, control_pending`，可有 `last_control_error`；设备包含 `device_id, name, read_enabled, applied_read_enabled, communication_ok`，可追加确实可获得的最后采集时间。未获得实际通信证据时不得根据启用开关推断健康。`Applied` 表示 worker 已执行设置，不代表物理通信正常；确认失败或未完成时必须核对快照。修改结果另附 `operation_revision` 关联本次控制版本。

`ConfigFile` 为 `name, kind, size_bytes, revision, active_revision, restart_required`；`ConfigDocument` 增加 `content, backups[]`，备份项为 `id, created_at, size_bytes`。列表最多返回最近 200 份备份，超出时附 `backups_truncated:true`，不会删除旧备份。预览备份时 `revision` 仍是**当前磁盘文件版本**，附 `viewing_backup_id, backup_revision`，供恢复时比较；`active_revision` 为本次进程启动加载的版本。校验错误与警告亦有字节上限，截断时附 `errors_truncated` / `warnings_truncated` 并保留省略提示，不能因截断误报校验通过。

`LogFile` 为 `name, size_bytes, modified_at, generation`。`LogChunk` 为 `name, generation, offset, next_offset, size_bytes, eof, truncated, text`。每页 `limit` 为 4–65536 字节，以容纳完整 UTF-8 字符。代次在正常追加时保持稳定；轮转、替换或游标失效返回 `conflict`，不得混接不同代次的内容。

筛选状态下跨页续读长行会保留该行的级别/关键词匹配上下文，不把后半行当作新记录丢弃。
正文向前扫描最多 254 KiB，必要时另向前回溯最多 256 KiB 找当前行首；回溯超限明确返回
`invalid_request`，提示清除筛选再按字节查看。无筛选分页不受日志行长度限制。

管理器在转发前按动作校验下级结果的必要字段、资源名称、版本、数组、字节上限与游标范围。
无效结果返回 `invalid_response`；对于已派发的修改，审计标为结果未知，不把缺字段的响应当作成功，
也不因此重复下发修改。

错误 code 包括 `unsupported, invalid_request, not_found, conflict, busy, too_large, io_error, persistence_failed, operation_failed`。实现必须拒绝任意路径、目录、链接和超限输入；这属于资源边界，不是新增身份认证。

## 管理 Web / REST

前缀：`/api/v1/sources/{source_id}/operations`。所有操作绑定路径中的物理来源。结果附加可信的 `source_id, source_revision`；不能使用下级返回的同名字段覆盖管理程序标识。

| HTTP | 路径 | 行为 |
|---|---|---|
| GET | `/capabilities` | 能力发现；旧下级返回 `supported:false` |
| GET | `/status` | 实时运维状态 |
| PUT | `/port` | 串口开关 |
| PUT | `/device-read` | 站点读取开关 |
| GET | `/configs` | 配置列表 |
| GET | `/config?name=…&backup_id=…` | 当前配置或备份草稿 |
| POST | `/config/validate` | 只校验，不写文件 |
| PUT | `/config` | 保存配置 |
| GET | `/logs` | 日志文件列表 |
| GET | `/log?name=…&generation=…&offset=…&limit=…&q=…&level=…&tail=true` | 日志分页 |
| GET | `/logging` | 当前运行等级、已保存等级、覆盖和推送状态 |
| PUT | `/logging` | 保存 `error/warn/info/debug/trace` 等级 |

所有修改型 PUT 必须携带 `If-Match: "下级版本"` 和 JSON 中的 `source_revision`，以同时阻止下级内容冲突、来源 Endpoint 被改后的误操作。日志等级使用 `config.toml` 的内容 SHA256 作为下级版本；缺少版本返回 428，冲突或 busy 返回 409；离线返回 503，调用超时返回 504；写超时后结果可能已生效，前端必须提示核对而不是重复提交。

沿用现有同源检查与操作员标识，不新增登录或修改 OPC 安全设置。管理审计记录动作、目标、版本、结果和内容摘要，不把配置正文或日志正文写进审计。采集控制审计以 `operation_revision` 关联本次提交；若其他端口随后修改了配置，返回的 `snapshot_revision` 单独记录，不混用两个版本。

## 与进程管理的配合

保存/恢复配置仍只写磁盘，不自动重启。用户显式通过已绑定 agent 重启后，下级重新读取**当时磁盘上的整个配置集**；不是仅应用当前编辑文件，未保存草稿也不会被应用。带文件版本的重启预检检查所列文件是否仍是预期版本；重启后仍须确认待重启标记、OPC 连接和实际点值。

管理器按物理来源阻止进程作业与代理命令、运维修改交叉执行；结果 unknown 时保留互斥并对原作业查询/对账，不以新 request_id 自动重试。普通进程控制不依赖 OPC 在线，但带配置版本预检的重启需要下级 OPC 可用。采集开关、配置版本和 agent 进程版本互不替代。

Rust 接收 SIGTERM 与 SIGINT 后进入同一退出流程，关闭期间拒绝新命令；停止进程不承诺已经发起的外部直连命令都能取得最终设备 ACK。串口开关和站点停读的持久期望不因进程 stop/start 自动重置。

## 验证要求

所有执行验证在远端独立实例进行，不操作原来 4840 端口的 Rust 服务。至少覆盖双站隔离、主备停读门禁、端口真实释放/重新打开、开关重启保持、Pending/故障/命令互斥、配置校验失败不落盘、CAS 冲突、备份往返、重启待应用标记、日志空态/分页/追加/轮转及路径限制。Web 必须实际点击操作，核对源级作用域、草稿不丢失、错误态和窄屏布局；构建成功不等于现场验收完成。
