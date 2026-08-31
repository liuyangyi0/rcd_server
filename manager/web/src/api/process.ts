import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { ApiError, request } from './client';
import { operationKeys, type OperationsTarget } from './operations';

export type ProcessState = 'stopped' | 'starting' | 'running' | 'stopping' | 'failed' | 'unknown';
export type ProcessAction = 'start' | 'stop' | 'restart';
export type ProcessOperationState = 'accepted' | 'running' | 'succeeded' | 'failed' | 'unknown';
export interface ProcessBinding {
  source_id: string;
  source_revision: number;
  agent_url: string;
  instance_id: string;
  revision: number;
  updated_at?: string;
}
export interface ProcessTarget extends OperationsTarget { bindingRevision: number; instanceId: string }
export interface ProcessSnapshot {
  schema_version?: number;
  instance_id: string;
  revision: string;
  state: ProcessState;
  desired_state: 'running' | 'stopped';
  pid?: number;
  started_at?: string;
  uptime_seconds?: number;
  exit_code?: number;
  last_error?: string;
  pending_request_id?: string;
  executable?: string;
  work_dir?: string;
  restart_policy?: 'never' | 'on-failure';
  restart_count?: number;
  last_exit_at?: string;
  last_stop_forced?: boolean;
}
export interface ProcessOperation {
  source_id: string;
  source_revision: number;
  binding_revision: number;
  request_id: string;
  instance_id: string;
  action: ProcessAction;
  state: ProcessOperationState;
  created_at: string;
  updated_at: string;
  error_code?: string;
  error?: string;
  forced?: boolean;
  snapshot?: ProcessSnapshot;
}
export interface SourceProcessStatus extends ProcessSnapshot {
  source_id: string;
  source_revision: number;
  binding_revision: number;
  pending_operation?: ProcessOperation;
}
export interface ProcessHistory {
  source_id: string;
  source_revision: number;
  items: ProcessOperation[];
  total: number;
  limit: number;
  offset: number;
}
export interface ProcessLog {
  source_id: string;
  source_revision: number;
  binding_revision: number;
  instance_id: string;
  text: string;
  truncated: boolean;
  size_bytes: number;
}
export interface ConfigRestartIntent { name: string; revision: string }

export const PROCESS_TIMEOUT_MS = 20_000;
export const PROCESS_POLL_MS = 2_000;
export const PROCESS_POLL_WINDOW_MS = 120_000;
export const MAX_PROCESS_LOG_BYTES = 65_536;
const options = { retry: false, refetchOnWindowFocus: false, refetchOnReconnect: false, gcTime: 0, staleTime: 1_000 } as const;
const route = (id: string, part: string) => `/sources/${encodeURIComponent(id)}/process/${part}`;
const states: readonly string[] = ['stopped', 'starting', 'running', 'stopping', 'failed', 'unknown'];
const operationStates: readonly string[] = ['accepted', 'running', 'succeeded', 'failed', 'unknown'];
const actions: readonly string[] = ['start', 'stop', 'restart'];

async function processRequest<T>(path: string, init: RequestInit = {}): Promise<T> {
  const controller = new AbortController();
  const cancel = () => controller.abort(init.signal?.reason);
  if (init.signal?.aborted) cancel();
  else init.signal?.addEventListener('abort', cancel, { once: true });
  let timedOut = false;
  const timer = window.setTimeout(() => { timedOut = true; controller.abort(); }, PROCESS_TIMEOUT_MS);
  try { return await request(path, { ...init, signal: controller.signal }) as T; }
  catch (error) {
    if (timedOut && !init.signal?.aborted) throw new ApiError(0, { code: 'process_timeout', message: '进程管理请求超时；不能据此判断动作是否已执行，请按请求编号核对。' }, '请求超时');
    throw error;
  } finally { window.clearTimeout(timer); init.signal?.removeEventListener('abort', cancel); }
}

function invalid(message: string): never { throw new ApiError(502, { code: 'invalid_response', message }, message); }
function identity<T extends { source_id: string; source_revision: number }>(value: T, target: OperationsTarget): T {
  if (value?.source_id !== target.id || value.source_revision !== target.revision) throw new ApiError(409, { code: 'source_revision_conflict', message: '来源版本已变化。旧面板不会显示新目标的数据或提交操作，请保留草稿后重新打开。' }, '来源版本冲突');
  return value;
}
function bindingIdentity<T extends { source_id: string; source_revision: number; binding_revision: number; instance_id: string }>(value: T, target: ProcessTarget): T {
  identity(value, target);
  if (value.binding_revision !== target.bindingRevision || value.instance_id !== target.instanceId) throw new ApiError(409, { code: 'binding_revision_conflict', message: '守护代理绑定已变化，不能使用旧目标的状态；请刷新绑定并重新确认。' }, '绑定版本冲突');
  return value;
}
function opaqueRevision(value: string): string {
  if (typeof value !== 'string' || !value.trim() || value.length > 4096 || /["\r\n]/.test(value)) invalid('进程代理返回了无效版本');
  return value;
}
function snapshot(value: ProcessSnapshot, instanceId: string): ProcessSnapshot {
  if (!value || value.instance_id !== instanceId || !states.includes(value.state) || !['running', 'stopped'].includes(value.desired_state)) invalid('进程代理返回了不匹配的实例或状态');
  opaqueRevision(value.revision);
  if (value.schema_version !== undefined && value.schema_version !== 0 && value.schema_version !== 1) invalid('进程协议版本暂不兼容');
  if (value.state === 'running' && (!Number.isInteger(value.pid) || Number(value.pid) <= 1)) invalid('运行状态缺少有效的进程编号');
  return value;
}
function operation(value: ProcessOperation, target: ProcessTarget, requestId?: string, action?: ProcessAction): ProcessOperation {
  bindingIdentity(value, target);
  if (!operationStates.includes(value.state) || !actions.includes(value.action) || !value.request_id || (requestId && value.request_id !== requestId) || (action && value.action !== action)) invalid('进程作业返回了不匹配的请求编号、动作或状态');
  if (value.snapshot) snapshot(value.snapshot, target.instanceId);
  if (value.state === 'succeeded') {
    const expected = value.action === 'stop' ? 'stopped' : 'running';
    if (value.snapshot?.state !== expected || value.snapshot.desired_state !== expected) invalid('成功作业缺少已确认的目标进程状态');
  }
  return value;
}
export function processTarget(target: OperationsTarget, binding?: ProcessBinding): ProcessTarget | undefined {
  return binding?.agent_url && binding.instance_id ? { ...target, bindingRevision: binding.revision, instanceId: binding.instance_id } : undefined;
}
export function processOperationTerminal(value?: ProcessOperation): boolean { return value?.state === 'succeeded' || value?.state === 'failed'; }
export function processLogLimit(value: number): number { return Number.isFinite(value) ? Math.max(4, Math.min(MAX_PROCESS_LOG_BYTES, Math.trunc(value))) : MAX_PROCESS_LOG_BYTES; }

export const processApi = {
  async binding(target: OperationsTarget, signal?: AbortSignal): Promise<ProcessBinding> {
    const result = identity(await processRequest<ProcessBinding>(route(target.id, 'binding'), { signal }), target);
    if (!Number.isSafeInteger(result.revision) || result.revision < 0 || typeof result.agent_url !== 'string' || typeof result.instance_id !== 'string') invalid('进程绑定返回格式不正确');
    return result;
  },
  async saveBinding(target: OperationsTarget, revision: number, agentUrl: string, instanceId: string): Promise<ProcessBinding> {
    return identity(await processRequest<ProcessBinding>(route(target.id, 'binding'), { method: 'PUT', headers: { 'If-Match': `"${revision}"` }, body: JSON.stringify({ source_revision: target.revision, agent_url: agentUrl.trim(), instance_id: instanceId.trim() }) }), target);
  },
  async status(target: ProcessTarget, signal?: AbortSignal): Promise<SourceProcessStatus> {
    const result = bindingIdentity(await processRequest<SourceProcessStatus>(route(target.id, 'status'), { signal }), target);
    snapshot(result, target.instanceId);
    if (result.pending_operation) operation(result.pending_operation, target);
    return result;
  },
  async act(target: ProcessTarget, revision: string, action: ProcessAction, requestId: string, expectedConfigRevisions?: Record<string, string>): Promise<ProcessOperation> {
    return operation(await processRequest<ProcessOperation>(route(target.id, 'actions'), { method: 'POST', headers: { 'If-Match': `"${opaqueRevision(revision)}"` }, body: JSON.stringify({ source_revision: target.revision, binding_revision: target.bindingRevision, action, request_id: requestId, expected_config_revisions: expectedConfigRevisions }) }), target, requestId, action);
  },
  async resolve(target: ProcessTarget, revision: string, action: ProcessAction, requestId: string, expectedConfigRevisions?: Record<string, string>): Promise<ProcessOperation> {
    // This seals or reconciles the original ID; it must never execute an action.
    // Keep exactly the original CAS and body fingerprint, including file hashes.
    return operation(await processRequest<ProcessOperation>(route(target.id, `operations/${encodeURIComponent(requestId)}/resolve`), { method: 'POST', headers: { 'If-Match': `"${opaqueRevision(revision)}"` }, body: JSON.stringify({ source_revision: target.revision, binding_revision: target.bindingRevision, action, request_id: requestId, expected_config_revisions: expectedConfigRevisions }) }), target, requestId, action);
  },
  async operation(target: ProcessTarget, requestId: string, signal?: AbortSignal): Promise<ProcessOperation> {
    return operation(await processRequest<ProcessOperation>(route(target.id, `operations/${encodeURIComponent(requestId)}`), { signal }), target, requestId);
  },
  async history(target: OperationsTarget, offset = 0, signal?: AbortSignal): Promise<ProcessHistory> {
    const result = identity(await processRequest<ProcessHistory>(route(target.id, `operations?limit=20&offset=${Math.max(0, Math.trunc(offset))}`), { signal }), target);
    if (!Array.isArray(result.items) || result.items.length > 20 || result.items.some((item) => item.source_id !== target.id || !actions.includes(item.action) || !operationStates.includes(item.state))) invalid('进程历史记录格式或来源不匹配');
    // Historical revisions are intentionally retained; never install them as a
    // current process snapshot after an agent has been rebound.
    return result;
  },
  async log(target: ProcessTarget, limit = MAX_PROCESS_LOG_BYTES, signal?: AbortSignal): Promise<ProcessLog> {
    const result = bindingIdentity(await processRequest<ProcessLog>(route(target.id, `log?limit=${processLogLimit(limit)}`), { signal }), target);
    if (typeof result.text !== 'string' || new TextEncoder().encode(result.text).byteLength > MAX_PROCESS_LOG_BYTES) invalid('代理日志超过允许的 64 KiB 单页大小');
    return result;
  },
};

export const processKeys = {
  source: (target: OperationsTarget) => [...operationKeys.source(target), 'process'] as const,
  binding: (target: OperationsTarget) => [...processKeys.source(target), 'binding'] as const,
  status: (target: ProcessTarget | OperationsTarget) => [...processKeys.source(target), 'status', 'bindingRevision' in target ? target.bindingRevision : 0, 'instanceId' in target ? target.instanceId : ''] as const,
  operation: (target: ProcessTarget, requestId: string) => [...processKeys.status(target), 'operation', requestId] as const,
  history: (target: OperationsTarget, offset = 0) => [...processKeys.source(target), 'history', offset] as const,
  log: (target: ProcessTarget, limit: number) => [...processKeys.status(target), 'log', processLogLimit(limit)] as const,
};

export function useProcessBinding(target: OperationsTarget) {
  return useQuery({ ...options, queryKey: processKeys.binding(target), queryFn: ({ signal }) => processApi.binding(target, signal) });
}
export function useProcessStatus(target: OperationsTarget, binding?: ProcessBinding) {
  const scope = processTarget(target, binding);
  return useQuery({ ...options, queryKey: processKeys.status(scope ?? target), queryFn: ({ signal }) => processApi.status(scope!, signal), enabled: Boolean(scope) });
}
export function useProcessOperation(target: ProcessTarget, requestId: string, poll: boolean, enabled = true) {
  return useQuery({ ...options, queryKey: processKeys.operation(target, requestId), queryFn: ({ signal }) => processApi.operation(target, requestId, signal), enabled: Boolean(requestId) && enabled, refetchInterval: (query) => enabled && poll && !processOperationTerminal(query.state.data) ? PROCESS_POLL_MS : false, refetchIntervalInBackground: false });
}
export function useProcessHistory(target: OperationsTarget, offset: number) {
  return useQuery({ ...options, queryKey: processKeys.history(target, offset), queryFn: ({ signal }) => processApi.history(target, offset, signal) });
}
export function useProcessLog(target: ProcessTarget, enabled: boolean, limit = MAX_PROCESS_LOG_BYTES) {
  return useQuery({ ...options, queryKey: processKeys.log(target, limit), queryFn: ({ signal }) => processApi.log(target, limit, signal), enabled });
}
export function useSaveProcessBinding(target: OperationsTarget) {
  const client = useQueryClient();
  return useMutation({ mutationFn: (input: { revision: number; agentUrl: string; instanceId: string }) => processApi.saveBinding(target, input.revision, input.agentUrl, input.instanceId), retry: false,
    onSuccess: (result) => { client.setQueryData(processKeys.binding(target), result); },
    onSettled: () => Promise.all([client.invalidateQueries({ queryKey: processKeys.binding(target) }), client.invalidateQueries({ queryKey: ['management-audit'] })]),
  });
}
export function useProcessAction(target: ProcessTarget) {
  const client = useQueryClient();
  return useMutation({ mutationFn: (input: { revision: string; action: ProcessAction; requestId: string; expectedConfigRevisions?: Record<string, string> }) => processApi.act(target, input.revision, input.action, input.requestId, input.expectedConfigRevisions), retry: false,
    onSuccess: (result) => { client.setQueryData(processKeys.operation(target, result.request_id), result); },
    onSettled: () => Promise.all([client.invalidateQueries({ queryKey: processKeys.source(target) }), client.invalidateQueries({ queryKey: ['management-audit'] })]),
  });
}
