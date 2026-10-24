# 站点状态计算输入：实施与验收记录

日期：2026-09-18。工作分支：`codex/opc-device-groups`。

发布版本：`0.7.3-state-inputs.1`，构建标记 `6f0dd6a+state-inputs-20260918`。

## 本次实现

- 计算 TOML 增加 `state_inputs`，支持固定来源和跟随组默认来源的站点映射。
- 状态是 UInt32 数值 0–5；离线 5 保留为 Good 状态输入，能用于报警公式。配置丢失返回 BadConfigurationError。
- 编译、运行、草稿校验、激活、版本差异和回滚均处理状态映射；别名与原始点、计算输出重名被拒绝。
- Web 增加状态输入页、站点选择、同名站点辅助匹配、分页筛选、复制别名、插入编辑器及计算结果依赖详情。
- 状态变化只标脏相关依赖；观测时间心跳不触发计算重算或 SSE；过期检查每秒执行一次。
- 未改下级串口、轮询、安全策略或端口，也未向正式计算表加入测试别名/规则。

## 远端验证

所有构建和可执行测试均在 `192.168.10.46` 执行，本机仅编辑、同步及检查差异。

| 项目 | 结果 |
| --- | --- |
| Go `go test ./... -count=1` | 通过 |
| Go race：calculation、app、api | 通过 |
| 新增状态心跳、路由、过期测试的 race 复测 | 通过 |
| 前端 TypeScript/Vite 构建 | 通过 |
| 前端 Vitest | 25 个测试文件、171 个测试通过 |
| 真实三下级：站点树 Browse/Read/Subscribe | 135 站点，3 个来源订阅通过 |
| 隔离管理器：固定来源与默认来源切换 | 通过，固定来源不变，默认来源输入随组默认源切换 |
| 隔离管理器：断开下级连接、恢复 | 通过，计算节点订阅观察到状态 2 → 5 → 2，质量 Good；报警计算保持有效 |
| Session 独立选源不影响中央计算 | 单元测试通过 |
| 分页、依赖过滤、草稿旧 revision | 通过；冲突返回 409 |
| 管理器重启恢复映射及版本回滚 | 通过；回滚生成新版本 |
| 正式管理器部署后 OPC Read/Subscribe | 通过 |
| 正式配置保护 | 活动 revision=6、1997 条规则及草稿内容未改变 |

真实验收使用 `.46`、`.30`、`.58` 现有下级，并非模拟器。测试规则只加入隔离数据库，未写入正式数据库。隔离管理器测试结束已停止。

验收时 135 站点中 29 个通信正常、106 个通信异常，这是下级当前上报的设备状态，不代表管理器或功能测试失败。三下级 OPC 连接及订阅均已恢复 Active。

限制：浏览器工具无法访问测试地址（内置浏览器 ERR_BLOCKED_BY_CLIENT，Chrome 连接工具失败），未完成浏览器人工视觉验收。前端交互自动测试已通过，但不能替代真实浏览器视觉确认。原编辑器测试仍有非失败的 React act 警告。

## 部署、日志与备份

- 正式 Web：`http://192.168.10.46:8087/calculations?tab=state-inputs`。
- 正式 OPC：`opc.tcp://192.168.10.46:4840`。
- 二进制：`/home/lenovo/code/rcd_device_groups_20260828/bin/RcdManager-0.7.3-state-inputs.1-linux-amd64`。
- SHA-256：`0842f2986d5727114e98b98d6db7172ba930686bafe7f8a599cd03a35fb8ce79`。
- 稳定启动入口：`/home/lenovo/.local/bin/RcdManager`；原 `rcd-manager.service` 自启配置保留。
- 备份：`/home/lenovo/code/rcd_state_inputs_20260918/backup/pre-deploy/`，包含 SQLite 一致性备份、旧二进制、活动/草稿快照和下级 PID。
- 验证日志：`/home/lenovo/code/rcd_state_inputs_20260918/logs/`。
- 关键日志：`go-all.log`、`go-race.log`、`web-tests-final.log`、`web-build-final.log`、`live-state-inputs.log`、`state-heartbeat-race.log`、`deployed-opc-read-subscribe.log`、`deploy.log`。

正式管理器在 16:57 重启。初始化期间有短暂的下级运维连接不可用记录；之后三来源连接/订阅恢复。检查时服务无自动重启（NRestarts=0），健康和就绪检查通过，下级采集 PID 未改变。

回滚使用备份的旧二进制或原 `RcdManager-0.7.2-station-health.1-linux-amd64`，重新指向稳定入口后只重启管理器。注意：旧程序不支持 `state_inputs`；用户开始使用新映射后，回滚前应先恢复一份不含状态输入的计算版本和草稿，不能直接用旧程序读取新格式。

使用步骤见 [站点状态计算输入说明](../guides/calculation-state-inputs.md)。
