# 状态输入同名 OPC 输出：2026-10-23 验收

## 结果

已在46隔离测试，并经 Semaphore 原生任务队列、Ansible 和现有发布器按30 → 58 → 46升级管理端。无需修改已激活的 `state_inputs` 或增加转发公式。

| 主机 | 平台升级任务 | 版本 | 现场 OPC 验收 |
| --- | --- | --- | --- |
| 192.168.10.30 | 64543 | 0.6.18-state-output.20261023.1 | 通过 |
| 192.168.10.58 | 64547 | 同上 | 通过 |
| 192.168.10.46 | 64551 | 同上 | 通过 |

本次已有控制暂停及三台管理端重启授权；升级结束已通知恢复控制。未重启采集、Agent、通信程序或Semaphore，未发送硬件控制命令，未修改安全策略、端口、串口时序或现场计算配置。

## 行为

- 每个活动状态输入按 `name` 发布 `Objects/Devices/<name>`，NodeId 为 `ns=2;s=<name>`，同名BrowseName、标量UInt32、只读、HasComponent关系；支持Browse、Read和Subscribe。
- 原有公式仍可引用状态输入，不同名称的转发公式继续有效；与原始点、命令、其他状态输入或公式输出重名仍拒绝。
- 固定来源不随Session选源变化；跟随默认来源仅随全局默认来源变化。没有公式引用的状态输入也可独立发布和通知。
- 保持0～5诊断值及其质量规则；下级离线为5/Good，配置失效为0/BadConfigurationError，观测过期为0/Good。
- 删除时当前进程保留节点并输出0/BadOutOfService；同名UInt32状态输入可热恢复。类型变化沿用待重启规则。
- 公式数量及公式质量统计仍只统计 `[[rules]]`；状态输入查看现有“状态输入”页。状态输出包含在管理OPC的CalculationNodeIds中。
- 状态输入页面、内嵌帮助、详细指南和带注释示例已更新。

## 远端测试

执行主机均为192.168.10.46；本机仅编辑、格式化、检查差异和传输。

1. 针对性测试通过：

   `go test ./internal/calculation ./internal/app -run 'Test(StateInputsPublish|StateOutputs|CalculationState|StateInputHeartbeat)' -count=1 -v`

   覆盖无公式配置、同名目录/类型/只读属性、精确重名报错、固定/默认来源、离线与重连、过期、配置错误、删除、旧UInt32公式节点恢复、无依赖状态节点刷新及统计不膨胀。

2. 真实OPC协议栈的隔离测试通过：`TestStateOutputsOverOPCWithoutForwardingRules`。使用临时回环端口、两个匿名Session、确定性站点快照，验证Read/Subscribe、只读写拒绝、离线5/Good、热删除及热恢复。未连接现场串口，不能将此项称为真实硬件收发测试。

3. Race通过：

   `go test -race ./internal/calculation ./internal/app ./internal/opc -count=1`

4. 正式发布器完成 `npm ci`、全量前端测试、前端构建、`go test ./...` 和带身份清单的管理端构建，退出码0。未执行一小时或过夜长测。

## 现场只读验收

三台均验证以下节点：

| NodeId | 类型/权限 | 现场值 | 质量 |
| --- | --- | --- | --- |
| ns=2;s=9CYE91GH201_COM1 | UInt32/只读 | 2 | Good |
| ns=2;s=9CYE91GH201_COM2 | UInt32/只读 | 1 | Good |

三台Browse关系、实际Read和订阅首值均通过，与对应 `Sources/rcd-58/.../CommunicationState`、`Sources/rcd-46/.../CommunicationState` 一致。数值2表示58对应站点通信异常，不是本次OPC输出发布失败；未据此判断物理故障原因。

健康及就绪检查通过，来源连接分别为3/3、3/3、4/4，在途命令0。活动配置hash、revision和1907条公式数量与部署前一致。采集和Agent的PID、启动身份、实际SHA不变；141/141/97份下级配置文件哈希不变。46的Semaphore及通信程序身份也未变。三台新管理端NRestarts均0；此为部署后短时核查，不代表长时稳定性已验收。

## 发布身份与证据

- 管理端二进制SHA256：`c670cd34d901fed325ca3570c344b7d8b308baea5d1f85b75ff2e191d39143f9`
- 源码清单SHA256：`70372fc07554017376e5b12165620bff1adc58994178b1aa8bb8a32a44400f1d`
- 发布包SHA256：`5fd25f6f808b134344cac7ff05b7cd1cf02085eadd073bfc79b7f2c04b47fe9c`
- 平台包ID：`3bf018b02ce8ff1f8317f1672f07b1675feaa24a5f0e1923c1df44371684ac804`
- 未提交/推送；以现有开发线的完整脏源码清单记录，不宣称来自干净提交。数据库schema仍为16，未新增迁移。

46证据目录：`/home/lenovo/code/rcd_state_outputs_20261023_X86kMz/`。

包括 `targeted-test.log/.exit`、`race.log/.exit`、`build.log/.exit`、`manager-build-result.json`、`package.json`、`browser-upload-30/`、各主机 `upgrade-*-intent/accepted/result.json`、`before-manager-observation.json`、`after-manager-*.json`、`opc-live-*.json`、`before-fleet.json`、`after-fleet.json`、`postdeploy-identity-verification.json`。一致性备份和发布事务由原生发布器随任务保留。

回退到0.6.17会失去状态输入直接发布能力；不应将旧版仍能加载同一数据库理解为该功能仍然存在。配置未被本次发布改写。
