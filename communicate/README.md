# RCD Communicate（机组间串口数据同步）

将原 `rcd_server_communicate/client` 的帧编解码与 `server` 的串口读写合为一个 Rust 程序。运行时不再需要 TCP 中转、DLL 或两个常驻进程。原目录不删除、不覆盖；现有 RCD 的业务 OPC、串口协议及现场配置不改变。配置管理现已接入管理网页，见下文。

已按用户提供的报文分析表实现 **1→9（1156 字节）和 9→1（105 字节）数据同步**，不是查询/ACK 轮询。连续接收、文档字段解析和错峰周期发送共用每串口唯一 worker；接收点只读，本方发送值由 OPC 客户端经管理端写入。新示例仍全部停用；没有本方有效快照时不发送。9→2 完整布局、外部客户端变量映射和物理串口参数尚待现场确认，不能宣称已完成现场接入。详见[机组同步使用说明](docs/unit-synchronization.md)。

## 1. 结构与责任

```text
受控配置 + 点表
    ↓
每串口唯一 worker → 本方完整快照 → 错峰发送 → 串口
    ↑                                       ↓
持续接收/超时清零 ← 文档字段解析 ← 完整帧校验和解压
                         ↓
               按任务原子更新内存点位缓存
                         ↓
              OPC UA Read / Subscribe（只读）
```

| 模块 | 职责 |
| --- | --- |
| `config` | 严格配置、CSV 点表、唯一标识、大小及数值范围检查 |
| `frame` | 原程序的三段 LE UInt16 长度、zlib、CRC32 帧；分片/连续帧、解压上限 |
| `serial` | 独占串口、限时读写、部分写入不从头重发、迟到帧隔离 |
| `poller` | 单串口串行、多串口独立、无积压的公平轮询和重连退避 |
| `protocol` | 查询型与主动同步型协议注册，禁止把同步报文当查询 |
| `unit_report` | 文档字段布局、双向编码、报警状态及原始值保留 |
| `synchronization` | 连续接收、错峰让路、完整快照 CAS、过期停止发送 |
| `points` | 类型检查、整批更新、值/质量/源时间及串口诊断 |
| `opc` | 只读接收点、可写同步发送输入及独立诊断节点；不借用采集控制命令 |
| `operations` | 配置专用 Operations v1；读取、生成参数草稿、联合校验、CAS 保存和自动备份 |
| `runtime` | 启停、任务监督；worker 异常退出不留下貌似正常的 OPC 服务 |

沿用本仓库固定的 `async-opcua 0.19.0` 和本地服务端补丁、`serialport-rs` 补丁。另给共享 OPC fork 增加非默认特性 `preserve-source-timestamps`，仅本模块启用，修复延后通知篡改源时间的问题；原 RCD 默认构建路径不变。独立 `Cargo.lock` 与主程序分开；构建时需要保留仓库相对目录，不单独复制本文件夹后丢失依赖。

## 2. 配置与启动

参考 [1号侧配置](examples/unit1-receiver.toml) / [点表](examples/unit1-receiver.csv)，或 [9号侧配置](examples/unit9-receiver.toml) / [点表](examples/unit9-receiver.csv)。旧 `config.toml` 仍是停用的 pending 示例。已实现协议的点表须与文档布局完全匹配；不猜测位定义、倍率或设备地址。

全部构建及可执行测试在 46 的隔离目录执行，不在本机运行，不占用现有服务端口或串口：

```sh
cd <隔离源码目录>/communicate
cargo build --locked
cargo test --locked --all-targets
cargo clippy --locked --all-targets -- -D warnings

# 校验不创建运行目录，也不访问串口。
target/debug/rcd_communicate check-config --config examples/config.toml

# 仅启动回环 OPC；所有串口/任务停用。
target/debug/rcd_communicate run --config examples/config.toml \
  --state-dir <隔离目录>/state --pki-dir <隔离目录>/pki --log-dir <隔离目录>/logs
```

点表相对路径以配置文件所在目录为基准；命令行目录相对路径以工作目录为基准。部署时应全部使用绝对路径。同一 state 目录使用独占运行锁；每个实例需独立 state/PKI/logs/OPC 端口，不能用换目录方式争抢串口。SIGTERM/Ctrl+C 取消采集并关闭 OPC。日志每份 10 MiB，保留 10 份轮转文件；`RUST_LOG` 可调整等级。

示例串口 `115200、8E2` 仅取自旧程序默认值，不代表现场参数；设备路径是不存在的占位路径。超时、查询周期和隔离间隔也只是可校验的示例。轮询间隔指**一次交换结束后到下一次该任务可调度的最短时间**，不是固定频率保证；忙时不补发积压查询。

## 3. OPC 数据与接入边界

示例地址为 `opc.tcp://127.0.0.1:14842/`，不修改现有管理 `4840`、下级 `4841`、Web `8087`。当前提供可配置的 None/Anonymous 和 Basic256Sha256/SignAndEncrypt/Anonymous，首次运行在指定 PKI 目录生成证书。示例自动信任客户端证书，仅适用于隔离测试；现场证书信任策略另行配置。

业务命名空间 URI 为 `urn:SerialServer`，**客户端按 URI 解析 namespace index，不写死 ns 数字**：

```text
Objects/Devices/<原始变量名>     NodeId = ns=<业务索引>;s=<原始变量名>
Objects/Methods          不提供硬件控制 Method
```

保持 String NodeId、标量类型和 HasComponent 引用。收发变量逐字沿用 FromUnit 文档，如 `DATA.Buildings.91UBH.Fire`、`DATA.PumpStationCtrl[0]`；不添加任务/端口/方向前缀。楼宇Fire统一为UInt32状态编号，接收解码和发送编码共用1–8，0为仅接收的未知值。接收点只读，发送输入可写；未定义结构的块按原名提供定长十六进制String。完整客户端接口和状态对照见[OPC客户端接入说明](docs/opc-client-guide.md)。

OPC 采样下限为 100 ms（与当前库的内存采样器一致），发布下限 50 ms；这与串口查询周期是不同参数。单订阅容量随点表增加，单次创建监控项仍按最多 1000 个分批；Browse 超过一页须使用 BrowseNext。25,000 点是配置上限，不代表已通过该规模的性能验收。

| 场景 | 值与 OPC 质量 |
| --- | --- |
| 适配器有效响应 | 发布类型匹配的值及适配器质量，源时间为完整响应解析后的接收时间 |
| 已启用，未收到首值 | 类型零值、`BadWaitingForInitialData` |
| 端口打不开/断开/响应超时 | 类型零值、`BadNoCommunication` |
| 解析或点表不匹配 | 类型零值、`BadConfigurationError` |
| 停用/退出 | 类型零值、`BadOutOfService` |

失败保留最后一次有效响应的 SourceTimestamp，不把失败发生时间伪装成新数据；没有有效响应时源时间为空。ServerTimestamp 是管理该响应的本程序读取时间。值不变但质量变化也需订阅通知。整批发布互斥、不会留下半批缓存，但多点 OPC Read 不承诺跨请求的事务快照。

独立诊断命名空间 `urn:RcdCommunicate:diagnostics` 提供 `Diagnostics/SoftwareVersion`、`InstanceId`、`ProtocolReady`、`AcquisitionState`、点位数量及 `Diagnostics/Ports/<id>/...`。诊断节点 Good 表示状态可读，不表示设备正常。`LastError` 是最近错误，不一定仍在发生；`Failures` 目前统计任务质量失效次数（包括打开失败、退出），不是硬件故障数。

## 4. 在 OPC 管理网页配置

本程序追加 `urn:SerialServer:operations`，声明 `configuration`、`configuration_forms` 和 `configuration_set`。沿用现有 `(Request String) -> (Response String)` 方法，不新增 HTTP 服务。接收点仍只读，发送输入可写；配置方法可以写文件，**匿名直连 OPC 客户端也可能调用，只允许用于已确认的受信任隔离内网**，管理网页登录不能替代 OPC 访问控制。

连接此来源后，打开“运维 → 配置”，可以编辑 `config.toml` 和 `points.csv`：

1. 参数表单修改串口、任务周期、超时、重连及 OPC 配置，或者展开原文编辑 TOML/CSV；也可从本地文件载入草稿。
2. 点击“生成配置草稿 → 校验当前草稿 → 保存到下级”。注释保留，旧文件自动备份；并发修改返回冲突，不覆盖他人修改。
3. 保存不改变正在运行的任务。必须另行明确重启后应用；页面分别显示磁盘版本与启动时加载版本。

`schema_version`、`instance_id`、`points_file` 不允许远程更换，文件只限启动时绑定的两个文件，不接受任意路径。TOML 最大 1 MiB；网页配置传输上限 2 MiB（超过此值的点表暂不支持网页编辑，启动加载上限仍为 8 MiB）。TOML 和点表联合校验；真实适配器未实现前不能把 `pending` 任务启用。

参数表单不自动增删串口/任务；数组、任务结构和点表用原文编辑。Agent 安全生命周期尚未接入本程序，不能把“配置保存成功”当作已经具备网页安全重启能力。细节见[下级配置操作说明](../manager/docs/guides/lower-operations.md)及[配置功能验证](../manager/docs/validation/lower-web-configuration-20261015.md)。

### 同时修改角色、任务与点表

在文件下拉框选择 **`configuration-set.toml · 成套配置（主配置＋点表）`**。这是两个文件的联合视图，不是第三份运行配置；无需修改管理端即可使用。

1. 编辑 `config_toml` 多行字符串中的主配置，以及 `points_csv` 多行字符串中的完整 CSV（也可下载、编辑后载入草稿）。保留外层字符串引号。
2. 点击“校验当前草稿”。角色对应的点数、名称和类型按新主配置与新点表一起校验，不再被旧的另一份文件阻挡。
3. 点击“保存到下级”。任一文件被其他人修改都会返回版本冲突；旧的完整配置集自动备份。保存不重启、不自动启用串口。
4. 需要恢复时在同一文件的备份下拉中载入旧配置集，校验后显式保存；恢复保留当前实例 ID 和文件绑定限制。

联合文本上限 2 MiB。普通单文件编辑仍保留，但角色、任务 ID 或点表结构一起变化时应使用成套配置。保存采用持久事务：中途掉电后，下次 `run` 在打开 OPC/串口前完成原事务；发现文件被外部修改或事务损坏时拒绝启动，保留证据，不能自动覆盖使用人的修改。`check-config` 遇到未完成事务只报告错误，不执行恢复。不要手工删除 `.rcd-config-set.pending.json` 绕过恢复检查。外部工具直接读文件不具备两次文件替换之间的事务快照保证。

## 5. 现场接入边界

- 已提供的两种布局和同步调度已实现；不生成查询或硬件控制命令。9→2 只有变量名清单，没有完整布局/样例，暂未注册。
- 本方发送值由 OPC 客户端经管理端写入原名 `DATA.*` 发送输入，通信程序整合组包。默认要求 5 秒刷新，任一输入 15 秒过期即停止发送；两项均可配置。首次输入未齐不发，变化更新缓存但不抢占串口错峰时序。完整快照使用 `TransmitSnapshot`，详见客户端接入说明。
- 原 DLL 的首字节分开发送及 30 ms 延迟不自动继承：必须确认是否确属串口线上的协议要求。
- 不承诺“50 ms 一个站点”；取决于实际报文、线速、设备响应和端口负载。
- 不发布假的 Topology、采集开关或维护能力；Operations 目前仅配置管理。Agent 安全维护、发布平台新实例绑定、业务日志推送待接口补齐后验收。
- 不增加 TCP 4000 中转或兼容旧 DLL 的服务；如仍有外部程序依赖它，另行确认兼容需求。
- 2026-10-19 已在 46 以串口停用模式安装独立服务，并接入现有管理端，见[46 部署记录](docs/deployment-46-20261019.md)。真实角色及串口仍待确认，未做真实收发或过夜验收；隔离软件测试不代替现场验收。

协议接入约束及需要补充的资料见 [协议适配说明](docs/protocol-adapter.md)。软件验证记录见 [OPC 发送输入验证](docs/opc-transmit-inputs-validation-20261019.md)、[机组同步验证](docs/validation-20261019.md)及[原基础功能验证](docs/validation-20261015.md)。

2026-10-20四项修复及0.1.1停用串口部署结果见[修复与部署验收](docs/fixes-deployment-20261020.md)。

0.2.0接口使用原始文档变量名、Fire数值双向转换及`TransmitSnapshot`原子提交；2026-10-20在46以串口停用模式同步更新了通信程序、管理端和点表，详见[原名接口验证与授权部署](docs/original-variable-names-validation-20261020.md)。旧0.1.2前缀接口属于历史部署，不能只更换二进制后继续沿用旧点表。上述记录不代表真实串口收发或长期运行已验收。
