import { useMemo, useState } from 'react';
import { ArrowRight, BellRing, CircleAlert, Plus, ServerOff, Split, TriangleAlert } from 'lucide-react';
import { NavLink } from 'react-router-dom';
import { useAlarms, useCommands, useGroups, useOverview, useSessions, useSources } from '../api/queries';
import type { Group, Source } from '../api/types';
import { EmptyState, ErrorState, LoadingState } from '../components/PageState';
import { PageHeader } from '../components/PageHeader';
import { StatusBadge } from '../components/StatusBadge';
import { GroupDetailDrawer } from '../features/selection/GroupDetailDrawer';
import { SetupWizard } from '../features/config/SetupWizard';
import { formatDuration, formatNumber, formatRelativeTime, shortSessionId } from '../lib/format';
import { stateLabel } from '../lib/labels';
import { sourceHealth } from '../lib/status';

function Quality({ good, bad, total, known }: { good: number; bad: number; total: number; known?: boolean }) {
  if (!known) return <span className="text-muted">目录 {formatNumber(total)} 点</span>;
  return <span className="quality-pair"><span className="text-good">Good {formatNumber(good)}</span><span className={bad ? 'text-bad' : 'text-muted'}>Bad {formatNumber(bad)}</span></span>;
}

function groupTone(group: Group, source?: Source): 'good' | 'warning' | 'bad' {
  if (group.pendingCommandCount > 0 || source?.catalogState.toLowerCase().includes('conflict')) return 'warning';
  return sourceHealth(source) === 'good' ? 'good' : 'bad';
}

export default function OverviewPage() {
  const overview = useOverview();
  const groupsQuery = useGroups();
  const sourcesQuery = useSources();
  const sessionsQuery = useSessions();
  const commandsQuery = useCommands({ page: 1, pageSize: 5 });
  const alarmsQuery = useAlarms({ page: 1, pageSize: 5, status: 'Open' });
  const [selectedGroupId, setSelectedGroupId] = useState<string>();
  const [setupOpen, setSetupOpen] = useState(false);
  const [message, setMessage] = useState('');
  const groups = groupsQuery.data ?? [];
  const sources = sourcesQuery.data ?? [];
  const sessions = sessionsQuery.data ?? [];
  const data = overview.data;

  const sourceMap = useMemo(() => new Map(sources.map((source) => [source.id, source])), [sources]);
  const selectedGroup = groups.find((group) => group.id === selectedGroupId);
  const unhealthySources = useMemo(() => sources.filter((source) => sourceHealth(source) !== 'good'), [sources]);
  const catalogConflicts = useMemo(() => sources.filter((source) => source.catalogState.toLowerCase().includes('conflict') || (source.catalogConflicts?.length ?? 0) > 0), [sources]);
  const deviations = useMemo(() => groups.reduce((count, group) => count + sessions.filter((session) => (session.selectedSources[group.id] ?? group.defaultSourceId) !== group.defaultSourceId).length, 0), [groups, sessions]);
  const selectedRouteCount = useMemo(() => groups.reduce((count, group) => count + sessions.filter((session) => (session.selectedSources[group.id] ?? group.defaultSourceId) === group.defaultSourceId).length, 0), [groups, sessions]);
  const prioritySources = useMemo(() => [...sources].sort((a, b) => {
    const healthOrder = { bad: 0, warning: 1, unknown: 2, good: 3 } as const;
    return healthOrder[sourceHealth(a)] - healthOrder[sourceHealth(b)] || (b.badPoints - a.badPoints) || a.id.localeCompare(b.id);
  }).slice(0, 5), [sources]);
  const issueCount = unhealthySources.length + catalogConflicts.length + (data?.unacknowledgedAlarms ?? 0) + (deviations > 0 ? 1 : 0);
  const loading = groupsQuery.isLoading || sourcesQuery.isLoading || sessionsQuery.isLoading;
  const firstError = groupsQuery.error ?? sourcesQuery.error ?? sessionsQuery.error;

  return (
    <div className="page overview-page operations-dashboard">
      <PageHeader title="运行总览" description={`实时更新 · ${new Date().toLocaleString('zh-CN', { hour12: false })}`} actions={groups.length === 0 && !groupsQuery.isLoading ? <button type="button" className="button button-primary" onClick={() => setSetupOpen(true)}><Plus size={16} />首次配置</button> : undefined} />
      {message ? <div className="success-banner" role="status">{message}<button type="button" onClick={() => setMessage('')}>关闭</button></div> : null}
      {overview.isError ? <div className="top-error">管理 API 当前不可用，以下运行数据可能不完整。</div> : null}

      <section className="health-strip" aria-label="运行态势">
        <div className="health-strip-heading"><span>运行态势</span><small>关键链路与质量</small></div>
        <div className="health-metric"><span>管理 OPC</span><strong className={data?.managerState === 'running' ? 'text-good' : 'text-bad'}>{data?.managerState === 'running' ? '运行中' : '异常'}</strong><small>已运行 {data ? formatDuration(data.uptimeSeconds) : '—'}</small></div>
        <div className="health-metric"><span>下级连接</span><strong><i className={data?.abnormalSources ? 'text-warning' : 'text-good'}>{data?.connectedSources ?? '—'}</i> / {data?.totalSources ?? '—'}</strong><small>{data?.abnormalSources ?? 0} 个异常</small></div>
        <div className="health-metric health-metric-wide"><span>点位质量</span><strong>{data?.qualityKnown ? <><i className="text-good">{formatNumber(data.goodPoints)} Good</i><em>/</em><i className={data.badPoints ? 'text-bad' : 'text-muted'}>{formatNumber(data.badPoints)} Bad</i></> : formatNumber(data?.totalPoints ?? 0)}</strong><small>{data?.qualityKnown ? `共 ${formatNumber(data.totalPoints)} 点` : '实时质量统计未提供'}</small></div>
        <div className="health-metric"><span>上级 Session</span><strong>{formatNumber(data?.sessionCount ?? 0)}</strong><small>{formatNumber(data?.subscriptionCount ?? 0)} 个订阅</small></div>
        <div className="health-metric"><span>未确认告警</span><strong className={data?.unacknowledgedAlarms ? 'text-bad' : 'text-good'}>{formatNumber(data?.unacknowledgedAlarms ?? 0)}</strong><small>{data?.openAlarms ?? 0} 个未恢复</small></div>
        <NavLink className="button button-primary health-strip-action" to="/operations?tab=alarms&state=Open">查看告警</NavLink>
      </section>

      <div className="overview-primary-grid">
        <section className="work-panel route-panel">
          <div className="panel-heading"><div><h2>分组路由</h2><p>默认路由、在线 Session 路由偏差与数据质量</p></div><NavLink to="/groups">管理选源 <ArrowRight size={15} /></NavLink></div>
          {loading ? <LoadingState label="正在加载分组路由…" /> : firstError ? <ErrorState error={firstError} onRetry={() => void Promise.all([groupsQuery.refetch(), sourcesQuery.refetch(), sessionsQuery.refetch()])} /> : groups.length === 0 ? <EmptyState title="尚未配置分组" description="创建分组并添加来源后，路由状态会显示在这里。" /> : (
            <div className="table-scroll"><table className="data-table route-summary-table"><thead><tr><th>分组</th><th>默认源</th><th>连接</th><th>点位质量</th><th>在线 Session 路由</th><th>待处理</th><th>最近数据</th><th className="align-right">操作</th></tr></thead><tbody>
              {groups.map((group) => {
                const source = sourceMap.get(group.defaultSourceId);
                const tone = groupTone(group, source);
                const consistent = sessions.filter((session) => (session.selectedSources[group.id] ?? group.defaultSourceId) === group.defaultSourceId).length;
                const deviated = sessions.length - consistent;
                return <tr key={group.id} className={`health-row health-row-${tone}${selectedGroupId === group.id ? ' selected-row' : ''}`}><td><strong>{group.name || group.id}</strong><span className="sub-cell">{group.id} · {group.sourceCount} 个来源</span></td><td>{group.defaultSourceId || <span className="text-bad">未设置</span>}</td><td><StatusBadge state={sourceHealth(source)} label={source ? stateLabel(source.connectionState) : '未知'} compact /></td><td><Quality good={group.goodPoints} bad={group.badPoints} total={group.totalPoints} known={group.qualityKnown} /></td><td><span>{consistent} 个一致</span>{deviated ? <span className="route-deviation">{deviated} 个偏离</span> : <span className="subtle-inline">无偏离</span>}</td><td className={group.pendingCommandCount ? 'text-warning' : ''}>{group.pendingCommandCount || '0'}</td><td>{formatRelativeTime(source?.lastDataAt)}</td><td className="align-right"><button className="table-action-link" type="button" onClick={() => setSelectedGroupId(group.id)}>查看</button></td></tr>;
              })}
            </tbody></table></div>
          )}
        </section>

        <aside className="work-panel attention-panel">
          <div className="panel-heading"><div><h2>需要处理</h2><p>异常与人工决策项</p></div><span className={issueCount ? 'count-badge count-badge-bad' : 'count-badge'}>{issueCount} 项</span></div>
          <div className="attention-list">
            {unhealthySources.slice(0, 2).map((source) => <NavLink key={`source-${source.id}`} to="/sources" className="attention-item"><ServerOff size={17} /><div><strong>来源异常 · {source.name || source.id}</strong><span>{source.lastError || stateLabel(source.connectionState)}</span></div><time>{formatRelativeTime(source.lastDataAt)}</time></NavLink>)}
            {catalogConflicts.slice(0, 1).map((source) => <NavLink key={`catalog-${source.id}`} to="/sources" className="attention-item attention-warning"><TriangleAlert size={17} /><div><strong>目录冲突 · {source.name || source.id}</strong><span>{source.catalogConflicts?.[0]?.message || stateLabel(source.catalogState)}</span></div><span>查看</span></NavLink>)}
            {(alarmsQuery.data?.items ?? []).slice(0, 2).map((alarm) => <NavLink key={`alarm-${alarm.id}`} to="/operations?tab=alarms&state=Open" className="attention-item"><BellRing size={17} /><div><strong>未确认告警 · {alarm.sourceId || '系统'}</strong><span>{alarm.message}</span></div><time>{formatRelativeTime(alarm.updatedAt)}</time></NavLink>)}
            {deviations > 0 ? <NavLink to="/groups" className="attention-item attention-warning"><Split size={17} /><div><strong>Session 路由偏离默认</strong><span>{deviations} 条偏离，{selectedRouteCount} 条与默认一致</span></div><span>查看</span></NavLink> : null}
            {issueCount === 0 ? <div className="attention-empty"><CircleAlert size={21} /><strong>当前无需人工处理</strong><span>连接、目录与告警均正常</span></div> : null}
          </div>
          <NavLink className="panel-footer-link" to="/operations">查看全部告警与审计 <ArrowRight size={15} /></NavLink>
        </aside>
      </div>

      <section className="work-panel source-health-panel">
        <div className="panel-heading"><div><h2>来源健康</h2><p>异常优先，合并展示连接链路、质量和错误摘要</p></div><NavLink to="/sources">查看全部 {sources.length} 个来源 <ArrowRight size={15} /></NavLink></div>
        {sourcesQuery.isLoading ? <LoadingState label="正在加载来源…" /> : sources.length === 0 ? <EmptyState title="暂无下级 OPC" /> : <div className="table-scroll"><table className="data-table compact-table"><thead><tr><th>来源</th><th>所属组</th><th>连接 / Session / 订阅</th><th>监控点</th><th>Good / Bad</th><th>最近数据</th><th>重连</th><th>错误摘要</th></tr></thead><tbody>{prioritySources.map((source) => <tr key={source.id} className={`health-row health-row-${sourceHealth(source)}`}><td><strong>{source.name || source.id}</strong><span className="sub-cell">{source.id}</span></td><td>{source.groupId}</td><td><div className="status-sequence"><StatusBadge state={source.connectionState.toLowerCase().includes('connected') && !source.connectionState.toLowerCase().includes('disconnected') ? 'good' : 'bad'} label={stateLabel(source.connectionState)} compact /><StatusBadge state={source.sessionState.toLowerCase().includes('active') ? 'good' : 'bad'} label={stateLabel(source.sessionState)} compact /><StatusBadge state={source.subscriptionState.toLowerCase().includes('active') ? 'good' : 'bad'} label={stateLabel(source.subscriptionState)} compact /></div></td><td>{formatNumber(source.monitoredCount)}</td><td><Quality good={source.goodPoints} bad={source.badPoints} total={source.totalPoints} known={source.qualityKnown} /></td><td>{formatRelativeTime(source.lastDataAt)}</td><td>{source.reconnectCount}</td><td className="message-cell">{source.lastError || <span className="text-muted">—</span>}</td></tr>)}</tbody></table></div>}
      </section>

      <div className="overview-secondary-grid">
        <section className="work-panel"><div className="panel-heading"><div><h2>在线上级 Session</h2><p>当前路由偏差与最后活动</p></div><span className="count-badge">{sessions.length}</span></div>{sessions.length === 0 ? <EmptyState title="暂无在线 Session" /> : <div className="table-scroll"><table className="data-table compact-table"><thead><tr><th>客户端</th><th>远端</th><th>路由偏差</th><th>最后活动</th></tr></thead><tbody>{sessions.slice(0, 5).map((session) => { const difference = groups.filter((group) => (session.selectedSources[group.id] ?? group.defaultSourceId) !== group.defaultSourceId).length; return <tr key={session.id}><td><strong>{session.clientName}</strong><span className="sub-cell">{shortSessionId(session.id)}</span></td><td>{session.remoteAddress}</td><td className={difference ? 'text-warning' : 'text-good'}>{difference ? `${difference} 个偏离` : '与默认一致'}</td><td>{formatRelativeTime(session.lastSeenAt)}</td></tr>; })}</tbody></table></div>}</section>
        <section className="work-panel"><div className="panel-heading"><div><h2>最近命令</h2><p>执行结果与端到端耗时</p></div><NavLink to="/commands">查看记录 <ArrowRight size={15} /></NavLink></div>{commandsQuery.isLoading ? <LoadingState label="正在加载命令…" /> : (commandsQuery.data?.items.length ?? 0) === 0 ? <EmptyState title="暂无命令记录" /> : <div className="command-timeline">{commandsQuery.data?.items.slice(0, 5).map((command) => <div key={command.id} className="command-line"><span className={command.error || command.result.toLowerCase().includes('fail') ? 'timeline-dot timeline-dot-bad' : 'timeline-dot'} /><time>{formatRelativeTime(command.createdAt)}</time><strong>{command.port}</strong><span>{command.sourceId}</span><b className={command.error ? 'text-bad' : 'text-good'}>{command.error ? '失败' : command.result || '完成'}</b><em>{command.durationMs === undefined ? '—' : `${command.durationMs} ms`}</em></div>)}</div>}</section>
      </div>

      <GroupDetailDrawer group={selectedGroup} sources={sources} sessions={sessions} onClose={() => setSelectedGroupId(undefined)} />
      <SetupWizard open={setupOpen} onClose={() => setSetupOpen(false)} onComplete={setMessage} />
    </div>
  );
}
