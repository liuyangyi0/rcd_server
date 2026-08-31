import { afterEach, describe, expect, it, vi } from 'vitest';
import { setOperatorName } from '../lib/operator';
import { MAX_PROCESS_LOG_BYTES, PROCESS_TIMEOUT_MS, processApi, processKeys, processOperationTerminal, processTarget, type ProcessBinding, type ProcessOperation, type ProcessTarget, type SourceProcessStatus } from './process';

const target: ProcessTarget = { id: 'lower/a b', revision: 7, bindingRevision: 4, instanceId: 'fixed-a' };
const binding: ProcessBinding = { source_id: target.id, source_revision: 7, revision: 4, agent_url: 'http://agent:4851', instance_id: 'fixed-a' };
const status: SourceProcessStatus = { source_id: target.id, source_revision: 7, binding_revision: 4, instance_id: 'fixed-a', schema_version: 1, revision: 'opaque-r9', state: 'stopped', desired_state: 'stopped' };
const job: ProcessOperation = { source_id: target.id, source_revision: 7, binding_revision: 4, instance_id: 'fixed-a', request_id: 'request/a', action: 'start', state: 'accepted', created_at: '2026-08-28T01:00:00Z', updated_at: '2026-08-28T01:00:00Z' };
function json(body: unknown, code = 200) { return new Response(JSON.stringify(body), { status: code, headers: { 'Content-Type': 'application/json' } }); }
function log(text = 'stdout\n中文 stderr') { return { source_id: target.id, source_revision: 7, binding_revision: 4, instance_id: 'fixed-a', text, size_bytes: 200, truncated: true }; }
afterEach(() => { vi.unstubAllGlobals(); vi.useRealTimers(); window.localStorage.clear(); });

describe('independent lower-process API', () => {
  it('keeps binding CAS separate and never derives an agent from an OPC endpoint', async () => {
    const fetchMock = vi.fn().mockImplementation(() => Promise.resolve(json(binding)));
    vi.stubGlobal('fetch', fetchMock); setOperatorName('值班员');
    await processApi.saveBinding(target, 0, ' http://agent:4851 ', ' fixed-a ');
    expect(fetchMock.mock.calls[0][0]).toBe('/api/v1/sources/lower%2Fa%20b/process/binding');
    const init = fetchMock.mock.calls[0][1] as RequestInit;
    expect(init.credentials).toBe('same-origin');
    expect(init.headers).toMatchObject({ 'If-Match': '"0"', 'X-RCD-Operator': '值班员', 'X-RCD-Request': 'web' });
    expect(JSON.parse(String(init.body))).toEqual({ source_revision: 7, agent_url: 'http://agent:4851', instance_id: 'fixed-a' });
    expect(processTarget(target, { ...binding, agent_url: '', instance_id: '', revision: 0 })).toBeUndefined();
    expect(processTarget({ id: target.id, revision: 7 }, binding)).toEqual(target);
  });

  it('sends all three revision scopes and regards HTTP 202 as unfinished', async () => {
    const fetchMock = vi.fn().mockImplementation(() => Promise.resolve(json(job, 202)));
    vi.stubGlobal('fetch', fetchMock);
    const result = await processApi.act(target, 'opaque-r9', 'start', 'request/a');
    expect(processOperationTerminal(result)).toBe(false);
    expect(fetchMock.mock.calls[0][0]).toBe('/api/v1/sources/lower%2Fa%20b/process/actions');
    const init = fetchMock.mock.calls[0][1] as RequestInit;
    expect(init.headers).toMatchObject({ 'If-Match': '"opaque-r9"' });
    expect(JSON.parse(String(init.body))).toEqual({ source_revision: 7, binding_revision: 4, action: 'start', request_id: 'request/a' });
  });

  it('preserves the complete original restart fingerprint for resolve without resending actions', async () => {
    const fetchMock = vi.fn().mockImplementation(() => Promise.resolve(json({ ...job, action: 'restart', state: 'unknown' }, 202)));
    vi.stubGlobal('fetch', fetchMock);
    const files = { 'calc.toml': 'a'.repeat(64) };
    await processApi.act(target, 'opaque-r9', 'restart', 'request/a', files);
    await processApi.resolve(target, 'opaque-r9', 'restart', 'request/a', files);
    expect(fetchMock.mock.calls.map(([url]) => url)).toEqual(['/api/v1/sources/lower%2Fa%20b/process/actions', '/api/v1/sources/lower%2Fa%20b/process/operations/request%2Fa/resolve']);
    expect(fetchMock.mock.calls[1][1].body).toBe(fetchMock.mock.calls[0][1].body);
    expect(fetchMock.mock.calls[1][1].headers).toMatchObject({ 'If-Match': '"opaque-r9"' });
    expect(JSON.parse(String(fetchMock.mock.calls[1][1].body)).expected_config_revisions).toEqual(files);
  });

  it('rejects source or binding identity drift before displaying agent data', async () => {
    vi.stubGlobal('fetch', vi.fn().mockImplementation(() => Promise.resolve(json({ ...binding, source_revision: 8 }))));
    await expect(processApi.binding(target)).rejects.toMatchObject({ code: 'source_revision_conflict', status: 409 });
    vi.stubGlobal('fetch', vi.fn().mockImplementation(() => Promise.resolve(json({ ...status, binding_revision: 5 }))));
    await expect(processApi.status(target)).rejects.toMatchObject({ code: 'binding_revision_conflict', status: 409 });
    vi.stubGlobal('fetch', vi.fn().mockImplementation(() => Promise.resolve(json({ ...log(), instance_id: 'different-machine' }))));
    await expect(processApi.log(target)).rejects.toMatchObject({ code: 'binding_revision_conflict', status: 409 });
  });

  it('requires a matching operation and a real process snapshot for succeeded', async () => {
    vi.stubGlobal('fetch', vi.fn().mockImplementation(() => Promise.resolve(json({ ...job, request_id: 'wrong' }))));
    await expect(processApi.operation(target, 'request/a')).rejects.toMatchObject({ status: 502 });
    vi.stubGlobal('fetch', vi.fn().mockImplementation(() => Promise.resolve(json({ ...job, state: 'succeeded' }))));
    await expect(processApi.operation(target, 'request/a')).rejects.toMatchObject({ status: 502 });
    vi.stubGlobal('fetch', vi.fn().mockImplementation(() => Promise.resolve(json({ ...job, state: 'succeeded', snapshot: { ...status, state: 'running', desired_state: 'running', pid: 30 } }))));
    expect(processOperationTerminal(await processApi.operation(target, 'request/a'))).toBe(true);
    expect(processOperationTerminal({ ...job, state: 'unknown' })).toBe(false);
    expect(processOperationTerminal({ ...job, state: 'failed', error_code: 'unconfirmed_cancelled' })).toBe(true);
  });

  it('never converts an unreachable agent into a stopped process', async () => {
    const fetchMock = vi.fn().mockImplementation(() => Promise.resolve(json({ error: { code: 'unavailable', message: 'agent unreachable' } }, 503)));
    vi.stubGlobal('fetch', fetchMock);
    await expect(processApi.status(target)).rejects.toMatchObject({ code: 'unavailable', status: 503 });
    expect(fetchMock).toHaveBeenCalledOnce();
  });

  it.each([[-1, 4], [0, 4], [3, 4], [4, 4], [4096, 4096], [1_000_000, 65_536], [Number.NaN, 65_536]])('bounds fixed agent log limit %s to %s without accepting a path', async (limit, expected) => {
    const fetchMock = vi.fn().mockImplementation(() => Promise.resolve(json(log())));
    vi.stubGlobal('fetch', fetchMock);
    await processApi.log(target, limit);
    expect(fetchMock.mock.calls[0][0]).toBe(`/api/v1/sources/lower%2Fa%20b/process/log?limit=${expected}`);
  });

  it('rejects oversized UTF-8 process log pages', async () => {
    vi.stubGlobal('fetch', vi.fn().mockImplementation(() => Promise.resolve(json(log('中'.repeat(Math.floor(MAX_PROCESS_LOG_BYTES / 3) + 1))))));
    await expect(processApi.log(target)).rejects.toMatchObject({ status: 502 });
  });

  it('allows historical revisions but validates the current history envelope and keeps pages scoped', async () => {
    const fetchMock = vi.fn().mockImplementation(() => Promise.resolve(json({ source_id: target.id, source_revision: 7, items: [{ ...job, binding_revision: 1, source_revision: 2 }], total: 1, limit: 20, offset: 20 })));
    vi.stubGlobal('fetch', fetchMock);
    expect((await processApi.history(target, 20)).items[0].binding_revision).toBe(1);
    expect(fetchMock.mock.calls[0][0]).toContain('operations?limit=20&offset=20');
    expect(processKeys.status(target)).not.toEqual(processKeys.status({ ...target, bindingRevision: 5 }));
    expect(processKeys.operation(target, 'first')).not.toEqual(processKeys.operation(target, 'second'));
    expect(processKeys.log(target, 4096)).not.toEqual(processKeys.log({ ...target, instanceId: 'b' }, 4096));
    expect(processKeys.history(target, 0)).not.toEqual(processKeys.history(target, 20));
  });

  it('cancels obsolete reads and does not retry an uncertain action timeout', async () => {
    const fetchMock = vi.fn((_url: string, init: RequestInit) => new Promise<Response>((_resolve, reject) => { init.signal?.addEventListener('abort', () => reject(new DOMException('aborted', 'AbortError')), { once: true }); }));
    vi.stubGlobal('fetch', fetchMock);
    const controller = new AbortController();
    const read = processApi.status(target, controller.signal);
    controller.abort();
    await expect(read).rejects.toMatchObject({ name: 'AbortError' });
    vi.useFakeTimers();
    const assertion = expect(processApi.act(target, 'opaque-r9', 'start', 'request/a')).rejects.toMatchObject({ status: 0, code: 'process_timeout' });
    await vi.advanceTimersByTimeAsync(PROCESS_TIMEOUT_MS);
    await assertion;
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });
});
