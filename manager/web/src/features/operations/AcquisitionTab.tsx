import { useEffect, useState } from 'react';
import { RefreshCw } from 'lucide-react';
import { useAcquisitionMutation, useAcquisitionStatus, type AcquisitionPort, type OperationsTarget } from '../../api/operations';
import { ErrorState, EmptyState, LoadingState } from '../../components/PageState';
import { InlineNotice } from '../../components/InlineNotice';
import { Modal } from '../../components/Modal';
import { StatusBadge } from '../../components/StatusBadge';
import { formatDateTime } from '../../lib/format';
import { operationFailure } from './shared';

interface Props { target: OperationsTarget; writable: boolean; onBusyChange: (busy: boolean) => void; onDialogChange: (open: boolean) => void }

export function AcquisitionTab({ target, writable, onBusyChange, onDialogChange }: Props) {
  const [pollUntil, setPollUntil] = useState(0);
  const query = useAcquisitionStatus(target, pollUntil > 0);
  const mutation = useAcquisitionMutation(target);
  const [closing, setClosing] = useState<{ port: AcquisitionPort; revision: string }>();
  const [confirmed, setConfirmed] = useState(false);
  const [message, setMessage] = useState('');
  const snapshot = query.data;
  const staleSource = snapshot ? snapshot.source_revision !== target.revision || snapshot.source_id !== target.id : false;
  const disabled = !writable || staleSource || mutation.isPending || query.isFetching || Boolean(query.error);

  useEffect(() => { onBusyChange(mutation.isPending); return () => onBusyChange(false); }, [mutation.isPending, onBusyChange]);
  useEffect(() => { onDialogChange(Boolean(closing)); return () => onDialogChange(false); }, [Boolean(closing), onDialogChange]);
  useEffect(() => {
    if (!pollUntil) return;
    const timer = window.setTimeout(() => setPollUntil(0), Math.max(0, pollUntil - Date.now()));
    return () => window.clearTimeout(timer);
  }, [pollUntil]);

  const apply = (input: { revision: string; portId: string; deviceId?: string; enabled: boolean }) => {
    mutation.mutate(input, { onSuccess: (result) => {
      setClosing(undefined);
      setConfirmed(false);
      setMessage(result.status === 'Pending' ? '请求已受理，仍待下级应用；将核对状态最多 1 分钟。' : '下级已确认控制配置；硬件是否就绪，请以串口打开和实际通信状态为准。');
      setPollUntil(result.status === 'Pending' ? Date.now() + 60_000 : 0);
    } });
  };

  return <div className="operations-tab-content">
    <InlineNotice><p>这里控制此下级的物理采集，不是 OPC Session 选源。关闭串口会停止该串口全部站的采集并释放串口；站点停读只停止数据采集，不禁止控制命令。</p></InlineNotice>
    <div className="operations-toolbar"><span className="operations-muted">配置目标、已应用状态与实际通信分别显示</span><button className="button button-secondary button-small" type="button" disabled={query.isFetching} onClick={() => void query.refetch()}><RefreshCw size={15} aria-hidden="true" />刷新实际状态</button></div>
    {message ? <div className="success-banner" role="status">{message}</div> : null}
    {mutation.error ? <div className="top-error" role="alert">{operationFailure(mutation.error)}</div> : null}
    {staleSource ? <InlineNotice tone="warning">来源配置版本已改变，禁止沿用旧目标操作；请关闭并重新打开运维。</InlineNotice> : null}
    {query.error ? <ErrorState error={query.error} title={snapshot ? '状态刷新失败，下方仅为上次快照' : '无法读取采集状态'} onRetry={() => void query.refetch()} /> : null}
    {!snapshot && query.isPending ? <LoadingState label="正在读取物理采集状态…" /> : snapshot ? <>
      <p className="operations-muted">下级状态版本：<code>{snapshot.revision}</code> · 快照时间：{formatDateTime(new Date(query.dataUpdatedAt).toISOString())}</p>
      {snapshot.ports.length === 0 ? <EmptyState title="下级未配置串口" description="此状态不代表 OPC 连接异常；请检查下级配置文件。" /> : snapshot.ports.map((port) => <section className="operations-port" key={port.port_id} aria-label={`采集串口 ${port.port_id}`}>
        <div className="operations-port-heading"><div><h3>{port.port_name || port.port_id}</h3><span className="operations-muted">{port.port_id} · {port.devices.length} 个站点</span></div><button className={`button button-small ${port.enabled ? 'button-outline' : 'button-primary'}`} type="button" disabled={disabled || port.control_pending || (port.pending_commands ?? 0) > 0} onClick={() => {
          mutation.reset(); setMessage('');
          if (port.enabled) { setClosing({ port, revision: snapshot.revision }); setConfirmed(false); }
          else apply({ portId: port.port_id, revision: snapshot.revision, enabled: true });
        }}>{port.enabled ? `关闭串口 ${port.port_name || port.port_id}` : `启用串口 ${port.port_name || port.port_id}`}</button></div>
        <div className="operations-state-row"><span>期望：<strong>{port.enabled ? '启用' : '停用'}</strong></span><span>已应用：<strong>{port.applied_enabled ? '启用' : '停用'}</strong></span><StatusBadge state={port.open ? 'good' : 'unknown'} label={port.open ? '串口已打开' : '串口未打开'} compact />{port.enabled !== port.applied_enabled ? <StatusBadge state="warning" label="待应用" compact /> : null}</div>
        {port.control_pending || (port.pending_commands ?? 0) > 0 ? <p className="operations-control-lock">{port.control_pending ? '串口控制待完成。' : ''}待处理命令：{port.pending_commands ?? 0}。完成前禁止再次变更此串口。</p> : null}
        {port.last_control_error ? <p className="text-bad operations-control-error" role="alert">最近控制错误：{port.last_control_error}</p> : null}
        {port.enabled && !port.open ? <p className="operations-muted">期望启用不代表硬件已打开；请核对控制错误、串口占用及设备连接。</p> : null}
        {port.devices.length ? <div className="table-scroll"><table className="data-table operations-device-table"><thead><tr><th>站点</th><th>期望采集</th><th>已应用</th><th>实际通信</th><th>最近数据</th><th>操作</th></tr></thead><tbody>{port.devices.map((device) => <tr key={device.device_id}>
          <td><strong>{device.name || device.device_id}</strong><span className="sub-cell">{device.device_id}</span></td><td>{device.read_enabled ? '读取' : '停读'}</td><td>{device.applied_read_enabled ? '读取' : '停读'}{device.read_enabled !== device.applied_read_enabled ? <span className="sub-cell">待应用</span> : null}</td>
          <td><StatusBadge state={device.communication_ok ? 'good' : 'unknown'} label={device.communication_ok ? '通信正常' : '通信未确认'} compact /></td><td>{formatDateTime(device.last_data_at)}</td>
          <td><button className="button button-small button-outline" type="button" disabled={disabled || port.control_pending || (port.pending_commands ?? 0) > 0} aria-label={`${device.read_enabled ? '停读' : '恢复读取'} ${port.port_id} ${device.device_id}`} onClick={() => { mutation.reset(); setMessage(''); apply({ revision: snapshot.revision, portId: port.port_id, deviceId: device.device_id, enabled: !device.read_enabled }); }}>{device.read_enabled ? '停读' : '恢复读取'}</button></td>
        </tr>)}</tbody></table></div> : <p className="operations-muted">此串口没有配置站点。</p>}
      </section>)}
    </> : null}
    <Modal open={Boolean(closing)} title="确认关闭物理串口" description="此操作影响所有使用该下级串口的站点和上级客户端，不仅是当前 Session。" onClose={() => !mutation.isPending && setClosing(undefined)} footer={<><button className="button button-secondary" type="button" disabled={mutation.isPending} onClick={() => setClosing(undefined)}>取消</button><button className="button button-danger" type="button" disabled={!confirmed || disabled || !closing || closing.revision !== snapshot?.revision} onClick={() => closing && apply({ revision: closing.revision, portId: closing.port.port_id, enabled: false })}>{mutation.isPending ? '正在提交…' : '确认关闭串口'}</button></>}>
      <p>串口：<strong>{closing?.port.port_name || closing?.port.port_id}</strong>；涉及 {closing?.port.devices.length ?? 0} 个站点。关闭后将释放该串口。</p>
      <label className="operations-check"><input type="checkbox" checked={confirmed} onChange={(event) => setConfirmed(event.target.checked)} />我确认停止该串口全部站的采集并释放串口</label>
      {closing && closing.revision !== snapshot?.revision ? <p className="text-bad">状态版本已变化，请取消后重新核对。</p> : null}
      {mutation.error ? <p className="text-bad" role="alert">{operationFailure(mutation.error)}</p> : null}
    </Modal>
  </div>;
}
