# 按改动选择验证

仅在需要选测试或交付时阅读。授权、测试主机及业务边界以根 [AGENTS.md](../../AGENTS.md) 为准；本文的命令示例不授予部署、停服务或发送设备命令的权限。

## 测试入口

以下命令在远端隔离源码副本运行。先确认工具版本、依赖和工作目录；从窄范围开始，不把所有命令当成每次必跑清单。

| 改动 | 工作目录与检查 | 应验证的行为 |
| --- | --- | --- |
| 说明、指令、目录链接 | 仓库根：`python3 tools/maintenance/check_layout.py`；人工核对授权及业务冲突，查看 `git diff --check` | 路径有效、无失效指令；不为纯文档重建所有程序 |
| Rust 解析、协议、采集 | 仓库根：`cargo test <相关测试过滤词>`；涉及跨模块再用 `cargo test` | 帧校验、超时、迟到帧隔离、正常采集不受干扰 |
| Rust 计算 | 仓库根：`cargo test calc_engine_comprehensive`，结合 `tests/calc_expected_oracle.py` 和 `test_data/` | 表达式语义、类型、依赖及质量；确认过滤词实际匹配测试 |
| Go 业务/API | `manager/`：`go test ./internal/<涉及包> -run <测试名>`，公共接口/跨包变化再用 `go test ./...` | revision 冲突、选源隔离、错误返回及恢复 |
| Go 并发/订阅/缓存 | `manager/`：对涉及包运行 `go test -race`，再选相关互操作测试 | 数据竞争、原 Session 恢复、epoch、同值切源通知、命令锁 |
| Web | `manager/web/`：依赖未就绪时 `npm ci`；`npm test -- <相关测试文件>`、`npm run build` | 受影响交互及类型检查；布局变化另用浏览器查看空态、错误态和分页 |

OPC 本地 fork 是独立模块；涉及其代码时还须在对应 `manager/third_party/*` 模块运行相关测试，管理端 `go test ./...` 不代表 fork 已覆盖。补丁背景见各模块 `FORK.md`。

需要编译 Rust 时用 `cargo build`；lint 按改动选择 `cargo clippy --all-targets --all-features`。带设备、写入、重启或负载行为的集成测试，先读脚本参数与副作用，再核对任务授权。纯逻辑测试可用确定性输入/桩，但不能据此声称真实串口验收通过。

## 部署与验收（仅当任务包含部署）

1. 只读确认目标主机、服务、当前二进制/版本、运行目录和配置；文档里的日期路径、旧端口及旧账号不能直接作为现状。
2. 备份涉及的二进制、配置及数据库；SQLite 使用一致性备份，不孤立复制正在写入的主数据库文件。记录可恢复路径，不覆盖唯一备份。
3. 使用本次已验证产物；Web 资源在 Go 发布构建前生成并嵌入。按实际平台参见 [Linux 自启动](../../manager/docs/deployment/linux-autostart.md) 或 [Windows 部署](../../manager/docs/deployment/windows-deployment.md)，不把 Linux 检查算作 Windows 验收。
4. 核对实际版本、服务存活、健康检查、来源连接和本次相关行为；真实设备测试不能另起实例占用现场串口。长测记录开始/结束时间、日志及是否真正完成。
5. 若失败，保存日志，按已授权回滚范围处理；涉及新增停机或数据恢复而授权不明时请求方向。

历史结果在 [验收文档索引](../../manager/docs/README.md)；`archive/` 与旧验收记录仅供追溯，不是自动执行清单。
