import { useEffect, useMemo, useState } from 'react';
import { Download, RefreshCw, Search } from 'lucide-react';
import { useSearchParams } from 'react-router-dom';
import { useLowerLogs, type LowerLogLevel } from '../api/lowerLogs';
import { useSources } from '../api/queries';
import { InlineNotice } from '../components/InlineNotice';
import { EmptyState, ErrorState, LoadingState } from '../components/PageState';
import { PageHeader } from '../components/PageHeader';
import { Pagination } from '../components/Pagination';
import { StatusBadge } from '../components/StatusBadge';
import { formatDateTime, formatRelativeTime } from '../lib/format';

const PAGE_SIZE = 100;

export default function LowerLogsPage() {
  const [params, setParams] = useSearchParams();
  const sources = useSources();
  const requestedSource = params.get('source') ?? '';
  const sourceId = requestedSource || sources.data?.[0]?.id || '';
  const page = Math.max(1, Number(params.get('page') ?? 1) || 1);
  const level = (params.get('level') ?? '') as LowerLogLevel;
  const query = params.get('q') ?? '';
  const portId = params.get('port') ?? '';
  const deviceId = params.get('device') ?? '';
  const [draft, setDraft] = useState({ query, portId, deviceId });
  const logs = useLowerLogs({ sourceId, level, query, portId, deviceId, page, pageSize: PAGE_SIZE });
  const selectedSource = useMemo(() => sources.data?.find((source) => source.id === sourceId), [sourceId, sources.data]);

  useEffect(() => {
    if (!requestedSource && sourceId) {
      const next = new URLSearchParams(params);
      next.set('source', sourceId);
      setParams(next, { replace: true });
    }
  }, [params, requestedSource, setParams, sourceId]);

  const update = (values: Record<string, string | number | undefined>) => {
    const next = new URLSearchParams(params);
    for (const [key, value] of Object.entries(values)) {
      if (value === undefined || value === '' || (key === 'page' && value === 1)) next.delete(key);
      else next.set(key, String(value));
    }
    setParams(next);
  };
  const pushState = logs.data?.pushState ?? 'never_received';
  const pushBadge = pushState === 'recent'
    ? <StatusBadge state="good" compact label="刚收到推送" />
    : pushState === 'stored'
      ? <StatusBadge state="warning" compact label="已有历史日志" />
      : <StatusBadge state="unknown" compact label="尚未收到推送" />;

  return <div className="page lower-logs-page">
    <PageHeader title="下级集中日志" description="下级通过独立 HTTP 队列批量发送；管理端按来源滚动保存，串口采集不会等待网络。" actions={<>
      <button className="button button-outline" type="button" disabled={!logs.data?.items.length} onClick={() => logs.data && exportCurrentPage(selectedSource?.name || sourceId, logs.data.items)}><Download size={16} />导出当前页</button>
      <button className="button button-secondary" type="button" disabled={logs.isFetching || !sourceId} onClick={() => void logs.refetch()}><RefreshCw size={16} />刷新</button>
    </>} />

    <InlineNotice><p>这里显示已推送到管理端的结构化日志。下级本机滚动日志仍完整保留；需要核对未推送区间时，可到“下级 OPC → 运维 → 日志”读取原始文件。</p></InlineNotice>

    <section className="lower-log-status content-panel" aria-label="日志接收状态">
      <div><span>当前来源</span><strong>{selectedSource?.name || sourceId || '—'}</strong><small>{sourceId || '等待来源配置'}</small></div>
      <div><span>接收状态</span>{pushBadge}<small>{logs.data?.lastReceivedAt ? `${formatRelativeTime(logs.data.lastReceivedAt)} · ${formatDateTime(logs.data.lastReceivedAt)}` : '启用下级 logging.http_push 后开始接收'}</small></div>
      <div><span>筛选结果</span><strong>{(logs.data?.total ?? 0).toLocaleString('zh-CN')}</strong><small>管理端保留的匹配记录</small></div>
      <div><span>文件异常行</span><strong className={(logs.data?.invalidLines ?? 0) > 0 ? 'text-warning' : ''}>{logs.data?.invalidLines ?? 0}</strong><small>损坏行会跳过，不影响其他日志</small></div>
    </section>

    <form className="toolbar filter-toolbar lower-log-filters" onSubmit={(event) => { event.preventDefault(); update({ q: draft.query.trim(), port: draft.portId.trim(), device: draft.deviceId.trim(), page: 1 }); }}>
      <label className="field inline-field"><span>下级来源</span><select value={sourceId} disabled={sources.isLoading} onChange={(event) => { setDraft({ query: '', portId: '', deviceId: '' }); update({ source: event.target.value, q: '', port: '', device: '', level: '', page: 1 }); }}><option value="">请选择来源</option>{(sources.data ?? []).map((source) => <option key={source.id} value={source.id}>{source.name || source.id} · {source.id}</option>)}</select></label>
      <label className="field inline-field"><span>等级</span><select value={level} onChange={(event) => update({ level: event.target.value, page: 1 })}><option value="">全部</option>{['TRACE', 'DEBUG', 'INFO', 'WARN', 'ERROR'].map((value) => <option key={value}>{value}</option>)}</select></label>
      <label className="field inline-field"><span>串口</span><input value={draft.portId} placeholder="/dev/ttyWCH0" onChange={(event) => setDraft((value) => ({ ...value, portId: event.target.value }))} /></label>
      <label className="field inline-field"><span>站点</span><input value={draft.deviceId} placeholder="13" onChange={(event) => setDraft((value) => ({ ...value, deviceId: event.target.value }))} /></label>
      <label className="search-field"><Search size={17} /><input value={draft.query} placeholder="搜索消息、组件、串口或站点" onChange={(event) => setDraft((value) => ({ ...value, query: event.target.value }))} /><button type="submit">筛选</button></label>
    </form>

    <section className="table-panel content-panel">
      {!sourceId ? <EmptyState title="尚未配置下级来源" description="先在“下级 OPC”添加来源，再为对应下级启用 HTTP 日志推送。" />
        : logs.isLoading && !logs.data ? <LoadingState label="正在读取管理端集中日志…" />
          : logs.error ? <ErrorState error={logs.error} title="无法读取集中日志" onRetry={() => void logs.refetch()} />
            : !logs.data?.items.length ? <EmptyState title="没有匹配的集中日志" description={logs.data?.pushState === 'never_received' ? '该来源尚未向管理端推送日志；原始文件仍可通过下级运维读取。' : '请调整等级、串口、站点或关键字筛选。'} />
              : <><div className="table-scroll"><table className="data-table lower-log-table"><thead><tr><th>发生时间</th><th>等级</th><th>组件</th><th>串口 / 站点</th><th>日志信息</th><th>接收</th></tr></thead><tbody>{logs.data.items.map((item) => <tr key={`${item.bootId}-${item.sequence}`}>
                <td>{formatDateTime(item.timestamp)}<span className="sub-cell">#{item.sequence} · {item.bootId.slice(0, 12)}</span></td>
                <td><span className={`log-level log-${item.level.toLowerCase()}`}>{item.level}</span></td>
                <td><code>{item.component || '—'}</code></td>
                <td>{item.portId || '—'}<span className="sub-cell">站点 {item.deviceId || '—'}</span></td>
                <td className="message-cell lower-log-message"><span>{item.message}</span>{item.fields && Object.keys(item.fields).length ? <code>{JSON.stringify(item.fields)}</code> : null}</td>
                <td>{formatRelativeTime(item.receivedAt)}<span className="sub-cell">{formatDateTime(item.receivedAt)}</span></td>
              </tr>)}</tbody></table></div><Pagination page={logs.data.page} pageSize={logs.data.pageSize} total={logs.data.total} onPageChange={(value) => update({ page: value })} /></>}
    </section>
  </div>;
}

function exportCurrentPage(sourceName: string, items: Array<{ timestamp: string; level: string; component: string; portId?: string; deviceId?: string; message: string; receivedAt: string }>) {
  const quote = (value: unknown) => `"${String(value ?? '').replaceAll('"', '""')}"`;
  const rows = [['发生时间', '等级', '组件', '串口', '站点', '信息', '管理端接收时间'], ...items.map((item) => [item.timestamp, item.level, item.component, item.portId ?? '', item.deviceId ?? '', item.message, item.receivedAt])];
  const blob = new Blob([`\uFEFF${rows.map((row) => row.map(quote).join(',')).join('\r\n')}`], { type: 'text/csv;charset=utf-8' });
  const anchor = document.createElement('a');
  anchor.href = URL.createObjectURL(blob);
  anchor.download = `${sourceName || 'lower'}-central-logs-${new Date().toISOString().slice(0, 10)}.csv`;
  anchor.click();
  URL.revokeObjectURL(anchor.href);
}
