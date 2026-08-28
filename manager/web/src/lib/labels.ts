const STATE_LABELS: Record<string, string> = {
  connected: '已连接', disconnected: '已断开', connecting: '连接中',
  active: '已激活', inactive: '未激活', creating: '创建中',
  running: '运行中', stopped: '已停止', closed: '已关闭', enabled: '已启用', disabled: '已停用', unknown: '未知',
  ready: '就绪', cached: '缓存目录', discovering: '发现中', catalogconflict: '目录冲突',
  waiting: '等待响应', received: '已接收', failed: '失败', timedout: '超时',
  open: '未恢复', resolved: '已恢复', warning: '警告', critical: '严重',
};

const STATUS_LABELS: Record<string, string> = {
  Good: '正常',
  BadNoCommunication: '通信中断',
  BadWaitingForInitialData: '等待首值',
  BadConfigurationError: '配置错误',
  BadOutOfService: '已停用',
  BadWaitingForResponse: '等待命令响应',
  BadTimeout: '超时',
};

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
  return STATUS_LABELS[value] ? `${STATUS_LABELS[value]}（${value}）` : value;
}

export function kindLabel(value: string): string {
  return KIND_LABELS[value] ?? value;
}

export function probeStageLabel(value: string): string {
  return PROBE_LABELS[value] ?? value;
}
