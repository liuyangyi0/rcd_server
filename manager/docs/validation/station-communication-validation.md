# 站点通信状态节点远端验收（2026-09-15）

## 本次交付

- 管理端版本：`0.7.2-station-health.1`，基线 `6f0dd6a` 加本次站点状态改动。
- 部署位置：`192.168.10.46:/home/lenovo/code/rcd_device_groups_20260828/bin/RcdManager-0.7.2-station-health.1-linux-amd64`。
- SHA-256：`ec468b5fbfcb044fbbbab5d635abda040190a91ea728ef67f3fbf358b4dcc688`。
- 固定启动链接：`/home/lenovo/.local/bin/RcdManager`；原 `rcd-manager.service` 自启动配置不变。
- 2026-09-15 16:39:56（北京时间）重启管理端，16:39:58 启动完成。
- 管理 OPC `4840`、下级 OPC `4841`、Web `8087`、安全策略和默认来源均不变。
- 没有修改或重启三台下级采集程序，没有改串口轮询参数，没有发送设备控制命令。

两个只读物理树都新增 `CommunicationState/UInt32`、`CommunicationStateText/String`、
`CommunicationObservedAt/DateTime`，不随 Session 路由改变。
数值定义和质量区别见 [站点状态说明](../guides/device-groups.md#站点通信状态数值变量)。

## 已执行测试

所有构建和可执行测试都在 `.46`，没有在本机执行测试。

| 项目 | 结果 |
| --- | --- |
| `go test ./... -count=1` | 通过 |
| `go test -race ./internal/domain ./internal/app ./internal/opc -count=1` | 通过，无 race 报告 |
| 数值映射、停用/离线优先级、过期、未来时间、旧 epoch、缓存、控制应用中 | 通过 |
| 仅通信变化更新内存、不触发版本快照持久化的比较逻辑 | 通过 |
| 只读 UInt32/标量、所有状态值自身质量 Good、Session 无关 | 通过 |
| 成员树增删和稳定节点回归 | 通过 |
| 隔离管理端 `14840/18087` 连接三台真实下级 | 135 个站点 Browse/Read 通过，两棵树状态一致，时间新鲜 |
| 三个来源各订阅一个真实站点 | 收到 UInt32、Good 通知 |
| 隔离管理端禁用/恢复 `.58` 来源 | 同一个订阅收到 `2 → 5 → 2`；只改变隔离管理器的连接，没有停下级 |
| 正式管理端 `4840` 部署后复验 | 135 个站点读取、三来源订阅通过 |
| 正式管理端双匿名 Session 选源 `.30/.58` | 独立路由、Read/Subscribe 回归通过；只修改测试新建 Session |
| `/healthz`、服务状态、来源状态、监听端口 | 正常，三来源 Connected/Active，各 3300 个监控项，默认仍为 `real-lower` |

16:40:26 的验收快照：28 个站点状态为 1，107 个为 2。该统计是实时物理采集状态，
不是测试失败数量：`.30/.58` 的示例站点均为 2，`.46` 的 WCH0/11 为 1。
本次不据此判断硬件故障。状态 2 只能说明下级尚无有效通信数据/通信异常。

正式启动日志显示有序停止和启动，没有 panic/异常退出；检查时 `NRestarts=0`。
`.46` 下级 PID 2507，启动时间仍为 2026-09-10 16:52:08，未被此次部署重启。
这不是长期稳定性结论；本次没有执行一夜 soak，也没有人为拔串口、停用真实站点来制造状态 3/4。
3/4 的判定经过自动测试，真实断开/恢复通过隔离管理器连接完成。

## 日志、源码及备份

远端工作目录：`/home/lenovo/code/rcd_station_health_20260915/`。

- `manager/`：构建及测试源码；前端资源沿用当前发布版，无前端布局变更。
- `logs/all-tests.log`、`logs/race.log`：完整测试记录。
- `logs/isolated-live.log`、`logs/isolated-reconnect.log`：隔离实例真实下级验收。
- `logs/production-live.log`、`logs/production-startup.log`：正式部署复验及启动日志。
- `logs/production-sources.json`、`logs/production-groups.json`：部署后状态快照。
- `backup/manager-before-deploy.db`：部署前 SQLite 在线备份。
- `backup/RcdManager-before`、`backup/rcd-manager.service`、`backup/manager.env`：旧二进制和启动配置。

验收结束已停止隔离实例，`14840/18087` 不再监听。
需要回滚时可将固定链接指回旧版 `RcdManager-0.7.1-web-fix.1-linux-amd64` 并重启管理服务。
本次没有数据库 schema 迁移，正常二进制回滚不需要覆盖数据库，以免丢失后续配置修改。
