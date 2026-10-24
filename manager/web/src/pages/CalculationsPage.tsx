import { lazy, Suspense, useEffect, useMemo, useRef, useState, type ReactNode } from 'react';
import { useQueries, useQueryClient } from '@tanstack/react-query';
import { AlertTriangle, CheckCircle2, Download, FileUp, History, Play, RefreshCw, RotateCcw, Save, Search, Sigma, Square } from 'lucide-react';
import { Link, useSearchParams } from 'react-router-dom';
import { API_BASE, apiErrorMessage, queryString } from '../api/client';
import {
  calculationApi,
  calculationKeys,
  useCalculationDraft,
  useCalculationMutation,
  useCalculationResults,
  useCalculationStatus,
  useCalculationVersions,
} from '../api/calculations';
import { operationsApi, operationKeys, type AcquisitionSnapshot, type ConfigDocument, type OperationsCapabilities } from '../api/operations';
import { useSources } from '../api/queries';
import type { CalculationDraft, CalculationValidation, ListParams, Source } from '../api/types';
import type { CalculationEditorHandle } from '../features/calculations/CalculationEditor';
import StateDependencies from '../features/calculations/StateDependencies';
import { isValidStateInputName } from '../features/calculations/stateInputNames';
import { InlineNotice } from '../components/InlineNotice';
import { EmptyState, ErrorState, LoadingState } from '../components/PageState';
import { PageHeader } from '../components/PageHeader';
import { Pagination, readPageSize } from '../components/Pagination';
import { formatDateTime, formatNumber } from '../lib/format';
import { stateLabel, statusCodeHex, statusCodeLabel } from '../lib/labels';

const CalculationEditor = lazy(() => import('../features/calculations/CalculationEditor'));
const StateInputsTab = lazy(() => import('../features/calculations/StateInputsTab'));
// Keep unsaved text across route/tab navigation in this browser instance only.
let pendingCalculationEdit: { draft: CalculationDraft; content: string } | undefined;
function clearPendingCalculationEdit(expected: typeof pendingCalculationEdit) {
  if (pendingCalculationEdit === expected) pendingCalculationEdit = undefined;
}
window.addEventListener('rcd-auth-cleared', () => { pendingCalculationEdit = undefined; });
window.addEventListener('beforeunload', (event) => { if (pendingCalculationEdit) { event.preventDefault(); event.returnValue = ''; } });
type Tab = 'results' | 'config' | 'versions' | 'lowers' | 'state-inputs';

export default function CalculationsPage() {
  const [params, setParams] = useSearchParams();
  const tab = parseTab(params.get('tab'));
  const status = useCalculationStatus();
  const setTab = (next: Tab) => {
    const updated = new URLSearchParams(params);
    if (next === 'results') updated.delete('tab'); else updated.set('tab', next);
    updated.delete('page');
    setParams(updated);
  };

  return <div className="page calculation-page">
    <PageHeader title="计算中心" description="集中管理 calc.toml、版本与实时计算结果；原始串口和 CSV 点表仍由各下级维护。" />
    <InlineNotice><strong>原始采集输入按组默认来源取数；状态输入按各自配置取数。</strong><p>所有计算结果均不随上级 OPC Session 单独选源变化。原始采集通信中断时按零参与计算；状态输入保留 0～5 状态码，离线为 5，不清零。</p><details className="calculation-routing-details"><summary>查看路由与质量规则</summary><p>中央计算不随单个 Session 切换，也不自动故障转移。状态输入可固定某台下级或跟随组默认来源；Good 表示状态判断有效，不表示设备正常。</p></details></InlineNotice>
    <CalculationSummary status={status.data} loading={status.isPending} />
    <nav className="calculation-tabs" aria-label="计算中心功能">
      <TabButton active={tab === 'results'} onClick={() => setTab('results')} icon={<Sigma size={16} />}>实时结果</TabButton>
      <TabButton active={tab === 'config'} onClick={() => setTab('config')} icon={<FileUp size={16} />}>规则配置</TabButton>
      <TabButton active={tab === 'state-inputs'} onClick={() => setTab('state-inputs')} icon={<Sigma size={16} />}>状态输入</TabButton>
      <TabButton active={tab === 'versions'} onClick={() => setTab('versions')} icon={<History size={16} />}>版本历史</TabButton>
      <TabButton active={tab === 'lowers'} onClick={() => setTab('lowers')} icon={<RefreshCw size={16} />}>下级模式</TabButton>
    </nav>
    {status.error ? <ErrorState error={status.error} onRetry={() => void status.refetch()} /> : null}
    {tab === 'results' ? <ResultsTab /> : null}
    {tab === 'config' ? <ConfigTab activeRevision={status.data?.activeRevision ?? 0} /> : null}
    {tab === 'state-inputs' ? <Suspense fallback={<LoadingState />}><StateInputsTab /></Suspense> : null}
    {tab === 'versions' ? <VersionsTab activeRevision={status.isSuccess ? status.data.activeRevision : undefined} /> : null}
    {tab === 'lowers' ? <LowerModesTab centralHash={status.data?.configHash ?? ''} /> : null}
  </div>;
}

function TabButton({ active, icon, children, onClick }: { active: boolean; icon: ReactNode; children: ReactNode; onClick(): void }) {
  return <button className={active ? 'active' : ''} type="button" aria-current={active ? 'page' : undefined} onClick={onClick}>{icon}{children}</button>;
}

function CalculationSummary({ status, loading }: { status?: ReturnType<typeof useCalculationStatus>['data']; loading: boolean }) {
  const cards = [
    ['活动规则', loading ? '—' : formatNumber(status?.ruleCount ?? 0), status?.activeRevision ? `revision ${status.activeRevision}` : '旧版下级转发模式'],
    ['结果质量', loading ? '—' : `${formatNumber(status?.goodCount ?? 0)} / ${formatNumber(status?.ruleCount ?? 0)}`, `${formatNumber(status?.badCount ?? 0)} 个非 Good`],
    ['默认路由版本', loading ? '—' : String(status?.defaultRouteRevision ?? 0), '默认源切换会更新；输入变化也会重算'],
    ['最后一批求值耗时', loading ? '—' : `${(status?.lastEvaluationDurationMs ?? 0).toFixed(2)} ms`, `执行于 ${formatDateTime(status?.lastEvaluationAt)}`],
  ];
  return <section className="calculation-summary" aria-label="中央计算状态">{cards.map(([label, value, detail]) => <div key={label}><span>{label}</span><strong>{value}</strong><small>{detail}</small></div>)}{status?.pendingRestart ? <div className="calculation-summary-warning"><AlertTriangle size={17} /><span>数据类型变更待重启</span><strong>revision {status.pendingRevision}</strong><small>{shortHash(status.pendingHash)}</small></div> : null}</section>;
}

function ResultsTab() {
  const [params, setParams] = useSearchParams();
  const PAGE_SIZE = readPageSize(params);
  const page = positive(params.get('page'));
  const query = params.get('q') ?? '';
  const status = params.get('quality') ?? '';
  const kind = params.get('type') ?? '';
  const [draftQuery, setDraftQuery] = useState(query);
  useEffect(() => setDraftQuery(query), [query]);
  const request = useMemo<ListParams>(() => ({ page, pageSize: PAGE_SIZE, query, status, kind }), [PAGE_SIZE, kind, page, query, status]);
  const results = useCalculationResults(request);
  const update = (values: Record<string, string | number>) => {
    const next = new URLSearchParams(params);
    for (const [key, value] of Object.entries(values)) {
      if (!value || (key === 'page' && value === 1)) next.delete(key); else next.set(key, String(value));
    }
    setParams(next);
  };
  const exportURL = `${API_BASE}/calculations/results/export${queryString({ query, status, kind })}`;
  return <section className="calculation-tab-panel">
    <form className="toolbar filter-toolbar" onSubmit={(event) => { event.preventDefault(); update({ q: draftQuery.trim(), page: 1 }); }}>
      <label className="search-field"><Search size={17} /><input value={draftQuery} onChange={(event) => setDraftQuery(event.target.value)} placeholder="搜索 KKS、表达式或错误" /><button type="submit">搜索</button></label>
      <label className="field inline-field"><span>数据类型</span><select value={kind} onChange={(event) => update({ type: event.target.value, page: 1 })}><option value="">全部</option><option>Boolean</option><option>UInt32</option><option>Float</option><option>Double</option></select></label>
      <label className="field inline-field"><span>质量</span><select value={status} onChange={(event) => update({ quality: event.target.value, page: 1 })}><option value="">全部</option><option value="good">正常（Good）</option><option value="bad">异常 / 不确定</option></select></label>
      <button className="button button-secondary" type="button" onClick={() => void results.refetch()} disabled={results.isFetching}><RefreshCw className={results.isFetching ? 'spin' : undefined} size={16} />刷新</button>
      <button className="button button-outline" type="button" disabled={!query && !status && !kind} onClick={() => { setDraftQuery(''); update({ q: '', quality: '', type: '', page: 1 }); }}>清空筛选</button>
      <a className="button button-outline" href={exportURL}><Download size={16} />导出筛选结果</a>
    </form>
    <section className="table-panel content-panel">
      {results.error ? <ErrorState error={results.error} onRetry={() => void results.refetch()} /> : results.isPending && !results.data ? <LoadingState label="正在读取中央计算结果…" /> : !results.data?.items.length ? <EmptyState title={query || status || kind ? "没有匹配的计算结果" : "暂无中央计算结果"} description={query || status || kind ? "请清空筛选或调整搜索条件；这不表示中央计算已停用。" : "导入并激活 calc.toml 后，计算结果会显示在这里。"} /> : <>
        <div className={`table-scroll${results.isFetching ? ' is-refreshing' : ''}`}><table className="data-table responsive-table calculation-results-table"><thead><tr><th>KKS</th><th>表达式</th><th>当前值</th><th>类型</th><th>质量</th><th>求值时间</th><th>依赖</th><th>默认路由版本</th></tr></thead><tbody>{results.data.items.map((item) => <tr className="virtual-row" key={item.nodeId}><td><strong>{item.nodeId}</strong>{item.error ? <span className="sub-cell text-bad">{item.error}</span> : null}</td><td><details className="calculation-expression-details"><summary><code>{item.expression}</code></summary><code>{item.expression}</code></details></td><td><CalculationValue value={item.value} code={item.statusCode} communicationInputs={item.communicationInputs} /></td><td>{item.dataType}</td><td><Quality code={item.statusCode} /></td><td>{formatDateTime(item.evaluatedAt)}{item.sourceTimestamp ? <span className="sub-cell">源：{formatDateTime(item.sourceTimestamp)}</span> : null}</td><td><StateDependencies node={item.nodeId} count={item.dependencyCount} /></td><td>{item.defaultRouteRevision}</td></tr>)}</tbody></table></div>
        <Pagination onPageSizeChange={(size) => update({ size, page: 1 })} page={results.data.page} pageSize={results.data.pageSize} total={results.data.total} onPageChange={(next) => update({ page: next })} />
      </>}
      {!results.error && results.data && !results.data.items.length ? <Pagination onPageSizeChange={(size) => update({ size, page: 1 })} page={page} pageSize={PAGE_SIZE} total={results.data.total} onPageChange={(next) => update({ page: next })} /> : null}
    </section>
  </section>;
}

function ConfigTab({ activeRevision }: { activeRevision: number }) {
  const [insertParams] = useSearchParams();
  const insertName = insertParams.get('insert') ?? '';
  const client = useQueryClient();
  const draftQuery = useCalculationDraft(true);
  const sources = useSources();
  const mutation = useCalculationMutation();
  const editor = useRef<CalculationEditorHandle>(null);
  const alive = useRef(true);
  const editGeneration = useRef(0);
  const [editorGeneration, setEditorGeneration] = useState(0);
  const [replacement, setReplacement] = useState<string | undefined>(pendingCalculationEdit?.content);
  const [baseDraft, setBaseDraft] = useState<CalculationDraft | undefined>(pendingCalculationEdit?.draft);
  const [localBusy, setLocalBusy] = useState(false);
  const [dirty, setDirty] = useState(Boolean(pendingCalculationEdit));
  const [validation, setValidation] = useState<CalculationValidation>();
  const [message, setMessage] = useState('');
  const [error, setError] = useState<unknown>();
  const [lowerSource, setLowerSource] = useState('');
  const draft = baseDraft ?? draftQuery.data;
  const content = replacement ?? draft?.content ?? '';
  const busy = mutation.isPending || localBusy;
  const remoteChanged = Boolean(baseDraft && draftQuery.data && baseDraft.revision !== draftQuery.data.revision);
  useEffect(() => { alive.current = true; return () => { alive.current = false; }; }, []);
  useEffect(() => {
    if (!dirty && !busy && draftQuery.data && draftQuery.data.revision !== baseDraft?.revision) {
      setBaseDraft(draftQuery.data); setReplacement(undefined); setValidation(undefined);
    }
  }, [draftQuery.data, baseDraft?.revision, dirty, busy]);
  useEffect(() => {
    if (!dirty) return;
    const warn = (event: BeforeUnloadEvent) => { event.preventDefault(); event.returnValue = ''; };
    window.addEventListener('beforeunload', warn);
    return () => window.removeEventListener('beforeunload', warn);
  }, [dirty]);
  const currentContent = () => editor.current?.getValue() ?? content;
  const rememberEdit = (value: string) => { if (draft && alive.current) { editGeneration.current += 1; setBaseDraft(draft); pendingCalculationEdit = { draft, content: value }; } };
  const replaceEditor = (value: string) => { rememberEdit(value); setReplacement(value); setEditorGeneration((item) => item + 1); setDirty(true); setValidation(undefined); };
  const acceptSavedDraft = (saved: CalculationDraft, expectedEdit: typeof pendingCalculationEdit, generation: number, feedback: string) => {
    client.setQueryData<CalculationDraft>(calculationKeys.draft, (current) => current && current.revision > saved.revision ? current : saved);
    clearPendingCalculationEdit(expectedEdit);
    if (!alive.current || editGeneration.current !== generation) return;
    setBaseDraft(saved); setReplacement(saved.content); setEditorGeneration((item) => item + 1); setDirty(false); setValidation(undefined); setMessage(feedback);
  };
  const save = async () => {
    if (!draft) return;
    setError(undefined);
    const expectedEdit = pendingCalculationEdit;
    const generation = editGeneration.current;
    try {
      const saved = await mutation.mutateAsync({ action: 'save', revision: draft.revision, content: currentContent() }) as CalculationDraft;
      acceptSavedDraft(saved, expectedEdit, generation, '草稿已保存；请校验后再激活，活动版本尚未改变。');
    } catch (reason) { if (alive.current) setError(reason); }
  };
  const validate = async () => {
    setError(undefined);
    setLocalBusy(true);
    try { const report = await calculationApi.validate(currentContent()); setValidation(report); setMessage(report.valid ? `校验通过：${formatNumber(report.ruleCount)} 条规则` : `发现 ${formatNumber(report.problems.length)} 项问题`); } catch (reason) { setError(reason); } finally { setLocalBusy(false); }
  };
  const activate = async () => {
    if (!draft || dirty || validation?.valid !== true) return;
    setError(undefined);
    try { const version = await mutation.mutateAsync({ action: 'activate', revision: draft.revision }); setMessage((version as { state?: string }).state === 'pending_restart' ? '配置已保存为待重启版本；当前活动版本继续运行。' : '中央计算配置已热激活。'); } catch (reason) { setError(reason); }
  };
  const importLower = async () => {
    const source = sources.data?.find((item) => item.id === lowerSource);
    if (!draft || !source) return;
    if (!window.confirm('将用下级 calc.toml 整份替换管理端草稿（包括已有状态输入映射），不会自动激活。请先导出备份，确定继续？')) return;
    setError(undefined);
    const expectedEdit = pendingCalculationEdit;
    const generation = editGeneration.current;
    try { const saved = await mutation.mutateAsync({ action: 'import_lower', revision: draft.revision, sourceId: source.id, sourceRevision: source.revision }) as CalculationDraft; acceptSavedDraft(saved, expectedEdit, generation, `已从 ${source.id} 导入 calc.toml 到草稿；请校验后再激活。`); } catch (reason) { if (alive.current) setError(reason); }
  };
  return <section className="calculation-tab-panel calculation-config-panel">
    {isValidStateInputName(insertName) ? <p>先把光标放到 expression 的引号内，再点击：<button className="button button-secondary" disabled={busy} onClick={() => editor.current?.insertText(insertName)}>插入 {insertName}</button></p> : null}
    {draftQuery.error ? <ErrorState error={draftQuery.error} onRetry={() => void draftQuery.refetch()} /> : null}
    {!draft ? <LoadingState label="正在载入计算草稿…" /> : <>
      <div className="calculation-config-toolbar">
        <label className="button button-outline calculation-file-button"><FileUp size={16} />导入本地 calc.toml<input type="file" accept=".toml,text/plain" disabled={busy} onChange={(event) => {
          const file = event.target.files?.[0]; event.currentTarget.value = ''; if (!file) return;
          if (file.size > 4 * 1024 * 1024) { setError(new Error('文件超过 4 MiB')); return; }
          if (!window.confirm('将用文件整份替换编辑区内容，不是追加规则。原活动版本不变；请先导出备份，确定继续？')) return;
          const generation = editGeneration.current;
          setLocalBusy(true); void file.text().then((value) => { if (alive.current && editGeneration.current === generation) replaceEditor(value); }).catch((reason) => { if (alive.current) setError(reason); }).finally(() => { if (alive.current) setLocalBusy(false); });
        }} /></label>
        <label className="field inline-field"><span>从下级导入</span><select value={lowerSource} disabled={busy} onChange={(event) => setLowerSource(event.target.value)}><option value="">选择来源</option>{(sources.data ?? []).map((source) => <option key={source.id} value={source.id}>{source.name || source.id}</option>)}</select></label>
        <button className="button button-outline" type="button" disabled={!lowerSource || busy} onClick={() => void importLower()}><Download size={16} />读取下级</button>
        <button className="button button-outline" type="button" onClick={() => downloadDraft(currentContent())}><Download size={16} />导出当前文本</button>
        <span className="calculation-config-meta">草稿 r{draft.revision} · {byteSize(new TextEncoder().encode(currentContent()).length)}{dirty ? ' · 未保存' : draft.valid ? ' · 已校验保存' : ' · 含校验问题'}</span>
      </div>
      {dirty ? <div className="workflow-note" role="status"><strong>编辑内容尚未保存</strong><span>切换页面后可返回继续编辑；刷新或关闭浏览器前请保存草稿或导出文本。</span></div> : null}
      {remoteChanged ? <div className="top-error" role="alert">服务器草稿已变为 r{draftQuery.data?.revision}，当前仍按 r{draft.revision} 编辑，不会覆盖你的输入。请先导出文本，再载入新草稿合并。</div> : null}
      <button className="button button-small button-outline" type="button" disabled={busy} onClick={async () => {
        if (dirty && !window.confirm('丢弃当前未保存编辑并载入服务器草稿？建议先导出当前文本。')) return;
        const expectedEdit = pendingCalculationEdit;
        const generation = editGeneration.current;
        setLocalBusy(true);
        try { const latest = await draftQuery.refetch(); if (latest.error) throw latest.error; if (latest.data) { acceptSavedDraft(latest.data, expectedEdit, generation, '已载入服务器草稿'); if (alive.current) setError(undefined); } } catch (reason) { if (alive.current) setError(reason); } finally { if (alive.current) setLocalBusy(false); }
      }}>重新载入服务器草稿</button>
      {message ? <div className="success-banner" role="status">{message}<button type="button" onClick={() => setMessage('')}>关闭</button></div> : null}
      {error ? <div className="top-error" role="alert">{apiErrorMessage(error)}</div> : null}
      <div className="calculation-editor-layout">
        <Suspense fallback={<LoadingState label="正在加载大文件编辑器…" />}><CalculationEditor key={`${draft.revision}:${editorGeneration}`} ref={editor} defaultValue={content} disabled={busy} onDirty={() => { rememberEdit(currentContent()); setDirty(true); setValidation(undefined); setMessage(''); }} /></Suspense>
        <aside className="calculation-validation-panel"><h3>校验报告</h3>{!validation ? <p>保存草稿后可执行严格整版校验；未通过不会替换当前活动版本。</p> : validation.valid ? <div className="calculation-valid"><CheckCircle2 size={20} /><strong>校验通过</strong><span>{formatNumber(validation.ruleCount)} 条规则</span><code>{shortHash(validation.hash)}</code></div> : <ol>{validation.problems.slice(0, 200).map((problem, index) => <li key={`${problem.ruleIndex}:${problem.field}:${index}`}><button type="button" onClick={() => editor.current?.focusProblem(problem)}><strong>{problem.ruleIndex ? `规则 ${problem.ruleIndex}` : '配置'}</strong><span>{problem.message}</span></button></li>)}</ol>}{validation && validation.problems.length > 200 ? <p>仅显示前 200 项，请修复后重新校验。</p> : null}</aside>
      </div>
      <div className="calculation-config-actions"><button className="button button-secondary" type="button" disabled={busy || !dirty} onClick={() => void save()}><Save size={16} />保存草稿</button><button className="button button-outline" type="button" disabled={busy} onClick={() => void validate()}><CheckCircle2 size={16} />校验当前内容</button><button className="button button-primary" type="button" disabled={busy || dirty || remoteChanged || validation?.valid !== true} title={dirty ? '先保存草稿并重新校验' : validation?.valid !== true ? '只有校验通过的草稿可以激活' : undefined} onClick={() => void activate()}><Play size={16} />激活中央版本</button>{activeRevision ? <button className="button button-danger" type="button" disabled={busy} onClick={() => { if (window.confirm('必须先确认所有下级已恢复 local 模式。确定停用中央计算吗？')) void mutation.mutateAsync({ action: 'deactivate', revision: activeRevision }).catch(setError); }}><Square size={15} />停用中央计算</button> : null}</div>
    </>}
  </section>;
}

function VersionsTab({ activeRevision }: { activeRevision?: number }) {
  const [page, setPage] = useState(1);
  const versions = useCalculationVersions({ page, pageSize: 50 });
  const mutation = useCalculationMutation();
  const [restoreTarget, setRestoreTarget] = useState<{ id: number; revision: number }>();
  const [error, setError] = useState<unknown>();
  const targetChanged = Boolean(restoreTarget && restoreTarget.revision !== activeRevision);
  return <section className="calculation-tab-panel">
    {activeRevision === undefined ? <p role="status">尚未确认当前活动版本，暂不能恢复历史版本。</p> : null}
    {targetChanged ? <p className="top-error" role="alert">活动版本已变化，请取消本次确认，重新核对后再恢复。</p> : null}
    <section className="table-panel content-panel">{versions.error ? <ErrorState error={versions.error} onRetry={() => void versions.refetch()} /> : versions.isPending ? <LoadingState /> : !versions.data?.items.length ? <EmptyState title="暂无计算版本" /> : <>
      <div className="table-scroll"><table className="data-table"><thead><tr><th>版本</th><th>状态</th><th>规则 / 差异</th><th>Hash</th><th>操作者</th><th>创建 / 激活时间</th><th>操作</th></tr></thead><tbody>{versions.data.items.map((item) => <tr key={item.id}>
        <td><strong>revision {item.revision}</strong></td><td>{versionState(item.state)}</td><td>{formatNumber(item.ruleCount)}<span className="sub-cell">+{item.addedCount} / −{item.removedCount} / 改 {item.changedCount}</span></td><td><code>{shortHash(item.hash)}</code></td><td>{item.actor || 'manager'}</td><td>{formatDateTime(item.createdAt)}{item.activatedAt ? <span className="sub-cell">激活：{formatDateTime(item.activatedAt)}</span> : null}</td>
        <td>{restoreTarget?.id === item.id ? <span className="inline-confirm"><button className="button button-small button-primary" type="button" disabled={mutation.isPending || activeRevision === undefined || targetChanged} onClick={() => void mutation.mutateAsync({ action: 'restore', id: restoreTarget.id, revision: restoreTarget.revision }).then(() => setRestoreTarget(undefined)).catch(setError)}>确认复制并激活</button><button className="button button-small button-secondary" type="button" disabled={mutation.isPending} onClick={() => setRestoreTarget(undefined)}>取消</button></span> : <button className="button button-small button-outline" type="button" disabled={item.state === 'active' || activeRevision === undefined || mutation.isPending} onClick={() => { if (activeRevision !== undefined) { setError(undefined); setRestoreTarget({ id: item.id, revision: activeRevision }); } }}><RotateCcw size={14} />{activeRevision === 0 ? '复制并激活历史版本' : '回滚为新版本'}</button>}</td>
      </tr>)}</tbody></table></div>
      <Pagination page={versions.data.page} pageSize={versions.data.pageSize} total={versions.data.total} onPageChange={setPage} />
    </>}</section>{error ? <div className="top-error" role="alert">{apiErrorMessage(error)}</div> : null}
  </section>;
}

function LowerModesTab({ centralHash }: { centralHash: string }) {
  const client = useQueryClient();
  const sources = useSources();
  const items = sources.data ?? [];
  const queries = useQueries({ queries: items.flatMap((source) => {
    const target = { id: source.id, revision: source.revision };
    return [
      { queryKey: operationKeys.capabilities(target), queryFn: ({ signal }: { signal: AbortSignal }) => operationsApi.capabilities(source.id, signal, source.revision), retry: false },
      { queryKey: operationKeys.status(target), queryFn: ({ signal }: { signal: AbortSignal }) => operationsApi.status(source.id, signal, source.revision), retry: false },
      { queryKey: operationKeys.config(target, 'config.toml'), queryFn: ({ signal }: { signal: AbortSignal }) => operationsApi.config(source.id, 'config.toml', '', signal, source.revision), retry: false },
    ];
  }) });
  const [pending, setPending] = useState<Record<string, boolean>>({});
  const [selected, setSelected] = useState<Set<string>>(() => new Set());
  const [messages, setMessages] = useState<Record<string, string>>({});
  const [errors, setErrors] = useState<Record<string, string>>({});
  const saveMode = async (source: Source, config: ConfigDocument, mode: 'local' | 'manager') => {
    setPending((old) => ({ ...old, [source.id]: true }));
    setMessages((old) => ({ ...old, [source.id]: '' }));
    setErrors((old) => ({ ...old, [source.id]: '' }));
    try {
      await operationsApi.calculationMode({ id: source.id, revision: source.revision }, config.revision, mode);
      setMessages((old) => ({ ...old, [source.id]: `已保存为${stateLabel(mode)}，尚未重启，下级当前运行模式不变。` }));
      await Promise.all([client.invalidateQueries({ queryKey: operationKeys.status({ id: source.id, revision: source.revision }) }), client.invalidateQueries({ queryKey: operationKeys.config({ id: source.id, revision: source.revision }, 'config.toml') })]);
      setSelected((old) => { const next = new Set(old); next.delete(source.id); return next; });
      return true;
    } catch (reason) {
      setErrors((old) => ({ ...old, [source.id]: apiErrorMessage(reason) }));
      return false;
    } finally {
      setPending((old) => { const next = { ...old }; delete next[source.id]; return next; });
    }
  };
  const eligible = items.filter((_, index) => {
    const capabilities = queries[index * 3]?.data as OperationsCapabilities | undefined;
    const config = queries[index * 3 + 2]?.data as ConfigDocument | undefined;
    return Boolean(capabilities?.features.includes('calculation_mode') && config);
  });
  const batchMode = async (mode: 'local' | 'manager') => {
    const targets = eligible.filter((source) => selected.has(source.id));
    for (let offset = 0; offset < targets.length; offset += 3) {
      await Promise.all(targets.slice(offset, offset + 3).map((source) => {
        const index = items.findIndex((item) => item.id === source.id);
        const config = queries[index * 3 + 2]?.data as ConfigDocument;
        return saveMode(source, config, mode);
      }));
    }
  };
  const allEligibleSelected = eligible.length > 0 && eligible.every((source) => selected.has(source.id));
  const anyPending = Object.keys(pending).length > 0;
  return <section className="calculation-tab-panel"><InlineNotice><strong>模式修改只保存 config.toml，不会自动重启。</strong><p>迁移时先激活中央配置，再把下级切到“管理端计算”并逐台显式重启；回滚顺序相反。批量修改最多同时处理 3 个来源，每行单独显示结果。</p></InlineNotice><div className="toolbar"><button className="button button-secondary" type="button" disabled={!eligible.length} onClick={() => setSelected(allEligibleSelected ? new Set() : new Set(eligible.map((source) => source.id)))}>{allEligibleSelected ? '取消全选' : '选择全部可用下级'}</button><button className="button button-primary" type="button" disabled={!selected.size || anyPending} onClick={() => void batchMode('manager')}>批量设为管理端计算</button><button className="button button-outline" type="button" disabled={!selected.size || anyPending} onClick={() => void batchMode('local')}>批量设为下级本地计算</button><span className="toolbar-meta">已选 {selected.size} 个来源</span></div><section className="table-panel content-panel">{sources.isPending ? <LoadingState /> : sources.error ? <ErrorState error={sources.error} onRetry={() => void sources.refetch()} /> : !items.length ? <EmptyState title="暂无下级来源" /> : <div className="table-scroll"><table className="data-table responsive-table"><thead><tr><th aria-label="选择"></th><th>下级</th><th>功能支持</th><th>运行 / 配置模式</th><th>规则</th><th>calc.toml Hash</th><th>状态</th><th>操作</th></tr></thead><tbody>{items.map((source, index) => {
    const capabilities = queries[index * 3]?.data as OperationsCapabilities | undefined;
    const snapshot = queries[index * 3 + 1]?.data as AcquisitionSnapshot | undefined;
    const config = queries[index * 3 + 2]?.data as ConfigDocument | undefined;
    const supported = capabilities?.features.includes('calculation_mode') ?? false;
    const calculation = snapshot?.calculation;
    const hashMatches = Boolean(centralHash && calculation?.config_hash && centralHash === calculation.config_hash);
    const selectable = supported && Boolean(config);
    return <tr key={source.id}><td><input type="checkbox" aria-label={`选择 ${source.name || source.id}`} disabled={!selectable || pending[source.id]} checked={selected.has(source.id)} onChange={(event) => setSelected((old) => { const next = new Set(old); if (event.target.checked) next.add(source.id); else next.delete(source.id); return next; })} /></td><td><strong>{source.name || source.id}</strong><span className="sub-cell">{source.id}</span></td><td>{!capabilities ? '读取中 / 不可用' : supported ? <span className="text-good">支持计算模式切换</span> : <span className="text-warning">旧下级不支持</span>}</td><td>{calculation ? <><strong>{stateLabel(calculation.active_mode)}</strong><span className="sub-cell">配置：{stateLabel(calculation.configured_mode)}{calculation.active_mode !== calculation.configured_mode ? ' · 待重启' : ''}</span></> : '未知'}</td><td>{calculation ? formatNumber(calculation.rule_count) : '—'}</td><td><code>{shortHash(calculation?.config_hash)}</code>{centralHash ? <span className={`sub-cell ${hashMatches ? 'text-good' : 'text-warning'}`}>{hashMatches ? '与中央一致' : '与中央不同或未知'}</span> : null}</td><td>{messages[source.id] ? <span className="text-good">{messages[source.id]}</span> : errors[source.id] ? <span className="text-bad">{errors[source.id]}</span> : calculation?.config_error ? <span className="text-bad">{calculation.config_error}</span> : pending[source.id] ? '正在保存…' : stateLabel(source.connectionState)}</td><td><div className="table-actions"><button className="button button-small button-primary" type="button" disabled={!supported || !config || pending[source.id] || calculation?.configured_mode === 'manager'} onClick={() => config && void saveMode(source, config, 'manager')}>设为管理端计算</button><button className="button button-small button-outline" type="button" disabled={!supported || !config || pending[source.id] || calculation?.configured_mode === 'local'} onClick={() => config && void saveMode(source, config, 'local')}>设为下级本地计算</button><Link className="button button-small button-secondary" to={`/sources?operations=${encodeURIComponent(source.id)}`}>前往运维重启</Link></div></td></tr>;
  })}</tbody></table></div>}</section></section>;
}

function Quality({ code }: { code: number }) {
  const severity = ((code >>> 0) & 0xc0000000) >>> 0;
  const encoded = code === 0 ? 'Good' : statusCodeHex(code);
  return <span className={severity === 0 ? 'text-good' : severity === 0x40000000 ? 'text-warning' : 'text-bad'}>{statusCodeLabel(encoded)}</span>;
}

function CalculationValue({ value, code, communicationInputs }: { value: unknown; code: number; communicationInputs: string[] }) {
  const bad = (((code >>> 0) & 0xc0000000) >>> 0) === 0x80000000;
  const baseCode = (((code >>> 0) & 0xffff0000) >>> 0);
  const detail = baseCode === 0x80310000 ? communicationDetail(communicationInputs) : bad ? '结果质量异常' : '';
  const title = communicationInputs.length ? `通信中断上游：${communicationInputs.join('、')}` : undefined;
  return <>{formatValue(value)}{detail ? <span className="sub-cell text-bad" title={title}>{detail}</span> : null}</>;
}

function communicationDetail(inputs: string[]): string {
  if (inputs.length === 1) return `上游点 ${inputs[0]} 通信中断，按 0 参与计算`;
  if (inputs.length > 1) return `上游点 ${inputs.slice(0, 2).join('、')}${inputs.length > 2 ? ` 等 ${inputs.length} 个` : ''}通信中断，按 0 参与计算`;
  return '通信中断，输入按 0 计算';
}

function parseTab(value: string | null): Tab { return value === 'config' || value === 'versions' || value === 'lowers' || value === 'state-inputs' ? value : 'results'; }
function positive(value: string | null): number { const parsed = Number(value); return Number.isInteger(parsed) && parsed > 0 ? parsed : 1; }
function downloadDraft(content: string) {
  const url = URL.createObjectURL(new Blob([content], { type: 'text/plain;charset=utf-8' }));
  const anchor = document.createElement('a'); anchor.href = url; anchor.download = 'calc-draft.toml';
  document.body.appendChild(anchor); anchor.click(); anchor.remove(); setTimeout(() => URL.revokeObjectURL(url), 1000);
}
function shortHash(value?: string): string { return value ? value.slice(0, 12) : '—'; }
function formatValue(value: unknown): string { return value === undefined || value === null ? '—' : typeof value === 'object' ? JSON.stringify(value) : String(value); }
function byteSize(value: number): string { return value >= 1024 * 1024 ? `${(value / 1024 / 1024).toFixed(2)} MiB` : `${Math.ceil(value / 1024)} KiB`; }
function versionState(value: string): string { return value === 'active' ? '活动' : value === 'pending_restart' ? '待重启' : '历史'; }
