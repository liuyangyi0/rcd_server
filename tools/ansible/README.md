# RCD Ansible 发布与巡检

日常操作请用 [Semaphore 原生发布网页](semaphore/README.md)：上传单组件包、选择设备升级、查看实际版本，从组件历史恢复。2026-10-08 定制平台已上线 46:3000，使用原生任务队列与每分钟检查。以下独立剧本供维护人员使用，不是要求现场逐项点击的操作流程。

所有启停/备份/替换仍由 [release 工具](../release/README.md)实现，Ansible 没有第二套 systemd restart handler。原生发布通过 `release-platform.yml`；`inspect-platform.yml` 产生与原生任务关联的版本检查报告。保留旧剧本作为受控应急入口，不自动覆盖用户配置或重投未知事务。

**部署入口会停服务，必须有当次目标授权和控制暂停窗口。** 未授权时只使用巡检、分发或预览。默认不改用户业务配置，不自动处理下级设备故障，不自动升级 58。

| 入口 | 用途 | 是否可能重启 |
| --- | --- | --- |
| `inspect.yml` | 既有服务、运行映像与标准路径巡检 | 否 |
| `stage.yml` | 分发已构建程序，校验 SHA-256/架构/版本，生成独立目标清单 | 否；只运行 `version --json` |
| `deploy.yml` | 单组件预检、备份、替换、启动和软件验收 | 是 |
| `verify.yml` | 核对运行映像摘要、readyz（已配置时）、OPC TCP 端口 | 否 |
| `recover.yml` | 显式选择原维护作业的查看/继续/回退/收尾 | 查看不改，其余可能启停 |
| `rollback.yml` | 明确选择一个 `backup-*`，调用原人工窗口回滚流程 | 是 |

这些入口不代表已完成现场发布、真实设备验收或长时间稳定性测试。

## 使用

控制端固定在 46 的隔离工具环境，不替换系统 Python，不安装到业务服务环境。依赖固定在 `requirements.lock`；控制端 Python 3.13.15、ansible-core 2.19.13 已验证。选择 2.19 系列是为了兼容现有目标 Python 3.8；后续须按 [Ansible 支持矩阵](https://docs.ansible.com/projects/ansible/latest/reference_appendices/release_and_maintenance.html)规划工具环境升级，不能无限期固定。

1. 复制 `inventory.example.yml` 为不提交的现场清单，指定主机、现有 SSH 账号和每个服务的 scope/unit/instance。
2. 凭据使用 SSH agent、交互输入或 Vault；禁止提交明文密码。保留主机密钥检查。
3. 在服务运行账号下执行；用户级服务需要可用的用户 systemd session / `XDG_RUNTIME_DIR`。不擅自提权或改变服务账号。
4. 显式选择目标：

```sh
ansible-playbook -i /private/inventory.yml tools/ansible/inspect.yml \
  --limit example_lower,example_manager
```

可先 `--syntax-check`。未给 `--limit` 拒绝执行；串行巡检，一个目标失败即停止后续目标。执行策略采用 Ansible 的 [serial 与失败控制](https://docs.ansible.com/projects/ansible/latest/playbook_guide/playbooks_strategies.html)。

## 输出与边界

- 显示服务 active/enabled、MainPID、NRestarts；分别显示入口文件与 `/proc/<pid>/exe` 的摘要和匹配发布身份。
- 只信任与 `/opt/rcd/releases/*/manifest.json` 摘要一致的产物；不执行发现的程序、不根据文件名猜版本。未知版本显示空，不伪造版本。
- `restart_required_for_entry=true` 表示运行映像与磁盘入口不一致，不会自动重启。
- 下级只列已登记 agent 主进程的直属 `rcd_server` child，不接管、不发送信号、不扫描用户配置或凭据。
- 服务停止、进程读不到、版本未知分别展示。设备是否在线不属于这份软件清单，不作为巡检失败条件。
- `changed=0` 表示没有修改业务/服务；Ansible 自身仍会传输并清理临时巡检脚本。未开启业务 Web 新页面。

底层独立入口：`python3 tools/release/inspect_runtime.py --component lower-stack --unit rcd-lower.service --scope system --instance example-lower`。

2026-09-28 已从 46 对 30/46/58 执行，三台均 `ok=3 changed=0 failed=0`。这是即时只读巡检，不等于长时间稳定性或机器重启验收。

## 分发与部署

1. 在 **46** 用 release 工具构建一次。控制端产物目录只需 `binary` 和原 `manifest.json`；下级目标机不需要源码、编译器或重新构建。旧多组件迁移包不直接视为新的单组件发布包。
2. 为每个 inventory 主机建立私有 `host_vars/<别名>.yml`，参考 [release-vars.example.yml](release-vars.example.yml)。同一次 play 对每台主机操作一个组件；管理端与下级分开运行，46/58 管理端不要同时停。
3. 目标机预先保存私有 release profile：主机/组件、固定 EXE、运行根、发布根、控制方式、完整配置和 SQLite 备份清单。`source_dir` 保留构建布局字段，目标机不要求该目录存在。不要把业务配置复制进分发包。
4. 将原构建 `manifest.json` 的 SHA-256 填入 `manifest_sha256`。Ansible 保留原清单为 `build-manifest.json`，不会把构建主机伪造为目标主机；新 `manifest.json` 绑定目标主机和发布 profile 摘要。用户修改业务配置不使绑定失效，修改发布 profile 则应重新暂存至新目录。
5. 新版下级使用 `mode: maintenance`，必须已有兼容 agent/下级维护接口。旧版下级或管理端只可**显式**选择 `mode: manual` 并建立人工控制暂停窗口。接口不支持或调用失败不会自动降级。schema v2 agent 自身升级仍由原工具拒绝，不能借 Ansible 绕过兼容性检查。

```sh
# 先预览；这不会检查远端服务、执行二进制或创建目录。
ansible-playbook -i /private/inventory.yml tools/ansible/stage.yml \
  --limit example_lower --check

# 真正分发；相同目标、包和 profile 重复执行应 changed=0。
ansible-playbook -i /private/inventory.yml tools/ansible/stage.yml \
  --limit example_lower

# 当次授权文件，不永久写入 host_vars：
# rcd_confirm_targets: ['192.0.2.30/lower']
# rcd_no_active_commands: true
# rcd_initiator: operator-name
ansible-playbook -i /private/inventory.yml tools/ansible/deploy.yml \
  --limit example_lower -e @/private/current-window.yml

ansible-playbook -i /private/inventory.yml tools/ansible/verify.yml \
  --limit example_lower
```

发布/分发/恢复仅接受 `--limit 主机别名[,主机别名]`，拒绝组名、`all`、通配符。采用 `serial: 1`、`any_errors_fatal: true`；前一台软件发布失败或结果未知时，后续主机不执行，不自动重试、不自动回滚。必须分别核对当前授权目标，不能用历史暂停记录冒充本次暂停。

**维护影响**：目标来源采集会短暂中断；固定选源的客户端可能收到坏质量，已开启自动切换的组按原策略处理，中央计算按默认源产生相应质量。发布工具不修改选源、轮询时序、业务 NodeId、安全策略或设备配置。现场设备离线不作为软件发布失败条件。

## 记录、重复执行和恢复

- 工具按内容摘要复制到 `tool_dir/<hash>/`，不同版本作业不互相覆盖 Python 代码。
- `<release_dir>/ansible-operations/<operation_id>/operation.json` 记录目标、profile/工具/产物摘要、意图、结果和异常类型。维护发布另有原 `deployments/<deployment_id>/deployment.json` 逐阶段记录及备份。
- 每次新修改操作用唯一 `operation_id`。已成功编号重复提交只返回原记录，`already_executed=true`，**不表示当前运行状态刚刚通过验收**；当前状态用 `verify.yml` 查。
- 未知/中断编号不重放；更换编号也不能绕过固定 EXE 旁的 `.ansible-active.json`。不要删除标记或锁文件。
- 维护作业恢复必须保留原 `deployment_id`，每次选定恢复动作使用新的 `operation_id`。先只读查看，再判断是否可继续：

```sh
ansible-playbook -i /private/inventory.yml tools/ansible/recover.yml \
  --limit example_lower -e rcd_recovery_action=inspect-deployment

# 已核实原 start 已执行、只需核对/收尾的例子：
# 先在私有 host_vars 中换一个 operation_id，保留原 deployment_id。
ansible-playbook -i /private/inventory.yml tools/ansible/recover.yml \
  --limit example_lower -e rcd_recovery_action=finish-deployment \
  -e @/private/current-window.yml
```

可选恢复动作：`resume-deployment`、`finish-deployment`、`rollback-deployment`、`resume-rollback`、`finish-rollback`。具体允许的阶段由原恢复工具检查，不是任意阶段都能回退；新版已运行后的通用自动回滚不在此入口支持范围内。

人工窗口回滚使用 `rollback.yml`：`mode=manual`，`artifact_dir` 指向精确 `backup-*`，使用新的操作编号。若前次人工发布结果未知，额外设置 `recover_operation_id` 为该次原操作编号。默认保护性备份当前数据、仅回退二进制；schema 改变则停止，由人工决定。`rcd_restore_backup_data: true` 会覆盖当前配置/数据库，只能在明确授权范围和可能数据损失后使用，绝不能自动添加。

## 验证边界

`--check` 是输入/计划预览，所有上传与执行任务跳过，不冒充验收。[Ansible check mode](https://docs.ansible.com/projects/ansible/latest/playbook_guide/playbooks_checkmode.html) 本身不是通用事务回滚机制。执行采用 [command argv](https://docs.ansible.com/projects/ansible/latest/collections/ansible/builtin/command_module.html) 且关闭变量展开，不拼 shell 命令。

隔离验证入口（只在 46）：

```sh
python3 -m unittest discover -s tools/release -p 'test_*.py'
RCD_ANSIBLE_PLAYBOOK=/absolute/venv/bin/ansible-playbook \
RCD_ANSIBLE_STAGE_BINARY=/absolute/isolated/rcd_server \
python3 tools/ansible/test_playbooks.py
```

实际 Ansible → Python → Go agent → Rust OPC 互操作使用 `manager/internal/processagent/maintenance_interop_test.go` 的 `AnsibleRelease` 子测试，需显式 `RCD_ANSIBLE_PLAYBOOK` 和 `RCD_MAINTENANCE_INTEROP_BINARY`，仅使用临时目录/端口、无真实串口。测试记录见 [Ansible 发布编排验收](../../manager/docs/validation/ansible-release-validation.md)。
