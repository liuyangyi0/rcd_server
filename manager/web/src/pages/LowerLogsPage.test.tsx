import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { api } from '../api/client';
import type { Source } from '../api/types';
import LowerLogsPage from './LowerLogsPage';

const source: Source = {
  id: 'lower-a', name: '下级 A', groupId: 'g1', endpoint: '', securityPolicy: 'None', securityMode: 'None',
  hasPassword: false, enabled: true, connectionState: 'Connected', sessionState: 'Active', subscriptionState: 'Active',
  catalogState: 'Ready', reconnectCount: 0, monitoredCount: 1, goodPoints: 1, badPoints: 0, totalPoints: 1, revision: 1,
};

afterEach(() => { vi.restoreAllMocks(); vi.unstubAllGlobals(); vi.useRealTimers(); });

describe('central lower log filters', () => {
  it('keeps filter inputs synchronized with browser history navigation', async () => {
    vi.spyOn(api, 'sources').mockResolvedValue([source]);
    const fetchMock = vi.fn().mockImplementation((raw: string) => {
      const url = new URL(raw, 'http://localhost');
      if (url.pathname.endsWith('/operations/capabilities')) return Promise.resolve(json({ source_id: source.id, source_revision: 1, supported: true, schema_version: 1, features: ['logging_configuration'], max_config_bytes: 2_097_152, max_log_bytes: 65_536, configuration_apply: 'restart' }));
      if (url.pathname.endsWith('/operations/logging')) return Promise.resolve(json(loggingStatus()));
      return Promise.resolve(json({ items: [], total: 0, limit: 100, offset: 0, invalid_lines: 0, push_state: 'stored' }));
    });
    vi.stubGlobal('fetch', fetchMock);
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const user = userEvent.setup();
    render(<QueryClientProvider client={client}><MemoryRouter initialEntries={[
      '/lower-logs?source=lower-a&q=read_timeout&port=%2Fdev%2FttyWCH4&device=13',
    ]}><LowerLogsPage /></MemoryRouter></QueryClientProvider>);

    expect(screen.getByRole('combobox', { name: '串口' })).toHaveValue('/dev/ttyWCH4');
    expect(screen.getByRole('combobox', { name: '站点' })).toHaveValue('13');
    expect(screen.getByPlaceholderText('搜索消息、组件、串口或站点')).toHaveValue('read_timeout');

    await user.click(screen.getByRole('button', { name: '显示最新日志' }));
    await waitFor(() => expect(screen.getByRole('combobox', { name: '串口' })).toHaveValue(''));
    expect(screen.getByRole('combobox', { name: '站点' })).toHaveValue('');
    expect(screen.getByPlaceholderText('搜索消息、组件、串口或站点')).toHaveValue('');
    await waitFor(() => expect(fetchMock.mock.calls.some(([url]) => {
      const value = String(url);
      return value.includes('source_id=lower-a') && !value.includes('port_id=') && !value.includes('device_id=') && !value.includes('q=');
    })).toBe(true));
  });

  it('shows the active lower logging state and saves a revision-pinned level', async () => {
    vi.spyOn(api, 'sources').mockResolvedValue([source]);
    let currentLogging: Record<string, unknown> = loggingStatus();
    const fetchMock = vi.fn().mockImplementation((raw: string, init: RequestInit = {}) => {
      const url = new URL(raw, 'http://localhost');
      if (url.pathname.endsWith('/operations/capabilities')) return Promise.resolve(json({ source_id: source.id, source_revision: 1, supported: true, schema_version: 1, features: ['logging_configuration'], max_config_bytes: 2_097_152, max_log_bytes: 65_536, configuration_apply: 'restart' }));
      if (url.pathname.endsWith('/operations/logging')) {
        if (init.method === 'PUT') currentLogging = { ...currentLogging, configured_level: 'debug', configured_spec: 'warn,rcd_server=debug', config_revision: 'cfg-2', restart_required: true, status: 'Saved' };
        return Promise.resolve(json(currentLogging));
      }
      return Promise.resolve(json({ items: [{ source_id: source.id, boot_id: 'b1', sequence: 1, timestamp: '2026-09-01T01:00:00Z', received_at: '2026-09-01T01:00:01Z', level: 'INFO', component: 'rcd_server', message: 'started' }], total: 1, limit: 100, offset: 0, push_state: 'recent' }));
    });
    vi.stubGlobal('fetch', fetchMock);
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const user = userEvent.setup();
    render(<QueryClientProvider client={client}><MemoryRouter initialEntries={['/lower-logs?source=lower-a']}><LowerLogsPage /></MemoryRouter></QueryClientProvider>);

    await user.click(screen.getByText('调整下级日志等级（与查看筛选无关）'));
    expect(await screen.findByText('HTTP 集中推送')).toBeInTheDocument();
    expect(screen.getByText('已启用')).toBeInTheDocument();
    await user.selectOptions(screen.getByRole('combobox', { name: '保存的日志等级' }), 'debug');
    await user.click(screen.getByRole('button', { name: '保存等级' }));
    expect(await screen.findByText('日志等级已保存；重启该下级后生效。')).toBeInTheDocument();
    const write = fetchMock.mock.calls.find(([raw, init]) => String(raw).endsWith('/operations/logging') && (init as RequestInit | undefined)?.method === 'PUT');
    expect(write?.[1]).toMatchObject({ headers: { 'If-Match': '"cfg-1"' } });
    expect(JSON.parse(String((write?.[1] as RequestInit).body))).toEqual({ source_revision: 1, level: 'debug' });
    expect(screen.getByText('配置尚未应用。')).toBeInTheDocument();
  });
});

it('refresh advances the rolling time window and invalid custom times do not fetch logs', async () => {
  vi.useFakeTimers({ toFake: ['Date'] });
  vi.setSystemTime(new Date('2026-09-19T02:00:00Z'));
  vi.spyOn(api, 'sources').mockResolvedValue([source]);
  const fetchMock = vi.fn().mockImplementation((raw: string) => {
    if (String(raw).includes('/operations/capabilities')) return Promise.resolve(json({ features: [] }));
    return Promise.resolve(json({ items: [], total: 0, limit: 100, offset: 0, push_state: 'stored' }));
  });
  vi.stubGlobal('fetch', fetchMock);
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const user = userEvent.setup();
  const view = render(<QueryClientProvider client={client}><MemoryRouter initialEntries={['/lower-logs?source=lower-a']}><LowerLogsPage /></MemoryRouter></QueryClientProvider>);
  await waitFor(() => expect(screen.getByRole('button', { name: '刷新' })).toBeEnabled());
  const queries = () => fetchMock.mock.calls.filter(([url]) => String(url).includes('/lower-logs?')).map(([url]) => new URL(String(url), 'http://localhost'));
  await waitFor(() => expect(queries().length).toBeGreaterThan(0));
  expect(queries().at(-1)?.searchParams.get('to')).toBe('2026-09-19T02:00:00.000Z');
  vi.setSystemTime(new Date('2026-09-19T03:00:00Z'));
  await user.click(screen.getByRole('button', { name: '刷新' }));
  await waitFor(() => expect(queries().at(-1)?.searchParams.get('to')).toBe('2026-09-19T03:00:00.000Z'));
  view.unmount(); fetchMock.mockClear();
  render(<QueryClientProvider client={client}><MemoryRouter initialEntries={['/lower-logs?source=lower-a&range=custom&from=2026-09-19T04:00:00Z&to=2026-09-19T03:00:00Z']}><LowerLogsPage /></MemoryRouter></QueryClientProvider>);
  expect(screen.getByText('请调整查询时间')).toBeInTheDocument();
  expect(screen.getByRole('button', { name: '刷新' })).toBeDisabled();
  expect(queries()).toHaveLength(0);
});

function json(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
}

function loggingStatus() {
  return {
    source_id: source.id, source_revision: 1, active_level: 'info', active_spec: 'warn,rcd_server=info',
    configured_level: 'info', configured_spec: 'warn,rcd_server=info', config_revision: 'cfg-1', restart_required: false,
    environment_override: false, serial_frame_logs: false, http_push_enabled: true, http_push_source_id: source.id,
  };
}
