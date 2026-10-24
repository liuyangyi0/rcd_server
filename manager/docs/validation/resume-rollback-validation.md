# 已登记回滚显式继续验证（2026-09-28）

P2 第八小批。增加 `resume-rollback`，只处理已登记回滚但从未记录回滚启动意图的作业。**未现场部署、未重启现场服务、未发送设备命令、未提交或推送。**

## 改动与边界

- 原主机/profile/实例/作业/程序摘要/维护代次必须一致；原 stop 明确成功且当前已停止，无 pending。保留原停止证据为 agent 已确认缓存，不冒充停止期间的 OPC 在线证明。
- 新版或回滚旧版一旦存在启动意图，拒绝继续回滚；即使没有收到回复或 POST 尚未发出，也不重投。继续使用原 `.rollback-start` 并独立 `finish-rollback`。
- 核对原备份、首次回滚保护副本、当前数据库完整性/schema 与入口旧/新摘要。每次继续新建当前数据备份，用户在中断后修改的配置保留，不恢复旧配置、数据库或维护代次。
- 不改写旧阶段、旧尝试和首次保护副本路径；新增恢复尝试。已经恢复旧二进制时只校验，不重复复制。所有已确认的继续回滚保护副本在启动前及收尾前复核。
- 本批提取原回滚的还原/首次启动步骤复用，未修改 Rust 产品逻辑或 Go agent 产品逻辑。工具说明见[继续回滚入口](../../../tools/release/README.md)。

## 测试环境与命令

全部在 `192.168.10.46` 隔离目录 `/home/lenovo/code/rcd_resume_rollback_20260928_aqUZf3` 执行，本机仅编辑、检查和传输。

```sh
python3 -m unittest discover -s tools/release -p 'test_*.py' -v
cd manager
RCD_MAINTENANCE_INTEROP_BINARY=<隔离Rust二进制> \
  go test -race ./internal/processagent -run '^TestMaintenanceRealRustInterop$' -count=1 -v
# 最后增加备份复核后，定向重新执行相关真实链路
RCD_MAINTENANCE_INTEROP_BINARY=<隔离Rust二进制> \
  go test -race ./internal/processagent \
  -run '^TestMaintenanceRealRustInterop$/^DurableReleaseRunner$/^ResumeInterruptedRollback$' -count=1 -v
```

- 最终 Python **78 项通过**，其中新增 14 项测试，含还原前后中断、最新配置保留、原记录不改写、启动意图持久化失败、启动意图落盘后尚未 POST、启动回复丢失、两类备份损坏、schema 漂移、目标/代次/停止状态变化、尝试上限、只读 inspect 和 CLI 当次确认要求。
- 首轮真实 Python → HTTP agent → OPC Rust 的 race 联调通过，同时回归原 stop/start/complete 回复丢失、启动前回滚和新增回滚中断恢复。
- 最终备份复核改动后，新增恢复场景的定向真实链路/race 再次通过（27.208 秒）；不是使用首轮日志代替最终版本结果。
- 新场景在临时 Python 发布器中注入“旧程序已恢复、首次启动意图前退出”，再修改合法配置注释，显式 resume-rollback；HTTP 层丢弃 agent 已受理的启动回复，确认第二次 resume 被拒绝，然后 finish-rollback 成功。断言每次只有一次 stop、一次 start、一次 complete，最终 outcome 为 rolled_back、data_restored 为 false，配置注释保留。
- 所有程序/PKI/日志/状态位于临时目录，使用独立 OPC 端口、无 CSV 串口配置。联调使用同一开发二进制，不冒充不同软件版本的降级；旧/新不同内容的替换路径由 Python 测试覆盖。

远端证据 `resume-rollback-evidence/`；本机归档 `artifacts/runs/2026-09-28-resume-rollback/`，含测试日志及本批源码、Rust 测试二进制 SHA-256 清单。

最终证据包 `resume-rollback-evidence.tar.gz` SHA-256：`d11aed8cf3dc1bb329b355ee9993ca7e6ba75218fba5044d75d095d3c9c498cd`。

## 未验收

不支持新版已运行后的任意降级、启动意图未知的自动重投、agent 自身升级、业务数据还原或自动解除维护。未验证真实断电/磁盘满、现场设备控制、满负载延迟或过夜运行。本批未修改 Web，未重复前端构建/浏览器验收；未改变 Rust 代码，不重复全 Rust 单测。整个 P2 尚未完成。
