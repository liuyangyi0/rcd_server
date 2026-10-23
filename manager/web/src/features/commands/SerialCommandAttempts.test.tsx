import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, expect, it, vi } from 'vitest';
import { api } from '../../api/client';
import type { Source } from '../../api/types';
import SerialCommandAttempts, { parseCommandSummary } from './SerialCommandAttempts';

afterEach(() => { vi.restoreAllMocks(); vi.unstubAllGlobals(); });

const summary = {
  schema_version: 1, ack_id: '18446744073709551615', port_id: '/dev/ttyWCH0', device_id: '17',
  request: '110903', request_truncated: false, request_bytes: 3,
  sent_count: 3, attempt_count: 3, max_attempts: 3, result: 'received', last_error: '', duration_ms: 120,
  failure_count: 2, failures: [{ attempt: 1, reason: 'ack_timeout' }, { attempt: 2, reason: 'ack_mismatch' }],
};

it('shows three sends as two retries with final ACK success and server-side filtering', async () => {
  vi.spyOn(api, 'sources').mockResolvedValue([{ id: 'a', name: '下级 A' }, { id: 'b', name: '下级 B' }] as Source[]);
  const fetchMock = vi.fn().mockResolvedValue(new Response());
  fetchMock.mockImplementation(() => Promise.resolve(new Response(JSON.stringify({
    items: [{ source_id: 'a', boot_id: 'boot', sequence: 1, timestamp: '2026-09-22T01:00:00Z', component: 'rcd_server::command_audit', message: JSON.stringify(summary) }], total: 26, limit: 25, push_state: 'recent',
  }), { headers: { 'Content-Type': 'application/json' } })));
  vi.stubGlobal('fetch', fetchMock);
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(<QueryClientProvider client={client}><MemoryRouter><SerialCommandAttempts /></MemoryRouter></QueryClientProvider>);
  expect(await screen.findByText('发送 3 次 · 补发 2 次')).toBeInTheDocument();
  expect(screen.getByText('补发后成功（ACK）')).toBeInTheDocument();
  expect(screen.getByText('第 1 次：等待 ACK 超时')).toBeInTheDocument();
  expect(screen.getByText('18446744073709551615')).toBeInTheDocument();
  const user = userEvent.setup();
  await user.click(screen.getByRole('button', { name: '下一页' }));
  await waitFor(() => expect(fetchMock.mock.calls.some(([url]) => String(url).includes('offset=25'))).toBe(true));
  await user.selectOptions(screen.getByRole('combobox', { name: '发送情况' }), 'three');
  await waitFor(() => expect(String(fetchMock.mock.calls.at(-1)?.[0])).toContain('command_attempts=three'));
  expect(String(fetchMock.mock.calls.at(-1)?.[0])).toContain('offset=0');
  await user.selectOptions(screen.getByRole('combobox', { name: '明细来源' }), 'b');
  await waitFor(() => expect(String(fetchMock.mock.calls.at(-1)?.[0])).toContain('source_id=b'));
});

it('never interprets a malformed or old log as successful single-send data', () => {
  expect(parseCommandSummary('ack_received')).toBeUndefined();
  expect(parseCommandSummary('{}')).toBeUndefined();
  expect(parseCommandSummary(JSON.stringify({ ...summary, sent_count: -1 }))).toBeUndefined();
  expect(parseCommandSummary(JSON.stringify({ ...summary, attempt_count: 1 }))).toBeUndefined();
  expect(parseCommandSummary(JSON.stringify({ ...summary, result: 'failed' }))?.result).toBe('failed');
});
