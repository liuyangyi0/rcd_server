import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter, useLocation, useNavigate } from 'react-router-dom';
import { afterEach, expect, it, vi } from 'vitest';
import { api } from '../api/client';
import { queryKeys } from '../api/queries';
import type { CommandAudit, PageResult } from '../api/types';
import CommandsPage from './CommandsPage';
import { useEventStream } from '../hooks/useEventStream';
import { TestEventSource } from '../test/eventSource';

vi.mock('../features/commands/CommandAuditHealth', () => ({ default: () => null }));
vi.mock('../features/commands/SerialCommandAttempts', () => ({ default: () => null }));

const clients: QueryClient[] = [];
afterEach(() => { clients.forEach((client) => client.clear()); clients.length = 0; vi.restoreAllMocks(); vi.unstubAllGlobals(); TestEventSource.instances = []; });

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (reason: unknown) => void;
  const promise = new Promise<T>((done, fail) => { resolve = done; reject = fail; });
  return { promise, resolve, reject };
}

function commandPage(result = 'Waiting'): PageResult<CommandAudit> {
  return { items: [{ id: 'command-1', createdAt: '2026-10-08T09:06:40Z', groupId: 'group-1', sourceId: 'source-1',
    port: 'ttyWCH5', nodeId: 'ttyWCH5_Send', operation: 'Write', request: '3F090330001A04', result,
    sessionId: 'session-1', statusCode: '0x00000000' }], page: 1, pageSize: 50, total: 1 };
}

function EventSubscriber() { useEventStream(); return null; }

it('shows and filters queued commands separately from awaiting an ACK', async () => {
  vi.spyOn(api, 'commands').mockResolvedValue(commandPage('Queued'));
  const { user } = setup();
  const row = await screen.findByRole('row', { name: /ttyWCH5_Send/ });
  expect(row).toHaveTextContent('排队待发送');
  await user.selectOptions(screen.getByLabelText('结果'), 'Queued');
  await waitFor(() => expect(api.commands).toHaveBeenLastCalledWith(expect.objectContaining({ status: 'Queued' })));
});

function HistoryControls() {
  const navigate = useNavigate();
  const location = useLocation();
  return <><button type="button" onClick={() => navigate(-1)}>测试后退</button><button type="button" onClick={() => navigate(1)}>测试前进</button><output data-testid="command-search-location">{location.search}</output></>;
}

function setup(events = false, entry = '/commands') {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
  clients.push(client);
  vi.spyOn(api, 'groups').mockResolvedValue([]);
  vi.spyOn(api, 'sources').mockResolvedValue([]);
  if (events) vi.stubGlobal('EventSource', TestEventSource);
  render(<QueryClientProvider client={client}><MemoryRouter initialEntries={[entry]}><CommandsPage /><HistoryControls />{events ? <EventSubscriber /> : null}</MemoryRouter></QueryClientProvider>);
  return { client, user: userEvent.setup() };
}

it('keeps the same rows and table mounted during a slow background refresh and updates the result in place', async () => {
  const first = deferred<PageResult<CommandAudit>>();
  const next = deferred<PageResult<CommandAudit>>();
  vi.spyOn(api, 'commands').mockReturnValueOnce(first.promise).mockReturnValueOnce(next.promise);
  const { client } = setup();
  expect(screen.getByText('正在加载…')).toBeInTheDocument();
  await act(async () => { first.resolve(commandPage()); });
  const table = await screen.findByRole('table');
  const row = screen.getByRole('row', { name: /ttyWCH5_Send/ });
  const scroll = table.parentElement!;
  scroll.scrollLeft = 120;
  act(() => { void client.invalidateQueries({ queryKey: queryKeys.allCommands }); });
  await waitFor(() => expect(scroll).toHaveAttribute('aria-busy', 'true'));
  expect(screen.getByRole('table')).toBe(table);
  expect(screen.getByRole('row', { name: /ttyWCH5_Send/ })).toBe(row);
  expect(scroll).not.toHaveClass('is-refreshing');
  expect(scroll.scrollLeft).toBe(120);
  expect(screen.queryByText('正在加载…')).not.toBeInTheDocument();
  await act(async () => { next.resolve(commandPage('Received')); });
  await waitFor(() => expect(scroll).toHaveAttribute('aria-busy', 'false'));
  expect(screen.getByRole('row', { name: /ttyWCH5_Send/ })).toBe(row);
  expect(row).toHaveTextContent('成功');
});

it('keeps cached records on a background error, labels them stale, and clears the notice after retry', async () => {
  const next = deferred<PageResult<CommandAudit>>();
  vi.spyOn(api, 'commands').mockResolvedValueOnce(commandPage()).mockReturnValueOnce(next.promise).mockResolvedValue(commandPage('Received'));
  const { client, user } = setup();
  const table = await screen.findByRole('table');
  act(() => { void client.invalidateQueries({ queryKey: queryKeys.allCommands }); });
  await act(async () => { next.reject(new Error('temporary network error')); });
  await screen.findByText(/保留上次读取的命令记录/);
  expect(screen.getByRole('table')).toBe(table);
  expect(screen.queryByText('无法获取数据')).not.toBeInTheDocument();
  await user.click(screen.getByRole('button', { name: '刷新' }));
  await waitFor(() => expect(screen.queryByText(/保留上次读取的命令记录/)).not.toBeInTheDocument());
  expect(screen.getByRole('table')).toBe(table);
});

it('shows a full error state only when no records have ever loaded', async () => {
  vi.spyOn(api, 'commands').mockRejectedValue(new Error('initial network error'));
  setup();
  await screen.findByText('无法获取数据');
  expect(screen.queryByRole('table')).not.toBeInTheDocument();
  expect(screen.queryByText(/保留上次读取的命令记录/)).not.toBeInTheDocument();
});

it('coalesces command events into the existing read instead of restarting it repeatedly', async () => {
  const next = deferred<PageResult<CommandAudit>>();
  const commands = vi.spyOn(api, 'commands').mockResolvedValueOnce(commandPage()).mockReturnValueOnce(next.promise).mockResolvedValue(commandPage('Received'));
  setup(true);
  const table = await screen.findByRole('table');
  act(() => { TestEventSource.instances[0].emit('command.changed'); });
  await waitFor(() => expect(commands).toHaveBeenCalledTimes(2));
  act(() => { for (let i = 0; i < 20; i++) TestEventSource.instances[0].emit('command.changed'); });
  expect(commands).toHaveBeenCalledTimes(2);
  await act(async () => { next.reject(new Error('read failed during event burst')); });
  await screen.findByText(/保留上次读取的命令记录/);
  expect(screen.getByRole('table')).toBe(table);
  act(() => { TestEventSource.instances[0].emit('command.changed'); });
  await waitFor(() => expect(commands).toHaveBeenCalledTimes(3));
  await waitFor(() => expect(screen.queryByText(/保留上次读取的命令记录/)).not.toBeInTheDocument());
});

it('keeps pagination on an empty out-of-range page and can return to the last valid page', async () => {
  vi.spyOn(api, 'commands').mockImplementation(async (params) => ({
    ...commandPage('Received'), items: params.page === 9 ? [] : commandPage('Received').items,
    page: params.page ?? 1, total: 80,
  }));
  const { user } = setup(false, '/commands?page=9');
  await screen.findByText('暂无命令记录');
  expect(screen.getByText('9 / 2 页')).toBeInTheDocument();
  await user.click(screen.getByRole('button', { name: '返回末页' }));
  await screen.findByRole('row', { name: /ttyWCH5_Send/ });
  expect(api.commands).toHaveBeenLastCalledWith(expect.objectContaining({ page: 2 }));
  expect(screen.getByText('2 / 2 页')).toBeInTheDocument();
});

it('synchronizes the search editor with browser back and forward navigation', async () => {
  vi.spyOn(api, 'commands').mockResolvedValue(commandPage());
  const { user } = setup(false, '/commands?q=first');
  await screen.findByRole('table');
  const search = screen.getByPlaceholderText('搜索命令、来源、端口或 Session');
  expect(search).toHaveValue('first');
  await user.clear(search);
  await user.type(search, 'second');
  await user.click(screen.getByRole('button', { name: '搜索' }));
  await waitFor(() => expect(api.commands).toHaveBeenLastCalledWith(expect.objectContaining({ query: 'second' })));
  await user.click(screen.getByRole('button', { name: '测试后退' }));
  await waitFor(() => expect(screen.getByTestId('command-search-location')).toHaveTextContent('?q=first'));
  expect(search).toHaveValue('first');
  await user.click(screen.getByRole('button', { name: '测试前进' }));
  await waitFor(() => expect(screen.getByTestId('command-search-location')).toHaveTextContent('?q=second'));
  expect(search).toHaveValue('second');
});
