# 显式目标发布工具（Linux）

本工具有实际停服务和文件替换能力，**只有得到当次部署授权才可使用 deploy / rollback**。
工具不默认批量升级主机；58 必须单独指定。2026-09-28 已使用配套 `migrate.py` 完成授权目录迁移；目标与证据见 [迁移记录](../../manager/docs/validation/standardization-path-migration.md)。

脚本在目标 Linux 主机上运行，检查 `--host` 与本机地址、profile 的主机和组件相同。
构建目录、运行目录、发布目录必须分离。PATH 由调用者明确配置 Go、Node 和 Rust 工具链；不安装系统工具、不写 SSH 密码。

独立串口同步程序使用第四组件`communicate`，不是`lower`。发布工具支持其源码摘要、独立Rust构建、`version --json`身份、标准单组件包及历史恢复；但当前没有活动通信排空证明，**仅支持已确认停止的服务安装/恢复，之后仍保持停止**。运行中会拒绝且不自动停止，不调用采集Agent或管理端socket。详见[包协议与安全边界](../../contracts/release-package/README.md#communicate-的发布边界)。

46现场路径不迁移：程序`/opt/rcd/bin/rcd_communicate`，用户级服务`rcd-communicate.service`（运行用户`lenovo`），运行数据`/var/lib/rcd/communicate/sync46`，配置目录`.../config`、PKI`.../pki`、状态`.../state`，日志`/var/log/rcd/communicate/sync46`，OPC端口4842。profile必须绑定这些独立路径，`databases=[]`、`config_dirs=["/var/lib/rcd/communicate/sync46/config"]`；发布只替换二进制，不迁移或覆盖配置/点表。30/58没有安装它时，巡检显示未安装，不借发布创建服务。

旧通信程序已有原始发布产物、源码校验清单及基线清单，但缺少平台版本登记时，可由维护人员使用`register_legacy_identity.py`补登。工具要求显式主机、当前PID及原始证据的固定SHA，校验全部源码和运行/安装/原始映像，仅原子新增只读版本档案；不替换程序、不执行程序、不创建成功部署记录或可恢复软件包。历史构建时间缺失时留空，不用登记时间冒充。此工具须使用Python 3.9以上，46执行路径为`/opt/semaphore/venv/bin/python3`；默认只核验，`--apply`才登记。补登后通过平台普通只读检查更新快照，不直接改平台数据库。46的`0.2.0`补登记录见[验收说明](../../manager/docs/validation/communication-version-registration-20261024.md)。

## 额外软件的标准包

平台先注册软件种类，再通过包页面上传新版本，不支持包内安装脚本、任意路径或shell。额外软件的版本取公开构建清单和实际映像SHA，不执行第三方`version`命令。

发布构建人员为已编译ELF准备公开元数据文件，例如：

```json
{
  "component": "weather-reader",
  "version": "1.0.0",
  "release_id": "weather-reader-20261021-1",
  "commit": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "source_sha256": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
  "dirty": false,
  "built_at": "2026-10-21T08:00:00Z",
  "target": "linux/amd64"
}
```

其中提交与来源摘要是格式示例，构建时必须填写实际值，不得伪造。生成包不会运行ELF：

```text
python3 tools/release/rcd_package.py --binary /absolute/build/weather-reader \
  --metadata /absolute/build/public-metadata.json \
  --notes "发布说明" --output /absolute/output/weather-reader.rcd.tar.gz
```

工具自动计算二进制SHA和大小、验证ELF平台，使用显式空`state_formats={}`；生成的包只能用于事先注册的软件标识。首次安装保持停止；已安装软件通过原生发布队列升级/历史恢复，只正常停机、不强杀，不声称设备事务排空。项目定义、宿主机ownership marker和profile必须一致，所有组件共用`/opt/rcd/bin`宿主发布锁。软件支持范围及注册接口见[包协议](../../contracts/release-package/README.md#注册额外软件)。

额外应用没有通用数据库schema检测器；空`state_formats`不是兼容证明。仅发布由构建方验证可读取当前数据、可安全二进制恢复的版本，破坏性数据格式变化必须另行迁移。历史恢复及自动恢复均保留当前配置和数据，不还原旧数据库。

示例 profile（占位路径，不能直接用于现场）：

```json
{
  "host": "192.168.10.46",
  "component": "manager",
  "source_dir": "/absolute/isolated-source",
  "release_dir": "/absolute/releases/manager46",
  "runtime_dir": "/absolute/manager-data",
  "executable": "/absolute/fixed-bin/RcdManager",
  "databases": ["/absolute/manager-data/manager.db"],
  "config_files": ["/absolute/manager-data/config.json"],
  "control": {"kind": "systemd-user", "unit": "rcd-manager.service"},
  "health_url": "http://127.0.0.1:8087/readyz",
  "opc_port": 4840
}
```

下级使用 `component=lower`，控制器改为
`{"kind":"process-agent","url":"http://127.0.0.1:8090","instance_id":"实际稳定实例ID"}`，OPC 端口 4841。
列出需要一致性备份的所有 SQLite 和配置文件；已存在的 `command-journal.sqlite`、`maintenance.sqlite` 必须列入数据库清单。工具检查 runtime 根和标准 state 子目录，遗漏时拒绝预检/备份，不把维护代次当缓存。其他布局仍须显式列出实际状态目录内全部数据库。
profile 属于环境配置，不提交真实路径、凭据或运行数据库。`runtime_dir` 必须包含全部数据库/配置；固定 EXE 可在另一个明确目录。

操作流程（每次都显式携带 `--profile ... --host ... --component ...`）：

1. `preflight`：核对运行 PID、实际 EXE、备份文件和目标主机，不停服务。
2. `build --base-commit <完整40位起点SHA> --record-dirty`：远端运行对应组件测试和构建；记录声明的起点提交、实际源码/依赖逐文件摘要、二进制 SHA-256 和工具链。脏快照不会被冒充为干净提交。构建期间源码变化则拒绝产物。管理端/Agent构建须同步完整`manager/web`源码（含未跟踪的新页面、测试和`public/help`图片），不能只复制本次修改文件；构建会拒绝缺少帮助路由或引用图片的快照，并以`RCD_VERIFY_WEB_HELP=1`校验Go实际内嵌的帮助分包、全部JPEG及HTTP响应。
3. `verify --artifact <build目录>`：校验目标和二进制摘要。
4. 建立控制维护窗口，确认所有关联管理端没有待执行命令；该检查不能只看一个管理端。
5. `deploy --artifact <build目录> --confirm-live-target <IP/组件> --confirm-no-active-commands`：停止指定组件、一致性备份 SQLite、备份 EXE/配置、原子替换、启动、检查 PID 的实际二进制摘要、readyz（若配置）和 OPC TCP 端口。
6. 失败时保存输出中的 `recovery_backup`。确认回滚范围后，使用 `rollback --artifact <backup目录>` 加同样的确认参数。默认先保存当前数据，再只回退程序，保留用户后来修改的配置和数据库。数据库 schema 与备份不同会保持停止并报错，不自动降级。只有另行确认覆盖配置/数据及可能损失的后续修改后，才增加 `--confirm-restore-backup-data`，不能盲目自动增加此参数。

“端口可连/readyz 正常”不是完整业务验收；仍需 Browse/Read/Subscribe、目录、真实命令目标及持续运行验收。
工具不会自动向设备发送报文，不会自动回滚或升级另一台管理端。备份失败可能使指定组件保持停止；此时不得盲目重复部署，应先读错误并确认恢复方案。

隔离工具测试：`python3 -m unittest discover -s tools/release -p 'test_*.py'`。

## 标准目录首次迁移

`migrate.py` 是一次性、单目标复制迁移入口，不是重复执行的配置管理工具。支持 `preflight/install`，必须明确主机、角色 `lower-stack/manager`、现有服务 scope/unit、运行账户、源目录、构建摘要和固定启动参数。`lower-stack` 明确包含 agent 与其 child；不是只更新下级而不重启 agent。

在目标 Linux 主机以 root 执行，用户级服务通过原运行账号管理，系统级服务直接管理；不新建账号、不修改串口权限。profile 保存于私有部署目录，不提交现场配置。

- `preflight`：校验唯一服务进程、旧运行映像和入口、目标目录不存在、足够空间、构建摘要、agent 无 pending。
- `install --confirm-no-active-commands`：要求当次人工控制暂停；备份实际运行程序，停止并确认 child 退出，复制配置/状态/PKI/日志并逐文件核对、检查 SQLite、安装普通文件入口，增加原服务 `90-rcd-paths.conf`，启动并核对运行摘要和端口。
- 对旧共享入口的 agent，可用 profile 的 `old_running_executable` 指定已核实的旧运行路径，不放宽到任意同名进程。
- 新目录已存在、源文件复制过程中变化、符号链接/特殊文件、空间不足、停止未确认、备份错误均停止操作。不覆盖目标目录、不合并配置、不自动强杀。
- `/var/backups/rcd/<deployment_id>/<role>/record.json` 持续保存阶段、源文件摘要、运行映像/入口摘要、工具摘要和结果。失败后检查记录，不盲目重跑。
- 原目录仍保留但不再使用；禁止再次启动旧下级争用串口。配置维护改用新目录或 Web。

迁移工具没有自动撤销到旧目录的命令。恢复前须停止写入并备份**新目录当前数据**，确认 schema 兼容，再将当前数据带回选定布局并恢复服务；不能仅删除 drop-in 后启动旧目录，否则可能回到过期配置。数据覆盖需要明确恢复范围授权。

`inspect_runtime.py` 是不启停服务的版本核对工具，[Ansible 编排](../ansible/README.md)复用它，并通过 `orchestration.py` 调用原发布/恢复事务。适配器只增加跨主机暂存和防重放记录，不复制启停实现。构建主机原清单保留，目标清单单独绑定 host/profile；下级目标不需要源码或编译器。部署锁只串行化迁移工具，不是设备命令维护栅栏。

## 底层维护入口：下级带维护栅栏发布

当前日常发布使用 [Semaphore 原生平台](../ansible/semaphore/README.md) 和 `platform_release.py`，已补充管理端排空、Agent 自身升级、兼容性检查与安全失败恢复。下文描述的是底层旧维护入口自身的限定范围，不能将其“只支持下级”等限制误解成原生平台也不支持管理端或 Agent。独立工具依然需要明确目标和当次授权。

只用于 `component=lower` + `process-agent`，不隐式套用到 agent 自身或管理端。旧下级仍使用原人工窗口流程；新接口不支持、超时或身份不明时**不自动降级**。仍要求当次目标授权和人工暂停确认。

profile 除前述字段外，必须明确 `gateway_instance_id` 和 `maintenance_endpoint`（例如 `opc.tcp://127.0.0.1:4841`），与 agent 本地启动绑定一致。若 agent 工作目录与数据根不同，额外指定绝对 `work_dir`；这不是修改下级配置的接口。首次进入前就检查 agent 的固定绑定。

在常规 `deploy` 参数上加：

```text
--maintenance --deployment-id <本次唯一作业ID> --initiator <操作者标签>
```

作业 ID 为 1–96 位 ASCII 字母、数字、点、横线、下划线，首位为字母/数字，不复用。操作者标签用于审计，不视为身份认证。产物必须有记录的软件版本。

流程：只读预检/产物校验 → 持久登记 → 下级进入维护/排空 → 带 token 停止 → 一致性备份 → 再校验产物并原子替换 → 带 token 启动 → 运行映像摘要/端口验证 → 真实 OPC 身份、代次、持久栅栏核对 → 解除维护并确认。

- `<release_dir>/deployments/<id>/deployment.json` 保存目标、profile/manifest/二进制摘要、作业、版本及阶段意图/时间/确认结果；网络修改前先保存意图。错误只记录异常类型，不保存任意远端错误正文、环境变量或配置内容。
- 固定 EXE 同目录的 `.<EXE>.release.lock` 串行化工具；`.<EXE>.release-active.json` 保留未完成标记，换一个发布目录/编号也不能绕过它。不是 root/SSH/其它程序的操作系统级锁；不要删除它来绕过失败。
- 日志、备份与原子替换使用 fsync；备份失败、磁盘错误、回复丢失、CAS 冲突、证据不符均停止后续流程，保留维护，不自动重发 enter/start/stop/complete，也不自动还原旧数据。
- 成功仅表示所列**软件发布检查**完成，报告明确 `business_acceptance=not_performed`，不声称串口设备健康、完整点表/客户端/性能验收通过。

中断后先运行（只读，不需要部署确认开关）：

```text
python3 tools/release/release.py inspect-deployment \
  --profile <同一profile> --host <同一IP> --component lower \
  --deployment-id <原作业ID>
```

它比对原目标/profile，展示阶段记录、当前入口摘要、agent 缓存状态和既定 stop/start 编号的查询结果。不会发 reconcile（该动作会更新 agent 记录）、不会启停、不会解除维护。记录中的 agent 证据仍是缓存，不能当新鲜的下级 OPC 证明。404/请求失败不等于操作没执行。profile 漂移会拒绝，运行配置内容独立可编辑，不纳入 profile 摘要。

**没有自动续跑或自动回滚。** 未完成标记会阻止普通 deploy/rollback，不能换 ID 再试。已确认停止但未发起启动的作业可显式 resume，或按下文限定条件回退程序；安装之后且原启动请求已成功的作业可显式 finish。准入未确认、原请求仍 unknown 等情况仍须制定恢复方案。成功的作业 ID 不可重用。

### 已停止作业的显式继续

先只读 inspect，核对原作业，再取得本次恢复目标和控制暂停授权：

```text
python3 tools/release/release.py resume-deployment \
  --profile <原profile> --host <原IP> --component lower \
  --deployment-id <原作业ID> --initiator <本次操作者> \
  --confirm-live-target <原IP/lower> --confirm-no-active-commands
```

它可能新建备份、替换程序并首次 start，但不会重发 stop、不会恢复配置/数据库、不会解除维护。

- 只接受原 stop 编号明确 succeeded、当前 stopped/PID=0/无 pending，以及 agent 仍持有同一持久/排空栅栏的作业。停止时 OPC 不可读，证据明确标为 `agent_cached_confirmed`；启动后仍必须通过 finish 获取真实 OPC 证据。
- 新发布日志记录安装前二进制摘要。旧日志缺此字段时拒绝早期恢复，不猜测当前文件是不是原版本；仍可按原条件使用 finish。
- 备份失败可重新建一份唯一目录，不覆盖先前不完整备份。已确认备份必须完整、摘要正确；损坏则拒绝。用户当前配置在备份中保留，不还原旧配置。
- 安装中断时仅允许入口仍是原二进制或目标二进制；已是目标则只确认，不重复复制。其它文件、符号链接、产物变化均拒绝。
- `resume_checkpoints` 追加恢复确认，不改写原阶段历史。首次 start 前先持久记录标准请求编号和意图；之后即使没有收到结果也禁止 resume 再发 start，只能 inspect/finish 或制定人工恢复方案。404 仍不证明未执行。
- 命令成功返回 `awaiting_finish`，维护仍保留。随后显式执行 `finish-deployment` 重新验收并收尾；不是看到新 PID 就解除维护。

验证范围见[已停止作业恢复记录](../../manager/docs/validation/stopped-resume-validation.md)。

### 已启动作业的显式收尾

先执行 `inspect-deployment` 查看记录。确认本次恢复目标和控制暂停窗口后，使用原 profile、原作业 ID：

```text
python3 tools/release/release.py finish-deployment \
  --profile <原profile> --host <原IP> --component lower \
  --deployment-id <原作业ID> --initiator <本次操作者> \
  --confirm-live-target <原IP/lower> --confirm-no-active-commands
```

这是**有副作用的显式恢复**，不是只读巡检；可能更新 agent 的核对记录，以及首次发起尚未请求过的维护解除。它绝不发送 enter/start/stop/restart、不替换程序、不恢复数据。

- 要求原日志已经确认安装、记录启动请求编号；当前 agent 必须确认原编号为 succeeded，当前程序运行且无 pending。404、unknown、已停止均拒绝，不能仅凭“看到 PID”推断原请求成功。
- 核对同一 profile/marker/作业/token、发布包和 manifest 摘要、安装/运行映像、完整备份清单及摘要。不会把当前用户配置与旧备份比较，也不会还原旧配置。
- 每次恢复单独追加 `recovery_attempts`，保留原阶段的 unknown，不涂改旧历史。恢复步骤仍先写 intent，最多保留本作业内 100 次尝试；反复失败要处理原因，不循环碰运气。
- 重新验收软件，再显式 reconcile。开发版 agent 即使缓存阶段为 completed，也会重新读下级 OPC；读取失败保持 unknown，不能用旧成功记录放行。
- 若确认维护已经解除，只补齐发布成功记录，不再次 complete；若原作业从未请求 complete 且当前仍排空，允许第一次 complete。任何历史 complete 意图（包括恢复尝试）存在但解除仍无法确认时，拒绝重发。
- 备份/停止/安装阶段失败、原启动状态缺失、下级停止、agent 自身升级、协调回滚仍不由此入口处理。不能将它理解为自动断点续跑。

测试范围与未完成项见[收尾恢复验证](../../manager/docs/validation/release-recovery-validation.md)。

### 状态回滚保护

#### 新版从未尝试启动时：只回退程序

只针对同一带维护栅栏发布作业：原 stop 确认成功、完整备份已保存、尚无任何新版 start 意图、下级当前 stopped 且持有原已确认 token。新版已经运行过或仅仅出现过 start 意图，都不接受本入口。

```text
python3 tools/release/release.py rollback-deployment \
  --profile <原profile> --host <原IP> --component lower \
  --deployment-id <原作业ID> --initiator <本次操作者> \
  --confirm-live-target <原IP/lower> --confirm-no-active-commands
```

- 校验备份内旧程序与发布前摘要相同，不只是相信备份自己提供的摘要；原程序就是进入本次维护时使用的程序。
- 检查当前 SQLite 完整性和 schema 与停止时备份一致。只证明本入口限定的“新版未启动”场景，不推断任意旧版本能读当前数据。差异或损坏时拒绝，绝不自动还原数据库。
- 另建一份回滚前的当前程序/配置/数据备份，再恢复旧程序。使用人最新配置、数据及 maintenance.sqlite 代次全部保留，不复制旧业务文件回来。
- 写入 rollback 意图后禁止原作业 resume/finish 升级路径；使用独立 `<deployment_id>.rollback-start`，不会借原新版 start 编号启动旧程序。
- 只首次启动，成功后仍保留维护；随后以相同目标/授权参数执行 `finish-rollback`。它核对回滚原请求、旧程序实际运行摘要和新鲜 OPC 证据，才收尾。成功报告 `outcome=rolled_back`，不能当新版本部署成功。
- 回滚启动或收尾回复丢失只补查，不重复发送。rollback 意图已保存而文件替换/启动前出错时保留停止/维护，可按下节限定条件显式继续；不能删除意图重试。

测试和限制见[启动前回滚验证](../../manager/docs/validation/prestart-rollback-validation.md)。

#### 回滚已登记、旧程序尚无启动意图：显式继续回滚

先只读 `inspect-deployment`，取得本次恢复目标及控制暂停授权，再使用原 profile 和作业编号：

```text
python3 tools/release/release.py resume-rollback \
  --profile <原profile> --host <原IP> --component lower \
  --deployment-id <原作业ID> --initiator <本次操作者> \
  --confirm-live-target <原IP/lower> --confirm-no-active-commands
```

- 仅支持原 stop 明确成功、当前 stopped/PID=0/无 pending、维护身份与代次仍一致，并且新版和回滚旧版都从未记录启动意图。不是“看不到进程就可以再启动”。
- 校验原发布包、旧程序备份、首次回滚保护副本、当前 schema；入口只能是记录的旧/新程序。损坏、错绑、未知 stop 或 schema 变化直接拒绝。
- 用户在中断后修改的配置照样保留，并再次保存独立的当前数据备份。原备份和阶段记录不覆盖；失败尝试留在 `recovery_attempts`。入口已经是旧程序时不重复复制。
- 首次启动仍只使用原 `<id>.rollback-start`。即使启动意图刚落盘、POST 尚未发出就退出，也不能再次 resume；回复丢失只能 inspect/finish-rollback，不能换编号重投。
- 成功返回 `awaiting_rollback_finish`，尚未解除维护；另行显式 `finish-rollback` 验收。确认过的继续回滚备份在启动前与收尾时重新校验，不以损坏副本作为成功依据。

此入口只收窄地补齐“回滚登记后、首次启动前”的恢复，不支持新版运行后的通用降级、数据库恢复或 agent 自身升级。验证见[继续回滚第八批](../../manager/docs/validation/resume-rollback-validation.md)。

#### 已经运行新版、agent 状态及数据恢复

- agent `process-state.json` 必须显式纳入备份；现有 schema v2（含维护作业）不允许走旧的普通 agent 部署/回滚路径，即使最近作业已经完成，也要另做兼容检查。
- `maintenance.sqlite` 必须备份，但工具禁止用 `--confirm-restore-backup-data` 恢复旧维护代次。授权恢复业务数据不代表可以回退栅栏 token；不能删除维护字段或库让旧程序启动。
- 本批未解决 agent 自身升级、复杂目录迁移恢复及新版已经运行后的兼容性回退。版本一致性、原配置保留和设备故障展示仍遵循原边界，不擅自修改用户业务配置。

实测范围及证据见[发布维护验证记录](../../manager/docs/validation/release-maintenance-validation.md)。
