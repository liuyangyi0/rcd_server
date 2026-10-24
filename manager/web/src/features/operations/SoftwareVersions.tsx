import { useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { request } from '../../api/client';
import { ErrorState, LoadingState } from '../../components/PageState';
import { InlineNotice } from '../../components/InlineNotice';
import { apiErrorMessage } from '../../api/client';
import { formatDateTime } from '../../lib/format';
import './SoftwareVersions.css';

export interface SoftwareVersion {
 source_id?: string; name: string; component: 'manager' | 'lower' | 'agent'; endpoint: string;
 build?: { version: string; release_id: string; commit: string; source_sha256: string; built_at: string; dirty: boolean; target: string };
 boot_id?: string; started_at?: string; observed_at?: string; checked_at?: string;
 state: string; cached: boolean; message?: string;
}
const roles = { manager: '管理端', lower: '下级 OPC', agent: 'Agent' };
const states: Record<string, string> = { online: '在线', offline: '不可达', cached: '等待确认', unknown: '未获取', unsupported: '版本功能不支持', unconfigured: '未绑定', disabled: '来源已停用', invalid: '版本响应无效' };

export function SoftwareVersionTable({ items }: { items: SoftwareVersion[] }) {
 const [role, setRole] = useState('');
 const [query, setQuery] = useState('');
 const filtered = items.filter(item => (!role || role === item.component) && `${item.name} ${item.source_id ?? ''} ${item.endpoint} ${item.build?.version ?? ''}`.toLowerCase().includes(query.toLowerCase()));
 const different = Object.keys(roles).filter(component => new Set(items.filter(item => item.component === component && item.build).map(item => `${item.build!.version}/${item.build!.release_id}/${item.build!.commit}/${item.build!.source_sha256}`)).size > 1);
 return <>
  <p className="section-description">当前管理端及其全部下级、Agent；每分钟读取一次。离线保留最后确认版本，管理端重启后仍可查看。在线只表示软件接口可达，不表示物理设备正常。</p>
  {different.length > 0 && <p role="status" className="text-warning">同类组件版本不一致：{different.map(value => roles[value as keyof typeof roles]).join('、')}（包含离线缓存，请结合最后确认时间核对）。</p>}
  <div className="settings-actions">
   <label className="field">组件 <select aria-label="软件组件筛选" value={role} onChange={event => setRole(event.target.value)}><option value="">全部组件</option>{Object.entries(roles).map(([value, name]) => <option key={value} value={value}>{name}</option>)}</select></label>
   <label className="field">搜索 <input aria-label="搜索软件版本" placeholder="来源、地址或版本" value={query} onChange={event => setQuery(event.target.value)} /></label>
   <span>共 {filtered.length} 项</span>
  </div>
  <div className="table-scroll"><table className="data-table"><thead><tr><th>组件 / 来源</th><th>版本 / 发布编号</th><th>状态</th><th>启动时间</th><th>最后确认</th><th>构建详情</th></tr></thead><tbody>
   {filtered.map(item => <tr key={`${item.component}/${item.source_id ?? 'self'}`}>
    <td>{roles[item.component]} · {item.name}<span className="sub-cell">{item.endpoint || '—'}</span></td>
    <td>{item.build?.version || '未获取'}<span className="sub-cell">{item.build?.release_id || '未报告发布编号'}</span></td>
    <td>{states[item.state] || '未知状态'}{item.cached && <strong className="sub-cell text-warning">离线 / 过期缓存</strong>}{item.message && <span className="sub-cell">{item.message}</span>}</td>
    <td>{formatDateTime(item.started_at)}</td><td>{formatDateTime(item.observed_at)}</td>
    <td>{item.build ? <details><summary>查看构建</summary><dl><dt>提交</dt><dd className="fingerprint">{item.build.commit || '未报告'}</dd><dt>源码摘要</dt><dd className="fingerprint">{item.build.source_sha256 || '未报告'}</dd><dt>构建时间</dt><dd>{item.build.built_at || '未报告'}</dd><dt>平台</dt><dd>{item.build.target || '未报告'}</dd><dt>含未提交修改</dt><dd>{item.build.dirty ? '是' : '否'}</dd><dt>启动标识</dt><dd className="fingerprint">{item.boot_id || '未报告'}</dd></dl></details> : '—'}</td>
   </tr>)}
   {filtered.length === 0 && <tr><td colSpan={6}>没有符合条件的组件</td></tr>}
  </tbody></table></div>
 </>;
}

export function SoftwareVersions() {
 const versions = useQuery({ queryKey: ['software-versions'], queryFn: async () => await request('/software-versions?refresh=1') as { items: SoftwareVersion[] }, refetchInterval: 60_000, retry: false });
 return <section className="settings-section software-versions" aria-label="软件版本总览">
  <div className="settings-actions"><h2>软件版本总览</h2><button className="button button-outline" type="button" disabled={versions.isFetching} onClick={() => void versions.refetch()}>{versions.isFetching ? '读取中…' : '刷新版本'}</button></div>
  {versions.error && (versions.data ? <InlineNotice tone="error"><p>刷新失败，以下保留上次读取结果：{apiErrorMessage(versions.error)}</p></InlineNotice> : <ErrorState error={versions.error} onRetry={() => void versions.refetch()} />)}
  {versions.data ? <SoftwareVersionTable items={versions.data.items} /> : versions.isLoading ? <LoadingState /> : null}
 </section>;
}
