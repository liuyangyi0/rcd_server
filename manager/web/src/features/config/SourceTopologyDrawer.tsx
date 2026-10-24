import { useMemo, useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { Link } from 'react-router-dom';
import { RefreshCw, Save } from 'lucide-react';
import { api, ApiError, apiErrorMessage } from '../../api/client';
import { queryKeys, useSessions, useSourceTopology } from '../../api/queries';
import type { DeviceGroupBinding, Group, Source } from '../../api/types';
import { Drawer } from '../../components/Drawer';
import { EmptyState, ErrorState, LoadingState } from '../../components/PageState';
import { InlineNotice } from '../../components/InlineNotice';
import { deviceKey, sameBindings, sourceGroupIds } from '../../lib/sourceGroups';
import { formatNumber } from '../../lib/format';
import { stateLabel } from '../../lib/labels';

interface Props {
  source: Source;
  groups: Group[];
  onClose: () => void;
  onSaved: (message: string) => void;
}

export default function SourceTopologyDrawer({ source, groups, onClose, onSaved }: Props) {
  const client = useQueryClient();
  const topologyQuery = useSourceTopology(source.id);
  const sessionsQuery = useSessions();
  // Keep the configuration revision paired with this editable snapshot. SSE
  // refreshes must not silently give an old draft a newer If-Match revision.
  const [editSource, setEditSource] = useState(source);
  const [bindings, setBindings] = useState<DeviceGroupBinding[]>(() => source.groupBindings ?? []);
  const topology = topologyQuery.data;
  const bindingMap = useMemo(() => new Map(bindings.map((binding) => [deviceKey(binding.portId, binding.deviceId), binding])), [bindings]);
  const deviceKeys = useMemo(() => new Set((topology?.ports ?? []).flatMap((port) => port.devices.map((device) => deviceKey(port.portId, device.deviceId)))), [topology]);
  const orphanBindings = bindings.filter((binding) => !deviceKeys.has(deviceKey(binding.portId, binding.deviceId)));
  const affectedGroups = new Set([...sourceGroupIds(editSource), ...bindings.map((binding) => binding.groupId)]);
  const pendingCount = groups.reduce((total, group) => total + (affectedGroups.has(group.id) ? group.pendingCommandCount : 0), 0);
  const onlineCount = sessionsQuery.data?.length ?? 0;
  const changed = !sameBindings(bindings, editSource.groupBindings ?? []);
  const groupIds = new Set(groups.map((group) => group.id));
  const invalidBinding = bindings.some((binding) => !groupIds.has(binding.groupId));
  const staleRevision = source.revision !== editSource.revision || Boolean(topology && topology.revision !== editSource.revision);
  const mutation = useMutation({
    mutationFn: () => api.updateSource(editSource.id, {
      id: editSource.id, name: editSource.name, endpoint: editSource.endpoint, groupId: editSource.groupId,
      groupBindings: bindings, securityPolicy: editSource.securityPolicy, securityMode: editSource.securityMode,
      username: editSource.username, enabled: editSource.enabled, revision: editSource.revision,
    }),
    onSuccess: async (saved) => {
      setEditSource(saved);
      setBindings(saved.groupBindings ?? []);
      await Promise.all([
        client.invalidateQueries({ queryKey: queryKeys.allSources }),
        client.invalidateQueries({ queryKey: queryKeys.sourceTopology(saved.id) }),
        client.invalidateQueries({ queryKey: queryKeys.groups }),
        client.invalidateQueries({ queryKey: queryKeys.allPoints }),
        client.invalidateQueries({ queryKey: queryKeys.overview }),
        client.invalidateQueries({ queryKey: queryKeys.allManagementAudit }),
      ]);
      onSaved(`${saved.id} 的设备映射已保存；同一逻辑组内的点位统一选源，其余组保持独立`);
    },
    onError: () => {
      void client.invalidateQueries({ queryKey: queryKeys.sessions });
      void client.invalidateQueries({ queryKey: queryKeys.allSources });
    },
  });
  const rediscover = useMutation({
    mutationFn: () => api.restartSource(source.id, source.revision, true),
    onSuccess: async () => {
      await Promise.all([
        client.invalidateQueries({ queryKey: queryKeys.allSources }),
        client.invalidateQueries({ queryKey: queryKeys.sourceTopology(source.id) }),
      ]);
    },
  });
  const setBinding = (portId: string, deviceId: string, groupId: string) => {
    setBindings((previous) => {
      const remaining = previous.filter((binding) => deviceKey(binding.portId, binding.deviceId) !== deviceKey(portId, deviceId));
      return groupId ? [...remaining, { portId, deviceId, groupId }] : remaining;
    });
    mutation.reset();
  };
  const resetDraft = () => {
    setEditSource(source);
    setBindings(source.groupBindings ?? []);
    mutation.reset();
    void Promise.all([topologyQuery.refetch(), sessionsQuery.refetch()]);
  };
  const canSave = changed && !invalidBinding && !staleRevision && !mutation.isPending && !rediscover.isPending &&
    !topologyQuery.isFetching && Boolean(topology) && !topologyQuery.error && !sessionsQuery.isPending &&
    !sessionsQuery.error && onlineCount === 0 && pendingCount === 0;
  const knownCatalog = ['ready', 'cached', 'catalogconflict'].includes((topology?.catalogState ?? '').toLowerCase());

  return (
    <Drawer open title={`串口 / 设备映射 · ${source.id}`} className="topology-drawer" onClose={onClose}>
      <div className="topology-heading">
        <p className="member-endpoint">{source.endpoint}</p>
        <div className="topology-controls">
          <button className="button button-small button-secondary" type="button" disabled={topologyQuery.isFetching} onClick={() => void topologyQuery.refetch()}><RefreshCw size={15} />刷新拓扑</button>
          <button className="button button-small button-outline" type="button" disabled={rediscover.isPending || mutation.isPending} onClick={() => rediscover.mutate()}>{rediscover.isPending ? '正在重连…' : '重连并发现拓扑'}</button>
        </div>
      </div>
      <InlineNotice><p>串口和设备表示此下级的物理归属。将两台下级中对应的设备绑定到同一个逻辑组，之后可按 OPC Session 对该整组独立选源，不会切换其他设备组。</p></InlineNotice>
      <InlineNotice tone="warning"><p>迁移冗余来源时，同名 KKS 必须映射到同一个逻辑组。分步保存第一个来源后，相关来源可能暂时出现目录冲突、不可选；继续完成其他来源的对应设备映射后即可恢复。请在维护窗口结束在线 Session 后操作，全部映射一致并确认目录恢复后再接入上级。</p></InlineNotice>
      <dl className="topology-summary">
        <div><dt>默认归属组</dt><dd>{editSource.groupId}<span className="sub-cell">未映射设备、跨设备计算、共享命令</span></dd></div>
        <div><dt>配置版本 / 目录</dt><dd>{editSource.revision} / {stateLabel(topology?.catalogState ?? source.catalogState)}</dd></div>
        <div><dt>已绑定设备</dt><dd>{bindings.length} 个 · {sourceGroupIds({ ...editSource, groupBindings: bindings }).length} 个逻辑组</dd></div>
      </dl>
      {source.catalogState.toLowerCase().includes('conflict') ? <InlineNotice tone="warning"><p>此来源的目录存在冲突，暂不能选源。可检查设备映射与其他来源的逻辑组是否一致；{source.lastError || '详细冲突请在下级列表的目录诊断中查看。'}</p></InlineNotice> : null}

      {topologyQuery.isLoading ? <LoadingState label="正在读取串口与设备拓扑…" /> : topologyQuery.error ? <ErrorState error={topologyQuery.error} onRetry={() => void topologyQuery.refetch()} /> : topology ? <>
        {!topology.supported ? <EmptyState title={knownCatalog ? '此下级未提供设备拓扑' : '拓扑尚未发现'} description={knownCatalog ? '旧版下级仍按默认归属组提供全部点位。升级子 OPC 后，重新发现即可配置设备映射。' : '请等待首次目录发现完成；离线且无缓存时无法推测设备归属。'} /> : topology.ports.length === 0 ? <EmptyState title="拓扑中没有串口设备" description="计算点仍由默认归属组提供；如预期存在设备，请检查下级配置并重新发现。" /> : topology.ports.map((port) => (
          <section className="topology-port" key={port.portId} aria-label={`串口 ${port.portId}`}>
            <div className="topology-port-header"><h3>{port.portName || port.portId}</h3><span>{port.devices.length} 台设备 · {formatNumber(port.pointCount)} 点</span></div>
            <p className="topology-port-meta">{port.portId}{port.sendNodeId ? ` · 命令 ${port.sendNodeId}` : ''}</p>
            {port.devices.length === 0 ? <p className="text-muted">该串口没有已发现的设备。</p> : <div className="table-scroll"><table className="data-table topology-table"><thead><tr><th>设备 / 地址</th><th>点位</th><th>当前归组</th><th>编辑逻辑组</th></tr></thead><tbody>{port.devices.map((device) => {
              const binding = bindingMap.get(deviceKey(port.portId, device.deviceId));
              return <tr key={device.deviceId} className="virtual-row">
                <td><strong>{device.name || `设备 ${device.deviceId}`}</strong><span className="sub-cell">地址 {device.deviceAddress} · ID {device.deviceId}</span>{device.kksPrefix ? <span className="sub-cell">{device.kksPrefix}</span> : null}</td>
                <td><Link className="table-link" to={`/points?catalog_source=${encodeURIComponent(source.id)}&port=${encodeURIComponent(port.portId)}&device=${encodeURIComponent(device.deviceId)}`}>{formatNumber(device.pointCount)} 点</Link><span className="sub-cell">计算 {device.calculationCount} 点</span></td>
                <td>{device.groupId}<span className="sub-cell">{device.bound ? '设备映射' : '默认归属'}</span></td>
                <td><label className="field"><span className="sr-only">{port.portId} 设备 {device.deviceId} 逻辑组</span><select aria-label={`${port.portId} 设备 ${device.deviceId} 逻辑组`} value={binding?.groupId ?? ''} disabled={mutation.isPending} onChange={(event) => setBinding(port.portId, device.deviceId, event.target.value)}><option value="">默认归属：{editSource.groupId}</option>{binding && !groupIds.has(binding.groupId) ? <option value={binding.groupId}>已删除组：{binding.groupId}</option> : null}{groups.map((group) => <option key={group.id} value={group.id}>{group.name || group.id} ({group.id})</option>)}</select></label></td>
              </tr>;
            })}</tbody></table></div>}
          </section>
        ))}
        {orphanBindings.length > 0 ? <section className="topology-orphans"><h3>已配置但本次未发现的设备</h3><p>不会自动删除旧映射。确认设备已移除后可手动解除，并保存配置。</p>{orphanBindings.map((binding) => <div key={deviceKey(binding.portId, binding.deviceId)}><span>{binding.portId} / 设备 {binding.deviceId} → {binding.groupId}</span><button className="button button-small button-outline" type="button" disabled={mutation.isPending} onClick={() => setBinding(binding.portId, binding.deviceId, '')}>解除映射</button></div>)}</section> : null}
        <InlineNotice><p>计算点共 {formatNumber(topology.calculationCount)} 个，其中跨设备计算 {formatNumber(topology.crossDeviceCalculationCount)} 个。单设备计算随设备归组；跨设备及常量计算保留在默认归属组，忠实转发下级已算好的结果，不按不同设备的选源重新混算。未分配点 {formatNumber(topology.unassignedPointCount)} 个。</p></InlineNotice>
      </> : null}

      <InlineNotice tone="warning"><p>设备地址仅在串口内唯一，不等于跨下级逻辑组。多个设备共用物理串口时命令仍互斥；切源只影响目标 Session 的目标逻辑组。</p></InlineNotice>
      {onlineCount > 0 || pendingCount > 0 ? <div className="topology-lock" role="status">映射保存已锁定：{onlineCount} 个在线 Session，相关组 {pendingCount} 条待处理命令。请先结束全部在线 Session 并等待命令完成，再保存映射；不会主动断开上级。</div> : null}
      {sessionsQuery.error ? <div className="form-error" role="alert">无法确认在线 Session，暂不能保存。{apiErrorMessage(sessionsQuery.error)}<button className="table-link" type="button" onClick={() => void sessionsQuery.refetch()}>重试</button></div> : null}
      {staleRevision ? <div className="form-error" role="alert">配置版本已变化，不能覆盖他人的修改。请重新载入配置后再编辑。</div> : null}
      {invalidBinding ? <div className="form-error" role="alert">有设备关联到已不存在的逻辑组，请重新选择。</div> : null}
      {mutation.error ? <div className="form-error" role="alert">{mutation.error instanceof ApiError && mutation.error.status === 409 ? '保存冲突：可能存在在线 Session、待处理命令或配置版本已变化。请刷新状态后重试。' : '保存失败。'} {apiErrorMessage(mutation.error)}</div> : null}
      {rediscover.error ? <div className="form-error" role="alert">{apiErrorMessage(rediscover.error)}</div> : null}
      <div className="topology-footer">
        <button className="button button-secondary" type="button" disabled={mutation.isPending} onClick={resetDraft}>重新载入配置 / 撤销修改</button>
        <button className="button button-primary" type="button" disabled={!canSave} onClick={() => mutation.mutate()}><Save size={16} />{mutation.isPending ? '正在保存…' : '保存设备映射'}</button>
      </div>
    </Drawer>
  );
}
