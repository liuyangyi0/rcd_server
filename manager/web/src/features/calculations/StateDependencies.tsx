import { useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { Link } from 'react-router-dom';
import { request, apiErrorMessage } from '../../api/client';

export default function StateDependencies({ node, count }: { node: string; count: number }) {
 const [open, setOpen] = useState(false);
 const [page, setPage] = useState(0);
 const query = useQuery({ queryKey: ['calculations', 'state-inputs', 'dependency', node, page], enabled: open,
  queryFn: async ({ signal }) => await request(`/calculations/state-inputs?rule=${encodeURIComponent(node)}&limit=50&offset=${page * 50}`, { signal }) as { total: number; items: { name: string; value: number; state_text: string; error?: string; resolved_target?: { source_id: string; port_id: string; device_id: string } }[] } });
 return <details onToggle={(e) => setOpen(e.currentTarget.open)}><summary>{count} 个依赖 · 查看状态输入</summary>{open ? query.isPending ? <span>读取中…</span> : query.error ? <span role="alert">{apiErrorMessage(query.error)}</span> : <div>{query.data?.items.map((item) => <p key={item.name}><Link to={`?tab=state-inputs&state_q=${encodeURIComponent(item.name)}`}>{item.name}</Link><span className="sub-cell">{item.resolved_target ? `${item.resolved_target.source_id} / ${item.resolved_target.port_id} / ${item.resolved_target.device_id}` : '目标未解析'}</span><span className="sub-cell">{item.value} · {item.state_text}{item.error ? ` · ${item.error}` : ''}</span></p>)}{!query.data?.total ? <span>无站点状态依赖</span> : <div><button disabled={!page} onClick={() => setPage(page - 1)}>上一批</button><button disabled={(page + 1) * 50 >= query.data.total} onClick={() => setPage(page + 1)}>下一批</button></div>}</div> : null}</details>;
}
