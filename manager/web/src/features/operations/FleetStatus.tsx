import { useQuery } from '@tanstack/react-query';
import { request } from '../../api/client';
import { InlineNotice } from '../../components/InlineNotice';
import { formatDateTime } from '../../lib/format';

interface FleetComponent { component: string; state: string; running_build?: { version?: string }; disk_build?: { version?: string }; running_sha256?: string; disk_sha256?: string }
interface FleetSource { source_id: string; device_id: string; platform_url?: string; applied_at?: string; error?: string;
  machine?: { name: string; address: string; agent_url: string; opc_endpoint: string; active_operation_id?: string; last_success_at?: string; components?: FleetComponent[] } }
interface Fleet { enabled: boolean; platform_url: string; last_sync_at?: string; last_attempt_at?: string; error?: string; sources: FleetSource[] }
export function useFleetStatus() {
  return useQuery({ queryKey: ['fleet'], queryFn: async ({ signal }) => await request('/fleet', { signal }) as Fleet, staleTime: 30_000, refetchInterval: 60_000, refetchOnWindowFocus: false });
}
function safeLink(value?: string) { try { const u = new URL(value || ''); return ['http:', 'https:'].includes(u.protocol) && !u.username && !u.password ? u.href : undefined; } catch { return undefined; } }
export function FleetStatus({ sourceId }: { sourceId?: string }) {
  const query = useFleetStatus(); const value = query.data;
  if (query.isPending && !value) return null;
  if (query.error) return <InlineNotice tone="warning">台账同步状态暂时不可读；这不代表 Agent 或 OPC 断开。</InlineNotice>;
  if (!value?.enabled) return sourceId ? null : <InlineNotice>尚未接入统一机器台账；当前使用本管理端保存的来源与代理绑定。</InlineNotice>;
  const rows = sourceId ? value.sources.filter((item) => item.source_id === sourceId) : value.sources;
  return <section className="operations-process-card" aria-label="统一机器台账">
    <h3>统一机器台账</h3>
    <p>每 60 秒同步地址、身份与软件信息，不同步计算、组选源或采集配置。平台离线时继续使用最后有效绑定。</p>
    <p>最近成功同步：{value.last_sync_at ? formatDateTime(value.last_sync_at) : '尚未成功'}</p>
    {value.error ? <InlineNotice tone="warning">{value.error} · 当前为缓存信息。</InlineNotice> : null}
    {rows.map((row) => <div key={row.source_id} className="operations-port-heading">
      <div><strong>{row.machine?.name || row.source_id}</strong><p>{row.machine?.address || '尚未核对'} · {row.error || (row.applied_at ? '已保存有效绑定' : '等待身份核对')}</p>
      {row.machine?.active_operation_id ? <p>平台正在操作或等待确认：{row.machine.active_operation_id}</p> : null}
      {row.machine?.components?.length ? <details><summary>平台软件检查快照（非实时探测）</summary>
        <p>检查时间：{row.machine.last_success_at ? formatDateTime(row.machine.last_success_at) : '尚未确认'}{!row.machine.last_success_at || Date.now() - Date.parse(row.machine.last_success_at) > 180_000 ? ' · 已过期' : ''}</p>
        <ul>{row.machine.components.map((component) => <li key={component.component}>
          {({ lower: '下级 OPC', manager: 'OPC 管理端', agent: 'RCD Agent' } as Record<string, string>)[component.component] || component.component}：
          {component.state === 'stopped' ? '已停止' : component.state === 'running' ? `运行版本 ${component.running_build?.version || '无法确认'}` : '运行状态无法确认'}
          {component.state === 'running' && component.running_sha256 && component.disk_sha256 && component.running_sha256 !== component.disk_sha256 ? ' · 磁盘与运行版本不同' : ''}
        </li>)}</ul>
      </details> : null}</div>
      {safeLink(row.platform_url) ? <a className="button button-outline" href={safeLink(row.platform_url)} target="_blank" rel="noreferrer">软件升级 / 版本恢复</a> : null}
    </div>)}
    {!sourceId && safeLink(value.platform_url) ? <a href={safeLink(value.platform_url)} target="_blank" rel="noreferrer">打开 Semaphore 软件运维平台</a> : null}
  </section>;
}
