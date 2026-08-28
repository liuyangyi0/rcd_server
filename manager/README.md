# RCD OPC Manager

`manager/` 是与现有 Rust 采集程序并列的独立 Go 模块。它同时扮演两种角色：

- 作为 OPC UA Client 连接多台下级 RCD 程序，发现并订阅 `urn:SerialServer` 点表；
- 作为 OPC UA Server 向上级客户端提供旧点表兼容视图，以及按 OPC Session 隔离的选源管理视图。

管理 Web、REST/SSE、OPC UA Server 和下级连接监督器运行在同一进程内。前端构建产物通过 `go:embed` 打进最终 EXE；SQLite 只保存配置、目录和审计，实时值留在内存。

面向现场运维，Web 还提供来源独立连接测试、重新连接并发现目录、持久告警与状态历史、管理操作审计、默认源/Session/指定来源点位视图、同组双来源值比较、CSV 导出、配置备份预检恢复、证书/磁盘/队列状态、Prometheus 指标和脱敏诊断包。上述操作都不会启用自动故障转移。

## 快速开始

开发机需要 Go 1.26、Node.js/npm 和 PowerShell。Windows 发布构建：

```powershell
Set-Location .\manager
.\build.ps1 -Version 0.1.0 -Commit (git rev-parse --short HEAD)
```

脚本依次执行 `npm ci`、前端生产构建、`go test ./...` 和 Go 发布构建，默认生成：

```text
manager\dist\RcdManager.exe
```

前台启动：

```powershell
$env:RCD_MANAGER_OPC_PASSWORD = '<请设置强密码>'
.\dist\RcdManager.exe run --opc-advertise-host manager01
```

默认地址如下：

- OPC UA：监听 `0.0.0.0:4840`，对外地址 `opc.tcp://<opc-advertise-host>:4840`；
- Web/REST/SSE：`http://<主机>:8080`；
- 数据目录：`%ProgramData%\RcdManager\`；
- 数据库：`manager.db`；日志：`logs\rcd-manager.log`；PKI：`pki\`。

首次运行数据库为空。先在 Web 或 REST 中创建分组，再创建下级来源；来源完成首次目录发现并进入 `Ready` 后，才能设置为默认源。使用缓存目录离线启动时，来源状态为 `Cached`，仍可被选择。

Web 首页会在空配置时显示首次配置向导。日常排障建议先在“下级 OPC”执行连接测试，再按需执行“重连并发现”；管理程序的每次下级重连都会重新读取 NamespaceArray、浏览目录并重建订阅，因此不再显示两个结果相同的操作按钮。该动作不会改变默认源或在线 Session 的选择。配置备份不包含密码，恢复只允许导入到空管理器，导入前可在任意实例执行预检。

## Windows 服务

在管理员 PowerShell 中执行：

```powershell
.\RcdManager.exe service install --opc-advertise-host manager01
.\RcdManager.exe service start
.\RcdManager.exe service stop
.\RcdManager.exe service uninstall
```

服务名为 `RcdManager`，显示名为 `RCD OPC Manager`，安装为延迟自动启动。安装时给出的运行参数会写入服务配置。完整部署、升级、备份和证书说明见 [Windows 部署](docs/windows-deployment.md)。

## 文档

- [架构与 OPC 行为](docs/architecture.md)
- [REST 与 SSE 接口](docs/rest-api.md)
- [Windows 部署、配置与 PKI](docs/windows-deployment.md)
- [安全边界](docs/security.md)
- [验证命令与发布验收](docs/validation.md)

## 当前真实性边界

仓库内自动测试已经覆盖：两种上级安全端点和匿名/用户名身份、Browse/Read/Write/Call、双 Session 选源隔离、固定命名空间、旧点表黄金元数据、目录冲突与热增删、命令锁和 epoch、SQLite 恢复、REST/SSE 以及浏览器端主要流程。发布脚本也会从生产前端资源生成单一 EXE。具体执行口径见 [验证文档](docs/validation.md)。

仓库内还使用当前 Rust 项目完成了本地真实互操作：None/None + Anonymous、Basic256Sha256/SignAndEncrypt + Anonymous/Username 均已完成发现、Browse、Read/Subscribe、Write/Call 和断线重订阅；详细限制见验证文档。该结果仍不能代替目标环境验收。当前不得宣称以下项目已经通过：

- 目标现场证书链、Namespace index 变化和 SecureChannel 原 Session 重附着的完整互操作矩阵；
- 从现场版本 Rust 下级导出的点表与仓库黄金约束之间的逐项差异审批；
- 20×约 5300 下级点、10 个上级全量订阅客户端下的 P95 ≤ 500 ms 压测；
- 1 小时稳定性、24 小时 soak、0%/1%/100% 变化率和切源 5300 点突发；
- 目标 Windows 主机上的服务安装、重启自启、备份恢复和防火墙/VLAN 验证。

以上项目必须按验证文档在目标环境执行并保存结果后，才可以对外声明通过。
