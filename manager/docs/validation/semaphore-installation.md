# Semaphore 安装与验收（2026-09-28）

## 结果

已在 `192.168.10.46` 安装 Semaphore UI Community 2.19.12，地址 `http://192.168.10.46:3000`。独立 `semaphore` 用户、独立 SQLite、systemd 服务 `semaphore.service`，已启用开机自启。

管理员账号为 `admin`，初始密码在目标机随机生成并仅存于 root 可读的 `/etc/semaphore/initial-admin-password`。本文与归档不包含密码、配置加密密钥、会话 cookie 或平台数据库。

固定目录与用法见 [Semaphore 说明](../../../tools/ansible/semaphore/README.md)。本次安装不修改 RCD 软件版本、业务配置、串口、选源、安全策略与端口。

## 下载与依赖

- 官方固定包：`semaphore_community_2.19.12_linux_amd64.tar.gz`。
- 与官方 checksums 核对 SHA-256：`2576f8a473c5e91bd0d7833976111c56f0ad43720210f9ca437037d10acd97cc`。
- 程序报告：`2.19.12-012ed06-1788086239`。
- 独立 Python 3.13.15、ansible-core 2.19.13；依赖按仓库 `tools/ansible/requirements.lock` 安装，不替换系统 Python。
- 安装过程修正了非 root 用户的程序目录执行权限，并按该版本源码将 SQLite 文件路径设置在 `sqlite.host`。数据库迁移、管理员创建与服务启动随后通过。没有为绕过问题将服务改为 root。

## 已验证

1. `semaphore.service` 为 active/running、User=semaphore、enabled；API ping 返回 `pong`。
2. 未登录访问 `/api/projects` 返回 HTTP 401。
3. “RCD 运维”项目已建立三台主机的 SSH 凭据、inventory 和只读巡检模板；凭据由 Semaphore 加密保存，主机公钥检查未关闭。
4. 从平台依次执行：任务 #1（30）、#2（46）、#3（58）全部 Success；内容是既有 `inspect.yml`，无设备命令或服务启停。
5. 在 46 上用真实 Chromium/Playwright 登录、查看三条成功历史、展开 #2 的日志详情；无浏览器 pageerror。日志控件按需渲染，并非所有行同时存在于 DOM，测试按可见日志检查。
6. 只重启新 Semaphore 服务，重启后登录和原三条任务历史保留。开机启用配置已核对，但没有为了验收重启整台机器。
7. 46 原业务服务保持安装前的进程：管理端 PID `2298214`，agent PID `2297709`，均 active、NRestarts=0；下级 PID `2297720`。4840、4841、8087 继续监听。

安装及浏览器验证都在 46 执行；30/58 只接收只读巡检 SSH 请求，没有在那里构建或启动测试采集程序。

## 边界与证据

- 未建立定时任务，未执行自动发布、回滚或现场重启；网页当前只提供已配置且验证过的巡检任务。发布类脚本快照已保留，但其环境 profile、产物和当次授权不得假定已准备好。
- 使用受信内网 HTTP，不是 HTTPS；未做公网暴露配置。
- 本批未做整机重启、性能/长时间稳定性或跨版本 Semaphore 恢复演练。
- 现有 Paramiko 连接方式在固定 Ansible 2.19 中可用，日志有上游弃用提示；后续升级 Ansible 前需迁移连接方式，不能忽略兼容期直接升级。

远端工作与证据目录：`/home/lenovo/code/rcd_semaphore_install_20260928_CreJW8/`。平台任务内保留各次完整输出；脱敏截图、任务结果及安装说明归档到本地 `artifacts/runs/2026-09-28-semaphore/`，不归档平台秘密。
