import { useState, type ReactNode } from 'react';
import { ChevronLeft, ChevronRight } from 'lucide-react';
import MatrixCanvas from '../features/flow/MatrixCanvas';
import { MX, mxColPitch, mxColX, mxRowPitch, mxRowY, TRACE_CELLS, traceTicks, traceX } from '../features/flow/MatrixScene';
import { sourceCanFlow, STATION_COLORS, STATION_LABELS, type Activity, type FlowEvent, type FlowGroup, type FlowSession, type FlowSnapshot, type FlowSource, type TraceFrame } from '../features/flow/model';
import { clock, EVENT_KIND, latestValue, shortPort, Spark, STATION_GROUPS, stationAlert, Ticker, usePulse } from '../features/flow/ui';
import './FlowMatrix.css';

// DOM labels sit on the matrix scene's 1680×750 units; the timeline shares the trace's x units.
const X=(x:number)=>`${x/1680*100}%`, Y=(y:number)=>`${y/750*100}%`, TIMELINE_W=MX.frameLeft-14, T=(x:number)=>`${x/TIMELINE_W*100}%`;
const COLUMNS_PER_PAGE=16;
type TimedEvent=FlowEvent & {id:number; at:string};

export interface FlowMatrixProps { data?: FlowSnapshot; group?: FlowGroup; sources: FlowSource[]; sessions: FlowSession[]; activity: Activity; frames: TraceFrame[]; events: TimedEvent[]; history: Record<string,number[]>; fresh: boolean; paused: boolean; now: number; selected?: string; focus?: string; maxRows: number; onSelect: (id: string) => void; onHover: (id?: string) => void; children?: ReactNode }

// Rows are the group's sources, columns its Sessions plus the calculation fed by the default source.
export default function FlowMatrix({data,group,sources,sessions,activity,frames,events,history,fresh,paused,now,selected,focus,maxRows,onSelect,onHover,children}:FlowMatrixProps) {
  const [rowPage,setRowPage]=useState(0), [columnPage,setColumnPage]=useState(0);
  const rowPages=Math.max(1,Math.ceil(sources.length/maxRows)), columnPages=Math.max(1,Math.ceil(sessions.length/COLUMNS_PER_PAGE));
  const rp=Math.min(rowPage,rowPages-1), cp=Math.min(columnPage,columnPages-1);
  const rows=sources.slice(rp*maxRows,rp*maxRows+maxRows), columns=sessions.slice(cp*COLUMNS_PER_PAGE,cp*COLUMNS_PER_PAGE+COLUMNS_PER_PAGE);
  const calc=data?.calculation, k=columns.length, computing=!!calc?.active, pitch=mxColPitch(k,computing), dense=pitch<120, compact=mxRowPitch(rows.length)<84;
  const recent=(stamp?:number)=>fresh && !!stamp && now-stamp<3200;
  const shown=frames.slice(-TRACE_CELLS), last=shown[shown.length-1], peak=Math.max(0,...shown.flatMap((frame)=>rows.map((source)=>frame.sources[source.id]?.delta ?? 0)));
  return <>
    <section className={`flow-mx-stage${dense?' is-dense':''}`} aria-label="来源吞吐轨迹与会话选源矩阵">
      <MatrixCanvas sources={rows} sessions={columns} frames={frames} activity={activity} live={fresh} paused={paused} focus={focus} group={group?.id} defaultSource={group?.default_source} calculation={calc?{active:calc.active,good:calc.good,bad:calc.bad,evaluated:calc.last_evaluation_at}:undefined}/>
      <div className="flow-mx-title" style={{left:X(MX.labelLeft)}}><small>SOURCES</small><b>现场采集</b><span>{sources.length} 来源</span></div>
      <div className="flow-mx-title" style={{left:X(MX.traceLeft)}}><small>THROUGHPUT</small><b>吞吐轨迹</b><span>每列一次快照 · 对数刻度{peak?` · 峰值 +${peak.toLocaleString()}`:''}</span></div>
      <div className="flow-mx-total" style={{left:X(MX.labelLeft),width:X(MX.labelRight-MX.labelLeft),top:Y(MX.sumTop),height:Y(MX.sumBottom-MX.sumTop)}}><small>TOTAL · 每次快照</small><p><b><Ticker value={last ? Object.values(last.sources).reduce((sum,cell)=>sum+cell.delta,0) : 0} prefix="+"/></b>样本 <em>全部来源</em></p><p className="is-notes"><b><Ticker value={last?.notes ?? 0} prefix="+"/></b>通知 <em>全部会话</em></p></div>
      <div className="flow-mx-title" style={{left:X(MX.frameLeft)}}><small>CLIENTS</small><b>上级订阅</b><span>{sessions.length} 会话</span></div>
      <div className="flow-mx-frame-title" style={{left:`calc(${X(MX.frameLeft)} + 24px)`,top:Y(MX.frameTop)}}><small>ROUTING MATRIX</small><b>会话选源</b><span>{group?.name || group?.id || '逻辑组'} · 默认来源 <strong>{group?.default_source || '未配置'}</strong></span></div>
      {rows.map((source,index)=><MatrixRow key={source.id} source={source} index={index} count={rows.length} compact={compact} preferred={source.id===group?.default_source} active={recent(activity.sources[source.id]) && sourceCanFlow(source)} delta={activity.sourceDeltas[source.id] ?? 0} history={history[`s:${source.id}`]} selected={selected===source.id} onSelect={()=>onSelect(source.id)} onHover={onHover}/>)}
      {columns.map((session,index)=><MatrixHead key={session.id} session={session} x={mxColX(index,k,computing)} width={Math.min(196,pitch-16)} dense={dense} index={index} route={group?session.routes[group.id] ?? '':undefined} visible={rows.some((source)=>source.id===session.routes[group?.id ?? ''])} active={recent(activity.sessions[session.id]) && session.attached} delta={activity.sessionDeltas[session.id] ?? 0} history={history[`c:${session.id}`]} onHover={onHover}/>)}
      {calc && computing?<article className="flow-mx-head flow-mx-calc" style={{left:X(mxColX(k,k,computing)),width:X(160)}} onMouseEnter={()=>onHover(group?.default_source)} onMouseLeave={()=>onHover(undefined)}>
        <span className="flow-card-head"><h2>中央计算</h2><span className="flow-card-tag">{calc.rules.toLocaleString()} 规则</span></span>
        <span className="flow-card-meta">{calc.duration_ms.toFixed(2)} ms / 批</span>
        <span className="flow-mx-route">← <strong>{group?.default_source || '未配置'}</strong> 默认</span>
        <span className="flow-mx-calc-split"><span className="flow-green">Good <Ticker value={calc.good} bare/></span><span className="flow-amber">其他 <Ticker value={calc.bad} bare/></span></span>
        <span className="flow-mx-calc-bar" aria-hidden="true"><i style={{flexGrow:calc.good}}/><i style={{flexGrow:calc.bad}}/></span>
      </article>:null}
      {children}
      {data && !rows.length?<div className="flow-empty flow-mx-empty-rows">当前组没有来源</div>:null}
      {data && !sessions.length?<div className="flow-mx-empty-cols" style={{left:X(MX.gridLeft),width:X(MX.gridRight-MX.gridLeft-(computing?200:0))}}>暂无上级会话 · 数据进入缓存{computing?'并供中央计算使用':''}<small>连接后按真实 Session 展开列</small></div>:null}
      {rowPages>1?<div className="flow-scene-pagination flow-mx-row-pages"><button type="button" aria-label="上一页来源" disabled={rp===0} onClick={()=>setRowPage(rp-1)}><ChevronLeft size={15}/></button><span>来源 {rp+1}/{rowPages} 页</span><button type="button" aria-label="下一页来源" disabled={rp+1>=rowPages} onClick={()=>setRowPage(rp+1)}><ChevronRight size={15}/></button></div>:null}
      {columnPages>1?<div className="flow-scene-pagination flow-mx-column-pages"><button type="button" aria-label="上一页会话" disabled={cp===0} onClick={()=>setColumnPage(cp-1)}><ChevronLeft size={15}/></button><span>会话 {cp+1}/{columnPages} 页</span><button type="button" aria-label="下一页会话" disabled={cp+1>=columnPages} onClick={()=>setColumnPage(cp+1)}><ChevronRight size={15}/></button></div>:null}
    </section>
    <section className="flow-mx-dock" aria-label="事件与站点">
      <Timeline frames={shown} events={events} pending={data?.pending_commands}/>
      <StationArray sources={sources} focus={focus} fresh={fresh} onSelect={onSelect} onHover={onHover}/>
    </section>
  </>;
}

function MatrixRow({source,index,count,compact,preferred,active,delta,history,selected,onSelect,onHover}:{source:FlowSource;index:number;count:number;compact:boolean;preferred:boolean;active:boolean;delta:number;history?:number[];selected:boolean;onSelect:()=>void;onHover:(id?:string)=>void}) {
  const tone=!source.enabled?'off':!source.connected?'down':source.catalog_state==='CatalogConflict'?'warn':active?'live':'idle';
  const status=!source.enabled?'来源停用':!source.connected?'OPC 未连接':source.catalog_state==='CatalogConflict'?'目录冲突':active?'正在收到更新':source.subscribed?'等待数据变化':'订阅未就绪';
  const devices=source.ports.flatMap((port)=>port.devices), alerts=devices.filter((device)=>stationAlert(device.state)).length, shown=latestValue(delta,history);
  const pulsing=usePulse(`${tone==='live' || tone==='idle'?'ok':tone}|${alerts}`);
  return <button type="button" className={`flow-mx-row is-${tone}${selected?' is-selected':''}${compact?' is-compact':''}${pulsing?' is-pulsing':''}`} style={{left:X(MX.labelLeft),width:X(MX.labelRight-MX.labelLeft),top:Y(mxRowY(index,count)),animationDelay:`${index*90}ms`}} onClick={onSelect} onMouseEnter={()=>onHover(source.id)} onMouseLeave={()=>onHover(undefined)} onFocus={()=>onHover(source.id)} onBlur={()=>onHover(undefined)} title="展开串口、站点和只读点位">
    <span className="flow-card-head"><span className="flow-source-heading">{source.name || source.id}</span>{preferred?<span className="flow-mx-default">DEFAULT</span>:<span className="flow-card-tag">{source.ports.length} 串口 · {devices.length}{source.truncated?'+':''} 站</span>}</span>
    <span className="flow-card-meta">{source.id} · {source.address.replace('opc.tcp://','')}{alerts?<em> · {alerts} 站异常</em>:null}</span>
    <span className="flow-card-status"><span><i/>{status}</span><b title="最近一次有更新的快照间隔收到的采集样本">{active && shown?<Ticker value={shown} prefix="+"/>:'—'}</b></span>
  </button>;
}

function MatrixHead({session,x,width,dense,index,route,visible,active,delta,history,onHover}:{session:FlowSession;x:number;width:number;dense:boolean;index:number;route?:string;visible:boolean;active:boolean;delta:number;history?:number[];onHover:(id?:string)=>void}) {
  const tone=!session.attached?'off':active?'live':'idle', name=session.name || `OPC 客户端 ${session.id.slice(0,6)}`, shown=latestValue(delta,history);
  const status=!session.attached?'通道分离':active?'正在生成通知':session.subscriptions?'订阅等待更新':'无订阅';
  const routeLabel=route===undefined?'无路由':route?`${route}${visible?'':' · 其他页'}`:'未选择来源';
  return <article className={`flow-mx-head is-${tone}${dense?' is-dense':''}`} style={{left:X(x),width:dense?undefined:X(width),animationDelay:`${500+index*70}ms`}} title={`${name} · ${session.monitored_items.toLocaleString()} 监控项 · 采集来源 ${routeLabel}`} onMouseEnter={()=>onHover(route || undefined)} onMouseLeave={()=>onHover(undefined)}>
    {dense?<span className="flow-mx-dense-label" aria-label={name}><i/>{[...name.replace(/\s+/g,'')].slice(0,8).map((char,i)=><b key={i} aria-hidden="true">{char}</b>)}</span>:<>
      <span className="flow-card-head"><h2>{name}</h2><span className="flow-card-tag">{session.subscriptions} 订阅</span></span>
      <span className="flow-card-meta">{session.id.slice(0,8)} · {session.monitored_items.toLocaleString()} 监控项</span>
      <span className="flow-mx-route">← <strong>{routeLabel}</strong></span>
      <span className="flow-card-status"><span><i/>{status}</span><b title="最近一次有更新的快照间隔生成的数据通知">{active && shown?<Ticker value={shown} prefix="+"/>:'—'}</b><Spark values={history}/></span>
    </>}
  </article>;
}

// Events are marked on the snapshot column they arrived with, so the axis lines up with the trace above.
function Timeline({frames,events,pending}:{frames:TraceFrame[];events:TimedEvent[];pending?:number}) {
  const column=new Map(frames.map((frame,index)=>[frame.at,index])), marks=new Map<number,TimedEvent[]>();
  for(const event of events){const index=column.get(event.at);if(index!==undefined)marks.set(index,[...(marks.get(index) ?? []),event]);}
  const count=frames.length, ticks=traceTicks(frames).filter((tick)=>tick.long);
  return <section className="flow-mx-timeline" style={{width:X(TIMELINE_W)}}>
    <header style={{left:T(MX.labelLeft),width:T(MX.labelRight-MX.labelLeft)}}><small>EVENTS</small><h2>事件时间线</h2><span>自打开页面起 {events.length} 条</span><span>待处理命令 <b>{pending===undefined?'—':<Ticker value={pending} bare/>}</b></span></header>
    <div className="flow-mx-axis" style={{left:T(MX.traceLeft),width:T(MX.traceRight-MX.traceLeft)}}>
      {ticks.map(({at,index})=><span key={at} className="flow-mx-axis-tick" style={{left:`${(traceX(index,count)-MX.traceLeft)/(MX.traceRight-MX.traceLeft)*100}%`}}/>)}
      {[...marks].map(([index,list])=>{
        const tone=list.some((event)=>event.tone==='bad')?'bad':list.some((event)=>event.tone==='good')?'good':'info';
        return <span key={list[0].id} className={`flow-mx-mark is-${tone}`} style={{left:`${(traceX(index,count)-MX.traceLeft)/(MX.traceRight-MX.traceLeft)*100}%`}} title={list.map((event)=>`${clock(event.at)} ${event.text}`).join('\n')}>{list.length>1?<b>{list.length}</b>:null}</span>;
      })}
      <span className="flow-mx-now">NOW</span>
    </div>
    <ol className="flow-mx-events" style={{left:T(MX.traceLeft)}}>{events.length?events.slice(0,3).map((event)=><li key={event.id} className={`is-${event.tone}`} title={event.text}><i/><time>{clock(event.at)}</time><b>{EVENT_KIND[event.kind]}</b><span>{event.text}</span></li>):<li className="flow-mx-events-empty"><i/>监视中 · 暂无新的连接、站点或选源变化</li>}</ol>
  </section>;
}

// Every station of the group as one cell, grouped by source and port; a cell flips when its state changes.
function StationArray({sources,focus,fresh,onSelect,onHover}:{sources:FlowSource[];focus?:string;fresh:boolean;onSelect:(id:string)=>void;onHover:(id?:string)=>void}) {
  const devices=sources.flatMap((source)=>source.ports.flatMap((port)=>port.devices)), counts=STATION_GROUPS.map((group)=>devices.filter((device)=>group.states.includes(device.state)).length);
  return <section className="flow-mx-stations">
    <header><small>STATIONS</small><h2>站点阵列</h2><span className="flow-mx-station-total"><b><Ticker value={counts[0]} bare/></b>/{devices.length} 正常</span><span className="flow-station-legend">{STATION_GROUPS.map((group,i)=><span key={group.label}><i style={{background:group.color}}/>{group.label} {counts[i]}</span>)}</span></header>
    <div className="flow-mx-station-grid">{sources.map((source)=><button type="button" key={source.id} className={`flow-mx-station-source${focus && focus!==source.id?' is-dim':''}`} onClick={()=>onSelect(source.id)} onMouseEnter={()=>onHover(source.id)} onMouseLeave={()=>onHover(undefined)} title={`${source.name || source.id} · 点击查看站点和点位`}>
      <em>{source.id}</em>
      {source.ports.map((port)=><span key={port.id} className="flow-mx-port"><small>{shortPort(port.id).replace(/^tty/,'')}</small><span>{port.devices.map((device)=><i key={`${device.id}:${device.state}`} className={stationAlert(device.state) && fresh?'is-alert':undefined} style={{background:STATION_COLORS[device.state],color:STATION_COLORS[device.state]}} title={`${source.id} / ${port.name || port.id} / 站${device.id} · ${fresh?STATION_LABELS[device.state]:'状态待确认'}`}/>)}</span></span>)}
    </button>)}</div>
  </section>;
}
