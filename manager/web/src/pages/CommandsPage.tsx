import { useEffect, useMemo, useState } from 'react';
import CommandTraceDrawer from '../features/commands/CommandTraceDrawer';
import CommandAuditHealth from '../features/commands/CommandAuditHealth';
import { Download, RefreshCw, Search } from 'lucide-react';
import { useSearchParams } from 'react-router-dom';
import { queryString } from '../api/client';
import { useCommands, useGroups, useSources } from '../api/queries';
import { EmptyState, ErrorState, LoadingState } from '../components/PageState';
import { PageHeader } from '../components/PageHeader';
import { Pagination, readPageSize } from '../components/Pagination';
import { CopyTextButton } from '../components/CopyTextButton';
import { formatDateTime, shortSessionId } from '../lib/format';
import { operationLabel, stateLabel, statusCodeLabel } from '../lib/labels';
import { sourceHasGroup } from '../lib/sourceGroups';
import { isGoodStatus } from '../lib/status';
import SerialCommandAttempts from '../features/commands/SerialCommandAttempts';



export default function CommandsPage() {
  const [detailId, setDetailId] = useState('');
  const [params, setParams] = useSearchParams();
  const PAGE_SIZE = readPageSize(params, 50);
  const page = Math.max(1, Number(params.get('page') ?? 1) || 1);
  const query = params.get('q') ?? '';
  const groupId = params.get('group') ?? '';
  const sourceId = params.get('source') ?? '';
  const status = params.get('status') ?? '';
  const [draft, setDraft] = useState(query);
  useEffect(() => setDraft(query), [query]);
  const groups = useGroups();
  const sources = useSources();
  const groupSources = useMemo(() => (sources.data ?? []).filter((source) => !groupId || sourceHasGroup(source, groupId)), [groupId, sources.data]);
  const listParams = { page, pageSize: PAGE_SIZE, query, groupId, sourceId, status };
  const commandsQuery = useCommands(listParams);
  const commands = commandsQuery.data?.items ?? [];

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
      <CommandAuditHealth />
      <PageHeader title="命令记录" description="_Send 写入按物理串口排队，最多64条（含执行中）；写入成功表示已受理，不表示设备已执行。排队待发送与等待响应分开显示。" actions={<a className="button button-outline" href={`/api/v1/commands/export${queryString(listParams)}`}><Download size={16} />导出筛选结果</a>} />
      <form className="toolbar filter-toolbar" onSubmit={(event) => { event.preventDefault(); update({ q: draft.trim(), page: 1 }); }}>
        <label className="search-field"><Search aria-hidden="true" size={17} /><input value={draft} placeholder="搜索命令、来源、端口或 Session" onChange={(event) => setDraft(event.target.value)} /><button type="submit">搜索</button></label>
        <label className="field inline-field"><span>分组</span><select value={groupId} onChange={(event) => update({ group: event.target.value, source: '', page: 1 })}><option value="">全部分组</option>{(groups.data ?? []).map((group) => <option key={group.id} value={group.id}>{group.name || group.id}</option>)}</select></label>
        <label className="field inline-field"><span>来源</span><select value={sourceId} onChange={(event) => update({ source: event.target.value, page: 1 })}><option value="">全部来源</option>{groupSources.map((source) => <option key={source.id} value={source.id}>{source.name || source.id}</option>)}</select></label>
        <label className="field inline-field"><span>结果</span><select value={status} onChange={(event) => update({ status: event.target.value, page: 1 })}><option value="">全部结果</option><option value="Queued">排队待发送</option><option value="Waiting">等待响应</option><option value="Received">成功</option><option value="Failed">失败</option><option value="TimedOut">超时</option></select></label>
        <button className="button button-secondary" type="button" onClick={() => void commandsQuery.refetch()}><RefreshCw aria-hidden="true" size={16} />刷新</button>
      </form>
      <section className="table-panel content-panel">
        {commandsQuery.isError && commandsQuery.data ? <p role="status" className="text-warning">刷新失败，当前保留上次读取的命令记录；可点击“刷新”重试。</p> : null}
        {commandsQuery.isLoading && !commandsQuery.data ? <LoadingState /> : commandsQuery.isError && !commandsQuery.data ? <ErrorState error={commandsQuery.error} onRetry={() => void commandsQuery.refetch()} /> : commands.length === 0 ? (
          <EmptyState title={query || groupId || sourceId || status ? '没有匹配的命令记录' : '暂无命令记录'} description="管理程序不会在这里提供主动发送命令的入口；命令只能由上级 OPC 客户端发起。" />
        ) : (
          <>
            <div className="table-scroll" aria-busy={commandsQuery.isFetching}>
              <table className="data-table responsive-table command-audit-table">
                <thead><tr><th>开始时间</th><th>操作 / 目标</th><th>分组 / 来源</th><th>端口</th><th>请求 / ACK</th><th>上级 Session</th><th>结果</th><th>协议结果 / 耗时</th><th>错误</th></tr></thead>
                <tbody>{commands.map((command) => {
                  const normalized = command.result.toLowerCase();
                  const succeeded = normalized === 'received' || normalized.includes('succeed') || normalized.includes('success');
                  const waiting = normalized === 'waiting' || normalized === 'queued';
                  return (
                    <tr key={command.id} className="virtual-row">
                      <td data-label="开始时间">{formatDateTime(command.createdAt)}</td><td data-label="操作 / 目标"><strong>{operationLabel(command.operation)}</strong><span className="sub-cell" title={command.nodeId}>{command.nodeId || command.id}</span></td>
                      <td data-label="分组 / 来源">{command.groupId}<span className="sub-cell">{command.sourceId}</span></td><td data-label="端口">{command.port}</td>
                      <td data-label="请求 / ACK"><code className="command-payload">{command.request || '—'}</code><span className="sub-cell">ACK：{normalized === 'queued' ? '尚未发送' : command.response || (waiting ? '等待中' : '无')}</span><span className="sub-cell">{normalized === 'queued' ? '等待串口空闲，尚未发往下级' : command.tracking ? `调用发送 ${command.tracking.attemptCount ?? '未知'} 次 / 完整写出 ${command.tracking.sentCount ?? '未知'} 次` : '旧协议 / 历史记录，无精确关联'}</span>{command.tracking?.auditIncomplete ? <span className="sub-cell text-warning">执行记录待补齐或不完整</span> : command.tracking?.historyGap ? <span className="sub-cell text-warning">执行快照已取得，历史同步曾有缺口</span> : null}</td>
                      <td data-label="上级 Session"><span title={command.sessionId}>{command.sessionId ? shortSessionId(command.sessionId) : '—'}</span>{command.sessionId ? <CopyTextButton value={command.sessionId} label="复制 Session ID" /> : null}</td>
                      <td data-label="结果" className={succeeded ? 'text-good' : waiting ? 'text-warning' : 'text-bad'}>{stateLabel(command.result)}<button type="button" className="button button-secondary" onClick={() => setDetailId(command.id)}>执行过程</button></td>
                      <td data-label="协议结果 / 耗时" className={command.statusCode ? isGoodStatus(command.statusCode) ? 'text-good' : waiting ? 'text-warning' : 'text-bad' : ''}>{command.statusCode ? isGoodStatus(command.statusCode) ? 'UA 成功' : statusCodeLabel(command.statusCode) : '—'}{command.statusCode && isGoodStatus(command.statusCode) ? <span className="sub-cell" title={statusCodeLabel(command.statusCode)}>{command.statusCode}</span> : null}<span className="sub-cell">{command.durationMs !== undefined ? `${command.durationMs} ms` : '—'}</span></td><td data-label="错误" className="message-cell text-bad">{command.error ?? '—'}</td>
                    </tr>
                  );
                })}</tbody>
              </table>
            </div>
          </>
        )}
        {commandsQuery.data ? <Pagination onPageSizeChange={(size) => update({ size, page: 1 })} page={commandsQuery.data.page ?? page} pageSize={commandsQuery.data.pageSize ?? PAGE_SIZE} total={commandsQuery.data.total} onPageChange={(value) => update({ page: value })} /> : null}
      </section>
      <SerialCommandAttempts />
      <CommandTraceDrawer id={detailId} onClose={() => setDetailId('')} />
    </div>
  );
}
