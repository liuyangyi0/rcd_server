import { useEffect, useState } from 'react';
import { RefreshCw } from 'lucide-react';
import { useAcquisitionMutation, useAcquisitionStatus, useDeviceVersionMutation, type AcquisitionDevice, type AcquisitionPort, type OperationsTarget } from '../../api/operations';
import type { ModuleType } from '../../api/types';
import { ErrorState, EmptyState, LoadingState } from '../../components/PageState';
import { InlineNotice } from '../../components/InlineNotice';
import { Modal } from '../../components/Modal';
import { StatusBadge } from '../../components/StatusBadge';
import { formatDateTime } from '../../lib/format';
import { humanizeTechnicalMessage } from '../../lib/labels';
import { operationFailure } from './shared';

interface Props { target: OperationsTarget; writable: boolean; stationVersions: boolean; onBusyChange: (busy: boolean) => void; onDialogChange: (open: boolean) => void }
// stationRevision is the version configuration the operator reviewed when opening the dialog; it is the CAS value sent.
interface VersionTarget { port: AcquisitionPort; device: AcquisitionDevice; action: 'type' | 'query'; moduleType: ModuleType; stationRevision: string }

export function AcquisitionTab({ target, writable, stationVersions, onBusyChange, onDialogChange }: Props) {
  const [pollUntil, setPollUntil] = useState(0);
  const query = useAcquisitionStatus(target, pollUntil > 0);
  const mutation = useAcquisitionMutation(target);
  const versionMutation = useDeviceVersionMutation();
  const [closing, setClosing] = useState<{ port: AcquisitionPort; revision: string }>();
  const [confirmed, setConfirmed] = useState(false);
  const [message, setMessage] = useState('');
  const [messageTone, setMessageTone] = useState<'success' | 'warning' | 'error'>('success');
  const [versionTarget, setVersionTarget] = useState<VersionTarget>();
  const [versionConfirmed, setVersionConfirmed] = useState(false);
  const snapshot = query.data;
  const staleSource = snapshot ? snapshot.source_revision !== target.revision || snapshot.source_id !== target.id : false;
  const busy = mutation.isPending || versionMutation.isPending;
  const disabled = !writable || staleSource || busy || query.isFetching || Boolean(query.error);
  // The query guard follows the live port and station, not the copy taken when the dialog opened.
  const livePort = versionTarget ? snapshot?.ports.find((port) => port.port_id === versionTarget.port.port_id) : undefined;
  const liveDevice = versionTarget ? livePort?.devices.find((device) => device.device_id === versionTarget.device.device_id) : undefined;
  const versionQueryBlockReason = versionTarget?.action === 'query' ? livePort && liveDevice ? acquisitionVersionQueryDisabled(livePort, liveDevice) : '站点已不在当前采集状态中' : undefined;
  const versionStale = Boolean(versionTarget && versionTarget.stationRevision !== snapshot?.station_revision);

  useEffect(() => { onBusyChange(busy); return () => onBusyChange(false); }, [busy, onBusyChange]);
  useEffect(() => { onDialogChange(Boolean(closing || versionTarget)); return () => onDialogChange(false); }, [Boolean(closing || versionTarget), onDialogChange]);
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
      setMessageTone(result.status === 'Pending' ? 'warning' : 'success');
      setPollUntil(result.status === 'Pending' ? Date.now() + 60_000 : 0);
    } });
  };

  const applyVersion = () => {
    if (!versionTarget || !versionTarget.stationRevision || versionStale || (versionTarget.action === 'query' && versionQueryBlockReason)) return;
    versionMutation.mutate(versionTarget.action === 'query' ? {
      action: 'query', target, stationRevision: versionTarget.stationRevision, portId: versionTarget.port.port_id, deviceId: versionTarget.device.device_id,
    } : {
      action: 'set_type', target, stationRevision: versionTarget.stationRevision, portId: versionTarget.port.port_id, deviceId: versionTarget.device.device_id, moduleType: versionTarget.moduleType,
    }, { onSuccess: (result) => {
      const station = result.ports.find((port) => port.port_id === versionTarget.port.port_id)?.devices.find((device) => device.device_id === versionTarget.device.device_id);
      const versionState = station?.version_state?.toLowerCase() ?? '';
      const waiting = ['waiting', 'pending', 'querying'].some((state) => versionState.includes(state));
      const failed = ['fail', 'error', 'timeout'].some((state) => versionState.includes(state));
      if (result.status === 'Pending' || (versionTarget.action === 'query' && waiting)) {
        setMessage(versionTarget.action === 'query' ? '版本查询已受理，等待下级完成并刷新结果。' : '模块类型设置已受理，等待下级应用。');
        setMessageTone('warning');
      } else if (versionTarget.action === 'query' && station?.type_mismatch) {
        setMessage(station.last_version_error || '查询完成，但实测模块类型与配置不一致。');
        setMessageTone('warning');
      } else if (versionTarget.action === 'query' && (station?.last_version_error || failed)) {
        setMessage(station?.last_version_error || '版本查询未成功，请刷新状态查看详情。');
        setMessageTone('error');
      } else {
        setMessage(versionTarget.action === 'query' ? '版本查询完成。' : '模块类型已保存；配置为国产时下级会在安全轮询点自动查询一次。');
        setMessageTone('success');
      }
      setVersionTarget(undefined);
      setVersionConfirmed(false);
    } });
  };

  return <div className="operations-tab-content">
    <InlineNotice><p>这里控制此下级的物理采集，不是 OPC Session 选源。关闭串口会停止该串口全部站的采集并释放串口；站点停读只停止数据采集，不禁止控制命令。</p></InlineNotice>
    <div className="operations-toolbar"><span className="operations-muted">配置目标、已应用状态与实际通信分别显示</span><button className="button button-secondary button-small" type="button" disabled={query.isFetching} onClick={() => void query.refetch()}><RefreshCw size={15} aria-hidden="true" />刷新实际状态</button></div>
    {message ? messageTone === 'warning' ? <InlineNotice tone="warning"><p role="status">{message}</p></InlineNotice> : messageTone === 'error' ? <InlineNotice tone="error"><p role="alert">{message}</p></InlineNotice> : <div className="success-banner" role="status">{message}</div> : null}
    {mutation.error ? <div className="top-error" role="alert">{operationFailure(mutation.error)}</div> : null}
    {versionMutation.error ? <div className="top-error" role="alert">{operationFailure(versionMutation.error)}</div> : null}
    {staleSource ? <InlineNotice tone="warning">来源配置版本已改变，禁止沿用旧目标操作；请关闭并重新打开运维。</InlineNotice> : null}
    {query.error ? <ErrorState error={query.error} title={snapshot ? '状态刷新失败，下方仅为上次快照' : '无法读取采集状态'} onRetry={() => void query.refetch()} /> : null}
    {!snapshot && query.isPending ? <LoadingState label="正在读取物理采集状态…" /> : snapshot ? <>
      <p className="operations-muted">下级状态版本：<code>{snapshot.revision}</code>{snapshot.station_revision ? <> · 站点版本配置：<code>{snapshot.station_revision}</code></> : null} · 快照时间：{formatDateTime(new Date(query.dataUpdatedAt).toISOString())}</p>
      {!stationVersions ? <InlineNotice>此下级版本不支持站点模块版本管理；正常采集和原有 OPC 点位不受影响。</InlineNotice> : null}
      {snapshot.ports.length === 0 ? <EmptyState title="下级未配置串口" description="此状态不代表 OPC 连接异常；请检查下级配置文件。" /> : snapshot.ports.map((port) => <section className="operations-port" key={port.port_id} aria-label={`采集串口 ${port.port_id}`}>
        <div className="operations-port-heading"><div><h3>{port.port_name || port.port_id}</h3><span className="operations-muted">{port.port_id} · {port.devices.length} 个站点</span></div><button className={`button button-small ${port.enabled ? 'button-danger-outline' : 'button-primary'}`} type="button" disabled={disabled || port.control_pending || (port.pending_commands ?? 0) > 0} onClick={() => {
          mutation.reset(); setMessage('');
          if (port.enabled) { setClosing({ port, revision: snapshot.revision }); setConfirmed(false); }
          else apply({ portId: port.port_id, revision: snapshot.revision, enabled: true });
        }}>{port.enabled ? `关闭串口 ${port.port_name || port.port_id}` : `启用串口 ${port.port_name || port.port_id}`}</button></div>
        <div className="operations-state-row"><span>期望：<strong>{port.enabled ? '启用' : '停用'}</strong></span><span>已应用：<strong>{port.applied_enabled ? '启用' : '停用'}</strong></span><StatusBadge state={port.open ? 'good' : 'unknown'} label={port.open ? '串口已打开' : '串口未打开'} compact />{port.enabled !== port.applied_enabled ? <StatusBadge state="warning" label="待应用" compact /> : null}</div>
        {port.control_pending || (port.pending_commands ?? 0) > 0 ? <p className="operations-control-lock">{port.control_pending ? '串口控制待完成。' : ''}待处理命令：{port.pending_commands ?? 0}。完成前禁止再次变更此串口。</p> : null}
        {port.last_control_error ? <p className="text-bad operations-control-error" role="alert">最近控制错误：{humanizeTechnicalMessage(port.last_control_error)}</p> : null}
        {port.enabled && !port.open ? <p className="operations-muted">期望启用不代表硬件已打开；请核对控制错误、串口占用及设备连接。</p> : null}
        {port.devices.length ? <div className="table-scroll"><table className="data-table operations-device-table operations-device-version-table"><thead><tr><th>站点</th><th>期望采集</th><th>已应用</th><th>实际通信</th><th>模块 / 版本</th><th>最近数据</th><th>操作</th></tr></thead><tbody>{port.devices.map((device) => <tr key={device.device_id}>
          <td><strong>{device.name || device.device_id}</strong><span className="sub-cell">{device.device_id}</span></td><td>{device.read_enabled ? '读取' : '停读'}</td><td>{device.applied_read_enabled ? '读取' : '停读'}{device.read_enabled !== device.applied_read_enabled ? <span className="sub-cell">待应用</span> : null}</td>
          <td><StatusBadge state={device.communication_ok ? 'good' : 'unknown'} label={device.communication_ok ? '通信正常' : '通信未确认'} compact /></td><td><StationVersionCell device={device} supported={stationVersions} /></td><td>{formatDateTime(device.last_data_at)}</td>
          <td><div className="operations-device-actions"><button className={`button button-small ${device.read_enabled ? 'button-danger-outline' : 'button-primary'}`} type="button" disabled={disabled || port.control_pending || (port.pending_commands ?? 0) > 0} aria-label={`${device.read_enabled ? '停读' : '恢复读取'} ${port.port_id} ${device.device_id}`} onClick={() => { mutation.reset(); setMessage(''); apply({ revision: snapshot.revision, portId: port.port_id, deviceId: device.device_id, enabled: !device.read_enabled }); }}>{device.read_enabled ? '停读' : '恢复读取'}</button>{stationVersions ? <><button className="table-action-link" type="button" disabled={disabled || !snapshot.station_revision || port.control_pending || (port.pending_commands ?? 0) > 0} onClick={() => { versionMutation.reset(); setVersionConfirmed(false); setVersionTarget({ port, device, action: 'type', moduleType: device.configured_module_type ?? 'unknown', stationRevision: snapshot.station_revision ?? '' }); }}>设置类型</button><button className="table-action-link" type="button" disabled={disabled || !snapshot.station_revision || Boolean(acquisitionVersionQueryDisabled(port, device))} title={acquisitionVersionQueryDisabled(port, device)} onClick={() => { versionMutation.reset(); setVersionConfirmed(false); setVersionTarget({ port, device, action: 'query', moduleType: 'domestic', stationRevision: snapshot.station_revision ?? '' }); }}>查询版本</button></> : null}</div></td>
        </tr>)}</tbody></table></div> : <p className="operations-muted">此串口没有配置站点。</p>}
      </section>)}
    </> : null}
    <Modal open={Boolean(closing)} title="确认关闭物理串口" description="此操作影响所有使用该下级串口的站点和上级客户端，不仅是当前 Session。" onClose={() => !mutation.isPending && setClosing(undefined)} footer={<><button className="button button-secondary" type="button" disabled={mutation.isPending} onClick={() => setClosing(undefined)}>取消</button><button className="button button-danger" type="button" disabled={!confirmed || disabled || !closing || closing.revision !== snapshot?.revision} onClick={() => closing && apply({ revision: closing.revision, portId: closing.port.port_id, enabled: false })}>{mutation.isPending ? '正在提交…' : '确认关闭串口'}</button></>}>
      <p>串口：<strong>{closing?.port.port_name || closing?.port.port_id}</strong>；涉及 {closing?.port.devices.length ?? 0} 个站点。关闭后将释放该串口。</p>
      <label className="operations-check"><input type="checkbox" checked={confirmed} onChange={(event) => setConfirmed(event.target.checked)} />我确认停止该串口全部站的采集并释放串口</label>
      {closing && closing.revision !== snapshot?.revision ? <p className="text-bad">状态版本已变化，请取消后重新核对。</p> : null}
      {mutation.error ? <p className="text-bad" role="alert">{operationFailure(mutation.error)}</p> : null}
    </Modal>
    <Modal open={Boolean(versionTarget)} title={versionTarget?.action === 'query' ? '查询通信模块版本' : '设置通信模块类型'} description={`${versionTarget?.port.port_name || versionTarget?.port.port_id || ''} · ${versionTarget?.device.name || versionTarget?.device.device_id || ''}`} onClose={() => !versionMutation.isPending && setVersionTarget(undefined)} footer={<><button className="button button-secondary" type="button" disabled={versionMutation.isPending} onClick={() => setVersionTarget(undefined)}>取消</button><button className="button button-primary" type="button" disabled={disabled || !versionTarget || !versionConfirmed || !versionTarget.stationRevision || versionStale || Boolean(versionQueryBlockReason)} onClick={applyVersion}>{versionMutation.isPending ? '正在提交…' : versionTarget?.action === 'query' ? '确认查询' : '确认保存'}</button></>}>
      {versionTarget?.action === 'type' ? <label className="field"><span>模块类型</span><select value={versionTarget.moduleType} disabled={versionMutation.isPending} onChange={(event) => { setVersionConfirmed(false); setVersionTarget((current) => current ? { ...current, moduleType: event.target.value as ModuleType } : current); }}><option value="unknown">未配置（不查询）</option><option value="legacy_russian">俄供原模块（不查询）</option><option value="domestic">国产自研模块</option></select></label> : null}
      <InlineNotice tone="warning">{versionTarget?.action === 'query' || versionTarget?.moduleType === 'domestic' ? '下级将使用现场物理串口发送版本请求，同一串口不会并发。' : '未知和俄供类型不会发送版本查询。'}</InlineNotice>
      {versionStale ? <p className="text-bad">站点版本配置已变化，请取消后重新核对。</p> : null}
      {versionQueryBlockReason ? <p className="text-bad" role="alert">当前不能查询：{versionQueryBlockReason}</p> : null}
      <label className="operations-check"><input type="checkbox" checked={versionConfirmed} onChange={(event) => setVersionConfirmed(event.target.checked)} />我已核对目标站点和操作</label>
      {versionMutation.error ? <p className="text-bad" role="alert">{operationFailure(versionMutation.error)}</p> : null}
    </Modal>
  </div>;
}

function acquisitionVersionQueryDisabled(port: AcquisitionPort, device: AcquisitionDevice): string | undefined {
  if (device.configured_module_type !== 'domestic') return '必须先配置为国产模块';
  if (!port.enabled) return '串口已停用';
  if (!port.applied_enabled) return '串口启用设置尚未应用';
  if (!port.open) return '串口尚未打开';
  if (!device.read_enabled) return '站点已停读';
  if (!device.applied_read_enabled) return '站点恢复读取设置尚未应用';
  if (port.control_pending || (port.pending_commands ?? 0) > 0) return '串口存在待处理控制或命令';
  return undefined;
}

function StationVersionCell({ device, supported }: { device: AcquisitionDevice; supported: boolean }) {
  if (!supported) return <span className="text-muted">下级不支持</span>;
  if (!device.configured_module_type) return <span className="text-muted">等待状态</span>;
  const configured = device.configured_module_type === 'domestic' ? '国产' : device.configured_module_type === 'legacy_russian' ? '俄供' : '未配置';
  return <><strong>{configured}</strong>{device.type_mismatch ? <span className="sub-cell text-bad">实测类型不一致</span> : null}<span className="sub-cell">模块 {device.communication_module_version || '—'} · PLC {device.plc_version || '—'}</span>{device.stale ? <span className="sub-cell text-warning">历史版本已过期</span> : null}{device.last_version_error ? <span className="sub-cell text-bad">{humanizeTechnicalMessage(device.last_version_error)}</span> : null}</>;
}
