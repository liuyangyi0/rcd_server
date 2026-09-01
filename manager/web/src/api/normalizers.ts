import type {
  Alarm,
  CommandAudit,
  ConfigurationImportResult,
  DeviceVersion,
  DeviceVersionPage,
  ModuleType,
  Group,
  HealthState,
  ManagementAudit,
  Overview,
  PageResult,
  Point,
  ServiceState,
  Source,
  SourceEvent,
  SourceProbeResult,
  SourceTopology,
  SystemInfo,
  UaSession,
} from './types';

type UnknownRecord = Record<string, unknown>;

function record(value: unknown): UnknownRecord {
  return value && typeof value === 'object' ? (value as UnknownRecord) : {};
}

function first(source: UnknownRecord, ...keys: string[]): unknown {
  for (const key of keys) {
    if (source[key] !== undefined && source[key] !== null) return source[key];
  }
  return undefined;
}

function text(value: unknown, fallback = ''): string {
  if (typeof value === 'string') return value;
  if (typeof value === 'number' || typeof value === 'boolean') return String(value);
  return fallback;
}

function number(value: unknown, fallback = 0): number {
  if (typeof value === 'number' && Number.isFinite(value)) return value;
  const parsed = Number(value);
  return Number.isFinite(parsed) ? parsed : fallback;
}

function bool(value: unknown, fallback = false): boolean {
  if (typeof value === 'boolean') return value;
  if (typeof value === 'number') return value !== 0;
  if (typeof value === 'string') return ['true', '1', 'yes'].includes(value.toLowerCase());
  return fallback;
}

function unwrap(payload: unknown): unknown {
  const body = record(payload);
  return body.data ?? payload;
}

function serviceState(value: unknown): ServiceState {
  const normalized = text(value, 'unknown').toLowerCase();
  if (['running', 'started', 'healthy', 'ok'].includes(normalized)) return 'running';
  if (['starting', 'initializing'].includes(normalized)) return 'starting';
  if (['stopped', 'down'].includes(normalized)) return 'stopped';
  if (['degraded', 'warning'].includes(normalized)) return 'degraded';
  return 'unknown';
}

function healthState(value: unknown): HealthState {
  const normalized = text(value, 'unknown').toLowerCase();
  if (['good', 'healthy', 'normal', 'connected', 'running', 'ok'].includes(normalized)) return 'good';
  if (['bad', 'abnormal', 'error', 'failed', 'disconnected', 'offline'].includes(normalized)) return 'bad';
  if (['warning', 'degraded', 'waiting'].includes(normalized)) return 'warning';
  return 'unknown';
}

function timestamp(value: unknown): string | undefined {
  const result = text(value);
  return result || undefined;
}

export function normalizeOverview(payload: unknown): Overview {
  const body = record(unwrap(payload));
  const downstream = record(first(body, 'downstream', 'sources'));
  const quality = record(first(body, 'data_quality', 'dataQuality', 'quality'));
  const selection = record(first(body, 'recent_selection', 'recentSelection'));
  const startedAt = timestamp(first(body, 'started_at', 'startedAt'));
  const uptimeSeconds = startedAt ? Math.max(0, Math.round((Date.now() - new Date(startedAt).getTime()) / 1000)) : number(first(body, 'uptime_seconds', 'uptimeSeconds', 'uptime'));
  const directSourceCount = number(first(body, 'source_count', 'sourceCount', 'total_sources', 'totalSources'));
  const directConnectedCount = number(first(body, 'connected_sources', 'connectedSources'));
  const directPointCount = number(first(body, 'point_count', 'pointCount', 'total_points', 'totalPoints'));
  const overviewQualityKnown = ['good', 'bad', 'good_points', 'goodPoints', 'bad_points', 'badPoints'].some((key) => quality[key] !== undefined || body[key] !== undefined);
  return {
    managerState: first(body, 'manager_state', 'managerState', 'state', 'status') === undefined ? 'running' : serviceState(first(body, 'manager_state', 'managerState', 'state', 'status')),
    uptimeSeconds,
    opcEndpoint: text(first(body, 'opc_endpoint', 'opcEndpoint', 'endpoint'), '—'),
    connectedSources: number(first(downstream, 'connected', 'connected_sources', 'connectedSources', 'good'), directConnectedCount),
    abnormalSources: number(first(downstream, 'abnormal', 'abnormal_sources', 'abnormalSources', 'bad'), Math.max(0, directSourceCount - directConnectedCount)),
    unknownSources: number(first(downstream, 'unknown', 'unknown_sources', 'unknownSources')),
    totalSources: number(first(downstream, 'total', 'total_sources', 'totalSources'), directSourceCount),
    goodPoints: number(first(quality, 'good', 'good_points', 'goodPoints'), number(first(body, 'good_points', 'goodPoints'))),
    badPoints: number(first(quality, 'bad', 'bad_points', 'badPoints'), number(first(body, 'bad_points', 'badPoints'))),
    totalPoints: number(first(quality, 'total', 'total_points', 'totalPoints'), directPointCount),
    qualityKnown: overviewQualityKnown,
    recentSelectionAt: timestamp(first(selection, 'at', 'selected_at', 'selectedAt', 'last_selected_at', 'lastSelectedAt') ?? first(body, 'recent_selection_at', 'recentSelectionAt')),
    recentSelectionCount: number(first(selection, 'count', 'switch_count', 'switchCount'), number(first(body, 'recent_selection_count', 'recentSelectionCount'))),
    sessionCount: number(first(body, 'session_count', 'sessionCount', 'sessions')),
    subscriptionCount: number(first(body, 'subscription_count', 'subscriptionCount', 'subscriptions')),
    openAlarms: number(first(body, 'open_alarms', 'openAlarms')),
    unacknowledgedAlarms: number(first(body, 'unacknowledged_alarms', 'unacknowledgedAlarms')),
    version: text(first(body, 'version', 'build_version'), '—'),
    gitSha: text(first(body, 'git_sha', 'gitSha', 'commit'), '—'),
    systemTime: timestamp(first(body, 'system_time', 'systemTime')),
    webApiState: text(first(body, 'web_api_state', 'webApiState'), '正常'),
  };
}

export function normalizeGroup(payload: unknown): Group {
  const body = record(payload);
  const points = record(first(body, 'points', 'point_quality', 'pointQuality'));
  const statusValue = first(body, 'status', 'health', 'selected_source_state', 'selectedSourceState');
  return {
    id: text(first(body, 'id', 'group_id', 'groupId')),
    name: text(first(body, 'name', 'display_name', 'displayName'), text(first(body, 'id', 'group_id', 'groupId'))),
    defaultSourceId: text(first(body, 'default_source_id', 'defaultSourceId')),
    selectedSourceId: text(first(body, 'selected_source_id', 'selectedSourceId')) || undefined,
    selectionRevision: number(first(body, 'selection_revision', 'selectionRevision', 'revision')),
    sourceCount: number(first(body, 'source_count', 'sourceCount', 'member_count', 'memberCount'), Array.isArray(first(body, 'member_source_ids', 'memberSourceIds')) ? (first(body, 'member_source_ids', 'memberSourceIds') as unknown[]).length : 0),
    goodPoints: number(first(points, 'good', 'good_points', 'goodPoints'), number(first(body, 'good_points', 'goodPoints'))),
    badPoints: number(first(points, 'bad', 'bad_points', 'badPoints'), number(first(body, 'bad_points', 'badPoints'))),
    totalPoints: number(first(points, 'total', 'total_points', 'totalPoints'), number(first(body, 'total_points', 'totalPoints'))),
    qualityKnown: ['good', 'bad', 'good_points', 'goodPoints', 'bad_points', 'badPoints'].some((key) => points[key] !== undefined || body[key] !== undefined),
    pendingCommandCount: number(first(body, 'pending_command_count', 'pendingCommandCount', 'pending_commands', 'pendingCommands')),
    lastSelectedAt: timestamp(first(body, 'last_selected_at', 'lastSelectedAt')),
    status: healthState(statusValue),
  };
}

export function normalizeSource(payload: unknown): Source {
  const body = record(payload);
  const points = record(first(body, 'points', 'point_quality', 'pointQuality'));
  const rawConflicts = first(body, 'catalog_conflicts', 'catalogConflicts');
  const rawBindings = first(body, 'group_bindings', 'groupBindings');
  const availableGroups = first(body, 'available_group_ids', 'availableGroupIds');
  const catalogConflicts = Array.isArray(rawConflicts) ? rawConflicts.map((value) => {
    const conflict = record(value);
    const sourceIds = first(conflict, 'source_ids', 'sourceIds');
    return {
      kind: text(first(conflict, 'kind')),
      groupId: text(first(conflict, 'group_id', 'groupId')),
      identifier: text(first(conflict, 'identifier')),
      sourceIds: Array.isArray(sourceIds) ? sourceIds.map((item) => text(item)).filter(Boolean) : [],
      message: text(first(conflict, 'message')),
    };
  }) : [];
  return {
    id: text(first(body, 'id', 'source_id', 'sourceId')),
    name: text(first(body, 'name', 'display_name', 'displayName'), text(first(body, 'id', 'source_id', 'sourceId'))),
    groupId: text(first(body, 'group_id', 'groupId')),
    groupBindings: Array.isArray(rawBindings) ? rawBindings.map((value) => {
      const binding = record(value);
      return { groupId: text(first(binding, 'group_id', 'groupId')), portId: text(first(binding, 'port_id', 'portId')), deviceId: text(first(binding, 'device_id', 'deviceId')) };
    }) : [],
    availableGroupIds: Array.isArray(availableGroups) ? availableGroups.map((value) => text(value)) : undefined,
    endpoint: text(first(body, 'endpoint', 'endpoint_url', 'endpointUrl')),
    securityPolicy: text(first(body, 'security_policy', 'securityPolicy'), 'None'),
    securityMode: text(first(body, 'security_mode', 'securityMode'), 'None'),
    username: text(first(body, 'username')) || undefined,
    hasPassword: bool(first(body, 'has_password', 'hasPassword')),
    enabled: bool(first(body, 'enabled'), true),
    connectionState: text(first(body, 'connection_state', 'connectionState'), 'Unknown'),
    sessionState: text(first(body, 'session_state', 'sessionState'), 'Unknown'),
    subscriptionState: text(first(body, 'subscription_state', 'subscriptionState'), 'Unknown'),
    catalogState: text(first(body, 'catalog_state', 'catalogState'), 'Unknown'),
    catalogConflicts,
    lastDataAt: timestamp(first(body, 'last_data_at', 'lastDataAt', 'last_success_at', 'lastSuccessAt')),
    lastProbeAt: timestamp(first(body, 'last_probe_at', 'lastProbeAt')),
    lastError: text(first(body, 'last_error', 'lastError')) || undefined,
    reconnectCount: number(first(body, 'reconnect_count', 'reconnectCount')),
    monitoredCount: number(first(body, 'monitored_count', 'monitoredCount', 'subscription_count', 'subscriptionCount')),
    goodPoints: number(first(points, 'good', 'good_points', 'goodPoints'), number(first(body, 'good_points', 'goodPoints'))),
    badPoints: number(first(points, 'bad', 'bad_points', 'badPoints'), number(first(body, 'bad_points', 'badPoints'))),
    totalPoints: number(first(points, 'total', 'total_points', 'totalPoints'), number(first(body, 'total_points', 'totalPoints', 'point_count', 'pointCount'))),
    qualityKnown: ['good', 'bad', 'good_points', 'goodPoints', 'bad_points', 'badPoints'].some((key) => points[key] !== undefined || body[key] !== undefined),
    revision: number(first(body, 'revision', 'config_revision', 'configRevision')),
  };
}

export function normalizeSession(payload: unknown): UaSession {
  const body = record(payload);
  const selected = record(first(body, 'selected_sources', 'selectedSources', 'routes'));
  const selectedSources: Record<string, string> = {};
  const routeRevisions: Record<string, number> = {};
  for (const [key, value] of Object.entries(selected)) {
    const route = record(value);
    selectedSources[key] = route.source_id !== undefined || route.sourceId !== undefined ? text(first(route, 'source_id', 'sourceId')) : text(value);
    routeRevisions[key] = number(first(route, 'revision'), 1);
  }
  return {
    id: text(first(body, 'id', 'session_id', 'sessionId')),
    clientName: text(first(body, 'client_name', 'clientName', 'application_name', 'applicationName', 'client_application_uri', 'clientApplicationURI'), '匿名客户端'),
    remoteAddress: text(first(body, 'remote_address', 'remoteAddress', 'remote'), '—'),
    connectedAt: timestamp(first(body, 'connected_at', 'connectedAt', 'activated_at', 'activatedAt')),
    lastSeenAt: timestamp(first(body, 'last_seen_at', 'lastSeenAt')),
    selectionRevision: number(first(body, 'selection_revision', 'selectionRevision', 'revision'), Math.max(1, ...Object.values(routeRevisions))),
    selectedSources,
    routeRevisions,
  };
}

export function normalizeSourceTopology(payload: unknown): SourceTopology {
  const body = record(unwrap(payload));
  const rawPorts = first(body, 'ports');
  const groupId = text(first(body, 'group_id', 'groupId', 'default_group_id', 'defaultGroupId'));
  return {
    sourceId: text(first(body, 'source_id', 'sourceId')),
    groupId,
    supported: bool(first(body, 'supported')),
    catalogState: text(first(body, 'catalog_state', 'catalogState'), 'Unknown'),
    revision: number(first(body, 'revision')),
    schemaVersion: number(first(body, 'schema_version', 'schemaVersion')),
    calculationCount: number(first(body, 'calculation_count', 'calculationCount')),
    crossDeviceCalculationCount: number(first(body, 'cross_device_calculation_count', 'crossDeviceCalculationCount')),
    unassignedPointCount: number(first(body, 'unassigned_point_count', 'unassignedPointCount')),
    ports: Array.isArray(rawPorts) ? rawPorts.map((value) => {
      const port = record(value);
      const rawDevices = first(port, 'devices');
      return {
        portId: text(first(port, 'port_id', 'portId')),
        portName: text(first(port, 'port_name', 'portName')),
        sendNodeId: text(first(port, 'send_node_id', 'sendNodeId')),
        sendMethodId: text(first(port, 'send_method_id', 'sendMethodId')),
        pointCount: number(first(port, 'point_count', 'pointCount')),
        devices: Array.isArray(rawDevices) ? rawDevices.map((item) => {
          const device = record(item);
          return {
            deviceId: text(first(device, 'device_id', 'deviceId')),
            deviceAddress: number(first(device, 'device_address', 'deviceAddress')),
            name: text(first(device, 'name')),
            kksPrefix: text(first(device, 'kks_prefix', 'kksPrefix')),
            pointCount: number(first(device, 'point_count', 'pointCount')),
            calculationCount: number(first(device, 'calculation_count', 'calculationCount')),
            groupId: text(first(device, 'group_id', 'groupId'), groupId),
            bound: bool(first(device, 'bound')),
          };
        }) : [],
      };
    }) : [],
  };
}

function moduleType(value: unknown): ModuleType {
  const normalized = text(value, 'unknown').trim().toLowerCase().replaceAll('-', '_');
  if (['domestic', 'self_developed', 'cn', '国产', '自研'].includes(normalized)) return 'domestic';
  if (['legacy_russian', 'legacy', 'russian', '俄供', '原模块'].includes(normalized)) return 'legacy_russian';
  return 'unknown';
}

function payloadText(value: unknown): string | undefined {
  if (Array.isArray(value)) {
    const bytes = value.map((item) => number(item, -1));
    if (bytes.every((item) => item >= 0 && item <= 255)) return bytes.map((item) => item.toString(16).padStart(2, '0').toUpperCase()).join(' ');
  }
  return text(value).trim() || undefined;
}

export function normalizeDeviceVersion(payload: unknown): DeviceVersion {
  const body = record(payload);
  const nested = record(first(body, 'station_version', 'stationVersion'));
  const version = { ...body, ...nested };
  const communicationRaw = first(version, 'communication_module_version_raw', 'communicationModuleVersionRaw');
  const plcRaw = first(version, 'plc_version_raw', 'plcVersionRaw');
  const communicationVersion = text(first(version, 'communication_module_version', 'communicationModuleVersion'));
  const plcVersion = text(first(version, 'plc_version', 'plcVersion'));
  const versionPayload = payloadText(first(version, 'version_payload', 'versionPayload', 'payload'));
  const stationRevision = text(first(body, 'station_revision', 'stationRevision') ?? first(version, 'station_revision', 'stationRevision'));
  const portEnabled = bool(first(body, 'port_enabled', 'portEnabled'), true);
  const deviceReadEnabled = bool(first(body, 'device_read_enabled', 'deviceReadEnabled', 'read_enabled', 'readEnabled'), true);
  return {
    sourceId: text(first(body, 'source_id', 'sourceId')),
    sourceName: text(first(body, 'source_name', 'sourceName')),
    sourceRevision: number(first(body, 'source_revision', 'sourceRevision')),
    rcdSoftwareVersion: text(first(body, 'rcd_software_version', 'rcdSoftwareVersion', 'software_version', 'softwareVersion')),
    rcdBuildCommit: text(first(body, 'rcd_build_commit', 'rcdBuildCommit', 'build_commit', 'buildCommit')),
    portId: text(first(body, 'port_id', 'portId')),
    portName: text(first(body, 'port_name', 'portName')),
    deviceId: text(first(body, 'device_id', 'deviceId')),
    deviceName: text(first(body, 'device_name', 'deviceName', 'name')),
    configuredModuleType: moduleType(first(version, 'configured_module_type', 'configuredModuleType')),
    detectedModuleType: moduleType(first(version, 'detected_module_type', 'detectedModuleType')),
    typeMismatch: bool(first(version, 'type_mismatch', 'typeMismatch')),
    communicationModuleVersion: communicationVersion || undefined,
    communicationModuleVersionRaw: rawVersionValue(communicationRaw),
    plcVersion: plcVersion || undefined,
    plcVersionRaw: rawVersionValue(plcRaw),
    versionPayload,
    versionState: text(first(version, 'version_state', 'versionState', 'state'), 'unknown'),
    lastVersionAttemptAt: timestamp(first(version, 'last_version_attempt_at', 'lastVersionAttemptAt')),
    lastVersionSuccessAt: timestamp(first(version, 'last_version_success_at', 'lastVersionSuccessAt')),
    lastVersionError: text(first(version, 'last_version_error', 'lastVersionError', 'error')) || undefined,
    stationRevision,
    versionSupported: bool(first(body, 'version_supported', 'versionSupported'), Boolean(stationRevision)),
    sourceOnline: bool(first(body, 'source_online', 'sourceOnline', 'online')),
    cached: bool(first(body, 'cached', 'from_cache', 'fromCache')),
    stale: bool(first(body, 'stale', 'version_stale', 'versionStale')),
    portEnabled,
    portAppliedEnabled: bool(first(body, 'port_applied_enabled', 'portAppliedEnabled', 'applied_enabled', 'appliedEnabled'), portEnabled),
    portOpen: bool(first(body, 'port_open', 'portOpen', 'open'), portEnabled),
    deviceReadEnabled,
    appliedDeviceReadEnabled: bool(first(body, 'applied_device_read_enabled', 'appliedDeviceReadEnabled', 'applied_read_enabled', 'appliedReadEnabled'), deviceReadEnabled),
    communicationOK: bool(first(body, 'communication_ok', 'communicationOK', 'communicationOk')),
    controlPending: bool(first(body, 'control_pending', 'controlPending')),
    pendingCommands: number(first(body, 'pending_commands', 'pendingCommands')),
  };
}

function rawVersionValue(value: unknown): number | undefined {
  if (value === undefined || value === null) return undefined;
  return number(value);
}

export function normalizeDeviceVersionPage(payload: unknown): DeviceVersionPage {
  const page = normalizePage(payload, normalizeDeviceVersion);
  const body = record(unwrap(payload));
  const rawSummary = record(first(body, 'summary'));
  const count = (snake: string, camel: string, fallback: number) => {
    const value = first(rawSummary, snake, camel);
    return value === undefined ? fallback : number(value);
  };
  const current = page.items;
  const rawSoftwareVersions = first(rawSummary, 'lower_software_versions', 'lowerSoftwareVersions');
  const lowerSoftwareVersions = Array.isArray(rawSoftwareVersions) ? rawSoftwareVersions.map((item) => {
    const version = record(item);
    return {
      softwareVersion: text(first(version, 'software_version', 'softwareVersion')),
      buildCommit: text(first(version, 'build_commit', 'buildCommit')),
      sourceCount: number(first(version, 'source_count', 'sourceCount')),
    };
  }).filter((item) => item.sourceCount > 0 || item.softwareVersion || item.buildCommit) : [];
  return {
    ...page,
    summary: {
      total: count('total', 'total', page.total),
      domestic: count('domestic', 'domestic', current.filter((item) => item.configuredModuleType === 'domestic').length),
      legacyRussian: count('legacy_russian', 'legacyRussian', current.filter((item) => item.configuredModuleType === 'legacy_russian').length),
      unknown: count('unknown', 'unknown', current.filter((item) => item.configuredModuleType === 'unknown').length),
      mismatch: count('mismatch', 'mismatch', current.filter((item) => item.typeMismatch).length),
      failed: count('failed', 'failed', current.filter((item) => deviceVersionFailed(item.versionState)).length),
      offlineCached: count('offline_cached', 'offlineCached', current.filter((item) => !item.sourceOnline && item.cached).length),
      lowerSoftwareVersions,
    },
  };
}

function deviceVersionFailed(state: string): boolean {
  const normalized = state.toLowerCase();
  return normalized.includes('fail') || normalized.includes('error') || normalized.includes('timeout');
}

export function normalizePoint(payload: unknown): Point {
  const body = record(payload);
  const node = record(first(body, 'node'));
  const key = record(first(node, 'key'));
  const sample = record(first(body, 'sample'));
  const identifier = text(first(key, 'identifier'));
  const statusValue = first(sample, 'status_code', 'statusCode') ?? first(body, 'status_code', 'statusCode', 'status');
  return {
    nodeId: text(first(body, 'node_id', 'nodeId', 'kks', 'id'), identifier),
    browseName: text(first(body, 'browse_name', 'browseName', 'name'), text(first(node, 'browse_name', 'browseName'), identifier)),
		nodeClass: text(first(body, 'node_class', 'nodeClass'), text(first(node, 'class', 'node_class', 'nodeClass'), 'Variable')),
    groupId: text(first(body, 'group_id', 'groupId'), text(first(node, 'group_id', 'groupId'))),
    portId: text(first(body, 'port_id', 'portId') ?? first(node, 'port_id', 'portId')) || undefined,
    deviceId: text(first(body, 'device_id', 'deviceId') ?? first(node, 'device_id', 'deviceId')) || undefined,
    kind: text(first(body, 'kind') ?? first(node, 'kind')) || undefined,
    selectedSourceId: text(first(body, 'selected_source_id', 'selectedSourceId', 'source_id', 'sourceId')) || undefined,
    dataType: text(first(body, 'data_type', 'dataType'), text(first(node, 'data_type', 'dataType'), text(first(sample, 'type'), 'Unknown'))),
    value: first(sample, 'value') ?? first(body, 'value', 'display_value', 'displayValue'),
    statusCode: statusCodeName(statusValue),
    sourceTimestamp: timestamp(first(sample, 'source_timestamp', 'sourceTimestamp') ?? first(body, 'source_timestamp', 'sourceTimestamp')),
    sourcePresent: bool(first(body, 'source_present', 'sourcePresent'), Object.keys(sample).length > 0),
    deleted: bool(first(body, 'deleted', 'out_of_service', 'outOfService'), bool(first(node, 'retired'))),
  };
}

export function normalizeCommand(payload: unknown): CommandAudit {
  const body = record(payload);
	const rawStatusCode = first(body, 'status_code', 'statusCode');
	const createdAt = text(first(body, 'created_at', 'createdAt', 'started_at', 'startedAt'));
	const completedAt = timestamp(first(body, 'completed_at', 'completedAt', 'finished_at', 'finishedAt'));
	const explicitDuration = first(body, 'duration_ms', 'durationMs');
	let durationMs = explicitDuration === undefined ? undefined : number(explicitDuration);
	if (durationMs === undefined && createdAt && completedAt) {
		const elapsed = new Date(completedAt).getTime() - new Date(createdAt).getTime();
		if (Number.isFinite(elapsed)) durationMs = Math.max(0, Math.round(elapsed));
	}
  return {
    id: text(first(body, 'id', 'command_id', 'commandId')),
		createdAt,
		completedAt,
    sessionId: text(first(body, 'session_id', 'sessionId')) || undefined,
    groupId: text(first(body, 'group_id', 'groupId')),
    sourceId: text(first(body, 'source_id', 'sourceId')),
    port: text(first(body, 'port', 'port_id', 'portId')),
    operation: text(first(body, 'operation', 'method', 'kind'), 'Write'),
    result: text(first(body, 'result', 'status', 'state'), 'Unknown'),
		statusCode: rawStatusCode === undefined ? undefined : statusCodeName(rawStatusCode),
		durationMs,
    error: text(first(body, 'error', 'last_error', 'lastError')) || undefined,
  };
}

export function normalizeAlarm(payload: unknown): Alarm {
  const body = record(payload);
  return {
    id: number(first(body, 'id')),
    sourceId: text(first(body, 'source_id', 'sourceId')) || undefined,
    kind: text(first(body, 'kind')),
    severity: text(first(body, 'severity')),
    state: text(first(body, 'state')),
    message: text(first(body, 'message')),
    occurrences: number(first(body, 'occurrences'), 1),
    acknowledgedAt: timestamp(first(body, 'acknowledged_at', 'acknowledgedAt')),
    acknowledgedBy: text(first(body, 'acknowledged_by', 'acknowledgedBy')) || undefined,
    silencedUntil: timestamp(first(body, 'silenced_until', 'silencedUntil')),
    openedAt: text(first(body, 'opened_at', 'openedAt')),
    updatedAt: text(first(body, 'updated_at', 'updatedAt')),
    resolvedAt: timestamp(first(body, 'resolved_at', 'resolvedAt')),
    revision: number(first(body, 'revision')),
  };
}

export function normalizeSourceEvent(payload: unknown): SourceEvent {
  const body = record(payload);
  return {
    id: number(first(body, 'id')),
    sourceId: text(first(body, 'source_id', 'sourceId')),
    kind: text(first(body, 'kind')),
    previous: text(first(body, 'previous')) || undefined,
    current: text(first(body, 'current')),
    message: text(first(body, 'message')) || undefined,
    occurredAt: text(first(body, 'occurred_at', 'occurredAt')),
  };
}

export function normalizeManagementAudit(payload: unknown): ManagementAudit {
  const body = record(payload);
  return {
    id: number(first(body, 'id')),
    kind: text(first(body, 'kind')),
    actor: text(first(body, 'actor')),
    target: text(first(body, 'target')),
    revision: number(first(body, 'revision')),
    details: record(first(body, 'details')),
    occurredAt: text(first(body, 'occurred_at', 'occurredAt')),
  };
}

export function normalizeSourceProbe(payload: unknown): SourceProbeResult {
  const body = record(payload);
  const stages = first(body, 'stages');
  return {
    sourceId: text(first(body, 'source_id', 'sourceId')),
    endpoint: text(first(body, 'endpoint')),
    ok: bool(first(body, 'ok')),
    error: text(first(body, 'error')) || undefined,
    endpointCount: number(first(body, 'endpoint_count', 'endpointCount')),
    namespaceCount: number(first(body, 'namespace_count', 'namespaceCount')),
    variableCount: number(first(body, 'variable_count', 'variableCount')),
    methodCount: number(first(body, 'method_count', 'methodCount')),
    durationMs: number(first(body, 'duration_ms', 'durationMs')),
    testedAt: text(first(body, 'tested_at', 'testedAt')),
    stages: Array.isArray(stages) ? stages.map((value) => {
      const stage = record(value);
      return { name: text(first(stage, 'name')), ok: bool(first(stage, 'ok')), message: text(first(stage, 'message')) || undefined, durationMs: number(first(stage, 'duration_ms', 'durationMs')) };
    }) : [],
  };
}

export function normalizeSystemInfo(payload: unknown): SystemInfo {
  const body = record(payload);
  const certificate = record(first(body, 'certificate'));
  const storage = record(first(body, 'storage'));
  const metrics = record(first(body, 'metrics'));
  const applicationUris = first(certificate, 'application_uris', 'applicationUris');
  return {
    opcEndpoint: text(first(body, 'opc_endpoint', 'opcEndpoint')),
    httpListen: text(first(body, 'http_listen', 'httpListen')),
    dataDirectory: text(first(body, 'data_directory', 'dataDirectory')),
    databasePath: text(first(body, 'database_path', 'databasePath')),
    logDirectory: text(first(body, 'log_directory', 'logDirectory')),
    commandTimeoutMs: number(first(body, 'command_timeout_ms', 'commandTimeoutMs')),
    pendingTimeoutMs: number(first(body, 'pending_timeout_ms', 'pendingTimeoutMs')),
    samplingIntervalMs: number(first(body, 'sampling_interval_ms', 'samplingIntervalMs')),
    publishingIntervalMs: number(first(body, 'publishing_interval_ms', 'publishingIntervalMs')),
    maxSources: number(first(body, 'max_sources', 'maxSources')),
    maxSessions: number(first(body, 'max_sessions', 'maxSessions')),
    certificate: {
      error: text(first(certificate, 'error')) || undefined,
      subject: text(first(certificate, 'subject')),
      serialNumber: text(first(certificate, 'serial_number', 'serialNumber')),
      sha256: text(first(certificate, 'sha256')),
      notBefore: text(first(certificate, 'not_before', 'notBefore')),
      notAfter: text(first(certificate, 'not_after', 'notAfter')),
      daysRemaining: number(first(certificate, 'days_remaining', 'daysRemaining')),
      validNow: bool(first(certificate, 'valid_now', 'validNow')),
      expiryWarning: bool(first(certificate, 'expiry_warning', 'expiryWarning')),
      applicationUris: Array.isArray(applicationUris) ? applicationUris.map((value) => text(value)) : [],
    },
    storage: {
      dataDirectoryBytes: number(first(storage, 'data_directory_bytes', 'dataDirectoryBytes')),
      databaseBytes: number(first(storage, 'database_bytes', 'databaseBytes')),
      logBytes: number(first(storage, 'log_bytes', 'logBytes')),
      diskFreeBytes: number(first(storage, 'disk_free_bytes', 'diskFreeBytes'), -1),
      auditRetentionDays: number(first(storage, 'audit_retention_days', 'auditRetentionDays')),
      maxAuditRows: number(first(storage, 'max_audit_rows', 'maxAuditRows')),
    },
    metrics: {
      goroutines: number(first(metrics, 'goroutines')),
      allocatedBytes: number(first(metrics, 'allocated_bytes', 'allocatedBytes')),
      heapObjects: number(first(metrics, 'heap_objects', 'heapObjects')),
      refreshQueueDepth: number(first(metrics, 'refresh_queue_depth', 'refreshQueueDepth')),
      dirtyCatalogs: number(first(metrics, 'dirty_catalogs', 'dirtyCatalogs')),
      dirtyCommands: number(first(metrics, 'dirty_commands', 'dirtyCommands')),
      dirtyAudits: number(first(metrics, 'dirty_audits', 'dirtyAudits')),
      dirtyStatusEvents: number(first(metrics, 'dirty_status_events', 'dirtyStatusEvents')),
      dirtyStatusAlarms: number(first(metrics, 'dirty_status_alarms', 'dirtyStatusAlarms')),
      eventSubscribers: number(first(metrics, 'event_subscribers', 'eventSubscribers')),
      droppedEventNotifications: number(first(metrics, 'dropped_event_notifications', 'droppedEventNotifications')),
    },
  };
}

export function normalizeConfigurationImport(payload: unknown): ConfigurationImportResult {
  const body = record(payload);
  const warnings = first(body, 'warnings');
  return {
    dryRun: bool(first(body, 'dry_run', 'dryRun')),
    createdGroups: number(first(body, 'created_groups', 'createdGroups')),
    updatedGroups: number(first(body, 'updated_groups', 'updatedGroups')),
    createdSources: number(first(body, 'created_sources', 'createdSources')),
    updatedSources: number(first(body, 'updated_sources', 'updatedSources')),
    warnings: Array.isArray(warnings) ? warnings.map((value) => text(value)) : [],
  };
}

export function normalizeList<T>(payload: unknown, mapper: (item: unknown) => T): T[] {
  const unwrapped = unwrap(payload);
  if (Array.isArray(unwrapped)) return unwrapped.map(mapper);
  const body = record(unwrapped);
  const items = first(body, 'items', 'results', 'rows');
  return Array.isArray(items) ? items.map(mapper) : [];
}

export function normalizePage<T>(payload: unknown, mapper: (item: unknown) => T): PageResult<T> {
  const unwrapped = unwrap(payload);
  if (Array.isArray(unwrapped)) {
    const items = unwrapped.map(mapper);
    return { items, page: 1, pageSize: items.length || 50, total: items.length };
  }
  const body = record(unwrapped);
  const pagination = record(first(body, 'pagination', 'meta'));
  const rawItems = first(body, 'items', 'results', 'rows');
  const items = Array.isArray(rawItems) ? rawItems.map(mapper) : [];
  const backendLimit = number(first(body, 'limit'), number(first(pagination, 'limit')));
  const backendOffset = number(first(body, 'offset'), number(first(pagination, 'offset')));
  const normalizedPageSize = number(first(body, 'page_size', 'pageSize'), number(first(pagination, 'page_size', 'pageSize'), backendLimit || items.length || 50));
  return {
    items,
    pageSize: normalizedPageSize,
    page: backendLimit > 0 ? Math.floor(backendOffset / backendLimit) + 1 : number(first(body, 'page', 'page_number', 'pageNumber'), number(first(pagination, 'page', 'page_number', 'pageNumber'), 1)),
    total: number(first(body, 'total', 'total_count', 'totalCount'), number(first(pagination, 'total', 'total_count', 'totalCount'), items.length)),
  };
}

function statusCodeName(value: unknown): string {
  const code = number(value, -1);
  const names = new Map<number, string>([
    [0x00000000, 'Good'], [0x80310000, 'BadNoCommunication'], [0x80320000, 'BadWaitingForInitialData'],
    [0x80890000, 'BadConfigurationError'], [0x808d0000, 'BadOutOfService'],
    [0x80b20000, 'BadWaitingForResponse'], [0x800a0000, 'BadTimeout'],
  ]);
  if (names.has(code)) return names.get(code) as string;
  if (code >= 0) return `0x${code.toString(16).padStart(8, '0').toUpperCase()}`;
  const asText = text(value);
  return asText || 'Unknown';
}
