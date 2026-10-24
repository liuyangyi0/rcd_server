# 发布维护主流程验证（2026-09-28）

本批是 P2 第三小批：接通下级发布主流程，增加持久阶段记录和中断只读核对。**没有现场部署、重启现场程序、发送设备命令、提交或推送。** 不是整个 P2 完成，也不是硬件验收。

## 改动

- `tools/release/deployment.py` 单独负责下级带栅栏发布；复用原构建、产物校验、备份和软件验收，不复制一套进程控制实现。
- 要求显式 lower/process-agent 目标、gateway/endpoint 绑定、唯一 deployment ID 和操作者标签。agent GET 返回固定绑定，发布器在第一次 enter 前核对。HTTP 禁止重定向、代理环境转发，响应上限 1 MiB；不改变服务端认证策略。
- 持久记录 intent 后才执行网络修改或替换；stop/start 使用固定作业编号和同一 token，丢失回复不重投、不回退旧接口。阶段记录只保存允许的结果字段/异常类型，不收集任意错误正文或配置内容。
- 排空后停止，再备份；备份和原子替换同步文件/目录，安装前再次核对摘要与停止状态。启动后检查实际运行映像、TCP 和新鲜的下级 OPC 身份/维护证据，再完成维护。
- EXE 同目录的发布锁/未完成标记防止跨发布目录、换作业 ID 或普通回滚绕过。`inspect-deployment` 只 GET：核对 profile、产物、入口/运行映像、agent 缓存和原进程请求编号；不会解除维护或恢复旧数据。
- agent 状态 v2 禁止走旧的普通 agent 升级/回滚路径；备份须含 process-state.json。旧 maintenance.sqlite 代次禁止经业务数据恢复开关覆盖。
- 配置内容不纳入 profile 摘要，不要求使用人停止日常修改配置；维护期间写入口仍按既有栅栏暂停。发布过程保留本机当前配置，不用其它主机配置覆盖。

## 环境和命令

全部可执行测试在 `192.168.10.46` 的隔离副本：

`/home/lenovo/code/rcd_release_maintenance_20260928_JsWlBI`

本机只编辑、检查差异与传输。Rust 本批未修改，联调使用前批已构建的开发二进制 SHA-256：

`b82c63470e7c652450b155f6138a6fa20e0835955645c695b204ae6794b71f8c`

```sh
python3 -m unittest discover -s tools/release -p 'test_*.py' -v
cd manager
RCD_MAINTENANCE_INTEROP_BINARY=<隔离Rust二进制> \
  go test -race ./internal/processagent -count=1 -v
RCD_MAINTENANCE_INTEROP_BINARY=<同一隔离Rust二进制> \
  go test -race ./internal/processagent -run '^TestMaintenanceRealRustInterop$' -count=1 -v
```

## 结果

- Python 发布工具 **32 项通过**。涵盖正常阶段顺序、用户配置保留、enter/stop/start/complete 回复丢失、无重复派发/降级、备份失败、校验后产物变化、错误身份/旧版不支持、验收失败、写 intent 失败、并发锁、重复 ID、profile 漂移、SQLite 回滚和维护状态保护。
- Go processagent 包 race **44 个顶层测试通过**，包括原进程生命周期、持久化、未知结果、维护互斥和真实 Rust 联调。没有 race 报警；本批未重跑所有管理端业务包。
- 真实链路为：Python 发布器 → HTTP agent → OPC Rust 下级，测试目录/PKI/状态/端口均独立。先验证下级和 agent 重启保留栅栏，再执行一次发布（使用同一已知测试二进制作原子替换），验证 token 从 1 增到 2、配置字节不变、维护完成和下级恢复运行。
- 最终定向重跑真实联调通过；随后执行只读 inspect，确认产物未变、实际运行映像摘要相同、agent revision 未变化。不是仅调用 TCP 探活。
- 所有测试下级无 CSV 端口配置，不打开真实串口。不把故障桩当真实设备；软件主流程联调也不代表跨版本数据库迁移已验收。
- 本机 `git diff --check` 通过。尝试远端全项目布局检查时，隔离源码副本没有包含 `tools/maintenance/check_layout.py`，该检查**未执行/未计为通过**；错误日志保留，本批不据此宣称全仓文档链接已验收。

证据保存在仓库本地 `artifacts/runs/2026-09-28-release-maintenance/`（生成物，不新增到 Git），以及上述远端隔离目录。包含对应源码、Python 结果、完整 agent race 日志、最终只读核对联调日志。

## 未完成和恢复边界

- `inspect-deployment` 是核对工具，不是恢复授权。未完成作业不自动续跑，也未提供协调回滚；回复丢失后必须按记录制定明确恢复步骤。不能删除 marker、恢复旧 token 或换 ID 再执行。
- agent 自身升级/状态 v2 兼容回退、manager 带维护编排、Web 维护显示、三语言完整契约样例仍待下一批。
- 本批软件验收仅包含实际映像、进程/端口和 OPC 身份/栅栏，不包括业务 Browse/Read/Subscribe 黄金点表、客户会话、现场双管理端竞争及控制报文。
- 未完成真实断电、磁盘满/慢盘压力、跨版本数据库迁移、性能 P95、一小时和过夜稳定性测试；不声称已验收。
- 现场当前版本/路径没有改变。新功能未启用，不影响现场 Web 现有启停流程；启用维护参数后的 Web 编排仍需先完成。
