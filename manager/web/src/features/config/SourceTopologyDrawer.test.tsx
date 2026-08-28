import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { Group, Source } from '../../api/types';
import SourceTopologyDrawer from './SourceTopologyDrawer';

const groups: Group[] = ['fallback', 'device-a', 'device-b'].map((id) => ({ id, name: id, defaultSourceId: '', selectionRevision: 1, sourceCount: 1, goodPoints: 0, badPoints: 0, totalPoints: 2, pendingCommandCount: 0, status: 'good' }));
const source: Source = { id: 'lower-1', name: '下级 1', groupId: 'fallback', groupBindings: [{ groupId: 'device-a', portId: '/dev/ttyS0', deviceId: '1' }], endpoint: 'opc.tcp://lower:4840', securityPolicy: 'None', securityMode: 'None', username: 'operator', hasPassword: true, enabled: true, connectionState: 'Connected', sessionState: 'Active', subscriptionState: 'Active', catalogState: 'Ready', reconnectCount: 0, monitoredCount: 4, goodPoints: 4, badPoints: 0, totalPoints: 4, revision: 2 };
const topology = { source_id: 'lower-1', group_id: 'fallback', revision: 2, supported: true, schema_version: 1, catalog_state: 'Ready', calculation_count: 2, cross_device_calculation_count: 1, unassigned_point_count: 1, ports: ['/dev/ttyS0', '/dev/ttyS1'].map((port, index) => ({ port_id: port, port_name: port, point_count: 2, devices: [{ device_id: '1', device_address: 1, name: `设备 ${index + 1}`, point_count: 2, calculation_count: 1, group_id: index === 0 ? 'device-a' : 'fallback', bound: index === 0 }] })) };

function json(body: unknown, status = 200) { return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } }); }
function setupFetch(online = false, conflict = false) {
  const fetchMock = vi.fn().mockImplementation((url: string, init?: RequestInit) => {
    if (init?.method === 'PUT') return Promise.resolve(conflict ? json({ error: { code: 'conflict', message: 'online session exists' } }, 409) : json({ ...source, revision: 3 }));
    if (url.endsWith('/topology')) return Promise.resolve(json(topology));
    if (url.endsWith('/sessions')) return Promise.resolve(json({ items: online ? [{ id: 's-1', routes: {} }] : [] }));
    return Promise.resolve(json({ items: [] }));
  });
  vi.stubGlobal('fetch', fetchMock);
  return fetchMock;
}
function mount() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(<MemoryRouter><QueryClientProvider client={client}><SourceTopologyDrawer source={source} groups={groups} onClose={() => undefined} onSaved={() => undefined} /></QueryClientProvider></MemoryRouter>);
}
afterEach(() => vi.unstubAllGlobals());

describe('SourceTopologyDrawer', () => {
  it('maps identical device addresses on different ports independently and preserves credentials', async () => {
    const fetchMock = setupFetch();
    mount();
    const first = await screen.findByLabelText('/dev/ttyS0 设备 1 逻辑组');
    const second = screen.getByLabelText('/dev/ttyS1 设备 1 逻辑组');
    expect(first).toHaveValue('device-a');
    expect(second).toHaveValue('');
    expect(screen.getByText(/分步保存第一个来源后/)).toBeInTheDocument();
    const user = userEvent.setup();
    await user.selectOptions(second, 'device-b');
    await user.click(screen.getByRole('button', { name: '保存设备映射' }));
    await waitFor(() => expect(fetchMock.mock.calls.some(([, init]) => init?.method === 'PUT')).toBe(true));
    const [, init] = fetchMock.mock.calls.find(([, options]) => options?.method === 'PUT')! as [string, RequestInit];
    expect(init.headers).toMatchObject({ 'If-Match': '"2"' });
    const body = JSON.parse(String(init.body));
    expect(body).toMatchObject({ group_id: 'fallback', username: 'operator', group_bindings: [{ group_id: 'device-a', port_id: '/dev/ttyS0', device_id: '1' }, { group_id: 'device-b', port_id: '/dev/ttyS1', device_id: '1' }] });
    expect(body).not.toHaveProperty('password');
    expect(fetchMock.mock.calls.every(([url]) => !String(url).includes('/points'))).toBe(true);
  });

  it('does not disconnect online sessions and blocks mapping saves while they exist', async () => {
    const fetchMock = setupFetch(true);
    mount();
    await userEvent.setup().selectOptions(await screen.findByLabelText('/dev/ttyS1 设备 1 逻辑组'), 'device-b');
    expect(screen.getByRole('button', { name: '保存设备映射' })).toBeDisabled();
    expect(screen.getByText(/映射保存已锁定：1 个在线 Session/)).toBeInTheDocument();
    expect(fetchMock.mock.calls.every(([, init]) => !init?.method)).toBe(true);
  });

  it('keeps the draft and explains HTTP 409 when a session starts during the save', async () => {
    setupFetch(false, true);
    mount();
    const select = await screen.findByLabelText('/dev/ttyS1 设备 1 逻辑组');
    const user = userEvent.setup();
    await user.selectOptions(select, 'device-b');
    await user.click(screen.getByRole('button', { name: '保存设备映射' }));
    expect(await screen.findByText(/保存冲突：可能存在在线 Session/)).toBeInTheDocument();
    expect(select).toHaveValue('device-b');
  });
});
