import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, waitFor, within } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { api } from '../api/client';
import { calculationApi } from '../api/calculations';
import { normalizeDeviceVersionPage, normalizeOverview, normalizeSource } from '../api/normalizers';
import { queryKeys } from '../api/queries';
import OverviewPage from './OverviewPage';

const healthyOverview = normalizeOverview({ manager_state: 'Running', total_sources: 0,
  data_quality: { good: 0, bad: 0, total: 0 } });
const emptyPage = { items: [], total: 0, page: 1, pageSize: 5 };
const calculationStatus = { routingMode: 'DefaultSources' as const, activeRevision: 1, configHash: '',
  ruleCount: 0, goodCount: 0, badCount: 0, defaultRouteRevision: 1,
  lastEvaluationDurationMs: 0, pendingRestart: false };
const clients: QueryClient[] = [];

function setup() {
  vi.spyOn(api, 'overview').mockResolvedValue(healthyOverview);
  vi.spyOn(api, 'groups').mockResolvedValue([]);
  vi.spyOn(api, 'sources').mockResolvedValue([]);
  vi.spyOn(api, 'sessions').mockResolvedValue([]);
  vi.spyOn(api, 'commands').mockResolvedValue(emptyPage);
  vi.spyOn(api, 'alarms').mockResolvedValue(emptyPage);
  vi.spyOn(api, 'deviceVersions').mockResolvedValue(normalizeDeviceVersionPage({ items: [], total: 0 }));
  vi.spyOn(calculationApi, 'status').mockResolvedValue(calculationStatus);
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  clients.push(client);
  return () => {
    render(<QueryClientProvider client={client}><MemoryRouter><OverviewPage /></MemoryRouter></QueryClientProvider>);
    return client;
  };
}

afterEach(() => {
  clients.splice(0).forEach((client) => client.clear());
  vi.restoreAllMocks();
});

describe('overview confirmation and error states', () => {
  it('renders inactive Session and subscription badges as bad, not active green', async () => {
    const mount = setup();
    vi.mocked(api.sources).mockResolvedValue([normalizeSource({ id: 'offline', group_id: 'line',
      connection_state: 'Disconnected', session_state: 'Inactive', subscription_state: 'Inactive', catalog_state: 'Ready' })]);
    mount();
    const badges = await screen.findAllByText('未激活');
    expect(badges).toHaveLength(2);
    for (const badge of badges) expect(badge.closest('.status-badge')).toHaveClass('status-bad');
  });

  it('does not claim all systems are normal when calculation status fails', async () => {
    const mount = setup();
    vi.mocked(calculationApi.status).mockRejectedValue(new Error('计算状态读取失败'));
    mount();
    await screen.findByText('部分运行数据尚未确认');
    expect(screen.getByText('计算状态读取失败')).toBeInTheDocument();
    expect(screen.queryByText('当前无需人工处理')).not.toBeInTheDocument();
  });

  it('does not claim normal device versions when their query fails', async () => {
    const mount = setup();
    vi.mocked(api.deviceVersions).mockRejectedValue(new Error('设备版本读取失败'));
    mount();
    await screen.findByText('部分运行数据尚未确认');
    expect(screen.queryByText('当前无需人工处理')).not.toBeInTheDocument();
  });

  it('marks a failed cached overview unknown instead of keeping a green running summary', async () => {
    const mount = setup();
    vi.mocked(api.overview).mockRejectedValue(new Error('概览读取失败'));
    const client = clients[clients.length - 1];
    client.setQueryData(queryKeys.overview, healthyOverview);
    mount();
    const strip = within(screen.getByLabelText('运行态势'));
    await waitFor(() => expect(strip.getByText('状态未知')).toBeInTheDocument());
    expect(strip.queryByText('运行中')).not.toBeInTheDocument();
    expect(strip.queryByText('0 个异常')).not.toBeInTheDocument();
    expect(screen.getByText(/上次概览确认/)).toBeInTheDocument();
  });

  it('shows independent source/session/command failures instead of empty-state claims', async () => {
    const mount = setup();
    vi.mocked(api.sources).mockRejectedValue(new Error('来源读取失败'));
    vi.mocked(api.sessions).mockRejectedValue(new Error('会话读取失败'));
    vi.mocked(api.commands).mockRejectedValue(new Error('命令读取失败'));
    vi.mocked(api.groups).mockRejectedValue(new Error('分组读取失败'));
    mount();
    await screen.findByText('命令读取失败');
    expect(screen.getByText('会话读取失败')).toBeInTheDocument();
    expect(screen.queryByText('暂无下级 OPC')).not.toBeInTheDocument();
    expect(screen.queryByText('暂无在线 Session')).not.toBeInTheDocument();
    expect(screen.queryByText('暂无命令记录')).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: '首次配置' })).not.toBeInTheDocument();
  });
});
