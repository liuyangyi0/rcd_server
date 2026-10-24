# 第二管理端部署（2026-09-21）

经用户授权，在 `192.168.10.58` 新增独立管理端。没有修改程序源码，没有替换或重启 `.46` 管理端，没有向真实设备发送命令。

## 入口和目录

- Web：`http://192.168.10.58:8087`，按用户既有约定初始化管理员账号。
- OPC：`opc.tcp://192.168.10.58:4840`；原下级仍使用 `4841`。
- 安装根：`/home/lenovo/code/rcd_manager/`，`bin/` 为固定入口及版本化二进制，`data/` 为独立数据库、日志和新生成 PKI；`import/` 保存此次导入快照。
- 版本：`0.7.5-redundancy.1`，从 `.46` 已发布二进制复制并校验 SHA-256 一致，没有本机编译。
- 用户服务：`rcd-manager.service`，已 enable；用户 Linger 原本即为 yes。已验证服务重启后配置、计算及来源连接恢复；未重启整台电脑。
- 新服务使用 `/home/lenovo/code/rcd_manager/bin/RcdManager`，保留原 `~/.local/bin/RcdManager` 采集进程守护入口不动。

## 复制范围

复制当前 `.46` 的一个逻辑组、三个来源、设备映射、默认来源及 1997 条活动计算规则。原始活动配置按原字节传输，哈希为 `4c16f61eda06a65847df32880f8a5dc11fb626b3b4a1489e161ab42a28462508`。首次导入脚本因文本读取规范化 CRLF 导致哈希校验失败，修正为保留原字节后重新激活，最终两端一致。

默认来源仍为 `real-hardware → real-lower`。新管理端自动切源、默认源自动策略和差异检测保持关闭。没有复制旧数据库、历史告警、运维审计、网页登录会话、计算版本历史和未激活草稿；证书是新实例独立生成，指纹与 `.46` 不同。

没有修改下级日志主动推送目标，集中日志仍发往 `.46`。没有复制下级进程管理 agent 绑定，也没有实现跨管理端控制锁或只读权限限制；运维控制仍应统一从 `.46` 操作。两端后续配置、计算规则、选源策略不会自动同步。

## 验证

验收客户端、Go 测试和 Chromium 浏览器均在 `.46` 执行，远程连接 `.58`；本机只编辑部署脚本、传输文件和保存结果。

- 实际网页登录、新功能按钮、页面无 JavaScript 错误通过。
- 两端配置 API 的 groups/sources 一致，计算 hash/规则数一致，均连接 3 个下级，点表均为 5297 点。
- `TestLiveManagerAnonymousNone`：连接 `.58:4840`，固定 NamespaceArray 通过。
- `TestLiveStationCommunicationTree`：Browse/Read 全部 135 个站点状态，订阅三个来源的站点状态通过。
- `TestLiveManagerCentralCalculations`：1997 条规则元信息、原 `ns=2` 计算 NodeId、只读属性、Read/Subscribe 通过；采样计算点 `9CYE91CB001XQ01` 为值 1、Good。
- 两端比较时均为 2669 个 Good 点、2628 个坏质量点；站点状态有 58 个正常、77 个通信异常。坏质量来自现有下级采集状态，本次没有把这些状态修成 Good，也未声称所有物理设备正常。
- `.58` 下级 PID 1730266、agent PID 1730252 及启动时间未变；`.46` 管理端 PID 2421820 未变。新管理端 readiness 正常、服务 active、NRestarts=0。

日志与截图：`.46:/home/lenovo/code/rcd_redundancy_20260920/artifacts/runs/manager58-export-20260921/`；本机副本 `artifacts/runs/2026-09-21-manager58/`。`.58` 安装摘要在 `/home/lenovo/code/rcd_manager/logs/installation-20260921.json`。

这次完成第二实例部署，不等于主备集群：没有设置客户软件的备用地址，没有停掉 `.46` 验证故障接管，也没有进行长稳或满负载验收。客户端需另行配置两地址、自动重连/订阅恢复及显式选源恢复；不得在结果未知时自动重发控制命令。
