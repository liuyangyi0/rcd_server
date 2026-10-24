import { useEffect, useState } from 'react';
import { Download, RefreshCw, Search } from 'lucide-react';
import { ApiError, apiErrorMessage } from '../../api/client';
import { MAX_LOG_PAGE_BYTES, MIN_LOG_PAGE_BYTES, useOperationLog, useOperationLogs, type LogFile, type LogFiles, type LogRequest, type OperationsTarget } from '../../api/operations';
import { EmptyState, ErrorState, LoadingState } from '../../components/PageState';
import { InlineNotice } from '../../components/InlineNotice';
import { formatDateTime } from '../../lib/format';
import { byteSize, downloadText } from './shared';

interface Props { target: OperationsTarget; maxBytes: number }
const FOLLOW_DURATION_MS = 5 * 60_000;

export function LogsTab(props: Props) {
  const files = useOperationLogs(props.target);
  return <div className="operations-tab-content">
    <InlineNotice><p>日志按字节分页，单页最多 64 KiB；仅保留当前页，不会持续追加到浏览器全局状态。日志轮转后必须重新从尾部读取。</p></InlineNotice>
    {files.error ? <ErrorState error={files.error} title="无法读取日志文件列表" onRetry={() => void files.refetch()} /> : null}
    {!files.data && files.isPending ? <LoadingState label="正在读取日志文件列表…" /> : files.data ? <LogWorkspace {...props} files={files.data} refreshing={files.isFetching} refreshFiles={async () => (await files.refetch({ throwOnError: true })).data} /> : null}
  </div>;
}

function LogWorkspace({ target, maxBytes, files, refreshing, refreshFiles }: Props & { files: LogFiles; refreshing: boolean; refreshFiles: () => Promise<LogFiles | undefined> }) {
  const [selected, setSelected] = useState(() => files.items[0]?.name || '');
  const file = files.items.find((item) => item.name === selected);
  return <>
    <div className="operations-toolbar"><label className="field operations-file-picker"><span>日志文件</span><select aria-label="日志文件" value={selected} disabled={!files.items.length} onChange={(event) => setSelected(event.target.value)}>{!selected ? <option value="">请选择日志文件</option> : !file ? <option value={selected}>{selected}（文件已变化）</option> : null}{files.items.map((item) => <option key={item.name} value={item.name}>{item.name} · {byteSize(item.size_bytes)}</option>)}</select></label><button className="button button-secondary button-small" type="button" disabled={refreshing} onClick={() => void refreshFiles().catch(() => undefined)}><RefreshCw size={15} aria-hidden="true" />刷新日志文件列表</button></div>
    {!files.items.length ? <EmptyState title="暂无日志文件" description="下级尚未产生日志，或当前没有可读取的日志文件。" /> : !file ? <EmptyState title={selected ? '所选日志文件已不存在' : '请选择日志文件'} description="请明确选择文件；不会把其他文件的内容拼接到旧日志中。" /> : <LogViewer key={file.name} target={target} file={file} maxBytes={maxBytes} refreshFiles={refreshFiles} />}
  </>;
}

function LogViewer({ target, file, maxBytes, refreshFiles }: Props & { file: LogFile; refreshFiles: () => Promise<LogFiles | undefined> }) {
  const limit = Math.max(MIN_LOG_PAGE_BYTES, Math.min(MAX_LOG_PAGE_BYTES, maxBytes || MAX_LOG_PAGE_BYTES));
  const [request, setRequest] = useState<LogRequest>(() => ({ name: file.name, generation: file.generation, offset: 0, limit: Math.min(32_768, limit), q: '', level: '', tail: true }));
  const [filters, setFilters] = useState({ q: '', level: '' });
  const [refreshKey, setRefreshKey] = useState(0);
  const [followUntil, setFollowUntil] = useState(0);
  const [resetting, setResetting] = useState(false);
  const [localError, setLocalError] = useState('');
  const query = useOperationLog(target, request, followUntil > 0, refreshKey);
  const page = query.data;
  const failed = Boolean(query.error);
  const sourceConflict = query.error instanceof ApiError && query.error.code === 'source_revision_conflict';
  const rotated = !sourceConflict && (file.generation !== request.generation || (query.error instanceof ApiError && query.error.status === 409));
  const pagingDisabled = query.isFetching || resetting || failed || rotated || !page;
  const pageSizes = [...new Set([4_096, 16_384, 32_768, 65_536].map((size) => Math.min(size, limit)))];

  useEffect(() => {
    if (!followUntil) return;
    const timer = window.setTimeout(() => setFollowUntil(0), Math.max(0, followUntil - Date.now()));
    return () => window.clearTimeout(timer);
  }, [followUntil]);
  useEffect(() => { if (failed || rotated) setFollowUntil(0); }, [failed, rotated]);

  const fromTail = async (follow = false) => {
    setFollowUntil(0); setResetting(true); setLocalError('');
    try {
      const latest = await refreshFiles();
      const current = latest?.items.find((item) => item.name === file.name);
      if (!current) throw new Error('日志文件已不存在，请重新选择文件');
      setRequest((previous) => ({ ...previous, generation: current.generation, offset: 0, tail: true }));
      setRefreshKey((previous) => previous + 1);
      if (follow) setFollowUntil(Date.now() + FOLLOW_DURATION_MS);
    } catch (error) { setLocalError(apiErrorMessage(error)); }
    finally { setResetting(false); }
  };
  const navigate = (offset: number) => {
    setFollowUntil(0);
    setRequest((previous) => ({ ...previous, offset: Math.max(0, offset), tail: false }));
    setRefreshKey((previous) => previous + 1);
  };

  return <section className="operations-log-viewer" aria-label={`日志查看 ${file.name}`}>
    <p className="operations-muted">文件更新时间：{formatDateTime(file.modified_at)} · 代次：<code>{request.generation}</code></p>
    <form className="operations-log-filters" onSubmit={(event) => {
      event.preventDefault(); setFollowUntil(0); setLocalError('');
      setRequest((previous) => ({ ...previous, q: filters.q.trim(), level: filters.level, offset: 0, tail: true }));
      setRefreshKey((previous) => previous + 1);
    }}><label className="field"><span>关键字</span><input aria-label="日志关键字" value={filters.q} maxLength={256} placeholder="按日志文本筛选" onChange={(event) => setFilters((previous) => ({ ...previous, q: event.target.value }))} /></label><label className="field"><span>等级</span><select aria-label="日志等级" value={filters.level} onChange={(event) => setFilters((previous) => ({ ...previous, level: event.target.value }))}><option value="">全部等级</option>{['TRACE', 'DEBUG', 'INFO', 'WARN', 'ERROR'].map((level) => <option key={level} value={level}>{level}</option>)}</select></label><button className="button button-secondary" type="submit" disabled={query.isFetching || resetting || rotated}><Search size={15} aria-hidden="true" />应用日志筛选</button></form>
    <div className="operations-toolbar operations-log-actions"><label className="field inline-field"><span>每页上限</span><select aria-label="日志每页大小" value={request.limit} disabled={resetting || query.isFetching} onChange={(event) => { setFollowUntil(0); setRequest((previous) => ({ ...previous, limit: Number(event.target.value), offset: 0, tail: true })); }}>{pageSizes.map((size) => <option key={size} value={size}>{byteSize(size)}</option>)}</select></label><button className="button button-small button-secondary" type="button" disabled={query.isFetching || resetting} onClick={() => { setFollowUntil(0); void Promise.all([query.refetch(), refreshFiles()]).catch((error: unknown) => setLocalError(apiErrorMessage(error))); }}><RefreshCw size={15} aria-hidden="true" />刷新当前页</button><button className="button button-small button-outline" type="button" disabled={resetting || query.isFetching} onClick={() => void fromTail()}>重新从尾部读取</button><button className="button button-small button-outline" type="button" disabled={!page || query.isFetching || failed} onClick={() => page && downloadText(`${target.id}-${file.name}-${page.offset}.log`, page.text)}><Download size={15} aria-hidden="true" />下载当前页</button></div>
    <label className="operations-check"><input type="checkbox" checked={followUntil > 0} disabled={resetting || query.isFetching || rotated || failed} onChange={(event) => { if (event.target.checked) void fromTail(true); else setFollowUntil(0); }} />尾部轮询（每 5 秒，最多 5 分钟；错误或离开标签即停止）</label>
    {rotated ? <div className="operations-confirm-inline" role="alert"><strong>日志已轮转或代次冲突。</strong><p>分页与轮询已停止，旧内容不会与新日志拼接。请点击“重新从尾部读取”。</p></div> : query.error ? <ErrorState error={query.error} title="读取日志失败，轮询已停止" onRetry={() => { setFollowUntil(0); void query.refetch(); }} /> : null}
    {localError ? <div className="top-error" role="alert">{localError}</div> : null}
    {sourceConflict && page ? <InlineNotice tone="warning">来源配置已改变。下方保留旧来源版本的最后成功页，仅供核对；不会显示新 Endpoint 的日志。</InlineNotice> : null}
    {!page && query.isPending ? <LoadingState label="正在读取一页日志…" /> : page && (!failed || sourceConflict) ? <>
      <div className="operations-log-range"><span>字节 {page.offset.toLocaleString()}–{page.next_offset.toLocaleString()} / {page.size_bytes.toLocaleString()}{page.eof ? ' · 已到文件末尾' : ''}</span>{query.isFetching ? <span role="status">正在刷新…</span> : null}</div>
      {page.text ? <pre className="operations-log-text" tabIndex={0} aria-label="日志内容">{page.text}</pre> : <EmptyState title={request.q || request.level ? '当前扫描页没有匹配日志' : '当前日志页为空'} description={page.eof ? '已到文件末尾，可手动刷新或重新从尾部读取。' : '可继续下一页；筛选只作用于服务端扫描的有限范围。'} />}
      {page.truncated ? <p className="operations-muted">本页达到扫描或返回上限。可继续翻页；页面不会自动追加全文。</p> : null}
    </> : null}
    <div className="operations-log-pagination"><button className="button button-small button-secondary" type="button" disabled={pagingDisabled} onClick={() => navigate(0)}>从文件开头读取</button><button className="button button-small button-outline" type="button" disabled={pagingDisabled || (page?.offset ?? 0) <= 0} onClick={() => page && navigate(page.offset - request.limit)}>上一页</button><button className="button button-small button-outline" type="button" disabled={pagingDisabled || page?.eof || (page ? page.next_offset <= page.offset : true)} onClick={() => page && navigate(page.next_offset)}>下一页</button></div>
  </section>;
}
