import { afterEach, describe, expect, it, vi } from 'vitest';
import { operationKeys, operationsApi, OPERATIONS_TIMEOUT_MS, type LogRequest } from './operations';
import { setOperatorName } from '../lib/operator';

const target = { id: 'lower/a b', revision: 7 };
const logInput: LogRequest = { name: 'service.log', generation: 'g-1', offset: 0, limit: 65_536, q: '', level: '', tail: true };
function json(body: unknown, status = 200) { return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } }); }
function page(text = 'line') { return { source_id: target.id, source_revision: target.revision, name: 'service.log', generation: 'g-1', offset: 0, next_offset: 4, size_bytes: 4, eof: true, truncated: false, text }; }

afterEach(() => { vi.unstubAllGlobals(); vi.useRealTimers(); window.localStorage.clear(); });

describe('lower operations API contract', () => {
  it('uses the normal same-origin/operator transport and both revision scopes', async () => {
    const fetchMock = vi.fn().mockImplementation(() => Promise.resolve(json({ status: 'Applied', ports: [] })));
    vi.stubGlobal('fetch', fetchMock);
    setOperatorName('operator-a');
    await operationsApi.port(target, 'lower-r9', '/dev/ttyS0', false);
    await operationsApi.deviceRead(target, 'lower-r10', '/dev/ttyS0', '2', false);
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe('/api/v1/sources/lower%2Fa%20b/operations/port');
    expect(init.credentials).toBe('same-origin');
    expect(init.headers).toMatchObject({ 'If-Match': '"lower-r9"', 'X-RCD-Operator': 'operator-a', 'X-RCD-Request': 'web' });
    expect(JSON.parse(String(init.body))).toEqual({ source_revision: 7, port_id: '/dev/ttyS0', enabled: false });
    expect(JSON.parse(String(fetchMock.mock.calls[1][1].body))).toEqual({ source_revision: 7, port_id: '/dev/ttyS0', device_id: '2', enabled: false });
  });

  it('sends source and station revisions for module type and version queries', async () => {
    const response = { source_id: target.id, source_revision: target.revision, schema_version: 1, revision: '2', station_revision: '12', status: 'Pending', station_operation_revision: '13', ports: [] };
    const fetchMock = vi.fn().mockImplementation(() => Promise.resolve(json(response)));
    vi.stubGlobal('fetch', fetchMock);
    const moduleResult = await operationsApi.deviceModuleType(target, '11', '/dev/ttyWCH0', '12', 'domestic');
    await operationsApi.queryDeviceVersion(target, '12', '/dev/ttyWCH0', '12');
    expect(fetchMock.mock.calls[0][0]).toBe('/api/v1/sources/lower%2Fa%20b/operations/device-module-type');
    expect(fetchMock.mock.calls[0][1].headers).toMatchObject({ 'If-Match': '"11"' });
    expect(JSON.parse(String(fetchMock.mock.calls[0][1].body))).toEqual({ source_revision: 7, station_revision: '11', port_id: '/dev/ttyWCH0', device_id: '12', module_type: 'domestic' });
    expect(fetchMock.mock.calls[1][0]).toBe('/api/v1/sources/lower%2Fa%20b/operations/device-version/query');
    expect(JSON.parse(String(fetchMock.mock.calls[1][1].body))).toEqual({ source_revision: 7, station_revision: '12', port_id: '/dev/ttyWCH0', device_id: '12' });
    expect(moduleResult).toMatchObject({ status: 'Pending', station_operation_revision: '13' });
  });

  it('flattens rolling-upgrade station_version records at the transport edge', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(json({
      source_id: target.id,
      source_revision: target.revision,
      schema_version: 1,
      revision: '1',
      station_revision: '1',
      ports: [{
        port_id: 'p1',
        devices: [{
          device_id: '1',
          name: 'D1',
          read_enabled: true,
          applied_read_enabled: true,
          communication_ok: true,
          station_version: {
            configured_module_type: 'domestic',
            communication_module_version: 'V1.4',
            version_state: 'success',
          },
        }],
      }],
    })));
    const status = await operationsApi.status(target.id, undefined, target.revision);
    expect(status.ports[0].devices[0]).toMatchObject({ device_id: '1', configured_module_type: 'domestic', communication_module_version: 'V1.4', version_state: 'success' });
    expect(status.ports[0].devices[0]).not.toHaveProperty('station_version');
  });

  it('keeps validation read-only and saves with the current file revision', async () => {
    const fetchMock = vi.fn().mockImplementation(() => Promise.resolve(json({ valid: true, source_id: target.id, source_revision: target.revision })));
    vi.stubGlobal('fetch', fetchMock);
    await operationsApi.config(target.id, 'serial ports.toml', 'backup/1');
    await operationsApi.validate(target.id, 'serial.toml', 'enabled = false');
    await operationsApi.save(target, 'file-current', 'serial.toml', 'enabled = false');
    expect(String(fetchMock.mock.calls[0][0])).toContain('name=serial+ports.toml&backup_id=backup%2F1');
    expect(JSON.parse(String(fetchMock.mock.calls[1][1].body))).toEqual({ name: 'serial.toml', content: 'enabled = false' });
    expect(fetchMock.mock.calls[2][1].headers).toMatchObject({ 'If-Match': '"file-current"' });
    expect(JSON.parse(String(fetchMock.mock.calls[2][1].body))).toEqual({ source_revision: 7, name: 'serial.toml', content: 'enabled = false' });
  });

  it('reads and saves the lower logging level with the config revision', async () => {
    const logging = { source_id: target.id, source_revision: target.revision, active_level: 'info', active_spec: 'warn,rcd_server=info', configured_level: 'debug', configured_spec: 'warn,rcd_server=debug', config_revision: 'cfg-2', restart_required: true, environment_override: false, serial_frame_logs: false, http_push_enabled: true, http_push_source_id: target.id };
    const fetchMock = vi.fn().mockImplementation(() => Promise.resolve(json(logging)));
    vi.stubGlobal('fetch', fetchMock);
    await operationsApi.logging(target.id, undefined, target.revision);
    await operationsApi.loggingLevel(target, 'cfg-1', 'debug');
    expect(fetchMock.mock.calls[0][0]).toBe('/api/v1/sources/lower%2Fa%20b/operations/logging');
    expect(fetchMock.mock.calls[1][1].headers).toMatchObject({ 'If-Match': '"cfg-1"' });
    expect(JSON.parse(String(fetchMock.mock.calls[1][1].body))).toEqual({ source_revision: 7, level: 'debug' });
  });

  it.each([[-10, 4], [0, 4], [1, 4], [3, 4], [4, 4], [65_536, 65_536], [100_000, 65_536], [Number.NaN, 65_536], [Number.POSITIVE_INFINITY, 65_536]])('bounds log limit %s to %s bytes', async (limit, expected) => {
    const fetchMock = vi.fn().mockImplementation(() => Promise.resolve(json(page())));
    vi.stubGlobal('fetch', fetchMock);
    await operationsApi.log(target.id, { ...logInput, limit, q: '串口 timeout', level: 'ERROR', offset: -1 });
    const url = new URL(String(fetchMock.mock.calls[0][0]), 'http://localhost');
    expect(url.searchParams.get('limit')).toBe(String(expected));
    expect(url.searchParams.get('offset')).toBe('0');
    expect(url.searchParams.get('q')).toBe('串口 timeout');
    expect(url.searchParams.get('level')).toBe('ERROR');
    expect(url.searchParams.get('generation')).toBe('g-1');
  });

  it('rejects a lower server that returns an oversized log page', async () => {
    vi.stubGlobal('fetch', vi.fn().mockImplementation(() => Promise.resolve(json(page('x'.repeat(65_537))))));
    await expect(operationsApi.log(target.id, logInput)).rejects.toMatchObject({ status: 502 });
  });

  it('rejects new-endpoint log data returned to an old source revision', async () => {
    vi.stubGlobal('fetch', vi.fn().mockImplementation(() => Promise.resolve(json({ ...page(), source_revision: 8 }))));
    await expect(operationsApi.log(target.id, logInput, undefined, 7)).rejects.toMatchObject({ status: 409, code: 'source_revision_conflict' });
    vi.stubGlobal('fetch', vi.fn().mockImplementation(() => Promise.resolve(json({ source_id: 'other', source_revision: 7, items: [] }))));
    await expect(operationsApi.logs(target.id, undefined, 7)).rejects.toMatchObject({ status: 409, code: 'source_revision_conflict' });
  });

  it('cancels obsolete reads when the query signal is aborted', async () => {
    const aborted = new DOMException('obsolete source', 'AbortError');
    const fetchMock = vi.fn((_url: string, init: RequestInit) => new Promise<Response>((_resolve, reject) => {
      init.signal?.addEventListener('abort', () => reject(aborted), { once: true });
    }));
    vi.stubGlobal('fetch', fetchMock);
    const controller = new AbortController();
    const pending = operationsApi.config(target.id, 'serial.toml', '', controller.signal);
    controller.abort();
    await expect(pending).rejects.toBe(aborted);
    expect(fetchMock.mock.calls[0][1].signal?.aborted).toBe(true);
  });

  it('times out an uncertain write once without retrying', async () => {
    vi.useFakeTimers();
    const fetchMock = vi.fn((_url: string, init: RequestInit) => new Promise<Response>((_resolve, reject) => {
      init.signal?.addEventListener('abort', () => reject(new DOMException('timeout', 'AbortError')), { once: true });
    }));
    vi.stubGlobal('fetch', fetchMock);
    const assertion = expect(operationsApi.port(target, 'r1', 'p1', false)).rejects.toMatchObject({ status: 0, code: 'operations_timeout' });
    await vi.advanceTimersByTimeAsync(OPERATIONS_TIMEOUT_MS);
    await assertion;
    expect(fetchMock).toHaveBeenCalledOnce();
  });

  it('isolates query cache entries by source, file, backup, generation and cursor', () => {
    expect(operationKeys.config(target, 'a', 'b1')).not.toEqual(operationKeys.config(target, 'a', 'b2'));
    expect(operationKeys.config(target, 'a')).not.toEqual(operationKeys.config({ ...target, id: 'other' }, 'a'));
    expect(operationKeys.log(target, logInput)).not.toEqual(operationKeys.log(target, { ...logInput, offset: 65_536 }));
    expect(operationKeys.log(target, logInput)).not.toEqual(operationKeys.log(target, { ...logInput, generation: 'g2' }));
    expect(operationKeys.log(target, logInput)).not.toEqual(operationKeys.log(target, { ...logInput, q: 'error' }));
  });
});
