import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { api } from '../api/client';
import type { DeviceVersion, DeviceVersionPage, Source } from '../api/types';
import DeviceVersionsPage from './DeviceVersionsPage';

const source: Source = { id: 'lower-a', name: '下级 A', groupId: 'g1', endpoint: '', securityPolicy: 'None', securityMode: 'None', hasPassword: false, enabled: true, connectionState: 'Connected', sessionState: 'Active', subscriptionState: 'Active', catalogState: 'Ready', reconnectCount: 0, monitoredCount: 2, goodPoints: 2, badPoints: 0, totalPoints: 2, revision: 7 };
const rows: DeviceVersion[] = [
  { sourceId: 'lower-a', sourceName: '下级 A', sourceRevision: 7, rcdSoftwareVersion: '0.2.0', rcdBuildCommit: 'abc12345', portId: '/dev/ttyWCH0', portName: 'ttyWCH0', deviceId: '12', deviceName: '站点 12', configuredModuleType: 'domestic', detectedModuleType: 'domestic', typeMismatch: false, communicationModuleVersion: 'V1.4', communicationModuleVersionRaw: 0x0104, plcVersion: 'V0.5', plcVersionRaw: 5, versionPayload: '01 04 00 05', versionState: 'success', lastVersionAttemptAt: '2026-09-01T00:00:00Z', lastVersionSuccessAt: '2026-09-01T00:00:00Z', stationRevision: '1', versionSupported: true, sourceOnline: true, cached: false, stale: false, portEnabled: true, portAppliedEnabled: true, portOpen: true, deviceReadEnabled: true, appliedDeviceReadEnabled: true, communicationOK: true, controlPending: false, pendingCommands: 0 },
  { sourceId: 'lower-a', sourceName: '下级 A', sourceRevision: 7, rcdSoftwareVersion: '0.2.0', rcdBuildCommit: 'abc12345', portId: '/dev/ttyWCH0', portName: 'ttyWCH0', deviceId: '14', deviceName: '站点 14', configuredModuleType: 'unknown', detectedModuleType: 'unknown', typeMismatch: false, versionState: 'never_queried', stationRevision: '1', versionSupported: true, sourceOnline: true, cached: false, stale: false, portEnabled: true, portAppliedEnabled: true, portOpen: true, deviceReadEnabled: true, appliedDeviceReadEnabled: true, communicationOK: true, controlPending: false, pendingCommands: 0 },
];
const versionPage: DeviceVersionPage = { items: rows, page: 1, pageSize: 50, total: 2, summary: { total: 2, domestic: 1, legacyRussian: 0, unknown: 1, mismatch: 0, failed: 0, offlineCached: 0, lowerSoftwareVersions: [{ softwareVersion: '0.2.0', buildCommit: 'abc12345', sourceCount: 1 }] } };

function setup(page = versionPage) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  vi.spyOn(api, 'deviceVersions').mockResolvedValue(page);
  vi.spyOn(api, 'sources').mockResolvedValue([source]);
  render(<QueryClientProvider client={client}><MemoryRouter initialEntries={['/device-versions']}><DeviceVersionsPage /></MemoryRouter></QueryClientProvider>);
  return userEvent.setup();
}

afterEach(() => { vi.restoreAllMocks(); vi.unstubAllGlobals(); });

describe('device version inventory', () => {
  it('shows raw versions, configured type, and summary independently from OPC routing', async () => {
    setup();
    await screen.findByRole('table');
    expect(screen.getByText('设备总数').parentElement).toHaveTextContent('2');
    const row = within(screen.getByRole('row', { name: /站点 12/ }));
    expect(row.getByText('V1.4')).toBeInTheDocument();
    expect(row.getByText('0x0104')).toBeInTheDocument();
    expect(row.getByText('V0.5')).toBeInTheDocument();
    expect(row.getByText('版本有效')).toBeInTheDocument();
    const softwareSummary = screen.getByRole('region', { name: '下级软件版本' });
    expect(softwareSummary).toHaveTextContent('0.2.0');
    expect(softwareSummary).toHaveTextContent('1 个来源');
  });

  it('requires explicit domestic confirmation and sends both revision scopes', async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify({ source_id: 'lower-a', source_revision: 7, schema_version: 1, revision: '2', station_revision: '2', status: 'Applied', station_operation_revision: '2', ports: [{ port_id: '/dev/ttyWCH0', devices: [{ device_id: '14', configured_module_type: 'domestic', detected_module_type: 'unknown', version_state: 'waiting' }] }] }), { status: 200, headers: { 'Content-Type': 'application/json' } }));
    vi.stubGlobal('fetch', fetchMock);
    const user = setup();
    const row = within(await screen.findByRole('row', { name: /站点 14/ }));
    expect(row.getByRole('button', { name: '查询版本' })).toBeDisabled();
    await user.click(row.getByRole('button', { name: '设置类型' }));
    await user.selectOptions(screen.getByRole('combobox', { name: '模块类型' }), 'domestic');
    expect(screen.getByRole('button', { name: '确认保存' })).toBeDisabled();
    await user.click(screen.getByText('我确认这些站点为国产模块，并允许下级发送一次版本请求'));
    await user.click(screen.getByRole('button', { name: '确认保存' }));
    await waitFor(() => expect(fetchMock).toHaveBeenCalledOnce());
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe('/api/v1/sources/lower-a/operations/device-module-type');
    expect(init.headers).toMatchObject({ 'If-Match': '"1"' });
    expect(JSON.parse(String(init.body))).toEqual({ source_revision: 7, station_revision: '1', port_id: '/dev/ttyWCH0', device_id: '14', module_type: 'domestic' });
  });

  it('shows old lower servers as unsupported and never opens a write dialog', async () => {
    const unsupported = { ...rows[1], stationRevision: '', versionSupported: false, versionState: 'unsupported' };
    setup({ items: [unsupported], page: 1, pageSize: 50, total: 1, summary: { ...versionPage.summary, total: 1, unknown: 1, domestic: 0 } });
    const row = within(await screen.findByRole('row', { name: /站点 14/ }));
    expect(row.getByText('下级版本功能不支持')).toBeInTheDocument();
    expect(row.getByRole('checkbox')).toBeDisabled();
    expect(row.getByRole('button', { name: '设置类型' })).toBeDisabled();
    expect(row.getByRole('button', { name: '查询版本' })).toBeDisabled();
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  it('distinguishes a supported lower waiting for its first revision from an unsupported lower', async () => {
    const waiting = { ...rows[1], stationRevision: '', versionSupported: true, versionState: 'waiting' };
    setup({ items: [waiting], page: 1, pageSize: 50, total: 1, summary: { ...versionPage.summary, total: 1, unknown: 1, domestic: 0 } });
    const row = within(await screen.findByRole('row', { name: /站点 14/ }));
    expect(row.getByText('等待版本状态')).toBeInTheDocument();
    expect(row.queryByText('下级版本功能不支持')).not.toBeInTheDocument();
    expect(row.getByRole('button', { name: '设置类型' })).toHaveAttribute('title', '等待下级返回站点版本状态');
  });
});
