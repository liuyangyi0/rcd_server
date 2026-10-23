# 同步解析修复与整包原子提交（2026-10-20）

## 范围

修复假长度头吞帧、超时质量被后续坏帧覆盖、坏帧诊断缺失和管理端同步写入额外ACK预读。通信程序增加 `RcdSync.<port>.TX.Snapshot`，由管理端按当前Session选源转发完整JSON字段集，整体校验/CAS/替换发送缓存。原子模式拒绝逐点混写；无效提交不改变原快照和有效期。

通信程序版本0.1.2；管理端版本0.6.9-sync.20261020.1。保持既有节点、端口、串口时序、现场配置和安全策略。仅46部署，30/58及原采集、Agent、Semaphore不升级。本次用户已确认控制暂停、允许重启46管理端；新通信端口、任务和发送始终停用。

接口完整说明见[机组同步](unit-synchronization.md)。Snapshot读取为状态JSON，提交使用全部输入NodeId到值的映射，不需要客户端处理压缩和CRC。单请求最大64KiB，管理端和下级传输上限对齐；普通逐点String仍受8192字节限制。

## 隔离验证

全部构建和执行测试在46，证据根目录：

`/home/lenovo/code/rcd_sync_atomic_20261020_dnDAdd`

- `rust-accepted.log`：55项Rust测试通过，包括两方向组包、缺项/重复/未知/非法字段、revision竞争、原子模式混写拒绝、并发读取不混合、停用拒绝、过期停止，以及假头后完整帧恢复、CRC计数、超时后坏帧保持通信中断。
- `clippy-accepted.log`：`cargo clippy --locked --all-targets -j2 -- -D warnings`通过。
- `release-accepted.log`：0.1.2发布构建通过。
- `manager-release.log`：36个前端测试文件、244项测试通过；前端构建、`go test ./...`、Go发布构建通过。带独立Rust进程的测试另外显式执行，不把默认跳过计为覆盖。
- `go-sync-verified.log`：`go test -overlay=../../test-overlay.json -race ./internal/app ./internal/opc -run 'TestSynchronization' -count=2 -v`全部通过。两轮真实Go管理端→Rust OPC进程测试分别36.39秒、31.30秒，覆盖179项输入、整包原子提交、旧revision和混写拒绝、同值刷新、15秒超时与恢复；没有连接物理串口。

保留的失败及修正：完整JSON首次被上级默认4096字节限制拒绝，现已设置64KiB并通过真实转发。旧测试把Good附带Overflow（128）当失败，诊断证明实际值已为42；修正为检查严重性位，不丢弃信息位。默认Status/Value订阅不保证通知同值时间戳变化，因此同值刷新验收改为核对下级revision递增，字段有效期由Rust缓存测试验证。仅测试断言修正通过 `test-overlay.json` 应用，生产源码及已生成发布包不因此改变；修正后的测试副本和失败日志均保留，未隐藏失败、重投现场操作或放宽坏质量要求。

## 产物与部署记录

- 通信程序SHA-256：`330520b796dbce47b57b7878c5b0bf786ebc7a88ceafc9f31852b6398a5ce6e9`。
- 管理端SHA-256：`e7870b5b882dbaafce289cbfafb7f15ce30d2a7f0923e2f40d7c2d4c6e9d3cd8`。
- 管理端源码摘要：`a06dac1dec93d4a3a6f0c4dffa3f4a779f2feea9d6b745bf2c480f25b68ed712`；脏工作树基线提交`769ef2a1f82e6e7bb5e77f9e79a3e88434005e42`，未提交/推送。
- Semaphore软件包ID：`5e27889df66f5dff63c2e0a15cd3fe6e33ed21350f336f0e806d026d06827c41`；升级任务 **50653**，操作`01df61f0656763531fb8a63ee353b782ff3eee0e96178f111d91d7a5452c8831`，原生网页验收结果`passed=true`、目标46、状态`succeeded`。北京时间11:00:22完成维护，未重投。
- 通信程序使用独立停用服务发布工具；管理端使用Semaphore原生网页、任务队列和现有发布器，不手工绕过发布器替换管理端。

## 上线验收

- 2026-10-20北京时间10:59:26更新通信程序，PID91300；管理端PID92824。两者active、NRestarts=0，`/proc/<pid>/exe`摘要与上述产物一致。
- `deployment-backup/deployment.json`保存旧二进制及配置/状态/PKI备份记录。config.toml摘要保持`546f46dc249e72661ff0d4c3c5376ef28672d2d47be41e691249cfeeddb0d819`；points.csv保持`e96c151cda76429027b6ccb2c374552abf689a3f3bfc229f013884a944a7ecd2`。
- `atomic-live.json`验证4842和4840的Snapshot访问级别、读取及订阅；状态`transmit_enabled=false`、`received_inputs=0`、`complete_writes=0`。`communicate-fds.txt`确认无打开的串口设备。
- `sync-live.json`验证两端Browse/Read/Subscribe，通信业务节点264、管理端业务节点5561；接收/发送输入均按停用配置返回BadOutOfService，刷新5000ms、超时15000ms。
- `health-post.json`、`ready-post.json`全部检查正常；上线短窗口`postdeploy-journal.log`未见异常崩溃或自动重启。原采集PID2777575、Agent PID3333828及启动时间未改变。未操作30/58或升级Semaphore。
- 现场验收只读，没有调用Write/Method或发送硬件报文；整包写入与冲突测试在隔离进程完成。证据全部保留在上述46目录，当前短窗口检查不代替长测。

没有真实串口收发或物理设备验收。没有一小时或过夜稳定性结论，不能把隔离软件测试作为现场硬件验收。
