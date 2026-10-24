# 数据流监视：46隔离验证（2026-10-24）

## 范围

在 `codex/opc-device-groups` 实现免登录只读 `/#/flow` 页面，以及两个限定字段的公共读取接口。视觉采用宽弧形环带、反向旋转刻度、发光粒子和长拖尾。第一轮普通拓扑效果未获用户认可，已重做视觉，不将该轮截图当作最终效果。

所有构建、测试和浏览器运行均在46：

`/home/lenovo/code/rcd_public_flow_20261024_a6OGud/`

隔离程序使用生产数据库的一致性副本及独立生成的PKI，HTTP `18087`，OPC仅回环 `14840`。未替换或重启现场管理端、采集、Agent、通信程序或Semaphore；未修改现场串口、业务配置或安全策略。现场管理端仍为8087/4840。

## 验证

| 检查 | 结果 |
| --- | --- |
| Go API/App：新增公共接口、样本计数、Session隔离、站点新鲜度、并发读取；附带原登录和数据质量回归 | `go test -race ./internal/api ./internal/app -run 'TestPublicFlow\|TestWebAuthLifecycle\|TestRuntimeSessionIsolationAndDataValueRules' -count=1` 通过 |
| fork订阅诊断 | `go test -race ./server -run TestSubscriptionDiagnosticsSnapshotConcurrentAddDelete -count=1` 通过；公开方法返回已有锁保护的副本 |
| 前端模型、页面、原登录与易用性 | 4个测试文件、12项通过；包括旧查询缓存不冒充新快照、下级离线不计为现场设备故障 |
| 前端构建及单EXE嵌入 | `npm run build`、`go build ./cmd/rcd-manager` 通过 |
| Chromium实际页面 | 1920×1080、1366×768；匿名查看、点值/串口筛选、键盘关闭详情、圆环像素变化、暂停静止、播放恢复、断线快照提示、自动重连、进入管理需登录均通过 |
| 权限边界 | 公共GET/HEAD可读取；来源、Session、命令、配置和原SSE匿名请求返回401；公共POST拒绝；没有公共写接口 |
| 多观看者 | 公共快照每秒共享一次生成；慢HTTP读取不持有生成锁，不阻塞其他观看者 |

前端测试命令：

```sh
npm test -- src/features/flow/model.test.ts src/pages/FlowPage.test.tsx \
  src/components/AuthGate.test.tsx src/components/Usability.test.tsx
```

可复用的浏览器验收脚本为 `tools/opc/public-flow-browser.cjs`，限定隔离端口18087，运行时指定46既有Playwright模块。最终截图、录像和12项浏览器结果保存于远端 `browser-r2/`；Go及Web日志分别为 `go-test-final.log`、`web-tests-final.log` 和 `web-build-final.log`。最终二进制为 `rcd-manager-flow-final-r2`，SHA-256：`e64c125f51acc5d5d35d2762e925a8f6215a68e5be1c90e5327bcabcc47f4edb`。最终浏览器检查时间为北京时间18:05:44；汇总记录为同目录 `acceptance-r2.json`。

## 真实连接及边界

隔离管理端实际连接 `rcd-30`、`rcd-46`、`rcd-58`、`communicate-46` 四个现有下级。原有物理站点异常继续原样显示，不计为软件构建或连接失败。

另外创建3个名称明确为“数据流验收客户端”的真实匿名OPC Session，在隔离管理端分别选择三台采集来源并建立订阅，核对各自路由和实际通知计数增长。为产生可观察的订阅变化，验收客户端订阅标准ServerCurrentTime；这不代表已有上层组态负载，也不作为现场设备动作验收。仅改变了隔离Session的选源，没有向下级发送任何硬件控制报文。验收客户端完成后关闭或自动到时退出。

录像为真实浏览器捕获，非生成图片。免登录页使用真实接口，不导入测试样例数据。输入粒子代表收到样本的来源级活动，输出粒子代表订阅**生成通知**；不是逐串口报文计数，也不是客户端接收确认。

46原有管理端、Agent、通信程序及Semaphore的PID、NRestarts与测试前基线一致，健康检查通过。通信程序与Semaphore原有的NRestarts=3未被改写成零。

临时预览按8小时自动退出；不是正式平台发布或自启动安装。尚未做一小时/过夜稳定性和目标现场浏览器性能验收，不据此宣称长期性能通过。后续正式发布仍使用运维平台，并另行确认管理端维护窗口。
