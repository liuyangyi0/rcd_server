import { useCallback, useEffect, useRef, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { Download, Play, RefreshCw, RotateCw, Save, ScrollText, Square } from 'lucide-react';
import { ApiError, apiErrorMessage } from '../../api/client';
import { operationKeys, operationsApi, type OperationsTarget } from '../../api/operations';
import { PROCESS_POLL_WINDOW_MS, processApi, processKeys, processOperationTerminal, processTarget, useProcessAction, useProcessBinding, useProcessHistory, useProcessLog, useProcessOperation, useProcessStatus, useSaveProcessBinding, type ConfigRestartIntent, type ProcessAction, type ProcessBinding, type ProcessOperation, type ProcessOperationState, type ProcessState, type ProcessTarget, type SourceProcessStatus } from '../../api/process';
import { InlineNotice } from '../../components/InlineNotice';
import { Modal } from '../../components/Modal';
import { EmptyState, ErrorState, LoadingState } from '../../components/PageState';
import { StatusBadge } from '../../components/StatusBadge';
import { formatDateTime, formatDuration } from '../../lib/format';
import { downloadText, operationFailure } from './shared';
import { processMaintenanceActive, processMaintenanceBlocks } from '../../api/process';
import { ProcessMaintenanceNotice } from './ProcessMaintenanceNotice';
import { ProcessAgentIdentity } from './ProcessAgentIdentity';
import { FleetStatus } from './FleetStatus';

const stateLabels: Record<ProcessState, string> = { stopped: '已停止', starting: '正在启动', running: '运行中', stopping: '正在停止', failed: '进程失败', unknown: '状态未知' };
const actionLabels: Record<ProcessAction, string> = { start: '启动', stop: '停止', restart: '重启' };
const jobLabels: Record<ProcessOperationState, string> = { accepted: '已接受，等待执行', running: '正在执行', succeeded: '已完成', failed: '未完成', unknown: '结果未知，待对账' };
interface Props {
  target: OperationsTarget;
  writable: boolean;
  connected: boolean;
  restartIntent?: ConfigRestartIntent;
  onClearRestartIntent: () => void;
  onDirtyChange: (dirty: boolean) => void;
  onBusyChange: (busy: boolean) => void;
  onDialogChange: (open: boolean) => void;
  onControlPendingChange: (pending: boolean) => void;
}

export function ProcessTab(props: Props) {
  const binding = useProcessBinding(props.target);
  const status = useProcessStatus(props.target, binding.data);
  const [bindingDirty, setBindingDirty] = useState(false);
  const [controlPending, setControlPending] = useState(false);
  const reportDirty = useCallback((value: boolean) => { setBindingDirty(value); props.onDirtyChange(value); }, [props.onDirtyChange]);
  const reportPending = useCallback((value: boolean) => { setControlPending(value); props.onControlPendingChange(value); }, [props.onControlPendingChange]);
  const scope = processTarget(props.target, binding.data);
  const blocked = controlPending || processMaintenanceActive(status.data) || Boolean(status.data?.pending_request_id || (status.data?.pending_operation && !processOperationTerminal(status.data.pending_operation)));

  return <div className="operations-tab-content operations-process">
    <InlineNotice><strong>独立守护代理管理真实下级进程。</strong><p>进程运行不等于 OPC 已连接，也不等于串口采集正常。无活动维护时可通过守护代理启动；不会改变 Session 选源或自动切换到其他来源。配置文件编辑需要下级 OPC 在线；进程管理和守护日志不受此限制。</p></InlineNotice>
    {binding.error ? <ErrorState error={binding.error} title="无法读取守护绑定" onRetry={() => void binding.refetch()} /> : null}
    {!binding.data && binding.isPending ? <LoadingState label="正在读取守护代理绑定…" /> : binding.data ? <>
      {binding.data.registry_device_id ? <FleetStatus sourceId={props.target.id} /> : null}
      <BindingEditor target={props.target} binding={binding.data} writable={props.writable && !binding.error && !blocked && !binding.data.registry_device_id} onDirtyChange={reportDirty} onBusyChange={props.onBusyChange} refresh={() => void binding.refetch()} refreshing={binding.isFetching} />
      {scope ? <ProcessControl key={`${scope.bindingRevision}:${scope.instanceId}`} {...props} target={scope} binding={binding.data} writable={props.writable && !binding.error && !bindingDirty} onControlPendingChange={reportPending} /> : <EmptyState title="尚未绑定进程守护代理" description="填写此物理下级对应的代理地址和固定实例 ID，并显式保存。未绑定不会影响现有 OPC 数据，也不会自动发现或启动任何程序。" />}
      <ProcessHistoryView target={props.target} />
    </> : null}
  </div>;
}

function BindingEditor({ target, binding, writable, onDirtyChange, onBusyChange, refresh, refreshing }: {
  target: OperationsTarget; binding: ProcessBinding; writable: boolean; onDirtyChange: (value: boolean) => void; onBusyChange: (value: boolean) => void; refresh: () => void; refreshing: boolean;
}) {
  const [baseline, setBaseline] = useState(binding);
  const [agentUrl, setAgentUrl] = useState(binding.agent_url);
  const [instanceId, setInstanceId] = useState(binding.instance_id);
  const [editing, setEditing] = useState(!binding.agent_url);
  const [message, setMessage] = useState('');
  const save = useSaveProcessBinding(target);
  const dirty = agentUrl !== baseline.agent_url || instanceId !== baseline.instance_id;
  const stale = baseline.revision !== binding.revision;
  const complete = (!agentUrl.trim() && !instanceId.trim()) || (Boolean(agentUrl.trim()) && Boolean(instanceId.trim()));
  useEffect(() => { onDirtyChange(dirty); return () => onDirtyChange(false); }, [dirty, onDirtyChange]);
  useEffect(() => { onBusyChange(save.isPending); return () => onBusyChange(false); }, [save.isPending, onBusyChange]);
  const saveDraft = async () => {
    if (!dirty || !complete || !writable || stale || save.isPending) return;
    try {
      const result = await save.mutateAsync({ revision: baseline.revision, agentUrl, instanceId });
      setBaseline(result); setAgentUrl(result.agent_url); setInstanceId(result.instance_id); setEditing(false);
      setMessage('守护绑定已保存。本次没有启动、停止或重启任何进程。');
    } catch { /* Binding drafts and their original CAS revision survive errors. */ }
  };
  return <section className="operations-process-card" aria-label="守护代理绑定">
    <div className="operations-port-heading"><h3>守护代理绑定</h3><span className="operations-muted">绑定版本 {binding.revision}</span></div>
    <dl className="operations-file-meta"><div><dt>代理地址</dt><dd>{binding.agent_url || '未配置'}</dd></div><div><dt>固定实例 ID</dt><dd>{binding.instance_id || '未配置'}</dd></div></dl>
    <div className="operations-editor-actions"><button className="button button-small button-secondary" type="button" disabled={!writable || save.isPending || dirty} onClick={() => { setBaseline(binding); setAgentUrl(binding.agent_url); setInstanceId(binding.instance_id); setEditing(true); setMessage(''); save.reset(); }}>编辑守护绑定</button><button className="button button-small button-outline" type="button" disabled={refreshing || save.isPending} onClick={refresh}><RefreshCw size={14} aria-hidden="true" />刷新绑定（保留编辑）</button></div>
    {editing ? <form className="operations-process-binding" onSubmit={(event) => { event.preventDefault(); void saveDraft(); }}>
      <label className="field"><span>守护代理 URL</span><input aria-label="守护代理 URL" type="url" maxLength={2048} value={agentUrl} disabled={save.isPending} placeholder="http://lower-host:8090" onChange={(event) => { setAgentUrl(event.target.value); setMessage(''); save.reset(); }} /></label>
      <label className="field"><span>固定实例 ID</span><input aria-label="固定实例 ID" maxLength={256} value={instanceId} disabled={save.isPending} onChange={(event) => { setInstanceId(event.target.value); setMessage(''); save.reset(); }} /></label>
      <p className="operations-muted">地址和实例必须与已安装的守护代理一致；实例 ID 必须稳定且在本管理程序内全局唯一，不从 OPC 地址推断。两个字段同时清空并保存表示解除绑定，不会停止下级。</p>
      {stale ? <InlineNotice tone="warning">绑定已被其他操作修改。当前编辑和原版本保留；请核对后明确载入最新绑定，不能覆盖新目标。</InlineNotice> : null}
      <div className="operations-editor-actions"><button className="button button-primary" type="submit" disabled={!dirty || !complete || !writable || stale || save.isPending || refreshing}><Save size={15} aria-hidden="true" />{save.isPending ? '正在保存绑定…' : '保存守护绑定'}</button><button className="button button-outline" type="button" disabled={save.isPending || refreshing} onClick={() => { setBaseline(binding); setAgentUrl(binding.agent_url); setInstanceId(binding.instance_id); setMessage(''); save.reset(); }}>放弃编辑并载入已读绑定</button></div>
    </form> : null}
    {save.error ? <div className="top-error" role="alert">{operationFailure(save.error)} 绑定草稿已保留。</div> : null}
    {message ? <div className="success-banner" role="status">{message}</div> : null}
  </section>;
}

interface TrackedRequest { requestId: string; action: ProcessAction; expectedRevision: string; config?: ConfigRestartIntent; expectedConfigRevisions?: Record<string, string>; resolveAttempted?: boolean }
function trackingKey(target: ProcessTarget): string { return `rcd.process.pending:${JSON.stringify([target.id, target.revision, target.bindingRevision, target.instanceId])}`; }
function readTracking(target: ProcessTarget): TrackedRequest | undefined {
  try {
    const item = JSON.parse(window.sessionStorage.getItem(trackingKey(target)) || 'null') as TrackedRequest | null;
    return item && typeof item.requestId === 'string' && item.requestId.length <= 256 && ['start', 'stop', 'restart'].includes(item.action) && typeof item.expectedRevision === 'string' ? item : undefined;
  } catch { return undefined; }
}
function writeTracking(target: ProcessTarget, item?: TrackedRequest) {
  try { if (item) window.sessionStorage.setItem(trackingKey(target), JSON.stringify(item)); else window.sessionStorage.removeItem(trackingKey(target)); } catch { /* The backend also persists accepted request IDs. */ }
}
function newRequestId(): string {
  if (typeof window.crypto.randomUUID === 'function') return window.crypto.randomUUID();
  const bytes = window.crypto.getRandomValues(new Uint8Array(16));
  bytes[6] = (bytes[6] & 15) | 64; bytes[8] = (bytes[8] & 63) | 128;
  const hex = Array.from(bytes, (value) => value.toString(16).padStart(2, '0')).join('');
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
}

function ProcessControl({ target, binding, writable, connected, restartIntent, onClearRestartIntent, onBusyChange, onDialogChange, onControlPendingChange }: Props & { target: ProcessTarget; binding: ProcessBinding }) {
  const client = useQueryClient();
  const status = useProcessStatus(target, binding);
  const mutation = useProcessAction(target);
  const [tracked, setTracked] = useState(() => readTracking(target));
  const [confirming, setConfirming] = useState<{ action: ProcessAction; snapshot: SourceProcessStatus; config?: ConfigRestartIntent }>();
  const [confirmed, setConfirmed] = useState(false);
  const [checking, setChecking] = useState(false);
  const [localError, setLocalError] = useState('');
  const [showLog, setShowLog] = useState(false);
  const [pollRound, setPollRound] = useState(0);
  const [polling, setPolling] = useState(true);
  const [configCheck, setConfigCheck] = useState<{ requestId: string; state: 'checking' | 'applied' | 'pending' | 'error'; text: string }>();
  const [resolveError, setResolveError] = useState('');
  const resolvingId = useRef('');
  const alive = useRef(true);
  const currentWritable = useRef(writable);
  currentWritable.current = writable;
  const snapshot = status.data;
  const latestStatus = useRef({ snapshot, unavailable: Boolean(status.error) || status.isFetching });
  latestStatus.current = { snapshot, unavailable: Boolean(status.error) || status.isFetching };
  const requestId = tracked?.requestId || snapshot?.pending_request_id || snapshot?.pending_operation?.request_id || '';
  const busy = mutation.isPending || checking;
  // Persist the original request before sending it, but do not query/resolve it
  // until that first POST settles. A pre-admission 404 could otherwise seal an
  // action which is still on its way. This local gate is not persisted: a new
  // mount must reconcile an existing request whose delivery is now uncertain.
  const reconciliationEnabled = Boolean(requestId) && !busy;
  const job = useProcessOperation(target, requestId, polling, reconciliationEnabled);
  const terminal = processOperationTerminal(job.data);
  const pending = Boolean((requestId && !terminal) || snapshot?.pending_request_id || (snapshot?.pending_operation && !processOperationTerminal(snapshot.pending_operation)) || snapshot?.state === 'starting' || snapshot?.state === 'stopping');
  const actionsDisabled = !writable || !snapshot || Boolean(status.error) || status.isFetching || busy || pending || snapshot.state === 'unknown';
  const confirmationStale = !confirming || !snapshot || confirming.snapshot.revision !== snapshot.revision || confirming.snapshot.binding_revision !== snapshot.binding_revision;
  const effectiveIntent = restartIntent ?? tracked?.config;
  const configApplied = Boolean(effectiveIntent && tracked?.config && configCheck?.state === 'applied' && configCheck.requestId === requestId && job.data?.request_id === requestId && job.data?.state === 'succeeded' && tracked.config.name === effectiveIntent.name && tracked.config.revision === effectiveIntent.revision);

  useEffect(() => { alive.current = true; return () => { alive.current = false; }; }, []);
  useEffect(() => { onBusyChange(busy); return () => onBusyChange(false); }, [busy, onBusyChange]);
  useEffect(() => { onDialogChange(Boolean(confirming)); return () => onDialogChange(false); }, [Boolean(confirming), onDialogChange]);
  // Keep a local unresolved-result fence even when this tab is hidden. Returning
  // to this tab reconciles the original ID; other tabs remain readable.
  useEffect(() => { onControlPendingChange(pending); }, [pending, onControlPendingChange]);
  useEffect(() => {
    if (!reconciliationEnabled) return;
    setPolling(true);
    const timer = window.setTimeout(() => setPolling(false), PROCESS_POLL_WINDOW_MS);
    return () => window.clearTimeout(timer);
  }, [requestId, pollRound, reconciliationEnabled]);
  useEffect(() => {
    if (!terminal || !job.data) return;
    writeTracking(target);
    void Promise.all([client.invalidateQueries({ queryKey: processKeys.status(target), exact: true }), client.invalidateQueries({ queryKey: processKeys.history(target) }), client.invalidateQueries({ queryKey: operationKeys.capabilities(target) }), client.invalidateQueries({ queryKey: operationKeys.status(target) }), client.invalidateQueries({ queryKey: operationKeys.configs(target) })]);
  }, [client, target.id, target.revision, target.bindingRevision, target.instanceId, terminal, job.data?.request_id, job.data?.updated_at]);
  useEffect(() => {
    if (terminal && tracked && snapshot?.pending_request_id && snapshot.pending_request_id !== tracked.requestId) setTracked(undefined);
  }, [terminal, tracked?.requestId, snapshot?.pending_request_id]);
  useEffect(() => {
    if (!reconciliationEnabled || !tracked || tracked.resolveAttempted || !(job.error instanceof ApiError) || job.error.status !== 404 || resolvingId.current === tracked.requestId) return;
    const original = { ...tracked, resolveAttempted: true };
    resolvingId.current = original.requestId;
    setTracked(original); writeTracking(target, original);
    // A missing manager record is not proof that a delayed POST cannot arrive.
    // Resolve atomically seals that ID, rather than resending start/restart.
    void processApi.resolve(target, original.expectedRevision, original.action, original.requestId, original.expectedConfigRevisions).then((result) => {
      client.setQueryData(processKeys.operation(target, original.requestId), result);
      if (alive.current) setResolveError('');
      return Promise.all([client.invalidateQueries({ queryKey: processKeys.status(target), exact: true }), client.invalidateQueries({ queryKey: processKeys.history(target) })]);
    }).catch((error: unknown) => { if (alive.current) setResolveError(`同一请求的对账尚未确认，控制仍锁定。${apiErrorMessage(error)}`); });
  }, [client, target.id, target.revision, target.bindingRevision, target.instanceId, tracked?.requestId, tracked?.resolveAttempted, reconciliationEnabled, job.error]);

  const verifyConfiguration = useCallback(async (item: ProcessOperation, config: ConfigRestartIntent) => {
    setConfigCheck({ requestId: item.request_id, state: 'checking', text: '正在核对下级运行中的配置版本…' });
    try {
      const file = await operationsApi.config(target.id, config.name, '', undefined, target.revision);
      if (!alive.current) return;
      const applied = file.revision === config.revision && file.active_revision === config.revision && !file.restart_required;
      setConfigCheck({ requestId: item.request_id, state: applied ? 'applied' : 'pending', text: applied ? `${config.name} 的已保存版本已由下级确认生效。` : `${config.name} 尚未确认应用所保存版本（磁盘 ${file.revision}，运行 ${file.active_revision || '未提供'}）。请核对当前文件；进程运行不代表该配置已生效。` });
      void client.invalidateQueries({ queryKey: operationKeys.config(target, config.name) });
    } catch (error) { if (alive.current) setConfigCheck({ requestId: item.request_id, state: 'error', text: `进程作业已完成，但配置生效尚未确认。${apiErrorMessage(error)}` }); }
  }, [client, target.id, target.revision]);
  useEffect(() => {
    const item = job.data;
    if (!item || item.state !== 'succeeded' || item.action === 'stop' || !tracked?.config || !connected || configCheck?.requestId === item.request_id) return;
    void verifyConfiguration(item, tracked.config);
  }, [job.data?.request_id, job.data?.state, connected, tracked?.config?.name, tracked?.config?.revision, configCheck?.requestId, verifyConfiguration]);

  const openAction = (action: ProcessAction) => {
    if (actionsDisabled || !snapshot || processMaintenanceBlocks(snapshot, action)) return;
    setConfirmed(false); setLocalError(''); mutation.reset();
    setConfirming({ action, snapshot, config: action === 'stop' ? undefined : restartIntent });
  };
  const submitAction = async () => {
    if (!confirming || !confirmed || confirmationStale || actionsDisabled || processMaintenanceBlocks(snapshot, confirming.action)) return;
    const captured = confirming;
    setLocalError(''); setChecking(true);
    let request: TrackedRequest | undefined;
    try {
      if (captured.config && connected) {
        const current = await operationsApi.config(target.id, captured.config.name, '', undefined, target.revision);
        if (current.revision !== captured.config.revision) throw new ApiError(409, { code: 'conflict', message: '所保存文件已再次变化，未发送进程动作。请回到配置页核对；不能把当前磁盘版本当成之前确认的版本。' }, '配置版本冲突');
      }
      if (!alive.current || !currentWritable.current) return;
      const latest = latestStatus.current;
      // Query notifications can be batched until after this async continuation.
      // Check the cache as well as the rendered state before recording any POST.
      const cached = client.getQueryState<SourceProcessStatus>(processKeys.status(target));
      const current = cached?.data ?? latest.snapshot;
      if (latest.unavailable || cached?.status === 'error' || cached?.fetchStatus === 'fetching' || !current || current.revision !== captured.snapshot.revision || processMaintenanceBlocks(current, captured.action)) {
        throw new ApiError(409, { code: 'conflict', message: '代理状态或维护保护已变化，未发送进程动作。请取消并刷新核对。' }, '维护状态变化');
      }
      request = { requestId: newRequestId(), action: captured.action, expectedRevision: captured.snapshot.revision, config: captured.config, expectedConfigRevisions: captured.action === 'restart' && captured.config ? { [captured.config.name]: captured.config.revision } : undefined };
      writeTracking(target, request); setTracked(request); setConfigCheck(undefined); setPollRound((value) => value + 1); setConfirming(undefined);
      await mutation.mutateAsync({ revision: request.expectedRevision, action: request.action, requestId: request.requestId, expectedConfigRevisions: request.expectedConfigRevisions });
    } catch (error) {
      if (!alive.current) return;
      setLocalError(operationFailure(error));
      // A known rejection did not accept this action. Transport/5xx failures
      // retain the exact ID, including a temporary GET 404, until reconciliation.
      if (request && error instanceof ApiError && error.status >= 400 && error.status < 500 && error.code !== 'source_revision_conflict' && error.code !== 'binding_revision_conflict') { writeTracking(target); setTracked(undefined); }
    } finally { if (alive.current) setChecking(false); }
  };
  const refresh = () => {
    if (reconciliationEnabled && tracked?.resolveAttempted && resolveError && job.error instanceof ApiError && job.error.status === 404) {
      // Only an explicit operator refresh may re-attempt a failed seal. This is
      // still the original fingerprint, never another start/stop/restart.
      const original = { ...tracked, resolveAttempted: false };
      resolvingId.current = ''; setResolveError(''); setTracked(original); writeTracking(target, original);
    }
    setPollRound((value) => value + 1);
    // Query refetch bypasses `enabled`; apply the same admission gate to manual
    // refresh while still allowing process state, history and log reads.
    void Promise.all([status.refetch(), reconciliationEnabled ? job.refetch() : Promise.resolve(), client.invalidateQueries({ queryKey: processKeys.history(target) })]);
  };

  return <>
    {snapshot ? <ProcessAgentIdentity value={snapshot.agent_identity} stale={Boolean(status.error)} /> : null}
    <section className="operations-process-card" aria-label="下级进程状态">
      <div className="operations-port-heading"><h3>下级进程</h3><button className="button button-small button-secondary" type="button" disabled={status.isFetching || job.isFetching} onClick={refresh}><RefreshCw size={14} aria-hidden="true" />刷新进程与作业</button></div>
      {status.error ? <ErrorState error={status.error} title="代理状态未确认，不能判断进程已停止" onRetry={refresh} /> : null}
      {!snapshot && status.isPending ? <LoadingState label="正在读取守护代理状态…" /> : snapshot ? <>
        <div className="operations-state-row"><StatusBadge state={status.error || snapshot.state === 'unknown' ? 'warning' : snapshot.state === 'running' ? 'good' : snapshot.state === 'failed' ? 'bad' : 'unknown'} label={status.error ? '旧状态，仅供核对' : stateLabels[snapshot.state]} /><span>期望：{snapshot.desired_state === 'running' ? '运行' : '停止'}</span><span>OPC：{connected ? '已连接' : '未连接'}</span></div>
        <dl className="operations-file-meta"><div><dt>进程编号 PID</dt><dd>{snapshot.pid || '—'}</dd></div><div><dt>代理状态版本</dt><dd><code>{snapshot.revision}</code></dd></div><div><dt>启动时间</dt><dd>{formatDateTime(snapshot.started_at)}</dd></div><div><dt>已运行时间（采样值）</dt><dd>{formatDuration(snapshot.uptime_seconds ?? 0)}</dd></div><div><dt>程序路径</dt><dd>{snapshot.executable || '代理未提供'}</dd></div><div><dt>工作目录</dt><dd>{snapshot.work_dir || '代理未提供'}</dd></div><div><dt>守护自动拉起策略</dt><dd>{snapshot.restart_policy === 'on-failure' ? '异常退出后拉起' : snapshot.restart_policy === 'never' ? '不自动拉起' : '代理未提供'}</dd></div><div><dt>累计重启尝试（含手动）</dt><dd>{snapshot.restart_count ?? 0} 次</dd></div><div><dt>最近退出</dt><dd>{formatDateTime(snapshot.last_exit_at)} · 退出码 {snapshot.exit_code ?? '—'}{snapshot.last_stop_forced ? ' · 上次停止使用了强制终止' : ''}</dd></div></dl>
        {snapshot.last_error ? <div className="top-error" role="alert">最近进程错误：{snapshot.last_error}</div> : null}
        <ProcessMaintenanceNotice snapshot={snapshot} stale={Boolean(status.error)} />
      </> : null}
      {effectiveIntent ? <InlineNotice tone={configApplied ? 'info' : 'warning'}><strong>{configApplied ? '已核对配置' : '待核对配置'}：{effectiveIntent.name}</strong><p>所保存版本 <code>{effectiveIntent.revision}</code>。{configApplied ? '该版本已由下级确认生效；此确认仅对应本次请求及此文件版本，不代表其他配置或串口采集健康。' : '启动或重启应用该下级当前磁盘的全部已保存配置，不是单文件原子应用；重启成功后还需核对运行版本。'}</p>{!busy && restartIntent ? <button className="button button-small button-outline" type="button" onClick={onClearRestartIntent}>仅管理进程，不核对该配置</button> : null}</InlineNotice> : null}
      {pending ? <InlineNotice tone="warning">进程作业待完成或结果未知，暂不接受新的控制。可继续查看日志、刷新状态或关闭面板；关闭不会取消已提交动作，也不会自动重发。</InlineNotice> : null}
      <div className="operations-editor-actions"><button className="button button-primary" type="button" disabled={actionsDisabled || processMaintenanceBlocks(snapshot, 'start') || !['stopped', 'failed'].includes(snapshot?.state ?? '')} onClick={() => openAction('start')}><Play size={15} aria-hidden="true" />启动下级</button><button className="button button-outline" type="button" disabled={actionsDisabled || processMaintenanceBlocks(snapshot, 'stop') || snapshot?.state !== 'running'} onClick={() => openAction('stop')}><Square size={15} aria-hidden="true" />停止下级</button><button className="button button-secondary" type="button" disabled={actionsDisabled || processMaintenanceBlocks(snapshot, 'restart') || snapshot?.state !== 'running'} onClick={() => openAction('restart')}><RotateCw size={15} aria-hidden="true" />重启下级</button></div>
      <p className="operations-muted">手动停止会保留守护代理；再次启动仍须满足当前维护限制。自动拉起策略只读，由代理启动参数配置；不会自动故障切源。</p>
      {localError ? <div className="top-error" role="alert">{localError}</div> : null}
      {resolveError ? <div className="top-error" role="alert">{resolveError} 本页不会自动重试封存请求或改用新的动作编号；可手动刷新，同一请求仍只作对账。</div> : null}
      {requestId ? <div className="operations-process-job" role="status"><strong>{job.data ? jobLabels[job.data.state] : busy ? '正在提交，等待受理' : '请求结果尚未确认'}</strong><p>请求编号：<code>{requestId}</code></p>{job.data ? <p>动作：{actionLabels[job.data.action]} · 最后更新 {formatDateTime(job.data.updated_at)}{job.data.forced ? ' · 使用过强制停止' : ''}</p> : null}{job.data?.error_code === 'unconfirmed_cancelled' ? <p>本次未确认作业已封存，不会再次执行；请核对当前进程状态。</p> : job.data?.error ? <p>{job.data.error}</p> : null}{job.error ? <p>暂未取得作业权威结果；包括临时 404 在内的查询失败不会当作未执行。{apiErrorMessage(job.error)}</p> : null}{!terminal ? <p>{!reconciliationEnabled ? '正在等待首次提交响应；此时不会查询或封存该请求。' : polling ? '仅在此页可见时查询该请求，最多自动查询两分钟；不会重发控制动作。' : '自动查询窗口已结束，结果仍未确认。请手动刷新进程与作业；不会自动解锁或重发。'}</p> : job.data?.state === 'succeeded' ? <p>进程动作已确认完成；OPC 连接、串口采集与配置生效状态仍需分别核对。</p> : null}</div> : null}
      {job.data?.state === 'succeeded' && tracked?.config && job.data.action !== 'stop' ? <div className={configCheck?.state === 'applied' ? 'success-banner' : 'operations-confirm-inline'} role="status"><p>{configCheck?.text || '等待 OPC 重新连接后核对配置是否真正生效。进程运行不代表配置已应用。'}</p><button className="button button-small button-outline" type="button" disabled={!connected || configCheck?.state === 'checking'} onClick={() => void verifyConfiguration(job.data!, tracked.config!)}>核对配置生效状态</button></div> : null}
    </section>
    <section className="operations-process-card" aria-label="守护进程日志"><div className="operations-port-heading"><h3>守护日志 · stdout / stderr</h3><button className="button button-small button-outline" type="button" onClick={() => setShowLog((value) => !value)}><ScrollText size={15} aria-hidden="true" />{showLog ? '收起守护日志' : '读取守护日志'}</button></div><p className="operations-muted">下级停止后仍可经守护代理读取固定日志尾部，最多 64 KiB；不查询任意文件，不后台轮询。</p>{showLog ? <ProcessOutput target={target} /> : null}</section>
    <Modal open={Boolean(confirming)} title={`确认${confirming ? actionLabels[confirming.action] : ''}下级进程`} description="此操作作用于物理下级的整个进程，不是重连 OPC，也不是切换当前 Session 的数据源。" onClose={() => { if (!busy) setConfirming(undefined); }} footer={<><button className="button button-secondary" type="button" disabled={busy} onClick={() => setConfirming(undefined)}>取消</button><button className={`button ${confirming?.action === 'start' ? 'button-primary' : 'button-danger'}`} type="button" disabled={!confirmed || confirmationStale || actionsDisabled || Boolean(confirming && processMaintenanceBlocks(snapshot, confirming.action))} onClick={() => void submitAction()}>{busy ? '正在核对并提交…' : `确认${confirming ? actionLabels[confirming.action] : ''}进程`}</button></>}>
      <div className="operations-process-confirm">
      <p>物理来源：<strong>{target.id}</strong>；实例：<code>{target.instanceId}</code>；当前 PID：{confirming?.snapshot.pid || '—'}。</p>
      <p>停止或重启会中断该下级全部串口、站点、命令与所有客户端的数据；不会自动切换其他来源。守护代理不随下级停止，之后仍可启动。</p>
      <p>启动或重启将应用当前磁盘的全部已保存配置；未保存的浏览器草稿不会应用。只有明确提交后才执行，接受请求不代表执行完成。</p>
      {confirming?.config ? <p>本次核对 {confirming.config.name} 的版本 <code>{confirming.config.revision}</code>。{!connected ? '当前 OPC 离线，提交前无法重新读取文件；启动后会核对实际运行版本。' : '提交前会再次读取文件版本；发现变更则不发送动作。'}</p> : null}
      {confirmationStale ? <InlineNotice tone="warning">代理状态版本已变化。请取消，刷新状态后重新确认。</InlineNotice> : null}
      {confirming && processMaintenanceBlocks(snapshot, confirming.action) ? <InlineNotice tone="warning">维护保护已阻止此动作，请取消并核对发布状态。</InlineNotice> : null}
      {localError ? <div className="top-error" role="alert">{localError}</div> : null}
      <label className="operations-check"><input type="checkbox" checked={confirmed} disabled={busy} onChange={(event) => setConfirmed(event.target.checked)} /><span>我确认此操作影响该物理下级的全部站点及所有客户端，并已核对实例、进程和已保存配置。</span></label>
      </div>
    </Modal>
  </>;
}

function ProcessOutput({ target }: { target: ProcessTarget }) {
  const query = useProcessLog(target, true);
  return <>
    <div className="operations-editor-actions"><button className="button button-small button-secondary" type="button" disabled={query.isFetching} onClick={() => void query.refetch()}><RefreshCw size={14} aria-hidden="true" />刷新守护日志</button><button className="button button-small button-outline" type="button" disabled={!query.data} onClick={() => query.data && downloadText(`${target.id}-process-output.log`, query.data.text)}><Download size={14} aria-hidden="true" />下载已读取守护日志</button></div>
    {query.error ? <ErrorState error={query.error} title="守护日志读取失败，已有内容仅供核对" onRetry={() => void query.refetch()} /> : null}
    {!query.data && query.isPending ? <LoadingState label="正在读取守护日志尾部…" /> : query.data ? <><p className="operations-muted">文件共 {query.data.size_bytes} 字节；{query.data.truncated ? '仅显示有界尾部，前部已省略。' : '当前内容未截断。'}</p>{query.data.text ? <pre className="operations-log-text" aria-label="守护日志内容" tabIndex={0}>{query.data.text}</pre> : <EmptyState title="守护日志暂无内容" />}</> : null}
  </>;
}

function ProcessHistoryView({ target }: { target: OperationsTarget }) {
  const [offset, setOffset] = useState(0);
  const query = useProcessHistory(target, offset);
  return <section className="operations-process-card" aria-label="进程操作历史"><div className="operations-port-heading"><h3>进程操作历史</h3><button className="button button-small button-secondary" type="button" disabled={query.isFetching} onClick={() => void query.refetch()}><RefreshCw size={14} aria-hidden="true" />刷新操作历史</button></div><p className="operations-muted">管理程序持久化的作业记录，代理离线时仍可查询。历史实例和版本不代表当前绑定或进程状态。</p>
    {query.error ? <ErrorState error={query.error} title="进程历史读取失败" onRetry={() => void query.refetch()} /> : null}
    {!query.data && query.isPending ? <LoadingState label="正在读取进程操作历史…" /> : query.data?.items.length ? <div className="table-scroll"><table className="data-table operations-process-history"><thead><tr><th>时间 / 请求编号</th><th>实例 / 绑定版本</th><th>动作 / 结果</th></tr></thead><tbody>{query.data.items.map((item) => <tr key={item.request_id}><td>{formatDateTime(item.created_at)}<code>{item.request_id}</code></td><td>{item.instance_id}<span className="operations-muted">绑定 {item.binding_revision} · 来源 {item.source_revision}</span></td><td>{actionLabels[item.action]} · {jobLabels[item.state]}{item.error ? <p>{item.error}</p> : null}{item.forced ? <span>使用过强制停止</span> : null}</td></tr>)}</tbody></table></div> : query.data ? <EmptyState title="暂无进程操作记录" /> : null}
    {query.data ? <div className="operations-log-pagination"><span className="operations-muted">共 {query.data.total} 条，每页最多 20 条</span><button className="button button-small button-outline" type="button" disabled={offset === 0 || query.isFetching} onClick={() => setOffset((value) => Math.max(0, value - 20))}>上一页操作</button><button className="button button-small button-outline" type="button" disabled={offset + query.data.items.length >= query.data.total || query.isFetching} onClick={() => setOffset((value) => value + 20)}>下一页操作</button></div> : null}
  </section>;
}
