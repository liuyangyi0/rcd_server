const STATE_LABELS: Record<string, string> = {
  connected: '已连接', disconnected: '已断开', connecting: '连接中',
  active: '已激活', inactive: '未激活', creating: '创建中',
  running: '运行中', stopped: '已停止', closed: '已关闭', enabled: '已启用', disabled: '已停用', unknown: '未知',
  ready: '就绪', cached: '缓存目录', discovering: '发现中', catalogconflict: '目录冲突',
  waiting: '等待响应', received: '成功', failed: '失败', timedout: '超时',
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
  'alarm.acknowledge': '确认告警', 'alarm.silence': '静默告警', 'configuration.import': '导入配置',
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
