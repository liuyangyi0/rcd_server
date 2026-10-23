import { lazy, Suspense, useCallback, useEffect, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { Activity, Cpu, FileText, ScrollText } from 'lucide-react';
import { ApiError } from '../../api/client';
import { operationKeys, useOperationsCapabilities, type OperationFeature } from '../../api/operations';
import { processMaintenanceActive, processOperationTerminal, useProcessBinding, useProcessStatus, type ConfigRestartIntent } from '../../api/process';
import type { Source } from '../../api/types';
import { Drawer } from '../../components/Drawer';
import { InlineNotice } from '../../components/InlineNotice';
import { Modal } from '../../components/Modal';
import { EmptyState, ErrorState, LoadingState } from '../../components/PageState';
import { StatusBadge } from '../../components/StatusBadge';
import { stateLabel } from '../../lib/labels';
import { AcquisitionTab } from './AcquisitionTab';
import { ConfigurationTab } from './ConfigurationTab';
import { LogsTab } from './LogsTab';
import { discardConfigurationDrafts, rememberedConfigurationFile } from './configurationDrafts';

const ProcessTab = lazy(() => import('./ProcessTab').then((module) => ({ default: module.ProcessTab })));
type OperationTab = OperationFeature | 'process';
const tabs = [{ id: 'acquisition', label: '采集', icon: Activity }, { id: 'configuration', label: '配置', icon: FileText }, { id: 'logs', label: '日志', icon: ScrollText }, { id: 'process', label: '进程', icon: Cpu }] as const;
interface Props { source: Source; sourceDeleted?: boolean; onClose: () => void }

export default function SourceOperationsDrawer(props: Props) {
  // Also reset safely when rendered by a caller which did not provide a key.
  return <OperationsPanel key={props.source.id} {...props} />;
}

function OperationsPanel({ source, sourceDeleted = false, onClose }: Props) {
  const client = useQueryClient();
  const [target] = useState(() => ({ id: source.id, revision: source.revision, endpoint: source.endpoint }));
  const capabilities = useOperationsCapabilities(target);
  // Guarding physical mutations needs one status snapshot, not per-source
  // background polling. ProcessTab alone polls a visible pending request.
  const processBinding = useProcessBinding(target);
  const processStatus = useProcessStatus(target, processBinding.data);
  const [selectedTab, setSelectedTab] = useState<OperationTab>(() => rememberedConfigurationFile(client, target) ? 'configuration' : 'acquisition');
  const [restartIntent, setRestartIntent] = useState<ConfigRestartIntent>();
  const [unresolvedProcess, setUnresolvedProcess] = useState(false);
  const [dirty, setDirty] = useState(false);
  const [busy, setBusy] = useState(false);
  const [childDialog, setChildDialog] = useState(false);
  const [discarding, setDiscarding] = useState(false);
  const supported = capabilities.data?.supported && capabilities.data.schema_version === 1;
  const features = capabilities.data?.features ?? [];
  const tab = dirty || busy || selectedTab === 'process' ? selectedTab : features.includes(selectedTab) ? selectedTab : tabs.find((item) => item.id !== 'process' && features.includes(item.id))?.id ?? 'acquisition';
  const sourceChanged = target.revision !== source.revision || target.endpoint !== source.endpoint;
  const connected = !sourceDeleted && source.enabled && source.connectionState.trim().toLowerCase() === 'connected';
  const capabilityConflict = capabilities.error instanceof ApiError && capabilities.error.code === 'source_revision_conflict';
  const processPending = unresolvedProcess || Boolean(processStatus.data?.pending_request_id || (processStatus.data?.pending_operation && !processOperationTerminal(processStatus.data.pending_operation)));
  const maintenanceActive = processMaintenanceActive(processStatus.data);
  const writable = connected && !sourceChanged && Boolean(supported) && !capabilityConflict && !processPending && !maintenanceActive;
  const reportDirty = useCallback((value: boolean) => { if (value && tab) setSelectedTab(tab); setDirty(value); }, [tab]);
  const reportBusy = useCallback((value: boolean) => { if (value && tab) setSelectedTab(tab); setBusy(value); }, [tab]);
  const requestClose = useCallback(() => {
    if (busy || childDialog || discarding) return;
    if (dirty) setDiscarding(true);
    else onClose();
  }, [busy, childDialog, discarding, dirty, onClose]);
  const discardAndClose = () => { discardConfigurationDrafts(client, target); onClose(); };

  useEffect(() => () => { void client.cancelQueries({ queryKey: operationKeys.source(target) }); }, [client, target.id, target.revision]);
  useEffect(() => {
    if (!dirty) return;
    const warn = (event: BeforeUnloadEvent) => { event.preventDefault(); event.returnValue = ''; };
    window.addEventListener('beforeunload', warn);
    return () => window.removeEventListener('beforeunload', warn);
  }, [dirty]);

  return <>
    <div className="operations-backdrop" aria-hidden="true" onClick={requestClose} />
    <Drawer open title={`下级运维 · ${target.id}`} className="operations-drawer" onClose={requestClose}>
      <div className="operations-heading"><p className="member-endpoint">{target.endpoint}</p><StatusBadge state={connected ? 'good' : 'warning'} label={sourceDeleted ? '来源已删除' : `OPC ${stateLabel(source.connectionState)}`} compact /><span className="operations-muted">来源配置版本 {target.revision}</span></div>
      {sourceDeleted ? <InlineNotice tone="warning">此来源已从管理配置删除。当前面板与未保存草稿已保留，所有写操作已锁定；可下载草稿，明确关闭后再操作其他来源。</InlineNotice> : null}
      {sourceChanged ? <InlineNotice tone="warning">来源配置已变为版本 {source.revision}。当前抽屉保留旧目标和草稿，已锁定所有写操作；请先下载草稿，再关闭并重新打开运维。</InlineNotice> : null}
      {!connected && !sourceDeleted ? <InlineNotice tone="warning">下级 OPC 当前未连接，不能确认最新采集状态。已有内容仅供核对或下载；“进程”页仍可通过独立守护代理查询、启动下级，不会自动重试写操作。</InlineNotice> : null}
      {processPending && tab !== 'process' ? <InlineNotice tone="warning">存在尚未核对完成的进程作业，采集控制和配置保存暂时锁定；读取、草稿与日志仍可使用。请到“进程”页按原请求编号核对结果。</InlineNotice> : null}
      {maintenanceActive && tab !== 'process' ? <InlineNotice tone="warning">发布维护尚未结束，采集控制和配置保存暂时锁定；草稿、读取与日志仍可使用。请到“进程”页查看维护阶段。</InlineNotice> : null}
      {busy ? <p className="operations-muted" role="status">正在核对请求结果，请稍候再关闭或切换标签。</p> : null}
      <div className="tab-list operations-tabs" role="tablist" aria-label="下级运维功能">{tabs.map(({ id, label, icon: Icon }) => {
        const available = id === 'process' || features.includes(id);
        return <button key={id} id={`operations-tab-${id}`} type="button" role="tab" aria-selected={tab === id} aria-controls="operations-active-panel" className={tab === id ? 'active' : ''} disabled={!available || ((dirty || busy) && tab !== id)} title={!available ? '此下级未提供该功能' : dirty && tab !== id ? '请先保存或明确放弃草稿' : undefined} onClick={() => setSelectedTab(id)}><Icon size={16} aria-hidden="true" />{label}</button>;
      })}</div>
      <div role="tabpanel" id="operations-active-panel" aria-labelledby={`operations-tab-${tab}`}>
        {tab === 'process' ? <Suspense fallback={<LoadingState label="正在加载进程管理…" />}><ProcessTab target={target} writable={!sourceDeleted && !sourceChanged} connected={connected} restartIntent={restartIntent} onClearRestartIntent={() => setRestartIntent(undefined)} onDirtyChange={reportDirty} onBusyChange={reportBusy} onDialogChange={setChildDialog} onControlPendingChange={setUnresolvedProcess} /></Suspense> : <>
          {capabilities.error ? <ErrorState error={capabilities.error} title="无法读取下级运维能力" onRetry={() => void capabilities.refetch()} /> : null}
          {!supported && (dirty || busy) ? <InlineNotice tone="warning">下级运维能力已变化。当前草稿保留，写入已锁定；请先下载草稿再关闭面板。</InlineNotice> : null}
          {!capabilities.data && capabilities.isPending ? <LoadingState label="正在检查下级运维能力…" /> : capabilities.data && !supported && !dirty && !busy ? <EmptyState title={capabilities.data.supported ? '下级运维协议版本暂不兼容' : '此下级未提供运维能力'} description={capabilities.data.reason || '旧版下级仍可正常提供 OPC 数据；独立进程守护功能可在“进程”页配置，不依赖此能力。'} /> : supported || dirty || busy ? <>
            {tab === 'acquisition' ? <AcquisitionTab target={target} writable={writable} stationVersions={features.includes('station_versions')} onBusyChange={reportBusy} onDialogChange={setChildDialog} /> : tab === 'configuration' ? <ConfigurationTab target={target} writable={writable} forms={features.includes('configuration_forms')} acquisition={features.includes('acquisition')} maxBytes={capabilities.data?.max_config_bytes ?? 2_097_152} onDirtyChange={reportDirty} onBusyChange={reportBusy} onRestartRequested={(intent) => { setRestartIntent(intent); setSelectedTab('process'); }} /> : tab === 'logs' ? <LogsTab target={target} maxBytes={capabilities.data?.max_log_bytes ?? 65_536} /> : <EmptyState title="下级未声明可用的运维功能" />}
          </> : null}
        </>}
      </div>
    </Drawer>
    <Modal open={discarding} title="放弃未保存的配置草稿？" description="明确放弃会清除本来源的内存草稿，不会写入下级。可取消并先下载草稿；普通页面跳转则保留当前登录期间的副本。" onClose={() => setDiscarding(false)} footer={<><button className="button button-secondary" type="button" onClick={() => setDiscarding(false)}>保留草稿，继续编辑</button><button className="button button-danger" type="button" onClick={discardAndClose}>放弃草稿并关闭</button></>}><p>此操作只丢弃浏览器内的编辑内容，不会修改下级配置文件。</p></Modal>
  </>;
}
