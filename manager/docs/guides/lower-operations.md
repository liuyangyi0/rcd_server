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

### 网页配置操作（按目标下级能力显示）

入口为“下级 OPC → 目标来源的运维 → 配置”。操作作用于所选物理来源，不跟随 Session 选源。

1. 选择下级公布的文件。`config.toml` 在支持 `configuration_forms` 时展示参数表单；可搜索中文名称或字段名，枚举使用下拉框。
2. 修改后点击“校验当前草稿”；参数表单的修改会先自动合并进草稿，再校验。不再单独点击“生成配置草稿”。参数准备、校验和刷新都不写文件。
3. 校验通过后“保存到下级”；保存前比较来源 revision 与文件 SHA-256，自动备份、原子替换。保存失败保留草稿，结果不明时先核对，不能自动重试。
4. 保存后明确重启才应用。现有 RCD 若已绑定支持安全生命周期的 Agent，可前往“进程”页操作；新 Communicate 尚未接入该能力，不能假定能从网页重启。

参数默认只显示常用项，可按类别下拉筛选或搜索全部字段；默认值和只读项单独标记。“配置原文”用于高级 TOML/CSV 编辑；“导入、下载与备份”收纳可选工具，“文件版本与放弃更改”收纳版本核对和刷新。已有草稿载入文件或备份需再次确认，不能静默覆盖。未保存的参数、文件均阻止误切换；后台刷新只更新比较信息，不覆盖草稿或升级原保存 revision。

管理端`0.6.13-ui.20261022.1`起，网页内部跳转或浏览器后退后，可重新打开同一来源、同一来源配置版本的运维面板，继续原文件和参数草稿。草稿只在当前网页登录的内存中保留，不写浏览器磁盘；刷新/关闭浏览器前仍须保存或导出。成功保存、明确放弃或退出登录会清除对应缓存。恢复草稿仍使用编辑开始时的文件版本；远端文件已变化时不能直接保存，需显式载入新文件并合并，不会换成新revision强行覆盖。

参数定义的后台读取不锁定原文输入；显式校验、保存等操作进入忙碌状态。准备失败或内容超限时保留参数修改，不继续校验或保存。

采集程序的重点参数：

| 参数 | 意义 |
| --- | --- |
| `settings.command_retry_count` | **最大总发送次数，包含首次**。3 不是再重试 3 次；收到有效 ACK 后仍立即结束 |
| `settings.command_interval_ms` | 同一串口不同控制事务之间的最小间隔，0–60000ms；0 不追加等待，保存后重启生效，详见下文 |
| `settings.query_retry_count` | 普通轮询失败补发次数，0 或 1 |
| `settings.timeout` | 串口读取超时，毫秒 |
| `settings.poll_interval_ms` | 轮询间隔，毫秒 |
| `settings.parity_switch_delay_ms` / `transmit_settle_guard_ms` | 校验位切换保护 / 发送完成保护，毫秒 |

原文件缺少受支持且带默认值的 `settings` 项时，页面标明“程序默认值”，仅修改的项会写入。其它已有标量也可编辑；复杂数组、未展示的可选项仍使用原文。全局波特率要与各串口 CSV 核对；预留但未生效的流控字段只读，避免误认为配置后有效。本功能不会主动替换现场的 5 ms 等已有值。

下级 `calculation.mode = "manager"` 时，未使用的本地 `calc.toml` 不阻止保存串口参数；切回 `local` 必须重新通过计算规则校验。

采集点表格式见[CSV 填写与校验规则](acquisition-csv-rules.md)。点地址和位范围越界保留具体提示，但不阻止保存；程序配置语法、类型和范围，以及 CSV 结构、通信头部、重复点或设备冲突仍阻止保存。软件不自动改长度或点地址，由配置编写人员修正。新保存规则及页面简化已随采集和管理端 `0.6.16-config.20261023.1` 发布30、58、46，见[本次记录](../validation/configuration-workflow-deployment-20261023.md)；旧版下级仍需升级，不能只刷新管理网页。

新 Communicate 只管理启动时绑定的 `config.toml`、`points.csv`。支持参数表单及相同保存规则，但不冒充采集开关、日志或 Agent 能力；身份与点表路径绑定不能远程修改，未完成协议不得启用。配置专用下级进入抽屉默认显示配置页，不请求它不支持的采集状态接口。

表单需要本轮新版下级与管理端配合；旧下级仍可使用已支持的原文配置功能。新 Communicate 的配置方法沿用已获确认的匿名 OPC 运维边界，只限受信任内网；网页登录并不保护绕过 Web 的 OPC 直连。

### 能力与方法

在现有命名空间末尾追加 `urn:SerialServer:operations`；管理客户端必须按 URI 解析索引。

| NodeId（String） | 类型 | 含义 |
|---|---|---|
| `Operations` | Object | 运维方法所属对象 |
| `Operations/Capabilities` | String，只读 | JSON 能力描述 |
| `Operations/Execute` | Method | `(Request String) -> (Response String)` |

能力描述：

```json
{"schema_version":1,"features":["acquisition","configuration","configuration_forms","logs","station_versions","calculation_mode","logging_configuration"],"max_config_bytes":2097152,"max_log_bytes":65536,"configuration_apply":"restart"}
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
| `prepare_config` | `name=config.toml, content, edits[]` | `{content, fields[]}`；仅准备草稿，不校验整套配置、不落盘 |
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
| POST | `/config/prepare` | 参数定义或草稿生成；必须携带 `source_revision`，不写文件 |
| PUT | `/config` | 保存配置 |
| GET | `/logs` | 日志文件列表 |
| GET | `/log?name=…&generation=…&offset=…&limit=…&q=…&level=…&tail=true` | 日志分页 |
| GET | `/logging` | 当前运行等级、已保存等级、覆盖和推送状态 |
| PUT | `/logging` | 保存 `error/warn/info/debug/trace` 等级 |

所有修改型 PUT 必须携带 `If-Match: "下级版本"` 和 JSON 中的 `source_revision`，以同时阻止下级内容冲突、来源 Endpoint 被改后的误操作。日志等级使用 `config.toml` 的内容 SHA256 作为下级版本；缺少版本返回 428，冲突或 busy 返回 409；离线返回 503，调用超时返回 504；写超时后结果可能已生效，前端必须提示核对而不是重复提交。

`prepare_config` 的 `edits` 为 `{path:["settings","command_retry_count"],value:5}` 数组；表数组索引使用从 0 起的整数，非展示用的点分隔字符串。空数组只返回参数定义。响应字段提供类型、标签、提示、选项、默认值与只读标记；管理端验证响应类型和路径再传给 Web。不信任来自下级的操作者、来源身份或文件路径。

沿用现有同源检查与操作员标识，不新增登录或修改 OPC 安全设置。管理审计记录动作、目标、版本、结果和内容摘要，不把配置正文或日志正文写进审计。采集控制审计以 `operation_revision` 关联本次提交；若其他端口随后修改了配置，返回的 `snapshot_revision` 单独记录，不混用两个版本。

## 控制命令最小发送间隔

新版采集程序提供 `settings.command_interval_ms`。入口为“下级 OPC → 目标来源的运维 → 配置 → config.toml → 参数设置”，搜索“控制命令最小间隔”。例如在已有配置的 `[settings]` 中增加：

```toml
command_interval_ms = 500 # 示例：同一串口上一条控制命令结束后，至少等500毫秒再开始下一条
```

- 范围为 **0–60000 毫秒的整数**；缺省为 0，表示不追加等待，保持原有行为。示例值并未自动写入现场，应按设备处理能力选择。
- 每台下级配置一个值，各物理串口分别计时；不是所有串口共用一把计时锁，也不是每站单独配置。来自多台管理端、旧 OPC 入口和 TCP 入口的控制命令最终经过同一个串口执行线程。
- 等待中的命令保留原载荷、FIFO 位置和维护占用，普通采集轮询、备机监听及满足准入条件的版本查询继续运行；不同串口不互相等待。
- 这是**不同控制命令事务之间**的间隔，不是轮询间隔，也不改变同一命令内部重试次数、重试时序或 ACK 判断。收到有效 `00 50 50` 仍立即结束该条重试。
- 等待计入命令原有期限，不能通过配置间隔延长期限。过期命令明确失败且不再发送；管理端执行结果不明时仍不自动重投。间隔过长或积压过多可能导致未发送就超时，不能把队列当成无限期任务表。
- 例如设置 500ms，并不表示每 500ms 一定发送一条：还要等待上一条交换、轮询安全点和实际排队。管理端队列默认总期限为 30 秒，关联下发还受当前短调用预算约束（通常最多 5 秒）；不要把允许填写的 60000ms 当成现有调用可以等待 60 秒。已进入串口的报文不能撤回，过期保护会阻止尚未开始的发送和后续重试，不保证远端设备没有执行已发出的命令。
- 点击“校验 → 保存”只更新磁盘；需要明确重启目标下级后生效。必须更新采集程序，**仅升级管理端不够**。旧下级尚未提供该参数时，不应仅凭手动添加字段就认为运行中已经生效。
- 该参数不用于机组同步通信程序的周期整包发送；后者仍使用自己的刷新/超时配置。

2026-10-22 曾通过运维平台将 30、58、46 的采集程序及管理端更新到 `0.6.14-command.20261022.2`，网页字段为“控制命令最小间隔（ms）”，当次三台保持默认 0ms。该发布时页面尚需“生成草稿”；本轮源码已将其合并进校验操作。不会因本说明中的示例值自动修改任何下级。[当次部署及只读验收记录](../validation/command-chain-deployment-20261022.md)。

## 与进程管理的配合

保存/恢复配置仍只写磁盘，不自动重启。用户显式通过已绑定 agent 重启后，下级重新读取**当时磁盘上的整个配置集**；不是仅应用当前编辑文件，未保存草稿也不会被应用。带文件版本的重启预检检查所列文件是否仍是预期版本；重启后仍须确认待重启标记、OPC 连接和实际点值。

管理器按物理来源阻止进程作业与代理命令、运维修改交叉执行；结果 unknown 时保留互斥并对原作业查询/对账，不以新 request_id 自动重试。普通进程控制不依赖 OPC 在线，但带配置版本预检的重启需要下级 OPC 可用。采集开关、配置版本和 agent 进程版本互不替代。

Rust 接收 SIGTERM 与 SIGINT 后进入同一退出流程，关闭期间拒绝新命令；停止进程不承诺已经发起的外部直连命令都能取得最终设备 ACK。串口开关和站点停读的持久期望不因进程 stop/start 自动重置。

## 验证要求

所有执行验证在远端独立实例进行，不操作原来 4840 端口的 Rust 服务。至少覆盖双站隔离、主备停读门禁、端口真实释放/重新打开、开关重启保持、Pending/故障/命令互斥、配置校验失败不落盘、CAS 冲突、备份往返、重启待应用标记、日志空态/分页/追加/轮转及路径限制。Web 必须实际点击操作，核对源级作用域、草稿不丢失、错误态和窄屏布局；构建成功不等于现场验收完成。
