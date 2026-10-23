# Semaphore 软件升级与回退接入验收

日期：2026-09-28。控制器和全部可执行测试：192.168.10.46。

## 接入结果

- 保留项目“RCD 运维”的三条状态巡检，新增六条日常模板：30/46/58 各“软件升级”“版本回退”。不提供七个后台步骤菜单，不增加定时任务。
- 模板 ID：30 升级 4 / 回退 5，46 升级 6 / 回退 7，58 升级 8 / 回退 9。
- 下拉选择组件和软件包。30 支持 lower/agent；46、58 支持 lower/agent/manager。默认仅检查，实际执行必须确认暂停控制命令。
- 复用现有 Ansible 和发布器。新增的是目标绑定、软件包登记、表单参数适配，没有重写服务控制或开发新的发布平台。
- 软件包为已登记的 `rcd-standard-paths-20260928` 现网版本，核对原构建清单和二进制 SHA-256 后入库。没有构建/发布新的 OPC 二进制。
- 发布使用 sudo 到 root；用户级服务通过其所属用户的 systemd 总线控制。未放宽 `/opt/rcd/bin` 权限。升级保留用户配置；回退日志显示明确的 `recovery_backup`，普通回退保留当前数据。
- profile 位于目标机 `/etc/rcd/release-profiles/`；包和备份位于 `/var/lib/rcd-release/`；Semaphore 仍以非 root 用户运行。

## 验证

隔离目录：`/home/lenovo/code/rcd_semaphore_release_20260928_BAOenf`。

1. `python3 -m unittest discover -s tools/release -v`：98 项通过，包括本次增加的动态配置清单、显式用户 systemd 总线检查。
2. `RCD_ANSIBLE_PLAYBOOK=/opt/semaphore/venv/bin/ansible-playbook RCD_ANSIBLE_STAGE_BINARY=/opt/rcd/bin/rcd_server /opt/semaphore/venv/bin/python -m unittest discover -s tools/ansible -v`：14 项通过。程序仅用于版本读取，分发文件在临时目录，不接真实串口。
3. 隔离 Go/Rust 互操作：`go test -race ./internal/processagent -run '^TestMaintenanceRealRustInterop$/^DurableReleaseRunner$/^AnsibleRelease$' -count=1 -v` 通过，约 67 秒。实际 Ansible → Python 发布器 → Go Agent → Rust OPC，覆盖部署、重复请求、验收、丢失确认和显式恢复；无真实串口。
4. 现网模板仅检查：任务 #5–#12，三机八个组件全部成功。#4 发现 sudo 凭据的目标用户不是 root，停在工具目录权限检查；已修正并保留失败记录，没有触发服务停止。
5. 在 46 用 Chromium 验证模板列表、升级和回退表单，默认下拉值正确。浏览器实际提交仅检查任务 #13 成功；请求拦截保护拒绝非 preview 或已确认控制暂停的请求。回退表单仅打开，不提交。

验后现场服务均 `active`、`NRestarts=0`：

| 主机 | 服务 | PID | 本次验后启动时间（CST） |
|---|---|---|---|
| 30 | 下级 Agent 服务 | 1180945 | 12:36:32 |
| 46 | 下级 Agent 用户服务 | 2297709 | 12:37:45 |
| 46 | 管理端用户服务 | 2298214 | 12:38:14 |
| 58 | 下级 Agent 服务 | 2958694 | 12:37:04 |
| 58 | 管理端用户服务 | 2959075 | 12:37:27 |

上述启动时间均早于本次下午约 16:58 开始的网页验证。46 PID 与接入前记录一致；没有替换或重启现场业务服务，没有发送设备控制报文。

## 证据与边界

- 远端 `release-tests.log`、`ansible-tests.log`、`interop.log`、`browser.log`、`browser/`；平台只检查结果 `/var/lib/semaphore/release-preview-verification.json`。
- 本地证据：`artifacts/runs/2026-09-28-semaphore-release/`，归档不含平台数据库、加密密钥、SSH 密码或业务配置。
- 更新运维脚本前，对平台数据库做 SQLite 一致性备份，并备份旧脚本，存放在受保护的 `/var/lib/semaphore/backups/release-connection-*`。备份不导出到仓库。
- **本次完成的是入口接入和验证，不是现场软件升级。** 没有执行现场回退或真实控制命令，也不据此宣称物理设备验收通过。
- 数据库版本变化、结果不确定等异常仍由维护人员使用既有工具处理；不会自动重试、自动回退或让现场人员选择复杂的事务阶段。

日常步骤见 [Semaphore 使用说明](../../../tools/ansible/semaphore/README.md)。
