# OPC选源CAS与成员语义修复（2026-10-21）

## 修改范围

- 保留`ns=3;s=Groups/<group>/MemberSourceIds`、`SelectedSourceId`、`SelectionRevision`的NodeId、数据类型、权限和原写入行为。
- MemberSourceIds仍列组内全部已配置成员，包含离线、停用和不可选来源；修正说明，并添加OPC Description指向Selectable、ConnectionState及CompleteCoverage。不能以“修复”为名过滤成员并破坏现有客户端。
- 新增组对象下`SelectSource(SourceId String, ExpectedRevision UInt64) -> (SelectedSourceId String, SelectionRevision UInt64)`，参数为标量，使用现有HasComponent/HasProperty结构公布签名。请求ObjectId必须是对应组。
- 原样把客户端期望revision交给现有SelectSource路由事务，不重新取最新revision覆盖它。Session、版本、组成员关系、目录可选性和pending保护共用原事务；成功输出取该事务返回值，失败无成功输出。
- 旧SelectedSourceId String写入继续兼容，不冒充端到端CAS。两种接口都保留原手动固定语义、版本推进、审计与Session订阅刷新，不修改默认来源或其他Session。
- 新Method不撤销下级已接受的发送快照，不实现写入所有权/主备发送协调；SelectionRevision与TransmitStatus.revision不同，使用说明明确区分。
- 新增[全部软件关系与使用说明](../../../docs/整体软件使用说明.md)，同步更新[管理客户端说明](../guides/使用说明.md)、[分组接口](../guides/device-groups.md)及[通信客户端说明](../../../communicate/docs/opc-client-guide.md)。

产品文件：`internal/opc/upstream_selection.go`及`upstream_nodes.go`。未改Rust业务逻辑、Go路由锁算法、数据库schema、OPC端口、安全策略或现场配置。

## 46隔离验证

目录：`/home/lenovo/code/rcd_selection_cas_20261021_gZHhAG`。本机只编辑/检查/传输源码；所有构建和执行测试在46完成，不使用现场OPC端点或真实串口。

| 检查 | 结果 | 证据 |
| --- | --- | --- |
| `go test -race ./internal/opc -run 'TestSelectSourceMethod\|TestReconcileCatalogRemovesStaleManagementNodesOnly' -count=1 -v` | 通过 | cas-method-verified.log |
| `go test -race ./internal/opc ./internal/app ./internal/domain` | 三包通过 | manager-regression.log |
| `go test ./...` | 全模块通过 | manager-regression.log |
| `go build -o ../../RcdManager-cas-isolated ./cmd/rcd-manager` | 通过，仅隔离产物 | RcdManager-cas-isolated |
| `cargo test --locked --test unit_reports -j2` | 9项通过，含文档39/14项完整JSON编码 | client-document-test-final.log |

新增测试使用实际RouteTable和回环OPC服务：

- 参数不足、多余、类型错误、空来源、失效Session及失败不返回成功输出。
- Method对象关系、输入/输出参数名称、String/UInt64类型、标量秩和重复目录发布身份稳定。
- 两个匿名Session独立选源，不改变另一组；手动调用退出该Session的自动参与。
- Web等效更新先提交后，客户端旧revision被拒绝，原来源和revision保持。
- 相同Session的两个并发旧版本Call只提交一次，另一次BadInvalidState。
- 停用、冲突、首次发现中、错误组和不存在来源拒绝；离线但缓存有效来源仍允许选择，和既有规则一致。
- pending同时阻止本组两个Session切源；释放后恢复。
- 旧String Write仍可用；新增Method在None和Basic256Sha256/SignAndEncrypt端点调用通过。
- 成员列表仍含不可选成员，Selectable/ConnectionState独立；组删除后Method与参数节点清理，重新添加可恢复。

首轮专项测试的安全连接因隔离自签证书未信任而EOF；仅对回环测试夹具启用既有临时证书跳过选项后通过，未修改生产配置或任何安全策略。一次修正文件传输在认证前超时，未写入；以最终已上传、gofmt后的源码和通过日志为准。首次Rust文档测试因隔离目录缺少既有examples夹具有两项失败，补齐原夹具后9项全部通过，没有更改产品逻辑或放宽断言。

## 隔离验证阶段的发布边界

本轮未现场部署、未重启、未修改三机配置、未发硬件命令、未提交或推送。隔离构建不是已登记的正式发布包。线上旧服务在后续授权部署前没有SelectSource Method；本轮文档均标明该新增能力需部署后使用。前端运行代码未修改，未声称重新做完整浏览器发布验收或长测。

## 随后的正式发布

2026-10-21取得当次授权后，SelectSource随`0.6.12-state-names.20261021.1`通过原生Semaphore＋Ansible发布到30、58、46管理端，任务分别为55869、55882、55892。三台现场Browse/Read核对了Method及输入/输出签名，旧选源变量仍存在；未在现场执行切源Call或硬件写入，不将签名检查冒充现场CAS事务验收。完整构建、平台任务、运行映像、网页及日志证据见[数字开头命名修复的正式发布记录](state-input-digit-names-20261021.md#2026-10-21-三台管理端正式发布与验收)。隔离CAS调用和并发测试结果仍以上文为准。
