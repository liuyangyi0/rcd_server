# 统一管理实施记录

目标：Semaphore/Ansible 管理管理端服务和软件发布；管理端经 Agent 安全启停固定下级。机器台账共享，业务配置不复制。

本轮 2026-10-09 先完成 46 隔离验证，随后取得当次明确授权：“已暂停控制，可以接入部署和启停验收”。平台及三机已按 30 → 58 → 46 接入，现场启停行为验收完成；13:15 开始只读稳定性观察。已通知恢复控制，维护窗口结束；未提交或推送源码。

## 交付检查表

- [x] Agent 安全生命周期：持久作业、排空、共享发布锁、停止保留、未知结果不重放。
- [x] 管理端过程客户端与 Web：能力检查、安全操作、占用者及阶段。
- [x] Semaphore 独立服务控制任务及页面：管理端启停、Agent 服务恢复。
- [x] 台账、身份验证、管理端缓存同步与页面跳转。
- [x] 隔离 Go/Python/Web 测试、race、浏览器与真实 Rust/systemd 握手（范围见下）。
- [x] 当次授权后按 30 → 58 → 46 现场部署、台账与固定身份绑定。
- [x] 现场管理端安全启停、九组下级控制及页面验收。
- [ ] 一小时观察、过夜结果。

该列表是完成证据索引，不以计划代替已实现或已测试结果。

## 实现及入口

- Agent：`manager/internal/processagent/lifecycle.go`，HTTP `/api/v1/lifecycle/`。Linux 下可用；Windows 无共享 flock，不宣称已支持。普通远程启停不能回退到旧的无保护 `/process/actions`。
- 平台：官方 2.19.16 上的原生定制源码，开发标识 `v2.19.16-rcd.3`。设备详情的管理端组件新增启动、停止、重启；Agent 只提供已停止服务的恢复；下级仅引导至业务管理网页。操作任务能按软件发布/服务控制筛选。
- 执行器：`control-platform.yml` → `service_control.py` → 原发布器、systemd/本机维护接口，共用原生队列和主机锁。没有包也可启停，不通过 HTTP 执行 SSH。
- 台账：平台设备“管理机器绑定”，保留现有 ID，核对 Agent/下级身份及检查得到的管理端身份；固定 IP 不随显示名称修改。三个管理端轮询原生 `/rcd/fleet`，缓存绑定和软件快照；地址与身份不匹配或有未完成作业时拒绝更新。
- 管理网页：来源进程页显示人工停止、运维作业的原发起者/阶段、台账缓存及平台跳转。平台软件快照明确不是实时远程探测，停止进程不展示磁盘版本为运行版本。

协议详见 [安全生命周期 v1](../../contracts/process-lifecycle/README.md)。接入步骤见 [部署与操作说明](../../manager/docs/deployment/unified-management.md)。

## 46 隔离验证证据

根目录：`/home/lenovo/code/rcd_unified_20261009_iuB7vj`。所有构建、测试及浏览器在此目录执行；没有在本机构建。下表为部署前的隔离结果，不冒充现场结果。

| 验证 | 结果与证据 |
| --- | --- |
| 管理端 `go test ./...` | 通过，`phase6-go-all.log` |
| `-race` processagent/app/store/opc | 通过，`phase6-go-race.log` |
| Semaphore services/rcd、db/sql、api/projects、services/tasks race | 通过，`phase7-semaphore-race.log` |
| 发布与运维 Python 自动测试 | 177 项，175 通过、2 项显式远端实测入口跳过，`phase9-python.log`；两个入口另行执行见下 |
| 平台接入/快照工具 | 11 通过，`phase7-platform-python.log` |
| Web 回归 | 过程页/API/台账 77 项通过；新增软件快照状态用例后单独复测台账 5 项通过，`phase7-manager-web-test.log`、`phase9-fleet-test.log` |
| 真实 Rust / Go / Python / Ansible | 通过，`phase8-rust-ansible.log`。三个管理端身份各 stop/start/restart；丢失回复不重投；原发布工具兼容。无 CSV/串口，不能算真实现场九组验收 |
| 真实 systemd 管理端启停 | 通过，`phase9-systemd.log`。唯一临时单元验证停止、普通启动受阻、明确启动、重启、身份不变和重复编号不重启；测试后停用并移除临时单元 |
| 真实 Agent 停止升级 | 单独通过，`phase8-systemd.log` 中第二项：实际二进制升级，停止状态及持久文件不变；该日志第一项当时失败，修复后另有 phase9 证据，不将整份 phase8 写成通过 |
| 浏览器 | 原生 Semaphore 3 组流程和管理端 10 组状态检查通过，`phase9-semaphore-browser.log` / `phase9-manager-browser.log`，1366/1920 布局；请求故障用例为隔离响应样例，不执行现场作业 |
| 完整管理 Web 回归与记录构建 | 35 文件、238 项通过，`phase13-web-all.log`；`phase13-manager-recorded-build.log` 另完成同套前端测试、Web 构建、Go 全包测试及带源码摘要的发布构建 |
| 原生 Ansible 服务任务 | `phase10-systemd-ansible.log`：真实 `control-platform.yml` → Python → systemd/管理端维护接口通过 |
| 首次接入协调器 | `phase13-bootstrap.log`：真实 Rust/Agent/管理端/systemd，原维护身份排空恢复，配置不变，移除旧管理端对 Agent 的依赖，重复接入编号不重放 |
| 平台只读账号 | `phase13-readers.log`：三个原生 guest 账号、私有令牌与重复登记检查通过，不建立第二套认证 |

本轮测试发现并修复：作业完成时状态与 flock 释放竞争；旧 Ansible 工具包漏发 `release_probe.py`；systemd `ConditionPathExists` 的错误引号导致条件被忽略。后一问题通过真实 systemd 复现与修复，不只是字符串断言。修正后的安装器仅接受固定普通绝对路径，执行器拒绝被错误引用的保护条件。

隔离回归另发现：原有 `95-rcd-maintenance.conf` 会覆盖过早的 Agent drop-in，改用 `zz-rcd-unified.conf`；systemd 不能通过空 drop-in 重置依赖，首次接入改为备份并只移除实际声明中的指定 Agent 依赖，不动其他依赖。

## 2026-10-09 现场接入

- Semaphore：`v2.19.16-rcd.3-f8cefe62-2026-10-09`，实际程序 SHA-256 `fe4f05b53ca5cd099a196e37b9b637f8c4ff3cf72e99e5aebfc74c6a1c6cb4ec`。配套备份 `/var/lib/semaphore/backups/native-platform-1791520012245247258`，包含一致性数据库、加密配置、软件包及原程序。
- 三机管理端 / Agent：`0.6.3-unified.20261009.1`，SHA-256 `b88e9570d9ebb2f0d7529f04da8b3be3c0a2ce624a8ad29ac584da98b5b34d39`。Rust 下级仍为 `0.6.2-ops.20261008.1`，没有替换 Rust 二进制或业务配置。
- 原生升级任务：30 Agent / manager `3365 / 3372`；58 `3385 / 3389`；46 `3396 / 3401`，全部已成功，逐项验收实际映像。
- 首次安全接入作业为 `unified-wiring-20261009-{30,58,46}`，全部成功。各机原始备份和阶段记录在 `/var/lib/rcd-release-platform/artifacts/agent/deployments/<该作业编号>/`。不是日常启停入口，不能重复执行已有作业。
- 三机保留原 source / device / Agent / gateway / manager 稳定 ID。平台原生只读账号分别供三个管理端使用；令牌为服务用户所有的 `0600` 文件，不写 Git、命令行或报告。
- 30、58、46 的 `/api/v1/fleet` 已确认三条来源均无绑定错误。Agent 均开放固定三管理端地址的安全生命周期入口；底层发布维护写接口继续仅允许本机。
- 46 管理端实际 `Wants` 仅保留 `network-online.target`，不再依赖 Agent；原声明文件已备份。端口、运行数据路径与 systemd 单元名称保持不变。

现场证据在上述 46 根目录 `live-unified/` 和 `live-browser/`；30 接入目录 `/home/RCD93/code/rcd_unified_20261009_5rgqWC`，58 为 `/home/lenovo/code/rcd_unified_20261009_u3NS1o`。现场启停行为验收完成；一小时及过夜尚未通过，主机断电/重启不以 systemd 条件测试替代。

### 现场验收进度与 KYSEC 修复

- 三管理端各停止、启动、重启均成功：30 任务 `3411/3416/3426`，58 `3431/3436/3441`，46 `3445/3449/3456`。检查实际 PID、下级与 Agent 不被连带停止；人为停止后普通 systemd 启动被条件保护拒绝，明确启动才恢复。
- 30 下级已完成来自三个管理端的 stop/start/restart 九项。30 Agent 停止后，三个管理端均返回明确不可达，平台任务 `3464` 恢复 Agent；下级仍保持人为停止，随后经明确启动恢复。
- 真实浏览器无接口模拟：平台重启 30 管理端、46 管理网页重启 30 下级；1366/1920 布局、确认与执行结果通过。证据 `live-browser/result.json`、`live-unified/browser-acceptance.json`，不将其余隔离状态样例冒充现场操作。
- 58 首次停止在打开共享锁时被 KYSEC 拒绝（Agent sid=999、锁 tid=9999），返回失败且未停止进程；不是锁竞争或采集崩溃。原失败作业 `unified-m58-l58-stop-2701aca62eee4ddf` 保留，没有重投。
- 用户本次明确授权定点标签修正。工具 `repair_unified_kysec.py` 持有原锁并备份，仅修正新增共享锁、共享发布标记和控制目录三个路径的 KID；保留 inode、内容、Unix 权限及标签其他部分，不关闭 KYSEC。原标签备份位于 58 接入目录 `kysec-shared-labels-before.json`。修复后新作业 `unified-m58-l58-stop-c2ac36c45e324d00` 成功。
- 发布器原子更新共享事务标记时保留已配置的 KID，不复制目录的可执行信任标志；标签写入失败则保留旧标记并终止。46 隔离回归 55 项通过，`phase14-kysec-tests.log`。原生队列切换到新不可变工具快照 `/opt/semaphore/rcd-platform/tools-28c850ffbf2bbabf82d8`，未原地改写正在使用的工具。
- 随后 58、46 下级也各完成来自三管理端的 stop/start/restart；共九组、27 项成功，逐项记录原请求、结果、实际 PID 和未涉及组件的稳定性。未发送设备控制报文。58 / 46 的 Agent 服务恢复任务 `3497 / 3504` 均成功，保持下级人为停止后再显式启动；跨机管理端 PID 在下级测试全过程保持不变。
- 发布工具完整回归 `phase14-release-all.log`：183 项、180 通过、3 个显式隔离实测入口跳过；对应真实入口已有上文独立日志，不把跳过记作本次执行通过。
- Semaphore 实际停止时，三个管理端均显示平台同步失败但保留三条有效绑定，业务 PID 不变；58 管理端继续通过 Agent 重启 46 下级成功（`unified-m58-l46-restart-b5a258703a994888`）。恢复平台后同步恢复，原任务没有重新执行，定时检查正常生成新快照。证据 `platform-offline-before.json`、`platform-offline-cache.json`、`platform-restored-fleet.json`。
- 最终六个 OPC 入口 Browse/Read/Subscribe 全部通过并收到业务点通知，Write/Call 均为零，`opc-readonly-final.jsonl`。30 管理端现有业务目录为 3300，58/46 为 5297，未为统一机器台账而复制计算配置。部分采集样本为 `BadNoCommunication`，不将既有物理站点通信故障说成软件发布失败，也不把端点订阅通过说成所有站点正常。

### 稳定性观察（待满时复核）

三机只读采样分别自北京时间 2026-10-09 13:15:13（30/58）、13:15:21（46）开始，各接入目录 `observation.jsonl`。首条 `issues=[]`、`health_warnings={}`，只能证明当时状态，不代表一小时/过夜通过。临时 `rcd-unified-observe-20261009.service` 每分钟检查，24 小时退出，无重启业务或控制写入能力。

观察内容包括实际进程映像/版本、PID、systemd 自动重启计数、HTTP 健康正文、OPC 端口、Agent 持久意愿及平台版本快照。46 采样还核对平台实际映像；只读探测先修正了固定地址监听和 Python 3.8 纳秒时间解析，原失败探测文件保留，不计入正式区间。

后续首次三台完整日志跨度均超过一小时后复核一次；2026-10-10 北京时间 09:00 后核查过夜。维护授权已结束，后续只读；新异常需报告，不自动重启、重投或修改配置。

## 上线后的对抗审查修复（已部署，长测待完成）

2026-10-09 针对六项审查发现完成修复，并通过跨组件回归补齐 Agent 终态快照和慢历史查询隔离问题。另获本次控制暂停授权后，平台更新为 `v2.19.16-rcd.4`，三台管理端/Agent 更新为 `0.6.3-unified.20261009.2`；按 30 → 58 → 46 通过网页完成六个原生升级任务 `3677/3681/3683/3687/3691/3695`。Rust 程序保持原版，未改业务配置、端口或采集行为。实际进程摘要、九条运维连接、健康检查和六个 OPC Browse/Read/Subscribe 通过，未发硬件控制报文。

源码、测试、发布摘要/备份及浏览器证据见 [对抗审查修复记录](../../manager/docs/validation/adversarial-review-fixes-20261009.md)。最后维护于 14:15:04 完成，以 14:16 后建立新观察窗口；13:15 旧观察窗口不能证明本版一小时稳定性。原只读观察服务与日志保留，14:18 基线文件位于 `/home/lenovo/code/rcd_review_fixes_20261009_Vt0AZp/postdeploy-observation-baseline.json`。已通知恢复控制，后续无启停、发布或控制授权。长测尚未完成，不以本次瞬时核验代替过夜结论。

2026-10-09 15:27 首次一小时复核已完成，但**整体未通过**：三机新窗口各 68 个样本、约 67 分钟，实际进程 PID/SHA 和 NRestarts 与新基线一致，HTTP/OPC 端口、平台队列及快照新鲜度正常；46/58 出现三条短暂命令追踪告警，进一步查到九条来源连接的重连计数均增长，下级 30/58/46 在 14:16–15:24 分别记录 53/35/6 条 `BadSequenceNumberInvalid`。这是 OPC 软件连接问题，不能作为物理站点故障排除；根因尚未定位，也未证明由本次版本引入。明细见上述修复记录。未擅自修改/重启；自动验收暂停等待用户决定修复，原 24 小时日志服务保留自然退出，过夜尚未验收。
