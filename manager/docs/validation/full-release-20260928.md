# 2026-09-28 全量部署记录（已完成，58 安全标签已恢复）

用户授权：全量部署 30/46/58 下级与 Agent、46/58 管理端，并明确确认所有管理端和直连客户端已暂停控制命令。保留每台机器业务配置、数据库与既有安全策略；未发送硬件控制命令。

## 正式构建

46 隔离目录：`/home/lenovo/code/rcd_full_release_20260928_MCNped`。

- 版本：`0.6.1-full.20260928`；发布编号：`rcd-full-20260928`。
- 起点提交：`769ef2a1f82e6e7bb5e77f9e79a3e88434005e42`，明确记录 dirty 工作区，未提交/推送。
- 源码摘要：`23845fb5d17ca251a239bcb74291137b640fa4bd609b9c734d9b918e49fd2fea`。
- 下级 SHA-256：`36f30e380fa1558f006b62b52593017641c44717e0bd364f65ddefaeabd4b29d`。
- 管理端 / Agent SHA-256：`0fc8cf14988fda36c0f161def4aebff87d80e63de9269ba129148d57cf57c651`。
- 正式包包含构建后的 Web；构建前后源码摘要一致。
- release 工具 98 项测试、Web 225 项测试、Web 构建、`go test -race ./...`、Rust lib/bin/runtime_identity_cli 测试与 release 构建通过。详细日志在隔离目录 `build/`，并非沿用上轮日志。

## 当前现场状态

| 主机 | 状态 |
| --- | --- |
| 30 | 下级、Agent 已升级并运行；真实 OPC Discovery、Session、Namespace 和目录 Browse 通过，3300 个变量、6 个 Method；发布清单已记录 |
| 58 | 定向修复 KYSEC 标签后，下级、Agent、管理端均已升级并运行；真实 OPC 与 Web 验收通过 |
| 46 | 下级、Agent、管理端均已升级并运行；真实 OPC 与 Web 验收通过 |

两台管理端部署前均 connected_sources=3、pending_commands=0、业务点 5297、计算规则 1997。58 管理端原有三个 Agent 绑定均为空，本轮不自动补配；46 已绑定三台 Agent。

## 58 阻塞证据与处理边界

58 第一次启动请求 `release-84896cc72e88427cbb4115a45a4183d8` 明确返回 failed。未重新提交该请求，也未重新执行原部署作业；旧 Agent 的 on-failure 策略曾自动重试，之后通过已获授权的服务停止结束重试。

启动报错：`无法获取单实例锁 ... Permission denied (os error 13)`。

系统审计直接记录 `KYSEC_KID denied { read }`，程序 `/opt/rcd/bin/rcd_server` 访问 `/var/lib/rcd/lower/rcd92-58/state/.rcd_server.lock` 被拒绝，sid=999、tid=9999，openat 返回 -13。文件普通权限为 lenovo:lenovo 0644；相同账号 Python 只打开/关闭文件成功。新程序可创建 config 目录中的锁，但无法打开带不同 KID 标签的旧 state 锁。没有证据表明第三方改动文件；先前目录迁移没有覆盖麒麟标签与新进程实际重启的兼容验收。

用户明确授权“现在处理掉就好了，现在是测试环境”后，先备份原标签，再仅将实例根、state 目录、运行锁、一个站点状态临时文件及命令库 WAL/SHM 的 KID 改为与可用 config 目录一致的 999，保留标签其他部分、文件内容、属主与权限。原标签备份：`/var/lib/rcd-release/rcd-full-20260928/kysec-labels-1790589363568460612.json`。未删除锁、绕过单实例保护或关闭 KYSEC。原生命令修改被拒绝后，验证失败即停止修复步骤；后续通过定向 xattr 修改并逐项核对成功。

修复后明确启动成功，随后升级 Agent 带来的再次重启也成功。原部署 unknown 记录不改写，独立恢复证据位于 `lower-recovery.json`。截至 18:04，58 服务 active/running、NRestarts=0；抽查审计最后的运行锁拒绝早于修复，未发现修复后新增同类拒绝。仍有物理站点 read_timeout，不等于软件崩溃或权限故障。

## 备份与作业记录

- 30 下级：`/var/lib/rcd-release/lower/artifacts/rcd-full-20260928-initial/backup-18efcccdfdc740e5b3accbd50c266748`。
- 30 Agent：`/var/lib/rcd-release/agent/artifacts/rcd-full-20260928-initial/backup-0225de2e46e34a6f803bddc1addc6dc6`。
- 58 下级：`/var/lib/rcd-release/lower/artifacts/rcd-full-20260928-initial/backup-8dc4438bedc4464b8f2bd8cb7eccc910`。
- 58 Agent：`/var/lib/rcd-release/agent/artifacts/rcd-full-20260928-initial/backup-a32b6255a16b4b4480a4d0c78a180bf1`。
- 58 管理端：`/var/lib/rcd-release/manager/artifacts/rcd-full-20260928-initial/backup-5dd7e810ea3e465d95516b9fbbc2aaf3`。
- 46 下级：`/var/lib/rcd-release/lower/artifacts/rcd-full-20260928-initial/backup-498dbe8389c248d5beeb705fcdf1226e`。
- 46 Agent：`/var/lib/rcd-release/agent/artifacts/rcd-full-20260928-initial/backup-086e7d8d8d0a48c1adf0ff016fb65d04`。
- 46 管理端：`/var/lib/rcd-release/manager/artifacts/rcd-full-20260928-initial/backup-bfe86688c4fd4d268a061a779dd27b5f`。
- 各目标发布 profile、预检和完成结果在 `/var/lib/rcd-release/rcd-full-20260928/`。
- 逐组件作业位于 `/var/lib/rcd-release/<component>/ansible-operations/rcd-full-20260928-<component>/operation.json`。58 原作业保留 unknown，不篡改为成功。
- 三台下级已生成 maintenance.sqlite，后续发布备份清单追加该数据库；没有启用或执行维护栅栏事务。

## 收尾验证（18:04）

- 三台实际运行下级、三台实际运行 Agent、两台实际运行管理端均逐一核对 `/proc/<pid>/exe` 与发布 SHA-256；每组件只有一个匹配进程。所有相关 systemd 服务 active/running、enabled、NRestarts=0。
- 两管理端均连接三个来源，pending=0、目录冲突=0；业务点 5297、中央规则 1997 与部署前一致。对三台下级执行真实 Browse；两管理端计算点 Read/Subscribe 成功。未发送硬件控制命令。
- 46 远端 Playwright 验证两个现网页面的版本总览、组件筛选与无 pageerror；截图已人工检查。46 展示 7 个在线新版组件；58 展示 4 个在线新版组件与 3 个原先未绑定的 Agent，未自动改变绑定。
- 逐一核对业务配置和 PKI 文件内容摘要：30/58 各 144 个下级文件，46 下级 100 个文件，两个管理端各 2 个 PKI 文件均与备份一致。三台下级审计/维护库、两管理端数据库 `PRAGMA quick_check` 均为 ok；管理端 user_version=15。
- Semaphore 已登记 `rcd-full-20260928` 并设为升级表单默认包，保留旧包和既有升级/回退模板；没有触发额外部署作业。目录备份 `releases-before-full-1790589771943973067.json`。
- 各机收尾证据：`/var/lib/rcd-release/rcd-full-20260928/final-check.json`。46 接口、OPC、浏览器证据位于构建隔离目录 `checks/`；本机只下载归档到 `artifacts/runs/2026-09-28-full-deploy/checks.tar.gz`，未在本机运行测试。

未验收：一小时/过夜稳定性、当前负载 P95 性能、指定物理设备控制动作；不能将本次启动与读订阅验收描述为这些长测已通过。现场配置、端口和安全策略未替换；58 定向标签修复已单独记录。下次麒麟目录迁移需补充标签一致性检查及实际新进程重启检查，不能仅凭普通文件权限判定可用。

## 30 管理端补装（后续明确授权）

用户明确要求最终拓扑为一套部署平台、三套 OPC 管理端、三套下级，随后授权处理。只读查明 30 旧 home 目录中的 `RcdManager` 是 Agent 共用二进制，未找到管理端数据库、自启动服务或 4840/8087 监听。因此这是新增独立实例，不是覆盖旧管理端。

- 使用同一已验证 `0.6.1-full.20260928` 二进制，安装 `/opt/rcd/bin/RcdManager`、数据 `/var/lib/rcd/manager/main/`、日志 `/var/log/rcd/manager/main/`，用户级 `rcd-manager.service` enabled；RCD93 已启用 linger。
- OPC `opc.tcp://192.168.10.30:4840`，Web `http://192.168.10.30:8087`。沿用此前明确指定的 Web 初始账号密码；未将凭据写进服务或发布记录。首次自动生成账户已在未配置业务、未使用的状态下备份，再由标准初始化流程完成指定账户设置。
- 30 新库、PKI 和管理实例独立生成，未复制 46/58 历史命令或实例身份。当前 groups/sources/calculation_versions 均为 0；已询问用户是否按 46 当前配置初始化，尚未得到确认。不可把软件部署完成表述为业务采集已配置。
- 保留原不可变发布清单，新增 `manager30-supplement.json` 和发布产物；安装/验收证据在 `/var/lib/rcd-release/rcd-full-20260928/manager30-install/`。新安装没有旧管理端二进制或业务库可回退。
- Semaphore 30 巡检清单、升级/回退组件选项、catalog/profile 均补充 manager；仍只有 46 上一套 Semaphore，未触发额外升级任务。平台修改备份 `/var/lib/semaphore/backups/manager30-1790590409868216512`。
- 46 远端运行 `tools/ansible/semaphore/test_topology.py` 三项测试通过；真实 `TestLiveManagerAnonymousNone` 检查固定命名空间通过；Playwright 检查新网页登录、版本总览、独立空配置、无 pageerror 通过。30 `/healthz`、`/readyz` 正常，实际映像摘要匹配，数据库 quick_check=ok。
- 本次只启停新装的 30 管理端以完成初始账户设置，没有重启三台下级、Agent 或 46/58 管理端，没有发送设备控制命令。
