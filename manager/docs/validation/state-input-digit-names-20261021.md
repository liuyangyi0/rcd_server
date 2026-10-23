# 数字开头状态输入名称修复（2026-10-21）

## 修改范围

`state_inputs.name`现支持`9CYE91GH201_COM1`、`11_STATE`等数字开头的名称，不自动加前缀或改写用户名称。

- 后端名称校验与现有表达式标识符规则对齐：1–128个ASCII英文字母、数字、下划线，至少含一个字母或下划线。纯数字、IF/true/false保留字（不分大小写）、非法字符、超长名称仍拒绝。
- 表达式词法解析本来已支持数字开头KKS，本次不改解析器或数字常量语义。
- 网页保存和“插入公式”共用同一个名称校验；增加输入提示，保存仍检查revision，保存草稿不自动激活。
- 状态输入之间、与原始点/命令或计算输出重名时，仍由原有校验拒绝。只定义别名不自动新增业务OPC节点。
- 更新[状态输入说明](../guides/calculation-state-inputs.md)及[注释示例](../examples/calc-station-state-commented.toml)，给出58来源WCH0、11号站点的完整示例。

## 回归覆盖

后端新增名称边界、纯数字/保留字拒绝、表达式变量与数字常量区分、TOML读写往返、状态0–5求值、增量依赖及重名测试；API测试覆盖网页结构化映射保存及revision；运行时测试覆盖保存/激活、检索映射和下级离线时状态5仍为Good。

前端覆盖旧名称和数字开头名称保存、非法名称不能提交、原样插入表达式并保存；没有绕过草稿校验或自动激活。

## 46隔离验证

目录：`/home/lenovo/code/rcd_state_names_20261021_XAR6Ti`。

| 命令/检查 | 结果 | 证据 |
| --- | --- | --- |
| `go test -race ./internal/calculation ./internal/app ./internal/api -count=1` | 三包通过；1.155s / 120.031s / 26.381s，退出码0 | `go-race-regression.log`、`go-race-regression.exit` |
| `npm test -- src/features/calculations/stateInputNames.test.ts src/features/calculations/StateInputsTab.test.tsx src/pages/CalculationsPage.test.tsx` | 3个文件、37项通过 | `web-targeted.log` |
| `npm run build` | TypeScript与Vite构建通过 | `web-build.log` |
| `go build -o <隔离目录>/RcdManager-state-names-isolated ./cmd/rcd-manager` | 通过，包含本轮前端构建 | `manager-build.log`、隔离二进制 |
| 涉及Go文件的`gofmt -l` | 无输出，已规范格式 | 远端终端记录 |
| 指南TOML代码块及注释配置语法、数字开头示例目标和表达式 | 通过 | `documentation-check.json` |
| 本机`git diff --check` | 通过；仅原有LF/CRLF转换提示 | 本机终端记录 |

隔离二进制SHA-256：`f5393415f5570c05528c5a3761b5a4b49d6595ebee597fe7b7aec9907a5acfb55`。这不是现场发布包或已部署版本标识。

所有构建和可执行测试均在46完成，本机只读写源码、检查差异与传输文件。隔离目录复用既有依赖/构建缓存，不替换运行文件。

## 隔离验证阶段的边界

未部署、未重启、未修改现场计算配置，未连接真实串口或发送硬件命令；未做本轮真实浏览器视觉或长期稳定性验收。现有OPC NodeId、计算取源、状态含义及安全策略不变。现场旧版本仍会拒绝数字开头的状态别名，需另行部署后使用。

## 2026-10-21 三台管理端正式发布与验收

本节是随后取得本次控制暂停、三台管理端重启授权后的现场结果，不改变上文隔离阶段的历史边界。仅更新管理端，按30→58→46逐台完成；未更新或重启下级采集、Agent、Semaphore及46通信程序，未发送硬件命令。

发布使用现有构建工具及原生运维平台：`tools/release/release.py build` → `rcd_package.py`生成单组件包 → Semaphore官方定制页面上传 → 原生任务队列 → Ansible → 现有发布器。没有通过SSH直接覆盖现场二进制；每台只提交一次发布请求，保存幂等编号和回执，没有重投。

- 版本：`0.6.12-state-names.20261021.1`，linux/amd64。
- 基础提交：`769ef2a1f82e6e7bb5e77f9e79a3e88434005e42`。包含已有未提交修改，清单明确记录`dirty=true`及逐文件摘要，不冒充干净提交构建。
- 源码摘要：`181666cbb0dd6fd67ad6c90b45021e2e80aac924566377f94afa82c3995f8681`。
- 二进制SHA-256：`4084a28393471e93d43c121c0779a3d0d88fa49246b453fbad65f34cba758ca7`，三台实际`/proc/<pid>/exe`均一致。
- 发布包：`manager-0.6.12-state-names.20261021.1.rcd.tar.gz`，SHA-256为`ab47350b77f429f81258ed7b9be8d0dbd2f38071dfe23fa54031bd9912c3ec45`。
- 平台包ID：`880100b4af3e442f40888de40535668a8a439e7008395df8aa0875bed938ba66b`。
- 正式构建使用既有工具执行前端测试/构建、Go全模块测试及版本检查，返回成功；此前专项race及37项前端结果见上文，不将两批测试次数相加。

| 目标 | 原生任务 | 维护收尾时间（北京时间） | 新管理端PID | 点数 / 来源数 | 结果 |
| --- | --- | --- | --- | --- | --- |
| 192.168.10.30 | 55869 | 15:57:01 | 1716958 | 5297 / 3 | 成功 |
| 192.168.10.58 | 55882 | 16:01:03 | 1603002 | 5297 / 3 | 成功 |
| 192.168.10.46 | 55892 | 16:04:05 | 2339731 | 5355 / 4 | 成功 |

三台任务均确认维护、停止、备份、安装、软件验收和维护完成阶段。浏览器核对确认框必须勾选本次控制暂停、只提交一次升级、页面任务及运行版本一致；1366/1920宽度检查无页面横向溢出，浏览器未捕获异常。此次平台上传和三次升级链路没有发现产品错误；未据此宣称所有故障恢复场景已验收。

逐台现场验收：

- 网页新增状态输入使用`9CYE91GH201_COM1`、固定`rcd92-58 /dev/ttyWCH0 / 11`，保存按钮可用；纯数字名`11`不可保存。核对“插入公式”入口保留原名称。取消编辑，不保存现场草稿。
- 仅调用无持久化副作用的配置校验接口：数字开头名称有效，纯数字无效。来源定义、组默认源、活动计算revision/hash及草稿revision/hash与发布前相同。
- 真实4840端点完成Browse/Read/Subscribe；核对旧选源变量及新增SelectSource的标量String/UInt64签名。订阅58来源WCH0、11站点CommunicationState，状态值0–5且质量Good；读取业务点，不以物理设备坏质量判定软件失败。现场未调用切源Method或执行OPC写入；CAS实际调用、冲突和Session隔离由上文链接的隔离测试覆盖。
- 16:07的三机`healthz`/`readyz`均200、所有持久化和目录检查为ok、pending为0、配置来源全部连接。4840/4841/8087可达，46的4842和3000亦可达。
- 三机管理端NRestarts均0；对比Agent及其下级子进程的PID、启动代次、实际映像SHA完全未变，46通信程序和Semaphore亦未变。管理端journal包含本次正常停启及INFO记录，所读发布窗口未见WARN/ERROR、panic或异常退出。

证据根目录：46的`/home/lenovo/code/rcd_state_release_20261021_oKfms5`。包含正式构建清单、`release-manager/`包、`browser-upload/`、`browser-upgrade-{30,58,46}/`原生请求和结果、`before-web/`与`after-*-web/`、`opc-{30,58,46}.jsonl`、`before-confirmed-fleet.json`、`after-fleet.json`、`deployment-summary.json`及截图。首次只读预检因46自连缺少known_hosts而中断，保留该部分结果；改用本机只读sudo后完整检查通过，未绕过主机信任。没有提交或推送。

部署验收结束后已通知可恢复控制命令；本次没有完成一小时/过夜观察、历史版本恢复或真实串口发送测试，不将短时验收写成长测通过。

### 恢复版本的配置兼容边界

本次数据库schema保持16，不代表旧二进制能解析新配置。**一旦激活含数字开头状态输入名的配置，不可只凭schema相同恢复到不支持该名称的旧管理端**，否则旧程序可能在启动加载活动配置时失败。应先在当前版本备份并改为旧版本可解析的配置、校验和激活，再按受控流程恢复程序，或使用明确包含本修复的版本。本次仅校验、取消编辑，没有激活新配置；没有修改平台恢复校验或自动覆盖用户数据库。
