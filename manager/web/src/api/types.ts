export type ServiceState = 'running' | 'starting' | 'stopped' | 'degraded' | 'unknown';
export type HealthState = 'good' | 'bad' | 'warning' | 'unknown';

export interface Overview {
  managerState: ServiceState;
  uptimeSeconds: number;
  opcEndpoint: string;
  connectedSources: number;
  abnormalSources: number;
  unknownSources: number;
  totalSources: number;
  goodPoints: number;
  badPoints: number;
  totalPoints: number;
  qualityKnown?: boolean;
  recentSelectionAt?: string;
  recentSelectionCount: number;
  sessionCount: number;
  subscriptionCount: number;
  openAlarms: number;
  unacknowledgedAlarms: number;
  version: string;
  gitSha: string;
  systemTime?: string;
  webApiState: string;
}

export interface Group {
  id: string;
  name: string;
  defaultSourceId: string;
  selectedSourceId?: string;
  selectionRevision: number;
  sourceCount: number;
  goodPoints: number;
  badPoints: number;
  totalPoints: number;
  qualityKnown?: boolean;
  pendingCommandCount: number;
  lastSelectedAt?: string;
  status: HealthState;
}

export interface CatalogConflict {
  kind: string;
  groupId: string;
  identifier: string;
  sourceIds: string[];
  message: string;
}

export interface Source {
  id: string;
  name: string;
  groupId: string;
  endpoint: string;
  securityPolicy: string;
  securityMode: string;
  username?: string;
  hasPassword: boolean;
  enabled: boolean;
  connectionState: string;
  sessionState: string;
  subscriptionState: string;
  catalogState: string;
  catalogConflicts?: CatalogConflict[];
  lastDataAt?: string;
  lastProbeAt?: string;
  lastError?: string;
  reconnectCount: number;
  monitoredCount: number;
  goodPoints: number;
  badPoints: number;
  totalPoints: number;
  qualityKnown?: boolean;
  revision: number;
}

export interface UaSession {
  id: string;
  clientName: string;
  remoteAddress: string;
  connectedAt?: string;
  lastSeenAt?: string;
  selectionRevision: number;
  selectedSources: Record<string, string>;
  routeRevisions: Record<string, number>;
}

export interface Point {
  nodeId: string;
  browseName: string;
	nodeClass: string;
  groupId: string;
  selectedSourceId?: string;
  dataType: string;
  value?: unknown;
  statusCode: string;
  sourceTimestamp?: string;
  sourcePresent: boolean;
  deleted: boolean;
}

export interface CommandAudit {
  id: string;
  createdAt: string;
  completedAt?: string;
  sessionId?: string;
  groupId: string;
  sourceId: string;
  port: string;
  operation: string;
  result: string;
  statusCode?: string;
  durationMs?: number;
  error?: string;
}

export interface PageResult<T> {
  items: T[];
  page: number;
  pageSize: number;
  total: number;
}

export interface ListParams {
  page?: number;
  pageSize?: number;
  query?: string;
  groupId?: string;
  sourceId?: string;
  routeSourceId?: string;
  sessionId?: string;
  status?: string;
  kind?: string;
}

export interface Alarm {
  id: number;
  sourceId?: string;
  kind: string;
  severity: string;
  state: string;
  message: string;
  occurrences: number;
  acknowledgedAt?: string;
  acknowledgedBy?: string;
  silencedUntil?: string;
  openedAt: string;
  updatedAt: string;
  resolvedAt?: string;
  revision: number;
}

export interface SourceEvent {
  id: number;
  sourceId: string;
  kind: string;
  previous?: string;
  current: string;
  message?: string;
  occurredAt: string;
}

export interface ManagementAudit {
  id: number;
  kind: string;
  actor: string;
  target: string;
  revision: number;
  details?: Record<string, unknown>;
  occurredAt: string;
}

export interface SourceProbeStage {
  name: string;
  ok: boolean;
  message?: string;
  durationMs: number;
}

export interface SourceProbeResult {
  sourceId: string;
  endpoint: string;
  ok: boolean;
  error?: string;
  endpointCount: number;
  namespaceCount: number;
  variableCount: number;
  methodCount: number;
  durationMs: number;
  stages: SourceProbeStage[];
  testedAt: string;
}

export interface SystemInfo {
  opcEndpoint: string;
  httpListen: string;
  dataDirectory: string;
  databasePath: string;
  logDirectory: string;
  commandTimeoutMs: number;
  pendingTimeoutMs: number;
  samplingIntervalMs: number;
  publishingIntervalMs: number;
  maxSources: number;
  maxSessions: number;
  certificate: {
    error?: string;
    subject: string;
    serialNumber: string;
    sha256: string;
    notBefore: string;
    notAfter: string;
    daysRemaining: number;
    validNow: boolean;
    expiryWarning: boolean;
    applicationUris: string[];
  };
  storage: {
    dataDirectoryBytes: number;
    databaseBytes: number;
    logBytes: number;
    diskFreeBytes: number;
    auditRetentionDays: number;
    maxAuditRows: number;
  };
  metrics: {
    goroutines: number;
    allocatedBytes: number;
    heapObjects: number;
    refreshQueueDepth: number;
    dirtyCatalogs: number;
    dirtyCommands: number;
    dirtyAudits: number;
    dirtyStatusEvents: number;
    dirtyStatusAlarms: number;
    eventSubscribers: number;
    droppedEventNotifications: number;
  };
}

export interface ConfigurationDocument {
  schema_version: number;
  exported_at?: string;
  groups: Array<{ id: string; name: string; default_source_id?: string }>;
  sources: Array<{
    id: string;
    group_id: string;
    name: string;
    endpoint: string;
    security_policy: string;
    security_mode: string;
    username?: string;
    enabled: boolean;
  }>;
}

export interface ConfigurationImportResult {
  dryRun: boolean;
  createdGroups: number;
  updatedGroups: number;
  createdSources: number;
  updatedSources: number;
  warnings: string[];
}

export interface RevisionMutation {
  sourceId: string;
  revision: number;
}

export interface GroupInput {
  id: string;
  name?: string;
  defaultSourceId?: string;
  revision?: number;
}

export interface SourceInput {
  id: string;
  name?: string;
  groupId: string;
  endpoint: string;
  username?: string;
  password?: string;
  securityPolicy?: string;
  securityMode?: string;
  enabled?: boolean;
  revision?: number;
}

export interface EventEnvelope {
  type: string;
  groupId?: string;
  sourceId?: string;
  sessionId?: string;
  revision?: number;
  occurredAt?: string;
}

export interface ApiFailure {
  code?: string;
  message?: string;
  detail?: string;
  details?: unknown;
}

export const EMPTY_OVERVIEW: Overview = {
  managerState: 'unknown',
  uptimeSeconds: 0,
  opcEndpoint: '—',
  connectedSources: 0,
  abnormalSources: 0,
  unknownSources: 0,
  totalSources: 0,
  goodPoints: 0,
  badPoints: 0,
  totalPoints: 0,
  recentSelectionCount: 0,
  sessionCount: 0,
  subscriptionCount: 0,
  openAlarms: 0,
  unacknowledgedAlarms: 0,
  version: '—',
  gitSha: '—',
  webApiState: '不可用',
};
