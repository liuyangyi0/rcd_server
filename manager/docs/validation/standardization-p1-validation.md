# 软件标准化第一批：版本身份与显式路径

日期：2026-09-28。状态：本批源码与隔离验证完成；未现场部署，整个标准化计划尚未完成。

## 范围

用户授权开始执行修订后的计划。当前批次只处理软件版本身份和下级显式路径基础；不建立现场配置/设备故障基线，不改用户业务配置，不排查设备故障。

开发线 `codex/opc-device-groups`，保留此前所有未提交功能。未提交、未推送；未迁移现场路径、未替换程序、未重启服务、未发送设备命令。

## 改动

- Rust `build_info` 与 Go `buildinfo` 提供相同语义的 schema v1 构建信息，包括组件、版本、起点提交、release_id、源码摘要、构建时间、脏快照和目标平台。
- `rcd_server version --json`、`RcdManager version --json`、`RcdManager agent version --json` 在读取运行配置或启动服务前返回。保留文本查询。
- 管理端 overview、下级 Operations status 追加 `build_info`；agent 新增受原实例头校验的只读版本接口。既有字段不删除，OPC 点表和控制协议不变。
- 下级增加 config/state/pki/log 四类显式目录参数与环境变量，未指定时保留原默认位置。日志专用 `RCD_CONFIG` 语义不变。
- 运行状态与命令审计可以独立于业务配置目录；显式分离时缺少旧状态文件会拒绝启动，不自动生成空状态替代。配置内容不自动复制、覆盖或修正。
- 实例锁覆盖配置/状态位置，并继续遵守存在的旧 EXE 同级锁；相同规范路径去重，失败时释放已取得的锁。尚未进行跨目录现场迁移。
- 发布工具支持 agent 角色及版本/release_id 注入，并在构建结束核对实际二进制报告的信息。未完成计划中的新维护栅栏、系统级服务适配或保留最新用户配置的完整回滚改造。

参数和兼容限制见 [使用说明](../deployment/version-and-paths.md)。

## 验证环境与命令

全部构建、格式化和可执行测试位于 `192.168.10.46`：

`/home/lenovo/code/rcd_standardization_20260928_jFl8UD`

复用既有 Rust 编译缓存，不从该缓存启动现场服务；测试源、日志及产物独立。Rust 1.87.0，Go 1.26.0。本机仅编辑/检查源码、打包传输、取回格式化结果和核对摘要。

| 检查 | 结果 | 证据 |
| --- | --- | --- |
| `cargo test --locked --lib --bin rcd_server --test runtime_identity_cli -j2` | 库 371 通过、1 忽略；binary 11 通过；CLI 3 通过 | `final-rust-tests.log` |
| `go test -race ./...`，工作目录 manager | 全包通过；首轮 app 77.091 秒、opc 62.987 秒；最终复核使用缓存 | `go-race.log`、`final-go-race.log` |
| `python3 -m unittest discover -s tools/release -p 'test_*.py'` | 7 通过，包括身份映射、输入限制、agent 角色和已有备份恢复单元测试 | `release-tests.log` |
| 实际 Go 构建与 Rust debug 构建，注入同一软件身份 | 成功；分别回读 lower/manager/agent CLI JSON，与身份清单逐字段一致 | `identity-verification.log`、`identity-build/*build.log` |
| 本地对照远端实际构建清单 | 738 个构建输入文件摘要全部一致 | `identity-build/verification.json` |
| `git diff --check` | 通过 | 本次本地静态检查 |

Rust 忽略项为已有 `calc_engine::config::tests::load_user_calc_real`，未使用或修改现场计算配置。

新增覆盖包含：旧默认路径、相对显式路径、命令行优先于环境变量、无效/重复参数、缺目录不回退、旧状态遗漏保护、只读程序目录下锁路径选择、锁去重及部分获取失败释放、独立状态目录保持命令身份、版本查询不加载配置/创建运行目录，以及 agent 版本接口的实例校验。

所有涉及配置、数据库或命令的单元测试使用隔离临时数据；没有以单元测试或不存在的串口替代真实设备成功验收。

## 构建身份与产物

- 测试版本：`0.0.0-p1.20260928`。
- release_id：`rcd-standardization-p1-20260928`。
- 起点提交：`769ef2a1f82e6e7bb5e77f9e79a3e88434005e42`；`dirty=true`，不是新代码已提交。
- 实际源码摘要：`d60d0483c7d8c17e697a0860d9542bba135c1558abb3e88c01e03eb76172f8f7`。
- 下级 debug 二进制 SHA-256：`f6257ca60e3b7e77c3737d4f499698cd95df1dc2819507398e70e969e36f5108`。
- Go 二进制 SHA-256：`e109b5189f4d3febb267c8ecf0590a443e91993fe91e65bc2ba6a4215296c42d`；manager/agent 为同一文件的不同运行角色。
- 产物目录：远端隔离目录的 `identity-build/`。这些是验证用产物，不是本轮现场发布包。

本地证据在 `artifacts/runs/2026-09-28-standardization/`：

- `evidence.tar.gz`：`b94b8237bcf7882fef59279e3d5198d8fac73f94820fbbc1cd898d610e4ad323`。
- `final-verified.tar.gz`：`43ce3e2c398f13bb4971a892085bbd83a0dd9cb619ebefcf87a8e493e924be46`，仅本批 18 个源码/测试文件，不是全仓库快照。
- 两包远端与取回后的摘要一致。证据不包含现场数据库、配置正文或凭据。

## 未完成项与下一批

- 未运行完整的 `release.py build` 前端 npm 流水线；本批实际执行的是使用其身份注入函数的后端构建及回读校验。Go 嵌入资源复用此前未改动的 Web 构建，不将其算成本批前端构建或浏览器验收。
- 未产出新的 Rust release 优化包，未在 30/46/58 部署本批版本，未移动任何现场目录。
- 未新增完整的多主机版本对比界面、agent 下级路径参数传递配置，以及规范目录安装/迁移器；手工启动下级可使用新参数，agent/systemd 接入需在下一批完善。
- 发布工具旧回滚逻辑仍可能恢复备份中的数据库/配置；尚未满足修订计划中的“默认保留用户最新配置”完整要求，不能作为已完成的新发布流程上线。
- 未实现跨入口维护协调、Ansible、Semaphore 或 HTTP 运维迁移；这些与第一批基础能力分开交付。
- 未做新一轮真实 OPC Browse/Read/Subscribe、硬件命令、整机自启、性能 P95、一小时或过夜验收；没有将设备在线率作为本批软件测试门槛。

下一批先完成 agent/服务启动参数与路径接入、软件/数据分离的安装规则，再实现不会误覆盖用户配置的发布与恢复流程。现场替换仍需明确目标和维护窗口授权。
