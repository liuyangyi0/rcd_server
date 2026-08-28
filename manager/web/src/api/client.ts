import {
  normalizeAlarm,
  normalizeCommand,
  normalizeConfigurationImport,
  normalizeGroup,
  normalizeList,
  normalizeManagementAudit,
  normalizeOverview,
  normalizePage,
  normalizePoint,
  normalizeSession,
  normalizeSource,
  normalizeSourceEvent,
  normalizeSourceProbe,
  normalizeSystemInfo,
} from './normalizers';
import type {
  Alarm,
  ApiFailure,
  CommandAudit,
  ConfigurationDocument,
  ConfigurationImportResult,
  Group,
  GroupInput,
  ListParams,
  ManagementAudit,
  Overview,
  PageResult,
  Point,
  RevisionMutation,
  Source,
  SourceEvent,
  SourceInput,
  SourceProbeResult,
  SystemInfo,
  UaSession,
} from './types';
import { getOperatorName } from '../lib/operator';

export const API_BASE = '/api/v1';
export const isMockMode = import.meta.env.VITE_API_MODE === 'mock';

export class ApiError extends Error {
  readonly status: number;
  readonly code?: string;
  readonly detail?: string;

  constructor(status: number, body: ApiFailure, fallback: string) {
    super(body.message || fallback);
    this.name = 'ApiError';
    this.status = status;
    this.code = body.code;
    this.detail = body.detail;
  }
}

export function queryString(params: ListParams): string {
  const query = new URLSearchParams();
  const limit = params.pageSize ?? 50;
  const page = Math.max(1, params.page ?? 1);
  query.set('limit', String(limit));
  query.set('offset', String((page - 1) * limit));
  if (params.query) query.set('q', params.query);
  if (params.groupId) query.set('group_id', params.groupId);
  if (params.sourceId) query.set('source_id', params.sourceId);
  if (params.routeSourceId) query.set('route_source_id', params.routeSourceId);
  if (params.sessionId) query.set('session_id', params.sessionId);
  if (params.status) query.set('status', params.status);
  if (params.kind) query.set('kind', params.kind);
  const value = query.toString();
  return value ? `?${value}` : '';
}

async function parseResponse(response: Response): Promise<unknown> {
  if (response.status === 204) return undefined;
  const contentType = response.headers.get('content-type') ?? '';
  if (contentType.includes('application/json')) return response.json();
  const text = await response.text();
  return text ? { message: text } : undefined;
}

async function request(path: string, init: RequestInit = {}): Promise<unknown> {
  if (isMockMode) {
    const { mockRequest } = await import('./mock');
    return mockRequest(path, init);
  }

  let response: Response;
  try {
    const operator = getOperatorName();
    response = await fetch(`${API_BASE}${path}`, {
      ...init,
      credentials: 'same-origin',
      headers: {
        Accept: 'application/json',
        ...(operator ? { 'X-RCD-Operator': operator } : {}),
        ...(init.body ? { 'Content-Type': 'application/json', 'X-RCD-Request': 'web' } : {}),
        ...init.headers,
      },
    });
  } catch (error) {
    throw new ApiError(0, { message: '无法连接管理 API', detail: error instanceof Error ? error.message : undefined }, '无法连接管理 API');
  }

  const body = await parseResponse(response);
  if (!response.ok) {
    const envelope = body && typeof body === 'object' ? (body as { error?: ApiFailure }) : {};
    const failure = envelope.error ?? (body && typeof body === 'object' ? (body as ApiFailure) : {});
    if (!failure.detail && failure.details !== undefined) {
      failure.detail = typeof failure.details === 'string' ? failure.details : JSON.stringify(failure.details);
    }
    throw new ApiError(response.status, failure, `管理 API 返回 ${response.status}`);
  }
  return body;
}

function encodeSegment(value: string): string {
  return encodeURIComponent(value);
}

export const api = {
  async overview(): Promise<Overview> {
    return normalizeOverview(await request('/overview'));
  },

  async groups(): Promise<Group[]> {
    return normalizeList(await request('/groups'), normalizeGroup);
  },

  async createGroup(input: GroupInput): Promise<Group> {
    return normalizeGroup(await request('/groups', { method: 'POST', body: JSON.stringify({ id: input.id, name: input.name ?? input.id, default_source_id: input.defaultSourceId || undefined }) }));
  },

  async updateGroup(groupId: string, input: GroupInput): Promise<Group> {
    return normalizeGroup(await request(`/groups/${encodeSegment(groupId)}`, {
      method: 'PUT',
      headers: { 'If-Match': `"${input.revision}"` },
      body: JSON.stringify({ id: input.id, name: input.name ?? input.id, default_source_id: input.defaultSourceId || undefined }),
    }));
  },

  async deleteGroup(groupId: string, revision: number): Promise<void> {
    await request(`/groups/${encodeSegment(groupId)}`, { method: 'DELETE', headers: { 'If-Match': `"${revision}"` } });
  },

  async sources(params: ListParams = {}): Promise<Source[]> {
    return normalizeList(await request(`/sources${queryString(params)}`), normalizeSource);
  },

  async createSource(input: SourceInput): Promise<Source> {
    return normalizeSource(await request('/sources', { method: 'POST', body: JSON.stringify(sourceMutation(input)) }));
  },

  async updateSource(sourceId: string, input: SourceInput): Promise<Source> {
    return normalizeSource(await request(`/sources/${encodeSegment(sourceId)}`, {
      method: 'PUT',
      headers: { 'If-Match': `"${input.revision}"` },
      body: JSON.stringify(sourceMutation(input)),
    }));
  },

  async deleteSource(sourceId: string, revision: number): Promise<void> {
    await request(`/sources/${encodeSegment(sourceId)}`, { method: 'DELETE', headers: { 'If-Match': `"${revision}"` } });
  },

  async probeSource(sourceId: string): Promise<SourceProbeResult> {
    return normalizeSourceProbe(await request(`/sources/${encodeSegment(sourceId)}/probe`, { method: 'POST' }));
  },

  async restartSource(sourceId: string, revision: number, rediscover = false): Promise<Source> {
    const operation = rediscover ? 'rediscover' : 'reconnect';
    return normalizeSource(await request(`/sources/${encodeSegment(sourceId)}/${operation}`, {
      method: 'POST', headers: { 'If-Match': `"${revision}"` },
    }));
  },

  async sessions(): Promise<UaSession[]> {
    return normalizeList(await request('/sessions'), normalizeSession);
  },

  async points(params: ListParams): Promise<PageResult<Point>> {
    return normalizePage(await request(`/points${queryString(params)}`), normalizePoint);
  },

  async commands(params: ListParams): Promise<PageResult<CommandAudit>> {
    return normalizePage(await request(`/commands${queryString(params)}`), normalizeCommand);
  },

  async alarms(params: ListParams): Promise<PageResult<Alarm>> {
    return normalizePage(await request(`/alarms${queryString(params)}`), normalizeAlarm);
  },

  async acknowledgeAlarm(id: number, revision: number): Promise<Alarm> {
    return normalizeAlarm(await request(`/alarms/${id}/acknowledgement`, {
      method: 'PUT', headers: { 'If-Match': `"${revision}"` }, body: '{}',
    }));
  },

  async silenceAlarm(id: number, revision: number, silencedUntil?: string): Promise<Alarm> {
    return normalizeAlarm(await request(`/alarms/${id}/silence`, {
      method: 'PUT', headers: { 'If-Match': `"${revision}"` }, body: JSON.stringify({ silenced_until: silencedUntil ?? '' }),
    }));
  },

  async sourceEvents(params: ListParams): Promise<PageResult<SourceEvent>> {
    return normalizePage(await request(`/source-events${queryString(params)}`), normalizeSourceEvent);
  },

  async managementAudit(params: ListParams): Promise<PageResult<ManagementAudit>> {
    return normalizePage(await request(`/management-audit${queryString(params)}`), normalizeManagementAudit);
  },

  async systemInfo(): Promise<SystemInfo> {
    return normalizeSystemInfo(await request('/system'));
  },

  async configuration(): Promise<ConfigurationDocument> {
    return await request('/configuration') as ConfigurationDocument;
  },

  async importConfiguration(document: ConfigurationDocument, dryRun: boolean): Promise<ConfigurationImportResult> {
    return normalizeConfigurationImport(await request('/configuration/import', {
      method: 'POST', body: JSON.stringify({ document, dry_run: dryRun }),
    }));
  },

  async selectSessionSource(sessionId: string, groupId: string, input: RevisionMutation): Promise<void> {
    await request(`/sessions/${encodeSegment(sessionId)}/groups/${encodeSegment(groupId)}/selection`, {
      method: 'PUT',
      headers: { 'If-Match': `"${input.revision}"` },
      body: JSON.stringify({ source_id: input.sourceId }),
    });
  },

  async setDefaultSource(groupId: string, input: RevisionMutation): Promise<Group> {
    return normalizeGroup(
      await request(`/groups/${encodeSegment(groupId)}/default-source`, {
        method: 'PUT',
        headers: { 'If-Match': `"${input.revision}"` },
        body: JSON.stringify({ source_id: input.sourceId }),
      }),
    );
  },
};

function sourceMutation(input: SourceInput) {
  return {
    id: input.id,
    group_id: input.groupId,
    name: input.name ?? input.id,
    endpoint: input.endpoint,
    security_policy: input.securityPolicy ?? 'None',
    security_mode: input.securityMode ?? 'None',
    username: input.username || undefined,
    password: input.password,
    enabled: input.enabled ?? true,
  };
}

export function apiErrorMessage(error: unknown): string {
  if (error instanceof ApiError) {
    if (error.status === 409) return error.message || '配置版本冲突或当前组存在执行中的命令';
    if (error.status === 404) return error.message || '目标资源已不存在';
    return error.detail ? `${error.message}：${error.detail}` : error.message;
  }
  return error instanceof Error ? error.message : '发生未知错误';
}
