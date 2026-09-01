import { afterEach, describe, expect, it, vi } from 'vitest';
import { calculationApi } from './calculations';

function json(body: unknown): Response {
  return new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } });
}

afterEach(() => vi.unstubAllGlobals());

describe('calculation API contract', () => {
  it('maps server paging and preserves legacy KKS bytes', async () => {
    const fetchMock = vi.fn().mockResolvedValue(json({
      items: [{ node_id: 'LEGACY ', data_type: 'UInt32', expression: 'A + 1', value: 2, status_code: 0, dependency_count: 1, config_revision: 7, default_route_revision: 9 }],
      total: 1, limit: 50, offset: 50,
    }));
    vi.stubGlobal('fetch', fetchMock);
    const page = await calculationApi.results({ page: 2, pageSize: 50, query: 'LEGACY', status: 'bad', kind: 'UInt32' });
    expect(fetchMock.mock.calls[0][0]).toBe('/api/v1/calculations/results?limit=50&offset=50&q=LEGACY&status=bad&kind=UInt32');
    expect(page.items[0]).toMatchObject({ nodeId: 'LEGACY ', dataType: 'UInt32', dependencyCount: 1, configRevision: 7, defaultRouteRevision: 9 });
  });

  it('pins draft/import revisions and maps immutable version diffs', async () => {
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(json({ revision: 3, content: '[[rules]]', valid: false, problems: [] }))
      .mockResolvedValueOnce(json({ revision: 4, content: '[[rules]]', valid: true, problems: [] }))
      .mockResolvedValueOnce(json({ items: [{ id: 2, revision: 8, content_hash: 'abc', rule_count: 6683, added_count: 2, removed_count: 1, changed_count: 4, state: 'active' }], total: 1, limit: 50, offset: 0 }));
    vi.stubGlobal('fetch', fetchMock);
    await calculationApi.saveDraft(2, '[[rules]]');
    await calculationApi.importLower(3, 'lower-a', 11);
    const versions = await calculationApi.versions({ page: 1, pageSize: 50 });
    const [, saveInit] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(saveInit.headers).toMatchObject({ 'If-Match': '"2"' });
    const [importURL, importInit] = fetchMock.mock.calls[1] as [string, RequestInit];
    expect(importURL).toBe('/api/v1/calculations/config/import-lower');
    expect(importInit.headers).toMatchObject({ 'If-Match': '"3"' });
    expect(JSON.parse(String(importInit.body))).toEqual({ source_id: 'lower-a', source_revision: 11 });
    expect(versions.items[0]).toMatchObject({ ruleCount: 6683, addedCount: 2, removedCount: 1, changedCount: 4 });
  });
});
