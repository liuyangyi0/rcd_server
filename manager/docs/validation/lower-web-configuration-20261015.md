# 下级网页配置与参数表单：2026-10-15 隔离验证

## 结论和范围

已实现现有 RCD 与新 Communicate 的网页配置链路；**未部署到现场**。本轮仅在 46 隔离实例写测试配置、启动和停止测试子进程；没有修改三台机器的现场配置、重启现场服务、发送硬件命令或提交推送。

- 工作树：`C:/Users/here/RustroverProjects/rcd_server_device_groups`，`codex/opc-device-groups`。已有未提交改动保留。
- 测试主机：`192.168.10.46`。
- 证据根：`/home/lenovo/code/rcd_config_web_20261015_gxtCJS`。
- 工具链：Rust 1.96.0、Go 1.26.4、Node 24.14.0；本次未验证 Windows/musl 或声明的最低 Rust 工具链。
- 用户已明确允许新通信程序沿用现有匿名 OPC 运维方式，仅限受信任内网；未以 Web 登录冒充 OPC 访问认证。

## 修改内容

1. 共享 `crates/rcd-config-form`：用 TOML 语法树生成类型化参数及编辑草稿，保留注释；无文件 I/O。不手写 TOML 正则，不复制两套编辑器。限制路径、类型、枚举、重复字段、深度、字段数和生成后的大小。
2. RCD 增加 `configuration_forms` / `prepare_config`。公开命令最大总下发次数、轮询补发、超时、间隔和保护时间等已有配置。未配置的五项默认参数只展示，修改时才落入草稿。
3. 修复 RCD 处于 `calculation.mode=manager` 时，未使用的 `calc.toml` 阻止保存其它参数的问题；切回 local 必须重新校验计算文件。未改变计算求值或业务协议。
4. Communicate 增加配置专用 Operations v1：仅两份启动时绑定文件，身份/路径不可远程更换，联合校验、SHA-256 CAS、备份和原子替换。校验与启动采用相同 TOML 大小限制，拒绝链接文件和保存期间的配置集变化。保存不改变运行态、不重启。
5. 管理端增加 `POST /api/v1/sources/{id}/operations/config/prepare`，固定来源 revision，并校验下级返回的字段结构。不增加第二套保存接口。
6. Web 增加搜索、参数表单、枚举下拉、本地文件载入；保留 TOML/CSV 原文、下载和备份恢复。参数草稿和文件草稿分阶段确认，后台刷新不覆盖编辑。配置专用下级默认显示配置页，不请求不支持的采集状态。
7. 发布源码摘要纳入共享 Rust crate；历史 musl 复制入口追加 `crates/`。本次没有运行该历史 Docker 脚本，不宣称其整体已验收。

## 自动化验证

以下均在上述远端隔离副本执行，未在本机构建或测试。

| 检查 | 实际结果 | 证据（相对根） |
| --- | --- | --- |
| RCD `cargo +stable test --locked --offline --lib operations::files::tests` | 22 通过，367 项未选中 | `acquisition-locked.log`、`acquisition-locked.exit=0` |
| Communicate `cargo +stable test --locked --offline --all-targets` | 30 通过，无忽略 | `communicate-locked.log` |
| 共享 crate `cargo +stable test --locked --offline -p rcd-config-form`（在 communicate 构建根） | 4 通过 | `forms-locked.log` |
| Communicate `cargo +stable clippy --locked --offline --all-targets -- -D warnings` | 通过 | `clippy-locked.log`、`locked-final.exit=0` |
| Go domain/api/app/opc，筛选 `Operation\|ConfigDraft\|PreparedConfiguration` | 四包通过 | `go-final.log` |
| 上述 Go 测试加 `-race -count=1` | 四包通过 | `go-final-race.log`、`go-final.exit=0` |
| Web 三个相关测试文件 | 37 通过 | `web-final.log` |
| Web TypeScript/Vite + Go 管理端编译 | 通过 | `web-build-final.log`、`manager-build-final.log`、`web-final.exit=0` |
| 发布器源码清单共享 crate 回归 | 1 通过 | `release-manifest.log`、`release-manifest.exit=0` |

Web 文件为 `ConfigParameters.test.tsx`、`SourceOperationsDrawer.test.tsx`、`api/operations.test.tsx`。覆盖参数错误保留、整数校验、旧版兼容、配置专用能力、版本冲突及原有草稿保护。

## 真实软件浏览器链路

使用本轮编译的 Go 管理程序与 Communicate OPC 服务，独立数据/PKI、随机回环端口、全部串口停用。不是静态 UI 假接口，也不属于真实串口验收。

最终界面证据：`browser-gIWANI/result.json`，执行日志 `browser-final2.log`，退出码 `browser-final2.exit=0`；无 JavaScript 页面异常。

已走通：登录 → 配置专用来源 → 参数修改 → 刷新仍保留草稿 → 生成草稿 → 校验 → 保存 → 核对文件注释、自动备份、启动版本与磁盘版本 → 本地文件导入 → 下载 → 只重启隔离下级 → 确认新文件生效、待重启标记清除。另验证旧文件 revision 返回 409、来源 revision 不符返回 409、路径越界拒绝、未实现协议启用拒绝。

已检查 1366×768、1920×1080、390×844；截图为 `configuration-desktop.png`、`configuration-wide.png`、`configuration-mobile.png`。表单在窄屏变为单列，无页面水平溢出。高频来源刷新不重新挂载脏编辑器。

中间失败日志保留：早期浏览器脚本遗漏 HashRouter 的 `#`，修正后通过；一次最终首次连接等待超时，诊断运行显示隔离下级尚未监听时 `connection refused`、管理端重连后正常，随后全流程通过。未将失败运行计为成功。发布清单测试首次因隔离副本缺少 `deployment.py` 无法导入，补齐已有源码后通过；没有改写测试断言规避失败。

浏览器最终通过后仅修正 RCD 日志等级提示（明确重启生效）、补充大小边界回归和格式化 import；这些变化已由最后一轮 Rust 测试覆盖，未宣称再次进行全部浏览器验收。

## 尚未验收 / 使用边界

- 现场三机升级、配置保存和安全重启未执行，需要当次部署维护授权。
- 新通信程序实际查询/响应解析仍待协议；不能启用 pending 任务，不提供猜测的发送命令。
- 新通信程序 Agent 安全生命周期、日志推送与平台绑定不在本次完成范围，不能因能保存配置就声称能安全网页重启。
- 数组增删和点表仍用原文编辑；不是所有缺省可选项都有表单。超过 2 MiB 的点表不支持现有网页传输。
- 本轮没有硬件、长时间运行或性能验收，不使用本次结果代替那些证据。

使用入口与参数解释见[下级配置操作说明](../guides/lower-operations.md)。
