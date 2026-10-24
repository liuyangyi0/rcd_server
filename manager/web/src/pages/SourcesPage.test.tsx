import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, expect, it, vi } from 'vitest';
import { api } from '../api/client';
import type { Source } from '../api/types';
import SourcesPage from './SourcesPage';

const source: Source = { id: 'rcd-46', name: 'RCD 46 (192.168.10.46)', groupId: 'serial-acquisition', endpoint: 'opc.tcp://192.168.10.46:4841',
  securityPolicy: 'None', securityMode: 'None', hasPassword: false, enabled: true, connectionState: 'Connected', sessionState: 'Active', subscriptionState: 'Active',
  catalogState: 'Ready', reconnectCount: 0, monitoredCount: 1, goodPoints: 1, badPoints: 0, totalPoints: 1, revision: 2 };
let client: QueryClient;
afterEach(() => { client?.clear(); vi.restoreAllMocks(); });
function setup(value = source) {
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  vi.spyOn(api, 'sources').mockResolvedValue([value]);
  vi.spyOn(api, 'groups').mockResolvedValue([]);
  render(<QueryClientProvider client={client}><MemoryRouter><SourcesPage /></MemoryRouter></QueryClientProvider>);
}
it('shows the machine display name and stable ID together and supports name search', async () => {
  setup();
  expect(await screen.findByText(source.name)).toBeInTheDocument();
  expect(screen.getByText(source.id, { exact: true })).toBeInTheDocument();
  const user = userEvent.setup();
  await user.type(screen.getByPlaceholderText('搜索名称、Source ID、Endpoint 或分组'), 'RCD 46');
  expect(screen.getByText(source.name)).toBeInTheDocument();
  expect(screen.getByRole('button', { name: '编辑 rcd-46' })).toBeInTheDocument();
});
it('falls back to the stable ID when no display name is configured', async () => {
  setup({ ...source, name: '' });
  expect(await screen.findByText(source.id, { exact: true })).toBeInTheDocument();
});
