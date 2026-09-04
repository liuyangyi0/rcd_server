import { useEffect, useMemo, useState } from 'react';
import { Download, RefreshCw, Search } from 'lucide-react';
import { useSearchParams } from 'react-router-dom';
import { queryString } from '../api/client';
import { useGroups, usePointComparison, usePoints, useSessions, useSources, useSourceTopology } from '../api/queries';
import type { ListParams, Point } from '../api/types';
import { EmptyState, ErrorState, LoadingState } from '../components/PageState';
import { PageHeader } from '../components/PageHeader';
import { Pagination } from '../components/Pagination';
import { formatDateTime, displayValue, shortClientName, shortSessionId } from '../lib/format';
import { statusCodeLabel } from '../lib/labels';
import { isBadStatus } from '../lib/status';
import { sourceHasGroup } from '../lib/sourceGroups';

const PAGE_SIZE = 50;

export default function PointsPage() {
  const [searchParams, setSearchParams] = useSearchParams();
  const [draftQuery, setDraftQuery] = useState(searchParams.get('q') ?? '');
  const groupId = searchParams.get('group') ?? '';
  const query = searchParams.get('q') ?? '';
  const quality = searchParams.get('quality') ?? '';
  const catalogSourceId = searchParams.get('catalog_source') ?? '';
  const portId = searchParams.get('port') ?? '';
  const deviceId = searchParams.get('device') ?? '';
  const view = searchParams.get('view') ?? 'default';
  const sessionId = searchParams.get('session') ?? '';
  const routeSourceId = searchParams.get('source') ?? '';
  const compare = searchParams.get('compare') === '1';
  const leftSourceId = searchParams.get('left') ?? '';
  const rightSourceId = searchParams.get('right') ?? '';
  const page = Math.max(1, Number(searchParams.get('page') ?? 1) || 1);
  const groupsQuery = useGroups();
  const sourcesQuery = useSources();
  const sessionsQuery = useSessions();
  const topologyQuery = useSourceTopology(catalogSourceId);
  const ports = topologyQuery.data?.ports ?? [];
  const devices = ports.find((port) => port.portId === portId)?.devices ?? [];
  const groupSources = useMemo(() => (sourcesQuery.data ?? []).filter((source) => !groupId || sourceHasGroup(source, groupId)), [groupId, sourcesQuery.data]);
  const selectedSession = sessionsQuery.data?.find((session) => session.id === sessionId);
  const sessionRequested = !compare && view === 'session' && Boolean(sessionId);
  const sessionEnded = sessionRequested && sessionsQuery.isSuccess && !selectedSession;
  const routeReady = view === 'default' || (view === 'session' && Boolean(selectedSession)) || (view === 'source' && Boolean(groupId && routeSourceId));
  const comparisonConfigured = Boolean(groupId && leftSourceId && rightSourceId);

  const routeParams: ListParams = {
    page, pageSize: PAGE_SIZE, query, groupId, sourceId: catalogSourceId, portId, deviceId, status: quality,
    sessionId: view === 'session' ? sessionId : undefined,
    routeSourceId: view === 'source' ? routeSourceId : undefined,
  };
  const pointsQuery = usePoints(routeParams, !compare && routeReady);
  // Both comparison requests must page over the same union directory. Applying
  // a quality filter independently would produce different node sets and make
  // an existing point look missing on one side.
  const leftParams: ListParams = { page, pageSize: PAGE_SIZE, query, groupId, sourceId: catalogSourceId, portId, deviceId, routeSourceId: leftSourceId };
  const rightParams: ListParams = { page, pageSize: PAGE_SIZE, query, groupId, sourceId: catalogSourceId, portId, deviceId, routeSourceId: rightSourceId };
  const comparisonQuery = usePointComparison(leftParams, rightParams, compare && comparisonConfigured);
  const points = pointsQuery.data?.items ?? [];
  const comparisonData = comparisonQuery.data;
  const comparison = useMemo(() => comparisonData ? joinComparison(comparisonData.left.items, comparisonData.right.items) : [], [comparisonData]);

  useEffect(() => setDraftQuery(query), [query]);

  const updateParams = (next: Record<string, string | number | undefined>) => {
    const params = new URLSearchParams(searchParams);
    for (const [key, value] of Object.entries(next)) {
      if (value === undefined || value === '' || (key === 'page' && value === 1)) params.delete(key);
      else params.set(key, String(value));
    }
    setSearchParams(params);
  };

  const loading = compare ? comparisonQuery.isPending : pointsQuery.isPending;
  const error = compare ? comparisonQuery.error : pointsQuery.error;
  const activeData = compare ? comparisonData?.left : pointsQuery.data;
  const exportParams = compare ? leftParams : routeParams;
  const refreshPoints = () => {
    if (compare && comparisonConfigured) void comparisonQuery.refetch();
    else if (!compare && routeReady) void pointsQuery.refetch();
  };

  return (
    <div className="page">
      <PageHeader title="点位浏览" description="浏览 OPC 目录节点（变量与命令方法）；顶部和页脚的“变量”总数不包含 Method。" actions={(!compare && routeReady) || (compare && Boolean(groupId && leftSourceId && rightSourceId)) ? <a className="button button-outline" href={`/api/v1/points/export${queryString(exportParams)}`}><Download size={16} />{compare ? `导出 ${leftSourceId} 筛选结果` : '导出筛选结果'}</a> : undefined} />
      <form className="toolbar filter-toolbar points-toolbar" onSubmit={(event) => { event.preventDefault(); updateParams({ q: draftQuery.trim(), page: 1 }); }}>
        <label className="search-field"><Search aria-hidden="true" size={17} /><input value={draftQuery} placeholder="搜索 Node ID 或 BrowseName" onChange={(event) => setDraftQuery(event.target.value)} /><button type="submit">搜索</button></label>
        <label className="field inline-field"><span>逻辑组</span><select value={groupId} onChange={(event) => updateParams({ group: event.target.value, page: 1, source: '', catalog_source: '', port: '', device: '', left: '', right: '' })}><option value="">全部分组</option>{(groupsQuery.data ?? []).map((group) => <option key={group.id} value={group.id}>{group.name || group.id}</option>)}</select></label>
        <label className="field inline-field"><span>目录成员</span><select value={catalogSourceId} onChange={(event) => updateParams({ catalog_source: event.target.value, port: '', device: '', page: 1 })}><option value="">并集全部点位</option>{groupSources.map((source) => <option key={source.id} value={source.id}>仅 {source.name || source.id} 提供</option>)}</select></label>
        <label className="field inline-field"><span>串口</span><select value={portId} disabled={!catalogSourceId || topologyQuery.isLoading} onChange={(event) => updateParams({ port: event.target.value, device: '', page: 1 })}><option value="">{catalogSourceId ? '全部串口' : '先选目录成员'}</option>{portId && !ports.some((port) => port.portId === portId) ? <option value={portId}>{portId}（未发现）</option> : null}{ports.map((port) => <option key={port.portId} value={port.portId}>{port.portId}</option>)}</select></label>
        <label className="field inline-field"><span>设备</span><select value={deviceId} disabled={!portId} onChange={(event) => updateParams({ device: event.target.value, page: 1 })}><option value="">全部设备</option>{deviceId && !devices.some((device) => device.deviceId === deviceId) ? <option value={deviceId}>{deviceId}（未发现）</option> : null}{devices.map((device) => <option key={device.deviceId} value={device.deviceId}>{device.name || '设备'} · 地址 {device.deviceAddress}</option>)}</select></label>
        <label className="field inline-field"><span>质量</span><select value={compare ? '' : quality} disabled={compare} title={compare ? '双来源对比必须按同一并集目录分页' : undefined} onChange={(event) => updateParams({ quality: event.target.value, page: 1 })}><option value="">全部质量</option><option value="Good">正常</option><option value="Bad">异常</option></select></label>
        <label className="comparison-toggle"><input type="checkbox" checked={compare} onChange={(event) => updateParams({ compare: event.target.checked ? '1' : '', quality: event.target.checked ? '' : quality, page: 1 })} /><span>双来源对比</span></label>
        <button className="button button-secondary" type="button" disabled={compare ? !comparisonConfigured : !routeReady} onClick={refreshPoints}><RefreshCw aria-hidden="true" size={16} />刷新</button>
      </form>
      {catalogSourceId && topologyQuery.error ? <div className="form-error" role="alert">无法加载此来源的串口 / 设备选项，点位查询仍按当前筛选执行。<button className="table-link" type="button" onClick={() => void topologyQuery.refetch()}>重试拓扑</button></div> : null}
      {portId ? <p className="point-topology-hint">目录按 {catalogSourceId} 的串口 / 设备过滤；样本仍按下面指定的路由读取。不同下级的实际串口名可以不同。</p> : null}

      {compare ? <div className="route-view-toolbar">
        <strong>对比来源</strong>
        {!groupId ? <span className="text-warning">请先选择一个分组</span> : <>
          <label className="field inline-field"><span>左侧</span><select value={leftSourceId} onChange={(event) => updateParams({ left: event.target.value, page: 1 })}><option value="">选择来源</option>{groupSources.map((source) => <option key={source.id} value={source.id}>{source.name || source.id}</option>)}</select></label>
          <label className="field inline-field"><span>右侧</span><select value={rightSourceId} onChange={(event) => updateParams({ right: event.target.value, page: 1 })}><option value="">选择来源</option>{groupSources.map((source) => <option key={source.id} value={source.id}>{source.name || source.id}</option>)}</select></label>
        </>}
      </div> : <div className="route-view-toolbar">
        <strong>样本路由</strong>
        <label className="field inline-field"><span>查看方式</span><select value={view} onChange={(event) => updateParams({ view: event.target.value === 'default' ? '' : event.target.value, page: 1 })}><option value="default">当前默认源</option><option value="session">指定在线 Session</option><option value="source">指定来源（诊断）</option></select></label>
        {view === 'session' ? <label className="field inline-field route-picker"><span>Session</span><select value={sessionId} onChange={(event) => updateParams({ session: event.target.value, page: 1 })}><option value="">选择 Session</option>{sessionEnded ? <option value={sessionId}>{shortSessionId(sessionId)}（已结束）</option> : null}{(sessionsQuery.data ?? []).map((session) => <option key={session.id} value={session.id}>{shortClientName(session.clientName)} · {shortSessionId(session.id)}</option>)}</select></label> : null}
        {view === 'source' ? <label className="field inline-field route-picker"><span>来源</span><select value={routeSourceId} onChange={(event) => updateParams({ source: event.target.value, page: 1 })}><option value="">选择来源</option>{groupSources.map((source) => <option key={source.id} value={source.id}>{source.name || source.id}</option>)}</select></label> : null}
        <span className="route-view-hint">{view === 'default' ? '显示每个分组当前默认源的数据' : view === 'session' ? '忠实显示该上级 Session 实际读取的路由' : '不修改任何选源，仅用于检查指定下级数据'}</span>
      </div>}

      <section className="table-panel content-panel">
        {compare && !comparisonConfigured ? <EmptyState title="选择两个来源开始比较" description="对比限定在同一分组，不会修改默认源或任何在线 Session。" /> : sessionRequested && sessionsQuery.isPending ? <LoadingState label="正在核对目标 Session…" /> : sessionRequested && sessionsQuery.error ? <ErrorState error={sessionsQuery.error} onRetry={() => void sessionsQuery.refetch()} /> : sessionEnded ? <EmptyState title="目标 Session 已结束" description="原会话的旧点值已停止展示。请显式选择其他在线 Session；不会自动更换查看目标或回退到默认源。" /> : !compare && !routeReady ? <EmptyState title={view === 'session' ? '请选择在线 Session' : '请选择分组和来源'} description="选择只用于查看，不会修改默认源或任何 Session 路由。" /> : error ? <ErrorState error={error} onRetry={refreshPoints} /> : loading ? <LoadingState label={compare ? '正在加载同一查询的双来源点位…' : undefined} /> : compare ? (
          comparison.length === 0 ? <EmptyState title="没有匹配的点位" description="请调整搜索、目录成员或质量条件。" /> : <>
            <div className={`table-scroll${comparisonQuery.isFetching ? ' is-refreshing' : ''}`}>
              <table className="data-table comparison-table"><thead><tr><th>Node ID / 类型</th><th>{leftSourceId} 当前值</th><th>{leftSourceId} 质量 / 时间</th><th>{rightSourceId} 当前值</th><th>{rightSourceId} 质量 / 时间</th><th>比较</th></tr></thead><tbody>{comparison.map(({ left, right }) => {
                const point = left ?? right!;
                const different = !samePointValue(left, right);
                return <tr key={`${point.groupId}:${point.nodeId}`} className={different ? 'comparison-different virtual-row' : 'virtual-row'}>
                  <td><strong>{point.nodeId}</strong><span className="sub-cell">{point.dataType}</span></td>
                  <ComparisonValue point={left} />
                  <ComparisonQuality point={left} />
                  <ComparisonValue point={right} />
                  <ComparisonQuality point={right} />
                  <td className={different ? 'text-warning' : 'text-good'}>{different ? '不同' : '一致'}</td>
                </tr>;
              })}</tbody></table>
            </div>
            <Pagination page={activeData?.page ?? page} pageSize={PAGE_SIZE} total={activeData?.total ?? 0} onPageChange={(nextPage) => updateParams({ page: nextPage })} />
          </>
        ) : points.length === 0 ? (
          <EmptyState title={query || groupId || quality || catalogSourceId || portId || deviceId ? '没有匹配的点位' : '暂无点位目录'} description={query || groupId || quality || catalogSourceId || portId || deviceId ? '请调整搜索或筛选条件。' : '首次目录发现完成后，点位将显示在这里。'} />
        ) : (
          <>
            <div className={`table-scroll${pointsQuery.isFetching ? ' is-refreshing' : ''}`}>
              <table className="data-table responsive-table points-table">
                <thead><tr><th>节点 / BrowseName</th><th>逻辑组</th><th>当前来源</th><th>串口 / 设备</th><th>数据类型</th><th>当前值</th><th>UA 质量</th><th>源时间戳</th></tr></thead>
                <tbody>{points.map((point) => {
                  const isMethod = point.nodeClass.toLowerCase() === 'method';
                  return (
                    <tr key={`${point.groupId}:${point.nodeId}`} className="virtual-row">
                      <td data-label="节点 / BrowseName"><strong>{point.nodeId}</strong>{point.browseName !== point.nodeId ? <span className="sub-cell">{point.browseName}</span> : null}{point.deleted ? <span className="sub-cell text-bad">已退役</span> : null}</td>
                      <td>{point.groupId}</td>
                      <td>{point.selectedSourceId ?? '—'}{!point.sourcePresent ? <span className="sub-cell text-bad">选中源缺少该点</span> : null}</td>
                      <td><PointOwnership point={point} /></td>
                      <td>{isMethod ? 'Method' : point.dataType}{point.kind ? <span className="sub-cell">{point.kind === 'manager_calculation' ? '管理端计算' : point.kind === 'calculation' ? '下级计算点' : point.kind === 'command' ? '命令' : '采集点'}</span> : null}</td>
                      <td className="value-cell">{isMethod ? '—' : displayValue(point.value)}</td>
                      <td className={isMethod ? 'text-muted' : isBadStatus(point.statusCode) ? 'text-bad' : 'text-good'}>{isMethod ? '—' : statusCodeLabel(point.statusCode)}</td>
                      <td>{isMethod ? '—' : formatDateTime(point.sourceTimestamp)}</td>
                    </tr>
                  );
                })}</tbody>
              </table>
            </div>
            <Pagination page={pointsQuery.data?.page ?? page} pageSize={pointsQuery.data?.pageSize ?? PAGE_SIZE} total={pointsQuery.data?.total ?? 0} onPageChange={(nextPage) => updateParams({ page: nextPage })} />
          </>
        )}
      </section>
    </div>
  );
}

function joinComparison(left: Point[], right: Point[]): Array<{ left?: Point; right?: Point }> {
  const rows = new Map<string, { left?: Point; right?: Point }>();
  for (const point of left) rows.set(`${point.groupId}:${point.nodeId}`, { left: point });
  for (const point of right) {
    const key = `${point.groupId}:${point.nodeId}`;
    rows.set(key, { ...rows.get(key), right: point });
  }
  return [...rows.entries()].sort(([a], [b]) => a.localeCompare(b)).map(([, value]) => value);
}

function samePointValue(left?: Point, right?: Point): boolean {
  if (!left || !right) return false;
  return left.sourcePresent === right.sourcePresent && left.statusCode === right.statusCode && JSON.stringify(left.value) === JSON.stringify(right.value);
}

function ComparisonValue({ point }: { point?: Point }) {
  if (!point) return <td className="text-bad">点位不存在</td>;
  if (!point.sourcePresent) return <td className="text-bad">来源缺少该点</td>;
  return <td className="value-cell">{point.nodeClass.toLowerCase() === 'method' ? 'Method' : displayValue(point.value)}<span className="sub-cell"><PointOwnership point={point} /></span></td>;
}

function PointOwnership({ point }: { point: Point }) {
  if (!point.sourcePresent) return <span className="text-muted">选中源无此点</span>;
  if (point.portId) return <>{point.portId}<span className="sub-cell">{point.deviceId ? `设备 ${point.deviceId}` : point.kind === 'command' ? '共享串口命令' : '未指定设备'}</span></>;
  return <span className="text-muted">{point.kind === 'manager_calculation' ? '按默认来源集中计算' : point.kind === 'calculation' ? '跨设备 / 无设备计算' : point.kind === 'command' ? '共享命令' : '未提供归属'}</span>;
}

function ComparisonQuality({ point }: { point?: Point }) {
  if (!point || !point.sourcePresent) return <td className="text-bad">—</td>;
  return <td className={isBadStatus(point.statusCode) ? 'text-bad' : 'text-good'}>{statusCodeLabel(point.statusCode)}<span className="sub-cell">{formatDateTime(point.sourceTimestamp)}</span></td>;
}
