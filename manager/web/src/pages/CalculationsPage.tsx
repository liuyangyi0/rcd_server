import { lazy, Suspense, useMemo, useRef, useState, type ReactNode } from 'react';
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
import { InlineNotice } from '../components/InlineNotice';
import { EmptyState, ErrorState, LoadingState } from '../components/PageState';
import { PageHeader } from '../components/PageHeader';
import { Pagination } from '../components/Pagination';
import { formatDateTime, formatNumber } from '../lib/format';
import { statusCodeHex, statusCodeLabel } from '../lib/labels';

const CalculationEditor = lazy(() => import('../features/calculations/CalculationEditor'));
const PAGE_SIZE = 50;
type Tab = 'results' | 'config' | 'versions' | 'lowers';

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
    <InlineNotice tone="warning"><strong>计算结果固定按各逻辑组的默认来源生成。</strong><p>上级 OPC Session 单独切换 SelectedSourceId 只影响原始点，不会改变任何中央计算点；系统不自动故障转移。</p><p>采集通信中断时，原始值按类型归零后继续参与计算；计算点保留公式所得值，并标记 BadNoCommunication。</p></InlineNotice>
    <CalculationSummary status={status.data} loading={status.isPending} />
    <nav className="calculation-tabs" aria-label="计算中心功能">
      <TabButton active={tab === 'results'} onClick={() => setTab('results')} icon={<Sigma size={16} />}>实时结果</TabButton>
      <TabButton active={tab === 'config'} onClick={() => setTab('config')} icon={<FileUp size={16} />}>规则配置</TabButton>
      <TabButton active={tab === 'versions'} onClick={() => setTab('versions')} icon={<History size={16} />}>版本历史</TabButton>
      <TabButton active={tab === 'lowers'} onClick={() => setTab('lowers')} icon={<RefreshCw size={16} />}>下级模式</TabButton>
    </nav>
    {status.error ? <ErrorState error={status.error} onRetry={() => void status.refetch()} /> : null}
    {tab === 'results' ? <ResultsTab /> : null}
    {tab === 'config' ? <ConfigTab activeRevision={status.data?.activeRevision ?? 0} /> : null}
    {tab === 'versions' ? <VersionsTab activeRevision={status.data?.activeRevision ?? 0} /> : null}
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
    ['默认路由版本', loading ? '—' : String(status?.defaultRouteRevision ?? 0), '只在默认来源变化时重算'],
    ['最近求值', loading ? '—' : `${(status?.lastEvaluationDurationMs ?? 0).toFixed(2)} ms`, formatDateTime(status?.lastEvaluationAt)],
  ];
  return <section className="calculation-summary" aria-label="中央计算状态">{cards.map(([label, value, detail]) => <div key={label}><span>{label}</span><strong>{value}</strong><small>{detail}</small></div>)}{status?.pendingRestart ? <div className="calculation-summary-warning"><AlertTriangle size={17} /><span>数据类型变更待重启</span><strong>revision {status.pendingRevision}</strong><small>{shortHash(status.pendingHash)}</small></div> : null}</section>;
}

function ResultsTab() {
  const [params, setParams] = useSearchParams();
  const page = positive(params.get('page'));
  const query = params.get('q') ?? '';
  const status = params.get('quality') ?? '';
  const kind = params.get('type') ?? '';
  const [draftQuery, setDraftQuery] = useState(query);
  const request = useMemo<ListParams>(() => ({ page, pageSize: PAGE_SIZE, query, status, kind }), [kind, page, query, status]);
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
      <label className="field inline-field"><span>质量</span><select value={status} onChange={(event) => update({ quality: event.target.value, page: 1 })}><option value="">全部</option><option value="good">Good</option><option value="bad">非 Good</option></select></label>
      <button className="button button-secondary" type="button" onClick={() => void results.refetch()} disabled={results.isFetching}><RefreshCw className={results.isFetching ? 'spin' : undefined} size={16} />刷新</button>
      <a className="button button-outline" href={exportURL}><Download size={16} />导出 CSV</a>
    </form>
    <section className="table-panel content-panel">
      {results.error ? <ErrorState error={results.error} onRetry={() => void results.refetch()} /> : results.isPending && !results.data ? <LoadingState label="正在读取中央计算结果…" /> : !results.data?.items.length ? <EmptyState title="暂无中央计算结果" description="导入并激活 calc.toml 后，计算结果会显示在这里。" /> : <>
        <div className={`table-scroll${results.isFetching ? ' is-refreshing' : ''}`}><table className="data-table calculation-results-table"><thead><tr><th>KKS</th><th>表达式</th><th>当前值</th><th>类型</th><th>质量</th><th>求值时间</th><th>依赖</th><th>默认路由版本</th></tr></thead><tbody>{results.data.items.map((item) => <tr className="virtual-row" key={item.nodeId}><td><strong>{item.nodeId}</strong>{item.error ? <span className="sub-cell text-bad">{item.error}</span> : null}</td><td><code className="calculation-expression">{item.expression}</code></td><td><CalculationValue value={item.value} code={item.statusCode} /></td><td>{item.dataType}</td><td><Quality code={item.statusCode} /></td><td>{formatDateTime(item.evaluatedAt)}{item.sourceTimestamp ? <span className="sub-cell">源：{formatDateTime(item.sourceTimestamp)}</span> : null}</td><td>{item.dependencyCount}</td><td>{item.defaultRouteRevision}</td></tr>)}</tbody></table></div>
        <Pagination page={results.data.page} pageSize={results.data.pageSize} total={results.data.total} onPageChange={(next) => update({ page: next })} />
      </>}
    </section>
  </section>;
}

function ConfigTab({ activeRevision }: { activeRevision: number }) {
  const client = useQueryClient();
  const draftQuery = useCalculationDraft(true);
  const sources = useSources();
  const mutation = useCalculationMutation();
  const editor = useRef<CalculationEditorHandle>(null);
  const [editorGeneration, setEditorGeneration] = useState(0);
  const [replacement, setReplacement] = useState<string>();
  const [dirty, setDirty] = useState(false);
  const [validation, setValidation] = useState<CalculationValidation>();
  const [message, setMessage] = useState('');
  const [error, setError] = useState<unknown>();
  const [lowerSource, setLowerSource] = useState('');
  const draft = draftQuery.data;
  const content = replacement ?? draft?.content ?? '';
  const busy = mutation.isPending;
  const currentContent = () => editor.current?.getValue() ?? content;
  const replaceEditor = (value: string) => { setReplacement(value); setEditorGeneration((item) => item + 1); setDirty(true); setValidation(undefined); };
  const save = async () => {
    if (!draft) return;
    setError(undefined);
    try {
      const saved = await mutation.mutateAsync({ action: 'save', revision: draft.revision, content: currentContent() }) as CalculationDraft;
      client.setQueryData(calculationKeys.draft, saved);
      setReplacement(saved.content); setEditorGeneration((item) => item + 1); setDirty(false); setValidation(undefined); setMessage('草稿已保存；请校验后再激活，活动版本尚未改变。');
    } catch (reason) { setError(reason); }
  };
  const validate = async () => {
    setError(undefined);
    try { const report = await calculationApi.validate(currentContent()); setValidation(report); setMessage(report.valid ? `校验通过：${formatNumber(report.ruleCount)} 条规则` : `发现 ${formatNumber(report.problems.length)} 项问题`); } catch (reason) { setError(reason); }
  };
  const activate = async () => {
    if (!draft || dirty || validation?.valid !== true) return;
    setError(undefined);
    try { const version = await mutation.mutateAsync({ action: 'activate', revision: draft.revision }); setMessage((version as { state?: string }).state === 'pending_restart' ? '配置已保存为待重启版本；当前活动版本继续运行。' : '中央计算配置已热激活。'); } catch (reason) { setError(reason); }
  };
  const importLower = async () => {
    const source = sources.data?.find((item) => item.id === lowerSource);
    if (!draft || !source) return;
    setError(undefined);
    try { const saved = await mutation.mutateAsync({ action: 'import_lower', revision: draft.revision, sourceId: source.id, sourceRevision: source.revision }) as CalculationDraft; client.setQueryData(calculationKeys.draft, saved); setReplacement(saved.content); setEditorGeneration((item) => item + 1); setDirty(false); setValidation(undefined); setMessage(`已从 ${source.id} 导入 calc.toml 到草稿；请校验后再激活。`); } catch (reason) { setError(reason); }
  };
  return <section className="calculation-tab-panel calculation-config-panel">
    {draftQuery.error ? <ErrorState error={draftQuery.error} onRetry={() => void draftQuery.refetch()} /> : !draft ? <LoadingState label="正在载入计算草稿…" /> : <>
      <div className="calculation-config-toolbar">
        <label className="button button-outline calculation-file-button"><FileUp size={16} />导入本地 calc.toml<input type="file" accept=".toml,text/plain" disabled={busy} onChange={(event) => { const file = event.target.files?.[0]; if (!file) return; if (file.size > 4 * 1024 * 1024) { setError(new Error('文件超过 4 MiB')); return; } void file.text().then(replaceEditor).catch(setError); event.currentTarget.value = ''; }} /></label>
        <label className="field inline-field"><span>从下级导入</span><select value={lowerSource} disabled={busy} onChange={(event) => setLowerSource(event.target.value)}><option value="">选择来源</option>{(sources.data ?? []).map((source) => <option key={source.id} value={source.id}>{source.name || source.id}</option>)}</select></label>
        <button className="button button-outline" type="button" disabled={!lowerSource || busy} onClick={() => void importLower()}><Download size={16} />读取下级</button>
        <span className="calculation-config-meta">草稿 r{draft.revision} · {byteSize(currentContent().length)}{dirty ? ' · 未保存' : draft.valid ? ' · 已校验保存' : ' · 含校验问题'}</span>
      </div>
      {message ? <div className="success-banner" role="status">{message}<button type="button" onClick={() => setMessage('')}>关闭</button></div> : null}
      {error ? <div className="top-error" role="alert">{apiErrorMessage(error)}</div> : null}
      <div className="calculation-editor-layout">
        <Suspense fallback={<LoadingState label="正在加载大文件编辑器…" />}><CalculationEditor key={`${draft.revision}:${editorGeneration}`} ref={editor} defaultValue={content} disabled={busy} onDirty={() => { setDirty(true); setValidation(undefined); }} /></Suspense>
        <aside className="calculation-validation-panel"><h3>校验报告</h3>{!validation ? <p>保存草稿后可执行严格整版校验；未通过不会替换当前活动版本。</p> : validation.valid ? <div className="calculation-valid"><CheckCircle2 size={20} /><strong>校验通过</strong><span>{formatNumber(validation.ruleCount)} 条规则</span><code>{shortHash(validation.hash)}</code></div> : <ol>{validation.problems.slice(0, 200).map((problem, index) => <li key={`${problem.ruleIndex}:${problem.field}:${index}`}><button type="button" onClick={() => editor.current?.focusProblem(problem)}><strong>{problem.ruleIndex ? `规则 ${problem.ruleIndex}` : '配置'}</strong><span>{problem.message}</span></button></li>)}</ol>}{validation && validation.problems.length > 200 ? <p>仅显示前 200 项，请修复后重新校验。</p> : null}</aside>
      </div>
      <div className="calculation-config-actions"><button className="button button-secondary" type="button" disabled={busy || !dirty} onClick={() => void save()}><Save size={16} />保存草稿</button><button className="button button-outline" type="button" disabled={busy} onClick={() => void validate()}><CheckCircle2 size={16} />校验当前内容</button><button className="button button-primary" type="button" disabled={busy || dirty || validation?.valid !== true} title={dirty ? '先保存草稿并重新校验' : validation?.valid !== true ? '只有校验通过的草稿可以激活' : undefined} onClick={() => void activate()}><Play size={16} />激活中央版本</button>{activeRevision ? <button className="button button-danger" type="button" disabled={busy} onClick={() => { if (window.confirm('必须先确认所有下级已恢复 local 模式。确定停用中央计算吗？')) void mutation.mutateAsync({ action: 'deactivate', revision: activeRevision }).catch(setError); }}><Square size={15} />停用中央计算</button> : null}</div>
    </>}
  </section>;
}

function VersionsTab({ activeRevision }: { activeRevision: number }) {
  const [page, setPage] = useState(1);
  const versions = useCalculationVersions({ page, pageSize: 50 });
  const mutation = useCalculationMutation();
  const [confirmId, setConfirmId] = useState<number>();
  const [error, setError] = useState<unknown>();
  return <section className="calculation-tab-panel"><section className="table-panel content-panel">{versions.error ? <ErrorState error={versions.error} onRetry={() => void versions.refetch()} /> : versions.isPending ? <LoadingState /> : !versions.data?.items.length ? <EmptyState title="暂无计算版本" /> : <><div className="table-scroll"><table className="data-table"><thead><tr><th>版本</th><th>状态</th><th>规则 / 差异</th><th>Hash</th><th>操作者</th><th>创建 / 激活时间</th><th>操作</th></tr></thead><tbody>{versions.data.items.map((item) => <tr key={item.id}><td><strong>revision {item.revision}</strong></td><td>{versionState(item.state)}</td><td>{formatNumber(item.ruleCount)}<span className="sub-cell">+{item.addedCount} / −{item.removedCount} / 改 {item.changedCount}</span></td><td><code>{shortHash(item.hash)}</code></td><td>{item.actor || 'manager'}</td><td>{formatDateTime(item.createdAt)}{item.activatedAt ? <span className="sub-cell">激活：{formatDateTime(item.activatedAt)}</span> : null}</td><td>{confirmId === item.id ? <span className="inline-confirm"><button className="button button-small button-primary" type="button" disabled={mutation.isPending || !activeRevision} onClick={() => void mutation.mutateAsync({ action: 'restore', id: item.id, revision: activeRevision }).then(() => setConfirmId(undefined)).catch(setError)}>确认复制并激活</button><button className="button button-small button-secondary" type="button" onClick={() => setConfirmId(undefined)}>取消</button></span> : <button className="button button-small button-outline" type="button" disabled={item.state === 'active' || !activeRevision} onClick={() => setConfirmId(item.id)}><RotateCcw size={14} />回滚为新版本</button>}</td></tr>)}</tbody></table></div><Pagination page={versions.data.page} pageSize={versions.data.pageSize} total={versions.data.total} onPageChange={setPage} /></>}</section>{error ? <div className="top-error" role="alert">{apiErrorMessage(error)}</div> : null}</section>;
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
    setErrors((old) => ({ ...old, [source.id]: '' }));
    try {
      await operationsApi.calculationMode({ id: source.id, revision: source.revision }, config.revision, mode);
      setMessages((old) => ({ ...old, [source.id]: `已保存为 ${mode}，尚未重启，下级当前运行模式不变。` }));
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
  return <section className="calculation-tab-panel"><InlineNotice><strong>模式修改只保存 config.toml，不会自动重启。</strong><p>迁移时先激活中央配置，再把下级切到 manager 并逐台显式重启；回滚顺序相反。批量修改最多同时处理 3 个来源，每行单独显示结果。</p></InlineNotice><div className="toolbar"><button className="button button-secondary" type="button" disabled={!eligible.length} onClick={() => setSelected(allEligibleSelected ? new Set() : new Set(eligible.map((source) => source.id)))}>{allEligibleSelected ? '取消全选' : '选择全部可用下级'}</button><button className="button button-primary" type="button" disabled={!selected.size || anyPending} onClick={() => void batchMode('manager')}>批量设为 manager</button><button className="button button-outline" type="button" disabled={!selected.size || anyPending} onClick={() => void batchMode('local')}>批量设为 local</button><span className="toolbar-meta">已选 {selected.size} 个来源</span></div><section className="table-panel content-panel">{sources.isPending ? <LoadingState /> : sources.error ? <ErrorState error={sources.error} onRetry={() => void sources.refetch()} /> : !items.length ? <EmptyState title="暂无下级来源" /> : <div className="table-scroll"><table className="data-table"><thead><tr><th aria-label="选择"></th><th>下级</th><th>功能支持</th><th>运行 / 配置模式</th><th>规则</th><th>calc.toml Hash</th><th>状态</th><th>操作</th></tr></thead><tbody>{items.map((source, index) => {
    const capabilities = queries[index * 3]?.data as OperationsCapabilities | undefined;
    const snapshot = queries[index * 3 + 1]?.data as AcquisitionSnapshot | undefined;
    const config = queries[index * 3 + 2]?.data as ConfigDocument | undefined;
    const supported = capabilities?.features.includes('calculation_mode') ?? false;
    const calculation = snapshot?.calculation;
    const hashMatches = Boolean(centralHash && calculation?.config_hash && centralHash === calculation.config_hash);
    const selectable = supported && Boolean(config);
    return <tr key={source.id}><td><input type="checkbox" aria-label={`选择 ${source.name || source.id}`} disabled={!selectable || pending[source.id]} checked={selected.has(source.id)} onChange={(event) => setSelected((old) => { const next = new Set(old); if (event.target.checked) next.add(source.id); else next.delete(source.id); return next; })} /></td><td><strong>{source.name || source.id}</strong><span className="sub-cell">{source.id}</span></td><td>{!capabilities ? '读取中 / 不可用' : supported ? <span className="text-good">支持 calculation_mode</span> : <span className="text-warning">旧下级不支持</span>}</td><td>{calculation ? <><strong>{calculation.active_mode}</strong><span className="sub-cell">配置：{calculation.configured_mode}{calculation.active_mode !== calculation.configured_mode ? ' · 待重启' : ''}</span></> : '未知'}</td><td>{calculation ? formatNumber(calculation.rule_count) : '—'}</td><td><code>{shortHash(calculation?.config_hash)}</code>{centralHash ? <span className={`sub-cell ${hashMatches ? 'text-good' : 'text-warning'}`}>{hashMatches ? '与中央一致' : '与中央不同或未知'}</span> : null}</td><td>{messages[source.id] ? <span className="text-good">{messages[source.id]}</span> : errors[source.id] ? <span className="text-bad">{errors[source.id]}</span> : calculation?.config_error ? <span className="text-bad">{calculation.config_error}</span> : pending[source.id] ? '正在保存…' : source.connectionState}</td><td><div className="table-actions"><button className="button button-small button-primary" type="button" disabled={!supported || !config || pending[source.id] || calculation?.configured_mode === 'manager'} onClick={() => config && void saveMode(source, config, 'manager')}>设为 manager</button><button className="button button-small button-outline" type="button" disabled={!supported || !config || pending[source.id] || calculation?.configured_mode === 'local'} onClick={() => config && void saveMode(source, config, 'local')}>设为 local</button><Link className="button button-small button-secondary" to="/sources">前往运维重启</Link></div></td></tr>;
  })}</tbody></table></div>}</section></section>;
}

function Quality({ code }: { code: number }) {
  const severity = ((code >>> 0) & 0xc0000000) >>> 0;
  const encoded = code === 0 ? 'Good' : statusCodeHex(code);
  return <span className={severity === 0 ? 'text-good' : severity === 0x40000000 ? 'text-warning' : 'text-bad'}>{statusCodeLabel(encoded)}</span>;
}

function CalculationValue({ value, code }: { value: unknown; code: number }) {
  const bad = (((code >>> 0) & 0xc0000000) >>> 0) === 0x80000000;
  const baseCode = (((code >>> 0) & 0xffff0000) >>> 0);
  const detail = baseCode === 0x80310000 ? '通信中断，输入按 0 计算' : bad ? '结果质量异常' : '';
  return <>{formatValue(value)}{detail ? <span className="sub-cell text-bad">{detail}</span> : null}</>;
}

function parseTab(value: string | null): Tab { return value === 'config' || value === 'versions' || value === 'lowers' ? value : 'results'; }
function positive(value: string | null): number { const parsed = Number(value); return Number.isInteger(parsed) && parsed > 0 ? parsed : 1; }
function shortHash(value?: string): string { return value ? value.slice(0, 12) : '—'; }
function formatValue(value: unknown): string { return value === undefined || value === null ? '—' : typeof value === 'object' ? JSON.stringify(value) : String(value); }
function byteSize(value: number): string { return value >= 1024 * 1024 ? `${(value / 1024 / 1024).toFixed(2)} MiB` : `${Math.ceil(value / 1024)} KiB`; }
function versionState(value: string): string { return value === 'active' ? '活动' : value === 'pending_restart' ? '待重启' : '历史'; }
