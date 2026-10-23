# Agent 运行身份展示验证（2026-09-28）

第九小批，补齐 P1 中 agent 运行身份的 Web 可见性。**未现场部署、未重启现场服务、未发送设备命令、未提交或推送。**

## 改动

- schema v1 的 process 快照追加可选 `agent_identity`，包含构建身份、守护 boot ID、初始化时间及报告时间。身份在 agent 初始化时取自编译元数据，读取不执行磁盘程序、不开串口、不改变控制 revision。
- 不复用 child 的启动时间/版本，也不将磁盘入口摘要当作运行版本；每次快照独立复制，调用者不能修改 agent 内部身份。重建守护实例重新生成 boot ID/启动时间。
- 管理器按现有来源、绑定、实例校验转发；Go 与 TypeScript 拒绝错误角色、未知身份 schema、无效或超长字段。旧 agent 不提供字段时保持兼容。
- Web 进程页默认展示版本/发布编号，其余构建和启动详情折叠显示。当前面板刷新失败保留旧版本并标记过期；不增加请求或后台轮询。全页刷新后的离线持久缓存尚未实现。

## 验证环境

测试主机 `192.168.10.46`；隔离目录 `/home/lenovo/code/rcd_agent_identity_20260928_tXfN0w`。本机仅编辑、查看差异/截图、归档与传输。

```sh
# manager/
go test -race ./internal/domain ./internal/processagent
go test -race ./internal/app ./internal/api -run Process -count=1
# manager/web/
npm test -- src/api/process.test.ts src/features/operations
npm run build
# 仓库根，远端 Playwright 与隔离页面数据
RCD_PLAYWRIGHT_MODULE=<远端模块路径> RCD_BROWSER_OUTPUT=<证据目录>/browser \
  node tools/opc/process-maintenance-browser.cjs
```

前端覆盖旧代理兼容、身份保留/更新、错误角色及畸形数据、刷新失败、详情展开；Go 覆盖身份复制、守护重建、HTTP 返回与管理器转发、读取不增加动作及不改变 CAS。浏览器使用真实构建资源、隔离 HTTP 数据，覆盖版本/维护/旧状态/日志/旧代理，共 8 场景；不连接现场 API，不发送写请求。

截图为桌面 1440×1000 与窄屏 390×844，已核对长摘要换行及主要操作布局。证据在远端 `agent-identity-evidence/` 和本机 `artifacts/runs/2026-09-28-agent-identity/`，保留源码摘要清单及实际测试日志。

最终结果：前端 **92 项 / 4 文件通过**，TypeScript/Vite 构建通过，浏览器 **8 场景通过**且无页面异常/写请求。domain/processagent 全包 race 通过（真实 Rust opt-in 未启用），app/api 的 Process 定向 race 通过。将构建信息改为初始化读取后重跑了相关 Go race；详情折叠布局调整后重跑了前端测试、构建及浏览器，不以首轮结果替代。

最终证据包 `agent-identity-evidence-final.tar.gz` SHA-256：`ee4d424cad2c28f13f0d74ac03d79da0d6f57d1f98fc90faefd633ef2fcab0e5`。

## 限制

构建版本为运行软件自报，不证明安装入口与运行映像一致；发布验收仍需独立摘要核验。时间为 agent 时钟，不据时钟回拨判定软件故障。旧代理无身份不会被判为下级故障。

本批没有 Rust 改动，未重复真实 Rust 协议互操作、设备控制或性能长测；Go 测试使用隔离 helper 进程，不是现场重启。未实现 agent 自身升级、全角色版本总览、跨面板/管理器重启的离线身份持久缓存。不能将本批视为 P1/P2 全部完成。
