# 机组串口同步：2026-10-19 隔离验证

## 范围与环境

依据用户提供的两份报文分析工作簿及 FromUnit 文本，新增 1→9、9→1 解析和双向周期同步。未修改原文档，未部署到现场，未访问真实串口、未发送硬件命令。全部编译、格式化、可执行测试在 192.168.10.46 隔离目录执行；本机只编辑、阅读及传输源码。

证据根目录：`/home/lenovo/code/rcd_unit_sync_20261019_yKKo8b`。

使用 Rust stable 1.96 和现有远端 Go 工具链。源码为正确工作树 `codex/opc-device-groups` 的未提交快照，不声称是已提交发布版。隔离 OPC 使用随机高位回环端口、独立 PKI/状态目录，不影响 4840/4841/8087 或现有服务。

## 结果

| 检查 | 实际结果 | 证据 |
|---|---|---|
| `cargo test --locked --all-targets` | 39 项通过，0 失败 | `test-final.log` |
| `cargo clippy --locked --all-targets -- -D warnings` | 通过 | `clippy-final.log` |
| `cargo test --manifest-path ../crates/rcd-config-form/Cargo.toml` | 4 项通过 | `config-form-tests.log` |
| 两种角色的 `check-config` | 81 / 250 点配置均通过，不访问串口 | `config-unit1-check.log`、`config-unit9-check.log` |
| None/Anonymous OPC 互操作 | 81 点浏览；读取、只读保护、订阅质量变化通过 | `opc-checks-final/none/` |
| Basic256Sha256/SignAndEncrypt/Anonymous 互操作 | 同上通过 | `opc-checks-final/secure/` |
| 同步快照 Method | 完整载荷接受，重复 revision 拒绝；只读状态可查 | 两种端点的 probe 日志 |

OPC 汇总：`opc-checks-final.log`。两种端点各收到 Good → BadNoCommunication → Good 三次通知，中断值为零并保留最后有效 SourceTimestamp。这里使用软件内存样本验证 OPC 行为，不代表真实串口验收。

新增 9 个协议/调度测试覆盖：文档黄金帧及 CRC、未压缩短帧、分片与连续帧、完整布局及身份校验、未定义报警原始值、点表类型和质量、错峰等待/半帧超时/无积压、快照过期和 CAS、连续接收、部分写失败不从头重发。

首次 OPC 检查在订阅通知阶段超时，保留 `opc-checks/` 与日志。测试夹具原先在服务就绪前启动短周期更新，已改为等待就绪并延长观察时间；最终两种端点重新执行均通过。没有修改业务采样行为来迁就夹具。

## 未验收与边界

- 未进行真实线路冲突、双机收发、线速或过夜验证。
- 1→9 的 250 点由协议黄金测试验证；本轮在线 OPC 客户端检查的是 9→1 的 81 点，不将其写成两种完整点表都已现场验收。
- 文档给出了双方字段，并未定义当前运行系统到这些字段的取数绑定；现支持显式提交本方完整快照，尚无自动 OPC 取数组包。
- HW/GMM/PumpStationCtrl 未定义的内部语义保留 RawHex；9→2 缺少完整布局和样例，未猜测实现。
- 新示例端口、任务、发送均默认停用。没有有效且未过期的本方快照不会发送。
- 尚未部署或接入在线管理端。本方角色、真实串口参数、数据绑定及当次现场授权确认后才能完成现场接入。
