import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, render, screen, waitFor } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import * as clientAPI from '../../api/client';
import CommandTraceDrawer, { type CommandTraceDetail } from './CommandTraceDrawer';

afterEach(() => vi.restoreAllMocks());

it('retains the displayed execution record across refresh failures without showing it for a different command', async () => {
  const detail: CommandTraceDetail = { command: { id: 'command-1', source_id: 'source-1', port: 'ttyWCH5', state: 'Waiting', request: '3F090330001A04' } };
  const request = vi.spyOn(clientAPI, 'request').mockResolvedValue(detail);
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
  const view = (id: string) => <QueryClientProvider client={client}><CommandTraceDrawer id={id} onClose={() => {}} /></QueryClientProvider>;
  const rendered = render(view('command-1'));
  try {
    const payload = await screen.findByText('3F090330001A04');
    request.mockRejectedValue(new Error('temporary read failure'));
    await act(async () => { await client.invalidateQueries({ queryKey: ['commands', 'detail', 'command-1'] }); });
    await screen.findByText(/保留上次读取的执行记录/);
    expect(screen.getByText('3F090330001A04')).toBe(payload);
    expect(screen.queryByText('无法获取数据')).not.toBeInTheDocument();
    request.mockResolvedValue(detail);
    await act(async () => { await client.invalidateQueries({ queryKey: ['commands', 'detail', 'command-1'] }); });
    await waitFor(() => expect(screen.queryByText(/保留上次读取的执行记录/)).not.toBeInTheDocument());
    expect(screen.getByText('3F090330001A04')).toBe(payload);
    request.mockRejectedValue(new Error('different command read failed'));
    rendered.rerender(view('command-2'));
    await screen.findByText('无法获取数据');
    expect(screen.queryByText('3F090330001A04')).not.toBeInTheDocument();
  } finally { client.clear(); }
});
