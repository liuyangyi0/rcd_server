import { afterEach, describe, expect, it, vi } from 'vitest';
import { api, ApiError } from './client';
import { setOperatorName } from '../lib/operator';

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
}

afterEach(() => {
  vi.unstubAllGlobals();
  window.localStorage.clear();
});

describe('API client contract', () => {
  it('serializes device mappings while preserving the explicit empty-array removal', async () => {
    const fetchMock = vi.fn().mockImplementation(() => Promise.resolve(jsonResponse({ id: 'rcd-1', group_id: 'fallback', revision: 4 })));
    vi.stubGlobal('fetch', fetchMock);
    await api.updateSource('rcd-1', { id: 'rcd-1', groupId: 'fallback', endpoint: 'opc.tcp://lower:4840', groupBindings: [{ portId: '/dev/ttyS0', deviceId: '1', groupId: 'device-a' }], revision: 3 });
    const [, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(init.headers).toMatchObject({ 'If-Match': '"3"' });
    expect(JSON.parse(String(init.body))).toMatchObject({ group_id: 'fallback', group_bindings: [{ port_id: '/dev/ttyS0', device_id: '1', group_id: 'device-a' }] });
    await api.updateSource('rcd-1', { id: 'rcd-1', groupId: 'fallback', endpoint: 'opc.tcp://lower:4840', groupBindings: [], revision: 4 });
    expect(JSON.parse(String((fetchMock.mock.calls[1][1] as RequestInit).body))).toHaveProperty('group_bindings', []);
  });

  it('requests topology summaries and paginates physical ownership filters', async () => {
    const fetchMock = vi.fn().mockImplementation(() => Promise.resolve(jsonResponse({ ports: [], items: [] })));
    vi.stubGlobal('fetch', fetchMock);
    await api.sourceTopology('lower-1');
    expect(fetchMock.mock.calls[0][0]).toBe('/api/v1/sources/lower-1/topology');
    await api.points({ page: 2, pageSize: 50, sourceId: 'lower-1', portId: '/dev/ttyS0', deviceId: '2' });
    expect(fetchMock.mock.calls[1][0]).toBe('/api/v1/points?limit=50&offset=50&source_id=lower-1&port_id=%2Fdev%2FttyS0&device_id=2');
  });

  it('uses If-Match and snake_case without a body revision', async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse({ id: 'fire', name: 'Fire', default_source_id: 'rcd-1', revision: 8 }));
    vi.stubGlobal('fetch', fetchMock);
    await api.updateGroup('fire', { id: 'fire', name: 'Fire', defaultSourceId: 'rcd-1', revision: 7 });

    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe('/api/v1/groups/fire');
    expect(init.headers).toMatchObject({ 'If-Match': '"7"' });
    const body = JSON.parse(String(init.body)) as Record<string, unknown>;
    expect(body).toEqual({ id: 'fire', name: 'Fire', default_source_id: 'rcd-1' });
    expect(body).not.toHaveProperty('revision');
  });

  it('maps UI pages to limit, offset and q', async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse({ items: [], total: 0, limit: 50, offset: 100 }));
    vi.stubGlobal('fetch', fetchMock);
    await api.points({ page: 3, pageSize: 50, query: 'KKS', groupId: 'fire' });
    expect(String(fetchMock.mock.calls[0][0])).toBe('/api/v1/points?limit=50&offset=100&q=KKS&group_id=fire');
  });

  it('passes session/diagnostic route filters and the local operator audit label', async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse({ items: [], total: 0, limit: 25, offset: 0 }));
    vi.stubGlobal('fetch', fetchMock);
    setOperatorName('Alice');
    await api.points({ page: 1, pageSize: 25, groupId: 'fire', sourceId: 'catalog-a', routeSourceId: 'route-b', sessionId: 'session-1', status: 'Bad', kind: 'connection' });
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe('/api/v1/points?limit=25&offset=0&group_id=fire&source_id=catalog-a&route_source_id=route-b&session_id=session-1&status=Bad&kind=connection');
    expect(init.headers).toMatchObject({ 'X-RCD-Operator': 'Alice' });
  });

  it('forwards point-query cancellation to fetch without turning cancellation into an API outage', async () => {
    const controller = new AbortController();
    const cancelled = new DOMException('Query superseded', 'AbortError');
    const fetchMock = vi.fn((_url: string, init: RequestInit) => new Promise<Response>((_resolve, reject) => {
      init.signal?.addEventListener('abort', () => reject(cancelled), { once: true });
    }));
    vi.stubGlobal('fetch', fetchMock);
    const request = api.points({ groupId: 'device-a', routeSourceId: 'lower-a' }, controller.signal);
    expect(fetchMock.mock.calls[0][1].signal).toBe(controller.signal);
    controller.abort();
    await expect(request).rejects.toBe(cancelled);
  });

  it('unwraps the backend error envelope', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse({ error: { code: 'revision_conflict', message: 'revision conflict', details: { expected: 2, actual: 3 } } }, 409)));
    await expect(api.deleteGroup('fire', 2)).rejects.toMatchObject({ status: 409, code: 'revision_conflict', message: 'revision conflict' } satisfies Partial<ApiError>);
  });

  it('preserves an explicit empty password so callers can clear the credential', async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse({
      id: 'rcd-1', group_id: 'fire', name: 'RCD 1', endpoint: 'opc.tcp://127.0.0.1:4841',
      security_policy: 'None', security_mode: 'None', enabled: true, revision: 3,
    }));
    vi.stubGlobal('fetch', fetchMock);

    await api.updateSource('rcd-1', {
      id: 'rcd-1', groupId: 'fire', name: 'RCD 1', endpoint: 'opc.tcp://127.0.0.1:4841',
      securityPolicy: 'None', securityMode: 'None', username: 'operator', password: '', enabled: true, revision: 2,
    });

    const [, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(JSON.parse(String(init.body))).toMatchObject({ username: 'operator', password: '' });
  });
});
