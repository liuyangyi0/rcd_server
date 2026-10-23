# 2026-10-15 基础模块隔离验证

## 范围与结论

- 开发工作树：`rcd_server_device_groups`，分支 `codex/opc-device-groups`。
- 新模块：`communicate/`，Rust 单程序；旧 `rcd_server_communicate` 原文件保留。
- 所有编译、执行测试均在 `192.168.10.46`，隔离目录 `/home/lenovo/code/rcd_communicate_20261015_H8C9J3`。本机仅源码编辑、差异检查、归档和传输。
- 测试工具链：`rustc 1.96.0`、`cargo 1.96.0`；本轮未单独验证声明的最低 Rust 1.87 工具链。
- 基础软件验证通过；实际查询内容和字段映射待用户提供，真实串口、业务点表、线上管理端接入及 Agent/发布平台集成未验收。
- 没有修改现场配置、重启现场服务、占用真实串口或发送硬件命令；测试子进程已退出。没有提交或推送。

## 最终执行结果

以远端 `validation-final.exit = 0`、`fork-regression.exit = 0` 为最终结果；早期失败日志保留，不作为通过证据。

| 检查 | 结果 | 远端证据（相对隔离目录） |
| --- | --- | --- |
| `cargo fmt --all --check` | 通过 | 最终验证链退出码 0 |
| `cargo test --locked --offline --all-targets` | 25 通过，0 失败、0 忽略 | `test-final.log` |
| `cargo clippy --locked --offline --all-targets -- -D warnings` | 通过 | `clippy-final.log` |
| `cargo build --locked --offline --bins --examples` | 通过 | `build-final.log` |
| OPC fork，开启时间戳保留特性 | 31 通过，1 项明确过滤 | `fork-enabled-v4.log` |
| OPC fork，未开启该特性 | 29 通过，1 项明确过滤 | `fork-default-v2.log` |
| 新二进制 + Go OPC 客户端，None/Anonymous | 通过 | `opc-checks-final/pending/probe-none.log` |
| 新二进制 + Go OPC 客户端，Basic256Sha256/SignAndEncrypt/Anonymous | 通过 | `opc-checks-final/pending/probe-secure.log` |
| 软件点位夹具，两种安全模式下的质量切换订阅 | 均通过 | `opc-checks-final/sequence/probe-*.log` |
| 同一状态目录重复启动、SIGTERM 正常退出 | 均通过 | `opc-checks-final/report.json` |

共享 fork 完整测试最初暴露缺失资源：`config::limits::tests::server_conf_limits_match_struct_field_names` 依赖 `vendor/samples/server.conf`，当前仓库不含该文件。两条最终 fork 测试命令均显式使用 `--skip` 排除此项，**不计入已通过**。没有伪造该文件或删除测试。

测试命令（fork 使用隔离目录自身依赖锁，未取回为仓库文件）：

```sh
CARGO_TARGET_DIR=<隔离目录>/communicate/target \
CARGO_PROFILE_TEST_DEBUG=0 CARGO_PROFILE_DEV_DEBUG=0 \
cargo test --locked --offline \
  --manifest-path ../vendor/async-opcua-server/Cargo.toml \
  --lib --features preserve-source-timestamps -- \
  --skip config::limits::tests::server_conf_limits_match_struct_field_names
# 不传 --features 再运行一遍，检查原默认路径。
```

Go 探针在 46 现有管理端源码副本的 Go module 下编译，使用其固定客户端 fork；仅允许 `127.0.0.1` 的高位端口和明确的示例实例身份。`tests/run_opc_checks.py` 使用随机空闲回环端口，创建的配置全部停用串口。真实 OPC 服务/客户端库参与测试，但数据由内存夹具提供，**不是硬件验收**。

## 覆盖内容

- 配置未知字段、重复 KKS、重复串口、点表引用、错误类型及未完成协议启用拒绝。
- 旧包络 CRC32 黄金向量、长度校验、每个分片位置、连续帧、噪声、截断 zlib、额外压缩字节、解压上限及确定性随机坏输入。
- 部分写入不从头重发、未匹配响应超时、取消、迟到帧隔离、停用不打开串口、打开失败和无积压公平调度。
- 整批发布、类型不符不部分更新、坏质量清零、其他任务不受影响、失败不伪造源时间。
- 协议注册唯一性、未启用不构造适配器、适配器初始化失败不启动。
- OPC 命名空间、HasComponent 浏览、标量 Double 类型、只读 AccessLevel、写入拒绝、初始坏质量及空源时间。
- 同为零值的 Good → BadNoCommunication → Good 通知、源时间保持、ServerTimestamp、请求时间戳选项。
- 订阅容量覆盖配置点表、内存采样器真实 100 ms 下限；**未执行 25,000 点负载测试**。

## 本轮发现并修复

1. 自定义节点管理器与 OPC 库内置 `diagnostics` 重名：改为独立管理器名。
2. 延后通知会改写 SourceTimestamp：共享 fork 新增非默认 `preserve-source-timestamps` 特性，仅新模块启用；按独立通知时间调度、合并最新样本，不修改设备源时间。默认路径单独回归。
3. 默认单订阅 1000 项与点表上限不一致：新模块按点表数量设置订阅容量，单次请求仍分批。

## 产物与复现

调试产物（不是正式发布包）：

- `communicate/target/debug/rcd_communicate`
- SHA-256：`f0f5c5aaf721f8ceaf9ecffef0dc76b085380fa59e849ee49076cf27d46c18cd`
- 独立 `communicate/Cargo.lock` SHA-256：`6a517cf1064a54352cbc1a066f500225f5857b1bf822bdbfba5dd6079bd805b6`
- 取回的格式化源码及锁文件归档 `formatted-source.tar.gz` SHA-256：`48471bb63ab05867dbdf0e2e9430b06b53658e8a9488409c7e1145f3317dea00`。

复现 OPC 检查先编译 `tests/opc_probe.go` 和 `examples/opc_fixture.rs`，再提供一个源码目录之外的空白隔离目录：

```sh
python3 tests/run_opc_checks.py --source <隔离源码>/communicate \
  --probe <Go探针路径> --scratch <空白隔离目录>
```

这次没有部署。下一步需先补真实协议与 KKS 映射，再另行确认实际串口、服务身份和现场接入窗口。
