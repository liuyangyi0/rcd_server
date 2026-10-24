import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { getRedundancy, redundancyKey, saveRedundancy, setSessionAutomatic, type RedundancyPolicy, type RedundancyView } from '../../api/redundancy';
import { apiErrorMessage } from '../../api/client';
import type { Group, Source, UaSession } from '../../api/types';
import { sourceHasGroup } from '../../lib/sourceGroups';
import { Modal } from '../../components/Modal';
import { ErrorState, LoadingState } from '../../components/PageState';
import { formatDateTime, shortSessionId } from '../../lib/format';

interface Props { group: Group; sources: Source[]; sessions: UaSession[]; onClose: () => void }
const comparisonLabels: Record<string, string> = { Disabled: '未开启', Unknown: '不可比较（通信/质量/时间条件不满足）', Confirming: '差异确认中', Different: '存在持续差异', Equal: '一致' };

export function RedundancyDialog(props: Props) {
  const query = useQuery({ queryKey: redundancyKey(props.group.id), queryFn: () => getRedundancy(props.group.id), refetchInterval: 3000 });
  return <Modal open title={`${props.group.name || props.group.id} · 自动切源与差异告警`} width="large" onClose={props.onClose}>
    {query.error ? <ErrorState error={query.error} onRetry={() => void query.refetch()} /> : query.data ? <RedundancyEditor {...props} view={query.data} /> : <LoadingState />}
  </Modal>;
}

function RedundancyEditor({ group, sources, sessions, view }: Props & { view: RedundancyView }) {
  const client = useQueryClient();
  const [draft, setDraft] = useState<RedundancyPolicy>(view.policy);
  const [acknowledged, setAcknowledged] = useState(false);
  const [message, setMessage] = useState('');
  const members = sources.filter((source) => sourceHasGroup(source, group.id));
  const stale = draft.revision !== view.policy.revision;
  const update = <K extends keyof RedundancyPolicy>(key: K, value: RedundancyPolicy[K]) => { setDraft((old) => ({ ...old, [key]: value })); setMessage(''); };
  const refresh = async () => { await Promise.all([client.invalidateQueries({ queryKey: redundancyKey(group.id) }), client.invalidateQueries({ queryKey: ['sessions'] }), client.invalidateQueries({ queryKey: ['groups'] })]); };
  const save = useMutation({ mutationFn: saveRedundancy, onSuccess: async (policy) => { setDraft(policy); setAcknowledged(false); setMessage('策略已保存；在线 Session 的参与方式请在下方逐个设置。'); await refresh(); } });
  const mode = useMutation({ mutationFn: ({ session, automatic, revision }: { session: string; automatic: boolean; revision: number }) => setSessionAutomatic(group.id, session, automatic, revision), onSuccess: refresh });
  const sourceOptions = (selected: string) => <><option value="">请选择来源</option>{selected && !members.some((source) => source.id === selected) ? <option value={selected}>{selected}（已不在组内）</option> : null}{members.map((source) => <option key={source.id} value={source.id}>{source.name || source.id} · {source.id}</option>)}</>;
  const timings: Array<[keyof Pick<RedundancyPolicy, 'failure_seconds' | 'healthy_seconds' | 'cooldown_seconds' | 'difference_seconds' | 'max_skew_ms'>, string, number]> = [
    ['failure_seconds', '故障持续时间（秒）', 300], ['healthy_seconds', '备用源正常确认（秒）', 300], ['cooldown_seconds', '切源冷却时间（秒）', 3600], ['difference_seconds', '差异持续时间（秒）', 300], ['max_skew_ms', '最大源时间差（毫秒）', 60000],
  ];
  return <div className="redundancy-editor">
    <p className="inline-notice">整组切换会改变该 Session 后续控制命令的目标。命令等待 ACK 时不切换；没有健康备用源时保持原来源；恢复后不自动切回。</p>
    <form onSubmit={(event) => { event.preventDefault(); save.mutate(draft); }}>
      <fieldset disabled={save.isPending}>
        <legend>自动切源</legend>
        <label className="checkbox-field"><input type="checkbox" checked={draft.enabled} onChange={(e) => update('enabled', e.target.checked)} />开启本组自动切源（新 Session 默认参与，已有 Session 单独设置）</label>
        <label className="checkbox-field"><input type="checkbox" checked={draft.default_automatic} onChange={(e) => update('default_automatic', e.target.checked)} />组默认来源也自动切换，中央计算跟随（需同时开启自动切源）</label>
        <p>候选优先级由上到下。仅完整覆盖本组、实际站点通信正常且采集点质量正常的来源可成为目标。</p>
        {draft.priority.map((id, index) => <div className="redundancy-priority" key={index}><label className="field"><span>第 {index + 1} 优先来源</span><select required value={id} onChange={(e) => update('priority', draft.priority.map((old, i) => i === index ? e.target.value : old))}>{sourceOptions(id)}</select></label><button className="button button-small" type="button" onClick={() => update('priority', draft.priority.filter((_, i) => i !== index))}>移除</button></div>)}
        <button className="button button-secondary" type="button" disabled={draft.priority.length >= members.length} onClick={() => update('priority', [...draft.priority, members.find((s) => !draft.priority.includes(s.id))?.id ?? ''])}>添加候选来源</button>
        <h3>数据差异检测</h3>
        <label className="checkbox-field"><input type="checkbox" checked={draft.compare_enabled} onChange={(e) => update('compare_enabled', e.target.checked)} />开启后台持续差异告警（不会因差异自动切源）</label>
        <div className="form-grid"><label className="field"><span>比较来源一</span><select required={draft.compare_enabled} value={draft.left_source_id} onChange={(e) => update('left_source_id', e.target.value)}>{sourceOptions(draft.left_source_id)}</select></label><label className="field"><span>比较来源二</span><select required={draft.compare_enabled} value={draft.right_source_id} onChange={(e) => update('right_source_id', e.target.value)}>{sourceOptions(draft.right_source_id)}</select></label>
          {timings.map(([key, label, max]) => <label className="field" key={key}><span>{label}</span><input required type="number" min={1} max={max} step={1} value={draft[key]} onChange={(e) => update(key, Number(e.target.value))} /></label>)}
          <label className="field"><span>数值允许绝对偏差</span><input required type="number" min={0} step="any" value={draft.absolute_tolerance} onChange={(e) => update('absolute_tolerance', Number(e.target.value))} /></label>
          <label className="field"><span>数值允许相对偏差（0～1，0.01 = 1%）</span><input required type="number" min={0} max={1} step="any" value={draft.relative_tolerance} onChange={(e) => update('relative_tolerance', Number(e.target.value))} /></label>
        </div>
        <p>差异需超过绝对/相对容差的较大者；开关量和字符串直接比较。通信异常、坏质量及时间差过大的数据不比较；“不可比较”不代表原告警已恢复。</p>
      </fieldset>
      {stale ? <div className="form-error" role="alert">策略已被其他操作修改，请重新加载后编辑。<button className="button button-small" type="button" onClick={() => { setDraft(view.policy); save.reset(); }}>重新加载策略</button></div> : null}
      <label className="checkbox-field"><input type="checkbox" checked={acknowledged} onChange={(e) => setAcknowledged(e.target.checked)} />已确认本组物理设备对应关系，以及自动切源对后续控制命令和计算的影响</label>
      {save.error ? <div className="form-error" role="alert">{apiErrorMessage(save.error)}</div> : null}
      <button className="button button-primary" type="submit" disabled={!acknowledged || stale || save.isPending || group.pendingCommandCount > 0}>保存策略</button>
      {message ? <p role="status">{message}</p> : null}
    </form>
    <section><h3>在线 Session 参与方式</h3><p>手动写入 SelectedSourceId 或使用原“切源”操作，会把该 Session 本组设为手动固定。开启组策略不会强行接管已有 Session。</p>
      {mode.error ? <div className="form-error" role="alert">{apiErrorMessage(mode.error)}</div> : null}
      {sessions.length ? <div className="table-scroll"><table className="data-table"><thead><tr><th>Session</th><th>实际来源</th><th>参与方式</th></tr></thead><tbody>{sessions.map((session) => { const route = view.routes[session.id]; return <tr key={session.id}><td title={session.id}>{shortSessionId(session.id)}</td><td>{route?.source_id || '未选择'}</td><td><select aria-label={`${session.id} 参与方式`} value={route?.automatic ? 'auto' : 'manual'} disabled={!route || mode.isPending || group.pendingCommandCount > 0} onChange={(e) => route && mode.mutate({ session: session.id, automatic: e.target.value === 'auto', revision: route.revision })}><option value="manual">手动固定</option><option value="auto" disabled={!view.policy.enabled}>跟随自动{!view.policy.enabled ? '（组策略未开启）' : ''}</option></select></td></tr>; })}</tbody></table></div> : <p>暂无在线 Session</p>}
    </section>
    <section><h3>当前检测结果</h3><p>{comparisonLabels[view.comparison_state] || view.comparison_state} · 已比较 {view.compared_count} 点 · 跳过 {view.skipped_count} 点 · 持续差异 {view.difference_count} 点</p><p>最近检测：{formatDateTime(view.evaluated_at)}；最近切源：{formatDateTime(view.last_switch_at)}</p>{view.last_switch_reason ? <p>{view.last_switch_reason}</p> : null}{view.last_error ? <p className="text-warning">{view.last_error}</p> : null}
      {view.difference_alarm_active ? <p className="text-warning" role="status">数据差异告警尚未恢复；当前不可比较时会保留原告警。</p> : null}
      <div className="table-scroll"><table className="data-table"><thead><tr><th>来源</th><th>自动切源资格</th><th>原因</th></tr></thead><tbody>{view.health.map((h) => <tr key={h.source_id}><td>{h.source_id}</td><td>{h.healthy ? '正常（仍需满足确认时间）' : '不可用'}</td><td>{h.reason}</td></tr>)}</tbody></table></div>
      {view.differences.length ? <><p>以下最多显示 20 个差异点，告警中心保留发生及恢复记录。</p><div className="table-scroll"><table className="data-table"><thead><tr><th>KKS</th><th>{view.policy.left_source_id}</th><th>{view.policy.right_source_id}</th><th>持续起点</th></tr></thead><tbody>{view.differences.map((d) => <tr key={d.node_id}><td>{d.node_id}</td><td>{String(d.left)}</td><td>{String(d.right)}</td><td>{formatDateTime(d.since)}</td></tr>)}</tbody></table></div></> : null}
    </section>
  </div>;
}
