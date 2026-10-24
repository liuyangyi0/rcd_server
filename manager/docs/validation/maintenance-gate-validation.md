# P2 第一批：下级持久化维护准入

日期：2026-09-28。状态：源码和远端隔离回归完成，**未部署**；不是完整分布式发布互斥验收。

## 改动

- Operations schema v1 增加 `maintenance_gate_v1` 及进入/状态/退出动作；稳定实例身份、当前 boot_id、deployment_id 和递增 token 必须匹配。
- 新增 state/maintenance.sqlite（SQLite WAL、FULL）；确认落盘前不返回维护成功，重启保留维护，不因断网或时间经过自动开放。故障、缺失记录、身份不符不被误判为已解除。
- 旧 `_Send`、Method、关联命令共用准入；维护前接收的命令继续按原 ACK/超时/重试执行，维护后新命令被拒绝。已接受编号的重复查询仍可返回原记录，不重新入队。
- Operations 配置/采集/版本写动作持有准入凭证；RPC 返回 Pending 后仍通过原端口状态检查排空。自动版本查询也持有凭证，维护期间延后而不丢弃待查询任务。
- 维护期间保留只读接口和正常采集。普通轮询、保护/切换延时、ACK 判断、重试次数、业务 NodeId、Method 参数与 OPC/HTTP 端口不变。Method 的 busy 拒绝现在映射 BadWaitingForResponse，而非返回 Good 加错误文字。
- 维护数据库纳入路径迁移保护和发布备份清单检查。没有新增运行时依赖；复用已锁定的 rusqlite 0.37.0。

接口与限制见 [维护协议说明](../../../contracts/maintenance/README.md)。新增模块自己持有短内存锁；磁盘转换串行化，但不持有 Operations 配置锁，串口 worker 不写维护数据库。

## 本批实际验证

仅在 192.168.10.46 隔离目录执行：

`/home/lenovo/code/rcd_maintenance_20260928_UEsod5`

复用已有 Rust 编译缓存，不从缓存启动现场进程；测试数据均为临时目录/内存通道。未占用真实串口、未发送硬件控制或版本查询。

| 命令 | 结果 |
| --- | --- |
| `rustfmt --edition 2021 --config skip_children=true <本批涉及Rust文件>` | 成功，格式化源码已取回 |
| `cargo test --lib maintenance` | 10 通过 |
| `cargo test --lib --bins --test runtime_identity_cli` | 397 通过、1 忽略、0 失败：库381、main11、串口诊断工具纯函数2、CLI3 |
| `python3 -m unittest discover -s tools/release -p 'test_*.py'` | 18 通过，含维护数据库漏备份拒绝及一致性备份 |

新增维护测试覆盖：相同作业幂等、旧代次拒绝、新作业不能抢占、重启保留且旧 boot 被拒绝、所有活动修改入口拒绝、RPC 结束但串口控制仍 pending、自动版本事务排空、命令许可克隆生命周期、三个命令入口不入队、数据库写锁/失败和恢复、损坏/缺行/身份冲突保守关闭、并发进入与准入的 30 轮竞争。

证据在隔离目录 `maintenance-test.log`、`rust-regression.log`、`release-tests.log`；本地归档为 `artifacts/runs/2026-09-28-maintenance-gate/formatted-and-logs.tar.gz`。

本轮只读确认：30、46、58 的下级托管服务以及46、58的管理端服务均 active/running，NRestarts=0，PID 与本日迁移后的记录一致。该计数只代表当前服务启动周期，不等同于完整历史无异常，也不证明所有现场设备正常。

## 未完成与发布边界

- 未接入 agent 权威维护作业和物理身份绑定/握手；agent 进程控制仍不是此栅栏的参与方。
- 未完成发布阶段日志、重新进入后的恢复、Web 维护状态和跨语言黄金样例。
- 未做跨进程崩溃/断电、双管理端与 agent 并发发布验收；数据库锁测试不是物理磁盘满/掉电测试。
- 未启用的可选 TCP 前端没有新增维护接线，不能启用后声称全部入口已受保护；现网入口为 OPC Operations 和 OPC 命令。
- 未重新进行 Go race、前端/浏览器、性能 P95、一小时/过夜稳定性或现场控制测试；本批未改变 Go/React 运行时代码。
- 未提交、未推送、未升级、未重启任何现场服务。现场仍使用 `0.6.0-paths.20260928`。

下一小批应完成 agent 持久化作业与下级身份/token 对账，随后接入发布工具和 Web；完整联调通过后再分批现场部署。不能把本批下级 `drained=true` 单独当作停止/替换许可。
