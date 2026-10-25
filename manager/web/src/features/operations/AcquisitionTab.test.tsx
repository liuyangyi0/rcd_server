import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, expect, it, vi } from 'vitest';
import type { AcquisitionSnapshot } from '../../api/operations';
import { AcquisitionTab } from './AcquisitionTab';

afterEach(() => vi.unstubAllGlobals());

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
const snapshot = (stationRevision: string): AcquisitionSnapshot => ({
  source_id: 'lower-1', source_revision: 2, schema_version: 1, revision: 'lower-r1', station_revision: stationRevision,
  ports: [{ port_id: '/dev/ttyS0', port_name: 'COM-A', enabled: true, applied_enabled: true, open: true, devices: [{ device_id: '7', name: '站7', read_enabled: true, applied_read_enabled: true, communication_ok: true, configured_module_type: 'domestic' }] }],
});

function mount(state: { current: AcquisitionSnapshot }) {
  const fetchMock = vi.fn().mockImplementation((raw: string) => {
    const url = new URL(raw, 'http://localhost');
    if (url.pathname.endsWith('/status')) return Promise.resolve(json(state.current));
    if (url.pathname.endsWith('/device-version/query')) return Promise.resolve(json(state.current));
    return Promise.resolve(json({ items: [] }));
  });
  vi.stubGlobal('fetch', fetchMock);
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  render(<QueryClientProvider client={client}><AcquisitionTab target={{ id: 'lower-1', revision: 2 }} writable stationVersions onBusyChange={() => undefined} onDialogChange={() => undefined} /></QueryClientProvider>);
  return fetchMock;
}

it('sends the station revision the operator reviewed and blocks it after a concurrent change', async () => {
  const state = { current: snapshot('st-1') };
  const fetchMock = mount(state);
  const user = userEvent.setup();
  await user.click(await screen.findByRole('button', { name: '查询版本' }));
  await user.click(screen.getByLabelText('我已核对目标站点和操作'));
  // Another operator switches the station to a Russian module while the dialog is open.
  state.current = { ...snapshot('st-2'), ports: [{ ...snapshot('st-2').ports[0], devices: [{ ...snapshot('st-2').ports[0].devices[0], configured_module_type: 'legacy_russian' }] }] };
  await user.click(screen.getByRole('button', { name: '刷新实际状态' }));
  expect(await screen.findByText('站点版本配置已变化，请取消后重新核对。')).toBeInTheDocument();
  expect(screen.getByText(/当前不能查询：必须先配置为国产模块/)).toBeInTheDocument();
  expect(screen.getByRole('button', { name: '确认查询' })).toBeDisabled();
  expect(fetchMock.mock.calls.some(([raw]) => String(raw).includes('/device-version/query'))).toBe(false);
});

it('confirms with the revision captured when the dialog opened', async () => {
  const state = { current: snapshot('st-1') };
  const fetchMock = mount(state);
  const user = userEvent.setup();
  await user.click(await screen.findByRole('button', { name: '查询版本' }));
  await user.click(screen.getByLabelText('我已核对目标站点和操作'));
  await user.click(screen.getByRole('button', { name: '确认查询' }));
  await waitFor(() => expect(fetchMock.mock.calls.some(([raw]) => String(raw).includes('/device-version/query'))).toBe(true));
  const [, init] = fetchMock.mock.calls.find(([raw]) => String(raw).includes('/device-version/query'))!;
  expect(new Headers((init as RequestInit).headers).get('If-Match')).toBe('"st-1"');
});
