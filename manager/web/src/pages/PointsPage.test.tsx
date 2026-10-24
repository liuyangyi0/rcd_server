import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { api } from '../api/client';
import { queryKeys } from '../api/queries';
import type { Group, ListParams, PageResult, Point, Source, UaSession } from '../api/types';
import { useEventStream } from '../hooks/useEventStream';
import { TestEventSource } from '../test/eventSource';
import PointsPage from './PointsPage';

const group: Group = { id: 'device-a', name: 'Device A', defaultSourceId: 'lower-a', selectionRevision: 1, sourceCount: 2, goodPoints: 2, badPoints: 0, totalPoints: 2, pendingCommandCount: 0, status: 'good' };
const sources: Source[] = ['lower-a', 'lower-b'].map((id) => ({ id, name: id, groupId: 'device-a', endpoint: '', securityPolicy: 'None', securityMode: 'None', hasPassword: false, enabled: true, connectionState: 'Connected', sessionState: 'Active', subscriptionState: 'Active', catalogState: 'Ready', reconnectCount: 0, monitoredCount: 2, goodPoints: 2, badPoints: 0, totalPoints: 2, revision: 1 }));
const sessions: UaSession[] = [
  { id: 'session-1', clientName: 'First', remoteAddress: '', selectedSources: { 'device-a': 'lower-a' }, routeRevisions: { 'device-a': 1 }, selectionRevision: 1 },
  { id: 'session-2', clientName: 'Second', remoteAddress: '', selectedSources: { 'device-a': 'lower-b' }, routeRevisions: { 'device-a': 2 }, selectionRevision: 2 },
];
const compareRoute = '/points?group=device-a&compare=1&left=lower-a&right=lower-b';

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (reason: unknown) => void;
  const promise = new Promise<T>((done, fail) => { resolve = done; reject = fail; });
  return { promise, resolve, reject };
}

function pointPage(params: ListParams): PageResult<Point> {
  const sourceId = params.routeSourceId || (params.sessionId === 'session-2' ? 'lower-b' : 'lower-a');
  const names = params.query ? ['A_VALUE'] : ['A_VALUE', 'CALC_A'];
  return { items: names.map((nodeId) => ({ nodeId, browseName: nodeId, nodeClass: 'Variable', groupId: 'device-a', selectedSourceId: sourceId, dataType: 'Double', value: sourceId === 'lower-a' ? 11 : 101, statusCode: 'Good', sourcePresent: true, deleted: false })), page: params.page ?? 1, pageSize: 50, total: names.length };
}

function EventSubscriber() {
  useEventStream();
  return null;
}

function setup(route: string, events = false) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
  client.setQueryData(queryKeys.groups, [group]);
  client.setQueryData(queryKeys.sources(), sources);
  client.setQueryData(queryKeys.sessions, sessions);
  vi.spyOn(api, 'groups').mockResolvedValue([group]);
  vi.spyOn(api, 'sources').mockResolvedValue(sources);
  const sessionList = vi.spyOn(api, 'sessions').mockResolvedValue(sessions);
  if (events) vi.stubGlobal('EventSource', TestEventSource);
  render(<QueryClientProvider client={client}><MemoryRouter initialEntries={[route]}><PointsPage />{events ? <EventSubscriber /> : null}</MemoryRouter></QueryClientProvider>);
  return { client, sessionList, user: userEvent.setup() };
}

afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  TestEventSource.instances = [];
});

describe('point comparison rendered transitions', () => {
  it.each(['/points?group=device-a&page=3', compareRoute + '&page=3'])('keeps pagination available on an empty stale page: %s', async (route) => {
    const points = vi.spyOn(api, 'points').mockImplementation(async (params) => ({ ...pointPage(params), items: params.page === 3 ? [] : pointPage(params).items }));
    const { user } = setup(route);
    await screen.findByRole('button', { name: '返回末页' });
    expect(screen.getByText('3 / 1 页')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: '返回末页' }));
    await screen.findByText('A_VALUE');
    expect(screen.getByText('1 / 1 页')).toBeInTheDocument();
    expect(points.mock.calls.some(([params]) => params.page === 1)).toBe(true);
  });

  it('does not render false missing points while only one filtered side has returned', async () => {
    const right = deferred<PageResult<Point>>();
    const points = vi.spyOn(api, 'points').mockImplementation(async (params) => params.query && params.routeSourceId === 'lower-b' ? right.promise : pointPage(params));
    const { user } = setup(compareRoute);
    await screen.findByText('CALC_A');
    await user.type(screen.getByPlaceholderText('搜索 Node ID 或 BrowseName'), 'A_VALUE');
    await user.click(screen.getByRole('button', { name: '搜索' }));
    await waitFor(() => expect(points.mock.calls.filter(([params]) => params.query === 'A_VALUE')).toHaveLength(2));
    expect(screen.getByText('正在加载同一查询的双来源点位…')).toBeInTheDocument();
    expect(screen.queryByRole('table')).not.toBeInTheDocument();
    expect(screen.queryByText('点位不存在')).not.toBeInTheDocument();
    await act(async () => { right.resolve(pointPage({ query: 'A_VALUE', routeSourceId: 'lower-b' })); });
    await screen.findByText('A_VALUE');
    expect(screen.queryByText('CALC_A')).not.toBeInTheDocument();
    expect(screen.queryByText('点位不存在')).not.toBeInTheDocument();
    const row = within(screen.getByRole('row', { name: /A_VALUE/ }));
    expect(row.getAllByRole('cell')[1]).toHaveTextContent('11');
    expect(row.getAllByRole('cell')[3]).toHaveTextContent('101');
  });

  it('shows a retryable error rather than an empty or partial comparison when one read fails', async () => {
    const right = deferred<PageResult<Point>>();
    let failRight = true;
    const points = vi.spyOn(api, 'points').mockImplementation(async (params) => params.routeSourceId === 'lower-b' && failRight ? right.promise : pointPage(params));
    const { user } = setup(compareRoute);
    await waitFor(() => expect(points).toHaveBeenCalledTimes(2));
    await act(async () => { right.reject(new Error('右侧查询失败')); });
    await screen.findByText('右侧查询失败');
    expect(screen.queryByRole('table')).not.toBeInTheDocument();
    expect(screen.queryByText('点位不存在')).not.toBeInTheDocument();
    failRight = false;
    await user.click(screen.getByRole('button', { name: '重试' }));
    await screen.findByText('A_VALUE');
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });
});

describe('Session point view lifecycle', () => {
  it('removes ended Session data on session.changed without needing lower samples and never changes the target implicitly', async () => {
    const points = vi.spyOn(api, 'points').mockImplementation(async (params) => pointPage(params));
    const { sessionList, user } = setup('/points?group=device-a&view=session&session=session-2', true);
    await screen.findByRole('table');
    expect(screen.getByRole('combobox', { name: /^Session/ })).toHaveValue('session-2');
    sessionList.mockResolvedValue([sessions[0]]);
    act(() => TestEventSource.instances[0].emit('session.changed', { closed_id: 'session-2' }));
    await screen.findByText('目标 Session 已结束');
    expect(screen.getByRole('combobox', { name: /^Session/ })).toHaveValue('session-2');
    expect(screen.queryByRole('table')).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: '刷新' })).toBeDisabled();
    expect(screen.queryByRole('link', { name: '导出当前结果' })).not.toBeInTheDocument();
    expect(points.mock.calls.every(([params]) => params.sessionId === 'session-2')).toBe(true);
    await user.selectOptions(screen.getByRole('combobox', { name: /^Session/ }), 'session-1');
    await screen.findByRole('table');
    expect(screen.queryByText('目标 Session 已结束')).not.toBeInTheDocument();
    expect(points.mock.calls.some(([params]) => params.sessionId === 'session-1')).toBe(true);
  });

  it('keeps an ended Session URL as an ended view instead of requesting a default route', async () => {
    const points = vi.spyOn(api, 'points').mockImplementation(async (params) => pointPage(params));
    setup('/points?group=device-a&view=session&session=already-closed');
    expect(screen.getByText('目标 Session 已结束')).toBeInTheDocument();
    expect(screen.getByRole('combobox', { name: /^Session/ })).toHaveValue('already-closed');
    expect(points).not.toHaveBeenCalled();
  });
});
