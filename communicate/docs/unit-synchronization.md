# 机组间串口数据同步

客户端连接、完整变量清单、状态编号及读写示例统一见[OPC客户端接入说明](opc-client-guide.md)。业务点名与 FromUnit 原文逐字一致，例如 `DATA.Buildings.91UBH.Fire`、`DATA.PumpStationCtrl[0]`。内部task/port ID不参与业务NodeId。

## 协议与字段布局

依据用户提供的01BAOWEIFENXI.xlsx、91BAOWEIFENXI.xls及FromUnit清单。三个小端UInt16依次为总字节数、原始载荷长度、存储载荷长度；后接载荷和小端CRC32。CRC覆盖头及存储载荷。两种载荷长度相等时不压缩，否则校验完整zlib流、解压长度及边界。短帧可在传输层合法，但不能用不完整布局刷新全部接收点。

偏移以解压载荷第0字节为起点：

| 方向 | 原始字段 | 偏移 | 字节数 | OPC类型 |
| --- | --- | ---: | ---: | --- |
| 1→9 | DATA.Header.nBlock.z | 0 | 2 | UInt32，固定1 |
| 1→9 | DATA.Header.detectorTemp3 / detectorTemp6 | 2 / 3 | 各1 | UInt32 |
| 1→9 | DATA.AKONs.1CYE18GH204.HW | 4 | 969 | String，1938位十六进制 |
| 1→9 | DATA.PumpStationCtrl | 973 | 8 | String，16位十六进制 |
| 1→9 | DATA.Buildings.<楼宇>.Fire，35项 | 981 | 每项5 | UInt32状态 |
| 9→1 | DATA.Header.nBlock.z | 0 | 2 | UInt32，固定9 |
| 9→1 | DATA.AES.SGC85.State | 2 | 5 | String，10位十六进制 |
| 9→1 | DATA.AES.GMM1 | 7 | 35 | String，70位十六进制 |
| 9→1 | DATA.PumpStationCtrl[0] | 42 | 8 | String，16位十六进制 |
| 9→1 | DATA.Buildings.<楼宇>.Fire，11项 | 50 | 每项5 | UInt32状态 |

布局严格为1156/105字节。接收分别40/15个原始变量；去掉自动填写的Header后，发送输入为39/14个。名称、类型、缺项、同名均在启动和配置保存前检查，不能通过静默加前缀修复冲突。9→2目前仅有名称清单，缺少完整载荷依据，保持不支持。

楼宇状态数值1–8分别为正常、红报、红报闪、黄报、黄报闪、故障、故障闪、闭锁；0仅用于接收未知。收发共用确定的五字节映射。91BAOWEIFENXI.xls Sheet2中的开路、短路、手动编码重复，不能区分细因。未知编码不改为正常；Good表示有效帧观测，值0仍表示未知。报警与采集质量分别判断。

## 收发时序

每物理串口只允许一个同步任务，持续解码接收。先处理对方数据；有半帧时等完成或frame_timeout_ms到期，随后满足idle_gap_ms才发送。send_interval_ms从本次发送完成计时，不追赶积压周期。两端initial_delay_ms错峰；静默检测不能保证物理层永不同时起发。

发送输入来自OPC客户端。变化立即更新缓存，串口在下一个安全机会发送。发送开始固定完整快照；发送中更新供下一次使用。部分写失败结束当前连接，不从第一字节重发。没有业务ACK，不借用采集程序的_Send/00 50 50。

接收超时receive_timeout_ms后清零并标记BadNoCommunication，保留最后有效SourceTimestamp。坏帧不刷新有效接收时间；RejectedFrames、DiscardedBytes、PartialTimeouts累计诊断保留。

## 输入缓存和原子提交

直接写原名发送节点，或通过辅助变量TransmitSnapshot提交完整fields。Snapshot读取为状态JSON，不回显提交正文。完整字段、类型、枚举和revision联合校验后才替换；失败不修改值、有效期和revision。首次原子成功后进入atomic模式，拒绝逐点混写。进程重启清空输入，禁止重放旧快照。

刷新要求与串口发送周期分别配置：

```toml
[ports.synchronization]
input_refresh_interval_ms = 5000
snapshot_max_age_ms = 15000
send_interval_ms = 5000
```

客户端同值写入续期；Read/Subscribe、管理端缓存和采样均不续期。首次未齐或任一输入超过15秒则停止整包发送。提交Good只表示缓存接受，complete_writes只证明完整串口写出。

管理端按Session组选源固定来源和epoch，转发原始NodeId与类型；无ACK等待、自动重投或向其它来源补写。同步输入与采集命令FIFO是独立路径；在途写入仍计入维护排空和组选源保护。

## 诊断与配置

直连通信程序的命名空间URI `urn:RcdCommunicate:diagnostics` 仍有
`Diagnostics/Ports/<port_id>/SetTransmitSnapshot(Request String)->Response String`，用于已有原始完整载荷诊断。日常客户端优先使用管理端的TransmitSnapshot及原名字段。

配置网页沿用Operations v1，可编辑config.toml及name列点表，校验/CAS保存后重启生效。默认样例端口和任务停用；仅完成OPC配置验证不代表真实串口验收。
