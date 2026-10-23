import { useEffect, useImperativeHandle, useRef, useState, type Ref } from 'react';
import { apiErrorMessage } from '../../api/client';
import { MAX_CONFIG_BYTES, operationsApi, type ConfigEdit, type ConfigField, type ConfigForm, type OperationsTarget } from '../../api/operations';
import './ConfigParameters.css';
import { parameterOption, parameterPresentation } from './configParameterPresentation';

export interface ConfigParametersHandle { prepare: () => Promise<string>; discard: () => void }
interface Props { target: OperationsTarget; content: string; disabled: boolean; maxBytes?: number; onApplied: (content: string) => void; onDirtyChange: (dirty: boolean) => void; onBusyChange: (busy: boolean) => void; initialEdits?: Record<string, string>; onEditsChange?: (edits: Record<string, string>) => void; preparationRef?: Ref<ConfigParametersHandle> }

export function ConfigParameters({ target, content, disabled, maxBytes = MAX_CONFIG_BYTES, onApplied, onDirtyChange, onBusyChange, initialEdits, onEditsChange, preparationRef }: Props) {
  const [form, setForm] = useState<ConfigForm>();
  const [edits, setEdits] = useState<Record<string, string>>(() => initialEdits ?? {});
  const [error, setError] = useState('');
  const [loading, setLoading] = useState(false);
  const [applying, setApplying] = useState(false);
  const [search, setSearch] = useState('');
  const [retry, setRetry] = useState(0);
  const [scope, setScope] = useState('常用参数');
  const preparing = useRef(false);
  const alive = useRef(true);
  useEffect(() => { alive.current = true; return () => { alive.current = false; }; }, []);
  const dirty = Object.keys(edits).length > 0;
  useEffect(() => {
    const controller = new AbortController();
    setLoading(true); setError(''); setForm(undefined);
    // Reading definitions is not a user transaction: debounce raw typing and
    // never lock or blur its textarea while the read-only preparation runs.
    const timer = window.setTimeout(() => {
      void operationsApi.prepare(target, 'config.toml', content, [], controller.signal).then((value) => {
        if (!controller.signal.aborted) setForm(value);
      }).catch((reason: unknown) => { if (!controller.signal.aborted) setError(apiErrorMessage(reason)); })
        .finally(() => { if (!controller.signal.aborted) setLoading(false); });
    }, 300);
    return () => { window.clearTimeout(timer); controller.abort(); };
  }, [target.id, target.revision, content, retry]);
  useEffect(() => { onDirtyChange(dirty); return () => onDirtyChange(false); }, [dirty, onDirtyChange]);
  useEffect(() => { onBusyChange(applying); return () => onBusyChange(false); }, [applying, onBusyChange]);
  useEffect(() => { onEditsChange?.(edits); }, [edits, onEditsChange]);

  const change = (field: ConfigField, value: string) => setEdits((old) => {
    const next = { ...old };
    if (value === String(field.value)) delete next[field.key]; else next[field.key] = value;
    return next;
  });
  const apply = async () => {
    if (!dirty) return content;
    if (!form || loading || preparing.current || disabled) {
      const reason = new Error('参数暂不可提交，请等待读取完成或检查来源状态。');
      setError(reason.message); throw reason;
    }
    preparing.current = true;
    setApplying(true); setError('');
    try {
      if (Object.keys(edits).some((key) => !form.fields.some((field) => field.key === key && !field.readonly))) throw new Error('修改项已不在可编辑参数定义中，请下载或核对草稿后重新读取。');
      const changes: ConfigEdit[] = form.fields.filter((field) => Object.hasOwn(edits, field.key)).map((field) => {
        const text = edits[field.key];
        let value: ConfigEdit['value'] = text;
        if (field.kind === 'integer') {
          if (!/^-?\d+$/.test(text) || !Number.isSafeInteger(Number(text))) throw new Error(`${field.label} 必须为安全范围内的整数`);
          value = Number(text);
        } else if (field.kind === 'boolean') {
          if (text !== 'true' && text !== 'false') throw new Error(`${field.label} 必须为启用或停用`);
          value = text === 'true';
        }
        return { path: field.path, value };
      });
      const prepared = await operationsApi.prepare(target, 'config.toml', content, changes);
      if (!alive.current) throw new Error('参数页面已关闭，未提交校验或保存。');
      if (new TextEncoder().encode(prepared.content).byteLength > maxBytes) throw new Error('合并后的配置超过允许大小，参数修改已保留。');
      setEdits({}); onApplied(prepared.content);
      return prepared.content;
    } catch (reason) { if (alive.current) setError(apiErrorMessage(reason)); throw reason; }
    finally { preparing.current = false; if (alive.current) setApplying(false); }
  };
  useImperativeHandle(preparationRef, () => ({ prepare: apply, discard: () => { setEdits({}); setError(''); } }));
  const fields = form?.fields ?? [];
  const hasCommon = fields.some((field) => parameterPresentation(field).common);
  const groups = [...new Set(fields.map((field) => parameterPresentation(field).group))];
  const visible = fields.filter((field) => {
    const display = parameterPresentation(field);
    if (search.trim()) return `${display.label} ${field.key} ${display.hint}`.toLowerCase().includes(search.trim().toLowerCase());
    return scope === '全部参数' || scope === '常用参数' && (!hasCommon || display.common) || scope === display.group;
  });
  return <section className="config-parameters" aria-label="配置参数表单">
    <div className="config-section-heading"><h3>修改参数</h3><span className="operations-muted">{Object.keys(edits).length ? `已修改 ${Object.keys(edits).length} 项，尚未保存` : '修改后点击“校验当前草稿”，通过后再保存'}</span></div>
    <div className="config-parameter-filters"><label className="field"><span>显示范围</span><select aria-label="参数显示范围" value={scope} onChange={(e) => setScope(e.target.value)}><option>常用参数</option><option>全部参数</option>{groups.map((group) => <option key={group}>{group}</option>)}</select></label>
    <label className="field"><span>查找参数</span><input type="search" value={search} placeholder="搜索全部参数，例如：发送保护、日志缓存" onChange={(e) => setSearch(e.target.value)} /></label></div>
    {form ? <p className="operations-muted">显示 {visible.length} / {fields.length} 项{search.trim() ? ' · 搜索范围为全部参数' : ''}。默认值、只读项会单独标记；不自动更改现有设置。</p> : null}
    {loading ? <p role="status">正在读取参数定义…</p> : null}
    {error ? <div role="alert" className="top-error">{error} 原文和参数修改均保留。{!form ? <button type="button" className="button button-small" onClick={() => setRetry((n) => n + 1)}>重新读取参数</button> : null}</div> : null}
    <div className="config-parameter-grid">{visible.map((field) => {
      const display = parameterPresentation(field);
      const value = edits[field.key] ?? String(field.value);
      const options = field.kind === 'boolean' ? [true, false] : field.options;
      return <label className={`field config-parameter${Object.hasOwn(edits, field.key) ? ' is-edited' : ''}`} key={field.key}><span>{display.label}{field.inherited ? <small className="config-field-badge">默认值</small> : null}{field.readonly ? <small className="config-field-badge">只读</small> : null}</span>
        {options.length ? <select aria-label={display.label} value={value} disabled={disabled || loading || applying || field.readonly} onChange={(e) => change(field, e.target.value)}>
          {!options.some((option) => String(option) === value) ? <option value={value}>{value}（当前自定义值）</option> : null}
          {options.map((option) => <option key={String(option)} value={String(option)}>{parameterOption(field, option)}</option>)}
        </select> : <input aria-label={display.label} type={field.kind === 'integer' ? 'number' : 'text'} step={field.kind === 'integer' ? 1 : undefined} value={value} disabled={disabled || loading || applying || field.readonly} onChange={(e) => change(field, e.target.value)} />}
        {display.hint && !display.hint.startsWith('保存后重启生效；完整校验') ? <small>{display.hint}</small> : null}<code title="对应配置字段">{field.key}</code></label>;
    })}</div>
    {!loading && form && !visible.length ? <p>没有匹配的参数。数组、点表和其它配置可在下方原文编辑。</p> : null}
    {dirty ? <p role="status">参数尚未保存。“校验当前草稿”会先合并这些参数再校验，不需要另点“生成草稿”。<button className="button button-small button-outline" type="button" disabled={disabled || applying || loading} onClick={() => { setEdits({}); setError(''); }}>撤销参数修改</button></p> : null}
  </section>;
}
