# 2026-10-13 审查问题修复与部署验证

## 结论与范围

三项已确认问题已修复，隔离回归通过后，已按本次控制暂停授权完成真实部署和短时验收。30/58/46 管理端为 `0.6.6-review.20261013.1`，46 Semaphore 为 `v2.19.16-rcd.6`。下级、Agent 未升级或重启。108 项交叉核对通过，已通知恢复控制；后续不再执行维护写操作。没有发送硬件命令、覆盖业务配置、清理日志、提交或推送。一小时、过夜和真实串口控制未验收。

工作树为 `rcd_server_device_groups`，分支 `codex/opc-device-groups`，基线提交 `769ef2a1f82e6e7bb5e77f9e79a3e88434005e42`。保留原有未提交内容，本次源码也是未提交开发状态，不冒充正式发布版本。

## 修复

| 问题 | 修复后的行为 |
| --- | --- |
| 旧协议迟到订阅 ACK 可能完成新命令 | 订阅与补读共用写前时间基线，派发前清除暂存旧终态；快速 ACK 保留，但等于/早于基线、缺少时间或旧 epoch 的通知被拒绝。网络调用不持共享锁，慢请求不阻塞其他节点。无法确认基线或下级仍等待上条 ACK 时不发送；无保护适配器不能退回普通写入。 |
| 不完整的 `recovered` 报告释放发布占用 | 任务创建时固定原二进制摘要和运行/停止状态；恢复必须匹配原摘要、最终恢复阶段及时间、PID/停止证明和对应组件就绪证据。缺少证据保持“结果待确认”和占用，仅补查原事务，不重新部署。旧任务没有固定原映像证据时不猜测补齐。 |
| 前一台成功、后一台失败仍显示“安装前失败” | 所有目标结果明确且部分成功时使用 `partial_failed`，界面显示“部分完成（有失败）”，保留各台成功、恢复、失败或未开始结果。任一目标未知/恢复失败/维护未完成时仍保留整体占用；不是用“部分完成”掩盖未知。 |

不改 Rust 下级、OPC 业务 NodeId/端口、安全策略、串口时序、ACK 格式、选源或计算语义。软件基线不能消除物理 `00 50 50` 没有命令编号的限制，不能据此承诺设备恰好执行一次。

## 实际验证

全部构建、测试和浏览器执行于 `192.168.10.46`，隔离根目录：

`/home/lenovo/code/rcd_review_fix_20261013_qp4V2W/`

| 检查 | 实际结果与证据 |
| --- | --- |
| Go 管理端全包并发回归 | `GOMAXPROCS=4 go -C source/manager test -race ./...` 通过；`manager-all-race.log`、`manager-all-race.exit=0`。未启用现场集成测试环境变量。 |
| Semaphore 任务、业务、存储及 API 回归 | `GOMAXPROCS=4 go -C semaphore-src test -race ./services/tasks ./services/rcd ./db/sql ./api/projects` 通过；`platform-all-race.log`、`platform-all-race.exit=0`。固定上游 Community 2.19.16 源码并应用仓库补丁，不修改运行平台。 |
| 前端与工具 | 25 项界面状态测试、2 项快照工具测试、修改页面 lint 和生产构建通过；`platform-ui.log`、`snapshot-tests.log`、`platform-lint.log`、`platform-web-build.log`。 |
| 单程序构建 | 管理端 `./cmd/rcd-manager`、Semaphore `./cli` 构建通过；`manager-build.log`、`platform-build.log` 及各自退出码 0。Semaphore 包含本次生成的官方前端资源。 |
| 浏览器 | 独立原生 Semaphore、随机隔离账号、独立 SQLite、loopback 端口。6 组检查通过，1366×768 / 1920×1080 无页面横向溢出；未知结果、回复丢失幂等、多机部分完成及逐机结果已显示。`browser-evidence-v2/result.json`：`errors=[]`、`fixtureOperations=true`、`liveTasksExecuted=0`；截图同目录。临时平台已退出。 |
| 源码与格式 | 涉及 Go 文件仅在 46 用 gofmt 格式化并导回；导回前核对本机自上传后未变动。18 个功能/测试文件摘要保存在 `fixed-files.sha256`；本次受跟踪文件 `git diff --check` 通过。 |

首次定向应用测试因新增状态码常量缺失编译失败，补齐后 `manager-focused-v2.log` 和全包 race 通过。首次新增浏览器用例在抽屉打开时点击被遮挡的刷新按钮失败，按正常操作关闭抽屉后刷新并重开详情，第二轮通过；保留 `browser.log` 和 `browser-v2.log`，不将失败记录删除或计为通过。

验证用二进制（非发布包）：

- `RcdManager-verified`：SHA-256 `0560defd8e455ef306b96471b69d679ea3e663a2f7232c4fb3c242b2c24063aa`。
- `semaphore-rcd-verified`：SHA-256 `a31637c0f8b6ad2d7760c1ddfd88e335e34f6f366d1a9565372421b23d77c7f1`。

隔离验证结束时，46 现场管理端/Agent/下级/Semaphore 原 PID 仍在，隔离浏览器没有创建真实发布任务。这些隔离证据不等于下述真实部署或硬件验收。

## 真实部署与验收

本次用户明确授权通过运维平台部署三台管理端、更新 Semaphore，并暂停控制至验收结束；范围不含下级、Agent 升级或重启。

正式包 `manager-0.6.6-review.20261013.1.rcd.tar.gz` 从原生网页上传并校验，包 ID `cf093c5ac8a611ef1430844dff6dc5acf28923ee749ba2bf968462ca754dba467`。浏览器逐台选择目标和本次暂停确认，由原生队列 → Ansible → RCD 发布器执行，没有直接替换业务二进制。每台只提交一次，原生任务、程序运行版本及实际映像均确认成功，浏览器 pageerror 为零。

| 机器 | 原生任务 | 新管理端 PID | 结果 |
| --- | --- | --- | --- |
| 30 | 20861 | 675677 | 升级成功 |
| 58 | 20865 | 2806245 | 升级成功 |
| 46 | 20872 | 449827 | 升级成功 |

正式构建完成 241 项 Web 测试、前端生产构建、管理端全包测试和 Go 构建。946 个冻结输入逐文件与正确工作树比对差异为零；源码摘要 `4ebf8445ba55cfe539321cb0ccc2efe6ca39406c62ace60c606672c14f6801f7`，明确记录 dirty。管理端实际二进制 SHA-256 为 `7fb1894954e2784ba04431dcf1ad7c6750f3dd5f8809a3e1a9ec1ec3a2bb19b5`，发布包 SHA-256 为 `97dbd2fba7316ceef078e8b24a1ccdca6c95c5669c2cddc979b3a1d2cd6f60a9`。数据兼容范围仍为 `manager_db=16`，最低适配器 `1.1.0`；包不携带配置、数据库或 PKI。

Semaphore 使用既有平台安装器完成空队列检查、一致性数据库/加密配置/包目录/程序备份后更新，未将“平台更新”伪装为业务组件任务。两次初始尝试均因巡检正在运行在首次预检处停止，没有停服务或替换。确认这些证据后，在同一进程中只读等待队列空闲，再仅调用一次安装器，更新成功。原失败日志保留，没有中断巡检、清空占用或绕过未知结果。

- 平台实际二进制 SHA-256：`b0eba8c952cb92db721300a9a3f4a1edfcacee8920a8075295eb90a440dfe576`；新 PID `429419`，`NRestarts=0`。
- 平台配套备份：`/var/lib/semaphore/backups/native-platform-1791868970622010613`。原数据库、凭据、包和历史任务保留。
- 原生发布仓库 2 切至不可变快照 `/opt/semaphore/rcd-platform/tools-380e9eaa71ee3c3a1c8f`，旧快照保留。
- `manager-backups-current.json`：三份管理端原映像与部署前摘要一致，SQLite 备份 `quick_check`/schema 正常，当前普通配置/PKI 文件与备份相同。没有还原旧配置或数据库。

`postdeploy-current.json` 与 `post-window-current.json` 两次只读完整检查均无 findings，九条来源/Agent 路径确认空闲。六个 `4840/4841` 入口 Browse/Read/Subscribe 成功，各订阅 12–13 个变量；业务 NodeId、NamespaceArray、类型、值秩、访问级别、引用及 Method 参数与本轮 `opc-current-before/` 相同。30 管理端与三个下级各 3300 个变量，58/46 管理端各 5297 个变量。

北京时间 **13:40:48–13:46:48** 的七个连续分钟样本覆盖 360 秒：三台管理端 HTTP/健康明细正常，九条来源重连增量均为零，启动时间与来源 revision 稳定，无待处理命令。之后完整检查仍确认管理端/平台 PID 稳定、所有服务 `NRestarts=0`。三台下级与 Agent 的 PID 和摘要从部署前到验收后均未变化，运行版本仍为 `0.6.5-review.20261012.2`。

`postdeploy-current-logs-final.json` 从北京时间 13:39:45 起读取三台日志及 journal：本轮既定的软件崩溃、异常握手、审计不可用/查询超时、连接重置等关键字均未出现。仍有下级串口 `read_timeout`：30/58/46 分别记录 768/774/272 条匹配日志；这是记录条数，不是发包次数或设备数。样例异常持续时间约 18 小时，早于本轮管理端发布。它们按采集通信报警单列，不能仅凭日志认定物理设备或串口卡故障，也不能把运维连接正常写成站点正常。

`deployment-final-current.json`：108 项交叉核对通过，包含真实网页单次提交、原映像固定证据、运行身份、未选组件保持、黄金点表、备份、窗口状态及日志。临时六分钟观察程序自然退出，没有新增常驻测试服务。证据均在 `/home/lenovo/code/rcd_review_fix_20261013_qp4V2W/`，没有清理或替换旧日志。

## 未覆盖

本轮未做真实串口控制/ACK、现场故障注入、负载延迟验收或新版过夜观察；没有 Rust 修改，未重复 Rust 构建。上述项目不得用隔离桩和页面示例冒充硬件验收。

现场本轮全部升级成功；失败恢复和多机部分失败仅使用前述隔离回归验证，没有为验收故意破坏现场服务。不能将六分钟观察计为一小时、过夜、全负载性能、Windows 或真实硬件 ACK 验收。
