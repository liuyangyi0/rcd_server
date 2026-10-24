# 原始变量名与Fire状态编号验证（2026-10-20）

## 已实现

- 收发业务节点逐字沿用FromUnit文档：`DATA.Buildings.91UBH.Fire`、`DATA.AKONs.1CYE18GH204.HW`、`DATA.PumpStationCtrl[0]`等，不拼接任务、串口或方向前缀。
- 通信模块的点表/内部字段统一使用name，旧kks表头仅保留读取兼容；不修改原采集程序的KKS接口。
- 每个Fire只发布一个UInt32。1–8为正常、红色报警、红色报警闪、黄色报警、黄色报警闪、故障报警、故障报警闪、闭锁；0表示接收未知。发送同一数值自动还原五字节，不接受0或非法枚举。
- 按91BAOWEIFENXI.xls Sheet2第1–6/16行及Sheet1完整报文核对。开路、短路、手动与故障编码相同，只能映射到6或7；未伪造独立原因。
- 两份接收示例点表分别15/40个变量；发送分别39/14个输入，Header由发送方向自动填写。原文方括号保留，不替换成点号。
- 管理端按目录权限转发原名UInt32/String及TransmitSnapshot；保留epoch、组选源和维护保护，不进入采集命令FIFO或ACK流程。仍能识别旧RcdSync节点以便分批升级，通信程序自身不再生成旧业务名。
- 完整原子提交、5秒输入刷新、15秒输入过期、串口错峰和帧校验语义保留。辅助节点改为TransmitSnapshot、TransmitStatus、InputRefreshIntervalMs、InputTimeoutMs。
- Fire含义写入示例CSV、下级与管理端OPC Description和[客户端接入说明](opc-client-guide.md)。完整14项JSON示例经过编码测试。
- 原名重复或多个同步端口冲突时明确拒绝，不自动加前缀。9→2名称清单的差异已核对；缺完整布局，继续明确不支持，不能套用9→1偏移。

## 远端验证

全部构建和执行测试在46的隔离目录：

`/home/lenovo/code/rcd_variable_names_20261020_FDQSNS`

| 检查 | 结果 | 证据 |
| --- | --- | --- |
| `cargo test --locked --all-targets -j2` | 58项通过 | rust-tests-final.log |
| `cargo clippy --locked --all-targets -j2 -- -D warnings` | 通过 | rust-clippy-final.log |
| `cargo build --locked --examples -j2` | 通过 | rust-fixtures-final.log |
| `go test -race ./internal/app ./internal/opc` | 两包通过 | go-regression-final.log |
| `RCD_SYNC_FIXTURE=... go test -race ./internal/app -run TestSynchronizationInputsThroughManagerToRust -count=1 -v` | 通过 | manager-rust-wire-final.log |
| `go test ./...` | 全模块通过 | go-all-final.log |
| Rust服务＋真实OPC客户端：None与Basic256Sha256/SignAndEncrypt | Browse/Read/Write/Call/Subscribe通过，各58个业务/辅助变量 | opc-fixture-checks/report.json、各端点probe日志 |

关键覆盖：原始TXT黄金点名；8种状态的工作簿黄金帧及五字节双向转换；未知状态；非法数值/类型不改变已接受值；所有字段完整性；CAS、并发原子快照、禁止混写；输入缺项/续期/过期/恢复；原始点权限；经过管理端的Fire数值写入、Description和完整快照。

原始工作簿只读取，未修改。使用独立回环端口和软件夹具，不打开真实串口。Rust构建缓存复用远端既有target目录，不替换运行文件。首轮严格lint发现一个遗留未使用import，已清除；首次Go全模块测试因隔离源码未带contracts夹具失败，补齐原始夹具后通过，未改产品逻辑规避失败。

## 上一轮发布状态

上一轮仅修改与隔离验证，未部署或重启现场程序。该改动包含NodeId和Fire类型/点数变化，需同步升级管理端、通信程序及接收点表，再让客户端按新文档选点；不能只替换程序却继续加载旧点表。

## 后续授权部署与验收（46，2026-10-20）

用户随后明确要求“通信程序、管理端和点表一起更新，串口继续保持停用，部署后测试”。本次仅操作46；未更新30、58，未重启原Rust采集、Agent或Semaphore，未发送硬件控制报文。

正式构建、备份及验收目录：

`/home/lenovo/code/rcd_names_release_20261020_jfvhg4`

### 正式产物

| 对象 | 版本/文件 | SHA-256 |
| --- | --- | --- |
| 通信程序 | `rcd_communicate 0.2.0` | `32367696f6f480acfd7c8cd77e5b485d441503cf201d1b62e4399d989bdc2921` |
| 管理端 | `0.6.11-names.20261020.1` | `fbb9821ac33ddde2402a68e8a740e1dfc9c4ebfaac8c9e743ebf9815c6e6e119` |
| 接收点表 | `unit1-receiver.csv`，15项 | `c18f3b0b53f2032fd1de54c9999841a2b01164abc0e0097ee2f555579f50ea7b` |
| 管理端发布包 | `manager-0.6.11-names.20261020.1.rcd.tar.gz` | `e85f07fda9139f08e90eb6babedca8814b984eb7993a2e0f54a17b8f1199f008` |

基线提交仍为`769ef2a1f82e6e7bb5e77f9e79a3e88434005e42`，包含已有未提交修改，未提交或推送。管理端构建清单记录`dirty=true`及源码摘要`d2ae6c950e091755010f5b96d1c88bb43967b47fbebd9f6e0373fea575a37632`。通信产物、点表及输入文件摘要记录在`release-communicate/SHA256SUMS`、`source-files.sha256`；源快照保留`source-overlay.tar.gz`。

正式构建在46再次执行：Rust 58项all-targets测试、strict clippy和release构建；Go全模块测试与带版本身份构建；前端36文件/246项测试及生产构建。全部通过，分别见`communicate-build.log`、`manager-build.log`。上一轮已通过的race及双库写入互操作记录仍见上节；本次没有把只读上线检查写成新的现场串口写入验收。

### 部署过程与保护

1. 上线前网页快照确认四个来源Connected、目录冲突0、pending命令0。记录原进程、配置摘要、端口和服务状态。
2. 通信程序尚未纳入平台的三组件发布类型，使用仓库中限定46/停用实例的`deploy-disabled-46.py`更新；未伪称该步骤通过Semaphore。工具使用宿主机共享发布锁，先以新程序校验成套配置，备份原二进制、config、state和PKI，停服务后校验配置CAS，再替换二进制和显式指定的points.csv。`config.toml`及服务定义不改。备份在`communication-backup/`，状态记录为`started`。
3. 通信服务于北京时间20:00:10恢复，PID798598；新业务目录54项加4个辅助节点。旧管理端热刷新暂保留264个退役旧节点，符合现有目录语义，未改数据库规避退役规则。
4. 使用远端浏览器实际操作Semaphore原生软件包上传和单台升级表单，任务**52283**、操作编号`f3d940bcba1e4c9ad3fbf2839ff5193449245c313fa92e694ba800681a7f041c`，结果succeeded。管理端20:02:02完成启动，PID801399；重启后的新目录只保留58项通信节点。请求、备份/验收阶段、运行摘要和页面截图见`deployment-browser/{upload,upgrade}/`。

原通信主配置SHA-256前后均为`546f46dc249e72661ff0d4c3c5376ef28672d2d47be41e691249cfeeddb0d819`。固定实例、4842、停用端口、停用任务、transmit_enabled=false及占位设备路径均保持。原采集PID2777575、Agent PID3333828和Semaphore PID429419未变。

### 上线验收结果

- `opc-original-names.json`：对通信4842和管理4840分别逐项读取54个黄金业务名、BrowseName、DataType、AccessLevel、Value和Fire Description。接收15项只读，发送39项可写；Header只读，方括号原样保留；抽查旧前缀NodeId返回BadNodeIdUnknown。
- 两端TransmitSnapshot Read/Subscribe通过，通知Good；required_inputs=39、received_inputs=0、complete_writes=0、transmit_enabled=false。刷新/过期值仍为5000/15000ms。54个业务值均保持停用质量BadOutOfService，未伪装Good。
- `web-after/`：浏览器真实登录、筛选原名点位、查看成套配置，在1366×768/1920×1080保存截图并人工查看。配置和运行版本一致、CSV表头为name、无旧rx9前缀、未保存配置。页面脚本错误0。
- `bindings-unchanged.json`：来源稳定ID、名称、固定IP、协议、revision和组默认源前后相同。四条连接均Connected/Active/Active；通信58项订阅，原三个来源各3300项。管理目录5355项（原5297加新通信58），目录冲突0。
- healthz/readyz均HTTP 200，持久化及目录检查均ok。`after-services.txt`确认通信进程没有串口设备fd，原采集仍持有原4841服务，新通信仅4842，管理端4840/Web8087不变。
- 截至20:08:42复核通信/管理端仍为PID798598/801399、NRestarts均0，实际`/proc/<pid>/exe`摘要与发布产物一致；所查部署窗口应用日志与journal没有新增WARN/ERROR、panic或自动重启。见`final-services.txt`、`manager-application-window-final.log`、`communication-application-window-final.log`和`postdeploy-journal-final.txt`。

验收脚本曾因REST列表包装、热退役节点计数及带引号CSV表头断言失败，修正脚本后通过，保留失败截图；没有因此修改产品、重复发布或重启服务。首次通信部署参数的摘要长度校验在任何写入/停机前拒绝，改为读取已记录产物摘要后正常执行一次。

本次验收为真实线上服务的只读OPC/Web/运行身份检查；业务写入、帧编解码和原子提交由46隔离回归覆盖。串口及发送始终停用，实际物理角色和独占串口尚未确认，不宣称已完成真实双向收发、一小时或过夜长测。客户端连接4840后需按[接入说明](opc-client-guide.md)重新选择原始DATA节点。

## 客户端说明交付校验（2026-10-20）

按用户要求将`opc-client-guide.md`整理为面向OPC客户端人员的独立使用说明，逐项列出当前46的15个接收变量和39个发送输入，补充Session选源、类型/状态编号、单点写入、完整39项/反向14项JSON、刷新/过期、原子模式、状态监视与失败处理。不修改产品运行逻辑、配置或服务。

校验在46隔离目录`/home/lenovo/code/rcd_client_guide_20261020_EVJHvI`完成：

- `cargo test --locked --test unit_reports -j2`：9项通过，包括直接提取文档中的两份JSON并调用现有协议编码器；见`document-examples-test.log`。
- `markdown-contract-check.json`：15/39行表格逐字匹配原始TXT清单，39/14项JSON字段完整、无重复键、数值类型/范围和定长十六进制均有效，请求分别3398/622字节，均小于64KiB。
- 文档SHA-256：`8f63da2af22e12c908976e4b1ef18b575f74a2120d184495d28f6cf2b2b0bebe`。

本轮只更新说明及其示例回归保护，未部署、未重启、未操作真实串口或发送硬件数据。
