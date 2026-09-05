# 下级进程管理：协议、部署与使用边界

本功能在 Linux 上通过独立进程守护程序管理一个固定的 Rust 下级。守护程序和管理器使用同一个 `RcdManager` 二进制，分别以 `agent run` 和 `run` 启动；不需要另一个 Go 运行时。Windows 进程守护尚未实现、未验收，不可将本说明作为 Windows 服务安装说明。

版本 `0.4.0-process-control`，管理器构建标识 `cc14f5d+lifecycle-v3`。新版已在独立远端实例运行，旧 `operations-v5` 部署未覆盖；最终 OPC、REST、浏览器结果与尚未验收的 systemd 等项目见 [验证记录](process-control-validation.md)。既有配置、采集开关、备份和业务日志协议见 [下级 OPC 运维协议](lower-operations.md)。

## 1. 两条管理通道

| 通道 | 目标与能力 | 下级进程停止后 |
|---|---|---|
| 管理器 → Rust OPC 运维接口 | 配置文件、串口开关、站点停读、Rust 业务日志 | 不可用；不通过此接口重启自身 |
| 管理器 → 独立 agent HTTP | 固定下级进程的 start / stop / restart、状态、进程输出 | agent 仍在运行，可再次 start |

进程操作针对来源绑定的物理实例，不经过 Session 分组选源；会影响使用该来源的全部组和客户端。不自动切换 OPC 来源。agent 本身停机或网络不可达时，管理器也不能启动它，需要现场恢复守护服务。

agent 只启动、等待和停止自己创建的 child，不接管已存在的 PID，也不接受请求指定可执行文件、参数、工作目录或任意 PID。Linux child 绑定父进程生命周期；agent 异常退出时该 child 可能被内核终止。不要用外部脚本同时守护同一个 Rust 实例。

## 2. 首次交接与目录

首次启用前，人工核对目标实例的可执行文件、配置、PKI、串口与端口；停止该目标原来的启动方式并确认已退出，再由 agent 启动。不能把“禁用管理器来源”当成停止 Rust，也不能删除 `.rcd_server.lock` 来绕过互斥。不要停止任何未列入本次交接的进程。

建议一个下级一个独立目录，使用实际二进制副本，不以多个软链接伪装不同实例：

```text
/absolute/path/to/lower-instance/
  rcd_server
  config/                 # config.toml、calc.toml、rcd*.csv、operations.json
  pki/                    # 保留原有证书与私钥，不随重启重新生成
  logs/                   # 默认业务日志；可由既有日志配置另行指定
/absolute/path/to/agent-state/example-lower/
  agent.lock
  process-state.json      # 期望状态、版本、有限作业记录
  process.log[.1]         # child 标准输出/标准错误
  agent.log[.1]           # 守护自身事件
```

Rust 业务配置目录位于**实际可执行文件**同级的 `config/`；PKI 按 child 工作目录解析，通常将 `--work-dir` 固定为下级目录。`RCD_CONFIG` 只影响既有日志配置，不改变业务配置目录。迁移时保留实际日志路径和 PKI，确认 TCP 主备地址、OPC 端口、串口没有实例间冲突。

agent 的 `--data-dir` 与管理器数据库目录、Rust 配置目录分开，不能共用给多个 agent。守护重启应继续使用原实例 ID 和原 data-dir；不要手工修改、清空状态文件或删除锁文件来消除未知作业。备份和升级前先确认停机窗口与该实例状态，不覆盖仍在执行的程序文件。

## 3. 前台运行及参数

以下全部路径均为占位值，应在确认旧目标已停止后替换。示例显式关闭首次自动启动，先启动 agent，再从管理器提交 start：

```bash
RCD_OPCUA_PORT=4841 /absolute/path/to/RcdManager agent run \
  --instance-id=example-lower \
  --executable=/absolute/path/to/lower-instance/rcd_server \
  --work-dir=/absolute/path/to/lower-instance \
  --data-dir=/absolute/path/to/agent-state/example-lower \
  --http-listen=0.0.0.0:8090 \
  --ready-address=127.0.0.1:4841 \
  --auto-start=false \
  --restart-policy=never \
  --stop-timeout=15s \
  --start-timeout=60s
```

| 参数 | 默认与含义 |
|---|---|
| `--instance-id` | 必填稳定标识；服务模板使用 `%i`，建议仅字母、数字、连字符 |
| `--executable` | 必填，解析为实际存在的固定文件；HTTP 不能修改 |
| `--work-dir` | 默认实际可执行文件所在目录；必须已存在 |
| `--data-dir` | 必填，独立持久化目录；不存在时由 agent 创建 |
| `--http-listen` | `0.0.0.0:8090`；生产使用明确非零端口 |
| `--ready-address` | 默认空；可设置该下级的 TCP 地址，非空时端口必须为 1–65535 |
| `--auto-start` | CLI 默认 `true`，仅首次无状态文件时决定是否启动；模板明确设为 `false` |
| `--restart-policy` | `never` 或 `on-failure`，默认 `never`，仅控制本实例异常退出后的拉起 |
| `--stop-timeout` | 默认 `15s`；SIGTERM 宽限期，超时后只强制终止自有 child，再等待最多 5 秒确认 |
| `--start-timeout` | 默认 `60s`；可选 TCP 就绪等待上限，启动另有存活观察期 |

时限必须为正且不超过 5 分钟，启动时限不能短于内置 1 秒存活观察期。`--ready-address` 不会修改 Rust 监听端口；非默认端口须同时设置 child 继承的 `RCD_OPCUA_PORT`，并与管理器的来源 Endpoint 一致。

`running`、PID、运行时长和 TCP 可连接只说明进程/端口状态，**不等于 OPC Session 已连通、目录已就绪或串口数据质量 Good**。启动完成后必须另看管理器的连接、订阅、目录、采集状态与点值。

## 4. systemd 用户服务（人工安装）

模板在 [rcd-process-agent@.service](../deploy/linux/rcd-process-agent@.service)，环境示例在 [agent.env.example](../deploy/linux/agent.env.example)。管理端和下级一起固定部署及开机自启的完整步骤见 [Linux 固定路径与开机自启](linux-autostart.md)。模板面向现有用户的 systemd user manager，不创建系统用户、不安装系统级服务，也不修改认证或内网安全策略。

以下步骤用于**首次人工安装**，不直接覆盖已有部署；如目标文件存在，先核对并备份。以实际运行下级、已具有串口访问权限的用户执行，不需要将这些命令改为 `sudo`：

```bash
install -d "$HOME/.local/bin" "$HOME/.config/systemd/user" "$HOME/.config/RcdManager/agents"
install -m 755 /absolute/path/to/new/RcdManager "$HOME/.local/bin/RcdManager"
install -m 644 /absolute/path/to/rcd-process-agent@.service "$HOME/.config/systemd/user/rcd-process-agent@.service"
install -m 600 /absolute/path/to/agent.env.example "$HOME/.config/RcdManager/agents/example-lower.env"
```

先编辑 `example-lower.env`：替换绝对路径、HTTP/OPC 端口；保持与实例 `example-lower` 对应的独立目录。环境文件不是 shell 脚本，不写 `export`，路径值不要使用 `~`、`$HOME`、`%h` 或其它变量拼接。模板通过 `${VAR}` 将每个完整参数传给固定程序，不调用 shell。[systemd 参数替换说明](https://raw.githubusercontent.com/systemd/systemd/main/man/systemd.service.xml)、[环境文件说明](https://raw.githubusercontent.com/systemd/systemd/main/man/systemd.exec.xml)。

确认环境文件及旧目标退出后，再执行：

```bash
systemctl --user daemon-reload
systemctl --user enable --now rcd-process-agent@example-lower.service
systemctl --user status rcd-process-agent@example-lower.service
journalctl --user -u rcd-process-agent@example-lower.service -n 100 --no-pager
```

`Type=simple` 的启动命令返回不表示应用已就绪，须检查 agent `/healthz` 和状态。模板的 `Restart=on-failure` 重启的是 **agent**；它与 child 的 `--restart-policy` 是两层不同策略。`KillMode=control-group` 让停止该用户服务覆盖其服务控制组内的进程；`TimeoutStopSec=40s` 为默认 HTTP 退出 5 秒、child 宽限 15 秒及强制退出确认 5 秒留余量。增大 stop-timeout 时必须同步增大该超时；服务管理器最终强杀不应被误称为 agent 的正常停止结果。[systemd 停止语义](https://raw.githubusercontent.com/systemd/systemd/main/man/systemd.kill.xml)。

`systemctl --user enable` 只配置用户服务，不单独保证未登录时开机运行。若需要退出登录后继续运行、系统启动时即拉起用户服务，须由现场明确授权后配置该用户的 `loginctl enable-linger`；不要在安装脚本中偷偷执行或获取 root 权限。[loginctl lingering 说明](https://raw.githubusercontent.com/systemd/systemd/main/man/loginctl.xml)。

网页 stop 只停止 child，agent 保持运行；`systemctl --user stop rcd-process-agent@example-lower.service` 则停止整个 agent 服务及其 child。要让下级在下次守护启动后仍保持停止，应先完成网页 stop，确认持久期望状态为 `stopped`。单纯停止 systemd 服务不把原来的 `running` 期望改为 `stopped`。

## 5. 管理器绑定与操作

在指定物理来源的进程管理面板配置 agent 的可达 HTTP URL 和固定 instance ID。地址须从**管理器所在机器**可访问；管理器不与下级同机时不能填 `127.0.0.1`。同一 instance ID 不能经不同地址别名重复绑定给另一个来源。该绑定独立于 OPC Endpoint，停止下级后仍保留。

操作前刷新状态，核对来源、实例、PID、期望状态和最后错误。start 启动固定程序；stop 等待实际退出；restart 先停旧 child，确认退出后再启动新 child。操作返回 accepted/running 只是已受理，必须等 succeeded/failed；停止超时升级强制终止时报告 `forced:true`，不是假称优雅退出。旧 child 未确认退出时不能冒险再启动第二个。

管理器在派发前持久保存意图，并以物理来源范围互斥：已有任意端口命令 pending 时拒绝进程操作；进程作业非终态期间阻止该来源的新代理命令、运维修改及冲突配置操作。另一物理来源不受影响。外部程序绕过管理器直接向 Rust/agent 发送命令不受这一层互斥保护；Rust 退出时拒绝新命令，但不承诺终止期间所有直接调用都能取得最终设备 ACK。

### 配置保存与重启应用

配置保存、备份恢复的原有流程不变，保存本身不重启。绑定 agent 后可明确执行“保存后重启/应用已保存配置”流程；该动作会重启整个物理下级，并重新加载**磁盘上当时的全部配置文件**，不只应用编辑器当前打开的文件，也不会保存尚未提交的草稿。

带 `expected_config_revisions` 的 restart 会先通过下级 OPC 核对所列文件的当前磁盘版本；任何不匹配都不派发重启。此预检只比较提交的文件，不是跨所有外部编辑器与启动加载的文件系统事务。操作期间不要在下级主机旁路修改文件。OPC 离线时无法执行此配置预检，但普通进程 start/stop/restart 仍可经在线 agent 执行。启动后重新检查待重启标记和实际数据，不能只看 PID。

## 6. REST 协议与版本

所有版本都应原样回传，不能自行加一。以下四者不能混用：来源配置 `source_revision`（数值）、进程绑定 `binding_revision`（数值）、agent 进程 `revision`（不透明 String）、下级配置文件 revision（SHA256）。实例 ID 请求头用于防止误连实例，**不是身份认证**；沿用已确认的受信任隔离内网部署边界。

### 管理器接口

前缀 `/api/v1/sources/{source_id}/process`：

| HTTP | 路径 | 行为 |
|---|---|---|
| GET / PUT | `/binding` | 查看 / 保存 agent URL、实例 ID；空 URL 和 ID 表示解除绑定 |
| GET | `/status` | 实时物理进程快照，附可信来源和绑定版本、当前作业；ETag 为 agent revision |
| POST | `/actions` | start / stop / restart |
| GET | `/operations?limit=50&offset=0` | 分页查看管理器持久作业记录，limit 1–100 |
| GET | `/operations/{request_id}` | 查看原作业并对账，不重新执行动作 |
| POST | `/operations/{request_id}/resolve` | 对账原请求或确定未受理，不是取消/重试动作 |
| GET | `/log?limit=65536` | 固定当前进程输出尾部；limit 4–65536 字节 |

PUT binding 携带 `If-Match: "当前绑定 revision"`（初始为 `"0"`），正文例如：

```json
{"source_revision":12,"agent_url":"http://agent-host:8090","instance_id":"example-lower"}
```

POST actions 携带 `If-Match: "刚读取的 agent revision"`，正文例如：

```json
{"action":"restart","request_id":"operator-unique-request-id","source_revision":12,"binding_revision":2}
```

仅 restart 可另带 `expected_config_revisions` 对象，键为配置文件名、值为对应的 64 位小写十六进制 SHA256。resolve 使用原动作、原 request_id、原来源/绑定版本与原 agent revision，不从新状态拼造另一条请求。

### agent 接口

除 `GET /healthz` 外，所有接口必须携带 `X-RCD-Instance-ID: example-lower`：

| HTTP | 路径 | 行为 |
|---|---|---|
| GET | `/healthz` | 守护 HTTP 可用与实例 ID，不代表 child/OPC 健康 |
| GET | `/api/v1/process` | 进程快照及 ETag |
| POST | `/api/v1/process/actions` | 提交固定实例动作 |
| GET | `/api/v1/process/operations` | 最近最多 100 条作业 |
| GET / POST | `/api/v1/process/operations/{request_id}` / 同路径加 `/resolve` | 查询 / 对账 |
| GET | `/api/v1/process/log?limit=65536` | 当前 process.log 的 UTF-8 尾部，limit 1–65536 |

动作/resolve 正文最多 4096 字节，只允许固定字段，均以正文的 expected_revision 为版本。POST actions 可另带 If-Match，但须与正文版本一致：

```json
{"request_id":"operator-unique-request-id","instance_id":"example-lower","action":"restart","expected_revision":"opaque-revision-from-status"}
```

进程快照含 `schema_version:1, instance_id, revision, state, desired_state`；按实际情况附 PID、启动时间、运行秒数、退出码、最后错误、pending_request_id、固定程序/目录、重启策略/次数和强制停止记录。进程 state 为 `stopped/starting/running/stopping/failed/unknown`；期望值只为 `running/stopped`。

作业含 `request_id, instance_id, action, state, created_at, updated_at`，可附 `forced, error_code, error, snapshot`。只有 `succeeded/failed` 为终态；`accepted/running/unknown` 仍需对账。提交/resolve 返回 202 表示非终态、200 表示终态；**HTTP 200 也可能是 state=failed**，查询接口返回 200 更不代表操作成功。业务错误使用 `{ "error": { "code": "conflict", "message": "…", "current_revision": "…" } }`，冲突/busy 为 409。

### 幂等与结果未知

一个用户动作生成一个 request_id，并在超时后保留原始参数。相同 ID、相同动作/实例/版本返回原作业，不再启动第二次；同 ID 参数不同返回冲突。不要以新 ID 自动重试 restart。网络中断后的 404 也不能证明动作未到达，必须对原请求 resolve：已经受理的返回原作业，未受理的登记失败记录，使迟到的原动作不能再执行。resolve 不取消已运行的作业。

管理器对 unknown 持续保留来源级互斥，恢复后查询并对账，而不是重派动作；只有得到并持久保存终态才释放。agent 的本地作业记录有界，当前保留最多 256 条，不能当作永久审计库；管理器 SQLite 中的作业与管理审计承担历史追溯。

## 7. 自动拉起、日志与恢复

`never` 不因 child 异常退出而自动拉起；`on-failure` 在期望仍为 running 时对本 child 的启动失败或非零退出进行重启，不因退出码 0 自动重启，也不响应 OPC 源离线做选源切换。人工 stop 的 stopped 期望会持久保存，重启 agent 后仍 stopped，即使 CLI auto-start=true。若期望仍 running，agent 重启时会恢复该期望；这是守护恢复，与 child 的异常退出策略分开。

进程日志固定为 agent data-dir 中的 `process.log`，包括 child stdout/stderr；API 只取当前文件最新尾部，不接收任意文件名，不提供连续历史游标或筛选。文本最多 64 KiB，保留完整 UTF-8 边界，截断用 `truncated` 标记；空日志不是业务健康证明。`process.log`、`agent.log` 各保留当前及一个 `.1`，每文件轮转阈值 5 MiB，不能用作无限期日志归档。Rust 自己的业务日志目录、轮转、筛选分页和备份配置流程不变。

故障排查顺序：确认 agent 可达且实例 ID 正确 → 检查原作业与进程输出 → 核对实际 PID/退出原因 → 检查 OPC 连接/目录/采集质量。遇到 unknown、持久化失败、锁冲突或停止未确认时，保留日志和状态文件交现场核查，不清库、不删锁、不批量杀同名进程。

## 8. 当前部署与验证状态

新版已在 `192.168.10.46` 的独立目录和 PTY 环境运行，入口如下；旧 Web 8085 / 管理 OPC 4862 包与部署保留，未覆盖原实例。

| 角色 | 本次运行端口 |
|---|---|
| 新管理器 Web / OPC | 8086 / 4872 |
| 两个独立 Rust 下级 | 4870 / 4871 |
| 两个独立 agent HTTP | 8091 / 8092 |

运行目录：`/home/lenovo/code/rcd_device_groups_20260828/runs/lifecycle-20260828-v1`。证据目录：`/home/lenovo/code/rcd_device_groups_20260828/logs/lifecycle-20260828-v1`。两个固定 agent 实例为 `lifecycle-a` / `lifecycle-b`，管理器来源为 `lower-a` / `lower-b`；运行中 PID 会因显式重启变化，应读取当前状态而不是抄写历史 PID。

本轮 Rust 335 项通过、4 项忽略；Web 141 项/17 个文件及构建通过；Go race、vet 和构建通过。最终 v3 OPC 四主测试通过，覆盖旧点表/安全、真实命令 pending 互斥、来源坏质量隔离、原 Session/Subscription/路由保持及幂等重放。v3 REST 再次 16/16，恢复和独立保护审计通过，包含真实退出/串口 FD 释放、agent 重启保持 stopped、启动失败恢复及配置 7→9→7 的显式重启应用。

仅操作新隔离实例。REST 最终逐一核对原服务及旧 v5 的 PID/startticks（398713、612417、618222、614880），并核对当时非目标 B 与 fixture 身份；保护审计通过。历史 PID 不是以后停进程的依据，也不能用于接管现有进程。

浏览器首轮提前查询的时序问题已修复。远端 ui-v2 六流程、恢复与七个保护进程检查通过；四次真实 POST 各延迟 250 ms，非预期 HTTP/console 错误和提前对账均为 0。桌面/窄屏无横向溢出，并已人工检查关键截图。最终只读审计 14/14：Schema 6、数据库完整性、无 pending、配置生效、双源 Good 及十个登记进程身份核对通过。这是本轮状态核对，不是持续稳定性压测。

最终验收时管理器为 v3（当时 PID 666309），两个 agent 仍运行同后台代码的 v2 EXE，未热替换为 v3；下级使用 Rust v1。本次发布包为管理器 v3 + Rust v1，均动态链接、保留调试信息、不 strip。systemd 安装/自启、Windows agent、真实串口硬件、满负载延迟和 24 小时稳定性未验收；普通启停成功也不代表所有强制停止/守护崩溃场景已在真实 Rust 上验证。完整证据和历史失败见 [验证记录](process-control-validation.md)。
