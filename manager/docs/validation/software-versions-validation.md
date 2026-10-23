# 软件版本总览与离线缓存验证

日期：2026-09-28。状态：代码完成、远端隔离验证通过，**本次未部署、未重启现场服务**。

## 范围

用户确认只汇总当前管理端、它管理的全部下级及 Agent；不增加跨管理端发现。新增只读 API、系统信息页版本面板、后台低频查询和 SQLite 最后成功身份缓存。未改变采集、计算、选源、串口和控制语义。

工作树为 `codex/opc-device-groups`，基于 `769ef2a1f82e6e7bb5e77f9e79a3e88434005e42` 的未提交工作区；保留已有改动，没有提交或推送。本次无需改动 Rust 或升级 Agent，兼容已部署 Agent 的 `/api/v1/version`。

## 验证环境与结果

全部构建、可执行测试、race 和浏览器在 192.168.10.46 执行。隔离目录：

`/home/lenovo/code/rcd_software_versions_20260928_vt5o3d`

- `go test -race ./internal/store ./internal/app ./internal/api ./internal/domain`：全部通过。覆盖磁盘缓存关闭后重开、Runtime 重建后缓存、离线保留成功时间、绑定变更、迟到回复、epoch 变化、旧端能力缺失、身份校验、只读 API 及已有相关回归。
- `npm test -- src/features/operations`：4 个测试文件、54 个测试通过。
- `npm run build`：TypeScript 与 Vite 构建通过。
- `go build -o ../RcdManager.software-versions ./cmd/rcd-manager`：通过，包含本次构建的 Web 静态资源。
- `tools/opc/software-versions-browser.cjs`：6 组浏览器场景通过，覆盖列表、缓存/不支持、构建展开、筛选/空结果、失败保留、窄屏横向表格；无页面异常、无写请求。使用隔离 API 固定样例，不把这项当作现场部署验收。
- `TestSoftwareVersionsRealAgentReadOnly`：通过显式目标环境变量，从 46 对 46、30、58 的现有 Agent 做只读状态/版本 GET。三台均报告 `0.6.0-paths.20260928`。未调用进程启停、维护动作或串口命令。
- `git diff --check`：通过（仅已有行尾格式提示）。

隔离二进制 SHA-256：

`b6d24880e8a685792da0ef614adb23ac7d5faa9e548c682abc2d687b8e26600a`

这是开发验证产物，不是已部署的正式版本。

## 证据

远端隔离目录内：`go-tests-final.log`、`web-tests-final.log`、`web-build.log`、`go-build.log`、`real-agent-readonly.log`、`browser.log`、`browser/result.json` 及两张页面截图。

本地副本位于工作树 `artifacts/software-versions-evidence/`。证据未包含密码、现场配置或现场数据库。

## 限制与上线注意

- 本次没有停止现场来源制造真实断线，也没有重启管理端验证现场数据库；离线和重启缓存由隔离测试覆盖。
- 下级 OPC 状态读取和 epoch 防迟到覆盖为代码回归，本次没有另外启动采集实例或做完整现场端到端升级验收。
- 没有执行一小时/过夜稳定性测试，不声称长期验收完成。
- SQLite 追加缓存表与地址/身份变化清理触发器。正式升级前使用一致性数据库备份；旧管理端拒绝新 schema 时必须同时恢复对应备份。
- 页面入口和状态解释见 [软件版本总览说明](../guides/software-versions.md)。
