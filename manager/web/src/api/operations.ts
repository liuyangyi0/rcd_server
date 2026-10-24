import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { ApiError, request } from './client';
import { queryKeys } from './queries';
import type { ModuleType } from './types';

export type OperationFeature = 'acquisition' | 'configuration' | 'configuration_forms' | 'logs' | 'station_versions' | 'calculation_mode' | 'logging_configuration';

export interface ConfigField { path: (string | number)[]; key: string; label: string; hint: string; kind: 'string' | 'integer' | 'boolean'; value: string | number | boolean; options: (string | number | boolean)[]; inherited: boolean; readonly: boolean }
export interface ConfigForm { source_id: string; source_revision: number; content: string; fields: ConfigField[] }
export interface ConfigEdit { path: (string | number)[]; value: string | number | boolean }
export type LoggingLevel = 'error' | 'warn' | 'info' | 'debug' | 'trace';
export interface OperationsTarget { id: string; revision: number }
export interface OperationsCapabilities {
  source_id: string;
  source_revision: number;
  supported: boolean;
  reason?: string;
  schema_version: number;
  features: OperationFeature[];
  max_config_bytes: number;
  max_log_bytes: number;
  configuration_apply: 'restart';
}
export interface AcquisitionDevice {
  device_id: string;
  name: string;
  read_enabled: boolean;
  applied_read_enabled: boolean;
  communication_ok: boolean;
  last_data_at?: string;
  configured_module_type?: ModuleType;
  detected_module_type?: ModuleType;
  type_mismatch?: boolean;
  communication_module_version?: string;
  communication_module_version_raw?: number;
  plc_version?: string;
  plc_version_raw?: number;
  version_payload?: string | number[];
  version_state?: string;
  last_version_attempt_at?: string;
  last_version_success_at?: string;
  last_version_error?: string;
  stale?: boolean;
}
export interface AcquisitionPort {
  port_id: string;
  port_name: string;
  enabled: boolean;
  applied_enabled: boolean;
  open: boolean;
  pending_commands?: number;
  control_pending?: boolean;
  last_control_error?: string;
  devices: AcquisitionDevice[];
}
export interface AcquisitionSnapshot {
  source_id: string;
  source_revision: number;
  schema_version: number;
  revision: string;
  station_revision?: string;
  software_version?: string;
  build_commit?: string;
  calculation?: {
    active_mode: 'local' | 'manager';
    configured_mode: 'local' | 'manager';
    rule_count: number;
    config_hash: string;
    restart_required?: boolean;
    config_error?: string;
  };
  status?: 'Applied' | 'Pending' | 'Unchanged';
  station_operation_revision?: string;
  ports: AcquisitionPort[];
}
export type AcquisitionApplyResult = AcquisitionSnapshot & { status: 'Applied' | 'Pending'; operation_revision?: string };
export interface ConfigFile {
  name: string;
  kind: string;
  size_bytes: number;
  revision: string;
  active_revision: string;
  restart_required: boolean;
}
export interface ConfigFiles { source_id: string; source_revision: number; items: ConfigFile[] }
export interface ConfigBackup { id: string; created_at: string; size_bytes: number }
export interface ConfigDocument extends ConfigFile {
  source_id: string;
  source_revision: number;
  content: string;
  backups: ConfigBackup[];
  backups_truncated?: boolean;
  viewing_backup_id?: string;
  backup_revision?: string;
}
export interface ConfigValidation { valid: boolean; errors: string[]; warnings: string[]; restart_required: boolean }
export interface LogFile { name: string; size_bytes: number; modified_at: string; generation: string }
export interface LogFiles { source_id: string; source_revision: number; items: LogFile[] }
export interface LogPage {
  source_id: string;
  source_revision: number;
  name: string;
  generation: string;
  offset: number;
  next_offset: number;
  size_bytes: number;
  eof: boolean;
  truncated: boolean;
  text: string;
}
export interface LogRequest { name: string; generation: string; offset: number; limit: number; q: string; level: string; tail: boolean }
export interface LoggingStatus {
  source_id: string;
  source_revision: number;
  active_level: LoggingLevel | 'custom';
  active_spec: string;
  configured_level: LoggingLevel | 'custom';
  configured_spec: string;
  config_revision: string;
  restart_required: boolean;
  environment_override: boolean;
  serial_frame_logs: boolean;
  http_push_enabled: boolean;
  http_push_source_id: string;
  status?: 'Saved' | 'Unchanged';
}

export const MAX_LOG_PAGE_BYTES = 65_536;
export const MIN_LOG_PAGE_BYTES = 4;
export const MAX_CONFIG_BYTES = 2_097_152;
export const OPERATIONS_TIMEOUT_MS = 20_000;
const queryOptions = { retry: false, refetchOnWindowFocus: false, refetchOnReconnect: false, gcTime: 0 } as const;
const prefix = (id: string) => `/sources/${encodeURIComponent(id)}/operations`;

// Keep the normal API's same-origin credentials, operator label and envelope
// handling. A cancelled write is uncertain, never an invitation to retry it.
async function operationRequest<T>(path: string, init: RequestInit = {}): Promise<T> {
  const controller = new AbortController();
  const abort = () => controller.abort(init.signal?.reason);
  if (init.signal?.aborted) abort();
  else init.signal?.addEventListener('abort', abort, { once: true });
  let timedOut = false;
  const timeout = window.setTimeout(() => { timedOut = true; controller.abort(); }, OPERATIONS_TIMEOUT_MS);
  try {
    return await request(path, { ...init, signal: controller.signal }) as T;
  } catch (error) {
    if (timedOut && !init.signal?.aborted) throw new ApiError(0, { code: 'operations_timeout', message: '运维请求超时，请刷新并核对下级实际状态' }, '运维请求超时');
    throw error;
  } finally {
    window.clearTimeout(timeout);
    init.signal?.removeEventListener('abort', abort);
  }
}

async function scopedResponse<T extends { source_id: string; source_revision: number }>(id: string, promise: Promise<T>, revision?: number): Promise<T> {
  const result = await promise;
  if (result.source_id !== id || (revision !== undefined && result.source_revision !== revision)) {
    throw new ApiError(409, { code: 'source_revision_conflict', message: '来源身份或配置版本已变化；请关闭旧运维面板后重新打开，不会显示新目标的数据' }, '来源配置版本冲突');
  }
  return result;
}

// During the rolling upgrade some lower builds nest station_version while the
// final additive schema flattens those fields onto each device. Normalize at
// the transport edge so the acquisition UI and mutation results have one safe
// shape throughout a mixed-version deployment.
function normalizeAcquisition<T extends AcquisitionSnapshot>(snapshot: T): T {
  return {
    ...snapshot,
    ports: snapshot.ports.map((port) => ({
      ...port,
      devices: port.devices.map((device) => {
        const nested = (device as AcquisitionDevice & { station_version?: Partial<AcquisitionDevice> }).station_version;
        if (!nested) return device;
        const flattened = { ...device, ...nested } as AcquisitionDevice & { station_version?: Partial<AcquisitionDevice> };
        delete flattened.station_version;
        return flattened;
      }),
    })),
  };
}

export const operationsApi = {
  capabilities: (id: string, signal?: AbortSignal, revision?: number) => scopedResponse(id, operationRequest<OperationsCapabilities>(`${prefix(id)}/capabilities`, { signal }), revision),
  status: async (id: string, signal?: AbortSignal, revision?: number) => normalizeAcquisition(await scopedResponse(id, operationRequest<AcquisitionSnapshot>(`${prefix(id)}/status`, { signal }), revision)),
  port: async (target: OperationsTarget, revision: string, portId: string, enabled: boolean) => normalizeAcquisition(await operationRequest<AcquisitionApplyResult>(`${prefix(target.id)}/port`, {
    method: 'PUT', headers: { 'If-Match': JSON.stringify(revision) }, body: JSON.stringify({ source_revision: target.revision, port_id: portId, enabled }),
  })),
  deviceRead: async (target: OperationsTarget, revision: string, portId: string, deviceId: string, enabled: boolean) => normalizeAcquisition(await operationRequest<AcquisitionApplyResult>(`${prefix(target.id)}/device-read`, {
    method: 'PUT', headers: { 'If-Match': JSON.stringify(revision) }, body: JSON.stringify({ source_revision: target.revision, port_id: portId, device_id: deviceId, enabled }),
  })),
  calculationMode: (target: OperationsTarget, revision: string, mode: 'local' | 'manager') => operationRequest<Record<string, unknown>>(`${prefix(target.id)}/calculation-mode`, {
		method: 'PUT', headers: { 'If-Match': JSON.stringify(revision) }, body: JSON.stringify({ source_revision: target.revision, mode }),
	}),
  logging: (id: string, signal?: AbortSignal, revision?: number) => scopedResponse(id, operationRequest<LoggingStatus>(`${prefix(id)}/logging`, { signal }), revision),
  loggingLevel: (target: OperationsTarget, revision: string, level: LoggingLevel) => scopedResponse(target.id, operationRequest<LoggingStatus>(`${prefix(target.id)}/logging`, {
    method: 'PUT', headers: { 'If-Match': JSON.stringify(revision) }, body: JSON.stringify({ source_revision: target.revision, level }),
  }), target.revision),
  deviceModuleType: async (target: OperationsTarget, stationRevision: string, portId: string, deviceId: string, moduleType: ModuleType) => normalizeAcquisition(await scopedResponse(target.id, operationRequest<AcquisitionSnapshot>(`${prefix(target.id)}/device-module-type`, {
    method: 'PUT', headers: { 'If-Match': JSON.stringify(stationRevision) }, body: JSON.stringify({ source_revision: target.revision, station_revision: stationRevision, port_id: portId, device_id: deviceId, module_type: moduleType }),
  }), target.revision)),
  queryDeviceVersion: async (target: OperationsTarget, stationRevision: string, portId: string, deviceId: string) => normalizeAcquisition(await scopedResponse(target.id, operationRequest<AcquisitionSnapshot>(`${prefix(target.id)}/device-version/query`, {
    method: 'POST', headers: { 'If-Match': JSON.stringify(stationRevision) }, body: JSON.stringify({ source_revision: target.revision, station_revision: stationRevision, port_id: portId, device_id: deviceId }),
  }), target.revision)),
  configs: (id: string, signal?: AbortSignal, revision?: number) => scopedResponse(id, operationRequest<ConfigFiles>(`${prefix(id)}/configs`, { signal }), revision),
  config: (id: string, name: string, backupId = '', signal?: AbortSignal, revision?: number) => {
    const params = new URLSearchParams({ name });
    if (backupId) params.set('backup_id', backupId);
    return scopedResponse(id, operationRequest<ConfigDocument>(`${prefix(id)}/config?${params}`, { signal }), revision);
  },
  validate: (id: string, name: string, content: string) => operationRequest<ConfigValidation>(`${prefix(id)}/config/validate`, { method: 'POST', body: JSON.stringify({ name, content }) }),
  prepare: (target: OperationsTarget, name: string, content: string, edits: ConfigEdit[] = [], signal?: AbortSignal) => scopedResponse(target.id, operationRequest<ConfigForm>(`${prefix(target.id)}/config/prepare`, { method: 'POST', signal, body: JSON.stringify({ source_revision: target.revision, name, content, edits }) }), target.revision),
  save: (target: OperationsTarget, revision: string, name: string, content: string) => operationRequest<ConfigDocument>(`${prefix(target.id)}/config`, {
    method: 'PUT', headers: { 'If-Match': JSON.stringify(revision) }, body: JSON.stringify({ source_revision: target.revision, name, content }),
  }),
  logs: (id: string, signal?: AbortSignal, revision?: number) => scopedResponse(id, operationRequest<LogFiles>(`${prefix(id)}/logs`, { signal }), revision),
  log: async (id: string, input: LogRequest, signal?: AbortSignal, revision?: number) => {
    const offset = Number.isFinite(input.offset) ? Math.max(0, Math.floor(input.offset)) : 0;
    const limit = Number.isFinite(input.limit) ? Math.max(MIN_LOG_PAGE_BYTES, Math.min(MAX_LOG_PAGE_BYTES, Math.floor(input.limit))) : MAX_LOG_PAGE_BYTES;
    const params = new URLSearchParams({ name: input.name, generation: input.generation, offset: String(offset), limit: String(limit) });
    if (input.q) params.set('q', input.q);
    if (input.level) params.set('level', input.level);
    if (input.tail) params.set('tail', 'true');
    const page = await scopedResponse(id, operationRequest<LogPage>(`${prefix(id)}/log?${params}`, { signal }), revision);
    if (new TextEncoder().encode(page.text).byteLength > MAX_LOG_PAGE_BYTES) throw new ApiError(502, { message: '下级返回的日志页超过 64 KiB，已拒绝显示' }, '日志页超限');
    return page;
  },
};

export const operationKeys = {
  source: (target: OperationsTarget) => ['source-operations', target.id, target.revision] as const,
  capabilities: (target: OperationsTarget) => [...operationKeys.source(target), 'capabilities'] as const,
  status: (target: OperationsTarget) => [...operationKeys.source(target), 'status'] as const,
  configs: (target: OperationsTarget) => [...operationKeys.source(target), 'configs'] as const,
  config: (target: OperationsTarget, name: string, backupId = '') => [...operationKeys.source(target), 'config', name, backupId] as const,
  logs: (target: OperationsTarget) => [...operationKeys.source(target), 'logs'] as const,
  log: (target: OperationsTarget, input: LogRequest) => [...operationKeys.source(target), 'log', input] as const,
  logging: (target: OperationsTarget) => [...operationKeys.source(target), 'logging'] as const,
};

export function useOperationsCapabilities(target: OperationsTarget, enabled = true) {
  return useQuery({ ...queryOptions, queryKey: operationKeys.capabilities(target), queryFn: ({ signal }) => operationsApi.capabilities(target.id, signal, target.revision), enabled: enabled && Boolean(target.id) });
}
export function useAcquisitionStatus(target: OperationsTarget, poll = false, enabled = true) {
  return useQuery({ ...queryOptions, queryKey: operationKeys.status(target), enabled, queryFn: ({ signal }) => operationsApi.status(target.id, signal, target.revision), refetchInterval: (query) => poll && !query.state.error ? 3_000 : false });
}
export function useOperationConfigs(target: OperationsTarget) {
  return useQuery({ ...queryOptions, queryKey: operationKeys.configs(target), queryFn: ({ signal }) => operationsApi.configs(target.id, signal, target.revision) });
}
export function useOperationConfig(target: OperationsTarget, name: string) {
  return useQuery({ ...queryOptions, queryKey: operationKeys.config(target, name), queryFn: ({ signal }) => operationsApi.config(target.id, name, '', signal, target.revision), enabled: Boolean(name) });
}
export function useOperationLogs(target: OperationsTarget) {
  return useQuery({ ...queryOptions, queryKey: operationKeys.logs(target), queryFn: ({ signal }) => operationsApi.logs(target.id, signal, target.revision) });
}
export function useOperationLog(target: OperationsTarget, input: LogRequest, poll = false, refreshKey = 0) {
  return useQuery({ ...queryOptions, queryKey: [...operationKeys.log(target, input), refreshKey], queryFn: ({ signal }) => operationsApi.log(target.id, input, signal, target.revision), refetchInterval: (query) => poll && !query.state.error ? 5_000 : false });
}
export function useLoggingStatus(target: OperationsTarget, enabled = true) {
  return useQuery({ ...queryOptions, queryKey: operationKeys.logging(target), queryFn: ({ signal }) => operationsApi.logging(target.id, signal, target.revision), enabled: enabled && Boolean(target.id) });
}

export function useLoggingLevelMutation(target: OperationsTarget) {
  const client = useQueryClient();
  return useMutation({
    retry: false,
    mutationFn: (input: { revision: string; level: LoggingLevel }) => operationsApi.loggingLevel(target, input.revision, input.level),
    onSuccess: (result) => client.setQueryData(operationKeys.logging(target), result),
    onSettled: async () => { await Promise.all([
      client.invalidateQueries({ queryKey: operationKeys.logging(target) }),
      client.invalidateQueries({ queryKey: operationKeys.configs(target) }),
      client.invalidateQueries({ queryKey: operationKeys.config(target, 'config.toml') }),
      client.invalidateQueries({ queryKey: queryKeys.allManagementAudit }),
    ]); },
  });
}

export function useAcquisitionMutation(target: OperationsTarget) {
  const client = useQueryClient();
  return useMutation({
    retry: false,
    mutationFn: (input: { revision: string; portId: string; deviceId?: string; enabled: boolean }) => input.deviceId === undefined
      ? operationsApi.port(target, input.revision, input.portId, input.enabled)
      : operationsApi.deviceRead(target, input.revision, input.portId, input.deviceId, input.enabled),
    onSuccess: (result) => { client.setQueryData(operationKeys.status(target), result); },
    onSettled: async () => { await Promise.all([
      client.invalidateQueries({ queryKey: operationKeys.status(target) }),
      client.invalidateQueries({ queryKey: queryKeys.allSources }),
      client.invalidateQueries({ queryKey: queryKeys.allManagementAudit }),
    ]); },
  });
}

export type DeviceVersionMutationInput = {
  target: OperationsTarget;
  stationRevision: string;
  portId: string;
  deviceId: string;
  deferInvalidation?: boolean;
} & ({ action: 'set_type'; moduleType: ModuleType } | { action: 'query' });

export function useDeviceVersionMutation() {
  const client = useQueryClient();
  return useMutation({
    retry: false,
    mutationFn: (input: DeviceVersionMutationInput) => input.action === 'set_type'
      ? operationsApi.deviceModuleType(input.target, input.stationRevision, input.portId, input.deviceId, input.moduleType)
      : operationsApi.queryDeviceVersion(input.target, input.stationRevision, input.portId, input.deviceId),
    onSuccess: (result, input) => { client.setQueryData(operationKeys.status(input.target), result); },
    onSettled: async (_result, _error, input) => { if (input.deferInvalidation) return; await Promise.all([
      client.invalidateQueries({ queryKey: operationKeys.status(input.target) }),
      client.invalidateQueries({ queryKey: queryKeys.allDeviceVersions }),
      client.invalidateQueries({ queryKey: queryKeys.allManagementAudit }),
    ]); },
  });
}
export function useValidateOperationConfig(target: OperationsTarget, name: string) {
  return useMutation({ retry: false, mutationFn: (content: string) => operationsApi.validate(target.id, name, content) });
}
export function useSaveOperationConfig(target: OperationsTarget, name: string) {
  const client = useQueryClient();
  return useMutation({
    retry: false,
    mutationFn: (input: { revision: string; content: string }) => operationsApi.save(target, input.revision, name, input.content),
    onSuccess: (result) => { client.setQueryData(operationKeys.config(target, name), result); },
    onSettled: async () => { await Promise.all([
      client.invalidateQueries({ queryKey: operationKeys.config(target, name) }),
      client.invalidateQueries({ queryKey: operationKeys.configs(target) }),
      client.invalidateQueries({ queryKey: operationKeys.status(target), refetchType: 'active' }),
      client.invalidateQueries({ queryKey: queryKeys.allManagementAudit }),
    ]); },
  });
}
