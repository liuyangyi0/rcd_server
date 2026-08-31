# RCD OPC Manager

`manager/` 是与现有 Rust 采集程序并列的独立 Go 模块。它同时扮演两种角色：

- 作为 OPC UA Client 连接多台下级 RCD 程序，发现并订阅 `urn:SerialServer` 点表；
- 作为 OPC UA Server 向上级客户端提供旧点表兼容视图，以及按 OPC Session 隔离的选源管理视图。

管理 Web、REST/SSE、OPC UA Server 和下级连接监督器运行在同一进程内。前端构建产物通过 `go:embed` 打进最终 EXE；SQLite 只保存配置、目录和审计，实时值留在内存。

两个 OPC 依赖通过 `go.mod` 固定到仓库内的补丁版本：[服务端 v1.4.0](third_party/awcullen-opcua/FORK.md) 与 [客户端 v0.9.1](third_party/gopcua-opcua/FORK.md)。构建、复制源码或升级依赖时应保留两个目录；不要移除 `replace` 后把未修复的上游版本误当成同一发布候选。

面向现场运维，Web 还提供来源独立连接测试、重新连接并发现目录、持久告警与状态历史、管理操作审计、默认源/Session/指定来源点位视图、同组双来源值比较、CSV 导出、配置备份预检恢复、证书/磁盘/队列状态、Prometheus 指标和脱敏诊断包。上述操作都不会启用自动故障转移。

## 新版设备分组

Rust 下级新增“串口 → 设备 → 点位”拓扑，旧点位和命令 NodeId 不变。一个下级仍只建立
一条连接，但可以服务多个逻辑组。切换作用于当前 OPC Session 的整组点位，不是单个变量，
也不影响其他设备组或其他 Session。

先创建各设备的逻辑组和一个共享兼容组，再添加下级。冗余下级使用同一共享兼容组；在
“下级 OPC → 串口 / 设备映射”把两边对应设备绑定到相同逻辑组，完成发现后设置默认来源。
两边串口名可以不同。逐台保存时可能暂时出现目录冲突，两边映射一致后重新校验恢复。
修改映射前须关闭在线 Session 并清空相关待处理命令。

单设备计算点随该设备组切换；跨设备、常量及归属未知的计算点留在共享兼容组，不在管理器
内重新计算。命令按报文设备地址路由，同一物理串口互斥；新增管理节点
`Groups/<group>/Send` / `SendCommand` 可明确指定逻辑组。老下级无拓扑时仍按原单组模式工作。

协议详见 [设备拓扑与多逻辑组选源](docs/device-groups.md)，本轮结果见
[远端验证记录](docs/device-groups-validation.md)。本次构建及测试仅在远端执行。

## 快速开始

### 下级配置、采集开关和日志

新版 Rust 下级支持在“下级 OPC → 运维”中管理指定的物理来源，不随当前 OPC Session 的
选源切换：

- “采集”：查看串口期望/已应用/实际打开状态、各站点读取及通信状态；单站停读不影响
  其他站点，也不禁用该站控制命令。关闭串口需确认，会停止该口所有站点并释放句柄，
  下级 OPC 服务本身继续运行。开关保存在下级 `config/operations.json`，重启保持。
- “配置”：查看、下载、校验和编辑已有 `config.toml`、`calc.toml`、`rcd*.csv`；保存前
  校验整个配置集，自动备份并原子保存。备份先载入草稿，再显式校验保存恢复。
  **文件保存后须重启下级才生效；管理器不会自动重启下级。**
- “日志”：读取实际启动日志目录内的日志，按文件、级别和文字筛选、分页查看与下载当前页。
  每页最多 64 KiB；轮转或截断会提示刷新，不混接不同文件内容。
- “进程”（Linux）：绑定独立 agent 后，启动、停止、重启固定的下级进程，查看 PID、
  退出原因、进程输出及操作历史。下级 OPC 停止后 agent 仍在线，可再次启动；“重连并发现”
  只重建管理器的 OPC 连接，不等同于重启下级。配置页可显式跳转到进程页，核对版本后重启
  应用磁盘上已保存的全部配置；保存自身不触发重启，也不会提交未保存草稿。

修改同时检查管理器来源版本和下级状态/文件版本。来源连接配置发生变化、存在待处理命令、
或其他页面已改过同一版本时应先刷新核对。请求超时后结果可能已经生效，不能直接重复提交。
旧版下级没有运维能力时会明确提示，原有采集仍可继续；本功能不新增登录或改变 OPC 安全策略。
详细协议、范围和错误语义见 [下级运维](docs/lower-operations.md)。

Linux agent 与管理器共用同一个二进制，以 `RcdManager agent run` 独立运行，只管理自己启动的
固定 child，不接管现有 PID。启用前须先交接目标下级，避免争用串口。手动停止状态持久保存；
进程作业与该来源命令/运维修改互斥，未知结果按原 request_id 查询和对账，不重复重启。
该功能不会重启主机、修改认证设置或自动切换来源。参数、人工安装的 systemd 用户服务模板及
限制见 [下级进程管理](docs/lower-process-management.md)。Windows agent 尚未实现。

### 构建和运行

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

`4840` 保留给管理 OPC，供客户端连接。Rust 下级 OPC 默认使用 `4841`；如果多个下级运行在同一主机，必须通过 `RCD_OPCUA_PORT` 分配不同端口，并在来源 Endpoint 中使用对应端口。

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
- [设备拓扑与多逻辑组选源](docs/device-groups.md)
- [下级配置、串口/站点与日志运维](docs/lower-operations.md)
- [下级进程启停、重启与守护部署](docs/lower-process-management.md)
- [设备分组新版远端验证记录](docs/device-groups-validation.md)
- [REST 与 SSE 接口](docs/rest-api.md)
- [Windows 部署、配置与 PKI](docs/windows-deployment.md)
- [安全边界](docs/security.md)
- [验证命令与发布验收](docs/validation.md)

## 当前真实性边界

仓库内自动测试已经覆盖：两种上级安全端点和匿名/用户名身份、Browse/Read/Write/Call、双 Session 选源隔离、固定命名空间、旧点表黄金元数据、目录冲突与热增删、命令锁和 epoch、SQLite 恢复、REST/SSE 以及浏览器端主要流程。发布脚本也会从生产前端资源生成单一 EXE。具体执行口径见 [验证文档](docs/validation.md)。

本轮在远端 Linux 主机使用两套真实 Rust 进程和隔离 PTY 完成了设备分组互操作，
包括旧节点对比、None/匿名下的 Read/Subscribe/Write/Call、断线恢复，以及
Basic256Sha256/SignAndEncrypt 下匿名/用户名读取；另通过双 Session 的浏览器定向选源验收。
详细日志和限制见新版远端验证记录。该结果仍不能代替现场硬件及目标环境验收。
当前不得宣称以下项目已经通过：

- 目标现场证书链、Namespace index 变化和 SecureChannel 原 Session 重附着的完整互操作矩阵；
- 从现场版本 Rust 下级导出的点表与仓库黄金约束之间的逐项差异审批；
- 20×约 5300 下级点、10 个上级全量订阅客户端下的 P95 ≤ 500 ms 压测；
- 1 小时稳定性、24 小时 soak、0%/1%/100% 变化率和切源 5300 点突发；
- 目标 Windows 主机上的服务安装、重启自启、备份恢复和防火墙/VLAN 验证。

以上项目必须按验证文档在目标环境执行并保存结果后，才可以对外声明通过。
