import { useQuery } from '@tanstack/react-query';
import { request } from './client';

export type LowerLogLevel = '' | 'TRACE' | 'DEBUG' | 'INFO' | 'WARN' | 'ERROR';

export interface LowerLogParams {
  commandAttempts?: 'all' | 'retried' | 'three';
  sourceId: string;
  level?: LowerLogLevel;
  portId?: string;
  deviceId?: string;
  query?: string;
  from?: string;
  to?: string;
  page?: number;
  pageSize?: number;
}

export interface LowerLogItem {
  sourceId: string;
  bootId: string;
  sequence: number;
  timestamp: string;
  receivedAt: string;
  level: string;
  component: string;
  portId?: string;
  deviceId?: string;
  message: string;
  fields?: Record<string, unknown>;
}

export interface LowerLogPage {
  items: LowerLogItem[];
  total: number;
  page: number;
  pageSize: number;
  invalidLines: number;
  lastReceivedAt?: string;
  pushState: 'never_received' | 'recent' | 'stored' | string;
}

export const lowerLogKeys = {
  all: ['lower-logs'] as const,
  source: (params: LowerLogParams) => ['lower-logs', params.sourceId, params] as const,
};

export async function listLowerLogs(params: LowerLogParams, signal?: AbortSignal): Promise<LowerLogPage> {
  const page = Math.max(1, params.page ?? 1);
  const pageSize = Math.max(1, Math.min(1000, params.pageSize ?? 100));
  const query = new URLSearchParams({
    source_id: params.sourceId,
    limit: String(pageSize),
    offset: String((page - 1) * pageSize),
  });
  if (params.level) query.set('level', params.level);
  if (params.portId) query.set('port_id', params.portId);
  if (params.deviceId) query.set('device_id', params.deviceId);
  if (params.query) query.set('q', params.query);
  if (params.commandAttempts) query.set('command_attempts', params.commandAttempts);
  if (params.from) query.set('from', params.from);
  if (params.to) query.set('to', params.to);
  const raw = await request(`/lower-logs?${query}`, { signal }) as {
    items?: Array<Record<string, unknown>>; total?: number; limit?: number; offset?: number;
    invalid_lines?: number; last_received_at?: string; push_state?: string;
  };
  return {
    items: (raw.items ?? []).map((item) => ({
      sourceId: String(item.source_id ?? ''), bootId: String(item.boot_id ?? ''), sequence: Number(item.sequence ?? 0),
      timestamp: String(item.timestamp ?? ''), receivedAt: String(item.received_at ?? ''), level: String(item.level ?? ''),
      component: String(item.component ?? ''), portId: item.port_id ? String(item.port_id) : undefined,
      deviceId: item.device_id ? String(item.device_id) : undefined, message: String(item.message ?? ''),
      fields: item.fields && typeof item.fields === 'object' ? item.fields as Record<string, unknown> : undefined,
    })),
    total: Number(raw.total ?? 0), page, pageSize: Number(raw.limit ?? pageSize),
    invalidLines: Number(raw.invalid_lines ?? 0), lastReceivedAt: raw.last_received_at,
    pushState: raw.push_state ?? 'never_received',
  };
}

export function useLowerLogs(params: LowerLogParams, enabled = true) {
  return useQuery({
    queryKey: lowerLogKeys.source(params),
    queryFn: ({ signal }) => listLowerLogs(params, signal),
    enabled: Boolean(params.sourceId) && enabled,
    placeholderData: (previous, query) => query?.queryKey[1] === params.sourceId ? previous : undefined,
    refetchOnWindowFocus: false,
  });
}
