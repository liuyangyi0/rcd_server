import { useCallback, useEffect, useRef, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { Activity as ActivityIcon, ArrowRight, ChevronLeft, ChevronRight, Expand, Pause, Play, X } from 'lucide-react';
import FlowCanvas, { laneY } from '../features/flow/FlowCanvas';
import { advanceActivity, publicRead, sourceCanFlow, STATION_COLORS, STATION_LABELS, visibleEvents, type Activity, type FlowPointPage, type FlowSnapshot, type FlowSource } from '../features/flow/model';
import { statusCodeHex, statusCodeLabel } from '../lib/labels';
import './FlowPage.css';

const emptyActivity: Activity = { sources:{}, sessions:{}, updates:0 };
const clock = (time?: string) => time ? new Date(time).toLocaleTimeString('zh-CN',{hour12:false}) : '—';
const shortPort = (port: string) => port.replace(/^\/dev\//,'');
const uaStatusLabel = (status: number) => statusCodeLabel(statusCodeHex(status));

export default function FlowPage() {
  const [paused,setPaused]=useState(false), [groupID,setGroupID]=useState(''), [sourcePage,setSourcePage]=useState(0), [sessionPage,setSessionPage]=useState(0);
  const [selected,setSelected]=useState<string>(), [events,setEvents]=useState<{id:number; at:string; text:string}[]>([]), [activity,setActivity]=useState(emptyActivity), [trend,setTrend]=useState<number[]>([0]);
  const [now,setNow]=useState(performance.now()), [lastReceived,setLastReceived]=useState(0), [fullscreenError,setFullscreenError]=useState('');
  const previous=useRef<FlowSnapshot | undefined>(undefined);
  const activityRef=useRef(emptyActivity), eventSeq=useRef(0);
  const closeDetail=useCallback(()=>setSelected(undefined),[]);
  const snapshot=useQuery({queryKey:['public-flow'],queryFn:({signal})=>publicRead<FlowSnapshot>('',signal),refetchInterval:1000,refetchIntervalInBackground:false,retry:false,staleTime:0});
  useEffect(()=>{
    if (!snapshot.data) return;
    const data=snapshot.data, time=performance.now();
    const next=advanceActivity(previous.current,data,activityRef.current,time);
    const additions=visibleEvents(previous.current,data).map((text)=>({id:++eventSeq.current,at:data.generated_at,text}));
    if (previous.current?.instance!==data.instance) { setEvents([]);setTrend([0]); }
    if (additions.length) setEvents((old)=>[...additions,...old].slice(0,40));
    if (previous.current) setTrend((old)=>[...old,next.updates].slice(-60));
    previous.current=data; activityRef.current=next;setActivity(next);
    setLastReceived(time-Math.max(0,Date.now()-snapshot.dataUpdatedAt));
  },[snapshot.data,snapshot.dataUpdatedAt]);
  useEffect(()=>{const id=window.setInterval(()=>setNow(performance.now()),1000);return ()=>window.clearInterval(id);},[]);
  useEffect(()=>{setSourcePage(0);setSessionPage(0);},[groupID]);
  const data=snapshot.data;
  const fresh=!!data && !snapshot.isError && now-lastReceived<5000 && !!lastReceived;
  const group=data?.groups.find((g)=>g.id===groupID) ?? data?.groups[0];
  const allSources=data?.sources.filter((s)=>!group || group.members.includes(s.id)) ?? [];
  const allSessions=data?.sessions.filter((s)=>!group || s.routes[group.id]!==undefined) ?? [];
  const sourcePages=Math.max(1,Math.ceil(allSources.length/4)), sessionPages=Math.max(1,Math.ceil(allSessions.length/4));
  const sp=Math.min(sourcePage,sourcePages-1), cp=Math.min(sessionPage,sessionPages-1);
  const sources=allSources.slice(sp*4,sp*4+4),sessions=allSessions.slice(cp*4,cp*4+4);
  const picked=data?.sources.find((s)=>s.id===selected);
  const devices=data?.sources.flatMap((s)=>s.ports.flatMap((p)=>p.devices)) ?? [];
  const abnormal=devices.filter((d)=>d.state===2 || d.state===3).length;
  const activeCount=data?.sources.filter((s)=>fresh && sourceCanFlow(s) && !!activity.sources[s.id] && now-activity.sources[s.id]<3200).length ?? 0;
  const trendMax=Math.max(1,...trend), trendPoints=trend.map((n,i)=>`${i*360/59},${60-n/trendMax*52}`), trendX=(trend.length-1)*360/59, trendY=60-trend[trend.length-1]/trendMax*52;
  async function fullscreen() {
    try { if(document.fullscreenElement)await document.exitFullscreen();else await document.documentElement.requestFullscreen();setFullscreenError(''); }
    catch {setFullscreenError('浏览器未允许全屏，可使用 F11。');}
  }
  return <main className={`flow-page${!fresh ? ' flow-stale':''}${paused ? ' flow-paused':''}`}>
    <header className="flow-header"><div className="flow-brand"><span className="flow-brand-mark" aria-hidden="true"><ActivityIcon size={22}/></span><h1>RCD 数据流监视</h1><span className="flow-readonly">只读展示</span></div><span className="flow-slogan">采集 / 路由 / 计算 / 发布</span><div className="flow-header-actions"><span className={`flow-monitor${fresh?' is-live':''}`} role="status"><i className="flow-live-dot" aria-hidden="true"/>{fresh?'监视连接正常':snapshot.isPending?'正在连接监视接口':'监视已断开 / 状态过期'}</span><button type="button" onClick={()=>setPaused((p)=>!p)} title={paused?'恢复视觉动画；不影响采集':'暂停视觉动画；数据继续更新'}>{paused?<Play size={14}/>:<Pause size={14}/>} {paused?'播放':'暂停动画'}</button><button type="button" onClick={()=>void fullscreen()} title="切换全屏"><Expand size={14}/>全屏</button><a href="#/">进入管理 ↗</a></div></header>
    <section className="flow-toolbar" aria-label="监视范围"><span>下级连接 <b>{data ? `${data.sources.filter((s)=>s.connected).length}/${data.sources.length}`:'—'}</b></span><span>上级会话 <b>{data?.sessions.length ?? '—'}</b></span><span>现场异常 <b className={abnormal?'flow-amber':''}>{data?`${abnormal} 站`:'—'}</b></span><label>逻辑组 <select aria-label="查看逻辑组" value={group?.id ?? ''} onChange={(e)=>setGroupID(e.target.value)}>{data?.groups.length?data.groups.map((g)=><option key={g.id} value={g.id}>{g.name || g.id}</option>):<option value="">暂无逻辑组</option>}</select></label><span className="flow-endpoint">{data?.endpoint || '等待管理端状态'}</span><span>最后收到 {clock(data?.generated_at)}</span></section>
    {!fresh && data ? <div className="flow-offline" role="alert">正在展示最后收到的快照；数据流已停止，设备状态待重新确认。{snapshot.isError?' 正在自动重连…':''}</div>:null}
    {fullscreenError?<div className="flow-offline" role="status">{fullscreenError}</div>:null}
    <section className="flow-scene" aria-label="采集、管理处理和客户端数据流拓扑">
      <FlowCanvas sources={sources} sessions={sessions} activity={activity} live={fresh} paused={paused} selected={selected} cacheActive={activeCount>0}/>
      <span className="flow-scene-label flow-ingress-label">现场采集 <small>串口 / 站点</small></span><span className="flow-scene-label flow-hub-label">当前 OPC 管理端</span><span className="flow-scene-label flow-egress-label">上级订阅数据流</span>
      {sources.map((source,index)=>{
        const states=source.ports.flatMap((p)=>p.devices.map((d)=>({port:p,device:d})));
        const active=fresh && sourceCanFlow(source) && !!activity.sources[source.id] && now-activity.sources[source.id]<3200;
        return <button type="button" className={`flow-source${selected===source.id?' is-selected':''}${active?' is-active':''}`} key={source.id} style={{top:`${laneY(index,sources.length)/750*100}%`}} onClick={()=>setSelected(source.id)} title="展开串口、站点和只读点位"><span className="flow-source-heading">{source.name || source.id}</span><span className="flow-source-address">{source.id} · {source.address.replace('opc.tcp://','')}</span><span className="flow-source-status" style={{color:!source.enabled?'#83949d':source.connected?'#53e3bb':'#f17479'}}>● {!source.enabled?'来源停用':source.connected?'OPC 已连接':'OPC 未连接'} <small>{source.catalog_state==='CatalogConflict'?'目录冲突':active?'正在收到更新':source.subscribed?'等待数据变化':'订阅未就绪'}</small></span><span className="flow-stations">{states.slice(0,3).map(({port,device})=><span key={`${port.id}/${device.id}`}><i className={device.state===2||device.state===3||device.state===5?'is-alert':undefined} style={{background:STATION_COLORS[device.state],color:STATION_COLORS[device.state]}}/>{shortPort(port.id)} / 站{device.id}<em>{STATION_LABELS[device.state]}</em></span>)}</span><span className="flow-source-footer">{source.ports.length} 个串口 · {states.length}{source.truncated?'+':''} 个站点 <span>展开 →</span></span></button>;
      })}
      <div className={`flow-processing${activeCount>0?' is-active':''}`}><section><span className="flow-section-index">01 / AGGREGATION</span><h2>{group?.name || '逻辑组'}</h2><p className="flow-mono">{group?.id || '尚未配置'}</p><p>默认来源 <strong>{group?.default_source || '未配置'}</strong></p><small>{group?.members.length ?? 0} 个来源 · OPC状态与现场状态独立</small></section><section><span className="flow-section-index">02 / ROUTING</span><h2>会话选源</h2><p>每个 Session 独立路由</p><small>右侧按当前组展示实际选择</small><div className="flow-process-line" aria-hidden="true"><i/><span><b/></span><ArrowRight size={17}/></div></section><section><span className="flow-section-index">03 / CALCULATION</span><h2>中央计算</h2><p>{data?.calculation.active ? `${data.calculation.rules.toLocaleString()} 条规则`:'中央计算未激活'}</p><small>按默认来源计算 · 结果共享</small>{data?.calculation.active?<div className="flow-calculation-count"><span>Good {data.calculation.good.toLocaleString()}</span><span>其他 {data.calculation.bad.toLocaleString()}</span><small>{data.calculation.duration_ms.toFixed(2)} ms / 批</small></div>:null}</section></div>
      {sessions.map((session,index)=>{
        const active=fresh && session.attached && !!activity.sessions[session.id] && now-activity.sessions[session.id]<3200;
        return <article className={`flow-client${active?' is-active':''}`} key={session.id} style={{top:`${laneY(index,sessions.length)/750*100}%`}}><h2>{session.name || `OPC 客户端 ${session.id.slice(0,6)}`}</h2><p>会话 {session.id.slice(0,8)}</p><p>采集来源 <strong>{group?session.routes[group.id] || '未选择':'无路由'}</strong></p><span className={session.attached?'flow-green':'flow-muted'}>● {session.attached?active?'正在生成通知':session.subscriptions?'订阅等待更新':'已连接 · 无订阅':'通道分离 · 保留会话'}</span><small>{session.subscriptions} 个订阅 · {session.monitored_items.toLocaleString()} 个监控项</small></article>;
      })}
      {!data ? <div className="flow-empty">{snapshot.isPending?'正在读取真实运行状态…':'无法读取监视接口，请确认管理端可访问。'}{snapshot.isError?<button type="button" onClick={()=>void snapshot.refetch()}>重新连接</button>:null}</div>:null}
      {data && !sources.length?<div className="flow-empty flow-empty-left">当前组没有来源</div>:null}
      {data && !sessions.length?<div className={`flow-cache-output${activeCount>0?' is-active':''}`}><span className="flow-section-index">LATEST SAMPLE BUFFER</span><h2>最新样本缓存</h2><p>{activeCount>0?'正在收到采集更新':'等待数据变化'}</p><strong>上级会话 0</strong><small>尚未向客户端发送<br/>连接后按真实 Session 展开分流</small></div>:null}
      <div className="flow-scene-pagination flow-source-pagination"><button type="button" aria-label="上一页来源" disabled={sp===0} onClick={()=>setSourcePage(sp-1)}><ChevronLeft size={15}/></button><span>来源 {allSources.length} · {sp+1}/{sourcePages} 页</span><button type="button" aria-label="下一页来源" disabled={sp+1>=sourcePages} onClick={()=>setSourcePage(sp+1)}><ChevronRight size={15}/></button></div><div className="flow-scene-pagination flow-client-pagination"><button type="button" aria-label="上一页客户端" disabled={cp===0} onClick={()=>setSessionPage(cp-1)}><ChevronLeft size={15}/></button><span>会话 {allSessions.length} · {cp+1}/{sessionPages} 页</span><button type="button" aria-label="下一页客户端" disabled={cp+1>=sessionPages} onClick={()=>setSessionPage(cp+1)}><ChevronRight size={15}/></button></div>
    </section>
    <section className="flow-dock"><section><h2>实时事件 <small>自打开页面起</small></h2><div className="flow-event-list">{events.length?events.slice(0,5).map((e)=><p key={e.id} title={e.text}><time>{clock(e.at)}</time>{e.text}</p>):<p className="flow-muted">暂无新的连接、站点或选源变化</p>}</div></section><section><h2>链路活动 <small>收到的采集样本 / 快照间隔</small></h2><span className="flow-trend-value" title="最近一次快照间隔收到的采集样本">{trend[trend.length-1].toLocaleString()}<small>样本</small></span><svg className="flow-trend" viewBox="0 0 360 65" role="img" aria-label="最近60次快照的真实样本更新量"><defs><linearGradient id="flow-trend-fill" x1="0" y1="0" x2="0" y2="1"><stop offset="0" stopColor="#5ef2e0" stopOpacity=".32"/><stop offset="1" stopColor="#5ef2e0" stopOpacity="0"/></linearGradient></defs><path className="flow-trend-grid" d="M0 21.5H360M0 43.5H360"/><path d={`M 0,62 L ${trendPoints.join(' L ')} L ${trendX},62 Z`} fill="url(#flow-trend-fill)"/><path className="flow-trend-line" d={`M ${trendPoints.join(' L ')}`} fill="none" stroke="#71e7dc" strokeWidth="1.5"/>{activeCount>0?<circle className="flow-trend-ping" cx={trendX} cy={trendY} r="3"/>:null}<circle className="flow-trend-head" cx={trendX} cy={trendY} r="2.4"/></svg><small>{activeCount} 个来源近期有更新 · 粒子仅表示活动，不代表逐报文计数</small></section><section><h2>选中来源</h2>{picked?<><p className="flow-detail-name">{picked.name || picked.id}</p><p>{picked.id} · {picked.ports.length} 个串口</p><small>最近采集更新 {clock(picked.last_data_at)}</small><button type="button" className="flow-text-button" onClick={()=>setSelected(picked.id)}>查看站点和点位 →</button></>:<p className="flow-muted">点击左侧来源，查看站点状态与实际值</p>}</section><section><h2>控制队列 <small>只读</small></h2><p className="flow-pending">{data?.pending_commands ?? '—'} <small>条待处理命令</small></p><p>接收 → 排队 → 发送 → ACK</p><small>这里只显示队列总数，不推断设备执行结果</small></section></section>
    <footer className="flow-footer"><span><i/>数据更新　<span className="flow-amber">● 现场异常</span>　<span className="flow-muted">● 停用 / 未知</span></span><span>圆环为视觉动画；流线依据收到的样本和生成通知变化</span><span>免登录只读 · 管理操作需登录</span></footer>
    {picked?<SourceDetail key={picked.id} source={picked} fresh={fresh} onClose={closeDetail}/>:null}
  </main>;
}

function SourceDetail({source,fresh,onClose}:{source:FlowSource;fresh:boolean;onClose:()=>void}) {
  const [port,setPort]=useState(''), [device,setDevice]=useState(''), [search,setSearch]=useState(''),[query,setQuery]=useState(''),[offset,setOffset]=useState(0);
  const heading=useRef<HTMLButtonElement>(null),dialog=useRef<HTMLElement>(null);
  useEffect(()=>{const old=document.activeElement as HTMLElement|null;heading.current?.focus();const key=(e:KeyboardEvent)=>{if(e.key==='Escape')onClose();if(e.key==='Tab'){const controls=dialog.current?.querySelectorAll<HTMLElement>('button:not(:disabled), input, select, a[href]');if(!controls?.length)return;const first=controls[0],last=controls[controls.length-1];if(e.shiftKey && document.activeElement===first){e.preventDefault();last.focus();}else if(!e.shiftKey && document.activeElement===last){e.preventDefault();first.focus();}}};window.addEventListener('keydown',key);return()=>{window.removeEventListener('keydown',key);old?.focus();};},[onClose]);
  useEffect(()=>{const timer=window.setTimeout(()=>{setQuery(search);setOffset(0);},250);return()=>window.clearTimeout(timer);},[search]);
  const params=new URLSearchParams({source_id:source.id,port_id:port,device_id:device,q:query,offset:String(offset)});
  const points=useQuery({queryKey:['public-flow-points',source.id,port,device,query,offset],queryFn:({signal})=>publicRead<FlowPointPage>(`/points?${params}`,signal),refetchInterval:fresh?2000:false,enabled:fresh,retry:false});
  const stations=source.ports.filter((p)=>!port||p.id===port).flatMap((p)=>p.devices.map((d)=>({port:p,device:d}))).filter((s)=>!device||s.device.id===device);
  const total=points.data?.total??0;
  return <div className="flow-detail-backdrop" onClick={onClose}><section ref={dialog} className="flow-detail" role="dialog" aria-modal="true" aria-labelledby="flow-detail-title" onClick={(e)=>e.stopPropagation()}><header><div><small>物理来源 / 只读</small><h2 id="flow-detail-title">{source.name || source.id}</h2><p>{source.id} · {source.address}</p></div><button ref={heading} type="button" aria-label="关闭来源详情" onClick={onClose}><X size={20}/></button></header>{!fresh?<p role="alert" className="flow-amber">监视连接已断开，以下为最后快照。</p>:null}<div className="flow-detail-filters"><label>串口<select aria-label="筛选串口" value={port} onChange={(e)=>{setPort(e.target.value);setDevice('');setOffset(0);}}><option value="">全部串口</option>{source.ports.map((p)=><option key={p.id} value={p.id}>{p.name||p.id}</option>)}</select></label><label>站点<select aria-label="筛选站点" value={device} onChange={(e)=>{setDevice(e.target.value);setOffset(0);}}><option value="">全部站点</option>{Array.from(new Set(source.ports.filter((p)=>!port||p.id===port).flatMap((p)=>p.devices.map((d)=>d.id)))).map((id)=><option key={id} value={id}>{id}</option>)}</select></label></div><div className="flow-station-grid">{stations.map(({port:p,device:d})=><span key={`${p.id}/${d.id}`} title={`${p.id} / ${d.name} · 状态时间 ${clock(d.observed_at)}`} style={{borderColor:STATION_COLORS[d.state]}}><b>{shortPort(p.id)} / 站{d.id}</b><em style={{color:STATION_COLORS[d.state]}}>{fresh?STATION_LABELS[d.state]:'状态待确认'}</em></span>)}</div>{source.truncated?<p className="flow-amber">站点较多，此快照最多展示512站；详细配置请登录管理页。</p>:null}<h3>采集点实时值</h3><input aria-label="搜索采集点" placeholder="输入变量名搜索" value={search} onChange={(e)=>setSearch(e.target.value)} maxLength={128}/><div className="flow-points-table"><table><thead><tr><th>变量 / 类型</th><th>值</th><th>质量 / 源时间</th></tr></thead><tbody>{points.data?.items.map((p)=><tr key={p.id}><td>{p.id}<small>{p.type}</small></td><td>{p.value_omitted?'值过长或非有限数，未展示':JSON.stringify(p.value)}</td><td className={p.status>>>30===0?'flow-green':'flow-amber'}>{uaStatusLabel(p.status)}<small>{clock(p.source_timestamp)}</small></td></tr>)}</tbody></table>{points.isPending?<p>正在读取点位…</p>:points.isError?<p role="alert">点位读取失败，等待重试。</p>:!points.data?.items.length?<p>没有匹配的只读采集点。</p>:null}</div><div className="flow-detail-paging"><button type="button" disabled={!offset} onClick={()=>setOffset(Math.max(0,offset-20))}>上一页</button><span>共{total}点 · 第{Math.floor(offset/20)+1}/{Math.max(1,Math.ceil(total/20))}页</span><button type="button" disabled={offset+20>=total} onClick={()=>setOffset(offset+20)}>下一页</button></div><small>数值来自管理端缓存；不直接访问串口。写入、选源和配置操作请进入管理页。</small></section></div>;
}
