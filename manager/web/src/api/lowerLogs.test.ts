import { afterEach, describe, expect, it, vi } from 'vitest';
import { listLowerLogs } from './lowerLogs';

afterEach(() => vi.unstubAllGlobals());

describe('central lower log API', () => {
  it('encodes server-side filters and normalizes snake case', async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify({
      items: [{ source_id: 'lower/a', boot_id: 'b1', sequence: 7, timestamp: '2026-09-01T01:00:00Z', received_at: '2026-09-01T01:00:01Z', level: 'ERROR', component: 'serial', port_id: '/dev/ttyWCH0', device_id: '13', message: 'timeout' }],
      total: 1, limit: 50, offset: 100, push_state: 'recent', last_received_at: '2026-09-01T01:00:01Z',
    }), { status: 200, headers: { 'Content-Type': 'application/json' } }));
    vi.stubGlobal('fetch', fetchMock);
    const page = await listLowerLogs({ sourceId: 'lower/a', level: 'ERROR', portId: '/dev/ttyWCH0', deviceId: '13', query: 'time out', page: 3, pageSize: 50 });
    expect(fetchMock.mock.calls[0][0]).toBe('/api/v1/lower-logs?source_id=lower%2Fa&limit=50&offset=100&level=ERROR&port_id=%2Fdev%2FttyWCH0&device_id=13&q=time+out');
    expect(page).toMatchObject({ total: 1, page: 3, pushState: 'recent', items: [{ sourceId: 'lower/a', sequence: 7, portId: '/dev/ttyWCH0', deviceId: '13' }] });
  });
});
