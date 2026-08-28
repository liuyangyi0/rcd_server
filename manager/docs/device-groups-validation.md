# 设备分组新版：远端验证记录

日期：2026-08-28。基线：`70586a3`；开发分支：`codex/opc-device-groups`。

## 环境与隔离

- 本机仅编辑、格式化、保存 Git 版本和传输文件，没有本机编译、测试或浏览器运行。
- 远端：`192.168.10.46`，Kylin Linux x86_64，Rust 1.87.0、Go 1.26.0、Node 24.14.0、Python 3.8.10。
- 新版测试目录：`/home/lenovo/code/rcd_device_groups_20260828/`；源码在 `src/`，构建物在 `bin/`。
- 原有 `/home/lenovo/code/rcd/rcd_server` 和 4840 端口不修改、不重启。新增两套真实 Rust 进程使用独立 PTY、配置、PKI 和日志，不打开现场串口。
- 下级 A/B：4845 / 4846；管理 OPC：4847；Web：8083。
- 安全行为沿用基线；没有增加登录、改变匿名访问策略或部署到公网。

| 来源 | 物理串口 | 设备 1 → device-a | 设备 2 → device-b |
|---|---|---|---|
| lower-a | 独立 PTY `serial_a` | A_VALUE = 11 | B_VALUE = 22 |
| lower-b | 独立 PTY `serial_b` | A_VALUE = 101 | B_VALUE = 202 |

另有单设备计算 `CALC_A`、跨设备计算 `CALC_AB` 和常量 `CALC_CONST`。
跨设备及常量归 `shared`，单设备计算随设备组选择。不同物理串口名用于验证旧命令名到实际下级串口的解析。

## 已执行的后端与协议验证

| 项目 | 结果 | 日志（均相对远端测试目录） |
|---|---|---|
| Rust 全量测试 | 287 通过，4 忽略，0 失败 | `logs/rust-test-final.log` |
| Rust 构建 | 通过 | `logs/rust-build-6.log` |
| Go 全包 race 检查 | 通过，含本轮默认源锁及 RSA 兼容修复 | `logs/go-race-final.log` |
| 底层 OPC fork 全包 race | client / server / ua 并行通过 | `logs/opc-fork-race-final.log` |
| 合法短 D 的 RSA 用户名连接 | 五种块长度回归及真实用户名连接通过；沿用曾失败的测试密钥，不更换证书掩盖问题 | `logs/username-regression-final.log` |
| Go vet | 通过 | `logs/go-vet-final.log` |
| React/Vitest | 9 个文件、42 项通过 | `logs/web-test-final.log` |
| Web 生产构建 | 通过，静态资源内嵌管理程序 | `logs/web-build-final.log` |
| Windows amd64 EXE 交叉构建 | 当前候选代码构建通过，PE32+ x86-64；未在 Windows 上运行 | `logs/windows-build-candidate-final.log` |
| 默认源同值写入的 pending 锁回归 | 通过；被拒绝时不增加 revision | `logs/default-lock-7.log` |
| 两个真实 Rust 下级的旧节点元数据对比 | 每来源 7 个节点均保留 NodeId、BrowseName、类型、值秩、访问和 Method 参数 | `logs/device-groups-live-2.log` |
| 安全端点读取 | 下级 A/B 和管理端的 Basic256Sha256 + 匿名/用户名共 6 个用例通过；None/匿名用于其余联调 | 同上 |
| 双 Session Read/Subscribe、Web 定向选源 | 通过；设备 A 切源不影响设备 B 或其他 Session；旧 revision 返回 409 | 同上 |
| 默认源/Session 生命周期 | 默认变更仅影响新 Session；Close 后目标 API 返回 404，新 Session 回到默认 | 同上 |
| 实际串口命令路由 | 通过；PTY 日志确认原字节不变、目标来源/串口正确、未知设备和同端口并发被拒绝 | 同上及 `runs/fixture-1/commands.jsonl` |
| 下级停止/重启、离线选源、epoch | 通过；最后值保留并变坏质量，无自动切源，重连后恢复新值 | 同上 |
| 错误/丢失 ACK 与后续恢复 | 通过；`3`/坏质量、失败审计、解锁、下一条命令成功 | `logs/device-groups-faults-1.log` |
| 命令中途关闭 Session | 通过；其他 Session 仍受组锁约束，回执后解锁，审计保留原发起 Session/来源/串口 | 同上 |

`SendCommand` 保留 Rust 的原有语义：同步代理 Method 返回不等于等待设备执行完成；实际 ACK 成功/失败/丢失由 `_Send` 写入链路验证。
Method 的 pending 锁持续至 RPC 返回或超时，审计 `Received` / 状态 `1` 表示代理调用完成；
`_Send` 的 pending 锁才持续至设备 ACK 或超时。

单元/集成回归还覆盖：设备映射与 SQLite 缓存恢复、部分缺点并集、跨组冲突及修复恢复、
非法拓扑隔离、热删除、只读 `*_Send` 合法点名、独立命令结果历史和被动复位。

补测修复了两类既有问题：默认源同值写入绕过 pending 组锁；底层库错误地使用 RSA 私钥
指数长度而非模数长度分块，导致部分合法证书的用户名连接失败。后者仅为协议兼容纠错，
没有改变安全策略、端点或权限。底层测试夹具另修复了两包争用 46010、未等待启动就绪、
退出前未清理的问题；测试端口现在分为 46010 / 46011，生产监听不变。

## 浏览器验收

通过远端 Playwright/Chromium 操作真实部署。未使用本机 Browser 插件，原因是用户明确要求所有测试在远端运行。
检查包括桌面 1600×1000、移动端 390×844、真实映射保存/恢复、默认源、点位过滤与比较、
在线 Session、空态、控制台错误和页面横向溢出。

- `logs/browser-qa-4/`：7 个流程通过；无在线 Session 时实际保存设备映射、触发目录冲突并恢复，
  校验配置 revision、无改动禁止保存、默认源切换、分页与串口/设备过滤、双源比较、空态及窄屏。
- `logs/browser-qa-5/`：两个真实匿名 OPC Session 同时在线，7 个流程通过；验证映射编辑被锁，
  Web 只切换指定 Session 的 `device-a`，其 `device-b` 及另一 Session 的路由/revision 保持不变，随后切回。
  默认源修改也未改变已有 Session。
- 两轮控制台错误/警告均为 0，同源 HTTP 错误为 0，无整页横向溢出；窄屏表格采用容器内横向滚动。
- 每轮 `device-groups-qa.json` 保存请求、revision、断言及恢复结果；默认源和映射均恢复原值。
  截图涵盖下级映射、切源确认框、双源比较、设备点表、Session 抽屉和 390px 窄屏；已查看截图确认布局。
- 早期 QA 的抽屉遮挡及 React 路由更新等待问题已在自动化脚本修正；最终两轮使用同一版本脚本。

## 复跑协议测试

以下命令仅用于已启动的这套隔离 PTY 环境，不能指向现场硬件：

```sh
cd /home/lenovo/code/rcd_device_groups_20260828/src/manager
RCD_DEVICE_GROUPS_ENDPOINT=opc.tcp://127.0.0.1:4847 \
RCD_DEVICE_GROUPS_API=http://127.0.0.1:8083 \
RCD_DEVICE_GROUPS_CONTROL=/home/lenovo/code/rcd_device_groups_20260828/runs/fixture-1/control.json \
go test -v ./internal/opc -run '^TestLiveDeviceGroups' -count=1 -timeout=4m
```

新环境可用 `tools/device-groups-fixture.py` 创建两套隔离下级，再用
`tools/configure-device-groups.py` 对空管理器完成对应配置。fixture 根目录须新建且为空，脚本拒绝覆盖已有环境。

## 保留的运行记录及边界

- 管理程序：`logs/manager-runtime.log` 和 `runs/manager/data/logs/`；数据库与 PKI 在 `runs/manager/data/`。
- fixture：`runs/fixture-1/Fixture.json`、`events.jsonl`、`commands.jsonl`、`polls.jsonl`，及各下级 `a/logs/`、`b/logs/`。
- 失败的早期构建/测试日志保留，以上表格明确指出修复后的通过记录，不用旧结果替代新结果。
- 此处验证了真实 Rust 软件与模拟串口互操作，不是现场仪表硬件验收。
- Windows EXE 已在 Linux 交叉构建；Windows 服务安装、自启、DPAPI 的目标机验证未在这台 Linux 主机执行。
- 未在本轮执行 20×5300 点、10 个全量订阅的 P95≤500ms 性能验收，也未完成 1 小时/24 小时 soak；不能据本轮小型功能联调宣称这些指标达标。
