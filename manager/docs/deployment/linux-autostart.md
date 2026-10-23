# Linux 固定路径与开机自启

> 本文保留早期用户级安装方式及模板说明。2026-09-28 现场已迁入 `/opt/rcd`、`/var/lib/rcd` 和 `/var/log/rcd`，manager/agent 入口分开；不要照本文旧入口覆盖现有服务。当前布局与原服务 drop-in 见 [软件版本与标准路径](version-and-paths.md)。

管理端和下级进程守护均使用同一个固定入口：`~/.local/bin/RcdManager`。管理端由
`rcd-manager.service` 运行；每个下级由 `rcd-process-agent@<instance>.service`
运行。agent 再启动固定工作目录中的 `rcd_server`，因此网页停止下级后仍能通过
agent 再次启动。

安装文件：

- `deploy/linux/rcd-manager.service` → `~/.config/systemd/user/rcd-manager.service`
- `deploy/linux/manager.env.example` → `~/.config/RcdManager/manager.env`
- `deploy/linux/rcd-process-agent@.service` → `~/.config/systemd/user/rcd-process-agent@.service`
- `deploy/linux/agent.env.example` → `~/.config/RcdManager/agents/<instance>.env`

环境文件必须使用真实绝对路径。首次交给 agent 前，应先停止原来手工运行的下级，
避免两个进程同时占用串口和 OPC 端口。配置完成后执行：

```bash
systemctl --user daemon-reload
systemctl --user enable --now rcd-process-agent@<instance>.service
systemctl --user enable --now rcd-manager.service
```

需要无人登录时随机器启动，还必须由管理员明确执行一次：

```bash
sudo loginctl enable-linger <user>
```

验证不能只看 `systemctl`：还需检查 agent `/healthz`、下级 OPC 端口、管理端
`/readyz`、来源目录以及上级 Session/订阅。升级时先把新管理器放到版本化文件，
原子更新 `~/.local/bin/RcdManager` 固定链接，再重启两个用户服务。
