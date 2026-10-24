import { useCallback, useEffect, useLayoutEffect, useRef, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { Activity as ActivityIcon, ChevronLeft, ChevronRight, Expand, Pause, Play, X } from 'lucide-react';
import FlowCanvas, { laneY } from '../features/flow/FlowCanvas';
import { advanceActivity, portRows, publicRead, sourceCanFlow, STATION_COLORS, STATION_LABELS, visibleEvents, type Activity, type FlowEvent, type FlowPointPage, type FlowSession, type FlowSnapshot, type FlowSource } from '../features/flow/model';
import { statusCodeHex, statusCodeLabel } from '../lib/labels';
import './FlowPage.css';

const emptyActivity: Activity = { sources:{}, sessions:{}, updates:0, sourceDeltas:{}, sessionDeltas:{} };
const clock = (time?: string) => time ? new Date(time).toLocaleTimeString('zh-CN',{hour12:false}) : '—';
const shortPort = (port: string) => port.replace(/^\/dev\//,'');
const uaStatusLabel = (status: number) => statusCodeLabel(statusCodeHex(status));
const stationAlert = (state: number) => state===2 || state===3 || state===5;

export default function FlowPage() {
  const [paused,setPaused]=useState(false), [groupID,setGroupID]=useState(''), [sourcePage,setSourcePage]=useState(0), [sessionPage,setSessionPage]=useState(0);
  const [selected,setSelected]=useState<string>(), [hovered,setHovered]=useState<string>(), [events,setEvents]=useState<(FlowEvent & {id:number; at:string})[]>([]), [activity,setActivity]=useState(emptyActivity), [trend,setTrend]=useState<number[]>([0]);
  const [now,setNow]=useState(performance.now()), [lastReceived,setLastReceived]=useState(0), [fullscreenError,setFullscreenError]=useState('');
  const [history,setHistory]=useState<Record<string,number[]>>({}), [noteTrend,setNoteTrend]=useState<number[]>([0]), [queueTrend,setQueueTrend]=useState<number[]>([]), [anchors,setAnchors]=useState<Record<string,[number,number]>>({}), [viewHeight,setViewHeight]=useState(()=>window.innerHeight);
  const sceneRef=useRef<HTMLElement>(null);
  const previous=useRef<FlowSnapshot | undefined>(undefined);
  const activityRef=useRef(emptyActivity), eventSeq=useRef(0);
  const closeDetail=useCallback(()=>setSelected(undefined),[]);
  const snapshot=useQuery({queryKey:['public-flow'],queryFn:({signal})=>publicRead<FlowSnapshot>('',signal),refetchInterval:1000,refetchIntervalInBackground:false,retry:false,staleTime:0});
  useEffect(()=>{
    if (!snapshot.data) return;
    const data=snapshot.data, time=performance.now();
    const next=advanceActivity(previous.current,data,activityRef.current,time);
    const additions=visibleEvents(previous.current,data).map((event)=>({...event,id:++eventSeq.current,at:data.generated_at}));
    if (previous.current?.instance!==data.instance) { setEvents([]);setTrend([0]);setNoteTrend([0]);setQueueTrend([]);setHistory({}); }
    if (additions.length) setEvents((old)=>[...additions,...old].slice(0,40));
    if (previous.current) {
      setTrend((old)=>[...old,next.updates].slice(-60));
      setNoteTrend((old)=>[...old,Object.values(next.sessionDeltas).reduce((sum,value)=>sum+value,0)].slice(-60));
      // Per-card sparklines: the last 30 snapshots of real samples and notifications.
      setHistory((old)=>Object.fromEntries([...data.sources.map((s)=>[`s:${s.id}`,next.sourceDeltas[s.id]??0] as const),...data.sessions.map((c)=>[`c:${c.id}`,next.sessionDeltas[c.id]??0] as const)].map(([key,value])=>[key,[...(old[key]??[]),value].slice(-30)])));
    }
    setQueueTrend((old)=>[...old,data.pending_commands].slice(-60));
    previous.current=data; activityRef.current=next;setActivity(next);
    setLastReceived(time-Math.max(0,Date.now()-snapshot.dataUpdatedAt));
  },[snapshot.data,snapshot.dataUpdatedAt]);
  useEffect(()=>{const id=window.setInterval(()=>setNow(performance.now()),1000);return ()=>window.clearInterval(id);},[]);
  useEffect(()=>{setSourcePage(0);setSessionPage(0);},[groupID]);
  useEffect(()=>{const resize=()=>setViewHeight(window.innerHeight);window.addEventListener('resize',resize);return ()=>window.removeEventListener('resize',resize);},[]);
  // Canvas wires start at the measured centre of each card port row, in scene units.
  useLayoutEffect(()=>{
    const scene=sceneRef.current, box=scene?.getBoundingClientRect();
    if (!scene || !box?.width || !box.height) return;
    const next: Record<string,[number,number]>={};
    scene.querySelectorAll<HTMLElement>('[data-anchor]').forEach((row)=>{const card=row.closest('.flow-source')?.getBoundingClientRect(), rect=row.getBoundingClientRect();if(card && rect.height)next[row.dataset.anchor ?? '']=[(card.right-box.left)/box.width*1680,(rect.top+rect.height/2-box.top)/box.height*750];});
    setAnchors((old)=>{const keys=Object.keys(next);return keys.length===Object.keys(old).length && keys.every((key)=>old[key] && Math.abs(old[key][0]-next[key][0])<.5 && Math.abs(old[key][1]-next[key][1])<.5)?old:next;});
  });
  const data=snapshot.data;
  const fresh=!!data && !snapshot.isError && now-lastReceived<5000 && !!lastReceived;
  const group=data?.groups.find((g)=>g.id===groupID) ?? data?.groups[0];
  const allSources=data?.sources.filter((s)=>!group || group.members.includes(s.id)) ?? [];
  const allSessions=data?.sessions.filter((s)=>!group || s.routes[group.id]!==undefined) ?? [];
  const sourcePages=Math.max(1,Math.ceil(allSources.length/4)), sessionPages=Math.max(1,Math.ceil(allSessions.length/4));
  const sp=Math.min(sourcePage,sourcePages-1), cp=Math.min(sessionPage,sessionPages-1);
  const sources=allSources.slice(sp*4,sp*4+4),sessions=allSessions.slice(cp*4,cp*4+4);
  const picked=data?.sources.find((s)=>s.id===selected), maxPorts=viewHeight>=860?5:3;
  const devices=data?.sources.flatMap((s)=>s.ports.flatMap((p)=>p.devices)) ?? [];
  const abnormal=devices.filter((d)=>d.state===2 || d.state===3).length;
  const activeCount=data?.sources.filter((s)=>fresh && sourceCanFlow(s) && !!activity.sources[s.id] && now-activity.sources[s.id]<3200).length ?? 0;
  async function fullscreen() {
    try { if(document.fullscreenElement)await document.exitFullscreen();else await document.documentElement.requestFullscreen();setFullscreenError(''); }
    catch {setFullscreenError('浏览器未允许全屏，可使用 F11。');}
  }
  return <main className={`flow-page${!fresh ? ' flow-stale':''}${paused ? ' flow-paused':''}`}>
    <header className="flow-header"><div className="flow-brand"><span className="flow-brand-mark" aria-hidden="true"><ActivityIcon size={18}/></span><div className="flow-brand-text"><h1>RCD 数据流监视</h1><small>DATA FLOW MONITOR</small></div><span className="flow-readonly">只读展示</span></div><span className="flow-slogan">采集<i/>路由<i/>计算<i/>发布</span><div className="flow-header-actions"><span className={`flow-monitor${fresh?' is-live':''}`} role="status"><i className="flow-live-dot" aria-hidden="true"/>{fresh?'监视连接正常':snapshot.isPending?'正在连接监视接口':'监视已断开 / 状态过期'}</span><button type="button" onClick={()=>setPaused((p)=>!p)} title={paused?'恢复视觉动画；不影响采集':'暂停视觉动画；数据继续更新'}>{paused?<Play size={14}/>:<Pause size={14}/>} {paused?'播放':'暂停动画'}</button><button type="button" onClick={()=>void fullscreen()} title="切换全屏"><Expand size={14}/>全屏</button><a href="#/">进入管理 ↗</a></div></header>
    <section className="flow-toolbar" aria-label="监视范围"><span className="flow-kpi"><small>下级连接</small><b>{data ? `${data.sources.filter((s)=>s.connected).length}/${data.sources.length}`:'—'}</b></span><span className="flow-kpi"><small>上级会话</small><b>{data?.sessions.length ?? '—'}</b></span><span className="flow-kpi"><small>现场异常</small><b className={abnormal?'flow-amber':''}>{data?`${abnormal} 站`:'—'}</b></span><label className="flow-kpi"><small>逻辑组</small><select aria-label="查看逻辑组" value={group?.id ?? ''} onChange={(e)=>setGroupID(e.target.value)}>{data?.groups.length?data.groups.map((g)=><option key={g.id} value={g.id}>{g.name || g.id}</option>):<option value="">暂无逻辑组</option>}</select></label><span className="flow-kpi flow-endpoint"><small>管理端</small><span>{data?.endpoint || '等待管理端状态'}</span></span><span className="flow-kpi flow-received"><small>最后收到</small><b>{clock(data?.generated_at)}</b></span></section>
    {!fresh && data ? <div className="flow-offline" role="alert">正在展示最后收到的快照；数据流已停止，设备状态待重新确认。{snapshot.isError?' 正在自动重连…':''}</div>:null}
    {fullscreenError?<div className="flow-offline" role="status">{fullscreenError}</div>:null}
    <section ref={sceneRef} className="flow-scene" aria-label="采集、管理处理和客户端数据流拓扑">
      <FlowCanvas sources={sources} sessions={sessions} activity={activity} live={fresh} paused={paused} selected={selected} cacheActive={activeCount>0} group={group?.id} focus={hovered ?? selected} defaultSource={group?.default_source} anchors={anchors} maxPorts={maxPorts} calculation={data?{active:data.calculation.active,good:data.calculation.good,bad:data.calculation.bad,evaluated:data.calculation.last_evaluation_at}:undefined}/>
      <div className="flow-col-head flow-col-sources"><small>SOURCES</small><b>现场采集</b><span>{allSources.length} 来源 · 串口 / 站点</span></div><div className="flow-col-head flow-col-clients"><small>CLIENTS</small><b>上级订阅</b><span>{allSessions.length} 会话</span></div>
      <div className="flow-frame-title"><small>MANAGER</small><b>当前 OPC 管理端</b><span>{data?.endpoint ?? ''}</span></div>
      <div className="flow-stage flow-stage-cache"><span className="flow-section-index">01 / LATEST SAMPLES</span><h2>样本缓存</h2><p>{group?.name || '逻辑组'} · 默认来源 <strong>{group?.default_source || '未配置'}</strong></p></div>
      <div className="flow-stage flow-stage-route"><span className="flow-section-index">02 / ROUTING</span><h2>会话选源</h2><p>每个 Session 独立选择来源</p></div>
      <div className="flow-stage flow-stage-calc"><span className="flow-section-index">03 / CALCULATION</span><h2>中央计算</h2>{data?.calculation.active?<><p>{data.calculation.rules.toLocaleString()} 条规则 · {data.calculation.duration_ms.toFixed(2)} ms / 批</p><p><span className="flow-green">Good {data.calculation.good.toLocaleString()}</span> · <span className="flow-amber">其他 {data.calculation.bad.toLocaleString()}</span></p></>:<p>中央计算未激活</p>}<small>按默认来源计算 · 结果共享</small></div>
      {sources.map((source,index)=><SourceCard key={source.id} source={source} index={index} count={sources.length} active={fresh && sourceCanFlow(source) && !!activity.sources[source.id] && now-activity.sources[source.id]<3200} selected={selected===source.id} delta={activity.sourceDeltas[source.id] ?? 0} history={history[`s:${source.id}`]} maxPorts={maxPorts} onSelect={()=>setSelected(source.id)} onHover={setHovered}/>)}
      {sessions.map((session,index)=><SessionCard key={session.id} session={session} index={index} count={sessions.length} route={group?session.routes[group.id] ?? '':undefined} active={fresh && session.attached && !!activity.sessions[session.id] && now-activity.sessions[session.id]<3200} delta={activity.sessionDeltas[session.id] ?? 0} history={history[`c:${session.id}`]} onHover={setHovered}/>)}
      {!data ? <div className="flow-empty">{snapshot.isPending?'正在读取真实运行状态…':'无法读取监视接口，请确认管理端可访问。'}{snapshot.isError?<button type="button" onClick={()=>void snapshot.refetch()}>重新连接</button>:null}</div>:null}
      {data && !sources.length?<div className="flow-empty flow-empty-left">当前组没有来源</div>:null}
      {data && !sessions.length?<div className={`flow-cache-output${activeCount>0?' is-active':''}`}><span className="flow-section-index">LATEST SAMPLE BUFFER</span><h2>最新样本缓存</h2><p>{activeCount>0?'正在收到采集更新':'等待数据变化'}</p><strong>上级会话 0</strong><small>尚未向客户端发送<br/>连接后按真实 Session 展开分流</small></div>:null}
      <div className="flow-scene-pagination flow-source-pagination"><button type="button" aria-label="上一页来源" disabled={sp===0} onClick={()=>setSourcePage(sp-1)}><ChevronLeft size={15}/></button><span>来源 {allSources.length} · {sp+1}/{sourcePages} 页</span><button type="button" aria-label="下一页来源" disabled={sp+1>=sourcePages} onClick={()=>setSourcePage(sp+1)}><ChevronRight size={15}/></button></div><div className="flow-scene-pagination flow-client-pagination"><button type="button" aria-label="上一页客户端" disabled={cp===0} onClick={()=>setSessionPage(cp-1)}><ChevronLeft size={15}/></button><span>会话 {allSessions.length} · {cp+1}/{sessionPages} 页</span><button type="button" aria-label="下一页客户端" disabled={cp+1>=sessionPages} onClick={()=>setSessionPage(cp+1)}><ChevronRight size={15}/></button></div>
    </section>
    <section className="flow-dock" aria-label="运行概况">
      <EventsPanel events={events}/>
      <ActivityPanel samples={trend} notes={noteTrend} active={activeCount} total={data?.sources.length ?? 0}/>
      <StationPanel sources={allSources} focus={hovered ?? selected}/>
      <QueuePanel pending={data?.pending_commands} history={queueTrend}/>
    </section>
    <footer className="flow-footer"><span className="flow-legend"><span><i className="is-data"/>数据更新</span><span><i className="is-warn"/>现场异常</span><span><i className="is-bad"/>OPC 断开</span><span><i className="is-off"/>停用 / 未知</span></span><span>数据包依据收到的样本、生成的通知和计算求值；扫描线与网格为装饰</span><span>免登录只读 · 管理操作需登录</span></footer>
    {picked?<SourceDetail key={picked.id} source={picked} fresh={fresh} onClose={closeDetail}/>:null}
  </main>;
}

// Activity lasts about 3 s, so an active card keeps showing its last non-zero count between snapshots.
const latestCount=(delta:number,history?:number[])=>{const value=delta || [...(history ?? [])].reverse().find((v)=>v>0);return value?`+${value.toLocaleString()}`:'—';};

function Spark({values}:{values?:number[]}) {
  const list=values?.length?values:[0], max=Math.max(1,...list);
  return <svg className="flow-spark" viewBox="0 0 56 16" aria-hidden="true"><polyline points={list.map((v,i)=>`${(56-(list.length-1-i)*56/29).toFixed(1)},${(14-v/max*12).toFixed(1)}`).join(' ')}/></svg>;
}

// Each port row is an anchor: the canvas wire for that port starts at the row.
function SourceCard({source,index,count,active,selected,delta,history,maxPorts,onSelect,onHover}:{source:FlowSource;index:number;count:number;active:boolean;selected:boolean;delta:number;history?:number[];maxPorts:number;onSelect:()=>void;onHover:(id?:string)=>void}) {
  const tone=!source.enabled?'off':!source.connected?'down':source.catalog_state==='CatalogConflict'?'warn':active?'live':'idle';
  const status=!source.enabled?'来源停用':!source.connected?'OPC 未连接':source.catalog_state==='CatalogConflict'?'已连接 · 目录冲突':active?'正在收到更新':source.subscribed?'已连接 · 等待数据变化':'已连接 · 订阅未就绪';
  const stations=source.ports.reduce((total,port)=>total+port.devices.length,0);
  return <button type="button" className={`flow-source is-${tone}${selected?' is-selected':''}`} style={{top:`${laneY(index,count)/750*100}%`}} onClick={onSelect} onMouseEnter={()=>onHover(source.id)} onMouseLeave={()=>onHover(undefined)} onFocus={()=>onHover(source.id)} onBlur={()=>onHover(undefined)} title="展开串口、站点和只读点位">
    <span className="flow-card-head"><span className="flow-source-heading">{source.name || source.id}</span><span className="flow-card-tag">{source.ports.length} 串口 · {stations}{source.truncated?'+':''} 站</span></span>
    <span className="flow-card-meta">{source.id} · {source.address.replace('opc.tcp://','')}</span>
    <span className="flow-card-status"><span><i/>{status}</span><b title="最近一次有更新的快照间隔收到的采集样本">{active?latestCount(delta,history):'—'}</b><Spark values={history}/></span>
    <span className="flow-ports">{portRows(source.ports,maxPorts).map(({key,ports})=>{
      const devices=ports.flatMap((port)=>port.devices.map((device)=>({port,device}))), ok=devices.filter(({device})=>device.state===1).length;
      const warn=!ok && devices.some(({device})=>device.state===2 || device.state===3);
      return <span key={key} className={`flow-port-row${warn?' is-warn':!ok?' is-idle':''}`} data-anchor={`${source.id}::${key}`}><em>{key==='+'?`+${ports.length} 串口`:shortPort(ports[0].id)}</em><span className="flow-port-dots">{devices.slice(0,14).map(({port,device})=><i key={`${port.id}/${device.id}`} className={stationAlert(device.state)?'is-alert':undefined} title={`${port.name || port.id} / 站${device.id} · ${STATION_LABELS[device.state]}`} style={{background:STATION_COLORS[device.state],color:STATION_COLORS[device.state]}}/>)}</span><small>{ok}/{devices.length}</small></span>;
    })}</span>
  </button>;
}

function SessionCard({session,index,count,route,active,delta,history,onHover}:{session:FlowSession;index:number;count:number;route?:string;active:boolean;delta:number;history?:number[];onHover:(id?:string)=>void}) {
  const tone=!session.attached?'off':active?'live':'idle';
  const status=!session.attached?'通道分离 · 保留会话':active?'正在生成通知':session.subscriptions?'订阅等待更新':'已连接 · 无订阅';
  return <article className={`flow-client is-${tone}`} style={{top:`${laneY(index,count)/750*100}%`}} onMouseEnter={()=>onHover(route || undefined)} onMouseLeave={()=>onHover(undefined)}>
    <span className="flow-card-head"><h2>{session.name || `OPC 客户端 ${session.id.slice(0,6)}`}</h2><span className="flow-card-tag">{session.subscriptions} 订阅</span></span>
    <span className="flow-card-meta">会话 {session.id.slice(0,8)} · {session.monitored_items.toLocaleString()} 监控项</span>
    <span className="flow-client-route">采集来源 <strong>{route===undefined?'无路由':route || '未选择'}</strong></span>
    <span className="flow-card-status"><span><i/>{status}</span><b title="最近一次有更新的快照间隔生成的数据通知">{active?latestCount(delta,history):'—'}</b><Spark values={history}/></span>
  </article>;
}

const EVENT_KIND: Record<FlowEvent['kind'],string> = { source:'来源', station:'站点', route:'选源', session:'会话' };
const STATION_GROUPS=[{label:'正常',color:STATION_COLORS[1],states:[1]},{label:'异常',color:STATION_COLORS[2],states:[2,3]},{label:'离线',color:STATION_COLORS[5],states:[5]},{label:'未知 / 停用',color:STATION_COLORS[0],states:[0,4]}];

function EventsPanel({events}:{events:(FlowEvent & {id:number; at:string})[]}) {
  return <section className="flow-panel flow-panel-events"><header><small>EVENTS</small><h2>实时事件</h2><span>自打开页面起 · {events.length} 条</span></header>
    <div className="flow-event-list">{events.length?events.slice(0,6).map((e)=><p key={e.id} className={`is-${e.tone}`} title={e.text}><time>{clock(e.at)}</time><b>{EVENT_KIND[e.kind]}</b><span>{e.text}</span></p>):<p className="flow-event-empty"><i/>监视中 · 暂无新的连接、站点或选源变化</p>}</div>
  </section>;
}

// Samples per snapshot as bars, generated notifications as a line, each scaled to its own peak.
function ActivityPanel({samples,notes,active,total}:{samples:number[];notes:number[];active:number;total:number}) {
  const peak=Math.max(0,...samples), mean=Math.round(samples.reduce((sum,value)=>sum+value,0)/Math.max(1,samples.length)), notePeak=Math.max(1,...notes);
  const bars=samples.slice(-60), line=notes.slice(-60).map((v,i,list)=>`${(360-(list.length-1-i)*6-3).toFixed(1)},${(58-v/notePeak*48).toFixed(1)}`).join(' ');
  return <section className="flow-panel flow-panel-activity"><header><small>THROUGHPUT</small><h2>链路活动</h2><span>最近 60 次快照</span></header>
    <div className="flow-activity-stats"><span><b>{(samples[samples.length-1] ?? 0).toLocaleString()}</b>样本 / 快照</span><span><b>{(notes[notes.length-1] ?? 0).toLocaleString()}</b>通知 / 快照</span><span>峰值 {peak.toLocaleString()} · 均值 {mean.toLocaleString()}</span><span>活跃来源 {active}/{total}</span></div>
    <svg className="flow-activity-chart" viewBox="0 0 360 60" preserveAspectRatio="none" role="img" aria-label="最近60次快照的真实样本数和通知数"><path className="flow-chart-grid" d="M0 20.5H360M0 40.5H360"/>{bars.map((v,i)=><rect key={i} x={360-(bars.length-i)*6+1} y={58-v/Math.max(1,peak)*48} width="4" height={Math.max(1,v/Math.max(1,peak)*48)} className={i===bars.length-1?'is-last':undefined}/>)}{notes.length>1?<polyline points={line}/>:null}</svg>
  </section>;
}

// Station health for the current group, or for the source under the pointer.
function StationPanel({sources,focus}:{sources:FlowSource[];focus?:string}) {
  const source=sources.find((s)=>s.id===focus), list=source?[source]:sources;
  const devices=list.flatMap((s)=>s.ports.flatMap((port)=>port.devices.map((device)=>({port,device}))));
  const counts=STATION_GROUPS.map((group)=>devices.filter(({device})=>group.states.includes(device.state)).length), alerts=devices.filter(({device})=>stationAlert(device.state));
  return <section className="flow-panel flow-panel-stations"><header><small>STATIONS</small><h2>{source?source.name || source.id:'站点概况'}</h2><span>{source?`${source.id} · ${source.ports.length} 串口 · 最近更新 ${clock(source.last_data_at)}`:`当前组 ${sources.filter((s)=>s.enabled && s.connected).length}/${sources.length} 来源在线`}</span></header>
    <p className="flow-station-total"><b>{counts[0]}</b>/{devices.length} 站通信正常</p>
    <div className="flow-station-bar" aria-hidden="true">{STATION_GROUPS.map((group,i)=>counts[i]?<i key={group.label} style={{flexGrow:counts[i],background:group.color,color:group.color}}/>:null)}</div>
    <div className="flow-station-legend">{STATION_GROUPS.map((group,i)=><span key={group.label}><i style={{background:group.color}}/>{group.label} {counts[i]}</span>)}</div>
    {source?<p className="flow-station-alerts">{alerts.length?alerts.slice(0,2).map(({port,device})=>`${shortPort(port.id)} / 站${device.id} ${STATION_LABELS[device.state]}`).join(' · '):'无异常站点'}</p>:<p className="flow-station-alerts">悬停来源卡查看单个来源 · 点击查看站点和点位</p>}
  </section>;
}

function QueuePanel({pending,history}:{pending?:number;history:number[]}) {
  const peak=Math.max(0,...history);
  return <section className="flow-panel flow-panel-queue"><header><small>COMMANDS</small><h2>控制队列</h2><span>只读</span></header>
    <div className="flow-queue-head"><p className="flow-pending">{pending ?? '—'}<small>条待处理</small></p><Spark values={history.slice(-30)}/><small>峰值 {peak}</small></div>
    <ol className="flow-steps"><li>接收</li><li>排队</li><li>发送</li><li>ACK</li></ol>
    <small>只显示队列总数，不推断某条命令所处阶段或设备执行结果</small>
  </section>;
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
