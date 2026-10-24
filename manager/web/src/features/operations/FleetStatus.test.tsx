import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { FleetStatus } from './FleetStatus';

function mount(body: unknown, status = 200, sourceId?: string) {
  const calls = vi.fn(async () => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } }));
  vi.stubGlobal('fetch', calls);
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(<QueryClientProvider client={client}><FleetStatus sourceId={sourceId} /></QueryClientProvider>);
  return calls;
}
afterEach(() => vi.unstubAllGlobals());
describe('shared machine registry status', () => {
  it('distinguishes an unconfigured registry from a disconnected Agent', async () => {
    mount({ enabled: false, sources: [] });
    expect(await screen.findByText(/尚未接入统一机器台账/)).toBeInTheDocument();
  });
  it('keeps offline bindings visible and limits links to the physical source', async () => {
    const calls = mount({ enabled: true, platform_url: 'http://platform/project/1/rcd/devices',
      error: '平台暂时不可达', last_sync_at: '2026-10-09T01:00:00Z', sources: [
        { source_id: 'source-46', device_id: 'd46', platform_url: 'http://platform/project/1/rcd/devices?device=d46', applied_at: '2026-10-09T01:00:00Z', machine: { name: '46 采集', address: '192.168.10.46', active_operation_id: 'original-job' } },
        { source_id: 'source-30', device_id: 'd30', machine: { name: '30 采集', address: '192.168.10.30' } },
      ] }, 200, 'source-46');
    expect(await screen.findByText(/当前为缓存信息/)).toBeInTheDocument();
    expect(screen.getByText(/original-job/)).toBeInTheDocument();
    expect(screen.queryByText('30 采集')).not.toBeInTheDocument();
    expect(screen.getByRole('link', { name: '软件升级 / 版本恢复' })).toHaveAttribute('href', 'http://platform/project/1/rcd/devices?device=d46');
    expect(calls).toHaveBeenCalledTimes(1);
  });
  it('does not equate a failed registry request with a physical disconnection', async () => {
    mount({ error: { code: 'unavailable', message: 'unavailable' } }, 503);
    expect(await screen.findByText(/这不代表 Agent 或 OPC 断开/)).toBeInTheDocument();
  });
  it('never renders an executable URL from cached metadata', async () => {
    mount({ enabled: true, platform_url: 'javascript:alert(1)', sources: [{ source_id: 'lower', device_id: 'a', platform_url: 'javascript:alert(1)', machine: { name: '下级', address: '192.168.10.46' } }] });
    await screen.findByRole('region', { name: '统一机器台账' });
    expect(screen.queryByRole('link')).not.toBeInTheDocument();
  });
  it('never presents disk identity as a stopped process running version', async () => {
    mount({ enabled: true, sources: [{ source_id: 'lower', device_id: 'a', machine: { name: '下级', components: [{ component: 'manager', state: 'stopped', disk_build: { version: 'disk-only' } }, { component: 'agent', state: 'running', running_build: { version: 'actual-version' } }] } }] });
    expect(await screen.findByText(/OPC 管理端：已停止/)).toBeInTheDocument();
    expect(screen.queryByText(/disk-only/)).not.toBeInTheDocument();
    expect(screen.getByText(/运行版本 actual-version/)).toBeInTheDocument();
    expect(screen.getByText(/已过期/)).toBeInTheDocument();
  });
});
