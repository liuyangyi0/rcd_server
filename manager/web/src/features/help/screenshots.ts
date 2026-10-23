// These are unmodified screenshots of the deployed 46 UI, not mockups.
// Capture evidence and original SHA-256 values remain in the remote manifests.
export interface HelpScreenshot { id: string; caption: string; capturedAt: string; points: string[] }
const managerAt = '2026-10-23T06:50:19Z';
const platformAt = '2026-10-23T06:52:15Z';

export const helpScreenshots: Record<string, HelpScreenshot[]> = {
  'relationships:0': [{ id: 'platform-devices', caption: '现有运维平台：设备与版本', capturedAt: platformAt,
    points: ['这里管理主机、软件包和发布，不管理串口点表与计算公式。', '一台机器有多个组件；查看详情可分别核对版本和维护操作。'] }],
  'connect:0': [{ id: 'manager-points', caption: '现有管理网页：点位浏览', capturedAt: managerAt,
    points: ['选择逻辑组与查看方式，核对完整节点名、类型、数值和质量。', '这是管理网页截图，不是某个第三方 OPC 客户端的操作画面；客户端连接步骤见上文。'] }],
  'selection:0': [{ id: 'manager-groups', caption: '分组与选源：当前 Session 和组默认来源分开显示', capturedAt: managerAt,
    points: ['先在顶部下拉框选需要查看的 Session。', '“此 Session 选中源”不是“新 Session 默认源”；行内切源只影响对应 Session/组。', '自动切源 / 差异告警是组策略入口，不是自动启用。截图中的数据和在途数量仅为当时状态。'] }],
  'commands:0': [{ id: 'manager-commands', caption: '命令记录：队列结果与执行过程', capturedAt: managerAt,
    points: ['用来源、端口、时间等定位原命令；不要因没有看到反馈而再次发送。', '请求 / ACK 下方是调用发送和完整写出次数；未知不能当成零次。', '点击单条“执行过程”查看关联事件。截图中的历史成功不能证明新命令成功。'] }],
  'configuration:0': [
    { id: 'manager-sources', caption: '第 1 步：在下级 OPC 列表核对来源，打开“运维”', capturedAt: managerAt,
      points: ['先看稳定来源 ID 和 Endpoint，确认修改的是哪台机器。', '点击该行“运维”，不是“重连并发现”，也不是编辑来源地址。'] },
    { id: 'manager-configuration', caption: '第 2 步：配置标签，选文件后修改、校验、保存', capturedAt: managerAt,
      points: ['在“配置文件”下拉框选 config.toml 或指定 CSV。', '常用参数直接填写；更多项通过分类和搜索查找，不必展开原文。', '上方“校验当前草稿”不会写文件；校验通过后才“保存到下级”。未修改时保存按钮禁用是正常现象。', '保存不自动重启，流程提示中的第 4 步需要明确操作。'] },
  ],
  'configuration:1': [{ id: 'manager-process', caption: '采集进程：管理端通过目标 Agent 安全启停', capturedAt: managerAt,
    points: ['核对当前目标来源和 Agent 身份。', '进程停止与串口/站点停用不是同一件事；存在作业时查询原结果，不能重复点重启。'] }],
  'calculations:0': [
    { id: 'manager-calculations', caption: '计算中心：实时结果、质量与依赖', capturedAt: managerAt,
      points: ['顶部说明给出默认来源取数规则；它不随客户端 Session 独立选源变化。', '实时结果可搜索、按质量筛选、展开依赖；截图中的通信中断是当时数据状态，不是本次说明触发。'] },
    { id: 'manager-calculation-config', caption: '计算中心：规则配置与草稿', capturedAt: managerAt,
      points: ['导入或编辑只改变草稿；保存草稿、校验、激活必须区分。', '回滚计算规则去“版本历史”；软件二进制恢复则去 Semaphore，二者不能混淆。'] },
  ],
  'versions-logs:0': [{ id: 'manager-device-versions', caption: '设备版本：通信模块和 PLC 的版本', capturedAt: managerAt,
    points: ['这里是物理站点的模块版本，不是采集程序、管理端或 Agent 的软件版本。', '“已过期”仍可显示上次成功值；不要把旧值当成当前查询成功。', '“查询版本”会发送串口请求，截图中没有执行该动作。'] }],
  'versions-logs:2': [{ id: 'manager-logs', caption: '下级集中日志：选择来源和时间范围', capturedAt: managerAt,
    points: ['先确认来源，再选择等级、时间及关键词。', '这是当前管理端接收到的日志，不保证另外两台管理端保有相同历史。'] }],
  'platform:0': [
    { id: 'platform-packages', caption: '升级第 1 步：打开现有平台“软件包”', capturedAt: platformAt,
      points: ['右上角“上传软件包”上传已有组件的新版本。', '“添加软件种类”只用于接入另一种程序，不用于每次更新版本。'] },
    { id: 'platform-upload', caption: '升级第 2 步：选择标准发布包并校验', capturedAt: platformAt,
      points: ['选择构建人员给出的 .rcd.tar.gz，再点“上传并校验”。', '上传不会自动部署。本说明截图未选择文件、未上传软件。'] },
    { id: 'platform-upgrade', caption: '升级第 3 步：选择软件 → 选择机器 → 确认执行', capturedAt: platformAt,
      points: ['组件和版本使用下拉框；下一步选择兼容机器。', '最终确认维护范围和本次控制暂停后才提交；说明截图停在向导内，没有执行升级。'] },
    { id: 'platform-tasks', caption: '升级第 4 步：在“操作任务”查看结果与原生日志', capturedAt: platformAt,
      points: ['按原操作查看阶段、每台结果及关联任务日志。', '失败或结果未知先核对原作业，不重复提交；成功后另核对实际运行版本。'] },
  ],
  'platform:1': [{ id: 'platform-add-host', caption: '添加主机：原生凭据、固定地址和主机身份', capturedAt: platformAt,
    points: ['从下拉框选择保存好的加密凭据，不把密码填进发布包。', '先核对主机密钥指纹并检查接入；登记成功不代表已安装、已配置或已启用串口。'] }],
  'platform:2': [{ id: 'platform-history', caption: '设备详情 → OPC 管理端 → 版本历史', capturedAt: '2026-10-23T07:00:36Z',
    points: ['点击目标组件下的“版本历史”，不是下级配置文件的自动备份。', '截图时状态检查已过期，恢复与启停被禁用；应先核对原检查结果，取得新鲜可信状态，再按兼容性选择版本。', '没有可恢复选项时不可绕过。此次未打开可提交的恢复确认页，更没有执行恢复。'] }],
  'platform:3': [{ id: 'platform-device-detail', caption: '设备详情：CPU、内存、磁盘、网络与确认时间', capturedAt: platformAt,
    points: ['指标带观测时间，是巡检快照，不是实时曲线。', '网卡 up 不证明 OPC 或物理站点正常；查看“最后确认”和组件运行身份。'] }],
  'platform:4': [{ id: 'platform-service-control', caption: '平台中的 OPC 管理端组件：安全启动、停止、重启', capturedAt: '2026-10-23T07:00:36Z',
    points: ['只操作此机器的管理端，不连带停止本机 Agent、采集或其他管理端。', '按钮禁用时先看下方原因；截图中缺少近期成功检查，不表示程序已停止。', '此处不是软件升级，不替换程序文件。采集的日常启停仍在 OPC 管理网页通过 Agent 执行。'] }],
};
