# 验证命令与发布验收

本文件区分“仓库内自动检查”和“必须在真实环境执行的验收”。命令存在或测试代码存在，不代表某次发布已经通过。每个发布候选都应保存命令、版本、主机配置、开始/结束时间和原始结果。

## 仓库内检查

从 `manager/` 执行 Go 检查：

```powershell
go test ./...
go test -race ./...
go vet ./...
```

前端检查：

```powershell
Set-Location .\web
npm ci
npm run test
npm run build
```

完整发布构建会再次运行前端 build 和全部 Go test：

```powershell
Set-Location ..
.\build.ps1 -Version '0.1.0-rc1' -Commit (git rev-parse --short HEAD)
.\dist\RcdManager.exe version
```

需要单独确认构建出的 EXE 在没有 `web/dist` 外部文件、没有 Node.js 的干净 Windows 主机上仍能打开 Web 页面，以证明静态资源确实内嵌。

### 原 Session 与订阅恢复回归

当前远端验证还必须分别执行两个固定 OPC 依赖的测试；管理模块的 `go test ./...` 不会自动递归进入嵌套模块。以下命令在隔离的远端 Linux 测试机运行，不在开发机启动程序或测试：

```sh
# 当前目录为远端 manager/
go test -race ./...
(cd third_party/awcullen-opcua && go test -race ./...)
(cd third_party/gopcua-opcua && go test -race ./...)

RCD_DEVICE_GROUPS_ENDPOINT=opc.tcp://127.0.0.1:4847 \
RCD_DEVICE_GROUPS_API=http://127.0.0.1:8083 \
go test -v -count=10 -timeout=5m ./internal/opc -run '^TestLiveDeviceGroupsSessionResume$'
```

该 opt-in 测试使用已配置好的两台真实 Rust 测试下级，仅截断自身代理连接。验收须同时确认原 Session ID、组路由 revision、原订阅及数据值保持正确，并覆盖断开时仍有 Publish 排队、重传/确认和通知序号不复用。不能用新建 Session、重建空订阅或只验证 Read 来替代原订阅恢复。测试代码存在不表示本次构建已经通过，须保留实际日志和版本。

### 2026-08-24 仓库内验证记录

本次实现已执行以下仓库级验证；它们验证 Go 管理程序和内嵌测试服务，不等价于后文的真实 Rust 下级或目标 Windows 主机验收：

- `go test ./...` 与 `go vet ./...`；
- Linux 容器中的 `go test -race ./...`（Windows 原生 Go 不提供 race detector）；
- fork OPC 服务端的测试、vet 与 Linux race 检查；
- React/Vitest 自动测试和 Vite 生产构建；
- Go 回环下的 None/None + Anonymous、Basic256Sha256/SignAndEncrypt + Anonymous/Username，以及 Browse/Read/Write/Call、双 Session 隔离和固定 NamespaceArray；
- 使用最终单一 EXE 启动 REST、SSE、Web 和 OPC 服务的冒烟；
- 浏览器检查主要路由、空态/错误态、配置 CRUD、在线 Session、手动选源弹窗、不可选来源禁用态、移动端导航和控制台错误。

本地发布候选：`RCD Manager 0.1.0 (local, 2026-08-24T05:02:01Z)`，`RcdManager.exe` 为 18,341,376 bytes，SHA-256 为 `4491669AA110A55F958EFDC72E23AE854C3B9E36CDD19FA002FF4CD698E94444`。该哈希只标识本次工作区产物；正式发布应使用实际提交号重建并生成新的记录。

每个正式发布候选仍应重新执行这些命令，并在发布记录中保存完整输出、EXE SHA-256 和运行环境。

### 2026-08-25 修复后回归记录

- 管理程序 `go test ./...`、`go vet ./...` 全部通过；
- 最新代码在 Linux/amd64、Go 1.26 容器中执行 `go test -race ./...` 全部通过（本机 Windows C 编译器不支持 Go race 所需的 64 位模式）；
- React/Vitest 共 5 个测试文件、19 个测试全部通过，Vite 生产构建通过；
- 当前 Rust 项目执行 `cargo test` 全部通过；
- 使用浏览器验证桌面端与 390×844 移动视口、主要页面、凭据编辑、清空默认源、双 Session 定向切源和错误质量展示，浏览器控制台无 warning/error；
- 使用真实 Rust 下级再次验证来源分阶段连接测试、立即重连计数、断线告警恢复、状态历史、管理审计、默认/指定来源点位视图、同组双来源对比、命令筛选与 CSV、证书/存储/运行参数、配置备份和诊断入口；移动视口无整页横向溢出；
- 最终内嵌 Web 的本地单 EXE 构建为 25,698,304 bytes，SHA-256 为 `21CD18B9368A512FF88BB763ABFFFC23420D1823C3344788DFF5D04916E99A24`；该 dev 产物只用于本次工作区验收；
- 下文真实 Rust 联调完成后，测试用 Go/Rust 进程均已停止，测试端口已释放。

## 基础运行冒烟

使用专用临时数据目录，避免污染生产数据：

```powershell
$env:RCD_MANAGER_OPC_PASSWORD = '<测试强密码>'
.\dist\RcdManager.exe run `
  --data-dir 'C:\RcdManager-TestData' `
  --opc-advertise-host '127.0.0.1'
```

另一个终端检查：

```powershell
Invoke-RestMethod http://127.0.0.1:8080/healthz
Invoke-RestMethod http://127.0.0.1:8080/readyz
Invoke-RestMethod http://127.0.0.1:8080/api/v1/overview
```

随后验证：

- 空数据库的 Web 空态，无控制台错误；
- 创建组、创建/编辑/禁用/删除来源；
- `If-Match` 缺失为 428，旧 revision 为 409；
- 浏览器跨源写为 403；
- SSE 能连接并收到 heartbeat/变化事件；
- 停止进程后 SQLite、日志、PKI 均可重新打开；
- 只保留证书或只保留私钥时启动应失败并给出明确错误。

## 真实 Rust 下级互操作矩阵

必须使用当前仓库编译出的真实 Rust 下级，不以 mock 或同一 Go 库服务端代替。每个组合记录 endpoint URL、下级提交号、管理程序版本、证书指纹和结果：

| Policy/Mode | Identity | DiscoverEndpoints | Browse | Read/Subscribe | Write `_Send` | Call `_SendCommand` | 断线重订阅 |
|---|---|---|---|---|---|---|---|
| None/None | Anonymous | 本地通过 | 本地通过 | 本地通过 | 本地通过 | 本地通过 | 本地通过 |
| Basic256Sha256/SignAndEncrypt | Anonymous | 本地通过 | 本地通过 | 本地通过 | 本地通过 | 本地通过 | 本地通过 |
| Basic256Sha256/SignAndEncrypt | Username | 本地通过 | 本地通过 | 本地通过 | 本地通过 | 本地通过 | 本地通过 |

还必须覆盖：

- 下级 NamespaceArray index 改变后，以 URI 正确重新解析；
- 服务端证书信任和客户端证书信任；
- SecureChannel 短暂断开后恢复原 Session；
- 无法恢复原 Session 时创建新 Session 并复制最新默认源；
- 连接 epoch 丢弃旧连接迟到的数据和状态；
- 单个 subscription 内按 500 点一批创建 MonitoredItem，以及连接断开后的删除和重建。

### 2026-08-25 本地真实 Rust 联调记录

在 Windows 本机使用当前仓库实际编译的 `target/debug/rcd_server.exe` 作为下级，并让 Go 管理程序同时连接三个来源：None/None + Anonymous、Basic256Sha256/SignAndEncrypt + Anonymous、Basic256Sha256/SignAndEncrypt + Username。Rust 下级监听 `127.0.0.1:4840`，管理程序测试端口为 OPC `4842`、HTTP `8081`。本次没有连接现场串口（配置中的 COM21 不存在），因此数据节点预期为 `BadNoCommunication`，命令协议可以到达下级但串口派发结果预期失败。

本次已确认：

- 三种安全/身份组合均完成 endpoint 发现、证书握手、Browse、Read、Subscribe、Write 和 Call；
- 每个来源发现 32 个目录节点，其中 31 个变量进入监控，另有 1 个 Method；管理程序固定 NamespaceArray 与约定完全一致；
- `_SendCommand` 返回 Rust 下级的 `Received`，`_Send` 在无串口时收到新的失败 ACK 并及时结束 pending；
- 发现并修复 Rust 下级 60 秒 SessionTimeout 与 Publish 不续活的互操作问题；加入 20 秒 Session Read 探针后，持续超过 60 秒未发生重连；
- 发现并修复连续命令最终 ACK 同值、在 250 ms 采样窗口内被合并的问题；基于 ServerTimestamp 的短时定向 Read 探针不会把旧 `3` 误认作新 ACK；
- 停止 Rust 下级后三个来源均进入 Disconnected/Inactive，缓存目录仍可用且全部点输出坏质量；重启后均自动重连、重新浏览并重建订阅；
- 两个匿名上级 Session 可对同一组选择不同来源，Web 修改仅影响目标 Session；清空默认源后新 Session 路由为空且业务点返回 `BadConfigurationError`；
- API 不提供密码时保留原 DPAPI 密文，显式清空密码后 Username 来源实际收到 `BadIdentityTokenRejected`，恢复密码后重新连接成功。

本记录证明当前本机源码组合的基础互操作，不替代目标现场证书链、Namespace index 变化、SecureChannel 原 Session 重附着、多批 5300 点订阅、正式 Windows 服务和性能/稳定性验收。上述未覆盖项仍不得标记为发布通过。

### 2026-08-25 Kylin 远端部署与真实 5900 点联调记录

管理程序部署到 Kylin V10 SP1 x86_64 主机 `192.168.10.46` 的 `/home/lenovo/code/rcd_manager_test/`，使用独立 OPC `4842`、HTTP `8081` 和数据目录；原 Rust 下级进程及其 `4840` 监听保持不变。远端运行构建为 `remote-test-3`，文件 SHA-256 为 `5C903F7A7F1EE8BB4BE4D88388EDBD51FB391D925975C4983D70D5EFB8BBD467`。

本次真实规模联调发现并修复三个问题：

- Rust 下级实际主机为 `192.168.10.46`，但 EndpointDescription 仍广告不可达的旧地址 `192.168.10.40`；下级客户端现使用发现结果选择安全策略和证书，但始终连接人工配置的实际 Endpoint，不再被旧广告地址重定向；
- 5900 点目录每批读取 500 个节点、即一次返回 1500 个属性时，下级出现 `BadDecodingError`；目录属性读取批次调整为 100 个节点；
- 每 500 点创建一个 Subscription 会在第 11 个时触发下级 `BadTooManySubscriptions`；现改为每来源一个 Subscription，在其中按 500 个一批创建 MonitoredItem。

远端结果：

- None/None + Anonymous 与 Basic256Sha256/SignAndEncrypt + Anonymous 两个来源均为 `Connected / Active / Active / Ready`，各监控 5900 个变量；独立连接探针分别发现 5900 个变量和 6 个 Method，四阶段全部通过；
- 现场样本中每个来源 465 个 Good、5435 个 Bad，管理默认路由与来源视图均能按实际 StatusCode 返回；
- 上级管理 OPC 的固定 NamespaceArray、Browse、Read、Subscribe、Session 级 `SelectedSourceId` 写入均通过；两个匿名 Session 同时选择 None 与 Secure Anonymous 来源时，各自路由和订阅保持隔离；为避免操作真实串口设备，本次没有发送 `_Send` 或调用 `_SendCommand`；
- 手动重连后来源恢复到 5900 个监控点，`ReconnectCount` 递增且管理审计记录操作者；配置导出不含密码，诊断 ZIP 完整性通过，SSE、Prometheus、`/healthz`、`/readyz` 均正常；
- 8 个 Linux 测试二进制包在远端执行全部通过；约 40 秒资源观察中 goroutine 为 47–48、分配内存随 GC 在约 38–60 MiB 变化，刷新队列和 SSE 丢弃计数持续为 0；
- 从管理工作站访问远端 Web 得到 HTTP 200，标题为 `RCD OPC 管理中心`，4842 TCP 可达。内置浏览器控制无法路由到该私网地址，远端主机又未安装 Chromium/Node，因此本次未生成远端渲染截图；这不计作视觉回归通过。

Kylin 不提供 Windows DPAPI，带持久密码的 Username 来源创建会明确失败；按照本轮“不修改安全实现”的边界，没有引入 Linux 明文或替代密钥存储。该远端部署为 `nohup` 测试进程，不是 systemd 服务，也不替代目标 Windows 服务、真实命令、长时间 soak 或完整性能验收。

### 2026-08-25 功能复审与远端 review-4 回归

在同一 Kylin 主机新增隔离构建目录和 Go 1.26.0/Node 24.14.0 工具链，源码、前端测试、Go 测试、race、生产构建和运行均在远端执行；原 Rust 下级始终保持 PID/4840 不变。候选版本为 `remote-review-4`，SHA-256 为 `5820BFFC843097DB899CC4F33F22CC22B419463B0893F3C903253DCB8FD44CFF`，当前测试实例 PID 为 `2531195`，OPC/HTTP 仍为 `4842/8081`。

本轮复审并修复：

- `source.status`、`group.changed` 和默认源 mutation 现在会失效活动点位查询，断线后即使不再收到样本也不会长期显示旧 Good 值；
- 未收录名称映射的 `0x8...`、`0xC...` OPC StatusCode 会按严重位显示 Bad，不再误标绿色；
- 双来源比较始终按同一并集目录分页；比较模式禁用独立质量筛选，避免两侧页集合不同而伪造“点位不存在”；
- 仅修改来源显示名称不再断开连接；真实在线来源改名并恢复后 revision `1 → 2 → 3`，连接保持 `Connected`、监控点保持 5900、ReconnectCount 保持 0；
- 来源状态事件和告警加入有序 dirty 重试，瞬时 SQLite 失败不再永久丢失断线历史/告警；系统信息和 Prometheus 增加待持久化数量；
- Web 将结果相同的“重连”和“重新发现”合并为“重连并发现”，并明确每次重连都会重新读取 NamespaceArray、浏览目录和重建订阅；
- 修复系统信息读取 `dirtyCommands` 时未使用其专用互斥锁的数据竞争。

远端结果：

- Vitest 6 个文件、22 个测试通过；TypeScript 和 Vite 生产构建通过；
- `go test ./...`、`go vet ./...`、`go test -race ./...` 全部通过；独立空库单 EXE 的 Web/REST/OPC、健康/就绪、配置 CRUD、管理审计及 graceful shutdown 通过；
- 新版本连接 None/Anonymous 与 Basic256Sha256/SignAndEncrypt/Anonymous 两个真实来源，均发现 5900 变量、6 方法并进入 Connected/Active/Active/Ready；
- 固定 NamespaceArray、Browse、Read、Subscribe、同值 Session 选源 revision 和双匿名 Session 隔离通过，真实命令仍按现场安全边界跳过；
- 40 秒运行抽样中 goroutine 36–38，内存随 GC 为约 34–64 MiB，刷新队列和目录/命令/审计/状态/告警 dirty 数均为 0；
- 内置浏览器仍无法路由到该私网，远端也没有 Chromium，因此没有把本轮远端渲染截图、浏览器控制台和交互视觉回归标记为通过。

### 2026-08-26 多角色对抗审查与远端 review-5 回归

后端持久化、OPC 路由/命令和 React 使用体验分别进行独立对抗审查，并在同一 Kylin 主机上完成修复后的全量回归。运行候选为 `remote-review-5`（commit 标签 `adversarial-review`），Linux/amd64 二进制 SHA-256 为 `44FEFEC8BB722776DC93843E9FFDCDD0AB234E43883FD1903B169C42161E11CB`。部署继续使用隔离的 OPC `4842`、HTTP `8081` 和测试数据目录，原 Rust 下级的进程与 `4840` 监听未停止、未覆盖。

本轮主要修复：

- 下级状态回调不再持有互斥锁执行 SQLite I/O；状态事件、告警和管理审计改为异步可靠重试，并在真正落库后发送 Web 失效事件；
- 告警批量核对改为单事务，避免部分成功重试造成 occurrence/revision 重复；删除来源或组时会关闭其遗留告警；
- 配置导入不再把尚未完成首次发现的来源设为默认源，导入后必须在目录 Ready 后显式选择；
- `_Send` ACK 不再强制要求先观测到中间态 `2`，避免 Rust 在一个 250 ms 采样窗口内完成 `2 → 1/3` 时错误超时；连接 epoch、sequence 和时间戳仍用于拒绝旧 ACK；
- 下级订阅与 ACK 定向读取统一串行去重，拒绝同 epoch 内较旧 SourceTimestamp 覆盖新缓存；
- 上级点位读取改为按 `Session + group` 定点查询路由，不再为每个点复制完整路由快照；被动写入 `_Send` 的空值/状态值不再创建虚假 pending；
- Web 仅让当前活动页发起高频查询，SSE 不再因每个 `source.sample` 重读 SQLite 历史；来源对比、Session 消失、表单刷新、Session 质量归属、Uncertain 状态显示、组删除缓存和带路径 OPC endpoint 校验均已修复。

远端回归结果：

- Vitest 7 个文件、27 个测试通过，TypeScript/Vite 生产构建通过；
- `go test ./...`、`go vet ./...`、`go test -race ./...` 全部通过；
- 独立空库候选的单一二进制、Web/REST/OPC、健康/就绪和关闭流程通过；
- 真实 Rust 下级的 None/Anonymous 与 Basic256Sha256/SignAndEncrypt/Anonymous 探针均发现 5900 个变量、6 个 Method；两个来源进入 Connected/Active/Active/Ready；
- 管理 OPC 的真实 Browse/Read/Subscribe、同一组双匿名 Session 选择不同来源和 Session 隔离通过；按现场安全边界设置 `RCD_MANAGER_LIVE_SKIP_COMMANDS=1`，没有向真实串口设备执行 Write/Call 命令；
- 部署后总目录 5900 点，两个来源均在线；30 秒观察中 goroutine `38 → 37`，刷新队列及目录/命令/审计/状态/告警 dirty 数持续为 0，最终 `/healthz`、`/readyz` 正常，在线上级 Session 和 pending 命令均为空。

该部署仍是 Kylin 上的 `nohup` 测试进程，不是 Windows 服务。内置浏览器无法访问私网地址，因此本轮未把远端视觉回归标记为通过；真实设备命令、10 个上级全量订阅客户端、P95 延迟、1 小时稳定性、24 小时 soak 和 Windows 服务安装/重启自启仍属于正式发布前验收项。安全配置按本轮要求保持不变。

### 2026-08-26 真实命令、满订阅及 24 小时后台测试

在用户明确授权测试环境可执行真实命令后，对 Kylin 上的 Rust 下级执行了 `ttyAP0_SendCommand("0B01")` 和 `ttyAP0_Send="0B01"`。Method 同步返回成功；Write 被管理程序接受，Rust 下级实际尝试串口发送并记录两次 300 ms ACK 超时，管理命令约 5 秒后进入 `Failed / lower source rejected command`，pending 和组级选源锁均释放。随后独立验证同端口第二条命令及 pending 期间切源均返回 `BadWaitingForResponse`，终态后切源恢复成功。

10 个上级 Session、每个完整订阅 5900 点的 2 分钟测试通过，初始 59000 个值全部收到、订阅错误为 0。满订阅下的 Session 切源曾出现一次未观察到重发的现象，因此增加服务端匹配数量日志和“订阅后切源必须重发”的自动验收；重新构建的 `remote-review-6` 上，单点同值切源通知约 250 ms，5900 点重发可稳定收到。自动切换三个 Session 的 5900 点全量交付时间分别为约 157.7 ms、275.2 ms 和 106.0 ms，均低于 500 ms，90 秒测试通过。测试工具自身曾因每客户端原子计数器未初始化而 panic，该测试代码已修复；管理服务未崩溃。硬杀测试客户端遗留 Session 时，服务端正确执行 10 Session 上限并返回 `BadTooManySessions`，重启测试管理进程后清理。

全量 `go test ./...` 与 `go test -race ./...` 再次通过。安全端点 Username 集成测试曾瞬时出现一次 `EOF`，同用例连续 5 次复跑及之后全量复跑全部通过，原始失败与复跑日志均保留。当前候选为 `remote-review-6 / refresh-diagnostics`，Linux/amd64 二进制 SHA-256 为 `1A9AD838D9F75C3D120C63A9FABB4A6106303DEFDCEA5545B94DE0C5F6BF5A70`。

最终 24 小时后台测试于 `2026-08-26T00:41:30+08:00` 启动，计划运行至约 `2026-08-27T00:41:30+08:00`：保持 10 个 Session × 5900 点，每 15 分钟切换一个 Session 并要求其 5900 点在 30 秒内完整重发，同时每分钟记录健康/就绪、进程 CPU/RSS、Go 指标、来源状态、Session、pending 和错误计数。首分钟 59000 个值、订阅错误 0，健康检查正常。该项在完成前只能标记为“运行中”，结束后由远端脚本写出 `soak-24h.exit`、`soak-24h.finished` 和 `summary-24h.txt`，不得提前宣称 24 小时通过。

## 旧点表黄金对比

从同一台真实 Rust 下级和管理程序分别导出地址空间元数据，逐项比较：

- NamespaceArray 顺序和 URI；
- NodeId identifier 与 namespace；
- BrowseName/DisplayName；
- NodeClass、DataType、ValueRank；
- AccessLevel；
- `HasComponent`/`HasProperty` 等引用；
- `_SendCommand` 的 InputArguments/OutputArguments 名称、类型和 rank。

所有差异都必须有修复或书面兼容性批准。仓库已包含 `internal/opc/legacy_golden_test.go`，自动约束管理程序输出的 NamespaceArray、NodeId、BrowseName、NodeClass、DataType、ValueRank、访问级别、引用以及 Method 参数。它证明实现符合仓库内定义的旧点表契约，但尚未附带从目标现场 Rust 版本导出的地址空间及差异审批报告；正式兼容性验收仍须完成上述双端导出比对。

## Session 隔离验收

至少启动两个独立匿名上级客户端 Session，并让同组两个来源输出可区分值：

- 两个 Session 对同一 KKS 选择不同来源，Read 和 Subscribe 都得到各自来源值；
- Web 只改变指定 Session；
- OPC 写 `SelectedSourceId` 只改变当前 Session；
- 同值切源仍递增 selection revision 并触发目标 Session 重采样；
- 默认源修改只影响之后激活的新 Session；
- SecureChannel 重附着保留原选择；
- CloseSession/SessionTimeout 后，新 Session 回到当前默认源；
- 选择离线来源不自动切换，并返回 `BadNoCommunication`；
- 所选来源缺少并集点返回 `BadConfigurationError`。

## 目录与热配置验收

- 同组同名同类型正常合并；
- 同组同名类型/rank 冲突使涉及来源为 `CatalogConflict`；
- 跨组重复 identifier 使涉及来源不可选；
- 非法 `_Send` 或 Method 签名不可选；
- 首次发现前不能设默认/选中；
- 缓存目录离线启动可选，输出类型零值 + `BadNoCommunication`；
- 热加点立即出现；热删点当前进程返回 `BadOutOfService`，重启后消失；
- 删除来源/组的 Session、默认源和 pending 命令约束均生效；
- 数据库写失败时 `/healthz` 显示目录持久化异常，并能在恢复后重试。
- 上级地址空间实际 reconcile 临时失败时 `/healthz` 显示 `upstream_catalog` 异常，并在故障解除后按最新快照自动重试收敛。

## 命令验收

- 同一 `source_id + port` 并发命令被拒绝，不排队；
- 任一端口 pending 时整个组的默认源和全部 Session 选源返回 `BadWaitingForResponse`/HTTP 409；
- `_Send` ACK `2` 保持等待，`1` 成功，`3` 失败；
- 重连后即使旧 epoch 已有 ACK sequence，当前 epoch 的首个 `_Send` 保留值到达前也不得下发；保留 `3` 建立 baseline 后，本次派发产生的新 `3` 可立即失败；
- pending timeout 后状态为 `TimedOut` 并释放选源锁；
- Method 成功、下级错误、网络错误和短超时均正确记录；
- Session 中途关闭、下级断线或默认源变化不重定向已开始命令；
- 管理程序重启后恢复尚未完成的持久 pending 锁，并最终超时释放。

## 性能与稳定性

目标场景：20 个下级 × 约 5300 个监控项，10 个上级全量订阅客户端，客户端 sampling/publishing 不慢于 250 ms。测量点为“管理程序收到下级 DataChange”到“上级客户端收到通知”，P95 目标 ≤ 500 ms。

发布前至少执行并保存：

- 0%、1%、100% 点变化率；
- 单组切源 5300 点突发；
- 1 小时无积压稳定性；
- 24 小时 soak；
- Go race 检查；
- goroutine、内存、通知队列、SQLite 写延迟和端到端延迟趋势。

只有轮询周期无持续积压、1 小时稳定性通过且 P95 达标，才能保留 Session-aware polling 行为。否则需要进一步实现和验证按 `(session,node)` 索引的定向合并推送，不能通过放宽 500 ms 指标验收。

当前仓库不包含完整现场规模的压测驱动、1 小时/24 小时结果或性能报告；这些目标尚未被本文件宣称为已通过。

## Web 与 Windows 服务验收

### 下级运维回归（仅远端隔离实例）

先用本次编译的 Rust 二进制启动新的 `tools/device-groups-fixture.py` 实例，再使用
`tools/configure-device-groups.py` 配置空的测试管理器。不得把下列命令指向原有现场实例。

```bash
python3 tools/lower-operations-qa.py \
  --api http://127.0.0.1:8085 \
  --fixture /absolute/isolated-run/operations-fixture/Fixture.json \
  --output /absolute/evidence/operations-e2e-unique \
  --allow-mutations
```

该脚本的宿主和目录保护按本次 `192.168.10.46` 隔离部署设置，换环境须先核对保护条件，
不可直接移除保护。它校验两来源身份，覆盖文件往返/预检/CAS/备份恢复、单站停读、物理
句柄释放、开关重启保持、文件重启生效、2 MiB 配置的最坏转义传输、UTF-8 日志分片/追加/轮转/截断、
带筛选长行的完整续页及回溯上限，并在 finally
恢复配置和采集开关。日志页与配置正文不进入管理审计，测试会检查哈希及目标记录。
结果写 `report.json`，须同时检查 `ok`、`restoration`、原进程未变及进程退出码；失败与复跑
必须用不同输出目录。脚本会保留自动备份和测试日志，不代表真实串口硬件验收。

另需在远端执行 Rust/Go 单元及 race 回归，并用实际浏览器点击采集确认、配置草稿/版本冲突、
备份恢复、日志筛选分页和下载；测试旧版无运维能力、下级离线、长文件、Pending/命令互斥及窄屏。
不能以 HTTP 自动化或构建通过替代这些 UI 与边界验证。

真实 OPC 回归仍从远端 `manager/` 目录执行，使用与上面相同的独立 PTY 实例。旧版本兼容
检查必须指向另一个确实未提供运维命名空间的测试下级，不能使用新下级或依赖用例跳过：

```bash
RCD_DEVICE_GROUPS_ENDPOINT=opc.tcp://127.0.0.1:4862 \
RCD_DEVICE_GROUPS_API=http://127.0.0.1:8085 \
RCD_DEVICE_GROUPS_CONTROL=/absolute/isolated-run/operations-fixture/control.json \
RCD_LOWER_OPERATIONS_LIVE=1 \
RCD_LOWER_OPERATIONS_LEGACY_ENDPOINT=opc.tcp://127.0.0.1:4858 \
go test -race -v -count=3 -timeout=6m ./internal/opc -run '^TestLiveDeviceGroups'
```

该组合覆盖业务点表、两个匿名 Session 隔离、命令 ACK/超时、组和物理串口互斥、原 Session
恢复，以及无运维能力下级仍可采集。运维用例验证停读不阻止合法命令、命令 pending 时修改
开关返回 409 且不改变持久化/实际开关，清理时恢复采集并关闭测试 Session。

浏览器验收脚本在远端调用已安装的 Playwright Chromium；即使工作站 Browser 插件可用，
也不违反“全部测试在远端”的约束而改在本机执行：

```bash
RCD_QA_REMOTE=1 RCD_QA_ALLOW_MUTATIONS=1 \
RCD_QA_URL=http://127.0.0.1:8085 \
RCD_QA_FIXTURE=/absolute/isolated-run/operations-fixture/Fixture.json \
RCD_QA_OUTPUT=/absolute/evidence/operations-ui-unique \
PLAYWRIGHT_MODULE=/absolute/browser-tools/node_modules/playwright/index.js \
node web/scripts/lower-operations-qa.mjs
```

浏览器脚本的应用修改全部通过可见按钮与编辑器进行；API 仅用于只读断言，额外写入仅为
专属 PTY 日志目录内唯一命名的合成日志。它记录站点隔离、真实句柄释放、草稿保护、双页面
真实 CAS 冲突、备份恢复、日志筛选/续页/下载/轮转及 390px 窄屏操作；旧版/离线 UI 注入
必须与真实 OPC 兼容测试分开记账。恢复同样通过 UI，分别检查正文、开关、另一来源和原进程。
须人工查看产出的桌面与窄屏截图，不能只依赖整页没有横向溢出的数值断言。

上述脚本与其他修改同一 PTY 实例的测试应串行运行。原有 `web/scripts/device-groups-qa.mjs`
需分别在零 Session 与两个真实在线 Session 下复跑，以覆盖映射往返和 Session 定向切源；
两轮各自有意跳过的场景必须由另一轮实际覆盖，不能把跳过当作通过。

### 2026-09-01 上级 Method 真实串口结果反馈

修复前，管理器把下级 `_SendCommand` RPC 返回当作命令成功；Rust 下级实际采用 fire-and-forget Method，串口随后打不开或执行失败时，上级 Call 仍可能得到 `Good`。修复后，上级 Method 保留原 NodeId 和 `String -> String` 签名，但管理器向下改走 `_Send`，以来源、物理串口、connection epoch、ACK sequence 和写前 ServerTimestamp 固定本次事务，并同步等待设备 `1/3`。

全部构建和测试在 `192.168.10.46:/home/lenovo/code/rcd_command_ack_20260901_a` 完成：`go test ./...`、`go test -race ./...`、`go vet ./...` 以及新增成功、快速 `3`、下级立即坏状态和超时测试均通过。真实验收连接正在运行的 Rust 下级 `127.0.0.1:4841`，隔离管理器使用 OPC `14840`、Web `18087`：

- 暂停 `/dev/ttyWCH5` 后调用 `ttyWCH5_SendCommand("3D01")`：Method=`BadOutOfService`，同 Session `ttyWCH5_Send="3"/BadOutOfService`，审计=`Failed`；
- 恢复为 `enabled=true/open=true` 后下发读取类报文 `3D0802501004`，设备未返回通用命令 ACK：Method=`BadUnexpectedError`，`_Send="3"/BadUnexpectedError`，审计=`Failed`，约 210 ms 返回；
- 串口配置最终恢复为 revision `5`、`enabled=true`、`open=true`、`pending_commands=0`。

真实验收日志保存在 `/home/lenovo/code/rcd_http_logs_20260901-113254/test-logs/manager-command-ack-smoke-20260901.log`。本轮验证命令结果反馈，不代替长期稳定性或全负载延迟验收。

### 原有 Web 与服务检查

使用支持的桌面浏览器逐项验证：

- 总览、分组与选源、下级 OPC、点位浏览、命令记录、告警与审计、系统信息与维护；
- 来源分阶段连接测试、立即重连、重新发现和目录冲突详情；
- 告警未恢复/已恢复筛选、状态变更历史、操作者与管理审计；
- 默认源、指定 Session、指定来源点位视图及同组双来源对比；
- 命令/点位 CSV 导出、配置预检导入、脱敏诊断包和 Prometheus 指标入口；
- 加载、空态、错误态、409 revision 冲突；
- 在线 Session 抽屉和手动选源弹窗；
- 服务端分页、滚动时 `content-visibility` 和约 10 万点目录下的响应；
- SSE 高频事件只失效相关 query，不制造全局点数据增长；
- 1536×1024 等目标视口的视觉回归、键盘操作和焦点恢复。

Windows 服务需要验证：

- install/start/stop/uninstall；
- 自动/延迟启动和 Windows 重启自启；
- 停止超时和日志完整性；
- EXE 升级后数据库 migration；
- 停服备份、同机恢复；
- 异机恢复时 DPAPI 密码不可迁移的预期处理；
- Windows 防火墙/VLAN 规则。

在这些记录完成前，不应在发行说明中写“已完成真实互操作”“已通过 24 小时 soak”或“性能达到 P95 ≤ 500 ms”。
