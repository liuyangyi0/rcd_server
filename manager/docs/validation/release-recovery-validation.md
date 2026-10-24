# 已启动发布作业的收尾恢复验证（2026-09-28）

P2 第四小批，接续发布主流程。**未部署、未重启现场服务、未下发真实设备命令，未提交或推送。** 本批不是完整断点续跑、协调回滚或 Web 维护功能。

## 已完成

- 增加显式 `finish-deployment`，必须使用原 profile/作业 ID，并重新提供本次目标、控制暂停窗口和操作者确认。
- 只处理安装已确认、原启动请求存在且由 agent 确认 succeeded、当前程序仍正常运行的作业。不发送 enter/start/stop/restart、不替换文件、不恢复数据库或配置。
- 重新校验活动 marker、profile、原请求编号、gateway/token、产物和 manifest、入口及实际运行映像、备份完整清单与摘要；当前用户配置独立保留，不拿旧备份覆盖。
- 恢复尝试追加保存，原失败/unknown 阶段保持原样。曾请求 complete 但结果仍不明时拒绝重发；下级确已退出维护时只补齐本地完成记录。
- 修正 agent completed 状态的 reconcile：显式补查也必须重新读取 OPC，断开时不能用缓存成功替代当前确认。GET 查询仍无副作用。
- 早期中断、原请求 missing/unknown、当前下级停止、身份变化、备份损坏均拒绝收尾；保留现场供下一步明确恢复。

## 远端验证

全部运行于 `192.168.10.46` 隔离目录：

`/home/lenovo/code/rcd_release_recovery_20260928_Ea6i8m`

```sh
python3 -m unittest discover -s tools/release -p 'test_*.py' -v
cd manager
RCD_MAINTENANCE_INTEROP_BINARY=<隔离Rust二进制> \
  go test -race ./internal/processagent -count=1 -v
```

结果：

- Python **44 项通过**，包括新增 12 项恢复/授权回归：丢失启动回复、丢失 complete 回复、最终落盘失败、早期拒绝、原启动未知、旧 complete 意图不得重放、释放未确认、错误代次/marker、备份损坏/入口被替换、恢复再丢回复、写入意图失败、缺少当前授权参数。
- Go processagent **45 个顶层测试通过，race 无报警**。新测试确认 completed 下级读取失败会变 unknown、恢复后仅补查不重复写入。
- 真实 Python → HTTP agent → OPC Rust 隔离联调通过。在真实 agent 接受 start / 执行 complete 后，由测试 HTTP 层故意丢掉对应正常回复并返回 503；发布器留下 unknown，然后显式 finish 成功。
- 两种回复丢失场景均计数确认：每个作业只有一次 start、一次 stop、一次 complete，没有恢复重投。该注入针对软件回复，不是模拟现场串口成功。
- 下级使用 TempDir、独立端口/PKI/状态、无串口 CSV；未触碰现场设备。仍使用第三批的同一开发版 Rust 二进制，SHA-256 为 `b82c63470e7c652450b155f6138a6fa20e0835955645c695b204ae6794b71f8c`，本批没有修改 Rust。

证据：远端目录中的 `recovery-tests-final.log`、`recovery-agent-race.log`；本机生成物目录 `artifacts/runs/2026-09-28-release-recovery/` 保存对应源码及日志归档。本机只编辑、检查差异及传输，没有运行构建/测试。

## 明确未覆盖

- 停止、备份、安装等早期中断的继续执行，以及协调二进制回滚/数据库兼容恢复。
- agent 自身升级，管理端/Web 维护入口与权限编排。
- 真实断电、磁盘满/慢盘、跨版本数据库迁移、现场双管理端竞争、设备控制与性能/长稳验收。
- 未执行全仓文档布局检查，未重跑无关管理端业务包和前端测试；不把本批范围内通过算作全项目验收。

现场版本和服务参数没有改变。恢复命令不是长期授权，也不能用过去暂停窗口直接在现场执行。
