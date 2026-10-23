import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { Group, Source } from '../../api/types';
import { SourceFormModal, validOpcEndpoint } from './SourceFormModal';
import { api } from '../../api/client';

const group: Group = { id: 'fire', name: 'Fire', defaultSourceId: '', selectionRevision: 1, sourceCount: 1, goodPoints: 0, badPoints: 0, totalPoints: 0, pendingCommandCount: 0, status: 'good' };
const source: Source = {
  id: 'rcd-1', name: 'RCD 1', groupId: 'fire', endpoint: 'opc.tcp://127.0.0.1:4841',
  securityPolicy: 'Basic256Sha256', securityMode: 'SignAndEncrypt', username: 'operator', hasPassword: true,
  enabled: true, connectionState: 'Connected', sessionState: 'Active', subscriptionState: 'Active',
  catalogState: 'Ready', reconnectCount: 0, monitoredCount: 1, goodPoints: 1, badPoints: 0, totalPoints: 1, revision: 2,
};

afterEach(() => { vi.unstubAllGlobals(); vi.restoreAllMocks(); });

describe('SourceFormModal', () => {
  it('does not close a new editor when an earlier closed editor finishes saving', async () => {
    let finish!: (saved: Source) => void;
    const pending = new Promise<Source>((resolve) => { finish = resolve; });
    const update = vi.spyOn(api, 'updateSource').mockReturnValue(pending);
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    const invalidate = vi.spyOn(client, 'invalidateQueries');
    const onClose = vi.fn();
    const onSaved = vi.fn();
    const form = (open: boolean, target: Source) => <QueryClientProvider client={client}><SourceFormModal open={open} source={target} groups={[group]} onClose={onClose} onSaved={onSaved} /></QueryClientProvider>;
    const view = render(form(true, source));
    const user = userEvent.setup();
    await user.click(screen.getByRole('button', { name: '保存并应用' }));
    await waitFor(() => expect(update).toHaveBeenCalledOnce());
    expect(screen.getByLabelText('显示名称')).toBeDisabled();
    expect(screen.getByRole('button', { name: '关闭（请求继续执行）' })).toBeEnabled();
    view.rerender(form(false, source));
    view.rerender(form(true, { ...source, id: 'rcd-2', name: 'RCD 2' }));
    await user.clear(screen.getByLabelText('显示名称'));
    await user.type(screen.getByLabelText('显示名称'), '新的未保存草稿');
    await act(async () => { finish(source); await pending; });
    await waitFor(() => expect(invalidate).toHaveBeenCalledTimes(5));
    expect(onClose).not.toHaveBeenCalled();
    expect(onSaved).not.toHaveBeenCalled();
    expect(screen.getByRole('dialog')).toBeInTheDocument();
    expect(screen.getByLabelText('显示名称')).toHaveValue('新的未保存草稿');
  });

  it('accepts a valid OPC endpoint path', () => {
    expect(validOpcEndpoint('opc.tcp://10.0.0.5:4840/UA/RcdServer')).toBe(true);
  });

  it('does not erase edits when background group data refreshes', async () => {
    const view = render(
      <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } })}>
        <SourceFormModal open source={source} groups={[group]} onClose={() => undefined} onSaved={() => undefined} />
      </QueryClientProvider>,
    );
    const user = userEvent.setup();
    const endpoint = screen.getByLabelText('OPC UA Endpoint');
    const password = screen.getByLabelText('新密码（留空不修改）');
    await user.clear(endpoint);
    await user.type(endpoint, 'opc.tcp://10.0.0.5:4840/UA/RcdServer');
    await user.type(password, 'temporary-secret');
    view.rerender(
      <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } })}>
        <SourceFormModal open source={{ ...source }} groups={[{ ...group }]} onClose={() => undefined} onSaved={() => undefined} />
      </QueryClientProvider>,
    );
    expect(endpoint).toHaveValue('opc.tcp://10.0.0.5:4840/UA/RcdServer');
    expect(password).toHaveValue('temporary-secret');
  });

  it('keeps the stored username and sends an explicit password clear', async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify({
      id: source.id, group_id: source.groupId, name: source.name, endpoint: source.endpoint,
      security_policy: source.securityPolicy, security_mode: source.securityMode, username: source.username,
      has_password: false, enabled: true, catalog_state: 'Ready', revision: 3,
    }), { status: 200, headers: { 'Content-Type': 'application/json' } }));
    vi.stubGlobal('fetch', fetchMock);
    render(
      <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } })}>
        <SourceFormModal open source={source} groups={[group]} onClose={() => undefined} onSaved={() => undefined} />
      </QueryClientProvider>,
    );

    expect(screen.getByLabelText('用户名（可选）')).toHaveValue('operator');
    const user = userEvent.setup();
    await user.click(screen.getByRole('checkbox', { name: '清除已保存的下级密码' }));
    await user.click(screen.getByRole('button', { name: '保存并应用' }));

    await waitFor(() => expect(fetchMock).toHaveBeenCalledOnce());
    const [, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(JSON.parse(String(init.body))).toMatchObject({ username: 'operator', password: '' });
  });

  it('preserves mappings and the editing revision across background source refreshes', async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify({ ...source, revision: 4 }), { status: 200, headers: { 'Content-Type': 'application/json' } }));
    vi.stubGlobal('fetch', fetchMock);
    const mapped = { ...source, groupBindings: [{ portId: '/dev/ttyS0', deviceId: '1', groupId: 'fire' }] };
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    const view = render(<QueryClientProvider client={client}><SourceFormModal open source={mapped} groups={[group]} onClose={() => undefined} onSaved={() => undefined} /></QueryClientProvider>);
    view.rerender(<QueryClientProvider client={client}><SourceFormModal open source={{ ...mapped, revision: 3, groupBindings: [] }} groups={[group]} onClose={() => undefined} onSaved={() => undefined} /></QueryClientProvider>);
    await userEvent.setup().click(screen.getByRole('button', { name: '保存并应用' }));
    await waitFor(() => expect(fetchMock).toHaveBeenCalledOnce());
    const [, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(init.headers).toMatchObject({ 'If-Match': '"2"' });
    expect(JSON.parse(String(init.body))).toHaveProperty('group_bindings', [{ port_id: '/dev/ttyS0', device_id: '1', group_id: 'fire' }]);
  });
});
