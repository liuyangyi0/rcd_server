import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter, useNavigate } from 'react-router-dom';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { api } from '../api/client';
import type { Source } from '../api/types';
import LowerLogsPage from './LowerLogsPage';

const source: Source = {
  id: 'lower-a', name: '下级 A', groupId: 'g1', endpoint: '', securityPolicy: 'None', securityMode: 'None',
  hasPassword: false, enabled: true, connectionState: 'Connected', sessionState: 'Active', subscriptionState: 'Active',
  catalogState: 'Ready', reconnectCount: 0, monitoredCount: 1, goodPoints: 1, badPoints: 0, totalPoints: 1, revision: 1,
};

function RouteHarness() {
  const navigate = useNavigate();
  return <><button type="button" onClick={() => navigate('/lower-logs?source=lower-a')}>清除路由筛选</button><LowerLogsPage /></>;
}

afterEach(() => { vi.restoreAllMocks(); vi.unstubAllGlobals(); });

describe('central lower log filters', () => {
  it('keeps filter inputs synchronized with browser history navigation', async () => {
    vi.spyOn(api, 'sources').mockResolvedValue([source]);
    const fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify({
      items: [], total: 0, limit: 100, offset: 0, invalid_lines: 0, push_state: 'stored',
    }), { status: 200, headers: { 'Content-Type': 'application/json' } }));
    vi.stubGlobal('fetch', fetchMock);
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const user = userEvent.setup();
    render(<QueryClientProvider client={client}><MemoryRouter initialEntries={[
      '/lower-logs?source=lower-a&q=read_timeout&port=%2Fdev%2FttyWCH4&device=13',
    ]}><RouteHarness /></MemoryRouter></QueryClientProvider>);

    expect(screen.getByRole('textbox', { name: '串口' })).toHaveValue('/dev/ttyWCH4');
    expect(screen.getByRole('textbox', { name: '站点' })).toHaveValue('13');
    expect(screen.getByPlaceholderText('搜索消息、组件、串口或站点')).toHaveValue('read_timeout');

    await user.click(screen.getByRole('button', { name: '清除路由筛选' }));
    await waitFor(() => expect(screen.getByRole('textbox', { name: '串口' })).toHaveValue(''));
    expect(screen.getByRole('textbox', { name: '站点' })).toHaveValue('');
    expect(screen.getByPlaceholderText('搜索消息、组件、串口或站点')).toHaveValue('');
    await waitFor(() => expect(fetchMock.mock.calls.some(([url]) => {
      const value = String(url);
      return value.includes('source_id=lower-a') && !value.includes('port_id=') && !value.includes('device_id=') && !value.includes('q=');
    })).toBe(true));
  });
});
