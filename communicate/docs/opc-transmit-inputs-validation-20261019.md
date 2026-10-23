# OPC 客户端发送输入：2026-10-19 验证

本文记录部署前的隔离软件测试。随后已完成 46 串口停用接入，现场软件验收与未覆盖范围见[46 部署记录](deployment-46-20261019.md)。

## 本轮实现

- 客户端经管理 OPC 写 `RcdSync.<port>.TX.*`，转发到选定来源和连接 epoch；不走 `_Send` ACK/重试逻辑，不创建逐点高频命令审计。
- Rust 每字段记录实际写入时刻，同值也更新；首次全部写齐才组包，任一字段超时则不发送。发送副本与后续写入隔离。
- 1→9 有 179 个可写输入，9→1 有 58 个。身份字段由布局生成；数据类型、原始块精确长度、字节范围均验证。
- `input_refresh_interval_ms=5000`、`snapshot_max_age_ms=15000` 可在配置中修改；网页参数表兼容旧配置省略默认项，读取表单不会改文件。
- OPC 公开刷新要求、超时和发送状态；维护/关闭时拒绝写入，批量不同字段可并发，诊断点不参与来源差异告警。

## 环境与证据

所有构建、格式化、可执行测试在 **192.168.10.46** 的独立目录：

`/home/lenovo/code/rcd_sync_inputs_20261019_Btmitl`

本机仅编辑、读差异及传输。使用未提交工作树源码，不标记为正式发布版本。没有重启、部署或修改任何现场服务/配置；没有打开真实串口、发送硬件命令。测试使用临时回环 OPC 端口和独立 PKI、内存数据库；Rust `unit_fixture` 只提供内存输入/样本，不启动串口 worker。

## 已完成检查

| 检查 | 结果/证据 |
|---|---|
| Rust 全目标测试 | 42 项通过；`rust-final-source.log` |
| Rust clippy `-D warnings` | 通过；`clippy-final-source.log` |
| 共享配置表单测试 | 5 项通过；`form-tests.log` |
| 管理端选源固定、同值转发、异常不重投、维护拒绝 | 通过；`wire-final.log`、`go-race.log` |
| 旧命令、Session、冗余相关回归 | 通过；`go-regression.log` |
| OPC Session 隔离/业务节点回归 | 通过；`go-opc.log` |
| None 与 Basic256Sha256/SignAndEncrypt | 263 点浏览；接收只读、发送可写、输入校验、Read/Subscribe 通过；`opc-final/report.json` |

全链路测试 `TestSynchronizationInputsThroughManagerToRust` 启动真实 Go 管理 OPC、生产下级连接监督器及 Rust OPC 栈，由 gopcua 客户端连接管理端写入。验证：178/179 输入不就绪，写齐后就绪；变化值读回；5 秒后同值写刷新时间；其他字段未刷新 15 秒后整体失效；批量 179 项写入恢复；没有 ACK pending 残留。`wire-final.log` 首次完整通过。最终源码的 **完整链路及三个业务用例 race 检查通过**，见 `wire-race-acceptance.log`（完整链路 36.31 秒，测试集 38.764 秒，无数据竞争报告）。

此前失败记录保留：`wire-test.log` 为夹具启动前连接导致发现超时；`wire-test-diagnostic.log` 为测试客户端未按服务端端点选择匿名 token。测试已增加等待夹具监听及标准端点选择，不修改生产认证策略绕过。

`wire-race-final.log` 首次写入时连接尚未就绪；`wire-race-ready.log` 夹具未在 10 秒内监听。已增加订阅就绪检查、启动诊断及独立 30 秒首次启动预算；测试夹具可限定为 90 秒运行，生产参数未改变。

## 命令

```sh
# 隔离 source/communicate
cargo test --locked --all-targets
cargo clippy --locked --all-targets -- -D warnings
cargo test --manifest-path ../crates/rcd-config-form/Cargo.toml

# 隔离 source/manager，RCD_SYNC_FIXTURE 指向隔离构建的 unit_fixture
go test -race ./internal/app -run '^TestSynchronizationInputs' -v -count=1
go test ./internal/app -run 'Test(RuntimeWrite|RuntimePassive|ConsecutiveCommand|NoFeedback|RuntimeSession|RuntimeDefault|SourceStatusRefresh|.*Redundancy)' -count=1
go test ./internal/opc -run 'Test(UpstreamServerReturnsDifferent|.*Business)' -count=1
```

## 未覆盖/使用边界

- 未部署到现场，也未进行真实对端、串口时序或过夜验收。软件夹具不能代替现场双向通信。
- 端到端实际 OPC 写入验证 1→9 的 179 个输入；9→1 的 58 个输入覆盖于 Rust 组包/字节完整性测试，不冒充两种角色均已现场验收。
- 每字段最新值组包，不承诺跨字段 OPC Write 的事务原子性。多客户端同时写同一来源以最后成功值为准，建议单生产者；切源后重新写齐新来源。
- 5 秒是服务端公布的刷新要求，外部客户端必须自行按该周期写入；管理端不会用缓存自动续期。任一输入超时停止整包发送，不只看最后一条写请求。
- HW/GMM 等未定义块仍需客户端按文档提供原始字段；本轮未猜测内部业务位、单位、倍率。OPC 写成功与串口写出、对端确认不能等同。
