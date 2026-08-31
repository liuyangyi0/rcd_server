import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { Group, Source, UaSession } from '../../api/types';
import { SourceSwitchDialog } from './SourceSwitchDialog';

const group: Group = { id: 'fire', name: 'fire', defaultSourceId: 'rcd-1', selectionRevision: 4, sourceCount: 2, goodPoints: 1, badPoints: 0, totalPoints: 1, pendingCommandCount: 0, status: 'good' };
const baseSource = { name: '', groupId: 'fire', securityPolicy: 'None', securityMode: 'None', hasPassword: false, enabled: true, connectionState: 'Connected', sessionState: 'Active', subscriptionState: 'Running', catalogState: 'Ready', reconnectCount: 0, monitoredCount: 1, goodPoints: 1, badPoints: 0, totalPoints: 1, revision: 1 };
const sources: Source[] = [
  { ...baseSource, id: 'rcd-1', name: 'rcd-1', endpoint: 'opc.tcp://10.0.0.1:4840/' },
  { ...baseSource, id: 'rcd-2', name: 'rcd-2', endpoint: 'opc.tcp://10.0.0.2:4840/' },
];
const sessions: UaSession[] = [{ id: 'session-1', clientName: 'SCADA', remoteAddress: '10.0.1.1', selectionRevision: 9, selectedSources: { fire: 'rcd-1' }, routeRevisions: { fire: 9 } }];

afterEach(() => vi.unstubAllGlobals());

describe('SourceSwitchDialog', () => {
  it('does not rebase an open selection draft onto a newer background revision', async () => {
    const client = new QueryClient();
    const view = render(<QueryClientProvider client={client}><SourceSwitchDialog open mode="session" group={group} sources={sources} sessions={sessions} initialSessionId="session-1" onClose={() => undefined} /></QueryClientProvider>);
    await userEvent.setup().click(screen.getByRole('checkbox', { name: '确认已核对目标下级状态' }));
    view.rerender(<QueryClientProvider client={client}><SourceSwitchDialog open mode="session" group={group} sources={sources} sessions={[{ ...sessions[0], routeRevisions: { fire: 10 } }]} initialSessionId="session-1" onClose={() => undefined} /></QueryClientProvider>);
    expect(screen.getByRole('button', { name: '确认切换' })).toBeDisabled();
    expect(screen.getByText(/该逻辑组的选源版本已变化/)).toBeInTheDocument();
  });

  it('includes mapped members but never offers a source from an unrelated group', () => {
    const mapped = { ...sources[1], groupId: 'fallback', groupBindings: [{ groupId: 'fire', portId: '/dev/ttyS1', deviceId: '1' }], availableGroupIds: ['fire'] };
    const unrelated = { ...sources[1], id: 'unrelated', groupId: 'other' };
    render(<QueryClientProvider client={new QueryClient()}><SourceSwitchDialog open mode="session" group={group} sources={[sources[0], mapped, unrelated]} sessions={sessions} initialSessionId="session-1" onClose={() => undefined} /></QueryClientProvider>);
    expect(screen.getByRole('radio', { name: /rcd-2/ })).toBeEnabled();
    expect(screen.queryByRole('radio', { name: /unrelated/ })).not.toBeInTheDocument();
    expect(screen.getByText(/不是单点切换/)).toBeInTheDocument();
  });

  it('rejects a mapped group until that group has been discovered', () => {
    const mapped = { ...sources[1], availableGroupIds: ['another-group'] };
    render(<QueryClientProvider client={new QueryClient()}><SourceSwitchDialog open mode="session" group={group} sources={[sources[0], mapped]} sessions={sessions} initialSessionId="session-1" onClose={() => undefined} /></QueryClientProvider>);
    expect(screen.getByRole('radio', { name: /rcd-2/ })).toBeDisabled();
    expect(screen.getByText(/未发现本组目录/)).toBeInTheDocument();
  });

  it('keeps the original target and disables submit when that Session ends', async () => {
    const fetchMock = vi.fn();
    vi.stubGlobal('fetch', fetchMock);
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    const view = render(
      <QueryClientProvider client={client}>
        <SourceSwitchDialog open mode="session" group={group} sources={sources} sessions={sessions} initialSessionId="session-1" onClose={() => undefined} />
      </QueryClientProvider>,
    );
    const user = userEvent.setup();
    await user.click(screen.getByRole('checkbox', { name: '确认已核对目标下级状态' }));
    view.rerender(
      <QueryClientProvider client={client}>
        <SourceSwitchDialog open mode="session" group={group} sources={sources} sessions={[{ ...sessions[0], id: 'session-2' }]} initialSessionId="session-2" onClose={() => undefined} />
      </QueryClientProvider>,
    );
    expect(screen.getByText('目标 Session 已结束')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: '确认切换' })).toBeDisabled();
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it('submits a session route revision through If-Match', async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify({ group_id: 'fire', source_id: 'rcd-2', revision: 10 }), { status: 200, headers: { 'Content-Type': 'application/json' } }));
    vi.stubGlobal('fetch', fetchMock);
    const onClose = vi.fn();
    render(
      <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } })}>
        <SourceSwitchDialog open mode="session" group={group} sources={sources} sessions={sessions} initialSessionId="session-1" onClose={onClose} />
      </QueryClientProvider>,
    );
    const user = userEvent.setup();
    await user.click(screen.getByRole('radio', { name: /rcd-2/ }));
    await user.click(screen.getByRole('checkbox', { name: '确认已核对目标下级状态' }));
    await user.click(screen.getByRole('button', { name: '确认切换' }));

    await waitFor(() => expect(fetchMock).toHaveBeenCalledOnce());
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe('/api/v1/sessions/session-1/groups/fire/selection');
    expect(init.headers).toMatchObject({ 'If-Match': '"9"' });
    expect(JSON.parse(String(init.body))).toEqual({ source_id: 'rcd-2' });
    expect(onClose).toHaveBeenCalled();
  });

  it('does not allow a source whose catalog has not completed discovery', () => {
    const discovering = { ...sources[1], catalogState: 'Discovering', connectionState: 'Disconnected' };
    render(
      <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } })}>
        <SourceSwitchDialog open mode="session" group={group} sources={[sources[0], discovering]} sessions={sessions} initialSessionId="session-1" onClose={() => undefined} />
      </QueryClientProvider>,
    );
    expect(screen.getByRole('radio', { name: /rcd-2/ })).toBeDisabled();
    expect(screen.getByText(/不可选 · Discovering/)).toBeInTheDocument();
  });

  it('allows the global default source to be cleared', async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify({ id: 'fire', default_source_id: '', revision: 5 }), { status: 200, headers: { 'Content-Type': 'application/json' } }));
    vi.stubGlobal('fetch', fetchMock);
    render(
      <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } })}>
        <SourceSwitchDialog open mode="default" group={group} sources={sources} sessions={sessions} onClose={() => undefined} />
      </QueryClientProvider>,
    );
    const user = userEvent.setup();
    await user.click(screen.getByRole('radio', { name: /未设置默认源/ }));
    await user.click(screen.getByRole('checkbox', { name: '确认已核对目标下级状态' }));
    await user.click(screen.getByRole('button', { name: '确认设置' }));

    await waitFor(() => expect(fetchMock).toHaveBeenCalledOnce());
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe('/api/v1/groups/fire/default-source');
    expect(init.headers).toMatchObject({ 'If-Match': '"4"' });
    expect(JSON.parse(String(init.body))).toEqual({ source_id: '' });
  });

  it('never invents a target when no explicit Session was provided', () => {
    render(<QueryClientProvider client={new QueryClient()}><SourceSwitchDialog open mode="session" group={group} sources={sources} sessions={sessions} onClose={() => undefined} /></QueryClientProvider>);
    expect(screen.getByText('目标 Session 已结束')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: '确认切换' })).toBeDisabled();
  });

  it('captures a fresh target and revision only after closing and reopening', async () => {
    const client = new QueryClient();
    const renderDialog = (open: boolean, nextSessions: UaSession[], initialSessionId: string) => <QueryClientProvider client={client}><SourceSwitchDialog open={open} mode="session" group={group} sources={sources} sessions={nextSessions} initialSessionId={initialSessionId} onClose={() => undefined} /></QueryClientProvider>;
    const view = render(renderDialog(true, sessions, 'session-1'));
    await userEvent.setup().click(screen.getByRole('checkbox', { name: '确认已核对目标下级状态' }));
    const replacement = [{ ...sessions[0], id: 'session-2', clientName: 'Replacement', routeRevisions: { fire: 12 } }];
    view.rerender(renderDialog(false, replacement, 'session-2'));
    view.rerender(renderDialog(true, replacement, 'session-2'));
    expect(screen.getByText(/Replacement/)).toBeInTheDocument();
    expect(screen.getByRole('checkbox', { name: '确认已核对目标下级状态' })).not.toBeChecked();
    expect(screen.queryByText(/选源版本已变化/)).not.toBeInTheDocument();
    await userEvent.setup().click(screen.getByRole('checkbox', { name: '确认已核对目标下级状态' }));
    expect(screen.getByRole('button', { name: '确认切换' })).toBeEnabled();
  });
});
