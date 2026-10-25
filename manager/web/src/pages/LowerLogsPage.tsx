import { useEffect, useMemo, useState } from 'react';
import { Download, RefreshCw, Search } from 'lucide-react';
import { Link, useSearchParams } from 'react-router-dom';
import { apiErrorMessage } from '../api/client';
import { useLowerLogs, type LowerLogItem, type LowerLogLevel } from '../api/lowerLogs';
import { useLoggingLevelMutation, useLoggingStatus, useOperationsCapabilities, type LoggingLevel } from '../api/operations';
import { useSources, useSourceTopology } from '../api/queries';
import type { Source } from '../api/types';
import { InlineNotice } from '../components/InlineNotice';
import { EmptyState, ErrorState, LoadingState } from '../components/PageState';
import { PageHeader } from '../components/PageHeader';
import { Pagination, readPageSize } from '../components/Pagination';
import { StatusBadge } from '../components/StatusBadge';
import { downloadCsv } from '../lib/download';
import { formatDateTime, formatRelativeTime } from '../lib/format';
import { humanizeTechnicalMessage } from '../lib/labels';


const LOG_LEVELS: Array<{ value: LoggingLevel; label: string; description: string }> = [
  { value: 'error', label: 'ERROR', description: '只记录错误' },
  { value: 'warn', label: 'WARN', description: '错误和告警' },
  { value: 'info', label: 'INFO', description: '推荐日常运行' },
  { value: 'debug', label: 'DEBUG', description: '详细排障' },
  { value: 'trace', label: 'TRACE', description: '最详细，日志量很大' },
];

export default function LowerLogsPage() {
  const [params, setParams] = useSearchParams();
  const PAGE_SIZE = readPageSize(params, 100);
  const sources = useSources();
  const requestedSource = params.get('source') ?? '';
  const sourceId = requestedSource || sources.data?.[0]?.id || '';
  const requestedPage = Number(params.get('page') ?? 1);
  const page = Number.isSafeInteger(requestedPage) && requestedPage > 0 ? requestedPage : 1;
  const level = (params.get('level') ?? '') as LowerLogLevel;
  const query = params.get('q') ?? '';
  const portId = params.get('port') ?? '';
  const deviceId = params.get('device') ?? '';
  const range = params.get('range') ?? '1h';
  const customFrom = params.get('from') ?? '';
  const customTo = params.get('to') ?? '';
  const [draft, setDraft] = useState({ query, portId, deviceId });
  const [refreshRevision, setRefreshRevision] = useState(0);
  const bounds = useMemo(() => logTimeBounds(range, customFrom, customTo), [range, customFrom, customTo, refreshRevision]);
  const timeError = bounds.from && bounds.to && new Date(bounds.from) > new Date(bounds.to) ? '开始时间不能晚于结束时间；今天 08:00 之前请改选最近 1 小时。' : '';
  const logs = useLowerLogs({ sourceId, level, query, portId, deviceId, ...bounds, page, pageSize: PAGE_SIZE }, !timeError);
  const groupedLogs = useMemo(() => groupRepeatedLogs(logs.data?.items ?? []), [logs.data?.items]);
  const selectedSource = useMemo(() => sources.data?.find((source) => source.id === sourceId), [sourceId, sources.data]);
  const topology = useSourceTopology(sourceId);
  const ports = topology.data?.ports ?? [];
  const devices = ports.find((port) => port.portId === draft.portId)?.devices ?? [];

  useEffect(() => {
    if (!requestedSource && sourceId) {
      const next = new URLSearchParams(params);
      next.set('source', sourceId);
      setParams(next, { replace: true });
    }
  }, [params, requestedSource, setParams, sourceId]);

  useEffect(() => {
    setDraft((current) => current.query === query && current.portId === portId && current.deviceId === deviceId
      ? current
      : { query, portId, deviceId });
  }, [deviceId, portId, query]);

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
  const showLatest = () => {
    setRefreshRevision((value) => value + 1);
    setDraft({ query: '', portId: '', deviceId: '' });
    update({ q: '', port: '', device: '', level: '', range: '1h', from: '', to: '', page: 1 });
  };
  const refreshLogs = () => {
    if (range === 'all' || range === 'custom') { void logs.refetch(); return; }
    setRefreshRevision((value) => value + 1);
    update({ page: 1 });
  };

  return <div className="page lower-logs-page">
    <PageHeader title="下级集中日志" description="下级通过独立 HTTP 队列批量发送；管理端按来源滚动保存，串口采集不会等待网络。" actions={<>
      <button className="button button-outline" type="button" disabled={logs.isFetching || !!timeError || !logs.data?.items.length} onClick={() => logs.data && exportCurrentPage(selectedSource?.name || sourceId, logs.data.items)}><Download size={16} />导出当前页</button>
      <button className="button button-outline" type="button" disabled={!sourceId || logs.isFetching} onClick={showLatest}>显示最新日志</button>
      <button className="button button-secondary" type="button" disabled={logs.isFetching || !sourceId || !!timeError} onClick={refreshLogs}><RefreshCw size={16} />刷新</button>
    </>} />

    <InlineNotice><p>这里显示已推送到管理端的结构化日志。下级本机滚动日志仍完整保留；需要核对未推送区间时，可到“下级 OPC → 运维 → 日志”读取原始文件。</p></InlineNotice>

    <section className="lower-log-status content-panel" aria-label="日志接收状态">
      <div><span>当前来源</span><strong>{selectedSource?.name || sourceId || '—'}</strong><small>{sourceId || '等待来源配置'}</small></div>
      <div><span>接收状态</span>{pushBadge}<small>{logs.data?.lastReceivedAt ? `${formatRelativeTime(logs.data.lastReceivedAt)} · ${formatDateTime(logs.data.lastReceivedAt)}` : '启用下级 logging.http_push 后开始接收'}</small></div>
      <div><span>筛选结果</span><strong>{(logs.data?.total ?? 0).toLocaleString('zh-CN')}</strong><small>管理端保留的匹配记录</small></div>
      <div><span>文件异常行</span><strong className={(logs.data?.invalidLines ?? 0) > 0 ? 'text-warning' : ''}>{logs.data?.invalidLines ?? 0}</strong><small>损坏行会跳过，不影响其他日志</small></div>
    </section>

    <details className="secondary-settings"><summary>调整下级日志等级（与查看筛选无关）</summary><SourceLoggingControl key={sourceId} source={selectedSource} /></details>

    <form className="toolbar filter-toolbar lower-log-filters" onSubmit={(event) => { event.preventDefault(); update({ q: draft.query.trim(), port: draft.portId.trim(), device: draft.deviceId.trim(), page: 1 }); }}>
      <label className="field inline-field"><span>下级来源</span><select value={sourceId} disabled={sources.isLoading} onChange={(event) => { setDraft({ query: '', portId: '', deviceId: '' }); update({ source: event.target.value, q: '', port: '', device: '', level: '', page: 1 }); }}><option value="">请选择来源</option>{(sources.data ?? []).map((source) => <option key={source.id} value={source.id}>{source.name || source.id} · {source.id}</option>)}</select></label>
      <label className="field inline-field"><span>等级</span><select value={level} onChange={(event) => update({ level: event.target.value, page: 1 })}><option value="">全部</option>{['TRACE', 'DEBUG', 'INFO', 'WARN', 'ERROR'].map((value) => <option key={value}>{value}</option>)}</select></label>
      <label className="field inline-field"><span>串口</span><select value={draft.portId} disabled={topology.isPending} onChange={(event) => { setDraft((value) => ({ ...value, portId: event.target.value, deviceId: '' })); update({ port: event.target.value, device: '', page: 1 }); }}><option value="">全部串口</option>{draft.portId && !ports.some((port) => port.portId === draft.portId) ? <option value={draft.portId}>{draft.portId}（历史筛选）</option> : null}{ports.map((port) => <option key={port.portId} value={port.portId}>{port.portName || port.portId}</option>)}</select></label>
      <label className="field inline-field"><span>站点</span><select value={draft.deviceId} disabled={!draft.portId} onChange={(event) => { setDraft((value) => ({ ...value, deviceId: event.target.value })); update({ device: event.target.value, page: 1 }); }}><option value="">{draft.portId ? '全部站点' : '请先选择串口'}</option>{draft.deviceId && !devices.some((device) => device.deviceId === draft.deviceId) ? <option value={draft.deviceId}>{draft.deviceId}（历史筛选）</option> : null}{devices.map((device) => <option key={device.deviceId} value={device.deviceId}>{device.deviceId} · {device.name}</option>)}</select></label>
      <label className="field inline-field"><span>时间范围</span><select value={range} onChange={(event) => update({ range: event.target.value, from: '', to: '', page: 1 })}><option value="1h">最近 1 小时</option><option value="24h">最近 24 小时</option><option value="today8">今天 08:00 起</option><option value="all">全部日志</option><option value="custom">自定义</option></select></label>
      {range === 'custom' ? <><label className="field inline-field"><span>开始</span><input type="datetime-local" value={logLocalInput(customFrom)} onChange={(event) => update({ from: logISO(event.target.value), page: 1 })} /></label><label className="field inline-field"><span>结束</span><input type="datetime-local" value={logLocalInput(customTo)} onChange={(event) => update({ to: logISO(event.target.value), page: 1 })} /></label></> : null}
      <label className="search-field"><Search size={17} /><input value={draft.query} placeholder="搜索消息、组件、串口或站点" onChange={(event) => setDraft((value) => ({ ...value, query: event.target.value }))} /><button type="submit">筛选</button></label>
      <span className="toolbar-order-note">最新在前</span>
    </form>
    <div className="query-context" role="status">{timeError || `当前查询：${bounds.from ? formatDateTime(bounds.from) : '最早记录'} 至 ${bounds.to ? formatDateTime(bounds.to) : '现在'}。翻页时保持时间范围；点击刷新获取最新区间。`}</div>
    {sources.error ? <ErrorState error={sources.error} onRetry={() => void sources.refetch()} /> : null}
    {topology.error ? <ErrorState error={topology.error} title="串口与站点选项读取失败" onRetry={() => void topology.refetch()} /> : null}

    <section className="table-panel content-panel">
      {timeError ? <EmptyState title="请调整查询时间" description={timeError} /> : !sourceId ? <EmptyState title="尚未配置下级来源" description="先在“下级 OPC”添加来源，再为对应下级启用 HTTP 日志推送。" />
        : logs.isLoading && !logs.data ? <LoadingState label="正在读取管理端集中日志…" />
          : logs.error ? <ErrorState error={logs.error} title="无法读取集中日志" onRetry={() => void logs.refetch()} />
            : !logs.data?.items.length ? <EmptyState title="没有匹配的集中日志" description={logs.data?.pushState === 'never_received' ? '该来源尚未向管理端推送日志；原始文件仍可通过下级运维读取。' : '请调整等级、串口、站点或关键字筛选。'} />
              : <><div className="table-scroll"><table className="data-table responsive-table lower-log-table"><thead><tr><th>发生时间</th><th>等级</th><th>组件</th><th>串口 / 站点</th><th>日志信息</th><th>接收</th></tr></thead><tbody>{groupedLogs.map(({ item, count, oldestAt }) => <tr key={`${item.bootId}-${item.sequence}`}>
                <td data-label="发生时间">{formatDateTime(item.timestamp)}<span className="sub-cell">#{item.sequence} · {item.bootId.slice(0, 12)}</span>{count > 1 ? <span className="sub-cell">连续 {count} 次 · 至 {formatDateTime(oldestAt)}</span> : null}</td>
                <td data-label="等级"><span className={`log-level log-${item.level.toLowerCase()}`}>{item.level}</span></td>
                <td data-label="组件"><code>{item.component || '—'}</code></td>
                <td data-label="串口 / 站点">{item.portId || '—'}<span className="sub-cell">站点 {item.deviceId || '—'}</span></td>
                <td data-label="日志信息" className="message-cell lower-log-message"><span>{humanizeTechnicalMessage(item.message)}</span>{item.fields && Object.keys(item.fields).length ? <details><summary>结构化字段</summary><pre>{JSON.stringify(item.fields, null, 2)}</pre></details> : null}</td>
                <td data-label="接收">{formatRelativeTime(item.receivedAt)}<span className="sub-cell">{formatDateTime(item.receivedAt)}</span></td>
              </tr>)}</tbody></table></div><Pagination onPageSizeChange={(size) => update({ size, page: 1 })} page={logs.data.page} pageSize={logs.data.pageSize} total={logs.data.total} onPageChange={(value) => update({ page: value })} /></>}
      {!timeError && !logs.error && logs.data && !logs.data.items.length ? <Pagination onPageSizeChange={(size) => update({ size, page: 1 })} page={page} pageSize={PAGE_SIZE} total={logs.data.total} onPageChange={(value) => update({ page: value })} /> : null}
    </section>
  </div>;
}

function logTimeBounds(range: string, customFrom: string, customTo: string): { from?: string; to?: string } {
  const now = new Date();
  if (range === 'all') return {};
  if (range === 'custom') return { from: customFrom || undefined, to: customTo || undefined };
  if (range === 'today8') {
    const from = new Date(now);
    from.setHours(8, 0, 0, 0);
    return { from: from.toISOString(), to: now.toISOString() };
  }
  const hours = range === '24h' ? 24 : 1;
  return { from: new Date(now.getTime() - hours * 3_600_000).toISOString(), to: now.toISOString() };
}

function logLocalInput(value: string): string {
  if (!value) return '';
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return '';
  return new Date(date.getTime() - date.getTimezoneOffset() * 60_000).toISOString().slice(0, 16);
}

function logISO(value: string): string {
  if (!value) return '';
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? '' : date.toISOString();
}

function groupRepeatedLogs(items: LowerLogItem[]): Array<{ item: LowerLogItem; count: number; oldestAt: string }> {
  const result: Array<{ item: LowerLogItem; count: number; oldestAt: string }> = [];
  for (const item of items) {
    const signature = JSON.stringify([item.level, item.component, item.portId, item.deviceId, item.message, item.fields ?? {}]);
    const previous = result[result.length - 1];
    const previousSignature = previous ? JSON.stringify([previous.item.level, previous.item.component, previous.item.portId, previous.item.deviceId, previous.item.message, previous.item.fields ?? {}]) : '';
    const closeInTime = previous ? Math.abs(new Date(previous.oldestAt).getTime() - new Date(item.timestamp).getTime()) <= 5 * 60_000 : false;
    if (previous && signature === previousSignature && closeInTime) {
      previous.count += 1;
      previous.oldestAt = item.timestamp;
    } else result.push({ item, count: 1, oldestAt: item.timestamp });
  }
  return result;
}

function SourceLoggingControl({ source }: { source?: Source }) {
  const target = useMemo(() => ({ id: source?.id ?? '', revision: source?.revision ?? 0 }), [source?.id, source?.revision]);
  const capabilities = useOperationsCapabilities(target, Boolean(source));
  const supported = capabilities.data?.features.includes('logging_configuration') ?? false;
  const logging = useLoggingStatus(target, supported);
  const mutation = useLoggingLevelMutation(target);
  const [desired, setDesired] = useState<LoggingLevel | ''>('');
  // The config revision the operator chose a level against; it is the CAS value saved with it.
  const [baseline, setBaseline] = useState<string>();
  const [notice, setNotice] = useState('');
  const edited = baseline !== undefined;
  const changedElsewhere = edited && Boolean(logging.data) && logging.data?.config_revision !== baseline;

  // Follow the saved configuration only until the operator picks a level.
  useEffect(() => {
    const configured = logging.data?.configured_level;
    if (!edited) setDesired(configured && configured !== 'custom' ? configured : '');
  }, [logging.data?.configured_level, edited]);

  useEffect(() => { setNotice(''); setBaseline(undefined); }, [source?.id]);

  const save = async (event: React.FormEvent) => {
    event.preventDefault();
    if (!logging.data || !desired) return;
    setNotice('');
    try {
      const result = await mutation.mutateAsync({ revision: baseline ?? logging.data.config_revision, level: desired });
      setBaseline(undefined);
      setNotice(result.status === 'Unchanged' ? '日志等级没有变化。' : '日志等级已保存；重启该下级后生效。');
    } catch (error) {
      setNotice(apiErrorMessage(error));
    }
  };

  return <section id="lower-log-control" className="content-panel lower-log-control" aria-label="下级日志等级">
    <div className="panel-heading"><div><h2>下级日志等级</h2><p>单独修改当前物理下级的日志详细程度，使用配置 revision 防止覆盖其他人的修改。</p></div></div>
    {!source ? <EmptyState title="请选择下级来源" />
      : capabilities.isPending ? <LoadingState label="正在读取下级日志能力…" />
        : capabilities.error ? <ErrorState error={capabilities.error} title="无法读取下级日志能力" onRetry={() => void capabilities.refetch()} />
          : !supported ? <InlineNotice tone="warning"><strong>该下级版本暂不支持快捷调整日志等级。</strong><p>仍可在“下级 OPC → 运维 → 配置”编辑 config.toml 的 logging.level。</p></InlineNotice>
            : logging.isPending ? <LoadingState label="正在读取当前日志等级…" />
              : logging.error ? <ErrorState error={logging.error} title="无法读取当前日志等级" onRetry={() => void logging.refetch()} />
                : logging.data ? <>
                  <div className="lower-log-control-status">
                    <div><span>当前运行</span><strong>{logging.data.active_level.toUpperCase()}</strong><code>{logging.data.active_spec}</code></div>
                    <div><span>已保存配置</span><strong>{logging.data.configured_level.toUpperCase()}</strong><code>{logging.data.configured_spec}</code></div>
                    <div><span>HTTP 集中推送</span><strong>{logging.data.http_push_enabled ? '已启用' : '未启用'}</strong><code>{logging.data.http_push_source_id || '未设置 source_id'}</code></div>
                  </div>
                  {logging.data.environment_override ? <InlineNotice tone="warning"><strong>当前存在 RUST_LOG 环境变量覆盖。</strong><p>保存的等级不会接管运行日志，需先清除该环境变量再重启。</p></InlineNotice> : null}
                  {logging.data.restart_required ? <InlineNotice tone="warning"><strong>配置尚未应用。</strong><p>当前运行仍为 {logging.data.active_level.toUpperCase()}；请到 <Link to="/sources">下级 OPC → 运维 → 进程</Link> 明确重启该下级。</p></InlineNotice> : null}
                  <form className="lower-log-level-form" onSubmit={(event) => void save(event)}>
                    <label className="field"><span>保存的日志等级</span><select aria-label="保存的日志等级" value={desired} disabled={mutation.isPending} onChange={(event) => { setDesired(event.target.value as LoggingLevel); setBaseline((old) => old ?? logging.data?.config_revision); }}>
                      {desired === '' ? <option value="">当前为自定义规则</option> : null}
                      {LOG_LEVELS.map((item) => <option key={item.value} value={item.value}>{item.label} · {item.description}</option>)}
                    </select></label>
                    <button className="button button-primary" type="submit" disabled={!desired || mutation.isPending || desired === logging.data.configured_level}>{mutation.isPending ? '正在保存…' : '保存等级'}</button>
                    <span className="operations-muted">DEBUG/TRACE 仅建议短时排障，集中日志和本机文件都会明显增大。</span>
                  </form>
                  {changedElsewhere ? <InlineNotice tone="warning"><p>选择等级后，该下级配置已被他人修改；按原版本保存会被拒绝。</p><button className="button button-small button-secondary" type="button" onClick={() => { setBaseline(undefined); setNotice(''); }}>放弃选择并按当前配置重新核对</button></InlineNotice> : null}
                  {notice ? <InlineNotice tone={mutation.error ? 'error' : 'info'}><p>{notice}</p></InlineNotice> : null}
                </> : null}
  </section>;
}

function exportCurrentPage(sourceName: string, items: Array<{ timestamp: string; level: string; component: string; portId?: string; deviceId?: string; message: string; receivedAt: string }>) {
  downloadCsv(`${sourceName || 'lower'}-central-logs-${new Date().toISOString().slice(0, 10)}.csv`, [['发生时间', '等级', '组件', '串口', '站点', '信息', '管理端接收时间'], ...items.map((item) => [item.timestamp, item.level, item.component, item.portId ?? '', item.deviceId ?? '', item.message, item.receivedAt])]);
}
