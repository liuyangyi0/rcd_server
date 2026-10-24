import { useState } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { Link, useSearchParams } from 'react-router-dom';
import { request as rawRequest, apiErrorMessage } from '../../api/client';
import { calculationApi } from '../../api/calculations';
import { useGroups, useSources } from '../../api/queries';
import { EmptyState, ErrorState, LoadingState } from '../../components/PageState';
import { Pagination } from '../../components/Pagination';
import { copyText } from '../../lib/clipboard';
import { formatDateTime } from '../../lib/format';
import { isValidStateInputName } from './stateInputNames';

export type StateTarget = { source_id: string; port_id: string; device_id: string };
export type StateBinding = { name: string; mode: 'fixed' | 'group_default'; group_id?: string; targets: StateTarget[] };
type StateView = StateBinding & { value: number; state_text: string; status_code: number; error?: string; observed_at?: string; resolved_target?: StateTarget };
type Candidate = StateTarget & { source_name: string; group_id: string; group_name: string; device_name: string };
type Page<T> = { items: T[]; total: number; revision: number };
type Bundle = { revision: number; content: string; inputs: StateBinding[] };
const request = async <T,>(path: string, init?: RequestInit): Promise<T> => await rawRequest(path, init) as T;

export const copyStateName = copyText;

async function loadBundle(): Promise<Bundle> {
  const draft = await calculationApi.draft();
  const inputs: StateBinding[] = [];
  for (let offset = 0; ; offset += 500) {
    const page = await request<Page<StateView>>(`/calculations/state-inputs?view=draft&limit=500&offset=${offset}`);
    if (page.revision !== draft.revision) throw new Error('草稿已更新，请刷新后重试');
    inputs.push(...page.items.map(bindingOnly));
    if (inputs.length >= page.total) break;
    if (!page.items.length) throw new Error('草稿分页不完整，请刷新后重试');
  }
  return { revision: draft.revision, content: draft.content, inputs };
}

export function bindingOnly(value: StateBinding): StateBinding {
  return { name: value.name, mode: value.mode, ...(value.mode === 'group_default' ? { group_id: value.group_id } : {}), targets: value.targets.map(({ source_id, port_id, device_id }) => ({ source_id, port_id, device_id })) };
}

export default function StateInputsTab() {
  const client = useQueryClient();
  const [urlParams] = useSearchParams();
  const groups = useGroups();
  const sources = useSources();
  const [view, setView] = useState<'active' | 'draft'>('active');
  const [page, setPage] = useState(0);
  const [query, setQuery] = useState(urlParams.get('state_q') ?? '');
  const [search, setSearch] = useState(query);
  const [filterMode, setFilterMode] = useState('');
  const [filterSource, setFilterSource] = useState('');
  const [filterStatus, setFilterStatus] = useState('');
  const [edit, setEdit] = useState<StateBinding | null>(null);
  const [originalName, setOriginalName] = useState('');
  const [base, setBase] = useState<Bundle | null>(null);
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState('');
  const [error, setError] = useState('');
  const [normalized, setNormalized] = useState(false);
  const params = new URLSearchParams({ view, limit: '50', offset: String(page * 50), q: query, mode: filterMode, source_id: filterSource, status: filterStatus });
  const values = useQuery({ queryKey: ['calculations', 'state-inputs', params.toString()], queryFn: ({ signal }) => request<Page<StateView>>(`/calculations/state-inputs?${params}`, { signal }), refetchInterval: 5000 });
  const draftBundle = useQuery({ queryKey: ['calculations', 'state-draft'], queryFn: loadBundle, retry: false });
  const refresh = async () => { await client.invalidateQueries({ queryKey: ['calculations'] }); };
  const act = async (action: () => Promise<void>) => {
    setBusy(true); setError(''); setMessage('');
    try { await action(); } catch (reason) { setError(apiErrorMessage(reason)); } finally { setBusy(false); }
  };
  const open = (binding?: StateBinding) => {
    if (!draftBundle.data) return;
    const bundle = draftBundle.data;
    // A displayed row can still belong to the previous page response. Content
    // and the CAS revision must always come from the same complete snapshot.
    const current = binding ? bundle.inputs.find((input) => input.name === binding.name) : undefined;
    if (binding && !current) { setError('该状态输入已从草稿移除，请刷新列表后再编辑。'); return; }
    setBase(bundle); setOriginalName(current?.name ?? ''); setNormalized(false); setError('');
    setMessage(binding && values.data?.revision !== bundle.revision ? `列表与草稿版本不同，已载入草稿 r${bundle.revision} 中的最新映射，请核对后保存。` : '');
    setEdit(current ? bindingOnly(current) : { name: '', mode: 'group_default', group_id: groups.data?.[0]?.id ?? '', targets: [] });
  };
  const save = async (remove = false) => {
    if (remove && !window.confirm(`从草稿删除 ${originalName}？引用它的公式也需要修改。当前活动版本不受影响。`)) return;
    if (!base || !edit || !normalized) return;
    if (!remove && !isValidStateInputName(edit.name)) throw new Error('变量名最长128字符，只能使用英文字母、数字和下划线，不能是纯数字或IF/true/false');
    if (!remove && base.inputs.some((input) => input.name === edit.name && input.name !== originalName)) throw new Error('变量名已存在');
    const inputs = base.inputs.filter((input) => input.name !== originalName);
    if (!remove) inputs.push(bindingOnly(edit));
    const result = await request<{ valid: boolean; problems?: { message: string }[] }>('/calculations/config/draft', { method: 'PUT', headers: { 'If-Match': `"${base.revision}"` }, body: JSON.stringify({ content: base.content, state_inputs: inputs }) });
    setEdit(null); setView('draft'); setPage(0);
    setMessage(result.valid ? '草稿已保存，尚未生效。请校验并激活。' : `草稿已保存但校验未通过：${result.problems?.map((p) => p.message).join('；') ?? ''}`);
    await refresh();
  };
  const sourceItems = sources.data ?? [];
  const memberSources = edit?.mode === 'group_default' ? sourceItems.filter((source) => source.groupId === edit.group_id || source.groupBindings?.some((binding) => binding.groupId === edit.group_id)) : sourceItems;
  const selectedSources = edit?.mode === 'fixed' ? memberSources.filter((source) => source.id === edit.targets[0]?.source_id) : memberSources;
  const autofill = async () => {
    if (!edit) return;
    const seed = edit.targets.find((t) => t.port_id && t.device_id);
    if (!seed) throw new Error('请先选择一个来源的串口和站点');
    const missing: string[] = [];
    const filled = await Promise.all(memberSources.map(async (source) => {
      const current = edit.targets.find((t) => t.source_id === source.id && t.port_id && t.device_id);
      if (current) return current;
      const query = new URLSearchParams({ group_id: edit.group_id ?? '', source_id: source.id, port_id: seed.port_id, device_id: seed.device_id, limit: '1' });
      const result = await request<Page<Candidate>>(`/calculations/state-input-candidates?${query}`);
      if (!result.items.length) { missing.push(source.id); return { source_id: source.id, port_id: '', device_id: '' }; }
      const { source_id, port_id, device_id } = result.items[0]; return { source_id, port_id, device_id };
    }));
    setEdit({ ...edit, targets: filled });
    setMessage(missing.length ? `以下来源未找到同名串口和站点，请手动选择：${missing.join('、')}` : '已匹配同名串口和站点，请核对后保存。');
  };
  return <section className="calculation-tab-panel state-inputs-panel">
    <div className="workflow-note"><strong>① 选择站点并命名 → ② 保存草稿 → ③ 在规则配置中校验、激活</strong><span>激活后直接生成同名只读 UInt32 OPC 节点（ns=2），无需转发公式。保存草稿不会立即生效。</span></div>
    <details className="secondary-settings"><summary>状态 0～5 是什么意思？</summary><div className="state-legend">{['未知 / 过期', '通信正常', '通信异常', '串口未打开', '已停用', '下级离线'].map((label, value) => <span key={value}><b>{value}</b>{label}</span>)}</div><p>状态质量 Good 表示判断有效，不表示设备正常。离线保留数值 5，不清零。</p></details>
    <form className="toolbar filter-toolbar state-input-filters" onSubmit={(event) => { event.preventDefault(); setQuery(search.trim()); setPage(0); }}>
      <label className="field inline-field">查看<select aria-label="配置视图" value={view} onChange={(e) => { setView(e.target.value as typeof view); setPage(0); }}><option value="active">当前生效</option><option value="draft">已保存草稿（未生效）</option></select></label>
      <label className="search-field"><input aria-label="搜索状态变量" placeholder="搜索变量名" value={search} onChange={(e) => setSearch(e.target.value)} /><button type="submit">搜索</button></label>
      <select aria-label="筛选取源模式" value={filterMode} onChange={(e) => { setFilterMode(e.target.value); setPage(0); }}><option value="">全部模式</option><option value="fixed">固定来源</option><option value="group_default">跟随组默认来源</option></select>
      <select aria-label="筛选实际来源" value={filterSource} onChange={(e) => { setFilterSource(e.target.value); setPage(0); }}><option value="">全部实际来源</option>{sourceItems.map((s) => <option key={s.id} value={s.id}>{s.name} ({s.id})</option>)}</select>
      <select aria-label="筛选状态" value={filterStatus} onChange={(e) => { setFilterStatus(e.target.value); setPage(0); }}><option value="">全部状态</option>{['未知或过期', '通信正常', '通信异常', '串口未打开', '已停用', '下级离线'].map((s, i) => <option key={i} value={i}>{i} · {s}</option>)}</select>
      <button type="button" className="button button-outline" onClick={() => { setQuery(''); setSearch(''); setFilterMode(''); setFilterSource(''); setFilterStatus(''); setPage(0); }}>清空筛选</button>
    </form><div className="toolbar state-input-actions">
      <button className="button button-primary" disabled={!draftBundle.data || busy || !!edit} onClick={() => open()}>新增状态输入</button>
      <button className="button button-secondary" disabled={busy} onClick={() => void refresh()}>刷新</button>
      <Link className="button button-outline" to="?tab=config">规则编辑 / 校验 / 激活</Link>
    </div>
    <p className="query-context">{view === 'active' ? '当前生效的映射' : '已保存、尚未激活的草稿映射'} · {values.data?.total ?? '—'} 项 · 草稿版本 {draftBundle.data?.revision ?? '—'}{view === 'active' ? ' · 每 5 秒更新状态' : ''}</p>
    {groups.error || sources.error ? <ErrorState error={groups.error || sources.error} onRetry={() => { void groups.refetch(); void sources.refetch(); }} /> : null}
    {error ? <p role="alert" className="text-bad">{error}</p> : null}{message ? <p role="status">{message}</p> : null}
    {draftBundle.error ? <ErrorState error={draftBundle.error} onRetry={() => void draftBundle.refetch()} /> : null}
    {edit ? <section className="content-panel state-input-editor" aria-label="状态输入配置">
      <h3>{originalName ? '编辑状态输入' : '新增状态输入'}</h3>
      <fieldset className="unstyled-fieldset" disabled={busy}>
      <div className="toolbar"><label className="field">变量名<input aria-label="变量名" aria-describedby="state-input-name-hint" value={edit.name} maxLength={128} onChange={(e) => setEdit({ ...edit, name: e.target.value })} placeholder="9CYE91GH201_COM1" /></label>
      <label className="field">取源模式<select aria-label="取源模式" value={edit.mode} onChange={(e) => setEdit({ name: edit.name, mode: e.target.value as StateBinding['mode'], group_id: e.target.value === 'group_default' ? groups.data?.[0]?.id : undefined, targets: [] })}><option value="group_default">跟随组默认来源</option><option value="fixed">固定来源</option></select></label>
      {edit.mode === 'group_default' ? <label className="field">逻辑组<select aria-label="逻辑组" value={edit.group_id ?? ''} onChange={(e) => setEdit({ ...edit, group_id: e.target.value, targets: [] })}><option value="">请选择</option>{groups.data?.map((g) => <option key={g.id} value={g.id}>{g.name} ({g.id})</option>)}</select></label> : <label className="field">下级来源<select aria-label="下级来源" value={edit.targets[0]?.source_id ?? ''} onChange={(e) => setEdit({ ...edit, targets: [{ source_id: e.target.value, port_id: '', device_id: '' }] })}><option value="">请选择</option>{sourceItems.map((s) => <option key={s.id} value={s.id}>{s.name} ({s.id})</option>)}</select></label>}</div>
      <p id="state-input-name-hint">允许数字开头，如9CYE91GH201_COM1；最长128字符，只能使用英文字母、数字和下划线，不能是纯数字或IF/true/false，不能与现有点或计算输出重名。</p>
      {selectedSources.map((source) => <TargetPicker key={source.id} source={source.id} label={`${source.name} (${source.id})`} group={edit.mode === 'group_default' ? edit.group_id : undefined} value={edit.targets.find((t) => t.source_id === source.id)} onChange={(target) => setEdit((current) => current ? { ...current, targets: [...current.targets.filter((t) => t.source_id !== target.source_id), target] } : current)} />)}
      {edit.mode === 'group_default' ? <p>每个候选来源都需要选择对应站点。允许各来源使用不同的串口或站号；缺少映射时不能激活。<button className="button button-secondary" disabled={busy} onClick={() => void act(autofill)}>按已选站点匹配其他来源</button></p> : null}
      <p>OPC 输出：<code>ns=2;s={edit.name || '状态变量名'}</code>（UInt32，只读，激活后生效）。也可在其他公式中引用：<code>IF({edit.name || '状态变量名'} == 1, 0, 1)</code>。</p>
      <label><input type="checkbox" checked={normalized} onChange={(e) => setNormalized(e.target.checked)} />我确认保存时规范化草稿 TOML 排版并移除注释，保留现有规则字段值。</label>
      <div className="toolbar"><button className="button button-primary" disabled={busy || !normalized || !isValidStateInputName(edit.name) || !selectedSources.length || selectedSources.some((source) => !edit.targets.some((target) => target.source_id === source.id && target.port_id && target.device_id))} onClick={() => void act(() => save())}>保存草稿</button>{originalName ? <button className="button button-outline" disabled={busy || !normalized} onClick={() => void act(() => save(true))}>从草稿删除</button> : null}<button className="button button-secondary" disabled={busy} onClick={() => setEdit(null)}>取消</button></div></fieldset>
    </section> : null}
    {values.isPending ? <LoadingState /> : values.error ? <ErrorState error={values.error} onRetry={() => void values.refetch()} /> : <section className="table-panel content-panel"><div className="table-scroll"><table className="data-table responsive-table state-input-table"><thead><tr><th>变量名</th><th>取源模式</th><th>当前实际站点</th><th>状态值</th><th>质量 / 错误</th><th>观测时间</th><th>操作</th></tr></thead><tbody>{values.data?.items.map((item) => <tr className="virtual-row" key={item.name}><td><code>{item.name}</code></td><td>{item.mode === 'fixed' ? '固定来源' : `跟随默认来源 · ${item.group_id}`}</td><td>{item.resolved_target ? <><strong>{sourceItems.find((source) => source.id === item.resolved_target?.source_id)?.name || item.resolved_target.source_id}</strong><span className="sub-cell">{item.resolved_target.port_id} / 站点 {item.resolved_target.device_id}</span><small>{item.resolved_target.source_id}</small></> : '无有效目标'}</td><td>{item.value} · {item.state_text}</td><td className={item.status_code ? 'text-bad' : ''}>{item.status_code ? 'BadConfigurationError' : 'Good'}{item.error ? <span className="sub-cell">{item.error}</span> : null}</td><td>{formatDateTime(item.observed_at)}</td><td><div className="table-actions"><button className="button button-small" onClick={() => void act(async () => { await copyStateName(item.name); setMessage('变量名已复制'); })}>复制变量名</button><Link className="button button-small" to={`?tab=config&insert=${encodeURIComponent(item.name)}`}>插入公式</Link>{view === 'draft' ? <button className="button button-small" disabled={busy || !draftBundle.data || !!edit} onClick={() => open(item)}>编辑</button> : null}</div></td></tr>)}</tbody></table></div>{!values.data?.items.length ? <EmptyState title={query || filterMode || filterSource || filterStatus ? '没有匹配的状态输入' : view === 'active' ? '尚未激活状态输入' : '草稿中尚无状态输入'} description={query || filterMode || filterSource || filterStatus ? '请清空筛选或调整条件。' : '新增站点映射并保存草稿后，前往规则配置校验、激活。'} /> : null}<Pagination page={page + 1} pageSize={50} total={values.data?.total ?? 0} onPageChange={(next) => setPage(next - 1)} /></section>}
  </section>;
}

function TargetPicker({ source, label, group, value, onChange }: { source: string; label: string; group?: string; value?: StateTarget; onChange(value: StateTarget): void }) {
  const [query, setQuery] = useState('');
  const [page, setPage] = useState(0);
  const parameters = new URLSearchParams({ source_id: source, group_id: group ?? '', q: query, offset: String(page * 50), limit: '50' });
  const candidates = useQuery({ queryKey: ['calculations', 'state-candidates', parameters.toString()], queryFn: ({ signal }) => request<Page<Candidate>>(`/calculations/state-input-candidates?${parameters}`, { signal }) });
  const items = candidates.data?.items ?? [];
  const key = value?.port_id && value.device_id ? JSON.stringify([value.port_id, value.device_id]) : '';
  return <div className="state-target-row"><strong>{label}</strong><input aria-label={`搜索站点 ${source}`} placeholder="搜索串口或站点" value={query} onChange={(e) => { setQuery(e.target.value); setPage(0); }} /><select aria-label={`站点 ${source}`} value={key} onChange={(e) => { if (!e.target.value) { onChange({ source_id: source, port_id: '', device_id: '' }); return; } const [port_id, device_id] = JSON.parse(e.target.value) as string[]; onChange({ source_id: source, port_id, device_id }); }}><option value="">请选择对应串口 / 站点</option>{key && !items.some((c) => c.port_id === value?.port_id && c.device_id === value?.device_id) ? <option value={key}>{value?.port_id} / {value?.device_id}（已配置，请确认）</option> : null}{items.map((c) => <option key={JSON.stringify([c.port_id, c.device_id])} value={JSON.stringify([c.port_id, c.device_id])}>{c.port_id} / {c.device_id} · {c.device_name}</option>)}</select><button disabled={!page} onClick={() => setPage(page - 1)}>上一批</button><button disabled={(page + 1) * 50 >= (candidates.data?.total ?? 0)} onClick={() => setPage(page + 1)}>下一批</button>{candidates.error ? <span className="text-bad">{apiErrorMessage(candidates.error)}</span> : null}</div>;
}
