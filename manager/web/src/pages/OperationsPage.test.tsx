import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter, useNavigate } from 'react-router-dom';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { api } from '../api/client';
import { normalizeOverview } from '../api/normalizers';
import type { Alarm, DeviceVersionPage, ListParams, PageResult } from '../api/types';
import OperationsPage from './OperationsPage';

vi.mock('../api/calculations', () => ({ useCalculationStatus: () => ({ data: { badCount: 0 } }) }));
let client: QueryClient;
const page = <T,>(items: T[] = [], total = 0, current = 1): PageResult<T> => ({ items, total, page: current, pageSize: 50 });
function Navigation() { const navigate = useNavigate(); return <button onClick={() => navigate(-1)}>返回之前筛选</button>; }
function setup(entry = '/operations', total = 0) {
  client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  vi.spyOn(api, 'sources').mockResolvedValue([]);
  vi.spyOn(api, 'overview').mockResolvedValue(normalizeOverview({}));
  vi.spyOn(api, 'deviceVersions').mockResolvedValue({ ...page(), summary: { failed: 0, mismatch: 0 } } as DeviceVersionPage);
  const alarms = vi.spyOn(api, 'alarms').mockImplementation(async (params: ListParams) => page<Alarm>([], total, params.page));
  const events = vi.spyOn(api, 'sourceEvents').mockResolvedValue(page());
  const audit = vi.spyOn(api, 'managementAudit').mockResolvedValue(page());
  render(<QueryClientProvider client={client}><MemoryRouter initialEntries={[entry]}><Navigation /><OperationsPage /></MemoryRouter></QueryClientProvider>);
  return { alarms, events, audit, user: userEvent.setup() };
}
afterEach(() => { client?.clear(); vi.restoreAllMocks(); vi.useRealTimers(); });

describe('operations filters', () => {
  it('distinguishes explicit all alarms from the default Open filter', async () => {
    const { alarms, user } = setup();
    await waitFor(() => expect(alarms).toHaveBeenCalled());
    expect(alarms.mock.calls.at(-1)?.[0].status).toBe('Open');
    await user.selectOptions(screen.getByRole('combobox', { name: '告警状态' }), 'all');
    await waitFor(() => expect(alarms.mock.calls.at(-1)?.[0].status).toBe(''));
    expect(screen.getByRole('combobox', { name: '告警状态' })).toHaveValue('all');
    await user.selectOptions(screen.getByRole('combobox', { name: '告警状态' }), 'Resolved');
    await waitFor(() => expect(alarms.mock.calls.at(-1)?.[0].status).toBe('Resolved'));
  });
  it('does not freeze a relative upper bound and advances its lower bound on refresh', async () => {
    vi.useFakeTimers({ toFake: ['Date'] });
    vi.setSystemTime(new Date('2026-10-22T02:00:00Z'));
    const { alarms, user } = setup('/operations?range=1h');
    await waitFor(() => expect(alarms).toHaveBeenCalled());
    expect(alarms.mock.calls.at(-1)?.[0]).toMatchObject({ from: '2026-10-22T01:00:00.000Z' });
    expect(alarms.mock.calls.at(-1)?.[0].to).toBeUndefined();
    vi.setSystemTime(new Date('2026-10-22T03:00:00Z'));
    await user.click(screen.getByRole('button', { name: '刷新' }));
    await waitFor(() => expect(alarms.mock.calls.at(-1)?.[0].from).toBe('2026-10-22T02:00:00.000Z'));
    expect(alarms.mock.calls.at(-1)?.[0].to).toBeUndefined();
  });
  it('preserves fixed custom history boundaries during refresh', async () => {
    const { alarms, user } = setup('/operations?range=custom&from=2026-10-20T00:00:00Z&to=2026-10-21T00:00:00Z');
    await waitFor(() => expect(alarms).toHaveBeenCalled());
    await user.click(screen.getByRole('button', { name: '刷新' }));
    expect(alarms.mock.calls.at(-1)?.[0]).toMatchObject({ from: '2026-10-20T00:00:00Z', to: '2026-10-21T00:00:00Z' });
  });
  it('keeps pagination available for an empty out-of-range page', async () => {
    const { alarms, user } = setup('/operations?page=4&range=all', 2);
    await screen.findByRole('button', { name: '返回末页' });
    expect(screen.getByRole('button', { name: '上一页' })).toBeEnabled();
    await user.click(screen.getByRole('button', { name: '返回末页' }));
    await waitFor(() => expect(alarms.mock.calls.at(-1)?.[0].page).toBe(1));
  });
  it('restores the search input when browser navigation restores its URL', async () => {
    const { alarms, user } = setup('/operations?q=old');
    const input = screen.getByPlaceholderText('搜索来源、类型、目标或信息');
    await user.clear(input); await user.type(input, 'new'); await user.click(screen.getByRole('button', { name: '搜索' }));
    await waitFor(() => expect(alarms.mock.calls.at(-1)?.[0].query).toBe('new'));
    await user.click(screen.getByRole('button', { name: '返回之前筛选' }));
    await waitFor(() => expect(input).toHaveValue('old'));
    expect(alarms.mock.calls.at(-1)?.[0].query).toBe('old');
  });
});
