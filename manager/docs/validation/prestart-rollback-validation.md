# 新版启动前的只回退程序验证（2026-09-28）

P2 第六小批。仅增加“下级已停止、备份完整、新版从未尝试启动”的带栅栏回滚及收尾。**未现场部署、未重启现场服务、未发送设备命令、未提交或推送。**

## 实现

- 新增 `rollback-deployment` 和 `finish-rollback`，显式目标/当次暂停确认/操作者均必需。
- 固定原 profile、作业、stop 请求、token 与旧程序摘要；任何新版 start 意图出现后拒绝此回滚，不根据缺失响应推断新版没执行。
- 先检查当前 SQLite 完整性和 schema，再保护性备份当前程序/配置/数据，然后只还原旧二进制。schema 不同不回退，绝不恢复维护代次或业务数据。
- 旧程序必须与发布前摘要相同；备份即使自带自洽摘要但换了另一份程序，也拒绝。
- 回滚意图保存后，原升级 resume/finish 被阻止，避免两个方向交替覆盖。首次启动使用独立 `.rollback-start`，丢失回复只读补查。
- finish-rollback 复用验收和维护解除边界，以旧程序摘要检查实际运行映像。结果明确 `outcome=rolled_back`、`data_restored=false`，原发布记录和失败阶段保留，不宣称新版本部署成功。

## 验证

测试机：`192.168.10.46`。隔离目录：

`/home/lenovo/code/rcd_prestart_rollback_20260928_WwcgsU`

```sh
python3 -m unittest discover -s tools/release -p 'test_*.py' -v
cd manager
RCD_MAINTENANCE_INTEROP_BINARY=<隔离Rust二进制> \
  go test -race ./internal/processagent -run '^TestMaintenanceRealRustInterop$' -count=1 -v
```

- Python **64 项通过**。新增 10 项覆盖二进制回退而配置保留、新版 start 意图拒绝、stop 未知、schema 漂移、回滚前保护性备份失败、回滚 start/complete 回复丢失、备份损坏、旧程序摘要身份不符、CLI 当次授权。
- 真实 Python → HTTP agent → OPC Rust 定向联调/race 通过。在临时发布器里注入“安装后、首次 start 意图前退出”，随后回滚；测试 HTTP 层再丢弃 agent 已接受的 rollback-start 正常回复，显式 finish-rollback 成功。
- 真实链路计数确认只有一次 stop、一次回滚 start、一次 complete，未发新版 start。停止后修改的合法配置注释在回滚和启动后原样保留。
- 同时回归原 stop/start/complete 回复丢失的恢复场景。仅扩展 Go 联调测试，未改本批 Go/Rust 产品逻辑，未重复全包 race。
- 所有下级使用临时目录、独立 PKI/端口，无 CSV 串口配置。联调用同一开发二进制，不冒充跨版本降级；不同旧/新文件的替换逻辑另由 Python 单测验证。

证据：远端 `rollback-tests-final.log`、`rollback-interop.log`；本机生成物 `artifacts/runs/2026-09-28-prestart-rollback/` 的源码/日志归档。本机只编辑、检查差异和传输，无本机可执行测试。

## 未完成

新版已经运行后的数据兼容性回退、rollback 意图落盘后但首次启动前再次崩溃的自动续跑、长期未知请求的人工协调工具、agent 自身升级、Web 维护入口，及真实断电/磁盘满、现场设备和性能长测。仅 schema 一致不足以授权任意旧软件读取新数据，本入口靠“从未尝试新版启动”进一步收窄。

未修改现场版本、服务参数或业务配置，未执行全仓文档布局/前端测试。
