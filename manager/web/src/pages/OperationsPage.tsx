import { useMemo, useState } from 'react';
import { Check, RefreshCw, Search, Volume2, VolumeX } from 'lucide-react';
import { Link, useSearchParams } from 'react-router-dom';
import { apiErrorMessage } from '../api/client';
import { useCalculationStatus } from '../api/calculations';
import { useAcknowledgeAlarm, useAlarms, useDeviceVersions, useManagementAudit, useOverview, useSilenceAlarm, useSourceEvents, useSources } from '../api/queries';
import type { ManagementAudit, SourceEvent } from '../api/types';
import { EmptyState, ErrorState, LoadingState } from '../components/PageState';
import { PageHeader } from '../components/PageHeader';
import { Pagination } from '../components/Pagination';
import { StatusBadge } from '../components/StatusBadge';
import { formatDateTime } from '../lib/format';
import { humanizeTechnicalMessage, kindLabel, operationLabel, stateLabel } from '../lib/labels';

const PAGE_SIZE = 50;
type Tab = 'alarms' | 'events' | 'audit';

export default function OperationsPage() {
  const [params, setParams] = useSearchParams();
  const tab = (['alarms', 'events', 'audit'].includes(params.get('tab') ?? '') ? params.get('tab') : 'alarms') as Tab;
  const page = Math.max(1, Number(params.get('page') ?? 1) || 1);
  const query = params.get('q') ?? '';
  const sourceId = params.get('source') ?? '';
  const state = params.get('state') ?? (tab === 'alarms' ? 'Open' : '');
  const range = params.get('range') ?? '24h';
  const customFrom = params.get('from') ?? '';
  const customTo = params.get('to') ?? '';
  const [draft, setDraft] = useState(query);
  const sources = useSources();
  const overview = useOverview();
  const calculation = useCalculationStatus();
  const versions = useDeviceVersions({ page: 1, pageSize: 1 });
  const bounds = useMemo(() => timeBounds(range, customFrom, customTo), [range, customFrom, customTo]);
  const listParams = { page, pageSize: PAGE_SIZE, query, sourceId, status: state, ...bounds };
  const alarms = useAlarms(listParams, tab === 'alarms');
  const events = useSourceEvents(listParams, tab === 'events');
  const audit = useManagementAudit({ page, pageSize: PAGE_SIZE, query, ...bounds }, tab === 'audit');
  const acknowledge = useAcknowledgeAlarm();
  const silence = useSilenceAlarm();
  const mutationError = acknowledge.error ?? silence.error;

  const activeQuery = tab === 'alarms' ? alarms : tab === 'events' ? events : audit;
  const sourceNames = useMemo(() => new Map((sources.data ?? []).map((source) => [source.id, source.name || source.id])), [sources.data]);
  const groupedEvents = useMemo(() => groupSourceEvents(events.data?.items ?? []), [events.data?.items]);
  const groupedAudit = useMemo(() => groupAuditLifecycle(audit.data?.items ?? []), [audit.data?.items]);

  const update = (next: Record<string, string | number | undefined>) => {
    const value = new URLSearchParams(params);
    for (const [key, item] of Object.entries(next)) {
      if (item === undefined || item === '' || (key === 'page' && item === 1)) value.delete(key);
      else value.set(key, String(item));
    }
    setParams(value);
  };

  return (
    <div className="page">
      <PageHeader title="告警与审计" description="查看下级状态历史、确认告警并追溯所有管理操作。" actions={
        <button className="button button-secondary" type="button" onClick={() => void activeQuery.refetch()}><RefreshCw size={16} />刷新</button>
      } />

      <div className="tab-list" role="tablist" aria-label="运维记录">
        {([['alarms', '告警'], ['events', '状态历史'], ['audit', '管理审计']] as const).map(([value, label]) => (
          <button key={value} type="button" role="tab" aria-selected={tab === value} className={tab === value ? 'active' : ''}
            onClick={() => { setDraft(''); update({ tab: value, page: 1, q: '', source: '', state: value === 'alarms' ? 'Open' : '' }); }}>{label}</button>
        ))}
      </div>

      <form className="toolbar filter-toolbar" onSubmit={(event) => { event.preventDefault(); update({ q: draft.trim(), page: 1 }); }}>
        <label className="search-field"><Search size={17} /><input value={draft} placeholder="搜索来源、类型、目标或信息" onChange={(event) => setDraft(event.target.value)} /><button type="submit">搜索</button></label>
        {tab !== 'audit' ? <label className="field inline-field"><span>来源</span><select value={sourceId} onChange={(event) => update({ source: event.target.value, page: 1 })}><option value="">全部来源</option>{(sources.data ?? []).map((source) => <option key={source.id} value={source.id}>{source.name || source.id}</option>)}</select></label> : null}
        {tab === 'alarms' ? <label className="field inline-field"><span>状态</span><select value={state} onChange={(event) => update({ state: event.target.value, page: 1 })}><option value="">全部</option><option value="Open">未恢复</option><option value="Resolved">已恢复</option></select></label> : null}
        <label className="field inline-field"><span>时间范围</span><select value={range} onChange={(event) => update({ range: event.target.value, from: '', to: '', page: 1 })}><option value="1h">最近 1 小时</option><option value="24h">最近 24 小时</option><option value="today8">今天 08:00 起</option><option value="all">全部记录</option><option value="custom">自定义</option></select></label>
        {range === 'custom' ? <><label className="field inline-field"><span>开始</span><input type="datetime-local" value={toLocalInput(customFrom)} onChange={(event) => update({ from: toISO(event.target.value), page: 1 })} /></label><label className="field inline-field"><span>结束</span><input type="datetime-local" value={toLocalInput(customTo)} onChange={(event) => update({ to: toISO(event.target.value), page: 1 })} /></label></> : null}
      </form>
      {mutationError ? <div className="top-error">{apiErrorMessage(mutationError)}</div> : null}

      {tab === 'alarms' ? <section className="quality-attention-strip" aria-label="当前数据质量关注项">
        <Link to="/points?quality=Bad"><span>采集点非 Good</span><strong>{overview.data?.badPoints ?? 0}</strong><small>当前变量质量</small></Link>
        <Link to="/calculations?quality=bad"><span>计算结果非 Good</span><strong>{calculation.data?.badCount ?? 0}</strong><small>中央计算质量</small></Link>
        <Link to="/device-versions?status=failed"><span>设备版本异常</span><strong>{(versions.data?.summary.failed ?? 0) + (versions.data?.summary.mismatch ?? 0)}</strong><small>失败或类型不一致</small></Link>
        <div><span>本页告警范围</span><strong>OPC 链路</strong><small>连接、会话、订阅和目录</small></div>
      </section> : null}

      <section className="table-panel content-panel">
        {activeQuery.isLoading && !activeQuery.data ? <LoadingState /> : activeQuery.isError ? <ErrorState error={activeQuery.error} onRetry={() => void activeQuery.refetch()} /> : tab === 'alarms' ? (
          (alarms.data?.items.length ?? 0) === 0 ? <EmptyState title="没有匹配的 OPC 链路告警" description={state === 'Open' ? '连接、会话、订阅和目录当前没有未恢复告警；采集、计算及版本异常请查看上方质量关注项。' : '调整筛选条件后重试。'} /> : <>
            <div className="table-scroll"><table className="data-table"><thead><tr><th>状态</th><th>告警</th><th>来源</th><th>信息</th><th>次数</th><th>打开 / 更新</th><th>确认</th><th className="align-right">操作</th></tr></thead><tbody>
              {alarms.data?.items.map((alarm) => {
                const open = alarm.state.toLowerCase() === 'open';
                const silenced = Boolean(alarm.silencedUntil && new Date(alarm.silencedUntil).getTime() > Date.now());
                return <tr key={alarm.id}>
                  <td><StatusBadge state={open ? alarm.severity.toLowerCase() === 'critical' ? 'bad' : 'warning' : 'good'} label={stateLabel(alarm.state)} /></td>
                  <td><strong>{kindLabel(alarm.kind)}</strong><span className="sub-cell">#{alarm.id} · {stateLabel(alarm.severity)}</span></td>
                  <td>{sourceNames.get(alarm.sourceId ?? '') ?? alarm.sourceId ?? '管理程序'}</td>
                  <td className="message-cell">{alarm.message}</td><td>{alarm.occurrences}</td>
                  <td>{formatDateTime(alarm.openedAt)}<span className="sub-cell">更新 {formatDateTime(alarm.updatedAt)}</span></td>
                  <td>{alarm.acknowledgedAt ? <><span className="text-good">已确认</span><span className="sub-cell">{alarm.acknowledgedBy}</span></> : <span className="text-warning">未确认</span>}</td>
                  <td className="align-right table-actions">
                    <button type="button" className="button button-small button-outline" disabled={Boolean(alarm.acknowledgedAt) || acknowledge.isPending} onClick={() => acknowledge.mutate({ id: alarm.id, revision: alarm.revision })}><Check size={14} />确认</button>
                    <button type="button" className="button button-small button-secondary" disabled={!open || silence.isPending} onClick={() => silence.mutate({ id: alarm.id, revision: alarm.revision, until: silenced ? undefined : new Date(Date.now() + 60 * 60 * 1000).toISOString() })}>{silenced ? <Volume2 size={14} /> : <VolumeX size={14} />}{silenced ? '取消静默' : '静默 1 小时'}</button>
                  </td>
                </tr>;
              })}
            </tbody></table></div>
            <Pagination page={alarms.data?.page ?? page} pageSize={PAGE_SIZE} total={alarms.data?.total ?? 0} onPageChange={(value) => update({ page: value })} />
          </>
        ) : tab === 'events' ? (
          (events.data?.items.length ?? 0) === 0 ? <EmptyState title="暂无状态历史" /> : <>
            <div className="table-scroll"><table className="data-table responsive-table"><thead><tr><th>时间</th><th>来源</th><th>状态变化</th><th>信息</th></tr></thead><tbody>{groupedEvents.map((group) => <tr key={group.key}><td data-label="时间">{formatDateTime(group.occurredAt)}</td><td data-label="来源"><strong>{sourceNames.get(group.sourceId) ?? group.sourceId}</strong></td><td data-label="状态变化"><strong>{group.items.length > 1 ? '同一轮连接状态更新' : kindLabel(group.items[0].kind)}</strong><div className="event-transition-list">{group.items.map((event) => <span key={event.id}><b>{kindLabel(event.kind)}</b>{event.previous ? stateLabel(event.previous) : '—'} → {stateLabel(event.current)}</span>)}</div></td><td data-label="信息" className="message-cell">{group.items.map((event) => event.message).filter(Boolean).map((message) => humanizeTechnicalMessage(message)).join('；') || '—'}</td></tr>)}</tbody></table></div>
            <Pagination page={events.data?.page ?? page} pageSize={PAGE_SIZE} total={events.data?.total ?? 0} onPageChange={(value) => update({ page: value })} />
          </>
        ) : (
          (audit.data?.items.length ?? 0) === 0 ? <EmptyState title="暂无管理审计" /> : <>
            <div className="table-scroll"><table className="data-table responsive-table management-audit-table"><thead><tr><th>时间</th><th>操作</th><th>操作者 / 目标</th><th>结果</th><th>摘要</th><th>技术明细</th></tr></thead><tbody>{groupedAudit.map((row) => { const entry = row.completed ?? row.requested ?? row.single; const outcome = auditOutcome(entry); return entry ? <tr key={row.key}><td data-label="时间">{formatDateTime(entry.occurredAt)}</td><td data-label="操作"><strong>{operationLabel(String(entry.details?.action ?? kindLabel(entry.kind)))}</strong><span className="sub-cell">{row.requested && row.completed ? '请求与完成已关联' : kindLabel(entry.kind)}</span></td><td data-label="操作者 / 目标">{entry.actor || '系统'}<span className="sub-cell">{entry.target}</span></td><td data-label="结果"><StatusBadge state={outcome === 'success' ? 'good' : outcome === 'pending' ? 'warning' : 'bad'} label={stateLabel(outcome)} compact /></td><td data-label="摘要" className="message-cell">{auditSummary(entry)}</td><td data-label="技术明细"><details className="audit-details"><summary>查看 JSON</summary><pre>{JSON.stringify({ requested: row.requested?.details, completed: row.completed?.details, single: row.single?.details }, null, 2)}</pre></details></td></tr> : null; })}</tbody></table></div>
            <Pagination page={audit.data?.page ?? page} pageSize={PAGE_SIZE} total={audit.data?.total ?? 0} onPageChange={(value) => update({ page: value })} />
          </>
        )}
      </section>
    </div>
  );
}

function timeBounds(range: string, customFrom: string, customTo: string): { from?: string; to?: string } {
  const now = new Date();
  if (range === 'all') return {};
  if (range === 'custom') return { from: customFrom || undefined, to: customTo || undefined };
  if (range === 'today8') {
    const from = new Date(now);
    from.setHours(8, 0, 0, 0);
    return { from: from.toISOString(), to: now.toISOString() };
  }
  const hours = range === '1h' ? 1 : 24;
  return { from: new Date(now.getTime() - hours * 60 * 60 * 1000).toISOString(), to: now.toISOString() };
}

function toLocalInput(value: string): string {
  if (!value) return '';
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return '';
  const local = new Date(date.getTime() - date.getTimezoneOffset() * 60_000);
  return local.toISOString().slice(0, 16);
}

function toISO(value: string): string {
  if (!value) return '';
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? '' : date.toISOString();
}

interface SourceEventGroup {
  key: string;
  sourceId: string;
  occurredAt: string;
  items: SourceEvent[];
}

function groupSourceEvents(items: SourceEvent[]): SourceEventGroup[] {
  const result: SourceEventGroup[] = [];
  for (const item of items) {
    const bucket = Math.floor(new Date(item.occurredAt).getTime() / 1000);
    const key = `${item.sourceId}:${bucket}`;
    const previous = result[result.length - 1];
    if (previous?.key === key) previous.items.push(item);
    else result.push({ key, sourceId: item.sourceId, occurredAt: item.occurredAt, items: [item] });
  }
  return result;
}

interface AuditLifecycle {
  key: string;
  requested?: ManagementAudit;
  completed?: ManagementAudit;
  single?: ManagementAudit;
}

function groupAuditLifecycle(items: ManagementAudit[]): AuditLifecycle[] {
  const rows: AuditLifecycle[] = [];
  const operations = new Map<string, AuditLifecycle>();
  for (const item of items) {
    const operationId = String(item.details?.operation_id ?? '');
    if (!operationId || !item.kind.startsWith('source.operation.')) {
      rows.push({ key: `single:${item.id}`, single: item });
      continue;
    }
    let row = operations.get(operationId);
    if (!row) {
      row = { key: `operation:${operationId}` };
      operations.set(operationId, row);
      rows.push(row);
    }
    if (item.kind.endsWith('.requested')) row.requested = item;
    else row.completed = item;
  }
  return rows;
}

function auditOutcome(entry?: ManagementAudit): string {
  if (!entry) return 'unknown';
  const details = entry.details ?? {};
  const state = String(details.version_state ?? '').toLowerCase();
  if (details.last_version_error || state.includes('fail') || state.includes('error') || state.includes('timeout')) return 'failed';
  if (details.type_mismatch === true) return 'warning';
  return String(details.outcome ?? (entry.kind.endsWith('.requested') ? 'pending' : 'success')).toLowerCase();
}

function auditSummary(entry: ManagementAudit): string {
  const details = entry.details ?? {};
  const pieces: string[] = [];
  if (details.port_id) pieces.push(`串口 ${details.port_id}`);
  if (details.device_id) pieces.push(`站点 ${details.device_id}`);
  if (details.module_type) pieces.push(`模块类型 ${stateLabel(String(details.module_type))}`);
  if (details.log_level) pieces.push(`日志等级 ${details.log_level}`);
  if (details.version_state) pieces.push(`版本状态 ${stateLabel(String(details.version_state))}`);
  if (details.communication_module_version) pieces.push(`通信模块 ${details.communication_module_version}`);
  if (details.plc_version) pieces.push(`PLC ${details.plc_version}`);
  if (details.duration_ms !== undefined) pieces.push(`耗时 ${details.duration_ms} ms`);
  if (details.last_version_error) pieces.push(humanizeTechnicalMessage(String(details.last_version_error)));
  return pieces.join(' · ') || `revision ${entry.revision}`;
}
