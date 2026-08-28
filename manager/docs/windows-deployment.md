# Windows 部署、配置与 PKI

## 构建单一 EXE

构建机需要：

- Windows PowerShell；
- Go 1.26；
- Node.js 与 npm；
- 能够按 `go.sum` 和 `web/package-lock.json` 获取依赖，或预先准备好的依赖缓存。

从仓库根目录执行：

```powershell
.\manager\build.ps1 `
  -Version '0.1.0' `
  -Commit (git rev-parse --short HEAD) `
  -Output '.\dist\RcdManager.exe'
```

`build.ps1` 的流程是固定的：

1. 在 `manager/web` 执行 `npm ci`；
2. 执行 `npm run build`，生成 `web/dist/index.html` 和带 hash 的静态资源；
3. 在 Go 模块执行 `go test ./...`；
4. 使用 `-trimpath`、`-s -w` 和版本 ldflags 构建 `cmd/rcd-manager`；
5. Go 的 `//go:embed dist/*` 将刚生成的前端打进 EXE。

因此发布 EXE 不依赖外部 Web 文件，也不需要安装 Node.js；Node.js 只在构建阶段使用。仓库里的 `web/dist/embed-placeholder.txt` 只让未构建前端的 clean checkout 可以编译，不能当作可发布 Web。

建议将发布 EXE 放在独立安装目录，例如：

```text
C:\Program Files\RcdManager\RcdManager.exe
```

不要直接从临时构建目录安装服务；Windows 服务记录的是安装命令执行时 EXE 的绝对路径。

## 运行配置

命令行参数优先于环境变量。默认配置：

| 配置 | 默认值 | 环境变量 | 命令行 |
|---|---|---|---|
| 数据目录 | `%ProgramData%\RcdManager` | `RCD_MANAGER_DATA_DIR` | `--data-dir` |
| OPC 监听 | `0.0.0.0:4840` | `RCD_MANAGER_OPC_LISTEN` | `--opc-listen` |
| OPC 广播主机 | 当前主机名 | `RCD_MANAGER_OPC_ADVERTISE_HOST` | `--opc-advertise-host` |
| 上级 Username | `user1` | `RCD_MANAGER_OPC_USERNAME` | `--opc-username` |
| 上级密码 | `pwd` | `RCD_MANAGER_OPC_PASSWORD` | 无，避免进入命令行/服务配置 |
| HTTP 监听 | `0.0.0.0:8080` | `RCD_MANAGER_HTTP_LISTEN` | `--http-listen` |
| 下级操作/方法超时 | 5 s | 无 | `--command-timeout` |
| `_Send` pending timeout | 30 s | 无 | `--command-pending-timeout` |
| 优雅关闭超时 | 15 s | 无 | `--shutdown-timeout` |
| 运维历史保留 | 90 天 | `RCD_MANAGER_AUDIT_RETENTION` | `--audit-retention`（Go duration，如 `2160h`） |
| 单表历史行数上限 | 100000 | `RCD_MANAGER_MAX_AUDIT_ROWS` | `--max-audit-rows` |
| 调试日志 | false | `RCD_MANAGER_DEV` | `--dev` |

默认容量是 20 个下级来源和 10 个上级 OPC Session，当前没有公开 flag 修改这两个限制。

`--opc-advertise-host` 必须是上级客户端实际可以解析/访问、并准备写入证书 SAN 的主机名或 IP。监听 `0.0.0.0` 不能直接作为客户端 endpoint host。

`RCD_MANAGER_OPC_PASSWORD` 必须在服务进程可见的系统环境中设置，不能只存在于安装者当前 PowerShell。示例（管理员权限）：

```powershell
setx /M RCD_MANAGER_OPC_PASSWORD "<强密码>"
setx /M RCD_MANAGER_OPC_ADVERTISE_HOST "manager01.example.local"
```

设置系统环境变量后再启动服务。系统环境值对本机管理员可见；它解决的是避免把密码写入服务命令行，并不是机密管理系统。

## 数据目录

默认目录内容：

```text
%ProgramData%\RcdManager\
├─ manager.db
├─ manager.db-wal / manager.db-shm   # 运行中可能存在
├─ logs\
│  ├─ rcd-manager.log
│  └─ rcd-manager.log.1 ... .5
└─ pki\
   ├─ rcd-manager-cert.pem
   └─ rcd-manager-key.pem
```

JSON 日志在 stderr 和文件同时输出。单个日志约 10 MiB 后轮转，保留 5 份备份。

连接/目录告警、来源状态历史、命令记录和管理审计按保留期限每日清理，并分别限制最大行数；尚未结束的 `Waiting` 命令和未恢复告警不会因行数限制被删除。Web“系统信息与维护”可查看当前 SQLite、日志、磁盘剩余空间及相关上限。

服务账户必须对数据目录有读取、创建、修改和删除权限。PKI 私钥、SQLite 文件和日志均不应对普通域用户开放。

## 安装与服务生命周期

以管理员身份，从最终安装目录执行：

```powershell
Set-Location 'C:\Program Files\RcdManager'
.\RcdManager.exe service install `
  --data-dir 'C:\ProgramData\RcdManager' `
  --opc-advertise-host 'manager01.example.local'
.\RcdManager.exe service start
```

服务安装信息：

- 服务名：`RcdManager`；
- 显示名：`RCD OPC Manager`；
- 启动类型：Automatic，Delayed Auto Start；
- 安装参数被保存为 EXE 的 `run ...` 参数；
- 停止和系统关机都会触发同一优雅关闭流程。

常用操作：

```powershell
.\RcdManager.exe service stop
.\RcdManager.exe service start
.\RcdManager.exe service uninstall
```

卸载前必须先停止服务。`service uninstall` 只删除 Windows 服务注册，不删除 EXE、数据库、日志或 PKI。

可在安装服务前用前台方式检查配置、端口和证书：

```powershell
.\RcdManager.exe run `
  --data-dir 'C:\ProgramData\RcdManager' `
  --opc-advertise-host 'manager01.example.local'
```

按 Ctrl+C 触发优雅停止。

## 上级 OPC endpoint

默认只公布并接受两种精确组合：

| SecurityPolicy | MessageSecurityMode | User token |
|---|---|---|
| None | None | Anonymous |
| Basic256Sha256 | SignAndEncrypt | Anonymous 或 Username |

安全 endpoint 应作为正常生产连接入口。但当前需求同时保留 None/None Anonymous，而且两种 endpoint 均允许匿名控制写入/Call；这要求网络层严格隔离，详见 [安全边界](security.md)。

默认兼容用户名/密码为 `user1`/`pwd`，必须在部署前覆盖密码。Username 只在 Basic256Sha256/SignAndEncrypt endpoint 上公布。

## 首次证书与轮换

首次启动时，如果证书和私钥都不存在，程序生成：

- 3072-bit RSA 自签名证书；
- 有效期 5 年；
- Application URI `urn:RcdManager:rcd_manager`；
- ServerAuth 与 ClientAuth EKU；
- SAN 包含 `--opc-advertise-host`、`localhost` 和 `127.0.0.1`。

同一证书/私钥既用于上级 OPC UA Server，也用于连接安全下级时的客户端身份。若只存在证书或只存在私钥，程序拒绝启动，避免悄悄替换身份。

轮换建议：

1. 停止服务；
2. 备份并将证书、私钥两者一起移出 `pki`；
3. 确认新 `--opc-advertise-host` 已配置；
4. 启动服务生成新证书；
5. 在所有上级客户端和要求客户端证书信任的下级服务器更新信任；
6. 完成两种 endpoint 的连接、读写和 Call 验证后再恢复业务。

不要只替换其中一个文件。证书轮换会改变应用身份，未经重新信任的客户端可能中断。

## 备份、恢复与升级

一致性备份的简单做法：

1. 停止服务；
2. 复制整个数据目录，包括 `manager.db`、PKI 和日志；
3. 替换 EXE；
4. 前台运行或启动服务，检查 `/healthz`、`/readyz`、OPC namespace 和来源状态；
5. 通过后再开放上级流量。

下级密码使用 Windows DPAPI machine scope 加密。把 `manager.db` 复制到另一台 Windows 主机后，原密文通常不能解密；迁移机器时必须重新录入下级密码。PKI 私钥本身不经过 DPAPI，必须用文件 ACL 和备份加密保护。

如果在服务运行时做文件级备份，必须同时正确处理 SQLite WAL/SHM；优先停服备份，或使用支持 SQLite online backup 的工具。

Web 和 `GET /api/v1/configuration` 可随时导出不含密码的配置 JSON。该文件可在任意管理器预检，但只允许实际导入到空配置实例；它适合重建组/来源拓扑，不能替代包含目录缓存、审计、DPAPI 密文和 PKI 的完整停服备份。`GET /api/v1/diagnostics` 只包含运行摘要、脱敏配置、近期告警/审计和最多 2 MiB 日志尾部，不包含数据库或密码。
