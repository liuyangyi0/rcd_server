# 46通信程序版本登记修复（2026-10-24）

## 结论

46的运维平台现已显示通信程序`0.2.0`、状态`running`（网页“程序运行”）。只补充已验证的版本档案并通过原生只读巡检刷新快照，没有替换或重启通信、管理端、采集、Agent或Semaphore，没有修改配置、点表、业务数据库或串口开关，没有发送硬件命令。30、58未做任何变更。

## 原因与证据

10月20日通信程序在标准四组件发布接入前已独立部署。原发布目录保留二进制及`source-files.sha256`，但没有巡检所需的`identity-public.json`。平台能确认进程运行及实际映像SHA，却没有可信版本映射，因此正确地显示`running_version_unknown`，不是程序未启动或版本回退。

现场进程PID`3974`、安装入口`/opt/rcd/bin/rcd_communicate`及原始发布产物三者SHA均为：

`32367696f6f480acfd7c8cd77e5b485d441503cf201d1b62e4399d989bdc2921`

原发布证据位于`/home/lenovo/code/rcd_names_release_20261020_jfvhg4/`。逐项核对142份原始源码及依赖锁文件，源码`Cargo.toml`版本和旧版只读`version`输出均为`0.2.0`；原始基线提交来自当次保留的构建清单。未用当前源码、当前Git提交或文件名猜测已运行程序版本。

新增维护工具`tools/release/register_legacy_identity.py`校验原始证据固定SHA、源码、ELF架构、运行映像和安装文件，只原子新增：

`/opt/rcd/releases/legacy-communicate-20261020-32367696f6f4/`

目录包含只读原始映像副本和公开版本清单。清单保留`provenance.kind=verified_legacy_source_record`，明确`embedded_build_identity=false`、`deployment_performed=false`；来源摘要是原始142项校验映射的规范化SHA，非虚构的历史内嵌构建标识。原构建时间无法确认，`built_at`留空，不用本次登记时间冒充。

## 验证

所有执行测试均在46隔离目录完成：

`/home/lenovo/code/rcd_communicate_identity_20261024_1eaRTg/`

- 新登记回归7项通过：摘要/源码损坏、路径越界及重复、运行映像不匹配、不可变登记冲突、重复执行幂等和档案损坏后不再提供版本。
- 原巡检回归10项通过：磁盘与运行版本分离、停止/未知、映像中途变化、身份冲突及隐私字段过滤等。
- 原生只读检查任务`68458`成功；北京时间2026-10-24 14:56:40确认通信组件`running`、运行/磁盘版本均`0.2.0`、SHA一致、`disk_differs=false`。
- 随后的自动检查任务`68461`再次确认版本，证明不是直接修改页面或数据库缓存。
- 真实浏览器登录46平台设备页，实际46行显示“串口通信程序”“0.2.0”“程序运行”；页面异常和除登录外的写请求均为0。证据`live-browser/result.json`、`device-46-version.png`，未使用页面数据夹具。
- 登记前后通信PID`3974`、`NRestarts=3`及配置摘要一致；管理端、Agent、Semaphore PID和相应服务状态亦不变。重启计数3是本次登记前已有值，本轮未分析其历史原因，不声称从未重启。

主要记录：`verified-identity-dry-run.json`、`registration-result.json`、`before-services.json`、`after-services.json`、`before-devices.json`、`after-device.json`、`inspection-accepted.json`、`result.json`、`identity-tests.log`、`inventory-tests.log`、`browser.log`及`live-browser/`。

## 边界

版本补登不等于升级，不制造成功安装/恢复历史，也不将旧二进制自动转成可升级软件包。通信组件仍缺独立发布profile，原兼容性检查提示保留，不能据版本已显示宣称一键升级/恢复已完成验收。后续标准版本仍应由正式构建工具内嵌完整身份、生成包并通过平台安装；不放宽新包校验。

串口继续停用。本轮没有真实串口收发、一小时观察或过夜验收；未提交或推送本轮新增工具和文档。
