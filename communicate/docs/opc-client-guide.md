# 串口通信程序 OPC 客户端使用说明

适用对象：组态软件、SCADA及其他OPC UA客户端的接入、配置和开发人员。\
适用接口：通信程序0.2.0及46管理端0.6.11-names.20261020.1。\
文档日期：2026-10-21。带revision的选源Method已随管理端`0.6.12-state-names.20261021.1`部署到30、58、46；其他旧服务须先确认是否支持，旧String接口继续有效。此次未升级通信程序或启用串口，见[管理端发布记录](../../manager/docs/validation/state-input-digit-names-20261021.md)。

本说明针对新增的机组间串口同步通信，不是原采集程序的 `<串口>_Send` 控制命令接口。

## 1. 先看这几条

1. 客户端连接管理端 `opc.tcp://192.168.10.46:4840`。无需直连通信程序4842。
2. 当前46采用“1号侧示例角色”：接收9号侧的数据，发送1号侧的数据。**串口、接收任务和发送开关仍停用**；已完成OPC接入，尚未完成真实串口收发验收。
3. 业务名称沿用原文档，例如 `DATA.Buildings.91UBH.Fire`，不再加 `rx9.`、`RcdSync.*.TX.` 等前缀。
4. 楼宇Fire状态是 `UInt32` 数值，不是五字节字符串。正常=1，红色报警=2，其他状态见第5节。
5. 发送可选“逐点写入”或“整包原子提交”。有跨变量一致性要求时使用整包。
6. 默认无变化每5秒刷新，有变化立即向OPC写入；15秒内没有成功刷新任一必需输入，会停止整包发送。不能只写变化值而从不刷新其他值。
7. **Write返回Good只表示输入已接受，不表示串口已经发送，更不表示对方已收到。**

当前串口停用时，业务节点显示 `BadOutOfService`，发送写入也会被拒绝。这不是客户端变量名或状态编号错误。不要为了消除坏质量擅自启用串口；启用前须由现场负责人确认实际角色、独占串口和通信参数。

## 2. 连接、选源和节点定位

### 2.1 连接参数

| 项目 | 当前46环境 |
| --- | --- |
| 管理OPC地址 | `opc.tcp://192.168.10.46:4840` |
| 业务浏览位置 | `Objects → Devices` |
| 业务命名空间URI | `urn:SerialServer` |
| 管理端业务命名空间索引 | `ns=2` |
| 管理命名空间URI / 索引 | `urn:RcdManager:management` / `ns=3` |
| 本通信逻辑组ID | `unit-sync-46` |
| 本通信来源ID | `communicate-46` |

安全策略、证书信任及用户令牌按现场管理端已经提供的Endpoint配置选择；Web账号与OPC身份不是同一个概念。本说明不要求修改安全配置。

### 2.2 确认本客户端Session选择的来源

通过Browse找到下列管理变量，或者使用对应字符串NodeId：

| 完整NodeId | 类型 | 操作 |
| --- | --- | --- |
| `ns=3;s=Groups/unit-sync-46/MemberSourceIds` | String数组 | 只读，组内已配置成员ID，含离线、停用及不可选成员 |
| `ns=3;s=Groups/unit-sync-46/SelectedSourceId` | String | 读写，本Session当前来源；旧写法不携带客户端期望版本 |
| `ns=3;s=Groups/unit-sync-46/SelectionRevision` | UInt64 | 只读，本Session/本组的路由版本，可用于SelectSource的ExpectedRevision |

读取SelectedSourceId，当前应为 `communicate-46`。只有一个成员且已经选中时，不需要周期性重写。成员ID列表不代表健康列表；展开 `Groups/unit-sync-46/Members/<来源ID>/`，分别检查Selectable、ConnectionState、CatalogState和CompleteCoverage。允许选择有缓存目录的离线来源，因此Selectable=true也不保证在线或数据质量Good。

推荐用新增Method防止旧操作覆盖Web或其他操作的新选择：

若要取得一致的“当前选择＋版本”用于显示确认，可依次读Revision、SelectedSourceId、Revision；前后版本相同才视为同一份路由，变化则重新读取。两个独立Read不能当成原子快照。

```text
ObjectId：ns=3;s=Groups/unit-sync-46
MethodId：ns=3;s=Groups/unit-sync-46/SelectSource
输入0 SourceId：String，communicate-46
输入1 ExpectedRevision：UInt64，刚在本Session读取的SelectionRevision
输出0 SelectedSourceId：String，本次接受的来源
输出1 SelectionRevision：UInt64，本次提交后的路由版本
```

例如读取的SelectionRevision为10，调用参数必须是String `communicate-46`、UInt64 `10`。成功时Call结果为Good，输出为来源ID和UInt64 `11`。不能把UInt64当String、UInt32或Double传入。若Web已将路由更新到11，该旧请求返回BadInvalidState，不改变来源，不输出成功结果；重新读取并让操作人员确认新选择，不能自动把ExpectedRevision改成最新值重试旧意图。

参数缺少/多余返回BadArgumentsMissing/BadTooManyArguments；类型错误返回BadTypeMismatch；空SourceId返回BadInvalidArgument。不存在的来源返回BadNotFound，来源不属于该组、停用、目录未完成或冲突返回BadConfigurationError；在途命令/写入锁定该组时返回BadWaitingForResponse。

没有Method支持的旧客户端仍可把 **String** `communicate-46` 写入SelectedSourceId，并检查Write结果及读回值。该写法保留兼容，但不会校验客户端之前读到的Revision；不能把“先读Revision再普通Write”当成并发保护。新Method部署前Browse不会出现该节点，不应盲目循环Call。

两种方式都是显式手动选源，会使本Session/本组退出自动选源参与；每次成功操作会推进路由版本，同值操作也不应当作心跳。返回的版本代表本次提交，不阻止之后的合法操作再次改变选择。已有订阅会异步刷新，Method成功不表示全部新通知已经到达。

**两个revision不能混用：** SelectionRevision用于管理端选源；TransmitStatus.revision用于下级发送快照的整包提交。前者不能填入TransmitSnapshot请求。

选源与后续业务Read/Subscribe/Write必须使用**同一个OPC Session**。另开连接写入选源不会替原连接切源。切换影响该Session的整个逻辑组，不是单独一个变量；不要把IP或网页显示名称作为来源ID填写，也不需要修改全局DefaultSourceId。

新Session从组默认来源初始化；重新建立的新Session应重新检查选择。若将来切换到其他物理来源，须重新读取该来源的发送状态/revision并提供一份新鲜完整输入，不沿用前一来源的revision。

不同Session的选源相互独立，但**写到同一物理来源的发送缓存是共享的**。建议每个物理发送目标只有一个数据生产客户端，避免多个客户端互相覆盖或产生revision冲突。

切源只改变读取及后续写入路由，不撤销旧下级已经接受的发送快照。旧来源可能在输入过期前继续周期发送；若还有其他客户端续期，则可能持续更久。不要用SelectedSourceId或SelectSource充当“停止旧来源发送”或“唯一发送者切换”接口。

### 2.3 怎样填写变量名

以接收变量为例：

```text
BrowseName：DATA.Buildings.91UBH.Fire
完整NodeId：ns=2;s=DATA.Buildings.91UBH.Fire
类型：UInt32
属性：Value
```

点号和方括号均属于**同一个字符串变量名**，不是额外的文件夹路径：

```text
ns=2;s=DATA.PumpStationCtrl[0]
```

这里的 `[0]` 不代表OPC数组下标。这是一个String标量，读取/写入时IndexRange应留空，不发送 `IndexRange=0`。

以下各表的“变量名”前面加上 `ns=2;s=`，就是管理端完整NodeId。大小写、点号、数字、方括号必须原样保留。

## 3. 当前46的接收变量：9号侧 → 1号侧

“接收”是指通信程序从串口对方收到后发布，OPC客户端只读/订阅。当前共15项，AccessLevel为1（CurrentRead）。

| 序号 | 完整变量名（不含ns=2;s=） | OPC类型 | 含义 / 格式 |
| --- | --- | --- | --- |
| 1 | `DATA.Header.nBlock.z` | UInt32 | 有效9→1帧的固定身份9；客户端只读 |
| 2 | `DATA.AES.SGC85.State` | String | 原始5字节，10个十六进制字符；不是Fire编号 |
| 3 | `DATA.AES.GMM1` | String | 原始35字节，70个十六进制字符 |
| 4 | `DATA.PumpStationCtrl[0]` | String | 原始8字节，16个十六进制字符；保留方括号 |
| 5 | `DATA.Buildings.91UBH.Fire` | UInt32 | Fire状态，见第5节 |
| 6 | `DATA.Buildings.91UGM.Fire` | UInt32 | Fire状态，见第5节 |
| 7 | `DATA.Buildings.91UGQ.Fire` | UInt32 | Fire状态，见第5节 |
| 8 | `DATA.Buildings.91UKT.Fire` | UInt32 | Fire状态，见第5节 |
| 9 | `DATA.Buildings.91UKZ.Fire` | UInt32 | Fire状态，见第5节 |
| 10 | `DATA.Buildings.91UYA.Fire` | UInt32 | Fire状态，见第5节 |
| 11 | `DATA.Buildings.91UZD.Fire` | UInt32 | Fire状态，见第5节 |
| 12 | `DATA.Buildings.11UGS.Fire` | UInt32 | Fire状态，见第5节 |
| 13 | `DATA.Buildings.12UGS.Fire` | UInt32 | Fire状态，见第5节 |
| 14 | `DATA.Buildings.13UGS.Fire` | UInt32 | Fire状态，见第5节 |
| 15 | `DATA.Buildings.91UYZ.Fire` | UInt32 | Fire状态，见第5节 |

例如，读取 `DATA.Buildings.91UBH.Fire` 得到 `Value=3, StatusCode=Good`，表示对方上报“红色报警闪”。

Header只有在收到有效帧后才有对应身份含义。当前停用时可能显示0和BadOutOfService，不能把这个0解释为“0号机组”。

HW、AES、GMM和泵站原始块没有足够的内部业务字段定义，因此保持原始定长十六进制String，不擅自拆分或套用Fire状态含义。

## 4. 当前46的发送变量：1号侧 → 9号侧

“发送输入”由OPC客户端提供，通信程序负责类型校验、数据整合、组包和串口发送。当前共39项，AccessLevel为3（CurrentRead | CurrentWrite）。

| 序号 | 完整变量名（不含ns=2;s=） | OPC类型 | 允许写入的范围 / 格式 |
| --- | --- | --- | --- |
| 1 | `DATA.Header.detectorTemp3` | UInt32 | 0…255，协议原始字节；不自行换算温度 |
| 2 | `DATA.Header.detectorTemp6` | UInt32 | 0…255，协议原始字节 |
| 3 | `DATA.AKONs.1CYE18GH204.HW` | String | 969字节，严格1938个十六进制字符 |
| 4 | `DATA.PumpStationCtrl` | String | 8字节，严格16个十六进制字符 |
| 5 | `DATA.Buildings.11UBE.Fire` | UInt32 | 1…8，见第5节 |
| 6 | `DATA.Buildings.11UBF.Fire` | UInt32 | 1…8，见第5节 |
| 7 | `DATA.Buildings.11UBN.Fire` | UInt32 | 1…8，见第5节 |
| 8 | `DATA.Buildings.11UBR.Fire` | UInt32 | 1…8，见第5节 |
| 9 | `DATA.Buildings.11UBS.Fire` | UInt32 | 1…8，见第5节 |
| 10 | `DATA.Buildings.11UBT.Fire` | UInt32 | 1…8，见第5节 |
| 11 | `DATA.Buildings.11UBU.Fire` | UInt32 | 1…8，见第5节 |
| 12 | `DATA.Buildings.11UBZ.Fire` | UInt32 | 1…8，见第5节 |
| 13 | `DATA.Buildings.11UCB.Fire` | UInt32 | 1…8，见第5节 |
| 14 | `DATA.Buildings.11UJA.Fire` | UInt32 | 1…8，见第5节 |
| 15 | `DATA.Buildings.11UJE.Fire` | UInt32 | 1…8，见第5节 |
| 16 | `DATA.Buildings.11UJG.Fire` | UInt32 | 1…8，见第5节 |
| 17 | `DATA.Buildings.11UJZ.Fire` | UInt32 | 1…8，见第5节 |
| 18 | `DATA.Buildings.11UKA.Fire` | UInt32 | 1…8，见第5节 |
| 19 | `DATA.Buildings.11UKC.Fire` | UInt32 | 1…8，见第5节 |
| 20 | `DATA.Buildings.11UKD.Fire` | UInt32 | 1…8，见第5节 |
| 21 | `DATA.Buildings.11UKT.Fire` | UInt32 | 1…8，见第5节 |
| 22 | `DATA.Buildings.11UMA.Fire` | UInt32 | 1…8，见第5节 |
| 23 | `DATA.Buildings.11UMZ.Fire` | UInt32 | 1…8，见第5节 |
| 24 | `DATA.Buildings.11UQA.Fire` | UInt32 | 1…8，见第5节 |
| 25 | `DATA.Buildings.11UQB.Fire` | UInt32 | 1…8，见第5节 |
| 26 | `DATA.Buildings.11USG.Fire` | UInt32 | 1…8，见第5节 |
| 27 | `DATA.Buildings.11USP.Fire` | UInt32 | 1…8，见第5节 |
| 28 | `DATA.Buildings.11USZ.Fire` | UInt32 | 1…8，见第5节 |
| 29 | `DATA.Buildings.11UYZ.Fire` | UInt32 | 1…8，见第5节 |
| 30 | `DATA.Buildings.12UBE.Fire` | UInt32 | 1…8，见第5节 |
| 31 | `DATA.Buildings.12UBF.Fire` | UInt32 | 1…8，见第5节 |
| 32 | `DATA.Buildings.12UBT.Fire` | UInt32 | 1…8，见第5节 |
| 33 | `DATA.Buildings.12UBZ.Fire` | UInt32 | 1…8，见第5节 |
| 34 | `DATA.Buildings.13UBF.Fire` | UInt32 | 1…8，见第5节 |
| 35 | `DATA.Buildings.13UBZ.Fire` | UInt32 | 1…8，见第5节 |
| 36 | `DATA.Buildings.14UBZ.Fire` | UInt32 | 1…8，见第5节 |
| 37 | `DATA.Buildings.1JME34.Fire` | UInt32 | 1…8，见第5节 |
| 38 | `DATA.Buildings.1JMF10.Fire` | UInt32 | 1…8，见第5节 |
| 39 | `DATA.Buildings.1JMF34.Fire` | UInt32 | 1…8，见第5节 |

不要将接收的 `DATA.PumpStationCtrl[0]` 与发送的 `DATA.PumpStationCtrl` 混淆；二者是文档定义的不同变量名。

发送输入中**不包含** `DATA.Header.nBlock.z`。本方发送身份由程序固定填写为1，客户端不能写入或在整包fields中附加该字段。

原始块String统一要求：

- 两个十六进制字符表示一个字节，例如8字节为16字符。
- 接受数字0…9、字母A…F/a…f；不含空格、换行、逗号、`0x`、冒号或其他分隔符。
- 长度必须严格一致，不能省略前导0，不得写入原始二进制ByteString。
- 空字符串不是“保持原值”，不足长度也不会自动补0。

## 5. Fire状态编号

本节只适用于 `DATA.Buildings.<名称>.Fire`。所有接收和发送方向采用同一套编号。

| OPC数值（UInt32） | 含义 | 对应串口五字节（十六进制） | 客户端可否发送 |
| --- | --- | --- | --- |
| 0 | 未知编码，不代表正常 | 没有固定对应 | 不允许 |
| 1 | 正常 | `0A 85 00 06 00` | 允许 |
| 2 | 红色报警 | `01 88 00 07 00` | 允许 |
| 3 | 红色报警闪 | `01 C8 00 07 00` | 允许 |
| 4 | 黄色报警 | `05 86 00 0C 00` | 允许 |
| 5 | 黄色报警闪 | `05 A6 00 0C 00` | 允许 |
| 6 | 故障报警 | `06 82 00 12 00` | 允许 |
| 7 | 故障报警闪 | `06 C2 00 12 00` | 允许 |
| 8 | 闭锁 | `09 85 00 04 00` | 允许 |

1…8是本OPC接口的状态编号，串口仍使用表中的原始五字节；客户端不需要自行拼接这五字节、压缩、长度头或CRC。

依据 `91BAOWEIFENXI.xls` 状态表，开路、短路、手动等部分故障示例使用相同五字节，现有报文不能区分具体原因，因此统一落在6/7，不提供虚构的细分类。

请区分两种0：

- **Good / 0**：收到有效业务帧，但其中五字节未匹配已定义的Fire编码。
- **非Good / 0**：可能是尚无数据、停用或通信中断，不能据此判定设备状态。

发送必须选择真正的OPC **UInt32** 类型。下列写法不同：

| 写法 | 是否正确 |
| --- | --- |
| OPC UInt32数值 `2` | 正确，红色报警 |
| OPC String `"2"` | 错误，类型不符 |
| OPC Double/Float数值 `2.0` | 错误，不是UInt32 |
| OPC UInt32数值 `0` 或 `9` | 错误，超出允许的发送状态 |
| OPC String `"0188000700"` 写Fire | 错误，Fire不接受五字节字符串 |

整包JSON里的Fire也必须使用整数 `2`，不能写为 `"2"` 或 `2.0`。

## 6. 发送方式一：逐点写入

### 6.1 操作顺序

1. 确认本Session选中正确来源，检查串口和发送功能已经由现场负责人启用。
2. 读取 `InputRefreshIntervalMs`、`InputTimeoutMs` 和 `TransmitStatus`，确认当前参数以及 `input_mode="individual"`。
3. 首次向第4节的**全部39个输入**逐项写入有效值。
4. 后续无变化时按默认5秒周期刷新全部39项；有变化立即写相应输入。即使值相同，也需要刷新。
5. 对每一项检查Write的节点级结果。不能只看网络调用成功或整次服务请求成功。
6. 读取发送状态；所有必需输入齐全且新鲜后，才具备组包发送条件。

只写一个Fire并不能形成完整发送包；少了HW、泵站、温度或其他任一Fire，整包不会发送。刷新某一项只更新它自己的有效期；客户端Read、Subscribe及管理端自动订阅都不会替生产者续期。

### 6.2 单点示例A：设置11UBE红色报警

在客户端的写入对话框中填写：

```text
NodeId：ns=2;s=DATA.Buildings.11UBE.Fire
Attribute：Value
Value类型：UInt32
Value：2
IndexRange：留空
```

发送普通Value即可，不附带Bad/Uncertain状态码。检查该节点的Write结果：

- Good：本次输入被接受。仍需其他38项已齐全、新鲜，且等待串口调度。
- BadOutOfService：当前发送或串口停用，输入未被接受。
- 其他非Good：按第10节处理，不能当作刷新成功。

### 6.3 单点示例B：泵站控制原始块

```text
NodeId：ns=2;s=DATA.PumpStationCtrl
Value类型：String
Value：0000000000000000
IndexRange：留空
```

这只是长度正确的8字节格式示例，**不是确认过的业务安全指令**。实际值须由上层系统提供并确认。

### 6.4 逐点方式的一致性边界

逐点写入互相独立。即使一次OPC Write请求携带39个节点，也**不是39项全成全败的事务**；在分项到达与串口发送之间可能出现“部分字段新、部分字段旧”。

同一输入的并发写入可能因占用被拒绝。建议由一个写入任务串行管理数据刷新；需要整批一致性时改用第7节，而不是尝试通过写入顺序模拟原子性。

## 7. 发送方式二：整包原子提交（跨变量一致性时使用）

### 7.1 入口与操作步骤

整包通过写入一个String变量完成，不需要调用Method：

```text
NodeId：ns=2;s=TransmitSnapshot
Attribute：Value
Value类型：String
Value：完整JSON文本
IndexRange：留空
```

请求结构为 `{"revision": 当前版本, "fields": {完整发送字段}}`。

1. 使用**同一个Session**读取 `ns=2;s=TransmitStatus`，解析返回的JSON字符串，取得当前 `revision`。
2. 将全部39个当前业务值放入fields，不带接收字段，也不带Header身份字段。
3. 将整个JSON文本作为一个OPC **String**写入TransmitSnapshot。不要把JSON对象作为OPC结构体，也不要再次对整段JSON加一层外部字符串引号。
4. 检查该单节点的Write结果。Good后，成功提交的revision为本次请求值加1。
5. 无变化每5秒重新提交一份完整快照；有变化立即提交包含全部字段的新快照。每次都使用对应的当前revision。
6. 再读TransmitStatus和需要的发送输入确认接受状态，观察complete_writes，而不是期待Write返回设备ACK。

管理端的读取来自订阅缓存，成功写入后短时间仍可能看到旧revision。等待状态推进；不要因此将旧revision/旧数据反复重投。每次成功的逐点写入也会推进同一revision。

### 7.2 当前46可用的完整39项示例：1号侧 → 9号侧

以下是**完整字段集**，没有省略项；HW使用了1938个十六进制字符（969字节）的全0示例。温度字节20/21、Fire及原始块均为格式演示值，不能直接作为真实业务数据使用。

示例revision为0，必须替换为刚从目标来源读取的当前值。当前46串口停用，实际写入预期被拒绝；本例不表示已经允许启用发送。

<!-- example: unit1-to9 -->

```json
{
  "revision": 0,
  "fields": {
    "DATA.Header.detectorTemp3": 20,
    "DATA.Header.detectorTemp6": 21,
    "DATA.AKONs.1CYE18GH204.HW": "000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000",
    "DATA.PumpStationCtrl": "0000000000000000",
    "DATA.Buildings.11UBE.Fire": 2,
    "DATA.Buildings.11UBF.Fire": 1,
    "DATA.Buildings.11UBN.Fire": 1,
    "DATA.Buildings.11UBR.Fire": 1,
    "DATA.Buildings.11UBS.Fire": 1,
    "DATA.Buildings.11UBT.Fire": 1,
    "DATA.Buildings.11UBU.Fire": 1,
    "DATA.Buildings.11UBZ.Fire": 1,
    "DATA.Buildings.11UCB.Fire": 1,
    "DATA.Buildings.11UJA.Fire": 1,
    "DATA.Buildings.11UJE.Fire": 1,
    "DATA.Buildings.11UJG.Fire": 1,
    "DATA.Buildings.11UJZ.Fire": 1,
    "DATA.Buildings.11UKA.Fire": 1,
    "DATA.Buildings.11UKC.Fire": 1,
    "DATA.Buildings.11UKD.Fire": 1,
    "DATA.Buildings.11UKT.Fire": 1,
    "DATA.Buildings.11UMA.Fire": 1,
    "DATA.Buildings.11UMZ.Fire": 1,
    "DATA.Buildings.11UQA.Fire": 1,
    "DATA.Buildings.11UQB.Fire": 1,
    "DATA.Buildings.11USG.Fire": 1,
    "DATA.Buildings.11USP.Fire": 1,
    "DATA.Buildings.11USZ.Fire": 1,
    "DATA.Buildings.11UYZ.Fire": 1,
    "DATA.Buildings.12UBE.Fire": 1,
    "DATA.Buildings.12UBF.Fire": 1,
    "DATA.Buildings.12UBT.Fire": 1,
    "DATA.Buildings.12UBZ.Fire": 1,
    "DATA.Buildings.13UBF.Fire": 1,
    "DATA.Buildings.13UBZ.Fire": 1,
    "DATA.Buildings.14UBZ.Fire": 1,
    "DATA.Buildings.1JME34.Fire": 1,
    "DATA.Buildings.1JMF10.Fire": 1,
    "DATA.Buildings.1JMF34.Fire": 1
  }
}
```

检查点：只有39个fields；第一个Fire值2表示红色报警，其余示例1表示正常；HW必须保留完整1938字符。若从排版工具复制，请确认没有把长字符串截断或插入换行。

### 7.3 原子模式规则

- 所有字段先完成名称、类型、长度和状态校验，再一次接受整份快照；失败不会只更新前半包。
- 缺项、多项、重复键、错误类型、非法状态、错误长度或revision冲突均整体拒绝；失败不会延长旧输入的有效期。
- 单次完整请求最大64KiB；String采用正常JSON文本编码。
- 首次整包成功后，目标通信程序本次进程进入 `atomic` 模式。后续**不能再逐点写入**，必须继续提交完整快照，不能随意混用。
- 同一物理来源的所有客户端共用该模式和revision。关闭或重建OPC Session不会退出atomic模式。
- 通信程序重启会清空发送输入、revision和模式；客户端必须重新读取状态、重新提供完整输入。不能只在OPC重连时假定通信程序也重启了。
- 多变量“一包一致”只由TransmitSnapshot保证；多个独立OPC Read也不构成跨请求的一致性快照。

若revision冲突，先重新读取目标来源的状态，确认是否有其他写入者或是否刚切过来源，再依据最新业务数据重新组织完整包。

若请求超时/连接中断，结果可能不明；**不要盲目重发旧快照**。重新连接、核实来源和当前状态，必要时核对输入值，确认后再提交最新完整数据。多个写入者存在时，revision增加本身不能证明“是哪一个客户端的包被接受”。

### 7.4 反向角色的完整14项示例：9号侧 → 1号侧

仅供以后配置为“9号发送侧”的实例使用。**不能将此14项示例写到当前46的39项发送入口**；会因字段集不匹配被拒绝。

反向角色接收第4节的1→9清单（含Header共40项，只读），发送第3节清单中除Header外的14项（读写）。角色改变的是权限和方向，原变量名不改。

<!-- example: unit9-to1 -->

```json
{
  "revision": 0,
  "fields": {
    "DATA.AES.SGC85.State": "0285000000",
    "DATA.AES.GMM1": "0000000000000000000000000000000000000000000000000000000000000000000000",
    "DATA.PumpStationCtrl[0]": "0000000000000000",
    "DATA.Buildings.91UBH.Fire": 2,
    "DATA.Buildings.91UGM.Fire": 1,
    "DATA.Buildings.91UGQ.Fire": 1,
    "DATA.Buildings.91UKT.Fire": 1,
    "DATA.Buildings.91UKZ.Fire": 1,
    "DATA.Buildings.91UYA.Fire": 1,
    "DATA.Buildings.91UZD.Fire": 1,
    "DATA.Buildings.11UGS.Fire": 1,
    "DATA.Buildings.12UGS.Fire": 1,
    "DATA.Buildings.13UGS.Fire": 1,
    "DATA.Buildings.91UYZ.Fire": 1
  }
}
```

反向同样先读取自己的revision、每5秒完整刷新，Fire类型和编号不变。`DATA.AES.SGC85.State` 仍为原始5字节String，不是Fire编号。

## 8. 刷新、超时和实际串口发送的关系

| 项目 | 默认值 | 客户端需要做什么 |
| --- | --- | --- |
| 输入刷新要求 | 5000ms | 无变化也要定时成功写入；变化时立即写 |
| 输入有效期 | 15000ms | 任一必需输入超期，整包停止发送；须恢复全部有效输入 |
| 串口周期发送间隔 | 5000ms | 程序按时隙发送最新有效快照，不由客户端逐字节发送 |
| 对方接收数据超时 | 15000ms | 长时间没有有效对方业务帧，接收变量输出坏质量 |

前两项可通过 `InputRefreshIntervalMs`、`InputTimeoutMs` 读取。后台配置可能调整，客户端不能永远硬编码5/15秒；应在启动及通信程序重启后重新读取。

**“有变化立即写”指客户端立即向OPC提交最新输入，不承诺立即占用串口发送。** 实际发送仍受周期时隙、对方正在发送的帧、空闲间隔和串口状态限制，程序不会为新值打断对方帧。

这是**当前状态快照同步**，不是逐条事件/控制队列。同一发送时隙前连续更新2→3→1，可能只发出最后的1，不保证三个中间状态都在线路出现。原采集程序的 `_Send` FIFO与本接口是两回事；需要每个动作逐条执行时，不能用本同步接口代替命令协议。

停止生产数据时，不要用旧值心跳持续续期来伪装数据源仍正常。输入过期会阻止继续使用旧快照发送；下级OPC读取、管理端订阅不会自动重放或保活输入。

## 9. 状态监视：怎样判断接收、接受和发送

### 9.1 四个辅助变量

均位于 `Objects → Devices`，与业务变量一样受该Session组内选源影响。

| 完整NodeId | 类型 / 权限 | 用途 |
| --- | --- | --- |
| `ns=2;s=TransmitSnapshot` | String，读写 | 写入完整原子请求；读取返回状态JSON，**不是上次提交原文** |
| `ns=2;s=TransmitStatus` | String，只读 | 当前输入与发送状态JSON |
| `ns=2;s=InputRefreshIntervalMs` | UInt32，只读 | 客户端周期刷新要求，默认5000 |
| `ns=2;s=InputTimeoutMs` | UInt32，只读 | 输入有效期，默认15000 |

当前46共58个业务/辅助节点：15接收变量＋39发送输入＋以上4项。两侧的总数相同，但收发权限和发送输入数量不同。

### 9.2 TransmitStatus字段

| JSON字段 | 含义及判断方式 |
| --- | --- |
| `revision` | 目标发送缓存的版本；逐点成功写入及整包成功提交均加1，不是发送次数 |
| `input_mode` | `individual`逐点模式；`atomic`整包模式 |
| `transmit_enabled` | 配置中的发送开关；true不等于串口已就绪或对方已连接 |
| `required_inputs` | 必需发送输入数；当前46为39，反向为14 |
| `received_inputs` | 已接受的字段数；包含可能已经过期的字段，不可单独据此判断新鲜 |
| `stale_inputs` | 已接受字段中过期的数量 |
| `snapshot_fresh` | 该快照当前是否完整且未过期；仍不能单独证明串口已写出 |
| `input_refresh_interval_ms` | 刷新要求，默认5000 |
| `input_timeout_ms` | 输入有效期，默认15000 |
| `snapshot_age_ms` | 普通fields输入方式可能为null；不要用null推断从未收到或发生故障 |
| `complete_writes` | 本次进程完整写出串口帧的累计次数，周期重发也会增加，不与revision一一对应 |
| `last_send_error` | 最近发送错误；空字符串不等于已发送成功，可能尚未尝试 |
| `meaning` | 状态语义说明文本 |

当前停用且刚启动时，常见状态为：transmit_enabled=false、required_inputs=39、received_inputs=0、complete_writes=0。这与停用配置一致，不是队列丢失。

### 9.3 三种“成功”不要混淆

| 看到的结果 | 能说明什么 | 不能说明什么 |
| --- | --- | --- |
| 发送变量Write返回Good | 通信程序接受了这一项输入 | 其他字段齐全、已经发出 |
| TransmitSnapshot Write返回Good | 完整快照校验通过并原子接受 | 对方已经收到或动作完成 |
| complete_writes增加 | 本程序完成了一次串口帧写出 | 对方已接收、已处理或动作已完成 |

本同步协议没有对方逐包接收ACK，**不要等待 `00 50 50` 作为本接口的成功依据**；该ACK属于原采集控制命令，不能套用到机组同步。

本接口的常规状态刷新不逐条进入原 `_Send` 命令审计列表。请观察TransmitStatus、业务质量及实际对方反馈，而不是只在“命令记录”页寻找每个数据输入。

## 10. 数据质量与写入失败处理

### 10.1 Read/Subscribe时

必须同时使用Value与DataValue.StatusCode，不能只显示数值。

| 质量 | 十六进制码 | 客户端含义与处理 |
| --- | --- | --- |
| Good | `0x00000000` | 当前业务值有效；辅助状态节点Good仅表示诊断状态可读 |
| BadWaitingForInitialData | `0x80320000` | 已启用但尚无首份有效数据/输入 |
| BadNoCommunication | `0x80310000` | 串口/对方数据超时、输入过期，或管理端与下级连接不可用；结合来源连接及发送状态定位 |
| BadConfigurationError | `0x80890000` | 解析/字段配置不匹配、所选来源缺点等配置问题 |
| BadOutOfService | `0x808D0000` | 停用、退出或退役；当前46停用通信节点出现此质量是预期结果 |

经本文主入口——管理端4840——读取时，接收超时、发送输入过期或来源断线导致BadNoCommunication，管理端将这些非计算变量按类型清零：UInt32为0，String为空字符串，并保留可用的源时间。清零只影响对外读取，不表示客户端成功提交了0或清除了下级缓存。

维护人员直连通信程序4842时，过期发送输入可能仍保留最后一次接受的值并返回BadNoCommunication；不要将它与管理端读值混为一谈。**不论显示0、空字符串还是旧值，只要质量非Good，就不能当作当前有效业务状态。**

接收变量SourceTimestamp表示完整有效报文解析后的本方接收时间，不是对方设备内部时间。发送输入的SourceTimestamp表示最后一次成功输入时间；ServerTimestamp不是“最后正常接收时间”。

### 10.2 Write时

每次检查WriteResponse中**对应节点的结果**。服务请求本身无异常，不代表每个变量都写成功。

| 常见结果 | 处理 |
| --- | --- |
| Good | 已接受输入，不等于串口或业务完成 |
| BadOutOfService | 检查停用、维护、程序退出状态；当前停用环境不得持续重试 |
| BadNotWritable | 选到了接收变量/只读辅助变量；核对权限和方向 |
| BadTypeMismatch / BadInvalidArgument / BadConfigurationError | 核对真正的OPC类型、字段集、状态值、String长度及模式；不同校验层可能返回不同码 |
| BadInvalidState（整包） | revision冲突；重新核对目标来源和当前版本，不覆盖他人新数据 |
| BadWaitingForResponse | 可能有维护、选源锁或同一输入在途占用；先检查状态，避免高频并发重试 |
| BadIndexRangeInvalid | 清空IndexRange；名称中的[0]不是OPC数组 |
| BadNodeIdUnknown | 节点不存在；检查是否还在使用旧前缀名，或连接到错误角色/实例 |
| BadTimeout / 请求中断 | 结果可能不明；对账后提交最新状态，不盲目重放旧包 |

逐点方式即使字段显示AccessLevel=3，也可能因atomic模式拒绝写入。此时应继续整包提交，不能通过换Session绕过模式限制。

### 10.3 收不到预期数据时的排查顺序

1. 查客户端是否连接46管理端4840，而不是其他环境。
2. 查本Session选源是否为communicate-46，来源是否连接正常。
3. 查变量名、数据类型、收发方向及是否旧前缀。
4. 查DataValue质量；当前停用时先停止数据发送测试，不把坏质量当正常业务值。
5. 查input_mode、required_inputs、received_inputs、stale_inputs、snapshot_fresh。
6. 查每次Write的节点级结果和周期刷新是否真实成功。
7. 查complete_writes是否推进、last_send_error是否出现错误。
8. 若已完整写出但对方未收到，由现场人员继续核对线路、串口参数、实际角色及对方程序；不能只凭OPC Write Good认定对方故障。

## 11. 客户端联调检查表

- [ ] 使用完整原始变量名和正确命名空间，未添加任务/方向前缀。
- [ ] 已在执行Read/Subscribe/Write的同一Session中核对选源。
- [ ] Fire使用UInt32，写入1…8；原始块使用定长String。
- [ ] Header只读，整包fields不包含Header。
- [ ] 已选择逐点或整包方式，未将多个普通Write误认为原子事务。
- [ ] 首次提供全部必需输入；无变化周期刷新，变化立即提交。
- [ ] 每项都检查Write结果；失败不视为续期成功。
- [ ] 多写入者已协调；revision按目标来源读取，不写死。
- [ ] 接收画面显示质量及时间，不将坏质量的0当正常。
- [ ] 明确“输入接受 / 串口写出 / 对方确认”三者区别。
- [ ] 已确认当前串口停用边界；未把OPC接入测试当作真实物理收发验收。

本次仅提供客户端说明，不更改角色、开关、配置、安全策略或运行服务。本文未实现或宣称支持9→2布局；其变量名和完整报文布局不能与9→1混用。
