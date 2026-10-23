# 连续命令 FIFO 修改与隔离验证（2026-10-20）

## 本次范围

管理端接收采集命令 `_Send` / 分组 `Send` 后，保存每一次被接受的写入，按固定来源、物理端口 FIFO 派发。不合并相同载荷，不从后来的节点值重新取发送内容。下级仍保持唯一串口 worker 和单个执行事务；不修改串口时序、ACK、重试次数或新通信程序 `RcdSync.*` 的整包快照语义。

- 容量：每端口最多 64 条，包含正在执行的一条；超限拒绝，不静默覆盖。
- `Queued` 表示排队，`Waiting` 表示执行中；两者都计入组选源锁和维护排空。
- 总期限沿用 `CommandPendingTTL`（默认 30 秒），从接受写入时开始，包含等待时间。
- 固定来源、Session、物理端口、载荷和连接代次；Session 关闭不重定向。
- 可信 ACK 成功或下级明确失败后继续下一条；执行结果未知、管理端等待超时或连接变化时终止剩余待发命令并记明原因。
- 管理端重启不重放队列；遗留 `Queued` 审计标记为中断失败。
- 同步 `_SendCommand` 方法不排队，仍在端口有执行或待发命令时拒绝。
- 队列仅属于当前管理端，不构成多个管理端之间的全局 FIFO。

OPC 新增只读 `Groups/<group>/QueuedCommandCount`、`ExecutingCommandCount` 和 `CommandQueueCapacityPerPort`；原 `PendingCommandCount` 包含两种占用。Write 返回 Good 只表示接受，执行结果仍须查询。`_Send` 反馈代表该 Session 最近接受的命令；每条独立终态保留在审计，不承诺订阅者收到全部中间状态。

Web 增加排队筛选、状态和说明；排队时显示尚未发往下级，不把尚无执行明细误称为旧协议或缺失审计。

## 验证环境

- 主机：192.168.10.46。
- 独立目录：`/home/lenovo/code/rcd_command_queue_20261020_70u9fX`。
- 源码：该目录下 `source/manager`，没有覆盖现场程序或数据库。
- Go/Node 使用远端既有工具链；所有构建、测试和 race 均在远端。
- 测试使用隔离的下级替身和真实 OPC UA 上级客户端/服务端连接，不访问真实串口或发送硬件命令。

## 测试结果

| 检查 | 结果 | 证据文件 |
| --- | --- | --- |
| `go test -race ./internal/domain ./internal/app ./internal/store ./internal/opc` | 通过 | `queue-regression-final.log` |
| 补充并发请求结果隔离后，`go test -race ./internal/app ./internal/domain ./internal/opc -run 'Test(Queued\|CommandQueue\|ConsecutiveCommand)' -count=3` | 通过；匹配的队列/线协议测试在 app、domain，opc 包无匹配测试 | `queue-burst-final.log` |
| 最终 `go test ./...` | 全部通过 | `go-all-final.log` |
| `npm test -- --maxWorkers=2 --minWorkers=1` | 36 文件、246 测试通过 | `web-tests-final.log` |
| `npm run build` | 通过 | `web-build.log` |
| `go build -o ../../rcd-manager-queue ./cmd/rcd-manager` | 通过 | `go-build-final.log` |

以上证据文件均在隔离目录根下。回归覆盖：

- 多个不同载荷在首条 ACK 前连续接受，按顺序逐条派发。
- 相同载荷不合并，64 条容量及第 65 条拒绝。
- 同一端口不并行、其它端口独立、同步方法不绕过队列。
- 关闭发起 Session 后继续固定路由，默认/Session 选源锁和维护计数。
- 管理端等待超时、断线、重连后不重放；可信下级 ACK 超时失败继续，Unknown 终态终止尾部。
- 进程重启遗留队列退役；并发写入调用不能领取另一请求并返回其派发错误。
- 真实 OPC UA Write 突发、ACK 终态通知和新队列计数节点读取。
- Web 排队行、详情说明及筛选。

首轮回归暴露旧测试仍断言第二次变量写入必须拒绝，已调整为同步 Method 的占用断言，并增加变量 FIFO 专项。后续编译发现一个未使用 import，已修正。首次全并发 Web 回归有一条无关运维抽屉测试超过原 5 秒期限；限制测试 worker 至 2 后全套通过，未延长该测试超时或更改业务实现掩盖问题。

隔离构建产物 `rcd-manager-queue` SHA-256：

`b8497e3a33980c84efb2132df27af55f3f305580a0440ee153351dc2834f3bea`

该产物仅为隔离测试构建，未登记为现场发布版本。

## 后续授权部署（2026-10-20）

用户随后明确授权部署，并确认“已暂停控制，可以部署三台管理端”。按 30 → 58 → 46 执行，仅更新管理端。没有升级下级采集、Agent、Semaphore 或新通信程序，没有发送硬件控制报文。

正式构建和发布证据目录：

`/home/lenovo/code/rcd_queue_release_20261020_iCpbWx`

正式构建使用已验证源码快照及现有 `tools/release/release.py`，重新执行前端测试/构建、Go 全模块测试/构建；源码逐文件摘要和 dirty 标记保存在构建清单中。通过现有 `rcd_package.py` 输出单组件包，数据库兼容范围仍为 schema 16，不覆盖现场配置或数据库。

- 版本/发布号：`0.6.10-queue.20261020.1`。
- 基线提交：`769ef2a1f82e6e7bb5e77f9e79a3e88434005e42`，`dirty=true`，未提交或推送。
- 构建源码摘要：`cfbf2d75845768f4c41ba394ae2443f72fed614aafe0f0481afd6ac81974ae57`。
- 正式二进制 SHA-256：`dbac3bb4a57f7f94667c74f9e492a20edfcf099ad3b2e004890e63b1bfcfa1d7`。
- 包 SHA-256：`ac3fcde200923aa8cea04dfe6301f7f77048be52b6cfd6abeba400c4e7dbf451`。
- 软件包 ID：`5942f4f40afadd66a2fc1cd4acc5b01ccbfa66b578c16665bf6d13204d93ca6bd`。

使用 46 上的浏览器实际操作 Semaphore 原生软件包页面：上传、选择单台设备、当次暂停确认、提交升级、查看执行结果。不是 SSH 直接覆盖现场程序，也没有重放未知任务。

| 目标 | Semaphore 任务 | 平台结果 | 启动日志时间（北京时间） | 验收 PID |
| --- | --- | --- | --- | --- |
| 30 | 51800 | succeeded | 17:22:00 | 361754 |
| 58 | 51807 | succeeded | 17:24:00 | 277278 |
| 46 | 51820 | succeeded | 17:28:02 | 593757 |

每台原生任务的请求、操作编号、阶段结果、运行版本确认及截图保存在 `deployment-browser/upgrade-<主机>/`。实际进程映像均与正式包一致，`NRestarts=0`。平台逐台检查未选择组件的运行摘要没有变化；46 另核对 Agent、下级、新通信程序和 Semaphore 的既有进程身份没有变化。

上线验收：

- 三台 `healthz` / `readyz` HTTP 200，数据库、目录、命令审计等检查均为 ok。
- `opc-30.jsonl`、`opc-58.jsonl`、`opc-46.jsonl`：真实 OPC Browse/Read/Subscribe 通过；全部组的排队/执行/总 pending 均为 0，容量为 64。
- 30/58 各三个来源、46 三个采集来源及 `communicate-46` 均为 Connected / Active / Active。
- `after-fleet.json`：三台管理端 journal 在所检查的部署窗口内只有正常停止/启动记录，没有新增错误、崩溃或自动重启。
- `manager-web-final/`：远端浏览器逐台登录管理网页，核对 FIFO 说明、“排队待发送”筛选、真实 API 返回及空态；1366×768 下页面无整体横向溢出，页面脚本错误为 0，未进行业务修改。
- 网页验收脚本最初误用非 hash 路径，随后下拉框定位/响应等待又超时；修正验收脚本后全部通过。没有因此修改产品、重复发布或重启服务。

完成上述验收后已通知用户恢复控制，本次维护窗口结束。未发送真实串口命令，因此不把上述只读验收当作真实设备突发命令验证；FIFO 行为由前述 46 隔离回归覆盖。一小时及过夜长测尚未执行，不宣称已通过。
