import { lazy, Suspense, useEffect, useMemo, useRef, useState } from 'react';
import { Network, Pencil, Plus, RefreshCw, ScanSearch, Search, TestTube2, Trash2, Wrench } from 'lucide-react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { useSearchParams } from 'react-router-dom';
import { api, apiErrorMessage } from '../api/client';
import { queryKeys, useGroups, useSources } from '../api/queries';
import type { Source } from '../api/types';
import { DeleteConfirmModal } from '../features/config/DeleteConfirmModal';
import { SourceFormModal } from '../features/config/SourceFormModal';
import { SourceDiagnosticsModal } from '../features/config/SourceDiagnosticsModal';
import { EmptyState, ErrorState, LoadingState } from '../components/PageState';
import { PageHeader } from '../components/PageHeader';
import { StatusBadge } from '../components/StatusBadge';
import { formatNumber, formatRelativeTime } from '../lib/format';
import { stateLabel } from '../lib/labels';
import { sourceHealth } from '../lib/status';
import { sourceGroupIds, sourceHasGroup } from '../lib/sourceGroups';

const SourceTopologyDrawer = lazy(() => import('../features/config/SourceTopologyDrawer'));
const SourceOperationsDrawer = lazy(() => import('../features/operations/SourceOperationsDrawer'));

export default function SourcesPage() {
  const [urlParams, setUrlParams] = useSearchParams();
  const client = useQueryClient();
  const groupsQuery = useGroups();
  const sourcesQuery = useSources();
  const groups = groupsQuery.data ?? [];
  const sources = sourcesQuery.data ?? [];
  const [query, setQuery] = useState('');
  const [groupId, setGroupId] = useState('');
  const [editing, setEditing] = useState<Source | 'new'>();
  const [deleting, setDeleting] = useState<Source>();
  const [topologySourceId, setTopologySourceId] = useState('');
  const [operationsSelection, setOperationsSelection] = useState<{ source: Source; deleted: boolean }>();
  const dismissedOperationsId = useRef('');
  const [diagnostics, setDiagnostics] = useState<{ source: Source; mode: 'probe' | 'conflicts' }>();
  const [message, setMessage] = useState('');

  useEffect(() => {
    const requested = urlParams.get('operations');
    if (!requested) {
      dismissedOperationsId.current = '';
      return;
    }
    if (requested === dismissedOperationsId.current || operationsSelection?.source.id === requested) return;
    const source = sources.find((item) => item.id === requested);
    if (source) setOperationsSelection({ source, deleted: false });
  }, [operationsSelection?.source.id, sources, urlParams]);

  const openOperations = (source: Source) => {
    dismissedOperationsId.current = '';
    setTopologySourceId('');
    setOperationsSelection({ source, deleted: false });
    const next = new URLSearchParams(urlParams);
    next.set('operations', source.id);
    setUrlParams(next, { replace: true });
  };
  const closeOperations = () => {
    dismissedOperationsId.current = operationsSelection?.source.id ?? urlParams.get('operations') ?? '';
    setOperationsSelection(undefined);
    const next = new URLSearchParams(urlParams);
    next.delete('operations');
    setUrlParams(next, { replace: true });
  };

  const filteredSources = useMemo(() => {
    const needle = query.trim().toLowerCase();
    return sources.filter((source) => (!groupId || sourceHasGroup(source, groupId)) && (!needle || `${source.id} ${source.name} ${source.endpoint} ${sourceGroupIds(source).join(' ')}`.toLowerCase().includes(needle)));
  }, [groupId, query, sources]);

  const removeMutation = useMutation({
    mutationFn: (source: Source) => api.deleteSource(source.id, source.revision),
    onSuccess: async (_, source) => {
      await Promise.all([
        client.invalidateQueries({ queryKey: queryKeys.allSources }),
        client.invalidateQueries({ queryKey: queryKeys.groups }),
        client.invalidateQueries({ queryKey: queryKeys.overview }),
        client.invalidateQueries({ queryKey: queryKeys.allTopologies }),
      ]);
      setMessage(`来源 ${source.id} 已删除；已有节点将在当前进程内标记 BadOutOfService`);
      setDeleting(undefined);
    },
  });

  const restartMutation = useMutation({
    mutationFn: (source: Source) => api.restartSource(source.id, source.revision, true),
    onSuccess: async (_, variables) => {
      await Promise.all([
        client.invalidateQueries({ queryKey: queryKeys.allSources }),
        client.invalidateQueries({ queryKey: queryKeys.groups }),
        client.invalidateQueries({ queryKey: queryKeys.overview }),
        client.invalidateQueries({ queryKey: queryKeys.allSourceEvents }),
        client.invalidateQueries({ queryKey: queryKeys.allManagementAudit }),
        client.invalidateQueries({ queryKey: queryKeys.sourceTopology(variables.id) }),
      ]);
      setMessage(`${variables.id} 已开始重新连接并发现目录`);
    },
  });

  const loading = groupsQuery.isLoading || sourcesQuery.isLoading;
  const error = groupsQuery.error ?? sourcesQuery.error;
  const topologySource = sources.find((source) => source.id === topologySourceId);
  const currentOperationsSource = operationsSelection ? sources.find((source) => source.id === operationsSelection.source.id) : undefined;
  const operationsSourceMissing = Boolean(operationsSelection && sourcesQuery.data && !currentOperationsSource);
  // Keep the opened snapshot and dirty editor mounted after deletion. Once
  // observed, deletion is terminal for this panel, even if the ID is reused.
  // Adjust only this component's state, before rendering its children, so no
  // transient render can unlock writes against a newly created source.
  if (operationsSelection && operationsSourceMissing && !operationsSelection.deleted) setOperationsSelection({ ...operationsSelection, deleted: true });
  const operationsSourceDeleted = operationsSourceMissing || Boolean(operationsSelection?.deleted);
  const operationsSource = operationsSourceDeleted ? operationsSelection?.source : currentOperationsSource ?? operationsSelection?.source;
  return (
    <div className="page">
      <PageHeader
        title="下级 OPC"
        description="一条下级连接可提供多个逻辑设备组。先发现串口 / 设备，再配置设备映射。"
        actions={<button className="button button-primary" type="button" disabled={groups.length === 0} onClick={() => setEditing('new')}><Plus aria-hidden="true" size={17} />添加下级 OPC</button>}
      />

      <div className="toolbar filter-toolbar">
        <label className="search-field"><Search aria-hidden="true" size={17} /><input value={query} placeholder="搜索 Source ID、Endpoint 或分组" onChange={(event) => setQuery(event.target.value)} /></label>
        <label className="field inline-field"><span>分组</span><select value={groupId} onChange={(event) => setGroupId(event.target.value)}><option value="">全部分组</option>{groups.map((group) => <option key={group.id} value={group.id}>{group.name || group.id}</option>)}</select></label>
        <button className="button button-secondary" type="button" onClick={() => void Promise.all([groupsQuery.refetch(), sourcesQuery.refetch()])}><RefreshCw aria-hidden="true" size={16} />刷新</button>
      </div>
      {message ? <div className="success-banner" role="status">{message}<button type="button" onClick={() => setMessage('')}>关闭</button></div> : null}
      {restartMutation.error ? <div className="top-error" role="alert">{apiErrorMessage(restartMutation.error)}</div> : null}

      <section className="table-panel content-panel">
        {loading ? <LoadingState /> : error ? <ErrorState error={error} onRetry={() => void Promise.all([groupsQuery.refetch(), sourcesQuery.refetch()])} /> : sources.length === 0 ? (
          <EmptyState title="尚未配置下级 OPC" description={groups.length === 0 ? '请先创建分组。' : '添加来源后，管理程序将自动执行连接与目录发现。'} action={groups.length > 0 ? <button className="button button-primary" type="button" onClick={() => setEditing('new')}><Plus aria-hidden="true" size={16} />添加来源</button> : undefined} />
        ) : filteredSources.length === 0 ? <EmptyState title="没有匹配的来源" description="请调整筛选条件。" /> : (
          <div className="table-scroll">
            <table className="data-table responsive-table source-table">
              <thead><tr><th>Source ID</th><th>默认归属 / 设备组</th><th>Endpoint</th><th>连接链路</th><th>目录</th><th>来源整体质量</th><th>最近活动</th><th>本次重连</th><th className="align-right">操作</th></tr></thead>
              <tbody>{filteredSources.map((source) => {
                const health = sourceHealth(source);
                const catalogBad = source.catalogState.toLowerCase().includes('conflict') || source.catalogState.toLowerCase().includes('error');
                const conflictTitle = source.catalogConflicts?.map((conflict) => `${conflict.kind} · ${conflict.identifier} · ${conflict.sourceIds.join(', ')} · ${conflict.message}`).join('\n');
                return (
                  <tr key={source.id}>
                    <td><strong>{source.id}</strong>{source.lastError ? <span className="sub-cell text-bad" title={conflictTitle}>{source.lastError}</span> : null}</td>
                    <td><span>{source.groupId}<span className="sub-cell">默认归属组</span></span>{source.groupBindings?.length ? <span className="sub-cell" title={sourceGroupIds(source).join('、')}>{source.groupBindings.length} 台设备已绑定 · {sourceGroupIds(source).length} 个组</span> : null}</td>
                    <td className="endpoint-cell">{source.endpoint}</td>
                    <td><div className="status-sequence labelled-status"><span>连接 <b>{stateLabel(source.connectionState)}</b></span><span>会话 <b>{stateLabel(source.sessionState)}</b></span><span>订阅 <b>{stateLabel(source.subscriptionState)}</b></span></div><StatusBadge state={health} label={health === 'good' ? '链路正常' : '链路异常'} compact /><span className="sub-cell">{formatNumber(source.monitoredCount)} 个监控变量</span></td>
                    <td>{catalogBad || source.catalogConflicts?.length ? <button className="table-link text-bad" type="button" onClick={() => setDiagnostics({ source, mode: 'conflicts' })}><StatusBadge state="bad" label={`${stateLabel(source.catalogState)} · ${source.catalogConflicts?.length ?? 0} 项`} compact /></button> : <StatusBadge state={source.catalogState.toLowerCase().includes('ready') ? 'good' : 'warning'} label={stateLabel(source.catalogState)} compact />}</td>
                    <td>{source.qualityKnown ? <span className={source.badPoints ? 'text-bad' : 'text-good'}>{formatNumber(source.goodPoints)} / {formatNumber(source.totalPoints)}</span> : <span>{formatNumber(source.totalPoints)} 点<span className="sub-cell">实时质量未提供</span></span>}</td>
                    <td>数据 {formatRelativeTime(source.lastDataAt)}<span className="sub-cell">探针 {formatRelativeTime(source.lastProbeAt)}</span></td>
                    <td>{source.reconnectCount}</td>
                    <td className="align-right table-actions">
                      <button className="button button-small button-outline" type="button" aria-label={`运维 ${source.id}`} title="管理此下级的物理采集、配置文件和日志，不是 Session 选源" onClick={() => openOperations(source)}><Wrench aria-hidden="true" size={15} />运维</button>
                      <button className="button button-small button-outline" type="button" aria-label={`串口 / 设备映射 ${source.id}`} onClick={() => setTopologySourceId(source.id)}><Network aria-hidden="true" size={15} />串口 / 设备映射</button>
                      <button className="button button-small button-outline" type="button" aria-label={`测试连接 ${source.id}`} title="独立连接测试，不影响在线连接" onClick={() => setDiagnostics({ source, mode: 'probe' })}><TestTube2 aria-hidden="true" size={15} />连接测试</button>
                      <button className="button button-small button-secondary" type="button" disabled={restartMutation.isPending} aria-label={`重新连接并发现目录 ${source.id}`} title="每次重新连接都会重新读取 NamespaceArray、浏览目录并重建订阅" onClick={() => restartMutation.mutate(source)}><ScanSearch aria-hidden="true" size={15} />重连并发现</button>
                      <button className="icon-button" type="button" aria-label={`编辑 ${source.id}`} title="编辑" onClick={() => setEditing(source)}><Pencil aria-hidden="true" size={16} /></button>
                      <button className="icon-button danger-icon" type="button" aria-label={`删除 ${source.id}`} title="删除" onClick={() => { removeMutation.reset(); setDeleting(source); }}><Trash2 aria-hidden="true" size={16} /></button>
                    </td>
                  </tr>
                );
              })}</tbody>
            </table>
          </div>
        )}
      </section>

      <SourceFormModal open={Boolean(editing)} source={editing === 'new' ? undefined : editing} groups={groups} onClose={() => setEditing(undefined)} onSaved={setMessage} />
      <SourceDiagnosticsModal source={diagnostics?.source} mode={diagnostics?.mode} onClose={() => setDiagnostics(undefined)} />
      {topologySource ? <Suspense fallback={<LoadingState label="正在加载设备映射…" />}><SourceTopologyDrawer key={topologySource.id} source={topologySource} groups={groups} onClose={() => setTopologySourceId('')} onSaved={setMessage} /></Suspense> : null}
      {operationsSource ? <Suspense fallback={<LoadingState label="正在加载下级运维…" />}><SourceOperationsDrawer key={operationsSource.id} source={operationsSource} sourceDeleted={operationsSourceDeleted} onClose={closeOperations} /></Suspense> : null}
      <DeleteConfirmModal open={Boolean(deleting)} title="删除下级 OPC" subject={deleting?.id ?? ''} pending={removeMutation.isPending} error={removeMutation.error ? apiErrorMessage(removeMutation.error) : undefined} onClose={() => setDeleting(undefined)} onConfirm={() => deleting && removeMutation.mutate(deleting)} />
    </div>
  );
}
