import { useMemo, useState } from 'react';
import { Download, RefreshCw, Search } from 'lucide-react';
import { useSearchParams } from 'react-router-dom';
import { queryString } from '../api/client';
import { useCommands, useGroups, useSources } from '../api/queries';
import { EmptyState, ErrorState, LoadingState } from '../components/PageState';
import { PageHeader } from '../components/PageHeader';
import { Pagination } from '../components/Pagination';
import { CopyTextButton } from '../components/CopyTextButton';
import { formatDateTime, shortSessionId } from '../lib/format';
import { operationLabel, stateLabel, statusCodeLabel } from '../lib/labels';
import { sourceHasGroup } from '../lib/sourceGroups';
import { isGoodStatus } from '../lib/status';

const PAGE_SIZE = 50;

export default function CommandsPage() {
  const [params, setParams] = useSearchParams();
  const page = Math.max(1, Number(params.get('page') ?? 1) || 1);
  const query = params.get('q') ?? '';
  const groupId = params.get('group') ?? '';
  const sourceId = params.get('source') ?? '';
  const status = params.get('status') ?? '';
  const [draft, setDraft] = useState(query);
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
      <PageHeader title="命令记录" description="审计通过 `_Send` 写入和 `_SendCommand` 调用路由的命令，并保留目标、请求与 ACK。" actions={<a className="button button-outline" href={`/api/v1/commands/export${queryString(listParams)}`}><Download size={16} />导出筛选结果</a>} />
      <form className="toolbar filter-toolbar" onSubmit={(event) => { event.preventDefault(); update({ q: draft.trim(), page: 1 }); }}>
        <label className="search-field"><Search aria-hidden="true" size={17} /><input value={draft} placeholder="搜索命令、来源、端口或 Session" onChange={(event) => setDraft(event.target.value)} /><button type="submit">搜索</button></label>
        <label className="field inline-field"><span>分组</span><select value={groupId} onChange={(event) => update({ group: event.target.value, source: '', page: 1 })}><option value="">全部分组</option>{(groups.data ?? []).map((group) => <option key={group.id} value={group.id}>{group.name || group.id}</option>)}</select></label>
        <label className="field inline-field"><span>来源</span><select value={sourceId} onChange={(event) => update({ source: event.target.value, page: 1 })}><option value="">全部来源</option>{groupSources.map((source) => <option key={source.id} value={source.id}>{source.name || source.id}</option>)}</select></label>
        <label className="field inline-field"><span>结果</span><select value={status} onChange={(event) => update({ status: event.target.value, page: 1 })}><option value="">全部结果</option><option value="Waiting">等待响应</option><option value="Received">成功</option><option value="Failed">失败</option><option value="TimedOut">超时</option></select></label>
        <button className="button button-secondary" type="button" onClick={() => void commandsQuery.refetch()}><RefreshCw aria-hidden="true" size={16} />刷新</button>
      </form>
      <section className="table-panel content-panel">
        {commandsQuery.isLoading && !commandsQuery.data ? <LoadingState /> : commandsQuery.isError ? <ErrorState error={commandsQuery.error} onRetry={() => void commandsQuery.refetch()} /> : commands.length === 0 ? (
          <EmptyState title={query || groupId || sourceId || status ? '没有匹配的命令记录' : '暂无命令记录'} description="管理程序不会在这里提供主动发送命令的入口；命令只能由上级 OPC 客户端发起。" />
        ) : (
          <>
            <div className={`table-scroll${commandsQuery.isFetching ? ' is-refreshing' : ''}`}>
              <table className="data-table responsive-table command-audit-table">
                <thead><tr><th>开始时间</th><th>操作 / 目标</th><th>分组 / 来源</th><th>端口</th><th>请求 / ACK</th><th>上级 Session</th><th>结果</th><th>协议结果 / 耗时</th><th>错误</th></tr></thead>
                <tbody>{commands.map((command) => {
                  const normalized = command.result.toLowerCase();
                  const succeeded = normalized === 'received' || normalized.includes('succeed') || normalized.includes('success');
                  const waiting = normalized === 'waiting';
                  return (
                    <tr key={command.id} className="virtual-row">
                      <td data-label="开始时间">{formatDateTime(command.createdAt)}</td><td data-label="操作 / 目标"><strong>{operationLabel(command.operation)}</strong><span className="sub-cell" title={command.nodeId}>{command.nodeId || command.id}</span></td>
                      <td data-label="分组 / 来源">{command.groupId}<span className="sub-cell">{command.sourceId}</span></td><td data-label="端口">{command.port}</td>
                      <td data-label="请求 / ACK"><code className="command-payload">{command.request || '—'}</code><span className="sub-cell">ACK：{command.response || (waiting ? '等待中' : '无')}</span></td>
                      <td data-label="上级 Session"><span title={command.sessionId}>{command.sessionId ? shortSessionId(command.sessionId) : '—'}</span>{command.sessionId ? <CopyTextButton value={command.sessionId} label="复制 Session ID" /> : null}</td>
                      <td data-label="结果" className={succeeded ? 'text-good' : waiting ? 'text-warning' : 'text-bad'}>{stateLabel(command.result)}</td>
                      <td data-label="协议结果 / 耗时" className={command.statusCode ? isGoodStatus(command.statusCode) ? 'text-good' : waiting ? 'text-warning' : 'text-bad' : ''}>{command.statusCode ? isGoodStatus(command.statusCode) ? 'UA 成功' : statusCodeLabel(command.statusCode) : '—'}{command.statusCode && isGoodStatus(command.statusCode) ? <span className="sub-cell" title={statusCodeLabel(command.statusCode)}>{command.statusCode}</span> : null}<span className="sub-cell">{command.durationMs !== undefined ? `${command.durationMs} ms` : '—'}</span></td><td data-label="错误" className="message-cell text-bad">{command.error ?? '—'}</td>
                    </tr>
                  );
                })}</tbody>
              </table>
            </div>
            <Pagination page={commandsQuery.data?.page ?? page} pageSize={commandsQuery.data?.pageSize ?? PAGE_SIZE} total={commandsQuery.data?.total ?? 0} onPageChange={(value) => update({ page: value })} />
          </>
        )}
      </section>
    </div>
  );
}
