import { useMemo, useState } from 'react';
import { Check, RefreshCw, Search, Volume2, VolumeX } from 'lucide-react';
import { useSearchParams } from 'react-router-dom';
import { apiErrorMessage } from '../api/client';
import { useAcknowledgeAlarm, useAlarms, useManagementAudit, useSilenceAlarm, useSourceEvents, useSources } from '../api/queries';
import { EmptyState, ErrorState, LoadingState } from '../components/PageState';
import { PageHeader } from '../components/PageHeader';
import { Pagination } from '../components/Pagination';
import { StatusBadge } from '../components/StatusBadge';
import { formatDateTime } from '../lib/format';
import { kindLabel, stateLabel } from '../lib/labels';

const PAGE_SIZE = 50;
type Tab = 'alarms' | 'events' | 'audit';

export default function OperationsPage() {
  const [params, setParams] = useSearchParams();
  const tab = (['alarms', 'events', 'audit'].includes(params.get('tab') ?? '') ? params.get('tab') : 'alarms') as Tab;
  const page = Math.max(1, Number(params.get('page') ?? 1) || 1);
  const query = params.get('q') ?? '';
  const sourceId = params.get('source') ?? '';
  const state = params.get('state') ?? (tab === 'alarms' ? 'Open' : '');
  const [draft, setDraft] = useState(query);
  const sources = useSources();
  const listParams = { page, pageSize: PAGE_SIZE, query, sourceId, status: state };
  const alarms = useAlarms(listParams, tab === 'alarms');
  const events = useSourceEvents(listParams, tab === 'events');
  const audit = useManagementAudit({ page, pageSize: PAGE_SIZE, query }, tab === 'audit');
  const acknowledge = useAcknowledgeAlarm();
  const silence = useSilenceAlarm();
  const mutationError = acknowledge.error ?? silence.error;

  const activeQuery = tab === 'alarms' ? alarms : tab === 'events' ? events : audit;
  const sourceNames = useMemo(() => new Map((sources.data ?? []).map((source) => [source.id, source.name || source.id])), [sources.data]);

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
      </form>
      {mutationError ? <div className="top-error">{apiErrorMessage(mutationError)}</div> : null}

      <section className="table-panel content-panel">
        {activeQuery.isLoading && !activeQuery.data ? <LoadingState /> : activeQuery.isError ? <ErrorState error={activeQuery.error} onRetry={() => void activeQuery.refetch()} /> : tab === 'alarms' ? (
          (alarms.data?.items.length ?? 0) === 0 ? <EmptyState title="没有匹配的告警" description={state === 'Open' ? '当前没有未恢复告警。' : '调整筛选条件后重试。'} /> : <>
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
            <div className="table-scroll"><table className="data-table"><thead><tr><th>时间</th><th>来源</th><th>项目</th><th>原状态</th><th>新状态</th><th>信息</th></tr></thead><tbody>{events.data?.items.map((event) => <tr key={event.id}><td>{formatDateTime(event.occurredAt)}</td><td><strong>{sourceNames.get(event.sourceId) ?? event.sourceId}</strong></td><td>{kindLabel(event.kind)}</td><td>{event.previous ? stateLabel(event.previous) : '—'}</td><td>{stateLabel(event.current)}</td><td className="message-cell">{event.message ?? '—'}</td></tr>)}</tbody></table></div>
            <Pagination page={events.data?.page ?? page} pageSize={PAGE_SIZE} total={events.data?.total ?? 0} onPageChange={(value) => update({ page: value })} />
          </>
        ) : (
          (audit.data?.items.length ?? 0) === 0 ? <EmptyState title="暂无管理审计" /> : <>
            <div className="table-scroll"><table className="data-table"><thead><tr><th>时间</th><th>操作</th><th>操作者</th><th>目标</th><th>Revision</th><th>明细</th></tr></thead><tbody>{audit.data?.items.map((entry) => <tr key={entry.id}><td>{formatDateTime(entry.occurredAt)}</td><td><strong>{kindLabel(entry.kind)}</strong><span className="sub-cell">{entry.kind}</span></td><td>{entry.actor}</td><td>{entry.target}</td><td>{entry.revision}</td><td className="message-cell"><code>{JSON.stringify(entry.details ?? {})}</code></td></tr>)}</tbody></table></div>
            <Pagination page={audit.data?.page ?? page} pageSize={PAGE_SIZE} total={audit.data?.total ?? 0} onPageChange={(value) => update({ page: value })} />
          </>
        )}
      </section>
    </div>
  );
}
