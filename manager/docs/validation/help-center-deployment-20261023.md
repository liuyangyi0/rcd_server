# 图文帮助完整打包与三台平台部署（2026-10-23）

## 结果

已通过现有Semaphore原生队列 → Ansible → 发布器，按30 → 58 → 46升级管理端至 `0.6.19-help.20261023.1`。三台实际浏览器均打开10个帮助主题，加载全部19张JPEG，逐张核对源文件SHA256，并打开原图新标签页；全部通过，无页面错误。

| 主机 | 升级任务 | 帮助入口 | 主题/图片 |
| --- | --- | --- | --- |
| 192.168.10.30 | 64657 | http://192.168.10.30:8087/#/help | 10 / 19 |
| 192.168.10.58 | 64661 | http://192.168.10.58:8087/#/help | 10 / 19 |
| 192.168.10.46 | 64665 | http://192.168.10.46:8087/#/help | 10 / 19 |

部署前已获得本次控制暂停和管理端重启授权，验收完成已通知恢复控制。本次没有重启采集、Agent、通信程序或Semaphore，没有改变现场配置、端口、安全策略、OPC点表及采集时序，未发送硬件命令。前一版同名状态输出继续有效。

## 漏包原因与修正

0.6.18虽然发布了状态输入修复，但构建沿用旧远端源码副本、只覆盖部分修改文件，漏掉未跟踪的帮助页面、导航和图片。三台旧版实际入口JS没有HelpPage分包，图片请求返回404，不是浏览器缓存问题。

本次将完整 `manager/web` 源码（含App、导航、新页面、测试和public/help）整体归档；排除node_modules、dist等生成目录。46保留原前端目录作证据，在新的空目录解包，防止旧文件混入。传输前后归档SHA256一致：

`b0745864cfc3ccea5baba5f6b0e9f381e682eb89e5f641d410ce2d40168dcc0f`

发布工具增加完整源码门禁，缺少帮助路由、导航、组件、资源测试或引用JPEG则构建失败。正式Go测试启用 `RCD_VERIFY_WEB_HELP=1`，检查实际嵌入的HelpPage分包、全部19张JPEG的源文件哈希、解码尺寸和HTTP响应。普通源码Go测试仍可使用已有dist占位，发布构建不得跳过检查。

## 实际验证（均在46）

- 发布工具：`python3 -m unittest discover -s source/tools/release -p test_release.py`，22项通过，含不完整源码、缺图、假JPEG拒绝测试。
- 正式发布构建：npm ci、全量前端测试、TypeScript/Vite生产构建、带资源门禁的 `go test ./...`、管理端构建及发布身份校验，退出码0。
- `TestEmbeddedHelpBundle` race检查通过：实际Go内嵌19张源文件一致的JPEG、HTTP类型/内容和帮助分包。没有以仅存在于源目录的图片冒充已内嵌。
- 候选二进制在隔离空数据目录、随机回环HTTP/OPC端口启动，实际HTTP请求帮助分包及全部19张图片，哈希一致；随后正常退出。未连接现场下级、未打开串口。
- 隔离浏览器通过：10主题、全部真实图片、1366/1920/390布局、全文搜索/分类/空态/历史、目录HashRouter、复制NodeId及完整39项JSON；仅只读应用壳夹具，不冒充现场硬件测试。
- 现场浏览器逐台通过：现有登录、帮助导航、10主题、全部19张图像解码和哈希、原图新标签页、1366×768和1920×1080无页面横向溢出，页面错误及业务写入尝试均0。

三台健康及就绪检查通过，来源连接3/3、3/3、4/4，在途命令0；活动计算hash、revision与1907条公式数量不变。采集和Agent的PID/实际映像不变，141/141/97份下级配置文件哈希不变，46的Semaphore及通信程序也未重启。新管理端NRestarts均0。未执行或声称完成一小时/过夜观察。

## 发布身份与证据

- 二进制SHA256：`c6cfc27ea1c6ddb5f89779d4308ce5bf181755b57d0618df6d96277a7b6aa1c7`
- 发布包SHA256：`dd779e8da8562c9083ec32fd6bc3ed52f641e396b069a7881f3e59589cc7fdad`
- 源码清单SHA256：`901b03addd0accb5373b878109144e503337af6e2799777a2977e92149cce3a0`
- 平台包ID：`58bf7b9d9cd844c9c61c87e79e67c6510e6bb298fa17fa6ebbc906a0dc5ca8121`
- 基线提交 `769ef2a1f82e6e7bb5e77f9e79a3e88434005e42`，实际使用记录了逐文件摘要的脏源码快照，未提交或推送。数据库schema仍为16，无迁移。

46证据目录：`/home/lenovo/code/rcd_help_bundle_20261023_R3x19w/`。

主要文件：`release-tests.log/.exit`、`build.log/.exit`、`race.log/.exit`、`embedded-probe-result.json`、`isolated-help-browser/result.json`及截图、`live-help-browser/result.json`及三台截图、`upgrade-*-intent/accepted/result.json`、`before/after-fleet.json`、`after-manager-*.json`、`postdeploy-identity-verification.json`、`manager-build-result.json`和`package.json`。原生发布器随任务保留备份与事务记录。

图片是2026-10-23约14:50–15:00的真实页面快照；页面标明日期及历史性质，不将图中来源名称、任务、版本和物理状态冒充当前实时值。原阶段设计与截图来源见[隔离验证记录](help-center-20261023.md)。
