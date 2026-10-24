# 统一版本、目录迁移与只读编排验收

日期：2026-09-28。用户本次明确授权部署、目录搬迁、现场重启，并确认暂停全部控制命令。操作完成后已通知可结束控制暂停。未提交、未推送 Git，未修改用户业务配置、串口时序或 OPC 安全策略，未发送人工设备控制命令。

## 发布与目录

| 主机 | 实际升级角色 | 服务 / 运行账号 | 完成时间（北京时间） |
| --- | --- | --- | --- |
| 192.168.10.30 | Rust 下级、agent | 系统级 rcd-lower / RCD93 | 12:36 |
| 192.168.10.58 | Rust 下级、agent；Go 管理端 | 系统级 rcd-lower、用户级 rcd-manager / lenovo | 12:37 |
| 192.168.10.46 | Rust 下级、agent；Go 管理端 | 用户级 rcd-process-agent@real-lower、rcd-manager / lenovo | 12:38 |

三台下级始终只有 agent 一个进程拥有者；30/58 的系统级服务原本也是启动 agent，并非另一个直接采集实例。保留原服务名、运行用户和 enabled 状态。46/58 管理端分开停止启动，未同时停机。

- 版本：`0.6.0-paths.20260928`；release_id：`rcd-standard-paths-20260928`。
- 起点提交：`769ef2a1f82e6e7bb5e77f9e79a3e88434005e42`，`dirty=true`；不是声称已有改动已提交。
- 构建源码摘要：`93a325c1c6c8fe864bb0563772cbc136dcfeb3690ffe1d7b2cf1deb1e8a1f9bc`。
- Rust release SHA-256：`4c321d9826f2dff3d3340edfa78f8414c27dfcde8000b07f14811a59bab29e64`。
- Go SHA-256：`00ef1343261e85d76e17df41ad46af41685d2c6d9476dc0d4c1635b6b116bb9f`；manager/agent 使用相同构建的两个独立入口文件。
- 二进制正式包摘要：`385f3de9f8eba2fd78dc5a18d92b8f937d09239f52aedf087f5a4295305c6bcd`。运行程序与清单一致，不是测试用 Rust debug 包。

程序统一在 `/opt/rcd/bin/`；完整配置、状态、PKI、日志、服务文件路径见 [标准路径说明](../deployment/version-and-paths.md)。管理 OPC 4840、下级 4841、Web 8087、原 agent 地址及 50 ms 下级订阅参数保持。

## 改动与保护

- agent 支持本地固定的下级 config/state/pki/log 参数和自身 log-dir，HTTP 不能设置任意参数；旧部署未传参时保持旧默认。
- release 工具支持系统级服务；默认回滚只换软件并保存当前用户数据，schema 不匹配拒绝自动降级。显式恢复备份数据需要额外确认。
- 新迁移工具在原服务上增加独立 drop-in，不新建第二套服务；先停止并确认 child 退出，再复制校验配置/状态/证书/日志，验证 SQLite 后启动。新目录存在或复制期间有外部修改则拒绝覆盖。
- 46 旧 agent 运行于早期软链接解析后的版本文件，而磁盘共享入口已经被管理端更新。预检阻止直接迁移后，核实实际路径，分别备份运行映像与磁盘入口，再继续。此处只修正迁移工具和显式 profile，没有修改已构建的应用；每台部署 record 另记录实际迁移工具摘要。发布包内初版迁移脚本因此不是最终现场工具，不能盲目重用该旧脚本。
- 原 home/code 运行目录全部保留；新目录才是当前生效目录。没有清理原日志或历史包。

## 验证

构建、单元测试、race、Web 测试及 OPC 验收客户端全部在 46 的隔离目录执行。30/58 仅安装经过校验的产物、执行服务操作和只读软件状态核对。

| 项目 | 实际结果 |
| --- | --- |
| Rust `cargo test --locked --lib --bin rcd_server --test runtime_identity_cli -j2` | 385 通过、1 个原有用例忽略 |
| Go `go test -race ./...` | 全包通过 |
| Web `npm ci`、`npm test`、`npm run build` | 30 文件、185 测试通过，正式构建成功并嵌入 Go |
| Python release/migration/inspection | 最终 17 项通过；覆盖保留数据回滚、schema 拒绝、复制中修改、拒绝符号链接、SQLite、运行/磁盘版本分离及仅同步服务路径字段 |
| 46 隔离真实 systemd 迁移演练 | 旧目录 → 标准目录成功，用户文件与旧目录保留；使用空管理端、14840/18087，不占现场串口；测试服务已停止 |
| 三台真实下级 OPC | 逐台迁移后 Discovery / Session / Namespace / 完整旧业务目录 Browse 通过 |
| 两台真实管理 OPC | NamespaceArray、计算元数据、只读类型/访问级别、Read 与 Subscribe 初值通过；未执行 Write/Call 控制 |
| Web/API | 登录、内嵌首页资源、来源、计算、日志查询通过；不是完整浏览器视觉回归 |
| 软件运行状态 | 12:52 核对所有目标 active/enabled，NRestarts=0，快速日志扫描未命中 panic/core dump 等崩溃标记 |
| 下一批 Ansible 只读巡检 | syntax-check 通过；从 46 对 30/46/58 跑通，均 ok=3 changed=0 failed=0；缺少 --limit 在执行巡检前拒绝 |

两台管理端均为 3 个已连接来源、5297 个业务点、1997 条计算规则、0 目录冲突，采样检查时 pending=0。计算点 `11UBN04111_Fire` 在两管理端均可读取并取得订阅初值。没有把物理站点在线率当作软件升级门槛。

复制后的配置/证书与当次备份逐文件核对：30 为 141 个 TOML/CSV（含配置备份）及 3 个 PKI 文件，46 为 96 及 4，58 为 141 及 3。三台 gateway identity、两个 manager identity 保持；管理 DB schema 14、下级命令库 schema 1，完整性检查通过。manager 来源/组/计算定义保持独立，不复制 46 数据库到 58。

46 的 station_versions 在重启自动查询后更新了动态 revision/时间；不是迁移丢失。校验保留了站点拓扑身份和人工配置类型，而非要求动态结果永远不变。两台其他下级的对应状态未变。业务配置内容没有改动。

46 中央日志继续收到三来源推送，历史文件已迁移；58 的中央推送记录为空，本轮保留原推送目标，未擅自改业务配置或宣称两管理端日志自动同步。

## 证据与恢复

- 构建源：46 `/home/lenovo/code/rcd_standardization_20260928_jFl8UD`；产物/构建日志：`/home/lenovo/code/rcd_path_release_20260928_v2`。
- OPC/API/Ansible 证据：46 `/home/lenovo/code/rcd_path_checks_20260928/`。
- 每台一致性备份：`/var/backups/rcd/paths-20260928-30|46|58/<lower-stack|manager>/`，含原运行映像、入口、unit、配置/状态/PKI/日志以及 record.json；主机目录内 verification.json 保存核对结果。
- 下一批隔离源码：46 `/home/lenovo/code/rcd_inspection_20260928`；Ansible 工具环境：`/home/lenovo/code/rcd_ansible_tools_20260928`，Python 3.13.15 + ansible-core 2.19.13，未替换系统 Python。
- 本地取回资料：`artifacts/runs/2026-09-28-path-migration/`，不加入现场配置/数据库正文到 Git。

收尾时同步了 46 服务环境文件中四个残留旧路径项，保留密码和其它参数；其它两台没有这些旧路径项。当前进程原本已由显式 CLI 使用新路径，本次环境说明同步未重启程序，另存 environment-path-normalization.json 和私有原文件。工具测试及正文链接、仓库原行尾策略下的 `git diff --check` 通过。

不能直接删除 drop-in 后启动旧目录：新目录可能已有用户修改和新审计，须先保全当前数据并确认兼容与恢复范围。工具不会自动覆盖当前数据去还原旧现场。

## 未验收 / 剩余任务

- 当前只是约 15 分钟部署后观察；一小时、一夜、容量/P95、整机重启、完整浏览器视觉回归和真实设备控制未验收。
- Ansible 此批只读，未实现自动发布/回滚任务和 Semaphore；软件自动维护栅栏、跨入口排空及部署作业恢复仍待下一批。
- 未修改 Rust—Go 通讯协议，未新增双管理端 HA，不同步两台管理端的业务配置。
- P1 与目录部署完成，不等于整个 P2–P5 架构计划完成。
