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
import { ConfigParameters, type ConfigParametersHandle } from './ConfigParameters';
import { forgetConfigurationDraft, readConfigurationDraft, rememberedConfigurationFile, rememberConfigurationDraft } from './configurationDrafts';
import './ConfigurationTab.css';

interface Props { target: OperationsTarget; writable: boolean; maxBytes: number; forms?: boolean; acquisition?: boolean; onDirtyChange: (dirty: boolean) => void; onBusyChange: (busy: boolean) => void; onRestartRequested?: (intent: ConfigRestartIntent) => void }

export function ConfigurationTab(props: Props) {
  const files = useOperationConfigs(props.target);
	const status = useAcquisitionStatus(props.target, false, props.acquisition ?? true);
	const managerMode = status.data?.calculation?.active_mode === 'manager';
  return <div className="operations-tab-content">
    <p className="operations-muted">先选择文件，再按“修改 → 校验 → 保存”操作。保存不会自动重启，明确重启下级后才生效。</p>
    {files.error ? <ErrorState error={files.error} title="无法刷新配置文件列表" onRetry={() => void files.refetch()} /> : null}
	{managerMode ? <InlineNotice><strong>此下级的 calc.toml 当前未生效，由管理端接管。</strong><p>本机文件保留为回滚副本并以只读方式显示；请到<Link to="/calculations?tab=config">计算中心</Link>编辑、校验和激活中央规则。</p></InlineNotice> : null}
    {!files.data && files.isPending ? <LoadingState label="正在读取配置文件列表…" /> : files.data ? <ConfigWorkspace {...props} managerMode={managerMode} files={files.data} refreshing={files.isFetching} refreshFiles={() => void files.refetch()} /> : null}
  </div>;
}

function ConfigWorkspace({ target, writable, managerMode, maxBytes, forms, onDirtyChange, onBusyChange, onRestartRequested, files, refreshing, refreshFiles }: Props & { managerMode: boolean; files: ConfigFiles; refreshing: boolean; refreshFiles: () => void }) {
  const client = useQueryClient();
  // Pick once after the list arrives. Background list additions/reordering must
  // not change the edited filename and silently unmount a dirty editor.
  const [selected, setSelected] = useState(() => rememberedConfigurationFile(client, target) || files.items[0]?.name || '');
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
    {!name ? <EmptyState title={files.items.length ? '请选择配置文件' : '没有可管理的配置文件'} description="只编辑下级公布的配置文件；不会自动创建或猜测文件路径。" /> : <ConfigFilePane key={name} target={target} name={name} forms={forms} writable={writable && !(managerMode && name === 'calc.toml')} maxBytes={maxBytes} onDirtyChange={reportDirty} onBusyChange={reportBusy} onRestartRequested={onRestartRequested} />}
  </>;
}

function ConfigFilePane({ target, name, writable, maxBytes, forms, onDirtyChange, onBusyChange, onRestartRequested }: Props & { name: string }) {
  const client = useQueryClient();
  const query = useOperationConfig(target, name);
  const document = query.data ?? readConfigurationDraft(client, target, name)?.baseline;
  return <>
    {query.error ? <ErrorState error={query.error} title={document ? '文件刷新失败，草稿保持不变' : '无法读取配置文件'} onRetry={() => void query.refetch()} /> : null}
    {document ? <ConfigEditor target={target} document={document} forms={forms} writable={writable && !query.error && Boolean(query.data)} maxBytes={maxBytes} refreshing={query.isFetching} refresh={async () => (await query.refetch({ throwOnError: true })).data} onDirtyChange={onDirtyChange} onBusyChange={onBusyChange} onRestartRequested={onRestartRequested} /> : query.isPending ? <LoadingState label="正在读取配置内容…" /> : null}
  </>;
}

interface EditorProps extends Props { document: ConfigDocument; refreshing: boolean; refresh: () => Promise<ConfigDocument | undefined> }

function ConfigEditor({ target, document, writable, maxBytes, forms, refreshing, refresh, onDirtyChange, onBusyChange, onRestartRequested }: EditorProps) {
  const client = useQueryClient();
  // The editable baseline and its CAS revision are one snapshot. New query
  // data is comparison-only until the operator explicitly reloads or saves.
  const [recovered] = useState(() => readConfigurationDraft(client, target, document.name));
  const [baseline, setBaseline] = useState(recovered?.baseline ?? document);
  const [draft, setDraft] = useState(recovered?.content ?? document.content);
  const [validated, setValidated] = useState<{ content: string; result: ConfigValidation }>();
  const [backupId, setBackupId] = useState('');
  const [loadedBackup, setLoadedBackup] = useState(recovered?.loadedBackup ?? '');
  const [loadingBackup, setLoadingBackup] = useState(false);
  const [reloading, setReloading] = useState(false);
  const [discarding, setDiscarding] = useState(false);
  const [localError, setLocalError] = useState('');
  const [message, setMessage] = useState('');
  const [parameterDirty, setParameterDirty] = useState(false);
  const [parameterEdits, setParameterEdits] = useState<Record<string, string>>(() => recovered?.parameterEdits ?? {});
  const [parameterBusy, setParameterBusy] = useState(false);
  const [importing, setImporting] = useState(false);
  const [validationStage, setValidationStage] = useState<'prepare' | 'validate' | ''>('');
  const [replacement, setReplacement] = useState<{ content: string; backup: string }>();
  const parameters = useRef<ConfigParametersHandle>(null);
  const validationInFlight = useRef(false);
  const alive = useRef(true);
  const validate = useValidateOperationConfig(target, baseline.name);
  const save = useSaveOperationConfig(target, baseline.name);
  const dirty = draft !== baseline.content || parameterDirty;
  const busy = save.isPending || validate.isPending || loadingBackup || reloading || parameterBusy || importing || Boolean(validationStage);
  const limit = Math.max(1, Math.min(MAX_CONFIG_BYTES, maxBytes || MAX_CONFIG_BYTES));
  const stale = document.revision !== baseline.revision;
  const targetChanged = baseline.source_id !== target.id || baseline.source_revision !== target.revision || document.source_id !== target.id || document.source_revision !== target.revision;
  const validation = validated?.content === draft ? validated.result : undefined;
  const canSave = dirty && !parameterDirty && validation?.valid && !busy && !refreshing && !stale && !targetChanged && writable;

  useEffect(() => { alive.current = true; return () => { alive.current = false; }; }, []);
  useEffect(() => { onDirtyChange(dirty); return () => onDirtyChange(false); }, [dirty, onDirtyChange]);
  useEffect(() => { onBusyChange(busy); return () => onBusyChange(false); }, [busy, onBusyChange]);
  useEffect(() => {
    if (draft !== baseline.content || Object.keys(parameterEdits).length) {
      rememberConfigurationDraft(client, target, { baseline, content: draft, loadedBackup, parameterEdits });
    } else forgetConfigurationDraft(client, target, baseline.name);
  }, [client, target.id, target.revision, baseline, draft, loadedBackup, parameterEdits]);

  const edit = (content: string) => {
    if (new TextEncoder().encode(content).byteLength > limit) { setLocalError(`内容超过 ${byteSize(limit)}，未载入超限内容，原草稿已保留。`); return; }
    setDraft(content); setValidated(undefined); setLocalError(''); setMessage(''); setDiscarding(false); setReplacement(undefined); save.reset(); validate.reset();
  };
  const replaceDraft = (content: string, backup = '') => {
    if (dirty) { setReplacement({ content, backup }); return; }
    edit(content); setLoadedBackup(backup);
  };
  const runValidation = async () => {
    if (busy || validationInFlight.current || !writable || targetChanged) return;
    validationInFlight.current = true;
    setLocalError('');
    try {
      let content = draft;
      if (parameterDirty) {
        if (!parameters.current) { setLocalError('参数表单尚未就绪，修改已保留。'); return; }
        setValidationStage('prepare');
        content = await parameters.current.prepare();
      }
      if (!alive.current) return;
      setValidationStage('validate');
      const result = await validate.mutateAsync(content);
      if (alive.current) setValidated({ content, result });
    } catch (error) { if (alive.current) setLocalError(`校验未完成，未保存配置。${apiErrorMessage(error)}`); }
    finally { validationInFlight.current = false; if (alive.current) setValidationStage(''); }
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
    if (!backupId || busy || parameterDirty) return;
    setLoadingBackup(true); setLocalError(''); setMessage('');
    try {
      const backup = await client.fetchQuery({ queryKey: operationKeys.config(target, baseline.name, backupId), queryFn: ({ signal }) => operationsApi.config(target.id, baseline.name, backupId, signal, target.revision), retry: false, staleTime: 0, gcTime: 0 });
      if (!alive.current) return;
      if (backup.source_id !== target.id || backup.source_revision !== target.revision || backup.name !== baseline.name) throw new Error('备份不属于当前来源版本或配置文件');
      if (new TextEncoder().encode(backup.content).byteLength > limit) throw new Error('备份超过允许的配置大小，未覆盖草稿');
      // Never replace the current file's revision with the backup hash. Loading
      // a backup is only an edit; the normal validate/save path performs CAS.
      replaceDraft(backup.content, backup.viewing_backup_id || backupId);
    } catch (error) { if (alive.current) setLocalError(`备份载入失败，草稿已保留。${apiErrorMessage(error)}`); }
    finally { if (alive.current) setLoadingBackup(false); }
  };
  const reloadCurrent = async () => {
    setReloading(true); setLocalError('');
    try {
      const latest = await refresh();
      if (!alive.current || !latest) return;
      if (latest.source_id !== target.id || latest.source_revision !== target.revision || latest.name !== baseline.name) throw new Error('文件不属于当前来源版本，未放弃草稿。');
      parameters.current?.discard(); setParameterEdits({}); setParameterDirty(false);
      setBaseline(latest); setDraft(latest.content); setLoadedBackup(''); setValidated(undefined); setDiscarding(false); setMessage('已明确放弃原草稿，并载入下级当前文件。'); save.reset(); validate.reset();
    } catch (error) { if (alive.current) setLocalError(`重新读取失败，草稿未丢弃。${apiErrorMessage(error)}`); }
    finally { if (alive.current) setReloading(false); }
  };

  return <section className="operations-editor" aria-label={`配置编辑 ${baseline.name}`}>
    {recovered ? <InlineNotice>已恢复本次网页登录期间离开页面前的未保存草稿和原版本；未自动保存或重新校验。刷新整个网页或退出登录不会保留此内存副本。</InlineNotice> : null}
    <div className="config-workflow"><ol aria-label="配置更新步骤"><li>1 修改</li><li>2 校验</li><li>3 保存</li><li>4 手动重启</li></ol><p>当前编辑：<strong>{baseline.name}</strong> · {dirty ? '有未保存更改' : document.restart_required ? '已保存，等待重启应用' : '未修改'}</p></div>
    <div className="config-primary-actions"><div><strong>{validationStage === 'prepare' ? '正在合并参数修改…' : validationStage === 'validate' ? '正在校验配置…' : validation ? validation.valid ? validation.warnings.length ? '可以保存，有点表或配置提示' : '校验通过，可以保存' : '配置有错误，暂不能保存' : dirty ? '修改完成后，请先校验' : '可修改参数，也可直接检查配置'}</strong><small>校验不写文件；只有“保存到下级”会写入。</small></div><div className="operations-editor-actions"><button className="button button-secondary" type="button" disabled={busy || !writable || targetChanged} onClick={() => void runValidation()}><ShieldCheck size={16} aria-hidden="true" />{validationStage ? '正在校验…' : '校验当前草稿'}</button><button className="button button-primary" type="button" disabled={!canSave} onClick={() => void saveDraft()}><Save size={16} aria-hidden="true" />{save.isPending ? '正在保存…' : '保存到下级'}</button></div></div>
    {localError ? <div className="top-error" role="alert">{localError}</div> : null}
    {validate.error ? <div className="top-error" role="alert">校验请求失败，未保存配置。{apiErrorMessage(validate.error)}</div> : null}
    {validation ? <div className={`operations-validation ${validation.valid ? validation.warnings.length ? 'is-warning' : 'is-valid' : 'is-invalid'}`} role={validation.valid ? 'status' : 'alert'}><strong>{validation.valid ? validation.warnings.length ? '配置可以保存，以下提示不阻止保存' : '当前草稿校验通过' : '当前草稿校验未通过'}</strong>{!validation.valid ? <p>以下错误阻止保存，请修正后重新校验。</p> : null}{validation.errors.map((error, index) => <p key={`${index}-${error}`}>{error}</p>)}{validation.warnings.length ? <details open><summary>点表及其它提示（{validation.warnings.length} 项，不阻止保存）</summary>{validation.warnings.map((warning, index) => <p key={`${index}-${warning}`}>{warning}</p>)}</details> : null}</div> : null}
    {save.error ? <div className="top-error" role="alert">{save.error instanceof ApiError && save.error.status === 409 && !operationBusy(save.error) ? '保存冲突：草稿及原版本已保留。已刷新下级文件，请核对最新版本后重新编辑。' : `${operationFailure(save.error)} 草稿已保留。`}</div> : null}
    {message ? <div className="success-banner" role="status">{message}</div> : null}
    {document.restart_required && onRestartRequested ? <InlineNotice tone="warning"><p>可以前往“进程”页显式重启。重启会应用此下级当前磁盘的全部已保存配置，并影响全部站点与客户端；保存按钮本身不会重启。</p><button className="button button-small button-secondary" type="button" disabled={dirty || busy || refreshing || stale || targetChanged || !writable} onClick={() => onRestartRequested({ name: document.name, revision: document.revision })}>前往进程页应用已保存配置</button></InlineNotice> : null}
    {stale ? <InlineNotice tone="warning">下级文件已被其他操作修改。草稿与原版本已保留；请下载草稿、核对最新文件，再明确放弃旧基线并重新编辑。</InlineNotice> : null}
    {targetChanged ? <InlineNotice tone="warning">来源配置版本已变化，不能向旧目标保存。请下载草稿后关闭抽屉并重新打开。</InlineNotice> : null}
    {loadedBackup ? <InlineNotice>已将备份 {loadedBackup} 载入草稿，尚未保存或应用。请校验并显式保存。</InlineNotice> : null}
	{!writable && baseline.name === 'calc.toml' ? <InlineNotice>只读回滚副本：当前下级为 manager 模式，请在计算中心维护活动规则。</InlineNotice> : null}
    {replacement ? <div className="operations-confirm-inline" role="alert"><p>当前有未保存更改。确认用{replacement.backup ? '所选备份' : '本地文件'}替换草稿？此步骤不会保存到下级。</p><div className="operations-editor-actions"><button className="button button-small button-secondary" type="button" disabled={busy || !writable || targetChanged} onClick={() => { edit(replacement.content); setLoadedBackup(replacement.backup); }}>确认替换草稿</button><button className="button button-small button-outline" type="button" onClick={() => setReplacement(undefined)}>取消载入</button></div></div> : null}
    {forms && baseline.name === 'config.toml' ? <ConfigParameters preparationRef={parameters} maxBytes={limit} target={target} content={draft} disabled={!writable || save.isPending || validate.isPending || loadingBackup || reloading || importing || stale || targetChanged} onApplied={edit} onDirtyChange={setParameterDirty} onBusyChange={setParameterBusy} initialEdits={parameterEdits} onEditsChange={setParameterEdits} /> : baseline.name === 'config.toml' ? <InlineNotice>此下级暂未提供参数表单。仍可在下方编辑配置原文；升级支持 configuration_forms 的下级后可使用中文参数设置。</InlineNotice> : null}
    <details open={!forms || baseline.name !== 'config.toml'} className="config-raw-editor"><summary>配置原文（高级编辑；点表 CSV 也在此编辑）</summary><label className="field operations-code-field"><span>配置内容 · {baseline.name}{dirty ? ' · 未保存' : ''}</span><textarea aria-label={`配置内容 ${baseline.name}`} spellCheck={false} value={draft} disabled={!writable || busy || parameterDirty} onChange={(event) => edit(event.target.value)} /></label><p className="operations-muted">UTF-8 文本，最多 {byteSize(limit)}。未保存的参数修改请先校验或撤销，再编辑原文。</p></details>
    <details className="config-secondary-tools"><summary>导入、下载与备份（可选）</summary><p className="operations-muted">这些操作不会直接写入下级。载入文件或备份将替换当前文件草稿，保存前仍须校验。</p>
    <label className="field"><span>从本地文件载入草稿（不自动保存）</span><input type="file" aria-label="从本地文件载入草稿" accept=".toml,.csv,.txt" disabled={!writable || busy || parameterDirty} onChange={async (event) => {
      const input = event.currentTarget; const file = input.files?.[0]; if (!file) return;
      setImporting(true);
      try {
        if (file.size > limit) throw new Error(`文件超过 ${byteSize(limit)}`);
        const content = new TextDecoder('utf-8', { fatal: true }).decode(await file.arrayBuffer());
        if (content.includes('\0')) throw new Error('配置不能包含 NUL');
        if (alive.current) replaceDraft(content);
      } catch (error) { if (alive.current) setLocalError(`载入失败，草稿已保留。${apiErrorMessage(error)}`); }
      finally { input.value = ''; if (alive.current) setImporting(false); }
    }} /></label>
    <div className="operations-editor-actions"><button className="button button-outline" type="button" disabled={parameterDirty} onClick={() => downloadText(`${target.id}-${baseline.name}.draft`, draft)}><Download size={15} aria-hidden="true" />下载草稿</button><button className="button button-outline" type="button" onClick={() => downloadText(`${target.id}-${document.name}`, document.content)}><Download size={15} aria-hidden="true" />下载已读取配置</button></div>
    <div className="operations-backups"><label className="field"><span>自动备份版本</span><select aria-label="自动备份版本" value={backupId} onChange={(event) => setBackupId(event.target.value)} disabled={busy || parameterDirty}><option value="">{document.backups.length ? '选择备份，只载入草稿' : '暂无自动备份'}</option>{document.backups.map((backup) => <option value={backup.id} key={backup.id}>{formatDateTime(backup.created_at)} · {backup.id} · {byteSize(backup.size_bytes)}</option>)}</select></label><button className="button button-small button-outline" type="button" disabled={!backupId || busy || parameterDirty || !writable} onClick={() => void loadBackup()}>{loadingBackup ? '正在读取备份…' : '载入备份为草稿'}</button></div>
    {document.backups_truncated ? <p className="operations-muted">仅列出最近 200 份自动备份，旧备份未删除。</p> : null}
    </details>
    <details className="config-version-tools"><summary>文件版本与放弃更改</summary><dl className="operations-file-meta"><div><dt>编辑基线版本</dt><dd><code>{baseline.revision}</code></dd></div><div><dt>运行中版本</dt><dd><code>{document.active_revision || '未提供'}</code></dd></div><div><dt>文件应用状态</dt><dd>{document.restart_required ? '已保存，待重启' : '文件版本与运行版本一致'}</dd></div></dl><div className="operations-editor-actions"><button className="button button-small button-secondary" type="button" disabled={busy || refreshing} onClick={() => void refresh().catch((error: unknown) => setLocalError(apiErrorMessage(error)))}><RefreshCw size={15} aria-hidden="true" />刷新文件版本（保留草稿）</button><button className="button button-small button-outline" type="button" disabled={busy || refreshing} onClick={() => setDiscarding(true)}>放弃草稿并载入最新</button></div></details>
    {discarding ? <div className="operations-confirm-inline" role="alert"><p>确认放弃当前草稿及参数修改？只有成功读取最新文件后才会替换编辑内容。</p><div className="operations-editor-actions"><button className="button button-danger button-small" type="button" disabled={busy} onClick={() => void reloadCurrent()}>确认放弃并载入最新</button><button className="button button-secondary button-small" type="button" disabled={busy} onClick={() => setDiscarding(false)}>取消放弃</button></div></div> : null}
  </section>;
}
