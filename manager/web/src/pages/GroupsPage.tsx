import { useMemo, useState } from 'react';
import { ArrowLeftRight, ChevronDown, Info, Pencil, Plus, RotateCcw, Trash2 } from 'lucide-react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { api, apiErrorMessage } from '../api/client';
import { queryKeys, useGroups, useSessions, useSources } from '../api/queries';
import type { Group } from '../api/types';
import { DeleteConfirmModal } from '../features/config/DeleteConfirmModal';
import { GroupFormModal } from '../features/config/GroupFormModal';
import { SourceSwitchDialog } from '../features/selection/SourceSwitchDialog';
import { GroupDetailDrawer } from '../features/selection/GroupDetailDrawer';
import { EmptyState, ErrorState, LoadingState } from '../components/PageState';
import { PageHeader } from '../components/PageHeader';
import { StatusBadge } from '../components/StatusBadge';
import { formatNumber, formatRelativeTime, shortSessionId } from '../lib/format';
import { stateLabel } from '../lib/labels';
import { sourceHealth } from '../lib/status';
import { sourceHasGroup } from '../lib/sourceGroups';
import { useSessionSelection } from '../hooks/useSessionSelection';

export default function GroupsPage() {
  const client = useQueryClient();
  const groupsQuery = useGroups();
  const sourcesQuery = useSources();
  const sessionsQuery = useSessions();
  const groups = groupsQuery.data ?? [];
  const sources = sourcesQuery.data ?? [];
  const sessions = sessionsQuery.data ?? [];
  const { sessionId, setSessionId, session: effectiveSession } = useSessionSelection(sessions);
  const [formGroup, setFormGroup] = useState<Group | 'new'>();
  const [switchState, setSwitchState] = useState<{ group: Group; mode: 'session' | 'default'; sessionId: string }>();
  const [deleteGroup, setDeleteGroup] = useState<Group>();
  const [detailGroupId, setDetailGroupId] = useState<string>();
  const [message, setMessage] = useState('');
  const sessionEnded = Boolean(sessionId && !effectiveSession && sessionsQuery.isSuccess);
  const sourceMap = useMemo(() => new Map(sources.map((source) => [source.id, source])), [sources]);
  const detailGroup = groups.find((group) => group.id === detailGroupId);
  const deviationCount = groups.filter((group) => effectiveSession && (effectiveSession.selectedSources[group.id] ?? group.defaultSourceId) !== group.defaultSourceId).length;

  const removeMutation = useMutation({
    mutationFn: (group: Group) => api.deleteGroup(group.id, group.selectionRevision),
    onSuccess: async (_, group) => {
      await Promise.all([
        client.invalidateQueries({ queryKey: queryKeys.groups }),
        client.invalidateQueries({ queryKey: queryKeys.allSources }),
        client.invalidateQueries({ queryKey: queryKeys.overview }),
      ]);
      setMessage(`分组 ${group.id} 已删除`);
      setDeleteGroup(undefined);
      setDetailGroupId(undefined);
    },
  });

  const loading = groupsQuery.isLoading || sourcesQuery.isLoading || sessionsQuery.isLoading;
  const firstError = groupsQuery.error ?? sourcesQuery.error ?? sessionsQuery.error;

  return (
    <div className="page groups-workbench">
      <PageHeader title="分组与选源" description="每个 OPC Session 按整逻辑设备组独立选源；一个下级可提供多个组，不自动故障转移。" actions={<button className="button button-primary" type="button" onClick={() => setFormGroup('new')}><Plus aria-hidden="true" size={17} />新建分组</button>} />

      <section className="session-context-bar" aria-label="当前查看会话">
        <label className="field session-context-picker"><span>当前查看会话</span><select value={sessionId} onChange={(event) => setSessionId(event.target.value)} disabled={sessions.length === 0}><option value="">{sessions.length === 0 ? '暂无在线会话' : '选择在线会话'}</option>{sessionEnded ? <option value={sessionId}>{shortSessionId(sessionId)}（已结束）</option> : null}{sessions.map((session) => <option key={session.id} value={session.id}>{session.clientName} · {shortSessionId(session.id)} · {session.remoteAddress}</option>)}</select></label>
        <div className="session-context-stat"><span>在线 Session</span><strong>{sessions.length}</strong></div>
        <div className="session-context-stat"><span>路由偏差</span><strong className={effectiveSession ? (deviationCount ? 'text-warning' : 'text-good') : 'text-muted'}>{effectiveSession ? `${deviationCount} / ${groups.length}` : '—'}</strong></div>
        <div className="context-actions"><button className="button button-secondary" type="button" onClick={() => void Promise.all([groupsQuery.refetch(), sourcesQuery.refetch(), sessionsQuery.refetch()])}><RotateCcw aria-hidden="true" size={16} />刷新</button></div>
      </section>
      {sessionEnded ? <div className="form-error" role="status">原目标 Session {shortSessionId(sessionId)} 已结束，已禁用按 Session 切源；请显式选择其他在线会话，不会自动更换目标。</div> : null}

      <div className="route-legend"><span><i className="legend-swatch legend-session" />蓝色：当前 Session 实际路由</span><span><i className="legend-swatch legend-default" />灰色：新 Session 默认路由</span><span><Info size={14} />来源异常时不会自动切换，需要人工选择</span></div>
      {message ? <div className="success-banner" role="status">{message}<button type="button" onClick={() => setMessage('')}>关闭</button></div> : null}

      <section className="table-panel content-panel groups-route-panel">
        {loading ? <LoadingState /> : firstError ? <ErrorState error={firstError} onRetry={() => void Promise.all([groupsQuery.refetch(), sourcesQuery.refetch(), sessionsQuery.refetch()])} /> : groups.length === 0 ? <EmptyState title="尚未配置分组" description="创建第一个分组后，可在“下级 OPC”中添加来源。" action={<button className="button button-primary" type="button" onClick={() => setFormGroup('new')}><Plus aria-hidden="true" size={16} />新建分组</button>} /> : (
          <div className="table-scroll"><table className="data-table groups-route-table"><thead><tr><th>逻辑设备组</th><th>此 Session 选中源</th><th>新 Session 默认源</th><th>选中源健康</th><th>本组默认源质量</th><th>来源成员</th><th>待处理命令</th><th>选源版本</th><th className="align-right">操作</th></tr></thead><tbody>{groups.map((group) => {
            const selectedId = effectiveSession ? (effectiveSession.selectedSources[group.id] ?? group.defaultSourceId) : '';
            const source = sourceMap.get(selectedId);
            const health = sourceHealth(source);
            const quality = group;
            const deviated = Boolean(effectiveSession && selectedId !== group.defaultSourceId);
            return <tr key={group.id} className={`${detailGroupId === group.id ? 'selected-row ' : ''}health-row health-row-${health}`}>
              <td><button className="group-name-button" type="button" onClick={() => setDetailGroupId(group.id)}><strong>{group.name || group.id}</strong><span>{group.id}</span></button></td>
              <td><span className="session-route-value">{effectiveSession ? selectedId || '未设置' : '—'}</span>{deviated ? <span className="route-deviation route-deviation-block">偏离默认</span> : <span className="sub-cell">{effectiveSession ? '与默认一致' : sessionEnded ? '目标会话已结束' : '尚未选择会话'}</span>}</td>
              <td><span className="default-route-value">{group.defaultSourceId || '未设置'}</span><span className="sub-cell">仅影响之后的新 Session</span></td>
              <td><StatusBadge state={health} label={source ? stateLabel(source.connectionState) : '未知'} /><span className="sub-cell">{source ? `${stateLabel(source.sessionState)} · ${stateLabel(source.subscriptionState)}` : '未找到来源'}</span></td>
              <td>{quality?.qualityKnown ? <span className="quality-pair"><span className="text-good">Good {formatNumber(quality.goodPoints)}</span><span className={quality.badPoints ? 'text-bad' : 'text-muted'}>Bad {formatNumber(quality.badPoints)}</span></span> : <span className="text-muted">目录 {formatNumber(quality?.totalPoints ?? 0)} 点</span>}</td>
              <td>{group.sourceCount} 个<span className="sub-cell">{sources.filter((item) => sourceHasGroup(item, group.id) && sourceHealth(item) === 'good').length} 个健康</span></td>
              <td className={group.pendingCommandCount ? 'text-warning' : ''}>{group.pendingCommandCount ? `${group.pendingCommandCount} · 选源锁定` : '0'}</td>
              <td>{effectiveSession ? (effectiveSession.routeRevisions[group.id] ?? group.selectionRevision) : '—'}<span className="sub-cell">{formatRelativeTime(group.lastSelectedAt)}</span></td>
              <td className="align-right table-actions"><button className="button button-small button-outline" type="button" disabled={!effectiveSession || group.pendingCommandCount > 0} onClick={() => setSwitchState({ group, mode: 'session', sessionId })}><ArrowLeftRight size={15} />切源</button><button className="icon-button" type="button" title="查看详情" aria-label={`查看 ${group.id}`} onClick={() => setDetailGroupId(group.id)}><ChevronDown size={16} /></button><button className="icon-button" type="button" title="编辑分组" aria-label={`编辑 ${group.id}`} onClick={() => setFormGroup(group)}><Pencil size={16} /></button><button className="icon-button danger-icon" type="button" title="删除分组" aria-label={`删除 ${group.id}`} onClick={() => { removeMutation.reset(); setDeleteGroup(group); }}><Trash2 size={16} /></button></td>
            </tr>;
          })}</tbody></table></div>
        )}
      </section>

      {sessions.length > 0 && groups.length > 0 ? <details className="session-route-comparison"><summary>在线 Session 路由对照 <span>查看 {sessions.length} 个 Session 在各分组的实际来源</span></summary><div className="table-scroll"><table className="data-table compact-table"><thead><tr><th>Session</th><th>远端</th>{groups.map((group) => <th key={group.id}>{group.name || group.id}</th>)}<th>最后活动</th></tr></thead><tbody>{sessions.map((session) => <tr key={session.id}><td><strong>{session.clientName}</strong><span className="sub-cell">{shortSessionId(session.id)}</span></td><td>{session.remoteAddress}</td>{groups.map((group) => { const selected = session.selectedSources[group.id] ?? group.defaultSourceId; const different = selected !== group.defaultSourceId; return <td key={group.id} className={different ? 'text-blue' : ''}>{selected || '未设置'}<span className="sub-cell">{different ? '偏离默认' : '与默认一致'}</span></td>; })}<td>{formatRelativeTime(session.lastSeenAt)}</td></tr>)}</tbody></table></div></details> : null}

      {detailGroup ? <GroupDetailDrawer key={detailGroup.id} group={detailGroup} sources={sources} sessions={sessions} sessionId={sessionId} onSessionChange={setSessionId} onClose={() => setDetailGroupId(undefined)} /> : null}
      <GroupFormModal open={Boolean(formGroup)} group={formGroup === 'new' ? undefined : formGroup} onClose={() => setFormGroup(undefined)} onSaved={setMessage} />
      {switchState ? <SourceSwitchDialog open mode={switchState.mode} group={groups.find((group) => group.id === switchState.group.id) ?? switchState.group} sources={sources.filter((source) => sourceHasGroup(source, switchState.group.id))} sessions={sessions} initialSessionId={switchState.sessionId} onClose={() => setSwitchState(undefined)} onSuccess={setMessage} /> : null}
      <DeleteConfirmModal open={Boolean(deleteGroup)} title="删除分组" subject={deleteGroup?.id ?? ''} pending={removeMutation.isPending} error={removeMutation.error ? apiErrorMessage(removeMutation.error) : undefined} onClose={() => setDeleteGroup(undefined)} onConfirm={() => deleteGroup && removeMutation.mutate(deleteGroup)} />
    </div>
  );
}
