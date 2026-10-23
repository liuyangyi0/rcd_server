// Help is bundled with the Web application: it remains readable without a lower
// connection and never submits an OPC, configuration or process-control request.
export interface HelpNode { name: string; type: string; access: string; description: string }
export interface HelpExample { title: string; text: string; note?: string }
export interface HelpSection {
  title: string;
  paragraphs?: readonly string[];
  steps?: readonly string[];
  caution?: string;
  nodes?: readonly HelpNode[];
  table?: { headings: readonly string[]; rows: readonly (readonly string[])[] };
  examples?: readonly HelpExample[];
}
export interface HelpTopic {
  id: string;
  category: string;
  title: string;
  summary: string;
  sections: readonly HelpSection[];
  links: readonly { to: string; label: string }[];
}

// Original protocol names, not KKS aliases. Keep the complete field set in one
// place so the table and the copyable atomic example cannot disagree.
export const transmitFireNames = [
  '11UBE', '11UBF', '11UBN', '11UBR', '11UBS', '11UBT', '11UBU', '11UBZ',
  '11UCB', '11UJA', '11UJE', '11UJG', '11UJZ', '11UKA', '11UKC', '11UKD',
  '11UKT', '11UMA', '11UMZ', '11UQA', '11UQB', '11USG', '11USP', '11USZ',
  '11UYZ', '12UBE', '12UBF', '12UBT', '12UBZ', '13UBF', '13UBZ', '14UBZ',
  '1JME34', '1JMF10', '1JMF34',
].map((name) => `DATA.Buildings.${name}.Fire`);
export const receiveFireNames = [
  '91UBH', '91UGM', '91UGQ', '91UKT', '91UKZ', '91UYA', '91UZD',
  '11UGS', '12UGS', '13UGS', '91UYZ',
].map((name) => `DATA.Buildings.${name}.Fire`);
export const transmitExampleFields = {
  'DATA.Header.detectorTemp3': 20,
  'DATA.Header.detectorTemp6': 21,
  'DATA.AKONs.1CYE18GH204.HW': '00'.repeat(969),
  'DATA.PumpStationCtrl': '00'.repeat(8),
  ...Object.fromEntries(transmitFireNames.map((name) => [name, 1])),
};
export const atomicExample = JSON.stringify({ revision: 0, fields: transmitExampleFields }, null, 2);

const communicationStates = [
  ['0', '未知、切换中或状态已过期'], ['1', '通信正常'], ['2', '通信异常：超时、坏帧或尚无有效数据'],
  ['3', '串口未打开'], ['4', '串口或站点读取已停用'], ['5', '下级 OPC 离线'],
];

export const helpTopics = [
  {
    id: 'relationships', category: '认识系统', title: '软件关系与入口',
    summary: '先分清数据链路、进程启停和软件发布：不是所有操作都在同一个软件里。',
    sections: [
      { title: '每个软件负责什么', table: { headings: ['软件 / 工具', '负责内容', '使用入口与边界'], rows: [
        ['OPC 管理端（Go + Web）', '合并下级数据、Session 选源、中央计算；配置和业务日志入口', '客户连接 4840；浏览器访问本机 8087。三个管理端独立，不是自动主备集群。'],
        ['采集程序（Rust 下级 OPC）', '按 CSV 点表轮询串口和站点；执行控制报文', '管理端连接其 4841。日常启停由目标机器的 Agent 执行。'],
        ['串口通信程序（Rust）', '机组间数据同步；接收数据并整合 OPC 客户端提供的发送输入', '46 的示例来源使用 4842。不是采集轮询，也不是 _Send 命令队列；不由采集 Agent 守护。'],
        ['RCD Agent', '唯一守护本机固定采集进程，持久记录安全启停与维护事务', '管理端后端访问其运维接口（现场通常为 8090）；浏览器不直连。Agent 不承载 OPC 实时数据。'],
        ['Semaphore + Ansible', '登记主机、软件包、升级恢复、管理端安全启停、主机资源巡检', '现场入口为 46:3000；复用原生登录、队列、任务日志和受控发布器。'],
      ] } },
      { title: '现场地址示例', paragraphs: ['以下是现有部署约定，不是实时探测结果。实际地址与身份以“系统信息 → 机器台账”和来源配置为准；显示名称、IP 与稳定 ID 是三回事。'], table: { headings: ['机器', 'OPC 管理网页', '客户 OPC 地址', '采集来源稳定 ID'], rows: [
        ['30', 'http://192.168.10.30:8087', 'opc.tcp://192.168.10.30:4840', 'rcd-30'],
        ['58', 'http://192.168.10.58:8087', 'opc.tcp://192.168.10.58:4840', 'rcd-58'],
        ['46', 'http://192.168.10.46:8087', 'opc.tcp://192.168.10.46:4840', 'rcd-46'],
      ] }, examples: [{ title: '运维平台地址（现场示例）', text: 'http://192.168.10.46:3000' }] },
      { title: '哪些故障不会连带停止其他软件', paragraphs: ['Semaphore 停止不会停止现有采集和 OPC 服务；某个管理端停止不会停止下级，其他独立管理端仍可连接。Agent 停止可能影响其采集子进程，需从平台恢复，不能让 Agent 自己启动自己。', '三个管理端不自动同步计算规则、业务配置、组选源或 Session。平台台账只同步机器和软件绑定；平台离线时使用最后有效缓存，并显示同步时间。'] },
    ], links: [{ to: '/settings', label: '查看系统信息与软件版本' }, { to: '/sources', label: '查看下级来源' }],
  },
  {
    id: 'connect', category: 'OPC 接入', title: '连接、浏览与读取数据',
    summary: 'OPC 客户端连接管理端 4840；先浏览命名空间和节点，再读取或订阅。',
    sections: [
      { title: '第一次连接', steps: ['在 OPC UA 客户端添加目标管理端的 Endpoint，例如 opc.tcp://192.168.10.46:4840。不要把 Web 8087 当成 OPC 端口。', '选择现场提供的安全 Endpoint 和身份，按现场流程信任证书。Web 登录账号与 OPC 身份不同，本帮助不要求更改安全策略。', '读取 NamespaceArray，确认 urn:SerialServer（管理端固定 ns=2）和 urn:RcdManager:management（ns=3）。', 'Browse：Objects → Devices 找业务点；Objects → Methods 找原采集命令方法；管理树查看来源、分组和状态。', '检查节点类型、访问级别和 Value 的 StatusCode，再建立订阅。不要只看数值是否变化。'] },
      { title: '节点填写示例', nodes: [
        { name: 'ns=2;s=<业务点原始名称>', type: '以点表为准', access: '以访问级别为准', description: '尖括号是待替换内容。计算输出保持原 KKS；通信点保留 DATA.* 原始名称。' },
        { name: 'ns=2;s=DATA.Buildings.91UBH.Fire', type: 'UInt32', access: '只读', description: '46 的 1 号侧示例角色：接收 9 号侧楼宇状态。' },
      ], paragraphs: ['点号、方括号属于 NodeId 字符串，不是额外目录。DATA.PumpStationCtrl[0] 是 String 标量，IndexRange 留空，不填写 0。'] },
      { title: '数值与质量一起看', table: { headings: ['质量', '处理方法'], rows: [
        ['Good', '样本有效；诊断节点仍须看数值（例如状态 5 表示离线）。'],
        ['BadWaitingForInitialData', '等待当前连接首值；不要当成正常 0。'],
        ['BadNoCommunication / BadCommunicationError', '沿来源 OPC 连接、串口、站点逐层检查；单凭质量码不能认定物理设备损坏。'],
        ['BadOutOfService', '点或端口/功能停用；核对配置，不要为消除质量提示擅自启用。'],
        ['BadConfigurationError', '检查选源、缺点、目录类型冲突、公式或站点映射。'],
      ] } },
    ], links: [{ to: '/points', label: '浏览点位与类型' }, { to: '/sources', label: '检查下级连接' }],
  },
  {
    id: 'selection', category: 'OPC 接入', title: '整组选源与站点状态',
    summary: '同一个 Session 按整个逻辑组选择来源；推荐使用带 UInt64 revision 校验的 SelectSource。',
    sections: [
      { title: '选源的影响范围', paragraphs: ['选择影响本 Session 的整个组，不是一个变量。另开 Session 写选择不会替原 Session 切源；业务 Read、Subscribe、Write 必须使用同一 Session。', '新 Session 复制组默认来源；恢复原 Session 保留选择，Close/超时后新建则重新初始化。默认来源变更影响后续新 Session 和中央计算，不改变在线 Session 的手动选择。', '自动切源默认关闭。组策略、Session 参与自动、组默认源自动切换是不同开关；显式手动选源会使该 Session/组退出自动参与。数值不一致可告警，但不作为自动切源依据。'] },
      { title: '三个兼容变量', nodes: [
        { name: 'ns=3;s=Groups/<组ID>/MemberSourceIds', type: 'String[]', access: '只读', description: '已配置成员，包含离线、停用和冲突成员；不是健康/可选来源清单。' },
        { name: 'ns=3;s=Groups/<组ID>/SelectedSourceId', type: 'String', access: '本 Session 读写', description: '保留旧接口，但普通 Write 不校验客户端期望版本。' },
        { name: 'ns=3;s=Groups/<组ID>/SelectionRevision', type: 'UInt64', access: '本 Session 只读', description: '用于 SelectSource；不是配置 revision，也不是发送快照 revision。' },
      ], paragraphs: ['组 ID、来源 ID 从分组/来源页面获取，不填显示名称或 IP。成员详情中的 Selectable、ConnectionState、CatalogState、CompleteCoverage 需分别检查；可选的离线缓存来源不等于数据健康。'] },
      { title: '推荐操作：带版本校验的 Method', steps: ['同一 Session 读 Revision → SelectedSourceId → Revision；前后版本相同才当成同一份显示快照，否则重读。', '明确目标来源后，以 String SourceId 和真正的 UInt64 ExpectedRevision 调用 SelectSource。', '检查 Call 的节点级结果及两个输出。BadInvalidState 表示版本冲突：重新读取并人工确认，不自动替换版本重试旧意图。BadWaitingForResponse 表示组有命令占用。', '成功后等待已有订阅异步刷新；Call 成功不表示所有新数据通知已到达。'], examples: [{ title: '选源 Method（46 通信组示例）', text: 'ObjectId: ns=3;s=Groups/unit-sync-46\nMethodId: ns=3;s=Groups/unit-sync-46/SelectSource\nInput[0] SourceId (String): communicate-46\nInput[1] ExpectedRevision (UInt64): <本Session刚读取的SelectionRevision>\nOutput[0] SelectedSourceId (String)\nOutput[1] SelectionRevision (UInt64)', note: '尖括号需替换为真实 UInt64，不能传 String、UInt32 或 Double。旧客户端仍可普通写 SelectedSourceId，但没有此并发保护。' }] },
      { title: '同时观察所有物理站点', nodes: [
        { name: 'ns=3;s=Groups/<组ID>/Members/<来源ID>/Ports/%2Fdev%2FttyWCH0/Devices/11/CommunicationState', type: 'UInt32', access: '只读', description: '物理来源状态，不随 Session 选源。示例串口是 /dev/ttyWCH0，路径段需转义。' },
      ], table: { headings: ['状态值', '含义'], rows: communicationStates }, paragraphs: ['配套 CommunicationStateText（String）和 CommunicationObservedAt（DateTime）。Good 表示判断有效；Value=5、Good 表示“已知下级离线”，不是设备正常。观测时间不是故障开始或最后正常采集时间。优先 Browse 获取完整 NodeId，不重复对 % 编码。'] },
    ], links: [{ to: '/groups', label: '打开分组与 Session 选源' }, { to: '/sources', label: '查看稳定来源 ID' }],
  },
  {
    id: 'commands', category: '日常操作', title: '下发命令、队列与结果',
    summary: '采集命令逐条保存和发送；入队、协议 ACK、设备动作反馈是三个不同结果。',
    sections: [
      { title: '正确下发步骤', steps: ['核对目标管理端、本 Session 的组选源、端口和报文设备地址。先确认维护状态及现场控制许可。', '将审核后的十六进制报文作为 String 写入 <端口>_Send。检查每个节点的 Write 结果，不只检查服务请求成功。', '到“命令记录”按时间、来源、端口查看排队、等待响应、完成或失败，展开“执行过程”查看调用发送与完整写出次数。', 'ACK 成功后，从实际业务反馈点确认设备动作；没有 ACK 或结果未知时先查原记录，不自动补发、切源重发。'], nodes: [{ name: 'ns=2;s=<端口>_Send', type: 'String', access: '写入命令 / 读取状态', description: '端口公开名称以 Browse 为准；这里只给模板，不提供可直接执行的设备控制报文。' }] },
      { title: '队列什么时候起作用', paragraphs: ['管理端 _Send（及组 Send）按物理来源+串口 FIFO，容量 64 条，包含执行中。每次有效提交独立保存，不以相同值去重，也不采用“只保留最后值”。队满返回 BadWaitingForResponse。', '总期限包含排队时间；已排队命令也锁住组选源并计入维护排空。明确完成后接续；断线、管理端超时或执行结果未知时取消未派发项，不重放到其他来源，进程重启也不重放。', '同步 <端口>_SendCommand(HexData String) → Result String 忙时拒绝，不走此 FIFO。空值、IDLE 和 0/1/2/3 是旧被动复位/状态写，不是硬件命令，不应作为发送报文。'] },
      { title: '重试次数与发送间隔在哪里设置', paragraphs: ['“下级 OPC → 目标来源 → 运维 → 配置”修改采集程序参数。command_retry_count 表示最大总尝试次数（含首次），设 3 不是首次加 3 次；收到有效 00 50 50 立即结束重试。', 'command_interval_ms 是同一物理串口上一控制事务结束，到下一条首次发送的最小间隔，范围 0–60000 ms，0 关闭。它不是重试间隔、普通轮询间隔，也不延长命令总期限；设置过大可能使后续排队项超时。保存后需明确重启下级才生效。'] },
      { title: '如何解释结果', table: { headings: ['结果 / 证据', '含义'], rows: [
        ['Write Good / 已受理', '仅接受请求或入队，不能当成动作完成。'], ['ACK 00 50 50', '协议确认成功，不等于已核实设备反馈。'],
        ['调用发送 / 完整写出', '前者是调用次数，后者是完整报文写出次数；部分写入不算完整写出。'],
        ['未知 / 审计不完整', '证据缺失，不代表发送 0 次，不可据此判断可安全重发。'],
        ['管理端超时，随后收到 ACK 证据', '保留原超时结论并补充迟到确认，不追溯改写已返回结果，不自动恢复剩余队列。'],
      ] }, caution: '本帮助没有“执行命令”按钮；复制文档不会发送设备报文。不要用机组同步 TransmitSnapshot 的状态来解释采集命令 ACK。' },
    ], links: [{ to: '/commands', label: '查看命令记录与执行过程' }, { to: '/sources', label: '设置下级发送间隔' }],
  },
  {
    id: 'configuration', category: '日常操作', title: '配置编辑与安全启停',
    summary: '修改 → 校验 → 保存到下级 → 明确重启；点表地址告警与配置格式错误分开处理。',
    sections: [
      { title: '通过网页修改下级配置', steps: ['“下级 OPC”选中明确的物理来源，打开“运维 → 配置”。这里不随客户端 Session 选源改变目标。', '选文件；常用参数用表单、下拉框编辑。需要其他参数时选择分类或搜索；原文、导入、下载与备份在折叠区。', '点击“校验当前草稿”。配置语法、类型、范围、点表结构/表头/重复点/冲突等错误会阻止保存；CSV 点位字节或位地址越界保留逐行警告，但允许保存。', '核对警告后“保存到下级”。保存使用文件 revision 防覆盖；冲突时重新读取并合并，不强行覆盖。', '仅保存不会自动重启；按提示明确执行安全重启，然后核对当前配置与采集状态。'], caution: '点表越界允许保存不表示设备支持该长度，也不表示错误点有效。软件只提示规则，不替使用人修改 DataLen、地址或现场点表。' },
      { title: '启动、停止和重启分别在哪操作', table: { headings: ['目标', '日常入口', '影响范围'], rows: [
        ['采集程序', '管理网页 → 来源运维 → 进程；后端调用目标 Agent', '影响连接该采集来源的所有管理端和直连客户端，不只是当前网页。'],
        ['OPC 管理端', 'Semaphore → 设备详情 → OPC 管理端 → 服务操作', '断开目标管理端的上级客户端，不停止远端下级。'],
        ['Agent', 'systemd 守护；异常从 Semaphore 的维护入口恢复', '可能影响本机采集子进程，不由管理网页直接强杀。'],
        ['串口通信程序', '独立服务；以平台实际能力提示为准', '尚无采集事务排空能力；运行时不能套用采集 Agent 升级/停止流程。'],
      ] } },
      { title: '配置开关不是一回事', paragraphs: ['端口停用、站点停止读取、进程停止是不同状态；停止站点读取不应被当作禁止控制的安全锁。采集配置、CSV、计算配置分别在对应页面维护。', '安全启停先拒绝新命令并等待已接受事务结束；排空超时、占用或结果不明时停止流程。保留原作业，补查，不换编号重试，不删锁或维护状态。人为停止会持久保存，升级和守护重启不应擅自恢复。', '通信来源仅展示其声明支持的配置与运维能力；配置格式和字段以该程序为准，不能把采集 config.toml 直接套给通信程序。'] },
    ], links: [{ to: '/sources', label: '打开下级 OPC 与运维' }],
  },
  {
    id: 'calculations', category: '日常操作', title: '计算中心与状态输入',
    summary: '中央计算按组默认来源取数，所有 Session 共享结果；站点状态可作为显式公式输入。',
    sections: [
      { title: '导入、校验和激活', steps: ['打开“计算中心 → 规则配置”，从文件或指定下级导入 calc.toml，或读取现有草稿。', '编辑后保存草稿、校验；通过后明确激活。草稿修改不等于当前已生效。', '“实时结果”按 KKS、类型和质量筛选，展开依赖查看实际来源、输入和错误；可导出当前筛选结果。', '“版本历史”选择旧配置恢复为新版本，校验后激活。类型变化可能提示待重启，不能当作已经热生效。', '下级切为 manager 模式后保留本机 calc.toml 作回退副本，但不执行；模式变更需要明确重启。回退先恢复下级 local 并确认节点，再停用中央配置，避免业务节点消失。'] },
      { title: '质量异常时为什么结果不一定为 0', paragraphs: ['采集断线时，原始输入按 0 参加计算，结果沿实际执行路径传播通信中断质量。例如输入 0 加常数 10，结果可能是 10 且 BadNoCommunication，不是把全部计算结果再清零。', '纯常数不依赖离线来源；IF 未执行分支的坏质量不影响结果。站点诊断输入的值 5 表示已知离线，其 Good 质量有效，不按普通采集值清零。', 'Session 独立选源不触发共享计算切源。要改变跟随取数目标，需要变更对应组的全局默认来源；fixed 状态输入仍固定在指定物理来源。'] },
      { title: '把 58 的 WCH0 / 11 站状态发布给 OPC 客户端', paragraphs: ['推荐“状态输入 → 新增状态输入”，使用来源、串口、站点下拉框；保存草稿后随计算配置一起校验激活。名称允许数字开头，但不能纯数字、保留字或重名。', '激活后直接生成同名只读 UInt32 节点，不需要转发公式，也可继续在其他公式中引用。以下 rcd-58 是来源 ID；实际 Endpoint 以来源配置为准。'], examples: [{ title: '状态输入 TOML（无需 rules）', text: '[[state_inputs]]\nname = "9CYE91GH201_COM1"\nmode = "fixed"\n\n[[state_inputs.targets]]\nsource_id = "rcd-58"\nport_id = "/dev/ttyWCH0"\ndevice_id = "11"', note: '来源与站点须存在；跟随默认来源模式需为所有候选来源补齐映射。不要添加同名 rules 输出；已有不同名称的转发公式继续有效。页面保存会规范化 TOML 并移除注释，请先备份。' }], table: { headings: ['输入数值', '状态'], rows: communicationStates }, nodes: [{ name: 'ns=2;s=9CYE91GH201_COM1', type: 'UInt32', access: '只读', description: '状态输入激活后直接发布，支持读取和订阅；删除后本进程内节点标记 BadOutOfService。' }] },
    ], links: [{ to: '/calculations', label: '打开计算中心' }],
  },
  {
    id: 'communication', category: 'OPC 接入', title: '机组同步：接收与发送',
    summary: '使用文档原始 DATA.* 变量名；逐点写入与整包原子提交任选，默认 5 秒刷新、15 秒输入超时。',
    sections: [
      { title: '先确认角色和边界', paragraphs: ['以下点表是 46 的 1 号侧示例角色：接收 9→1，发送 1→9；组 unit-sync-46、来源 communicate-46。它不是查询轮询，而是双方按错开时序同步，碰撞处理遵循通信程序调度。实际角色和启用状态必须核对来源配置。', '业务 NodeId 为 ns=2;s= 加下面的完整原始变量名，不添加 rx、TX、机组或 KKS 前缀。当前部署约定串口保持停用；实时状态以运维页面为准。串口/发送停用时写入被拒绝，业务质量可为 BadOutOfService，不能擅自启用来消除提示。'] },
      { title: '接收点：只读，共 15 项', nodes: [
        { name: 'ns=2;s=DATA.Header.nBlock.z', type: 'UInt32', access: '只读', description: '有效帧身份 9；没有有效帧时不能用零值推断机组。' },
        { name: 'ns=2;s=DATA.AES.SGC85.State', type: 'String', access: '只读', description: '5 字节原始块，10 个十六进制字符，不套用 Fire 状态。' },
        { name: 'ns=2;s=DATA.AES.GMM1', type: 'String', access: '只读', description: '35 字节，70 个十六进制字符。' },
        { name: 'ns=2;s=DATA.PumpStationCtrl[0]', type: 'String', access: '只读', description: '8 字节，16 字符；[0] 属于名称，IndexRange 留空。' },
        ...receiveFireNames.map((name) => ({ name: `ns=2;s=${name}`, type: 'UInt32', access: '只读', description: '楼宇 Fire 状态，编号见下表。' })),
      ] },
      { title: '发送输入：读写，共 39 项', nodes: [
        { name: 'ns=2;s=DATA.Header.detectorTemp3', type: 'UInt32', access: '读写', description: '0–255，原始温度字节，不自行换算。' },
        { name: 'ns=2;s=DATA.Header.detectorTemp6', type: 'UInt32', access: '读写', description: '0–255，原始温度字节。' },
        { name: 'ns=2;s=DATA.AKONs.1CYE18GH204.HW', type: 'String', access: '读写', description: '969 字节，严格 1938 个十六进制字符。' },
        { name: 'ns=2;s=DATA.PumpStationCtrl', type: 'String', access: '读写', description: '8 字节，16 字符；与接收的 [0] 名称不同。' },
        ...transmitFireNames.map((name) => ({ name: `ns=2;s=${name}`, type: 'UInt32', access: '读写', description: '仅允许 1–8；不能传 String 或 Double。' })),
      ], paragraphs: ['原始块 String 只含 0–9、A–F/a–f，不能带空格、0x、分隔符或省略前导零，不是 ByteString。发送身份由程序填写，不把 DATA.Header.nBlock.z 放进发送输入。'] },
      { title: 'Fire 状态编号', table: { headings: ['UInt32', '含义', '原始五字节（十六进制）'], rows: [
        ['0', '未知编码，仅接收可能出现，不能发送', '无固定对应'], ['1', '正常', '0A 85 00 06 00'],
        ['2', '红色报警', '01 88 00 07 00'], ['3', '红色报警闪', '01 C8 00 07 00'],
        ['4', '黄色报警', '05 86 00 0C 00'], ['5', '黄色报警闪', '05 A6 00 0C 00'],
        ['6', '故障报警', '06 82 00 12 00'], ['7', '故障报警闪', '06 C2 00 12 00'], ['8', '闭锁', '09 85 00 04 00'],
      ] }, paragraphs: ['编号只适用于 DATA.Buildings.*.Fire，不适用于 HW、AES、GMM、泵站原始块。某些故障共用相同五字节，协议不能区分具体原因。Good/0 表示未知编码；非 Good/0 不能当成收到有效状态。'] },
      { title: '逐点写入步骤', steps: ['同一 Session 核对选源为 communicate-46，读取 TransmitStatus，确认 input_mode=individual 和当前刷新/超时配置。', '首次写齐全部 39 个有效输入，每项检查节点级 Write 结果。只写一个 Fire 不会形成完整发送包。', '没有变化时默认每 5 秒刷新全部输入；有变化立即写变化项，但其他未变输入也必须周期续期。任何必需输入超过默认 15 秒未成功刷新，整包停发。', '一次 Write 携带 39 个节点仍非原子事务；有跨字段一致性要求请用下一节整包提交。Read/Subscribe 不续期。'], examples: [{ title: '单点写入格式示例', text: 'NodeId: ns=2;s=DATA.Buildings.11UBE.Fire\nAttribute: Value\nValueType: UInt32\nValue: 2\nIndexRange: 留空', note: '2 代表红色报警。这是格式说明，不是批准发送的业务数据，其他 38 项仍须齐全且新鲜。' }] },
      { title: '推荐：整包原子提交', nodes: [
        { name: 'ns=2;s=TransmitStatus', type: 'String（JSON）', access: '只读', description: '读取 revision、input_mode、snapshot_fresh、stale_inputs、complete_writes、last_send_error。' },
        { name: 'ns=2;s=TransmitSnapshot', type: 'String（JSON）', access: '读写', description: '完整 fields 和该来源当前 revision；管理端按整包转发。' },
        { name: 'ns=2;s=InputRefreshIntervalMs', type: 'UInt32', access: '只读', description: '当前输入刷新要求，默认 5000 ms；通过通信配置修改，不在此节点写。' },
        { name: 'ns=2;s=InputTimeoutMs', type: 'UInt32', access: '只读', description: '当前输入超时，默认 15000 ms。' },
      ], steps: ['从同一目标来源的 TransmitStatus JSON 读取 revision；这不是组选源 SelectionRevision。', '把下面示例的 revision 和全部 39 项替换成当前有效业务数据，将整个 JSON 作为一个 OPC String 写入 TransmitSnapshot。', '检查该节点 Write 结果。Good 后 revision 加 1；默认每 5 秒重提完整快照，有变化立即提交完整新快照。', '状态经过管理端订阅缓存可能短暂滞后；等待状态推进。冲突或回复不明时先核对原结果，不用旧 revision 反复重投。'], caution: '整包模式与逐点模式不要混写；按 TransmitStatus.input_mode 和来源能力使用。生产环境建议一个物理发送来源只由一个数据生产客户端写入。切源不会撤回旧来源已接受的快照，也不能当成停止旧来源发送。', examples: [{ title: '完整 39 项 JSON 格式示例', text: atomicExample, note: 'revision=0、温度、全零原始块及 Fire=1 都是演示值，不可直接当成现场业务数据。必须替换当前 revision 和所有实际值。HW 为 969 字节，未省略字段。' }] },
      { title: '如何确认发送状态', paragraphs: ['Write Good 仅表示输入/快照被接受。snapshot_fresh 表示完整且新鲜，不证明串口写出；complete_writes 增加表示本进程完整写出一帧，不证明对方接收或处理。', '本同步协议没有逐包接收 ACK，不等待采集程序的 00 50 50，也不把每次输入写入放到采集命令 FIFO。同步传输定时发送当前快照，不保证逐个发送每个中间值。'] },
    ], links: [{ to: '/sources', label: '查看通信来源与配置' }, { to: '/points', label: '浏览通信点位' }],
  },
  {
    id: 'versions-logs', category: '日常操作', title: '软件版本、设备版本与日志',
    summary: '软件运行版本、硬件模块版本、业务日志和命令审计各有不同入口。',
    sections: [
      { title: '要看哪一种版本', table: { headings: ['信息', '入口', '注意'], rows: [
        ['当前管理端、下级、Agent 软件', '系统信息 → 软件版本；全主机软件在 Semaphore', '看实际运行证据、观测时间及离线缓存，不把磁盘文件版本当成运行版本。'],
        ['通信模块与 PLC 版本', '设备版本', '属于来源+串口+站点，不随 Session 选源；离线或查询失败保留上次值并显示过期。'],
      ] } },
      { title: '模块版本查询不是纯读取', paragraphs: ['站点类型可选 unknown、domestic、legacy_russian。同一串口可混用；未知和俄供不主动发送版本请求。国产站点可按规则自动查询一次或从 Web 手动刷新，查询会发送真实串口报文。', '配置国产但实测全 FF 会显示类型不一致，不自动改配置。硬件超时、坏帧不应被当成俄供；失败保留上次成功版本并标记过期。执行查询前确认实际类型和现场许可。'] },
      { title: '日志在哪看、怎样调整等级', steps: ['“下级日志”选来源、等级、时间和关键词，确认时间范围与分页；推送日志只保存在接收它的管理端，不保证三台历史相同。', '来源运维中的进程输出、下级操作日志、管理端业务日志和平台任务日志是不同证据。看不到某种日志不等于程序没有运行。', '修改下级日志等级在来源配置中进行，校验保存后按提示明确重启；DEBUG 会增加磁盘和推送量。', '控制命令问题优先看“命令记录 → 执行过程”，独立审计不依赖普通日志上传。审计缺口显示未知，不伪造成功或发送 0 次。'], caution: '不要为排障删除锁、数据库、维护状态或原始日志。复制日志给他人前移除凭据等秘密。' },
    ], links: [{ to: '/settings', label: '查看软件版本' }, { to: '/device-versions', label: '查看设备版本' }, { to: '/lower-logs', label: '查看下级日志' }, { to: '/operations', label: '查看告警与审计' }],
  },
  {
    id: 'platform', category: '软件维护升级', title: '软件维护、升级与恢复',
    summary: '日常发布只去现有 Semaphore，不在 OPC 帮助页另建发布入口或执行队列。',
    sections: [
      { title: '上传一个新版本', steps: ['通过“系统信息 → 机器台账”的平台链接打开 Semaphore。沿用平台已有登录，和 OPC 管理网页是两套登录。', '“软件包 → 上传软件包”选择构建工具生成的单组件 *.rcd.tar.gz，等待组件、架构、版本及摘要校验；上传不会部署。', '在设备或软件包页点击升级，选择软件版本与兼容主机，核对当前版本、目标版本和受影响程序。', '确认本次控制暂停及维护范围后提交一次；多机按 30 → 58 → 46 串行，失败或结果未知不继续。', '在“操作任务”查看阶段与原生日志；完成后检查实际运行映像与版本。任务 Success 不等于所有物理站点通信正常。'], paragraphs: ['包只带单个程序与公开发布清单，不包含现场配置、数据库、日志、PKI、秘密或任意安装脚本。升级一个组件不能替换其他组件。串口通信程序运行时不支持假安全排空升级；以平台禁用原因处理。'] },
      { title: '新增主机或软件种类', steps: ['已有组件的新版本直接上传；其他软件先“软件包 → 添加软件种类”登记固定运行定义。首版只接受 Linux 单 ELF 程序，不支持任意脚本、容器或 Windows 安装器。', '空白主机须有 Linux、Python3、systemd、SSH 和非 root 服务账户；首次安装需要受控提权。先在高级管理保存原生加密凭据。', '“设备与版本 → 添加主机”，填写固定地址、账户并下拉选择凭据；从可信渠道核对完整主机密钥指纹后确认。', '只读预检成功后登记主机，再选兼容包明确首次安装。首次安装拒绝覆盖已有程序/服务/数据，安装后保持停止、待配置，不自动启用串口。'], caution: '已有主机的未知状态不能当成“未安装”。不要为接入关闭主机指纹校验、放宽 KYSEC 或覆盖现场配置。' },
      { title: '恢复与未知结果', steps: ['从“设备详情 → 组件 → 版本历史”选择有证据且兼容的成功版本，确认重启影响。恢复是新任务，不改写原历史。', '默认只恢复程序，保留当前配置、数据库和维护身份。不兼容或缺少包的版本不能一键恢复。', 'SSH/HTTP 回复丢失后查询原操作和原任务，不重新提交；接入与首装记录也可找回原作业。结果未知保留占用。', '能够证明安全时才自动恢复；进程不明、磁盘或 KYSEC 拒绝、恢复失败交维护人员处理，不反复重启或删除状态文件。'] },
      { title: '主机资源与检查结果', paragraphs: ['Semaphore 的设备列表/详情显示 CPU、内存、磁盘和网络快照，每分钟检查。刷新页面只读已有结果，“检查状态”才创建新只读任务；超过 180 秒或连接失败显示历史时间，不伪装实时在线。', '网卡 up 只说明链路状态，不证明 OPC 或串口设备正常。管理端自身资源在“系统信息”查看；软件检查与物理设备故障分别处理。'] },
      { title: '安全启停与平台自身维护', paragraphs: ['在“设备与版本 → 查看详情”找到 OPC 管理端组件，再选择启动、停止或重启。不需要先选择软件包；确认维护范围后进入原生任务，先排空已接受命令再停止，结果不明不自动强杀。', '采集程序的普通启停请打开 OPC 管理网页，由目标 Agent 执行；Agent 升级则由平台独立协调，可能影响其采集子进程。人为停止的程序升级后仍应保持停止。', 'Semaphore 自身、加密配置及受控 Ansible/发布脚本由维护人员成套更新，不能把平台程序当成 OPC 管理端组件上传。先备份平台数据库、加密配置、包目录和原二进制，等待已有任务结束，再在授权窗口更新与核对。只重启平台不应停止业务软件；平台版本恢复必须使用配套的数据兼容检查，不能只换旧二进制。'], caution: '截图中若显示“需要先获得近期成功的软件状态检查”，先查原任务/日志；不要把历史快照伪装实时，也不要为解除禁用删除锁或更改维护代次。' },
    ], links: [{ to: '/settings', label: '查看平台链接与台账' }],
  },
  {
    id: 'troubleshooting', category: '日常操作', title: '常见问题：从哪一层查起',
    summary: '先区分连接、选源、串口、配置和执行结果；不知道结果时不重复发送。',
    sections: [
      { title: '按现象定位', table: { headings: ['现象', '先检查', '不要这样处理'], rows: [
        ['OPC 客户端读不到值', '4840 连通、Session、命名空间、完整 NodeId、质量；再看所选来源与缺点', '不凭变量名相同就判定数据源相同。'],
        ['不同客户端数据不同', '是否同一管理端；各自 Session 选源、组自动参与及中央计算默认来源', '不把一个客户端选源当成全局切换。'],
        ['连续命令无反馈', '命令记录：入队/派发、队列期限、端口占用、间隔、ACK 与审计完整性', '不把已受理当成动作完成，不自动重发未知命令。'],
        ['配置保存被阻止', '看阻断错误而非点位越界警告；核对文件类型、结构、范围和 revision', '不为通过校验猜改 DataLen 或站点地址。'],
        ['同步通信没有发送', '启用状态、角色、39 个输入是否齐全且新鲜、input_mode、revision、complete_writes', '不只写变化项；不等待采集 ACK；不擅自启用串口。'],
        ['下级 OPC 在线，站点状态异常', 'CommunicationState 值、串口打开状态、读取开关及该站通信；其他站是否同时异常', '不把 OPC 在线等同于全部物理站在线。'],
        ['Agent 不可达 / 作业未知', '固定目标、Agent 连接、原作业和维护占用；需要时由平台恢复 Agent', '不换编号绕锁、不删除事务，不反复重启。'],
      ] } },
      { title: '报告问题至少带这些信息', paragraphs: ['管理端地址、时间范围、来源稳定 ID、串口/站点、完整 NodeId、值与质量码、操作/命令编号及截图。提供原始错误文字，区分复现步骤与猜测；不要附账号密码。', '帮助文档讲接口和步骤，不反映实时健康，也不会自动执行任何复制的示例。实际功能是否可用，以当前来源能力声明、页面禁用原因和现场维护许可为准。'] },
    ], links: [{ to: '/commands', label: '命令记录' }, { to: '/operations', label: '告警与审计' }, { to: '/sources', label: '来源与运维状态' }],
  },
] as const satisfies readonly HelpTopic[];

export type HelpTopicId = typeof helpTopics[number]['id'];
export const helpCategories = [...new Set(helpTopics.map((topic) => topic.category))];

// JSON indexing includes labels, full NodeIds and examples, but never renders
// user-entered search as HTML or a regular expression.
const searchIndex = new Map(helpTopics.map((topic) => [topic.id, JSON.stringify(topic).toLocaleLowerCase()]));
export function findHelpTopics(query: string, category: string = ''): readonly HelpTopic[] {
  const terms = query.trim().toLocaleLowerCase().split(/\s+/).filter(Boolean);
  return helpTopics.filter((topic) => (!category || topic.category === category)
    && terms.every((term) => searchIndex.get(topic.id)?.includes(term)));
}
