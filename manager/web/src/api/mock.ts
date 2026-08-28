import type { Alarm, CommandAudit, ConfigurationDocument, Group, ManagementAudit, Overview, Point, Source, SourceEvent, SystemInfo, UaSession } from './types';

const groups: Group[] = [
  { id: 'fire-panel-a', name: 'fire-panel-a', defaultSourceId: 'rcd-a-01', selectedSourceId: 'rcd-a-01', selectionRevision: 6, sourceCount: 3, goodPoints: 10609, badPoints: 24, totalPoints: 10633, pendingCommandCount: 0, lastSelectedAt: new Date(Date.now() - 120000).toISOString(), status: 'bad' },
  { id: 'fire-panel-b', name: 'fire-panel-b', defaultSourceId: 'rcd-b-01', selectedSourceId: 'rcd-b-01', selectionRevision: 1, sourceCount: 1, goodPoints: 5311, badPoints: 0, totalPoints: 5311, pendingCommandCount: 0, lastSelectedAt: new Date(Date.now() - 604800000).toISOString(), status: 'good' },
  { id: 'pump-station-1', name: 'pump-station-1', defaultSourceId: 'rcd-p-01', selectedSourceId: 'rcd-p-01', selectionRevision: 2, sourceCount: 1, goodPoints: 8864, badPoints: 0, totalPoints: 8864, pendingCommandCount: 0, lastSelectedAt: new Date(Date.now() - 1036800000).toISOString(), status: 'good' },
  { id: 'ahu-system', name: 'ahu-system', defaultSourceId: 'rcd-h-02', selectedSourceId: 'rcd-h-02', selectionRevision: 3, sourceCount: 2, goodPoints: 3102, badPoints: 16, totalPoints: 3118, pendingCommandCount: 0, lastSelectedAt: new Date(Date.now() - 259200000).toISOString(), status: 'good' },
];

const sources: Source[] = [
  { id: 'rcd-a-01', name: 'rcd-a-01', groupId: 'fire-panel-a', endpoint: 'opc.tcp://10.10.1.21:4840/', securityPolicy: 'None', securityMode: 'None', hasPassword: false, enabled: true, connectionState: 'Connected', sessionState: 'Active', subscriptionState: 'Running', catalogState: 'Ready', lastDataAt: new Date(Date.now() - 120000).toISOString(), lastError: 'BadNoCommunication', reconnectCount: 12, monitoredCount: 12, goodPoints: 10609, badPoints: 24, totalPoints: 10633, revision: 4 },
  { id: 'rcd-a-02', name: 'rcd-a-02', groupId: 'fire-panel-a', endpoint: 'opc.tcp://10.10.1.22:4840/', securityPolicy: 'None', securityMode: 'None', hasPassword: false, enabled: true, connectionState: 'Connected', sessionState: 'Active', subscriptionState: 'Running', catalogState: 'Ready', lastDataAt: new Date().toISOString(), reconnectCount: 0, monitoredCount: 12, goodPoints: 5311, badPoints: 0, totalPoints: 5311, revision: 2 },
  { id: 'rcd-a-03', name: 'rcd-a-03', groupId: 'fire-panel-a', endpoint: 'opc.tcp://10.10.1.23:4840/', securityPolicy: 'None', securityMode: 'None', hasPassword: false, enabled: true, connectionState: 'Disconnected', sessionState: 'Closed', subscriptionState: 'Stopped', catalogState: 'Cached', lastError: '连接超时', reconnectCount: 18, monitoredCount: 0, goodPoints: 0, badPoints: 5311, totalPoints: 5311, revision: 3 },
  { id: 'rcd-b-01', name: 'rcd-b-01', groupId: 'fire-panel-b', endpoint: 'opc.tcp://10.10.2.21:4840/', securityPolicy: 'None', securityMode: 'None', hasPassword: false, enabled: true, connectionState: 'Connected', sessionState: 'Active', subscriptionState: 'Running', catalogState: 'Ready', lastDataAt: new Date(Date.now() - 2000).toISOString(), reconnectCount: 1, monitoredCount: 8, goodPoints: 5311, badPoints: 0, totalPoints: 5311, revision: 1 },
  { id: 'rcd-p-01', name: 'rcd-p-01', groupId: 'pump-station-1', endpoint: 'opc.tcp://10.10.3.21:4840/', securityPolicy: 'None', securityMode: 'None', hasPassword: false, enabled: true, connectionState: 'Connected', sessionState: 'Active', subscriptionState: 'Running', catalogState: 'Ready', lastDataAt: new Date().toISOString(), reconnectCount: 0, monitoredCount: 16, goodPoints: 8864, badPoints: 0, totalPoints: 8864, revision: 1 },
  { id: 'rcd-h-02', name: 'rcd-h-02', groupId: 'ahu-system', endpoint: 'opc.tcp://10.10.4.22:4840/', securityPolicy: 'None', securityMode: 'None', hasPassword: false, enabled: true, connectionState: 'Connected', sessionState: 'Active', subscriptionState: 'Running', catalogState: 'Ready', lastDataAt: new Date().toISOString(), reconnectCount: 2, monitoredCount: 10, goodPoints: 3102, badPoints: 16, totalPoints: 3118, revision: 2 },
];

const sessions: UaSession[] = [
  { id: 'session-b0f1', clientName: 'SCADA-01', remoteAddress: '10.20.0.31:54212', connectedAt: new Date(Date.now() - 86400000).toISOString(), lastSeenAt: new Date().toISOString(), selectionRevision: 4, selectedSources: { 'fire-panel-a': 'rcd-a-01', 'fire-panel-b': 'rcd-b-01', 'pump-station-1': 'rcd-p-01', 'ahu-system': 'rcd-h-02' }, routeRevisions: { 'fire-panel-a': 6, 'fire-panel-b': 1, 'pump-station-1': 2, 'ahu-system': 3 } },
  { id: 'session-a14c', clientName: 'Historian', remoteAddress: '10.20.0.42:60901', connectedAt: new Date(Date.now() - 7200000).toISOString(), lastSeenAt: new Date().toISOString(), selectionRevision: 2, selectedSources: { 'fire-panel-a': 'rcd-a-02', 'fire-panel-b': 'rcd-b-01', 'pump-station-1': 'rcd-p-01', 'ahu-system': 'rcd-h-02' }, routeRevisions: { 'fire-panel-a': 2, 'fire-panel-b': 1, 'pump-station-1': 1, 'ahu-system': 1 } },
];

const points: Point[] = Array.from({ length: 126 }, (_, index) => ({
  nodeId: `KKS-${String(index + 1).padStart(5, '0')}`,
  browseName: `KKS-${String(index + 1).padStart(5, '0')}`,
	nodeClass: 'Variable',
  groupId: groups[index % groups.length].id,
  selectedSourceId: groups[index % groups.length].defaultSourceId,
  dataType: index % 7 === 0 ? 'Boolean' : index % 3 === 0 ? 'Double' : 'UInt32',
  value: index % 7 === 0 ? index % 2 === 0 : Number((index * 1.73).toFixed(2)),
  statusCode: index % 23 === 0 ? 'BadNoCommunication' : 'Good',
  sourceTimestamp: new Date(Date.now() - index * 1000).toISOString(),
  sourcePresent: true,
  deleted: false,
}));

const commands: CommandAudit[] = Array.from({ length: 18 }, (_, index) => ({
  id: `cmd-${1000 + index}`,
  createdAt: new Date(Date.now() - index * 3600000).toISOString(),
  completedAt: new Date(Date.now() - index * 3600000 + 420).toISOString(),
  sessionId: sessions[index % sessions.length].id,
  groupId: groups[index % groups.length].id,
  sourceId: groups[index % groups.length].defaultSourceId,
  port: `COM${(index % 6) + 1}`,
  operation: index % 2 === 0 ? 'Write _Send' : 'Call _SendCommand',
  result: index % 9 === 0 ? 'Failed' : 'Succeeded',
  statusCode: index % 9 === 0 ? 'BadTimeout' : 'Good',
  durationMs: 300 + index * 17,
  error: index % 9 === 0 ? 'BadTimeout' : undefined,
}));

const alarms: Alarm[] = [{ id: 1, sourceId: 'rcd-a-03', kind: 'source.connection', severity: 'Critical', state: 'Open', message: '连接超时', occurrences: 18, openedAt: new Date(Date.now() - 86400000).toISOString(), updatedAt: new Date().toISOString(), revision: 1 }];
const sourceEvents: SourceEvent[] = [{ id: 1, sourceId: 'rcd-a-03', kind: 'connection', previous: 'Connected', current: 'Disconnected', message: '连接超时', occurredAt: new Date().toISOString() }];
const managementAudit: ManagementAudit[] = [{ id: 1, kind: 'source.update', actor: '演示操作者@127.0.0.1', target: 'rcd-a-03', revision: 3, details: { group_id: 'fire-panel-a' }, occurredAt: new Date().toISOString() }];
const configuration: ConfigurationDocument = {
  schema_version: 1,
  groups: groups.map((group) => ({ id: group.id, name: group.name, default_source_id: group.defaultSourceId })),
  sources: sources.map((source) => ({ id: source.id, group_id: source.groupId, name: source.name, endpoint: source.endpoint, security_policy: source.securityPolicy, security_mode: source.securityMode, username: source.username, enabled: source.enabled })),
};
const systemInfo: SystemInfo = {
  opcEndpoint: 'opc.tcp://127.0.0.1:4840', httpListen: '0.0.0.0:8080', dataDirectory: 'C:\\ProgramData\\RcdManager', databasePath: 'C:\\ProgramData\\RcdManager\\manager.db', logDirectory: 'C:\\ProgramData\\RcdManager\\logs',
  commandTimeoutMs: 5000, pendingTimeoutMs: 30000, samplingIntervalMs: 250, publishingIntervalMs: 250, maxSources: 20, maxSessions: 10,
  certificate: { subject: 'CN=RCD Manager', serialNumber: '01', sha256: 'AA:BB:CC:DD', notBefore: new Date(Date.now() - 86400000).toISOString(), notAfter: new Date(Date.now() + 300 * 86400000).toISOString(), daysRemaining: 300, validNow: true, expiryWarning: false, applicationUris: ['urn:RcdManager:rcd_manager'] },
  storage: { dataDirectoryBytes: 24 * 1024 * 1024, databaseBytes: 4 * 1024 * 1024, logBytes: 20 * 1024 * 1024, diskFreeBytes: 120 * 1024 * 1024 * 1024, auditRetentionDays: 90, maxAuditRows: 100000 },
  metrics: { goroutines: 84, allocatedBytes: 48 * 1024 * 1024, heapObjects: 19000, refreshQueueDepth: 0, dirtyCatalogs: 0, dirtyCommands: 0, dirtyAudits: 0, dirtyStatusEvents: 0, dirtyStatusAlarms: 0, eventSubscribers: 1, droppedEventNotifications: 0 },
};

const overview: Overview = {
  managerState: 'running', uptimeSeconds: 1565051, opcEndpoint: 'opc.tcp://0.0.0.0:4840/', connectedSources: 5, abnormalSources: 1, unknownSources: 0, totalSources: 6, goodPoints: 10609, badPoints: 24, totalPoints: 10633, recentSelectionAt: new Date(Date.now() - 120000).toISOString(), recentSelectionCount: 1, sessionCount: 12, subscriptionCount: 28, openAlarms: 2, unacknowledgedAlarms: 1, version: 'v0.1.0', gitSha: 'devmock', systemTime: new Date().toISOString(), webApiState: '正常',
};

function bodyOf(init: RequestInit): Record<string, unknown> {
  if (typeof init.body !== 'string') return {};
  return JSON.parse(init.body) as Record<string, unknown>;
}

function page<T>(items: T[], url: URL): { items: T[]; page: number; page_size: number; total: number } {
  const pageSize = Math.max(1, Number(url.searchParams.get('limit') || 50));
  const start = Math.max(0, Number(url.searchParams.get('offset') || 0));
  const pageNumber = Math.floor(start / pageSize) + 1;
  return { items: items.slice(start, start + pageSize), page: pageNumber, page_size: pageSize, total: items.length };
}

export async function mockRequest(path: string, init: RequestInit): Promise<unknown> {
  await new Promise((resolve) => window.setTimeout(resolve, 120));
  const method = init.method ?? 'GET';
  const url = new URL(path, window.location.origin);
  const segments = url.pathname.split('/').filter(Boolean);

  if (method === 'GET' && url.pathname === '/overview') return overview;
  if (method === 'GET' && url.pathname === '/groups') return { items: groups };
  if (method === 'GET' && url.pathname === '/sources') {
    const groupId = url.searchParams.get('group_id');
    return { items: groupId ? sources.filter((source) => source.groupId === groupId) : sources };
  }
  if (method === 'GET' && url.pathname === '/sessions') return { items: sessions };
  if (method === 'GET' && url.pathname === '/points') {
    const query = (url.searchParams.get('q') ?? '').toLowerCase();
    const groupId = url.searchParams.get('group_id');
    const filtered = points.filter((point) => (!query || point.nodeId.toLowerCase().includes(query)) && (!groupId || point.groupId === groupId));
    return page(filtered, url);
  }
  if (method === 'GET' && url.pathname === '/commands') return page(commands, url);
  if (method === 'GET' && url.pathname === '/alarms') return page(alarms.filter((alarm) => !url.searchParams.get('status') || alarm.state === url.searchParams.get('status')), url);
  if (method === 'GET' && url.pathname === '/source-events') return page(sourceEvents, url);
  if (method === 'GET' && url.pathname === '/management-audit') return page(managementAudit, url);
  if (method === 'GET' && url.pathname === '/system') return systemInfo;
  if (method === 'GET' && url.pathname === '/configuration') return configuration;

  if (method === 'POST' && segments[0] === 'sources' && segments[2] === 'probe') {
    const source = sources.find((item) => item.id === decodeURIComponent(segments[1]));
    if (!source) throw new Error('来源不存在');
    return { source_id: source.id, endpoint: source.endpoint, ok: true, endpoint_count: 3, namespace_count: 4, variable_count: source.totalPoints, method_count: 1, duration_ms: 182, tested_at: new Date().toISOString(), stages: ['DiscoverEndpoints', 'ConnectAndActivateSession', 'ResolveNamespaceArray', 'BrowseAndValidateCatalog'].map((name, index) => ({ name, ok: true, duration_ms: 30 + index * 12 })) };
  }
  if (method === 'POST' && segments[0] === 'sources' && ['reconnect', 'rediscover'].includes(segments[2])) {
    const source = sources.find((item) => item.id === decodeURIComponent(segments[1]));
    if (!source) throw new Error('来源不存在');
    source.connectionState = 'Connecting';
    source.sessionState = 'Inactive';
    source.subscriptionState = 'Inactive';
    source.reconnectCount += 1;
    return source;
  }
  if (method === 'POST' && url.pathname === '/configuration/import') {
    const input = bodyOf(init);
    const document = input.document as ConfigurationDocument;
    return { dry_run: Boolean(input.dry_run), created_groups: 0, updated_groups: document.groups.length, created_sources: 0, updated_sources: document.sources.length, warnings: ['演示模式只执行预检'] };
  }

  if (method === 'PUT' && segments[0] === 'alarms') {
    const alarm = alarms.find((item) => item.id === Number(segments[1]));
    if (!alarm) throw new Error('告警不存在');
    if (segments[2] === 'acknowledgement') {
      alarm.acknowledgedAt = new Date().toISOString();
      alarm.acknowledgedBy = '演示操作者';
    } else if (segments[2] === 'silence') {
      const body = bodyOf(init);
      alarm.silencedUntil = String(body.silenced_until || '') || undefined;
    }
    alarm.revision += 1;
    return alarm;
  }

  if (method === 'PUT' && segments[0] === 'sessions' && segments[2] === 'groups' && segments[4] === 'selection') {
    const session = sessions.find((item) => item.id === decodeURIComponent(segments[1]));
    if (!session) throw new Error('目标会话已结束');
    const groupId = decodeURIComponent(segments[3]);
    const body = bodyOf(init);
    session.selectedSources = { ...session.selectedSources, [groupId]: String(body.source_id ?? '') };
    session.routeRevisions = { ...session.routeRevisions, [groupId]: (session.routeRevisions[groupId] ?? 1) + 1 };
    session.selectionRevision = Math.max(...Object.values(session.routeRevisions));
    overview.recentSelectionAt = new Date().toISOString();
    overview.recentSelectionCount += 1;
    return { group_id: groupId, source_id: String(body.source_id ?? ''), revision: session.routeRevisions[groupId] };
  }

  if (method === 'PUT' && segments[0] === 'groups' && segments[2] === 'default-source') {
    const group = groups.find((item) => item.id === decodeURIComponent(segments[1]));
    if (!group) throw new Error('分组不存在');
    const body = bodyOf(init);
    group.defaultSourceId = String(body.source_id ?? '');
    group.selectionRevision += 1;
    return group;
  }

  throw new Error(`mock 未实现 ${method} ${path}`);
}
