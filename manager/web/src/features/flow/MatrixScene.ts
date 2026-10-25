import { sourceCanFlow, type Activity, type FlowSession, type FlowSource, type TraceFrame } from './model';
import { along, burstSize, clamp, easeOut, fract, hash, INK, partial, RECENT_MS, SCENE_HEIGHT, SCENE_WIDTH, SceneBase, sheet, SHIFT_S, track, withAlpha, type Point, type SceneCalculation, type SceneFrame, type Shift, type Tone, type Track, type View } from './sceneKit';

// Scene units, shared with the percentages in FlowMatrix.tsx: row labels end at labelRight, the
// throughput trace runs traceLeft → traceRight (newest at the right), the manager frame holds the
// routing matrix, and Session column heads sit above it with their sockets on headY.
export const MX = { sumTop: 64, sumBottom: 168, labelLeft: 24, labelRight: 250, traceLeft: 276, traceRight: 756, frameLeft: 790, frameRight: 1660, frameTop: 172, frameBottom: 712, gridLeft: 850, gridRight: 1620, headY: 150, busY: 196, rowsTop: 226, rowsBottom: 694, axisY: 730 };
export const TRACE_CELLS = 90;
const TRACE_PITCH = (MX.traceRight - MX.traceLeft) / TRACE_CELLS, OUTPUT_DELAY = .4, SLIDE_S = .7, SCROLL_S = .45;
export const mxRowPitch = (count: number) => Math.min(124, (MX.rowsBottom - MX.rowsTop) / Math.max(count, 1));
export const mxRowY = (index: number, count: number) => (MX.rowsTop + MX.rowsBottom) / 2 + (index - (count - 1) / 2) * mxRowPitch(count);
// The calculation column keeps its own slot at the right; Session columns share the rest.
const CALC_SLOT = 170;
export const mxColPitch = (sessions: number, calc: boolean) => Math.min(220, (MX.gridRight - MX.gridLeft - (calc ? CALC_SLOT : 0)) / Math.max(sessions, 1));
export const mxColX = (index: number, sessions: number, calc: boolean) => index >= sessions ? MX.gridRight - CALC_SLOT / 2 : (MX.gridLeft + MX.gridRight - (calc ? CALC_SLOT : 0)) / 2 + (index - (sessions - 1) / 2) * mxColPitch(sessions, calc);
// Centre of trace cell index (0 = oldest shown) when count cells are shown; the newest sits against the NOW line.
export const traceX = (index: number, count: number) => MX.traceRight - (count - 1 - index + .5) * TRACE_PITCH;
// Ruler marks sit on the first snapshot of each 10-second wall-clock span, so they scroll with the
// trace; long marks start a 30-second span and carry its receive time.
export function traceTicks(frames: TraceFrame[]) {
  const span=(at: string,size: number)=>Math.floor(Date.parse(at)/1000/size);
  return frames.flatMap((frame,index)=>index&&span(frame.at,10)!==span(frames[index-1].at,10)?[{index,at:frame.at,long:span(frame.at,30)!==span(frames[index-1].at,30)}]:[]);
}

export interface MatrixState { sources: FlowSource[]; sessions: FlowSession[]; frames: TraceFrame[]; activity: Activity; live: boolean; paused: boolean; focus?: string; group?: string; defaultSource?: string; calculation?: SceneCalculation }
// alerts counts the source's stations in an abnormal or offline state.
interface Row { key: string; id: string; y: number; link: Tone; alerts: number; line: Track; ready: boolean; stamp?: number; delta: number; running: boolean }
// A Session column, or the calculation column fed by the default source. path runs along the selected
// source's row, turns up at the crosspoint (distance turn) and rises to the column head.
interface Column { key: string; x: number; calc: boolean; route?: string; row?: Row; attached: boolean; spine: Track; riser: Track; path: Track; turn: number; ready: boolean; stamp?: number; delta: number; running: boolean }
// after is the colour once a packet has turned into a Session column.
interface Packet { track: Track; key: string; owner?: string; born: number; life: number; lead: boolean; color: string; after: string; turn: number; land: 'head' | 'calc' | 'none'; passed: boolean }
// A Session switching source: its crosspoint slides along the column from the old row to the new one.
interface Slide { key: string; from: number; to: number; start: number }
// Trace figures that only change with a snapshot: the row scale, the totals and the ruler.
interface TraceInfo { peak: number; samples: number[]; total: number; notes: number; ticks: { index: number; long: boolean; label?: string }[] }
function summarize(frames: TraceFrame[], rows: Row[]): TraceInfo {
  const samples=frames.map((frame)=>Object.values(frame.sources).reduce((sum,cell)=>sum+cell.delta,0));
  return {peak:Math.max(10,...frames.flatMap((frame)=>rows.map((row)=>frame.sources[row.id]?.delta??0))),samples,total:Math.max(0,...samples),notes:Math.max(0,...frames.map((frame)=>frame.notes)),
    ticks:traceTicks(frames).map(({index,at,long})=>({index,long,label:long?new Date(at).toLocaleTimeString('zh-CN',{hour12:false}):undefined}))};
}

// A horizontal run that turns up into a column with a rounded corner.
function elbow(start: Point, [cx,cy]: Point, top: number, r: number): Point[] {
  return [start,...Array.from({length:7},(_,i): Point=>{const a=Math.PI/2*(1-i/6);return [cx-r+Math.cos(a)*r,cy-r+Math.sin(a)*r];}),[cx,top]];
}

function layout(state: MatrixState, view: View) {
  const {sx,sy,u}=view,X=(x: number)=>x*sx,Y=(y: number)=>y*sy,n=state.sources.length,calc=!!state.calculation?.active,k=state.sessions.length;
  const start=X(MX.traceRight)+6*u,end=X(MX.gridRight+20),head=Y(MX.headY);
  const rows=state.sources.map((source,index): Row=>{
    const y=Y(mxRowY(index,n)),link: Tone=!source.enabled?'off':!source.connected?'down':source.catalog_state==='CatalogConflict'?'warn':'ok';
    return {key:`in:${source.id}`,id:source.id,y,link,alerts:source.ports.reduce((total,port)=>total+port.devices.filter((d)=>d.state===2||d.state===3||d.state===5).length,0),line:track([[start,y],[end,y]]),
      ready:state.live&&sourceCanFlow(source),stamp:state.activity.sources[source.id],delta:state.activity.sourceDeltas[source.id]??0,running:false};
  });
  const bottom=rows.length?rows[rows.length-1].y+26*sy:Y(MX.rowsBottom);
  const column=(key: string,index: number,route: string|undefined,extra: Pick<Column,'calc'|'attached'|'ready'|'stamp'|'delta'>): Column=>{
    const x=X(mxColX(index,k,calc)),row=rows.find((item)=>item.id===route),r=8*u,spine=track([[x,bottom],[x,head]]);
    // A source off this page (or no selection) has no crosspoint; its traffic rises from the column foot.
    if(!row)return {key,x,route,row,spine,riser:spine,path:spine,turn:0,running:false,...extra};
    return {key,x,route,row,spine,riser:track([[x,row.y],[x,head]]),path:track(elbow([start,row.y],[x,row.y],head,r)),turn:x-r-start,running:false,...extra};
  };
  const columns=state.sessions.map((session,index)=>column(`out:${session.id}`,index,state.group?session.routes[state.group]||undefined:undefined,
    {calc:false,attached:session.attached,ready:state.live&&session.attached,stamp:state.activity.sessions[session.id],delta:state.activity.sessionDeltas[session.id]??0}));
  if(calc)columns.push(column('calc',k,state.defaultSource,{calc:true,attached:true,ready:state.live,delta:0}));
  const sessionX=columns.filter((item)=>!item.calc).map((item)=>item.x);
  // Shared calculation results run back along the bus to every Session column.
  const bus=calc&&sessionX.length?track([[columns[columns.length-1].x,Y(MX.busY)],[Math.min(...sessionX),Y(MX.busY)]]):undefined;
  return {rows,columns,bus,bottom};
}

// Same truth rules as the pipeline scene: packets only for new samples, Session notifications or
// a new calculation evaluation; the trace adds one column per received snapshot. Route slides,
// alarms and link effects follow real changes between snapshots. The opening sequence, lattice and
// ruler are decoration. A stale monitor greys out, freezes the trace and clears every effect.
export class MatrixScene extends SceneBase {
  private evaluated?: string; private calcAt=-1e9; private motion=false;
  private rows: Row[]=[]; private columns: Column[]=[]; private bus?: Track; private bottom=0; private frames: TraceFrame[]=[]; private info=summarize([],[]); private frameKey=''; private arrived=-1e9;
  private shown?: MatrixState; private shownSize=''; private crossY=new Map<string,number>();
  private packets: Packet[]=[]; private slides: Slide[]=[];
  private backdrop?: HTMLCanvasElement; private backdropKey='';

  // Packets launched or still travelling.
  get inFlight() { return this.packets.length; }
  // Crosspoints currently sliding to a newly selected source.
  get switching() { return this.slides.length; }

  draw(ctx: CanvasRenderingContext2D, state: MatrixState, frame: SceneFrame) {
    const size=`${frame.width}x${frame.height}`;
    if(state!==this.shown||size!==this.shownSize){
      const sx=frame.width/SCENE_WIDTH,sy=frame.height/SCENE_HEIGHT;
      this.view={sx,sy,u:Math.sqrt(sx*sy)};
      ({rows:this.rows,columns:this.columns,bus:this.bus,bottom:this.bottom}=layout(state,this.view));
      this.frames=state.frames.slice(-TRACE_CELLS);this.info=summarize(this.frames,this.rows);this.shown=state;this.shownSize=size;this.focus=state.focus;
      this.notice(state.live&&frame.motion);
    }
    this.advance(state,frame);
    ctx.setTransform(1,0,0,1,0,0);ctx.globalCompositeOperation='source-over';ctx.globalAlpha=1;
    ctx.clearRect(0,0,frame.width,frame.height);
    ctx.globalAlpha=this.phase(0,.3);ctx.drawImage(this.backdropFor(frame.width,frame.height),0,0);
    ctx.globalCompositeOperation='lighter';
    this.frame(ctx,state.live);
    this.trace(ctx,state.live);
    this.links(ctx,state.live);
    this.flight(ctx);
    this.points(ctx,state.live);
    this.ripples(ctx);
    ctx.setTransform(1,0,0,1,0,0);ctx.globalCompositeOperation='source-over';ctx.globalAlpha=1;
  }

  // Compare link, station and route states with the previous snapshot and start the matching effect.
  private notice(active: boolean) {
    const seen=new Set<string>();
    const mark=(key: string,owner: string,value: string,kind: (old: string)=>Shift['kind']|undefined,at: Point,count=0)=>{
      const old=this.changed(key,value,seen),change=old===undefined?undefined:kind(old);
      // Packets already travelling on a row that just broke would cross the break, so they are dropped,
      // also while the animation is paused.
      if(change==='break')this.packets=this.packets.filter((packet)=>packet.owner!==owner);
      if(!active||!change)return;
      this.shifts.push({key,kind:change,start:this.clock});
      const color=change==='warn'?INK.amber:change==='break'?INK.red:change==='recover'?INK.ok:INK.data;
      this.waves.push({start:this.clock,color,at});if(count)this.burst(at,color,count);
    };
    for(const row of this.rows){
      const at=row.line.pts[0];
      mark(row.key,row.id,row.link,(old)=>row.link==='down'&&old!=='down'?'break':old==='down'&&row.link==='ok'?'join':undefined,at,row.link==='down'?8:0);
      mark(`alert:${row.id}`,row.id,String(row.alerts),(old)=>row.alerts>Number(old)?'warn':row.alerts<Number(old)?'recover':undefined,at);
    }
    const crossY=new Map<string,number>();
    for(const column of this.columns){
      const old=this.changed(`route:${column.key}`,column.route??'',seen),from=this.crossY.get(column.key);
      if(column.row)crossY.set(column.key,column.row.y);
      if(old===undefined)continue;
      // A newer selection replaces any slide still running for the column.
      this.slides=this.slides.filter((slide)=>slide.key!==column.key);
      if(!active||!column.row)continue;
      if(from!==undefined)this.slides.push({key:column.key,from,to:column.row.y,start:this.clock});
      else this.waves.push({start:this.clock,color:INK.out,at:[column.x,column.row.y]});
    }
    this.crossY=crossY;
    this.forget(seen);
  }

  private advance(state: MatrixState, {dt,motion,now}: SceneFrame) {
    const ease=motion&&state.live?1-Math.exp(-dt*3):1,keys=new Set<string>(['calc']);
    this.motion=motion;
    this.tick(dt,motion,this.rows.length>0||this.columns.length>0);
    // Each received snapshot adds one trace column, which slides in at the NOW line.
    const last=state.frames[state.frames.length-1],frameKey=`${state.frames.length}:${last?.at??''}`;
    if(frameKey!==this.frameKey){if(this.frameKey&&motion&&state.live&&last)this.arrived=this.clock;this.frameKey=frameKey;}
    for(const row of this.rows)this.follow(row,now,motion,ease,keys,()=>this.launchInput(row));
    for(const column of this.columns)if(!column.calc)this.follow(column,now,motion,ease,keys,()=>this.launchOutput(column));
    // A changed evaluation timestamp is the only calculation activity the snapshot reports.
    const evaluated=state.calculation?.active?state.calculation.evaluated:undefined,feed=this.columns.find((column)=>column.calc);
    if(evaluated&&this.evaluated&&evaluated!==this.evaluated){this.calcAt=now;if(motion&&state.live&&feed?.row)this.launchCalc(feed);}
    this.evaluated=evaluated;
    const calcLevel=this.level('calc');
    this.levels.set('calc',calcLevel+(Number(state.live&&!!feed&&now-this.calcAt<RECENT_MS)-calcLevel)*ease);
    this.prune(keys);
    if(!state.live){this.packets=[];this.slides=[];this.clearEffects();return;}
    // A still frame shows every crosspoint on the row actually selected.
    if(!motion){this.slides=[];return;}
    const flying: Packet[]=[],spawned: Packet[]=[];
    for(const packet of this.packets){
      const age=this.clock-packet.born;
      // The lead packet closes the crosspoint as it turns into its column.
      if(packet.lead&&!packet.passed&&packet.turn>0&&age>=packet.life*packet.turn/packet.track.total){packet.passed=true;this.flash(`x:${packet.key}`,1);}
      if(age<packet.life){flying.push(packet);continue;}
      const end=packet.track.pts[packet.track.pts.length-1];
      if(packet.land==='head'&&packet.lead){this.flash(packet.key,1);this.waves.push({start:this.clock,color:packet.after,at:end});this.burst(end,packet.after,7);}
      // A landed evaluation sends one pulse back along the shared result bus.
      if(packet.land==='calc'){this.flash('calc',1);if(this.bus)spawned.push({track:this.bus,key:'bus',born:this.clock,life:this.bus.total/(380*this.view.u),lead:true,color:INK.data,after:INK.data,turn:0,land:'none',passed:false});}
    }
    this.packets=flying.concat(spawned);
    this.settle();
    // A finished slide closes the crosspoint on its new row.
    for(const slide of this.slides)if(this.clock-slide.start>=SLIDE_S)this.flash(`x:${slide.key}`,1);
    this.slides=this.slides.filter((slide)=>this.clock-slide.start<SLIDE_S);
  }

  private spawn(packet: Omit<Packet,'passed'>) {
    this.packets.push({...packet,passed:false});
    if(this.packets.length>900)this.packets.splice(0,this.packets.length-900);
  }

  // Samples enter at the NOW line and run the whole row: the manager caches them whoever selects the source.
  private launchInput(row: Row) {
    const count=burstSize(row.delta)||3,{u}=this.view;
    for(let i=0;i<count;i++){
      const seed=++this.seq;
      this.spawn({track:row.line,key:row.key,owner:row.id,born:this.clock+(i+hash(seed)*.8)/count*.95,life:row.line.total/(420*u)*(.9+hash(seed*1.7)*.2),lead:i===0,color:INK.data,after:INK.data,turn:0,land:'none'});
    }
  }

  // Notifications follow the selected row, turn at the crosspoint and rise to the Session head.
  // With no source selected in this group they cannot be traced to a row, so only the head lights.
  private launchOutput(column: Column) {
    if(!column.route){this.flash(column.key,1);return;}
    const count=burstSize(column.delta)||3,{u}=this.view;
    for(let i=0;i<count;i++){
      const seed=++this.seq;
      this.spawn({track:column.path,key:column.key,owner:column.route,born:this.clock+OUTPUT_DELAY+(i+hash(seed)*.8)/count*.95,life:column.path.total/(460*u)*(.9+hash(seed*1.7)*.2),lead:i===0,color:column.row?INK.data:INK.out,after:INK.out,turn:column.turn,land:'head'});
    }
  }

  private launchCalc(column: Column) {
    for(let i=0;i<2;i++)this.spawn({track:column.path,key:column.key,owner:column.route,born:this.clock+i*.25,life:column.path.total/(460*this.view.u),lead:i===0,color:INK.data,after:INK.data,turn:column.turn,land:i===0?'calc':'none'});
  }

  // The manager boundary around the matrix, with a port on its top edge for every column.
  private frame(ctx: CanvasRenderingContext2D, live: boolean) {
    const {sx,sy,u}=this.view,x0=MX.frameLeft*sx,y0=MX.frameTop*sy,x1=MX.frameRight*sx,y1=MX.frameBottom*sy,cut=12*u,accent=live?INK.data:INK.stale,drawn=this.phase(0,.35);
    const perimeter=2*(x1-x0+y1-y0);
    ctx.beginPath();ctx.moveTo(x0+cut,y0);ctx.lineTo(x1,y0);ctx.lineTo(x1,y1-cut);ctx.lineTo(x1-cut,y1);ctx.lineTo(x0,y1);ctx.lineTo(x0,y0+cut);ctx.closePath();
    if(drawn<1)ctx.setLineDash([perimeter*drawn,perimeter]);
    ctx.strokeStyle=INK.rule;ctx.globalAlpha=.6;ctx.lineWidth=u;ctx.stroke();ctx.setLineDash([]);
    ctx.strokeStyle=accent;ctx.globalAlpha=.9*drawn;ctx.lineWidth=1.5*u;ctx.beginPath();
    ctx.moveTo(x0,y0+cut+16*u);ctx.lineTo(x0,y0+cut);ctx.lineTo(x0+cut,y0);ctx.lineTo(x0+cut+16*u,y0);
    ctx.moveTo(x1,y1-cut-16*u);ctx.lineTo(x1,y1-cut);ctx.lineTo(x1-cut,y1);ctx.lineTo(x1-cut-16*u,y1);ctx.stroke();
    for(const column of this.columns){ctx.globalAlpha=drawn;ctx.fillStyle=INK.plate;ctx.fillRect(column.x-3*u,y0-2*u,6*u,4*u);ctx.strokeStyle=INK.rule;ctx.lineWidth=u;ctx.strokeRect(column.x-3*u,y0-2*u,6*u,4*u);}
    // Decorative scan line sweeping across the matrix.
    const scan=x0+fract(this.clock*.05)*(x1-x0),beam=ctx.createLinearGradient(scan-80*u,0,scan,0);
    beam.addColorStop(0,withAlpha(accent,0));beam.addColorStop(1,withAlpha(accent,.03));
    ctx.globalAlpha=drawn;ctx.fillStyle=beam;ctx.fillRect(scan-80*u,y0,80*u,y1-y0);ctx.fillStyle=withAlpha(accent,.12);ctx.fillRect(scan-.5*u,y0,u,y1-y0);
  }

  // One waveform column per received snapshot, mirrored about the row baseline; height follows
  // the logarithm of the real sample count against the largest count on screen.
  private trace(ctx: CanvasRenderingContext2D, live: boolean) {
    const {sx,sy,u}=this.view,shown=this.phase(.1,.5);
    if(shown<=0||!this.rows.length)return;
    const left=MX.traceLeft*sx,right=MX.traceRight*sx,frames=this.frames,count=frames.length,pitch=TRACE_PITCH*sx,width=Math.max(1.2*u,pitch*.56);
    const since=this.clock-this.arrived,slide=this.motion?pitch*(1-easeOut(since/SCROLL_S)):0,fresh=this.motion&&live?clamp(1-since/.9,0,1):0;
    const {peak,ticks}=this.info,band=Math.min(22*sy,mxRowPitch(this.rows.length)*sy*.3),top=this.rows[0].y-band-18*sy,bottom=this.rows[this.rows.length-1].y+band+14*sy;
    // Each label card plugs into its row's baseline.
    for(const row of this.rows){ctx.globalAlpha=.8*this.weight(row.id)*shown;ctx.fillStyle=INK.rule;for(let x=MX.labelRight*sx+4*u;x<left;x+=4*u)ctx.fillRect(x,row.y-.5*u,1.5*u,u);}
    ctx.save();ctx.beginPath();ctx.rect(left,0,right-left,SCENE_HEIGHT*sy);ctx.clip();
    for(const row of this.rows){
      const weight=this.weight(row.id)*shown;
      ctx.globalAlpha=.9*weight;ctx.fillStyle=INK.line;ctx.fillRect(left,row.y-.5*u,right-left,u);
      frames.forEach((frame,index)=>{
        const cell=frame.sources[row.id],x=traceX(index,count)*sx+slide;
        if(!cell||x<left-pitch)return;
        const age=.45+.55*(index+1)/count;
        if(cell.tone==='down'){ctx.globalAlpha=.85*weight;ctx.fillStyle=live?INK.red:INK.stale;ctx.fillRect(x-width/2,row.y-.75*u,width,1.5*u);return;}
        if(cell.tone==='off'||!cell.delta){ctx.globalAlpha=.8*weight;ctx.fillStyle=cell.tone==='off'?INK.off:INK.rule;ctx.fillRect(x-.6*u,row.y-.6*u,1.2*u,1.2*u);return;}
        const v=Math.log10(1+cell.delta)/Math.log10(1+peak),h=1.5*u+v*band,color=!live?INK.stale:cell.tone==='warn'?INK.amber:INK.data;
        ctx.fillStyle=color;
        ctx.globalAlpha=(.18+.45*v)*age*weight;ctx.fillRect(x-width/2,row.y-h,width,h*2);
        ctx.globalAlpha=(.55+.45*v)*age*weight;ctx.fillRect(x-width/2,row.y-h,width,u);ctx.fillRect(x-width/2,row.y+h-u,width,u);
        if(index===count-1&&fresh>0){const size=h*3+10*u;ctx.globalAlpha=.5*fresh*weight;ctx.drawImage(this.glow(color),x-size/2,row.y-size/2,size,size);}
      });
    }
    ctx.strokeStyle=INK.rule;ctx.lineWidth=u;ctx.globalAlpha=.8*shown;ctx.beginPath();
    for(const {index,long} of ticks){const x=traceX(index,count)*sx+slide;ctx.moveTo(x,bottom);ctx.lineTo(x,bottom+(long?6:3)*u);}
    ctx.stroke();
    ctx.restore();
    for(const {index,label} of ticks){const x=traceX(index,count)*sx+slide;if(label&&x>left+24*u&&x<right-24*u)this.text(ctx,label,x,bottom+14*u,INK.muted,.75*shown,9,undefined,'center');}
    this.totals(ctx,live,slide,shown);
    // Where the recorded history starts: the page opened or the manager restarted less than a full trace ago.
    if(count&&count<TRACE_CELLS){
      const x=(traceX(0,count)-TRACE_PITCH/2)*sx+slide;
      ctx.globalAlpha=.7*shown;ctx.fillStyle=INK.rule;for(let y=top;y<bottom;y+=6*u)ctx.fillRect(x,y,u,3*u);
      this.text(ctx,'记录起点',x-5*u,top-8*u,INK.muted,.8*shown,8.5,undefined,'right');
    }
    // The NOW line: where each snapshot is written, flashing as it arrives.
    ctx.globalAlpha=(.45+.55*fresh)*shown;ctx.fillStyle=live?INK.data:INK.stale;ctx.fillRect(right+2*u,top,1.2*u,bottom-top);
    if(fresh>0){ctx.globalAlpha=.25*fresh*shown;ctx.fillRect(right-4*u,top,6*u,bottom-top);}
    this.text(ctx,'NOW',right+2.6*u,top-8*u,live?INK.data:INK.stale,.9*shown,8.5,undefined,'center');
  }

  // Totals over every source and Session per snapshot: samples as bars, notifications as a line,
  // each against its own peak on screen.
  private totals(ctx: CanvasRenderingContext2D, live: boolean, slide: number, shown: number) {
    const {sx,sy,u}=this.view,frames=this.frames,count=frames.length,left=MX.traceLeft*sx,right=MX.traceRight*sx,top=MX.sumTop*sy,base=MX.sumBottom*sy,width=Math.max(1.2*u,TRACE_PITCH*sx*.56);
    const {samples,total,notes}=this.info,peak=Math.max(1,total),notePeak=Math.max(1,notes);
    ctx.globalAlpha=.9*shown;ctx.fillStyle=INK.line;ctx.fillRect(left,base,right-left,u);
    for(const k of [1,2])for(let x=left;x<right;x+=5*u){ctx.globalAlpha=.6*shown;ctx.fillRect(x,base-(base-top)*k/3,1.5*u,u);}
    ctx.save();ctx.beginPath();ctx.rect(left,top-10*u,right-left,base-top+12*u);ctx.clip();
    samples.forEach((total,index)=>{
      if(!total)return;
      const x=traceX(index,count)*sx+slide,h=Math.max(1.5*u,total/peak*(base-top)),age=.45+.55*(index+1)/count;
      ctx.fillStyle=live?INK.data:INK.stale;ctx.globalAlpha=.32*age*shown;ctx.fillRect(x-width/2,base-h,width,h);ctx.globalAlpha=.9*age*shown;ctx.fillRect(x-width/2,base-h,width,u);
    });
    if(notes){
      ctx.beginPath();frames.forEach((frame,index)=>{const x=traceX(index,count)*sx+slide,y=base-frame.notes/notePeak*(base-top)*.92;if(index)ctx.lineTo(x,y);else ctx.moveTo(x,y);});
      ctx.strokeStyle=live?INK.out:INK.stale;ctx.lineWidth=1.3*u;ctx.globalAlpha=.85*shown;ctx.lineJoin='round';ctx.stroke();
    }
    ctx.restore();
    if(total)this.text(ctx,`峰值 +${total.toLocaleString()}`,right,top-8*u,INK.muted,.75*shown,8.5,undefined,'right');
  }

  private links(ctx: CanvasRenderingContext2D, live: boolean) {
    const {sy,u}=this.view,wired=this.phase(.25,.6),risen=this.phase(.45,.8),bused=this.phase(.7,1);
    // Column structure: every Session can select any row.
    for(const column of this.columns)this.stroke(ctx,partial(column.spine,risen),!live?INK.stale:column.attached?INK.line:INK.off,.9*this.weight(column.route),.8*u,[1*u,4*u]);
    for(const row of this.rows){
      const weight=this.weight(row.id),level=this.level(row.key),link=this.shift(row.key),alarm=this.shift(`alert:${row.id}`),t=partial(row.line,wired);
      if(!live||row.link==='off'){this.stroke(ctx,t,live?INK.off:INK.stale,.7*weight,u,[1.5*u,5*u]);continue;}
      if(row.link==='down'){
        // A broken link: a red stub at the entry; on a fresh break the lit row retracts into it.
        const p=link?.kind==='break'?(this.clock-link.start)/SHIFT_S.break:1;
        if(p<1)this.energise(ctx,partial(row.line,1-.94*easeOut(p)),INK.red,1-p,weight);
        const stub=partial(row.line,Math.min(1,60*u/row.line.total)*wired),[x,y]=stub.pts[stub.pts.length-1],s=3.5*u;
        this.stroke(ctx,t,INK.off,.5*weight,u,[1.5*u,6*u]);
        this.stroke(ctx,stub,INK.red,.8*weight,1.2*u,[4*u,4*u]);
        if(wired>=1){ctx.globalAlpha=weight;ctx.lineWidth=1.5*u;ctx.beginPath();ctx.moveTo(x-s,y-s);ctx.lineTo(x+s,y+s);ctx.moveTo(x+s,y-s);ctx.lineTo(x-s,y+s);ctx.stroke();}
        continue;
      }
      if(row.link==='warn')this.stroke(ctx,t,INK.amber,.6*weight,1.1*u,[4*u,4*u]);
      else{this.stroke(ctx,t,INK.line,weight,1.2*u);this.energise(ctx,t,INK.data,level,weight);}
      // A reconnected link lights up again from the entry across the matrix.
      if(link?.kind==='join'){const p=easeOut((this.clock-link.start)/SHIFT_S.join),lit=partial(row.line,p);this.energise(ctx,lit,INK.data,1,weight);this.comet(ctx,row.line,lit.total,60*u,'#ffffff',weight,2.4*u);}
      // A station turning abnormal (or recovering) sends an alarm pulse along the row.
      if(alarm){
        const p=(this.clock-alarm.start)/SHIFT_S[alarm.kind],color=alarm.kind==='warn'?INK.amber:INK.ok,fade=(1-p)**1.5;
        for(const [width,strength] of [[14,.2],[5,.35],[2,.9]])this.stroke(ctx,row.line,color,strength*fade*weight,width*u);
        this.comet(ctx,row.line,easeOut(p*1.4)*row.line.total,110*u,'#ffffff',fade*weight,3*u);
      }
    }
    for(const column of this.columns){
      const weight=this.weight(column.route),level=column.calc?this.level('calc'):this.level(column.key),riser=partial(column.riser,risen),color=column.calc?INK.data:INK.out;
      if(!column.route)continue;
      if(!live||!column.attached){this.stroke(ctx,riser,live?INK.off:INK.stale,.8*weight,u,[1.5*u,5*u]);continue;}
      if(!column.row){
        // Selected source on another page: a dashed riser from the column foot, labelled with the source.
        this.stroke(ctx,riser,INK.rule,weight,u,[3*u,4*u]);this.energise(ctx,riser,color,level,weight);
        this.text(ctx,`↑ ${column.route}`,column.x,this.bottom+12*sy,INK.muted,.85*weight*risen,8.5,90*u,'center');
        continue;
      }
      this.stroke(ctx,riser,INK.rule,weight,1.2*u);this.energise(ctx,riser,color,level,weight);
    }
    if(this.bus){
      const t=partial(this.bus,bused),calc=this.columns[this.columns.length-1];
      this.stroke(ctx,t,INK.line,1,1.1*u,[3*u,3*u]);this.energise(ctx,t,INK.data,live?this.level('calc')*.6:0,1);
      for(const column of this.columns)if(!column.calc){ctx.globalAlpha=bused;ctx.fillStyle=INK.rule;ctx.fillRect(column.x-2*u,this.bus.pts[0][1]-2*u,4*u,4*u);}
      this.text(ctx,'RESULT · 计算结果共享',calc.x-10*u,this.bus.pts[0][1]-9*u,INK.muted,.8*bused,8.5,undefined,'right');
    }
  }

  // Crosspoints, row entries and column-head sockets.
  private points(ctx: CanvasRenderingContext2D, live: boolean) {
    const {u}=this.view,shown=this.phase(.6,.9),head=MX.headY*this.view.sy;
    if(shown<=0)return;
    // Idle switch matrix: a small cross at every intersection nobody selected.
    ctx.strokeStyle=live?INK.rule:INK.stale;ctx.lineWidth=u;
    for(const row of this.rows)for(const column of this.columns){
      if(column.row===row)continue;
      const s=2.5*u;ctx.globalAlpha=.75*shown*this.weight(row.id)*this.weight(column.route);
      ctx.beginPath();ctx.moveTo(column.x-s,row.y);ctx.lineTo(column.x+s,row.y);ctx.moveTo(column.x,row.y-s);ctx.lineTo(column.x,row.y+s);ctx.stroke();
    }
    for(const row of this.rows){
      const [x,y]=row.line.pts[0],level=this.level(row.key),color=!live?INK.stale:row.link==='ok'?INK.data:row.link==='down'?INK.red:row.link==='warn'?INK.amber:INK.off,weight=this.weight(row.id)*shown;
      if(level>.01){const size=14*u;ctx.globalAlpha=.6*level*weight;ctx.drawImage(this.glow(color),x-size/2,y-size/2,size,size);}
      ctx.globalAlpha=weight;ctx.fillStyle=color;ctx.fillRect(x-1.5*u,y-1.5*u,3*u,3*u);
    }
    for(const column of this.columns){
      const weight=this.weight(column.route)*shown,level=column.calc?this.level('calc'):this.level(column.key),landed=this.flashes.get(column.calc?'calc':column.key)??0,closed=this.flashes.get(`x:${column.key}`)??0;
      const color=!live?INK.stale:!column.attached?INK.off:column.calc?INK.data:INK.out;
      // Head socket: lit while the Session generates notifications, flashing as each burst lands.
      if(landed>.02){const size=40*u*landed+10*u;ctx.globalAlpha=.5*landed*weight;ctx.drawImage(this.glow(color),column.x-size/2,head-size/2,size,size);}
      ctx.globalAlpha=weight;ctx.fillStyle=level>.01?color:INK.rule;ctx.fillRect(column.x-5*u,head-1.5*u,10*u,3*u);
      if(!column.row)continue;
      const slide=this.slides.find((item)=>item.key===column.key),p=slide?easeOut((this.clock-slide.start)/SLIDE_S):1,y=slide?slide.from+(slide.to-slide.from)*p:column.row.y;
      if(slide){const path=track([[column.x,slide.from],[column.x,slide.to]]);this.comet(ctx,path,p*path.total,Math.min(path.total,90*u),'#ffffff',(1-p*.6)*weight,2.4*u);}
      // Selected crosspoint: a diamond (a ring for the calculation feed), closing with a pulse as data turns.
      const s=(5+2*closed)*u,x=column.x;
      if(level+closed>.01){const size=(18+30*closed)*u;ctx.globalAlpha=Math.min(1,.45*level+.7*closed)*weight;ctx.drawImage(this.glow(color),x-size/2,y-size/2,size,size);}
      ctx.globalCompositeOperation='source-over';ctx.beginPath();
      if(column.calc)ctx.arc(x,y,s,0,Math.PI*2);else{ctx.moveTo(x,y-s);ctx.lineTo(x+s,y);ctx.lineTo(x,y+s);ctx.lineTo(x-s,y);ctx.closePath();}
      ctx.globalAlpha=Math.max(weight,.7*shown);ctx.fillStyle=INK.plate;ctx.fill();
      ctx.globalCompositeOperation='lighter';ctx.globalAlpha=weight;ctx.lineWidth=1.4*u;ctx.strokeStyle=level+closed<=.01&&!column.calc&&live&&column.attached?INK.muted:color;ctx.stroke();
      if(level>.01){ctx.globalAlpha=level*weight;ctx.fillStyle=color;ctx.fillRect(x-1.2*u,y-1.2*u,2.4*u,2.4*u);}
    }
  }

  private flight(ctx: CanvasRenderingContext2D) {
    const {u}=this.view,spark=this.glow('#ffffff'),shown=this.phase(.6,1);
    for(const packet of this.packets){
      const age=this.clock-packet.born;
      if(age<0||age>=packet.life)continue;
      const head=age/packet.life*packet.track.total,fade=Math.min(1,age*10,(packet.life-age)*8)*(packet.owner?this.weight(packet.owner):1)*shown,scale=packet.lead?1.3:1;
      const glow=this.glow(head<packet.turn?packet.color:packet.after);
      for(let k=9;k>=0;k--){
        const [x,y]=along(packet.track,head-k*4.5*u),size=13*u*scale*(1-k/10*.7);
        ctx.globalAlpha=fade*(1-k/10)**1.5*(k?.5:.9);ctx.drawImage(glow,x-size/2,y-size/2,size,size);
      }
      const [x,y]=along(packet.track,head),size=6.5*u*scale;
      ctx.globalAlpha=fade;ctx.drawImage(spark,x-size/2,y-size/2,size,size);
    }
  }

  // Static layer: dot lattice inside the manager frame and a ruler along its top edge.
  private backdropFor(width: number, height: number) {
    const key=`${width}x${height}`;
    if(this.backdrop&&this.backdropKey===key)return this.backdrop;
    const canvas=this.backdrop??sheet(width,height);
    canvas.width=width;canvas.height=height;this.backdrop=canvas;this.backdropKey=key;
    const g=canvas.getContext('2d');
    if(!g)return canvas;
    const {u,sx,sy}=this.view;
    g.fillStyle=INK.rule;g.globalAlpha=.4;
    for(let x=MX.frameLeft+30;x<MX.frameRight;x+=30)for(let y=MX.frameTop+30;y<MX.frameBottom;y+=30)g.fillRect(x*sx-.6*u,y*sy-.6*u,1.2*u,1.2*u);
    g.strokeStyle=INK.rule;g.lineWidth=u;g.globalAlpha=.8;g.beginPath();
    for(let y=MX.frameTop+30;y<MX.frameBottom;y+=15){const long=(y-MX.frameTop)%150===0;g.moveTo(MX.frameRight*sx,y*sy);g.lineTo(MX.frameRight*sx-(long?7:3)*u,y*sy);}
    g.stroke();
    return canvas;
  }
}
