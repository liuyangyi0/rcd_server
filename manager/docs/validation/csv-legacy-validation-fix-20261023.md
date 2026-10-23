# 旧 CSV 格式兼容与严格保存校验（2026-10-23）

后续经当次授权，已通过运维平台升级 30、58、46 的采集程序，见[发布记录](csv-legacy-deployment-20261023.md)。下文“未部署”描述的是先前隔离验证阶段，不代表最新发布状态。

## 范围和结论

修复网页修改“发送完成保护”时，未改动的合法旧点表被误报“第 1 行应为配置字段 com”等问题。保留完整配置集的严格校验，不将真实越界降为警告，不因修改时序就跳过点表。

**本轮未部署、未重启。** 30、58、46 的现场程序和配置未修改；没有自动改 `DataLen`、点地址或删除变量，没有发送现场串口命令。依据用户最终澄清，配置问题只提供规则和错误，由配置编写人员处理，不继续代改点表或推定正确长度。保留本工作树其它未提交修改，未提交或推送。

## 实现

- `src/csv_parser.rs`：保存前校验兼容 `COM/BaudRate/IsSpecial/ComIndex/DeviceID/DataLen/Pre`、旧 `KKS/Type/F_Type/.../LH` 列头、标签两侧空白、UTF-8 BOM 和 CRLF；兼容现场已有的 `FALSE`，其含义仍为 false。
- 仍严格检查配置行和列顺序、类型、位段、字节越界、重复点名及整套配置冲突。字段兼容不改写文件、不规范化业务点名，也不改变启动解析和串口时序。大写 `TRUE` 不被新解释为 true，避免静默改变原启动语义；规范小写 `true` 不变。
- `src/operations/files.rs`：新增合法旧 CSV 下修改发送保护的草稿、校验、CAS 保存及重启标记测试；另固定“只改时序但未改点表越界仍拒绝保存”，核对错误定位、原文/版本不变且不创建备份。
- `manager/internal/opc/command_tracking_interop_test.go`：真实 Rust 下级夹具使用旧标签和 `FALSE`，通过 Go OPC 连接生成保护时间与命令间隔草稿，校验、保存、冲突拒绝及待重启结果。不改 Go 生产逻辑或 Web 资源。
- 填写规则集中到[采集 CSV 规则](../guides/acquisition-csv-rules.md)，同步纠正文档中“第 8 行为任意分隔行”的旧说明：实际第 8 行是点表列头。

## 46 隔离验证

目录：`/home/lenovo/code/rcd_csv_compat_20261023_k6EskD/`。源码从上一轮已验证快照复制，更新本轮文件。所有构建、格式化和执行测试在 46，本机只编辑、对比和传输。

| 检查 | 结果及证据 |
| --- | --- |
| CSV 定向单元 | `cargo test --offline --lib strict_csv`：9 通过，`csv-unit.log`、`.exit=0` |
| 合法旧表头 + 保护时间保存 | `cargo test --offline --lib legacy_csv_does_not_block_transmit_guard`：1 通过，`guard-save.log`、`.exit=0` |
| 严格时序保存回归 | `cargo test --offline --lib timing_edit_still_rejects_unchanged_out_of_bounds_csv`：1 通过，`strict-timing.log`、`.exit=0` |
| Rust 全量 | 最终 `cargo test --offline`：**440 通过、4 既有忽略**，`rust-final.log`、`.exit=0`；其中 lib 414 通过、1 忽略。初轮 439 通过的 `rust-all.log` 保留，不冒充最终包含新增回归 |
| 真实 Rust/Go OPC | 显式设置本轮 `RCD_TRACKING_INTEROP_BINARY`，`go test -race ./internal/opc -run '^TestCommandTrackingRealRustInterop$' -count=1 -v`：通过，16.841 秒，`rust-go-legacy-guard.log`、`.exit=0` |

互操作使用临时目录、随机回环端口、停用且不存在的串口，覆盖 None 及 Basic256Sha256/SignAndEncrypt 匿名/用户名连接、旧 CSV 下的草稿/校验/CAS 保存和重启恢复语义；没有把 Skip 当成通过，也不代表真实硬件读写验收。

3 份涉及源码的最终 SHA-256 保存在 `tested-sources.sha256`；本地保留相同字节的测试版本，未用远端测试文件覆盖其它工作树修改。新增严格回归仅修改测试代码，真实互操作已覆盖的生产实现未改变。

本轮 5 份修改文档的 7 个新增本地引用在 46 定向检查通过（`doc-links.log`、`.exit=0`），相关 `git diff --check` 通过。隔离源码快照未包含 `tools/maintenance/check_layout.py`，因此没有把完整仓库目录检查列为通过项；首轮检查环境缺项的 `layout.log` 保留。

## 现场只读检查

只读使用实际 Rust 校验器检查 46 的 `/var/lib/rcd/lower/real-lower/config`，并比较读取前后字节未变化：45 份 CSV，38 份通过，旧表头误报 0，7 份仍有真实地址/长度矛盾。没有检查或修改 30、58 点表，不能外推三台的配置结论。

| 现场文件 | 配置串口 / 站点 | 已配置 DataLen | 超出兼容上界的点 |
| --- | --- | --- | --- |
| `rcd_05_00_9CYE92GH201.csv`–`206.csv`（6 份） | `/dev/ttyWCH1` / 11–16 | 18 | 每份第 83–87 行，AI4–AI8，共 30 点 |
| `rcd_15_02_9CYE93GH251.csv` | `/dev/ttyWCH2` / 61 | 29 | 第 131–132 行，AI7–AI8，共 2 点 |

`actual-csv-validation.json` 为各文件首个校验错误；`actual-point-bounds.json` 为只读提取的 32 个确定越界点的行号、地址、配置摘要。它们证明配置矛盾，**不是推荐替代长度，更不是实际硬件响应长度的证明**。现场 CSV 正文未加入仓库。

合法旧表头误报已修，并不表示整套现场配置已经可保存；上述真实错误仍应显示并阻止保存。配置编写人员修改后重新校验即可，软件不代替使用人员决定设备配置。
