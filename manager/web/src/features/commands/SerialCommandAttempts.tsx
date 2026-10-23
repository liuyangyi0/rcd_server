import { useState } from 'react';
import { Link } from 'react-router-dom';
import { useSources } from '../../api/queries';
import { useLowerLogs } from '../../api/lowerLogs';
import { EmptyState, ErrorState, LoadingState } from '../../components/PageState';
import { Pagination } from '../../components/Pagination';
import { formatDateTime } from '../../lib/format';

interface Summary {
  schema_version: number; ack_id: string | null; port_id: string; device_id: string;
  request: string; request_truncated: boolean; request_bytes: number;
  sent_count: number; attempt_count: number; max_attempts: number;
  result: string; last_error: string; duration_ms: number;
  failure_count: number; failures: { attempt: number; reason: string }[];
}

const reasons: Record<string, string> = {
  ack_timeout: '等待 ACK 超时', ack_mismatch: '收到数据但未匹配 00 50 50',
  ack_io_error: '读取 ACK 时串口 I/O 错误', port_not_open: '串口未打开',
  write_failed: '串口发送失败（可能只写出部分字节）', deadline_expired: '命令执行期限已到',
};

export function parseCommandSummary(message: string): Summary | undefined {
  try {
    const value = JSON.parse(message) as Summary;
    if (value?.schema_version !== 1 || !Number.isInteger(value.sent_count) || value.sent_count < 0 ||
      !Number.isInteger(value.attempt_count) || value.attempt_count < value.sent_count ||
      !Number.isInteger(value.max_attempts) || value.max_attempts < value.attempt_count ||
      typeof value.port_id !== 'string' || typeof value.device_id !== 'string' || typeof value.request !== 'string' ||
      typeof value.request_truncated !== 'boolean' || !Number.isSafeInteger(value.request_bytes) || value.request_bytes < 0 ||
      typeof value.last_error !== 'string' || !Number.isSafeInteger(value.duration_ms) || value.duration_ms < 0 ||
      !Number.isInteger(value.failure_count) || value.failure_count < 0 ||
      (value.ack_id !== null && typeof value.ack_id !== 'string') ||
      !['received', 'failed', 'expired', 'interrupted'].includes(value.result) ||
      !Array.isArray(value.failures) || value.failures.length > 8 || value.failure_count < value.failures.length ||
      value.failures.some((failure) => !failure || !Number.isInteger(failure.attempt) || failure.attempt < 0 || typeof failure.reason !== 'string')) return undefined;
    return value;
  } catch { return undefined; }
}

export default function SerialCommandAttempts() {
  const sources = useSources();
  const [selected, setSelected] = useState('');
  const sourceId = selected || sources.data?.[0]?.id || '';
  const [filter, setFilter] = useState<'all' | 'retried' | 'three'>('retried');
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState(25);
  const logs = useLowerLogs({ sourceId, commandAttempts: filter, page, pageSize });
  return <section className="content-panel serial-command-attempts" aria-label="串口发送明细">
    <h2>串口发送明细</h2>
    <p className="text-muted">下级实际执行记录，与上方管理端命令审计独立。发送 3 次 = 首次发送 + 补发 2 次；收到 ACK 即结束，不会为了凑够次数继续发送。</p>
    <div className="toolbar filter-toolbar">
      <label className="field inline-field"><span>明细来源</span><select value={sourceId} onChange={(event) => { setSelected(event.target.value); setPage(1); }}>{!sourceId && <option value="">请选择来源</option>}{(sources.data ?? []).map((source) => <option key={source.id} value={source.id}>{source.name || source.id}</option>)}</select></label>
      <label className="field inline-field"><span>发送情况</span><select value={filter} onChange={(event) => { setFilter(event.target.value as typeof filter); setPage(1); }}><option value="retried">发生补发（至少 2 次）</option><option value="three">发送至少 3 次</option><option value="all">全部发送结果</option></select></label>
      <button type="button" className="button button-secondary" disabled={!sourceId} onClick={() => void logs.refetch()}>刷新发送明细</button>
      {sourceId && <Link to={`/lower-logs?source=${encodeURIComponent(sourceId)}`}>查看该来源原始日志</Link>}
    </div>
    <p className="text-muted">需新版下级启用 HTTP 日志推送到本管理端。历史命令不补算次数；无记录不代表没有补发。明细在命令结束后上报，受日志保留周期和推送容量限制。成功表示收到 ACK，不代表已核实设备动作反馈。</p>
    {sources.isError ? <ErrorState error={sources.error} onRetry={() => void sources.refetch()} /> : sources.isLoading || (sourceId && logs.isLoading) ? <LoadingState /> : logs.isError ? <ErrorState error={logs.error} onRetry={() => void logs.refetch()} /> : !sourceId ? <EmptyState title="请先配置下级来源" /> : <>
      {logs.data?.items.length ? <div className="table-scroll"><table className="data-table responsive-table"><thead><tr><th>结束时间</th><th>串口 / 站点</th><th>下级命令编号</th><th>命令报文</th><th>发送 / 补发</th><th>最终结果</th><th>补发原因 / 错误</th><th>耗时</th></tr></thead><tbody>{logs.data.items.map((event) => {
        const value = parseCommandSummary(event.message);
        if (!value) return <tr key={`${event.bootId}:${event.sequence}`}><td colSpan={8}>无法解析发送明细，请查看原始日志。</td></tr>;
        const retried = value.sent_count > 1;
        const result = value.result === 'received' ? retried ? '补发后成功（ACK）' : '首次成功（ACK）' : value.result === 'expired' ? '执行期限已到' : value.result === 'interrupted' ? '执行中断，结果未知' : '未获得成功确认';
        return <tr key={`${event.bootId}:${event.sequence}`}>
          <td data-label="结束时间">{formatDateTime(event.timestamp)}</td>
          <td data-label="串口 / 站点">{value.port_id}<span className="sub-cell">站点 {value.device_id}</span></td>
          <td data-label="下级命令编号" title={`来源 ${sourceId}；启动 ${event.bootId}；事件 ${event.sequence}`}>{value.ack_id ?? '无编号'}<span className="sub-cell">启动：{event.bootId}</span></td>
          <td data-label="命令报文"><code className="command-payload">{value.request}</code>{value.request_truncated && <span className="sub-cell">仅前 256 字节 / 共 {value.request_bytes} 字节</span>}</td>
          <td data-label="发送 / 补发" className={retried ? 'text-warning' : ''}><strong>发送 {value.sent_count} 次 · 补发 {Math.max(0, value.sent_count - 1)} 次</strong><span className="sub-cell">尝试 {value.attempt_count} / 上限 {value.max_attempts} 次</span></td>
          <td data-label="最终结果" className={value.result === 'received' ? 'text-good' : 'text-bad'}>{result}</td>
          <td data-label="补发原因 / 错误">{value.failures.map((failure, index) => <span className="sub-cell" key={index}>第 {failure.attempt} 次：{reasons[failure.reason] ?? failure.reason}</span>)}{value.failure_count > value.failures.length && <span className="sub-cell">共 {value.failure_count} 次异常，仅保留最后 8 次</span>}{!value.failures.length && '—'}</td>
          <td data-label="耗时">{value.duration_ms} ms</td>
        </tr>;
      })}</tbody></table></div> : <EmptyState title="暂无符合条件的串口发送明细" description="旧下级不提供次数。请确认下级已更新并启用日志推送，或选择“全部发送结果”。" />}
      <Pagination page={page} pageSize={pageSize} total={logs.data?.total ?? 0} onPageChange={setPage} onPageSizeChange={(size) => { setPageSize(size); setPage(1); }} />
    </>}
  </section>;
}
