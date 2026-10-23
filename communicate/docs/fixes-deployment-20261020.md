# 通信程序四项修复与46部署验收（2026-10-20）

## 结论和范围

按本次授权修复四项问题，全部构建及可执行测试在46隔离目录完成。北京时间10:17:41将独立通信服务升级到 **0.1.1**；10:21核对时服务正常。只重启 `rcd-communicate.service`，没有重启管理端、原采集、Agent或Semaphore，没有修改30/58。

串口、接收任务及发送继续停用；现场配置和点表摘要与部署前一致。没有真实串口收发、硬件控制命令、现场发送值写入或真实角色切换。本轮不能作为物理通讯和一小时/过夜稳定性验收。

## 修复

1. `write_once` 遇到10ms分片的 `TimedOut` / `WouldBlock`，继续处理未写出的后缀，直到总期限或取消；不重发已接受的前缀，不把硬错误当可重试错误。
2. Operations v1增加虚拟文件 `configuration-set.toml`，通过已有管理网页的文件下拉即可使用。主配置与CSV在同一草稿中共同校验、共同CAS；备份为完整配置集。持久事务覆盖两次物理替换，启动在取得运行锁后、开启OPC前恢复。外部编辑冲突、坏事务均拒绝继续，保留记录。单文件编辑仍兼容。
3. 同步写入失败立即更新 `Disconnected` 和实际错误；worker释放句柄后标记 `Reconnecting`，等待期间不再显示 `Open`。
4. 同步连接收到并成功发布有效数据后，下一次重连等待复位到最小值。仅成功打开串口、收到噪声或坏帧不会清零退避。

成套配置是联合视图，不新增第三份生效配置。保存回执保留提交文本，满足既有Go管理端严格回执校验；不必更新管理端二进制。字段说明和使用步骤见[通信程序说明](../README.md)。

## 远端测试

证据目录：`/home/lenovo/code/rcd_communicate_fixes_20261020_nR4LEi`。

| 检查 | 本轮结果 |
| --- | --- |
| `cargo test --locked -j2` | 49项通过，`rust-accepted.log` |
| `cargo clippy --locked --all-targets -j2 -- -D warnings` | 通过，`clippy-accepted.log` |
| `cargo build --locked --release -j2 --bin rcd_communicate` | 通过，`release-accepted.log` |
| `RCD_COMMUNICATE_BINARY=<本轮release> go test -race ./internal/opc -run '^TestCommunicateConfigurationSetInterop$' -count=1 -v` | 真实Rust进程/OPC与现有Go结果校验器通过；列举、成套校验、角色切换、重复revision拒绝、备份读取及恢复，`go-interop-accepted.log` |
| 启动恢复 | 中断于未替换、单文件替换、双文件替换阶段的恢复；外部修改拒绝；check-config不恢复；实例锁占用不得提前恢复 |
| 串口软件回归 | 临时超时恢复、部分写后取消/总超时、硬失败不重发、断开诊断、有效接收复位退避、无有效接收继续退避 |
| 部署后真实OPC | 直连4842和管理端4840 Browse/Read/Subscribe通过；263个新节点，管理端5560个；`opc-live-accepted.json` |
| 部署后真实网页 | 1366×768、1920×1080下读取成套配置并点击校验通过；JS错误0、配置保存0、硬件写入0；`web-acceptance/result.json`及截图 |
| 软件健康 | HTTP healthz/readyz正常，`health-after-deploy.json`、`ready-after-deploy.json` |

测试固定使用临时目录、回环端口及停用串口。角色切换/保存/恢复测试只修改隔离配置，不修改现场配置。Go测试只验证所述链路，不声称全管理端重新做了race或整套回归。

保留失败记录：首轮发现无效配置启动创建state目录的回归，已恢复预检；新增启动集成用例一次15秒内未就绪，补充日志，单独复核通过，并把首次证书/地址空间初始化预算与串口期限分开设为45秒。OPC集成首轮客户端未选择服务端匿名令牌策略，已修正；第二轮发现联合视图重新排版的回执不符合管理端原文比较，修复并通过最终验收。未放宽管理端校验或认证。一次调用旧探针二进制使用了错误参数，探针在usage阶段退出；随后从已审阅的只读探针源码执行通过。失败均保留，不用失败输出充当通过证据。

## 部署、配置与身份

- 安装位置 `/opt/rcd/bin/rcd_communicate`；用户级服务 `rcd-communicate.service`。
- 发布与运行SHA-256一致：`8c9af39d17b63f8d721269f8ff649e83035c5c31601ec0e3db80f8535e2cbf52`。
- 原二进制SHA-256：`a4a8fbd7d56a4c5cdc63ec82fe6a14cf57210c3e8b23848b072004b5ee752661`。
- 回退证据：上述证据目录下 `deployment-backup/`，保存原程序、config/state/PKI及 `deployment.json`，权限受限；没有执行回退。不能用旧state或旧配置覆盖使用人后续修改。
- 配置SHA-256未变化：`config.toml = 546f46dc249e72661ff0d4c3c5376ef28672d2d47be41e691249cfeeddb0d819`；`points.csv = e96c151cda76429027b6ccb2c374552abf689a3f3bfc229f013884a944a7ecd2`。
- 新通信PID从2992721变为35394，`NRestarts=0`；管理端2997559、原采集2777575、Agent3333828均未变。
- 进程设备句柄只有 `/dev/null`，未打开串口。OPC发送状态 `transmit_enabled=false`、`complete_writes=0`、`received_inputs=0`；接收及发送输入为预期 `BadOutOfService`。刷新5000ms、超时15000ms不变。
- 源码基于当前脏工作树，仅本轮相关源码经远端格式化后同步；`source-files.sha256`记录远端构建输入。没有提交或推送。

本程序还不是Semaphore三组件发布器支持的组件，本次使用受控的独立停用服务更新工具，不冒称经平台发布。工具取得现有宿主机发布锁，检查平台未结束事务、固定服务绑定、停用配置、产物SHA并备份，只替换新通信服务。没有改变既有管理端4840、下级4841、Web8087或新通信4842。

## 日志说明

`service-after-deploy.log`显示一次正常停止/启动，没有进程崩溃或自动重启。启动同秒有一条建通道阶段 `BadCommunicationError`，与本次部署工具连接TCP后未做OPC握手即关闭的就绪探针吻合；之后真实OPC读、订阅及网页调用通过，所查区间没有反复出现。部署工具随后改为被动核对监听端口归属，避免未来产生此类探针日志；未因此再次重启服务。管理端所查区间没有匹配到panic/fatal/error/reconnect日志。上述为本轮短时验收，不是长期无故障保证。
