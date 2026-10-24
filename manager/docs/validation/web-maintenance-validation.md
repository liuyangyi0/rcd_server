# Web 维护状态验证（2026-09-28）

P2 第七小批：只读维护展示与普通控制约束。未现场部署、未重启现场服务、未发送设备命令、未提交或推送。

## 改动

- agent 的进程快照新增可选 `maintenance_enabled`，明确区分启用、未启用及旧代理未报告；活动作业继续附加发布编号与阶段。每次返回独立布尔快照，不暴露内部可变指针。
- Go 与前端校验维护字段完整性、长度及矛盾状态；未知将来阶段可显示但不解除界面锁定。
- 下级运维 → 进程显示维护原因，活动期间禁用普通启停和绑定修改；同一抽屉的采集/配置写入锁定，保留草稿、读取与日志。刷新失败保留旧状态而非显示空闲。
- 配置预检后的提交直接核对最新查询缓存。新增回归测试抓到 React 尚未重绘时的旧确认竞态，修复后不再发 POST、也不登记假 pending。
- 不新增发布写接口，不改变串口/OPC/计算语义，不提供 Web 自动发布恢复；旧 agent 控制仍兼容。

## 远端验证

主机 `192.168.10.46`，隔离目录 `/home/lenovo/code/rcd_web_maintenance_20260928_PcfGcO`。本机仅编辑、差异检查、归档和传输。

```sh
# manager/
go test -race ./internal/domain ./internal/processagent
go test -race ./internal/app ./internal/api -run Process -count=1
# manager/web/
npm test -- src/api/process.test.ts src/features/operations
npm run build
# 仓库根，使用远端已安装的 Playwright
RCD_PLAYWRIGHT_MODULE=<远端Playwright路径> \
  RCD_BROWSER_OUTPUT=<隔离证据目录>/browser \
  node tools/opc/process-maintenance-browser.cjs
```

浏览器使用构建后的完整页面和隔离 HTTP 数据，检查活动维护、日志可读、503 旧状态保留、未来阶段/窄屏长编号、空闲保护、旧代理兼容，共 6 场景，无页面异常及写请求。桌面 1440×1000、窄屏 390×844 截图已人工核对。这不是设备或现场发布验收。

最终结果：前端 **77 项 / 4 文件通过**，类型检查及 Vite 构建通过；domain/processagent 包 race 通过，app/api 的 Process 定向 race 通过。新增异步缓存竞态测试在修复前失败、修复后通过。可选真实 Rust 互操作依赖本轮未启用，不计入通过范围。

证据在远端 `web-maintenance-evidence/`；本机归档在 `artifacts/runs/2026-09-28-web-maintenance/`。保留修复前失败日志、最终测试/构建日志、截图及本批源码 SHA-256 清单，不以旧版本测试结果代替最终验证。

最终 `web-maintenance-evidence.tar.gz` SHA-256：`6e3deda0722abcf7c8fd04416f00ca7566c92ff24dc1ab22cdef7743d7d17b32`。

## 尚未验收

现场升级、开启现场维护绑定、Web 发布编排、agent 自身升级、真实设备命令、跨版本恢复、满负载 P95、1 小时/过夜长测。本批不重复未改变的 Rust 协议互操作，不声称整个 P2 完成。前端禁用仅为使用约束，服务端维护令牌/准入校验仍是最终保护。
