const STATE_LABELS: Record<string, string> = {
  connected: '已连接', disconnected: '已断开', connecting: '连接中',
  active: '已激活', inactive: '未激活', creating: '创建中',
  running: '运行中', stopped: '已停止', closed: '已关闭', enabled: '已启用', disabled: '已停用', unknown: '未知',
  ready: '就绪', cached: '缓存目录', discovering: '发现中', catalogconflict: '目录冲突',
  queued: '排队待发送', waiting: '等待响应', received: '成功', failed: '失败', timedout: '超时',
  success: '成功', pending: '处理中', manager: '管理端计算', local: '下级本地计算', unsupported: '不支持', offline: '离线',
  open: '未恢复', resolved: '已恢复', warning: '警告', critical: '严重',
};

const STATUS_LABELS: Record<string, string> = {
  Good: '正常',
  BadUnexpectedError: '执行失败',
  BadNoCommunication: '通信中断',
  BadWaitingForInitialData: '等待首值',
  BadConfigurationError: '配置错误',
  BadOutOfService: '已停用',
  BadWaitingForResponse: '等待命令响应',
  BadTimeout: '超时',
};

const STATUS_NAMES_BY_BASE = new Map<number, string>([
  [0x00000000, 'Good'],
  [0x80010000, 'BadUnexpectedError'],
  [0x80310000, 'BadNoCommunication'],
  [0x80320000, 'BadWaitingForInitialData'],
  [0x80890000, 'BadConfigurationError'],
  [0x808d0000, 'BadOutOfService'],
  [0x80b20000, 'BadWaitingForResponse'],
  [0x800a0000, 'BadTimeout'],
]);

const KIND_LABELS: Record<string, string> = {
  connection: '连接状态', session: '下级会话', subscription: '订阅状态', catalog: '目录状态', error: '错误信息',
  'source.connection': '下级连接异常', 'source.catalog': '目录冲突', 'source.subscription': '订阅异常',
  'group.create': '创建分组', 'group.update': '修改分组', 'group.delete': '删除分组',
  'group.default_source': '修改默认源', 'source.create': '创建来源', 'source.update': '修改来源',
  'source.delete': '删除来源', 'source.probe': '测试来源', 'source.reconnect': '立即重连',
  'source.rediscover': '重新发现目录', 'session.selection': '修改会话选源',
  'group.redundancy': '修改自动切源与差异策略', 'session.automatic': '修改会话自动参与',
  'session.automatic_switch': '会话自动切源', 'group.automatic_switch': '默认源自动切换',
  'alarm.acknowledge': '确认告警', 'alarm.silence': '静默告警', 'configuration.import': '导入配置',
  'source.operation.requested': '下级操作已请求', 'source.operation.completed': '下级操作已完成',
};

const PROBE_LABELS: Record<string, string> = {
  DiscoverEndpoints: '发现服务端 Endpoint',
  ConnectAndActivateSession: '连接并激活 Session',
  ResolveNamespaceArray: '解析 NamespaceArray',
  BrowseAndValidateCatalog: '浏览并校验点表',
};

export function stateLabel(value: string): string {
  return STATE_LABELS[value.toLowerCase()] ?? value;
}

export function statusCodeLabel(value: string): string {
  if (STATUS_LABELS[value]) return `${STATUS_LABELS[value]}（${value}）`;
  if (!/^0x[0-9a-f]{8}$/i.test(value)) return value;

  const code = Number.parseInt(value.slice(2), 16) >>> 0;
  const base = (code & 0xffff0000) >>> 0;
  const name = STATUS_NAMES_BY_BASE.get(base);
  const severity = (code & 0xc0000000) >>> 0;
  const severityName = severity === 0 ? 'Good' : severity === 0x40000000 ? 'Uncertain' : severity === 0x80000000 ? 'Bad' : '保留状态';
  const label = name ? STATUS_LABELS[name] : severity === 0 ? '正常' : severity === 0x40000000 ? '质量不确定' : '异常';
  const detail = [name ?? severityName];
  if ((code & 0x80) !== 0) detail.push('通知队列溢出');
  return `${label}（${detail.join('，')} · ${statusCodeHex(code)}）`;
}

export function statusCodeHex(value: number): string {
  return `0x${(value >>> 0).toString(16).padStart(8, '0').toUpperCase()}`;
}

export function kindLabel(value: string): string {
  return KIND_LABELS[value] ?? value;
}

export function probeStageLabel(value: string): string {
  return PROBE_LABELS[value] ?? value;
}

export function operationLabel(value: string): string {
  const labels: Record<string, string> = {
    Write: '变量写入', Call: '方法调用', query_device_version: '查询设备版本',
    set_device_module_type: '设置模块类型', set_logging_level: '调整日志等级',
    set_port_enabled: '切换串口', set_device_read_enabled: '切换站点采集',
    set_calculation_mode: '切换计算模式', restart: '重启下级',
  };
  return labels[value] ?? kindLabel(value);
}

export function humanizeTechnicalMessage(value?: string): string {
  if (!value) return '—';
  const substitutions: Array<[RegExp, string]> = [
    [/station version query timed out after (\d+) ms/i, '设备版本查询在 $1 ms 后超时'],
    [/frame header mismatch/i, '返回帧头不匹配'],
    [/checksum mismatch/i, '返回帧校验失败'],
    [/connection refused/i, '连接被拒绝'],
    [/context deadline exceeded/i, '操作超时'],
  ];
  for (const [pattern, replacement] of substitutions) {
    if (pattern.test(value)) return value.replace(pattern, replacement);
  }
  return value;
}
