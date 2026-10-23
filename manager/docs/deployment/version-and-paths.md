# 软件版本与标准路径

2026-09-28 已在 30/46/58 下级及 agent、46/58 管理端完成授权升级和目录迁移，版本 `0.6.0-paths.20260928`。业务配置继续归使用人管理，软件不冻结、校正或跨机器同步配置，不以物理设备是否在线判断软件安装成功。实际范围见 [迁移记录](../validation/standardization-path-migration.md)。

后续更新：三台下级/Agent 和三台管理端现已安装 `0.6.1-full.20260928`；30 管理端为后续新装、独立空配置，等待业务配置选择。仍只在 46 部署一套 Semaphore。最新实际状态见 [全量部署及补装记录](../validation/full-release-20260928.md)，下文旧迁移范围仅作历史说明。

## 查询软件版本

```text
rcd_server version --json
RcdManager version --json
RcdManager agent version --json
```

三个入口在读取运行配置、加实例锁、启动采集或服务之前返回。纯文本 `version` 仍可用。agent 角色由 `agent` 子命令明确指定，不根据文件名猜测；若安装入口命名为 `RcdAgent`，仍使用 `RcdAgent agent version --json`。

统一字段：`schema_version`、`component`、`version`、`commit`、`built_at`、`release_id`、`source_sha256`、`dirty`、`target`。其中：

- `release_id` 关联同一批软件产物，`component` 区分 lower/manager/agent；同批不同角色的二进制摘要不要求相同。
- `commit` 是源码起点，`dirty=true` 表示明确记录的未提交源码快照。最新内容以 `source_sha256` 和清单为准，不能把起点提交当成全部新代码已提交。
- 未注入的开发构建保留 dev/unknown 等标识，不能冒充正式发布。
- `target` 是实际编译平台；不同语言的架构名称可能分别为 x86_64/amd64，不能只凭字符串不同判断不兼容。
- 程序内信息不冒充实际 EXE 摘要；发布时仍须核对产物及运行中进程映像的 SHA-256。

只读接口追加：管理端 overview 的 `build_info`；下级 Operations status 的 `build_info`；agent 的 `GET /api/v1/version`。agent 查询仍须携带原有 `X-RCD-Instance-ID`，未改变实例校验或认证边界。旧字段保留。

本批未增加完整的多主机版本对比页面；已有页面的版本字段可使用注入的版本值，完整构建信息已由后端提供。

## 下级运行路径

| 参数 | 环境变量 | 未指定时 |
| --- | --- | --- |
| `--config-dir` | `RCD_CONFIG_DIR` | 实际 EXE 同级 `config/` |
| `--state-dir` | `RCD_STATE_DIR` | 选定的 config-dir |
| `--pki-dir` | `RCD_PKI_DIR` | 工作目录下 `pki/` |
| `--log-dir` | `RCD_LOG_DIR` | 原 `[logging].directory` 规则；缺省 EXE 同级 `logs/` |

同项优先级：命令行 → 对应环境变量 → 旧默认规则。支持 `--config-dir PATH` 和 `--config-dir=PATH`。显式相对路径基于启动时工作目录；服务部署推荐绝对路径。空值、重复参数和未知参数会报错，不静默使用另一套配置。

`RCD_CONFIG` 仍只覆盖日志配置文件，不成为业务配置目录参数。显式 log-dir 优先于日志配置文件中的 directory；原日志等级、轮转、推送配置不变。

显式指定的 config/state/pki 目录必须预先存在；程序不会自动创建默认业务配置或从历史目录复制配置。日志目录仍由既有日志实现按需创建。

### 配置与状态分离

- config-dir：`config.toml`、`calc.toml`、`rcd*.csv` 等用户业务配置。
- state-dir：`operations.json`、`station_versions.json`、版本备份和 `command-journal.sqlite` 等运行状态与命令身份。
- 若未设置 state-dir，这些文件继续留在 config-dir，兼容现有部署。
- 显式分离时，旧配置目录存在的状态文件若在目标状态目录缺失，启动会报 `state migration required`，不以空状态启动并重置开关、版本或命令身份。
- 该检查只是防止明显遗漏，不代替完整迁移工具或数据库一致性备份。迁移须停止写入并保留当前用户数据，不能单独复制正在写入的 SQLite 主文件。

实例锁增加配置/状态目录范围的保护，并继续检查已存在的旧 EXE 同级锁；新显式安装不要求在程序目录创建新锁。不能删除锁文件绕过冲突，也不能同时启动两套新旧路径采集同一串口。

### agent 启动参数

agent 新增本地参数 `--lower-config-dir`、`--lower-state-dir`、`--lower-pki-dir`、`--lower-log-dir`，分别作为固定参数传给下级。目录必须已存在；未指定时不添加参数，保留旧默认。HTTP 请求不能更改这些路径或添加任意启动参数。

agent 自己的 `--log-dir` 指定 `process.log/agent.log` 目录，缺省仍在 data-dir；process-state.json 与 agent.lock 留在 data-dir。不影响 Rust 业务日志等级、轮转或推送。

### 已部署标准布局

| 内容 | 路径 |
| --- | --- |
| 程序入口 | `/opt/rcd/bin/rcd_server`、`RcdManager`、`RcdAgent` |
| 发布产物及清单 | `/opt/rcd/releases/rcd-standard-paths-20260928/` |
| 服务环境文件 | `/etc/rcd/<role>-<instance>.env` |
| 下级配置 / 状态 / PKI | `/var/lib/rcd/lower/<instance>/{config,state,pki}/` |
| 下级业务日志 | `/var/log/rcd/lower/<instance>/` |
| agent 状态 / 日志 | `/var/lib/rcd/agent/<instance>/`、`/var/log/rcd/agent/<instance>/` |
| 管理端数据 / 日志 | `/var/lib/rcd/manager/main/`、`/var/log/rcd/manager/main/` |
| 迁移备份 | `/var/backups/rcd/paths-20260928-<主机末段>/<role>/` |

实例 ID：30=`rcd93-30`，46=`real-lower`，58=`rcd92-58`。manager 的 data/logs 是指向标准日志目录的链接；两台管理器数据库独立保留。30 未运行管理端。

原服务名和运行账号保持：30/58 下级栈为系统级 `rcd-lower.service`（内部 agent 拥有唯一 child）；46 下级栈为用户级 `rcd-process-agent@real-lower.service`；46/58 管理端为用户级 `rcd-manager.service`。通过单独 `90-rcd-paths.conf` 覆盖启动路径，未建立第二套自启动入口。

旧 home/code 目录是保留的历史副本，**不再是生效配置目录**，不要再次从那里启动采集程序。迁移恢复不能简单删 override 并启动旧数据，须先保全和迁移当前数据，详见 [发布工具说明](../../../tools/release/README.md)。

## 发布构建身份

现有 `tools/release/release.py build` 增加 `--release-id`、`--app-version`，角色增加 `agent`。管理端与 agent 可来自同一 Go 源码/二进制，但作为不同发布目标处理。

Rust 通过编译环境注入，Go 通过链接参数注入；构建结束实际执行无副作用的 `version --json`，逐项对比目标角色、版本、release_id、源码摘要、提交、时间和脏快照标记。不匹配则拒绝产物。

省略版本参数时生成明确的 snapshot 标识，不继续产出看似正式的 unknown 版本。源清单增加测试输入和 Rust 工具链文件。构建不替用户修改运行配置。

发布工具现支持 systemd-user/systemd-system；默认软件回滚保留当前用户配置和数据，schema 变化拒绝自动回退。只读 Ansible 巡检已跑通。跨入口维护栅栏、自动发布恢复及 Semaphore 尚未实现，不能把本次人工维护窗口下的迁移等同于整个发布平台完成。
