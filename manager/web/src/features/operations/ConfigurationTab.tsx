import { useCallback, useEffect, useRef, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { Link } from 'react-router-dom';
import { Download, RefreshCw, Save, ShieldCheck } from 'lucide-react';
import { ApiError, apiErrorMessage } from '../../api/client';
import { MAX_CONFIG_BYTES, operationKeys, operationsApi, useAcquisitionStatus, useOperationConfig, useOperationConfigs, useSaveOperationConfig, useValidateOperationConfig, type ConfigDocument, type ConfigFiles, type ConfigValidation, type OperationsTarget } from '../../api/operations';
import type { ConfigRestartIntent } from '../../api/process';
import { InlineNotice } from '../../components/InlineNotice';
import { EmptyState, ErrorState, LoadingState } from '../../components/PageState';
import { formatDateTime } from '../../lib/format';
import { byteSize, downloadText, operationBusy, operationFailure } from './shared';

interface Props { target: OperationsTarget; writable: boolean; maxBytes: number; onDirtyChange: (dirty: boolean) => void; onBusyChange: (busy: boolean) => void; onRestartRequested?: (intent: ConfigRestartIntent) => void }

export function ConfigurationTab(props: Props) {
  const files = useOperationConfigs(props.target);
	const status = useAcquisitionStatus(props.target);
	const managerMode = status.data?.calculation?.active_mode === 'manager';
  return <div className="operations-tab-content">
    <InlineNotice tone="warning"><strong>保存后需重启下级，不自动重启。</strong><p>配置校验不等于保存。备份仅载入编辑草稿，核对并校验后才可显式保存；采集页的运行状态不代表新文件已应用。</p></InlineNotice>
    {files.error ? <ErrorState error={files.error} title="无法刷新配置文件列表" onRetry={() => void files.refetch()} /> : null}
	{managerMode ? <InlineNotice><strong>此下级的 calc.toml 当前未生效，由管理端接管。</strong><p>本机文件保留为回滚副本并以只读方式显示；请到<Link to="/calculations?tab=config">计算中心</Link>编辑、校验和激活中央规则。</p></InlineNotice> : null}
    {!files.data && files.isPending ? <LoadingState label="正在读取配置文件列表…" /> : files.data ? <ConfigWorkspace {...props} managerMode={managerMode} files={files.data} refreshing={files.isFetching} refreshFiles={() => void files.refetch()} /> : null}
  </div>;
}

function ConfigWorkspace({ target, writable, managerMode, maxBytes, onDirtyChange, onBusyChange, onRestartRequested, files, refreshing, refreshFiles }: Props & { managerMode: boolean; files: ConfigFiles; refreshing: boolean; refreshFiles: () => void }) {
  // Pick once after the list arrives. Background list additions/reordering must
  // not change the edited filename and silently unmount a dirty editor.
  const [selected, setSelected] = useState(() => files.items[0]?.name || '');
  const [dirty, setDirty] = useState(false);
  const [busy, setBusy] = useState(false);
  const name = selected;
  const reportDirty = useCallback((value: boolean) => { setDirty(value); onDirtyChange(value); }, [onDirtyChange]);
  const reportBusy = useCallback((value: boolean) => { setBusy(value); onBusyChange(value); }, [onBusyChange]);

  return <>
    <div className="operations-toolbar">
      <label className="field operations-file-picker"><span>配置文件</span><select aria-label="配置文件" value={name} disabled={dirty || busy || !files.items.length} onChange={(event) => setSelected(event.target.value)}>{!selected ? <option value="">请选择配置文件</option> : null}{selected && !files.items.some((file) => file.name === selected) ? <option value={selected}>{selected}（已不在文件列表）</option> : null}{files.items.map((file) => <option key={file.name} value={file.name}>{file.name} · {file.kind} · {byteSize(file.size_bytes)}{file.restart_required ? ' · 待重启' : ''}</option>)}</select></label>
      <button className="button button-secondary button-small" type="button" disabled={refreshing} onClick={refreshFiles}><RefreshCw size={15} aria-hidden="true" />刷新文件列表</button>
    </div>
    {dirty ? <p className="operations-muted">有未保存草稿。保存或明确放弃后，才能切换文件或标签页；后台刷新不会覆盖编辑内容。</p> : null}
    {!name ? <EmptyState title={files.items.length ? '请选择配置文件' : '没有可管理的配置文件'} description="只编辑下级公布的配置文件；不会自动创建或猜测文件路径。" /> : <ConfigFilePane key={name} target={target} name={name} writable={writable && !(managerMode && name === 'calc.toml')} maxBytes={maxBytes} onDirtyChange={reportDirty} onBusyChange={reportBusy} onRestartRequested={onRestartRequested} />}
  </>;
}

function ConfigFilePane({ target, name, writable, maxBytes, onDirtyChange, onBusyChange, onRestartRequested }: Props & { name: string }) {
  const query = useOperationConfig(target, name);
  return <>
    {query.error ? <ErrorState error={query.error} title={query.data ? '文件刷新失败，草稿保持不变' : '无法读取配置文件'} onRetry={() => void query.refetch()} /> : null}
    {!query.data && query.isPending ? <LoadingState label="正在读取配置内容…" /> : query.data ? <ConfigEditor target={target} document={query.data} writable={writable && !query.error} maxBytes={maxBytes} refreshing={query.isFetching} refresh={async () => (await query.refetch({ throwOnError: true })).data} onDirtyChange={onDirtyChange} onBusyChange={onBusyChange} onRestartRequested={onRestartRequested} /> : null}
  </>;
}

interface EditorProps extends Props { document: ConfigDocument; refreshing: boolean; refresh: () => Promise<ConfigDocument | undefined> }

function ConfigEditor({ target, document, writable, maxBytes, refreshing, refresh, onDirtyChange, onBusyChange, onRestartRequested }: EditorProps) {
  const client = useQueryClient();
  // The editable baseline and its CAS revision are one snapshot. New query
  // data is comparison-only until the operator explicitly reloads or saves.
  const [baseline, setBaseline] = useState(document);
  const [draft, setDraft] = useState(document.content);
  const [validated, setValidated] = useState<{ content: string; result: ConfigValidation }>();
  const [backupId, setBackupId] = useState('');
  const [loadedBackup, setLoadedBackup] = useState('');
  const [loadingBackup, setLoadingBackup] = useState(false);
  const [reloading, setReloading] = useState(false);
  const [discarding, setDiscarding] = useState(false);
  const [localError, setLocalError] = useState('');
  const [message, setMessage] = useState('');
  const alive = useRef(true);
  const validate = useValidateOperationConfig(target, baseline.name);
  const save = useSaveOperationConfig(target, baseline.name);
  const dirty = draft !== baseline.content;
  const busy = save.isPending || validate.isPending || loadingBackup || reloading;
  const limit = Math.max(1, Math.min(MAX_CONFIG_BYTES, maxBytes || MAX_CONFIG_BYTES));
  const stale = document.revision !== baseline.revision;
  const targetChanged = baseline.source_id !== target.id || baseline.source_revision !== target.revision || document.source_id !== target.id || document.source_revision !== target.revision;
  const validation = validated?.content === draft ? validated.result : undefined;
  const canSave = dirty && validation?.valid && !busy && !refreshing && !stale && !targetChanged && writable;

  useEffect(() => { alive.current = true; return () => { alive.current = false; }; }, []);
  useEffect(() => { onDirtyChange(dirty); return () => onDirtyChange(false); }, [dirty, onDirtyChange]);
  useEffect(() => { onBusyChange(busy); return () => onBusyChange(false); }, [busy, onBusyChange]);

  const edit = (content: string) => {
    if (new TextEncoder().encode(content).byteLength > limit) { setLocalError(`内容超过 ${byteSize(limit)}，未载入超限内容，原草稿已保留。`); return; }
    setDraft(content); setValidated(undefined); setLocalError(''); setMessage(''); setDiscarding(false); save.reset(); validate.reset();
  };
  const runValidation = async () => {
    const content = draft;
    setLocalError('');
    try {
      const result = await validate.mutateAsync(content);
      if (alive.current) setValidated({ content, result });
    } catch { /* the mutation renders the shared API error */ }
  };
  const saveDraft = async () => {
    if (!canSave) return;
    try {
      const result = await save.mutateAsync({ revision: baseline.revision, content: draft });
      if (!alive.current) return;
      setBaseline(result); setDraft(result.content); setLoadedBackup(''); setValidated(undefined); setDiscarding(false);
      setMessage('配置已保存到下级文件；需要手动重启下级后生效，本操作未重启程序。');
    } catch { /* retain both draft and its original revision on every failure */ }
  };
  const loadBackup = async () => {
    if (!backupId || busy) return;
    setLoadingBackup(true); setLocalError(''); setMessage('');
    try {
      const backup = await client.fetchQuery({ queryKey: operationKeys.config(target, baseline.name, backupId), queryFn: ({ signal }) => operationsApi.config(target.id, baseline.name, backupId, signal, target.revision), retry: false, staleTime: 0, gcTime: 0 });
      if (!alive.current) return;
      if (backup.source_id !== target.id || backup.source_revision !== target.revision || backup.name !== baseline.name) throw new Error('备份不属于当前来源版本或配置文件');
      if (new TextEncoder().encode(backup.content).byteLength > limit) throw new Error('备份超过允许的配置大小，未覆盖草稿');
      // Never replace the current file's revision with the backup hash. Loading
      // a backup is only an edit; the normal validate/save path performs CAS.
      setDraft(backup.content); setLoadedBackup(backup.viewing_backup_id || backupId); setValidated(undefined); save.reset(); validate.reset(); setDiscarding(false);
    } catch (error) { if (alive.current) setLocalError(`备份载入失败，草稿已保留。${apiErrorMessage(error)}`); }
    finally { if (alive.current) setLoadingBackup(false); }
  };
  const reloadCurrent = async () => {
    setReloading(true); setLocalError('');
    try {
      const latest = await refresh();
      if (!alive.current || !latest) return;
      setBaseline(latest); setDraft(latest.content); setLoadedBackup(''); setValidated(undefined); setDiscarding(false); setMessage('已明确放弃原草稿，并载入下级当前文件。'); save.reset(); validate.reset();
    } catch (error) { if (alive.current) setLocalError(`重新读取失败，草稿未丢弃。${apiErrorMessage(error)}`); }
    finally { if (alive.current) setReloading(false); }
  };

  return <section className="operations-editor" aria-label={`配置编辑 ${baseline.name}`}>
    <dl className="operations-file-meta"><div><dt>编辑基线版本</dt><dd><code>{baseline.revision}</code></dd></div><div><dt>运行中版本</dt><dd><code>{document.active_revision || '未提供'}</code></dd></div><div><dt>文件应用状态</dt><dd>{document.restart_required ? '已保存，待重启' : '文件版本与运行版本一致'}</dd></div></dl>
    {document.restart_required && onRestartRequested ? <InlineNotice tone="warning"><p>可以前往“进程”页显式重启。重启会应用此下级当前磁盘的全部已保存配置，并影响全部站点与客户端；保存按钮本身不会重启。</p><button className="button button-small button-secondary" type="button" disabled={dirty || busy || refreshing || stale || targetChanged || !writable} onClick={() => onRestartRequested({ name: document.name, revision: document.revision })}>前往进程页应用已保存配置</button></InlineNotice> : null}
    {stale ? <InlineNotice tone="warning">下级文件已被其他操作修改。草稿与原版本已保留；请下载草稿、核对最新文件，再明确放弃旧基线并重新编辑。</InlineNotice> : null}
    {targetChanged ? <InlineNotice tone="warning">来源配置版本已变化，不能向旧目标保存。请下载草稿后关闭抽屉并重新打开。</InlineNotice> : null}
    {loadedBackup ? <InlineNotice>已将备份 {loadedBackup} 载入草稿，尚未保存或应用。请校验并显式保存。</InlineNotice> : null}
    <div className="operations-backups"><label className="field"><span>自动备份版本</span><select aria-label="自动备份版本" value={backupId} onChange={(event) => setBackupId(event.target.value)} disabled={busy}><option value="">{document.backups.length ? '选择备份，只载入草稿' : '暂无自动备份'}</option>{document.backups.map((backup) => <option value={backup.id} key={backup.id}>{formatDateTime(backup.created_at)} · {backup.id} · {byteSize(backup.size_bytes)}</option>)}</select></label><button className="button button-small button-outline" type="button" disabled={!backupId || busy || !writable} onClick={() => void loadBackup()}>{loadingBackup ? '正在读取备份…' : '载入备份为草稿'}</button></div>
    {document.backups_truncated ? <p className="operations-muted">仅列出最近 200 份自动备份，旧备份未删除。</p> : null}
	{!writable && baseline.name === 'calc.toml' ? <InlineNotice>只读回滚副本：当前下级为 manager 模式，请在计算中心维护活动规则。</InlineNotice> : null}
    <label className="field operations-code-field"><span>配置内容 · {baseline.name}{dirty ? ' · 未保存' : ''}</span><textarea aria-label={`配置内容 ${baseline.name}`} spellCheck={false} value={draft} disabled={!writable || save.isPending || loadingBackup || reloading} onChange={(event) => edit(event.target.value)} /></label>
    <p className="operations-muted">UTF-8 文本，最多 {byteSize(limit)}。页面刷新、校验和备份预览都不会自动写入文件。</p>
    {localError ? <div className="top-error" role="alert">{localError}</div> : null}
    {validate.error ? <div className="top-error" role="alert">校验请求失败，未保存配置。{apiErrorMessage(validate.error)}</div> : null}
    {validation ? <div className={`operations-validation ${validation.valid ? 'is-valid' : 'is-invalid'}`} role={validation.valid ? 'status' : 'alert'}><strong>{validation.valid ? '当前草稿校验通过' : '当前草稿校验未通过'}</strong>{validation.errors.map((error, index) => <p key={`${index}-${error}`}>{error}</p>)}{validation.warnings.map((warning, index) => <p key={`${index}-${warning}`}>警告：{warning}</p>)}</div> : null}
    {save.error ? <div className="top-error" role="alert">{save.error instanceof ApiError && save.error.status === 409 && !operationBusy(save.error) ? '保存冲突：草稿及原版本已保留。已刷新下级文件，请核对最新版本后重新编辑。' : `${operationFailure(save.error)} 草稿已保留。`}</div> : null}
    {message ? <div className="success-banner" role="status">{message}</div> : null}
    <div className="operations-editor-actions"><button className="button button-secondary" type="button" disabled={busy || !writable || targetChanged} onClick={() => void runValidation()}><ShieldCheck size={16} aria-hidden="true" />{validate.isPending ? '正在校验…' : '校验当前草稿'}</button><button className="button button-primary" type="button" disabled={!canSave} onClick={() => void saveDraft()}><Save size={16} aria-hidden="true" />{save.isPending ? '正在保存…' : '保存到下级'}</button><button className="button button-outline" type="button" onClick={() => downloadText(`${target.id}-${baseline.name}.draft`, draft)}><Download size={15} aria-hidden="true" />下载草稿</button><button className="button button-outline" type="button" onClick={() => downloadText(`${target.id}-${document.name}`, document.content)}><Download size={15} aria-hidden="true" />下载已读取配置</button></div>
    <div className="operations-editor-actions"><button className="button button-small button-secondary" type="button" disabled={busy || refreshing} onClick={() => void refresh().catch((error: unknown) => setLocalError(apiErrorMessage(error)))}><RefreshCw size={15} aria-hidden="true" />刷新文件版本（保留草稿）</button><button className="button button-small button-outline" type="button" disabled={busy || refreshing} onClick={() => setDiscarding(true)}>放弃草稿并载入最新</button></div>
    {discarding ? <div className="operations-confirm-inline" role="alert"><p>确认放弃当前草稿？只有成功读取最新文件后才会替换编辑内容。</p><div className="operations-editor-actions"><button className="button button-danger button-small" type="button" disabled={busy} onClick={() => void reloadCurrent()}>确认放弃并载入最新</button><button className="button button-secondary button-small" type="button" disabled={busy} onClick={() => setDiscarding(false)}>取消放弃</button></div></div> : null}
  </section>;
}
