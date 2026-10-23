import { useQuery } from '@tanstack/react-query';
import { request } from '../../api/client';
import { Drawer } from '../../components/Drawer';
import { EmptyState, ErrorState, LoadingState } from '../../components/PageState';
import { formatDateTime } from '../../lib/format';

export interface CommandTraceDetail {
  command: { id: string; source_id: string; port: string; state: string; request?: string };
  trace?: { protocol: string; gateway_instance_id: string; boot_id: string; history_gap: boolean;
    snapshot?: { phase: string; ack_id: string; accepted: boolean; attempt_count: number; sent_count: number;
      audit_incomplete: boolean; error: string; updated_at: string;
      attempts: { attempt: number; sent: boolean; result: string; at: string }[] } };
}

const labels: Record<string, string> = {
  SentUnconfirmed: '完整写出，旧接口不等待设备 ACK',
  Accepted: '下级已接收', Executing: '执行中', AckReceived: '收到设备 ACK', Failed: '下级未获得成功确认',
  Expired: '执行期限已到', Unknown: '执行中断，结果未知', Sending: '开始发送', WaitingForAck: '等待 ACK',
  ack_timeout: '等待 ACK 超时', ack_mismatch: '响应未匹配 ACK', ack_io_error: '读取 ACK 失败', write_failed: '串口写入失败',
};

export function CommandTraceBody({ detail }: { detail: CommandTraceDetail }) {
  const value = detail.trace?.snapshot;
  return <>
    <p>命令编号：<code>{detail.command.id}</code></p>
    <p>来源：{detail.command.source_id}　串口：{detail.command.port}</p>
    <p>请求：<code className="command-payload">{detail.command.request || '—'}</code></p>
    {detail.command.state === 'Queued' ? <EmptyState title="排队待发送" description="命令内容和目标已固定，正在等待同一串口的前一条命令结束，尚未发送给下级。" /> : !detail.trace ? <EmptyState title="旧协议或历史记录，无法精确关联" description="独立发送明细仍可查看；不按报文内容或时间猜测关联。" /> : !value ?
      <EmptyState title="尚未取得下级执行记录" description="请求可能尚未执行，也可能已执行但确认丢失。系统只补查，不自动重发命令。" /> : <>
      <p>{labels[value.phase] || value.phase}</p>
      {detail.command.state === 'TimedOut' && value.phase === 'AckReceived' && <p className="text-warning">管理端原请求已超时；随后确认下级收到 ACK。原超时结果保留。</p>}
      {value.audit_incomplete && <p role="alert" className="text-warning">下级审计记录不完整：以下次数和过程仅代表已获得的记录，不能据此排除其他发送。</p>}
      {detail.trace.history_gap && !value.audit_incomplete && <p role="status" className="text-warning">历史同步曾有缺口；已取得下级执行快照，以下次数来自该快照，不代表所有中间事件均已补齐。</p>}
      <p>已记录：调用发送 {value.attempt_count} 次，完整写出 {value.sent_count} 次；补发 {Math.max(0, value.sent_count - 1)} 次。</p>
      <p>下级编号：<code>{value.ack_id}</code>　最后更新：{formatDateTime(value.updated_at)}</p>
      {value.error && <p className="text-bad">{value.error}</p>}
      <ol className="command-trace-attempts">{value.attempts.map((attempt) => <li key={attempt.attempt}>
        <strong>第 {attempt.attempt} 次</strong> · {attempt.sent ? '完整写出' : '未确认完整写出'} · {labels[attempt.result] || attempt.result}
        <span className="sub-cell">{formatDateTime(attempt.at)}</span>
      </li>)}</ol>
      <p className="text-muted">收到 ACK 表示协议确认，不代表已经核实设备动作反馈。</p>
    </>}
  </>;
}

export default function CommandTraceDrawer({ id, onClose }: { id: string; onClose: () => void }) {
  const detail = useQuery({ queryKey: ['commands', 'detail', id], enabled: Boolean(id),
    queryFn: async () => await request(`/commands/${encodeURIComponent(id)}`) as CommandTraceDetail,
    refetchInterval: id ? 2000 : false });
  return <Drawer open={Boolean(id)} title="命令执行过程" onClose={onClose}>
    {id && detail.isError && detail.data ? <p role="status" className="text-warning">刷新失败，当前保留上次读取的执行记录，正在等待重新确认。</p> : null}
    {id && (detail.data ? <CommandTraceBody detail={detail.data} /> : detail.isLoading ? <LoadingState /> : detail.isError ? <ErrorState error={detail.error} onRetry={() => void detail.refetch()} /> : null)}
  </Drawer>;
}
