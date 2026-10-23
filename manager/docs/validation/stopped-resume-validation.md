# 已停止发布作业的显式继续验证（2026-09-28）

P2 第五小批，接续第四批的发布收尾恢复。**本批未现场部署、未重启现场服务、未发设备命令、未提交或推送。**

## 改动与边界

- 新增 `resume-deployment`：用原作业、profile、请求编号和维护 token 核对已停止的下级；需要当次目标/控制暂停授权。
- 支持 stop 回复丢失后补查成功、备份失败、安装完成而本地确认丢失。可新建保护性备份、核对/安装目标二进制、首次 start，返回 `awaiting_finish`，不自动解除维护。
- stop/首次 start 意图已记录但执行结果未知时不重新派发；resume 之后 start 回复再丢失，只允许按原请求补查/finish。意图已落盘但尚未真正 POST 就崩溃，也保守拒绝重发。
- 新日志保存安装前二进制摘要；旧日志缺少该字段时拒绝早期恢复。备份、包或入口被改动/损坏时阻止后续动作。
- 新的 `resume_checkpoints` 仅追加确认，不重写原失败/unknown 阶段。finish 复用这些确认，但依旧核对实际运行映像和新鲜 OPC 证据。
- 停止期间只能读取 agent 已持久确认的缓存栅栏，结果明确标注，不冒充实时 OPC 检查。启动后由独立 finish 验收。
- 不恢复旧配置或数据库、不回退维护代次。新备份目录不覆盖旧的不完整备份。

## 测试

全部可执行测试在 `192.168.10.46` 隔离目录：

`/home/lenovo/code/rcd_stopped_resume_20260928_8LQc64`

```sh
python3 -m unittest discover -s tools/release -p 'test_*.py' -v
cd manager
RCD_MAINTENANCE_INTEROP_BINARY=<隔离Rust二进制> \
  go test -race ./internal/processagent -run '^TestMaintenanceRealRustInterop$' -count=1 -v
```

- Python **54 项通过**。新增 9 项停止恢复测试和 1 项 CLI 授权测试，并回归原发布/finish：stop 回复丢失、备份失败、安装确认丢失、不重复复制目标、原 stop unknown、当前仍运行/栅栏未确认、入口被改、resume 中 start 回复丢失、POST 前崩溃、旧日志缺少摘要。
- 真实 Python → HTTP agent → OPC Rust 定向联调及 race 通过。测试 HTTP 层在实际 agent 接受 stop 后故意丢掉正常回复并返回 503；补查 stop succeeded 后 resume，再 finish。start/complete 回复丢失旧场景同时回归通过。
- 每个注入作业均确认只出现一次 stop、一次 start、一次 complete。resume 后维护仍保留，finish 成功才结束。未改 Go/Rust 产品逻辑，本批只扩展发布器和联调测试；未重复全包 race。
- 所有子进程、端口、PKI 和状态在临时目录，无串口 CSV，不打开现场设备。联调使用同一测试二进制原子发布，不能当跨版本数据迁移或硬件验收。
- 本机只读写源码、检查差异及传输；`git diff --check` 通过。未执行全仓文档布局或前端测试。

证据保存在上述远端的 `resume-tests.log`、`resume-interop.log`，以及本机 `artifacts/runs/2026-09-28-stopped-resume/` 生成物目录内对应源码/日志归档。

## 尚未完成

维护进入尚未确认、原进程请求长期 unknown/缺失的自动化处置；协调回滚、agent 自身升级、Web 维护入口；真实断电/磁盘满、跨版本数据库迁移、现场业务及性能长测。不得删除 marker、改旧日志、回退 token 或换 ID 绕过阻止。
