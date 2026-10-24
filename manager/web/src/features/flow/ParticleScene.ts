import { portRows, sourceCanFlow, type Activity, type FlowPort, type FlowSession, type FlowSource } from './model';

export const SCENE_WIDTH = 1680, SCENE_HEIGHT = 750;
// Scene units. Cards and stage labels in FlowPage.css are placed with the same percentages:
// source cards end at CARD_RIGHT, the manager frame spans FRAME, client cards start just after EGRESS_X;
// the result bus runs along BUS_Y under the calculation label.
const CARD_RIGHT = 319, SLOT_X = 548, SLOT_W = 120, SLOT_H = 28, NODE_X = 1012, NODE_W = 120, NODE_H = 28, EGRESS_X = 1403, BUS_X = 1156;
const FRAME = { left: 500, top: 150, right: 1180, bottom: 650 }, CALC = { x: 840, y: 575, r: 22 }, BUS_Y = 622, RECENT_MS = 3200, OUTPUT_DELAY = .9;
// Slots and nodes stack compactly inside the frame; wires fan in from the cards and out to the clients.
const stackY = (index: number, count: number) => 375 + (index - (count - 1) / 2) * Math.min(80, 320 / Math.max(count - 1, 1));
// Structure is neutral slate; the accent only appears where real data is moving.
const INK = { data: '#59c7ff', out: '#8fb4ff', line: '#22324a', rule: '#3a4f68', amber: '#f0b45a', red: '#ff6f6f', off: '#56657a', idle: '#2c3b4f', stale: '#3a4656', text: '#e6edf5', muted: '#7d8ea3', plate: '#0a121c' };
export const laneY = (index: number, count: number) => 48 + (index + 0.5) * (654 / Math.max(count, 1));
const fract = (value: number) => value - Math.floor(value);
const clamp = (value: number, low: number, high: number) => Math.max(low, Math.min(high, value));
// Deterministic noise: paused and reduced-motion frames must be pixel-stable.
const hash = (seed: number) => fract(Math.sin(seed * 127.1 + 311.7) * 43758.5453);
const withAlpha = (hex: string, alpha: number) => hex + Math.round(clamp(alpha, 0, 1) * 255).toString(16).padStart(2, '0');
// Packets per snapshot follow the logarithm of real updates, so 10 and 10 000 samples both stay readable.
export const burstSize = (delta: number) => delta > 0 ? Math.min(18, 3 + Math.round(5 * Math.log10(1 + delta))) : 0;
type Point = [number,number];
// ok carries data; warn = every station on the port abnormal; idle = no station in normal communication.
type Tone = 'ok' | 'warn' | 'idle' | 'down' | 'off';
export interface SceneCalculation { active: boolean; good: number; bad: number; evaluated?: string }
export interface SceneState { sources: FlowSource[]; sessions: FlowSession[]; activity: Activity; live: boolean; paused: boolean; selected?: string; focus?: string; cacheActive?: boolean; group?: string; defaultSource?: string; calculation?: SceneCalculation; anchors?: Record<string, Point>; maxPorts?: number }
export interface SceneFrame { width: number; height: number; dt: number; motion: boolean; now: number }
interface View { sx: number; sy: number; u: number }
interface Box { x: number; y: number; w: number; h: number }
interface Track { pts: Point[]; acc: number[]; total: number }
interface Wire { track: Track; tone: Tone }
interface Slot { key: string; id: string; box: Box; wires: Wire[]; link: Tone; ready: boolean; stamp?: number; delta: number; running: boolean; selected: boolean; preferred: boolean }
interface Node { key: string; label: string; box: Box; route?: string; slot?: Slot; edge?: Track; path: Track; exit: Track; attached: boolean; ready: boolean; stamp?: number; delta: number; running: boolean }
interface Calc { center: Point; r: number; good: number; bad: number; feed?: Track; bus: Track; joins: Point[] }
// owner is the source id used for hover focus; via is the distance where an output packet enters its Session node.
interface Packet { track: Track; key: string; owner?: string; born: number; life: number; lead: boolean; color: string; land: 'slot' | 'client' | 'calc' | 'none'; via: number; passed: boolean }
interface Wave { start: number; color: string; at: Point }
interface Spark { x: number; y: number; vx: number; vy: number; born: number; color: string }

function bezier(a: Point,b: Point,c: Point,d: Point,t: number): Point {
  const u=1-t;
  return [u*u*u*a[0]+3*u*u*t*b[0]+3*u*t*t*c[0]+t*t*t*d[0],u*u*u*a[1]+3*u*u*t*b[1]+3*u*t*t*c[1]+t*t*t*d[1]];
}
// Horizontal-tangent S-curve, the shape every link in the scene uses.
const curve=(a: Point,d: Point,steps=28): Point[]=>{const mid=(a[0]+d[0])/2;return Array.from({length:steps+1},(_,i)=>bezier(a,[mid,a[1]],[mid,d[1]],d,i/steps));};
function track(pts: Point[]): Track {
  const acc=[0];
  for(let i=1;i<pts.length;i++)acc.push(acc[i-1]+Math.hypot(pts[i][0]-pts[i-1][0],pts[i][1]-pts[i-1][1]));
  return {pts,acc,total:acc[acc.length-1]};
}
function along(t: Track, distance: number): Point {
  const s=clamp(distance,0,t.total);let low=0,high=t.acc.length-1;
  while(high-low>1){const mid=(low+high)>>1;if(t.acc[mid]<=s)low=mid;else high=mid;}
  const span=t.acc[high]-t.acc[low]||1,k=(s-t.acc[low])/span,a=t.pts[low],b=t.pts[high];
  return [a[0]+(b[0]-a[0])*k,a[1]+(b[1]-a[1])*k];
}
function sheet(width: number, height: number) {
  const canvas=document.createElement('canvas');canvas.width=width;canvas.height=height;
  return canvas;
}
function glowSprite(color: string) {
  const sprite=sheet(64,64),g=sprite.getContext('2d');
  if(g){const glow=g.createRadialGradient(32,32,0,32,32,32);glow.addColorStop(0,'#ffffff');glow.addColorStop(.14,withAlpha(color,1));glow.addColorStop(.38,withAlpha(color,.3));glow.addColorStop(1,withAlpha(color,0));g.fillStyle=glow;g.fillRect(0,0,64,64);}
  return sprite;
}
const portTone=(ports: FlowPort[]): Tone=>ports.some((p)=>p.devices.some((d)=>d.state===1))?'ok':ports.some((p)=>p.devices.some((d)=>d.state===2||d.state===3))?'warn':'idle';
const toneInk=(tone: Tone,out=false)=>tone==='ok'?out?INK.out:INK.data:tone==='warn'?INK.amber:tone==='down'?INK.red:tone==='idle'?INK.idle:INK.off;

// Port wires run from each card row into the source's cache slot; each Session node
// is linked only to the slot it selected, then out to its client card.
function layout(state: SceneState, view: View) {
  const {sx,sy,u}=view,X=(x: number)=>x*sx,Y=(y: number)=>y*sy,n=Math.max(state.sources.length,1),m=Math.max(state.sessions.length,1);
  const slots=state.sources.map((source,index): Slot=>{
    const y=stackY(index,n),card=laneY(index,n),box={x:X(SLOT_X),y:Y(y)-SLOT_H*sy/2,w:SLOT_W*sx,h:SLOT_H*sy};
    const link: Tone=!source.enabled?'off':!source.connected?'down':source.catalog_state==='CatalogConflict'?'warn':'ok';
    const rows=portRows(source.ports,state.maxPorts??5),count=Math.max(rows.length,1);
    const wires=(rows.length?rows:[{key:'',ports:[]}]).map((row,k): Wire=>{
      const anchor=state.anchors?.[`${source.id}::${row.key}`]??[CARD_RIGHT,card+(k-(count-1)/2)*15];
      const a: Point=[X(anchor[0])+5*u,Y(anchor[1])],d: Point=[box.x,box.y+box.h/2+(k-(count-1)/2)*Math.min(4*sy,box.h/(count+1))];
      return {track:track(curve(a,d)),tone:link!=='ok'?link:portTone(row.ports)};
    });
    return {key:`in:${source.id}`,id:source.id,box,wires,link,ready:state.live&&sourceCanFlow(source),stamp:state.activity.sources[source.id],delta:state.activity.sourceDeltas[source.id]??0,running:false,selected:state.selected===source.id,preferred:source.id===state.defaultSource};
  });
  const nodes=state.sessions.map((session,index): Node=>{
    const y=stackY(index,m),box={x:X(NODE_X),y:Y(y)-NODE_H*sy/2,w:NODE_W*sx,h:NODE_H*sy},route=state.group?session.routes[state.group]||undefined:undefined,slot=slots.find((item)=>item.id===route);
    const enter: Point=[box.x,Y(y)],leave: Point=[box.x+box.w,Y(y)],exitPoints=curve(leave,[X(EGRESS_X),Y(laneY(index,m))]);
    const edgePoints=slot?curve([slot.box.x+slot.box.w,slot.box.y+slot.box.h/2],enter):undefined;
    return {key:`out:${session.id}`,label:session.name||`客户端 ${session.id.slice(0,6)}`,box,route,slot,edge:edgePoints&&track(edgePoints),path:track([...(edgePoints??[enter]),...exitPoints]),exit:track(exitPoints),
      attached:session.attached,ready:state.live&&session.attached,stamp:state.activity.sessions[session.id],delta:state.activity.sessionDeltas[session.id]??0,running:false};
  });
  let calc: Calc|undefined;
  if(state.calculation?.active){
    const center: Point=[X(CALC.x),Y(CALC.y)],r=CALC.r*Math.min(sx,sy),feeder=slots.find((slot)=>slot.preferred),top=nodes.length?Math.min(...nodes.map((node)=>node.box.y+node.box.h/2)):center[1];
    // The feed leaves the default slot downward and to the right, clear of the slots below it.
    const from: Point|undefined=feeder&&[feeder.box.x+feeder.box.w*.8,feeder.box.y+feeder.box.h],to: Point=[center[0]-r,center[1]];
    const feed=from&&track(Array.from({length:25},(_,i)=>bezier(from,[from[0]+50*sx,from[1]+60*sy],[to[0]-120*sx,to[1]],to,i/24)));
    calc={center,r,good:state.calculation.good,bad:state.calculation.bad,feed,bus:track([[center[0],center[1]+r],[center[0],Y(BUS_Y)],[X(BUS_X),Y(BUS_Y)],[X(BUS_X),top]]),joins:nodes.map((node)=>[X(BUS_X),node.box.y+node.box.h/2])};
  }
  const guides=slots.flatMap((slot)=>nodes.filter((node)=>node.slot!==slot).map((node)=>({owner:slot.id,track:track(curve([slot.box.x+slot.box.w,slot.box.y+slot.box.h/2],[node.box.x,node.box.y+node.box.h/2],16))})));
  return {slots,nodes,calc,guides};
}

// Packets are launched only when a snapshot shows new samples, Session notifications
// or a new calculation evaluation; their number follows that count. The snapshot has no
// per-port counters, so a source's packets are shared over its ports with a normal station.
// The frame scan line, matrix guides and drifting dust are decoration. A stale monitor
// greys out and clears every packet.
export class FlowScene {
  private clock=0; private seq=0; private focus?: string; private evaluated?: string; private calcAt=-1e9;
  private view: View={sx:1,sy:1,u:1};
  private slots: Slot[]=[]; private nodes: Node[]=[]; private calc?: Calc; private guides: {owner: string; track: Track}[]=[]; private shown?: SceneState; private shownSize='';
  private levels=new Map<string,number>(); private stamps=new Map<string,number>(); private flashes=new Map<string,number>();
  private packets: Packet[]=[]; private waves: Wave[]=[]; private sparks: Spark[]=[];
  private glows=new Map<string,HTMLCanvasElement>(); private backdrop?: HTMLCanvasElement; private backdropKey='';

  // Packets launched or still travelling.
  get inFlight() { return this.packets.length; }

  draw(ctx: CanvasRenderingContext2D, state: SceneState, frame: SceneFrame) {
    const size=`${frame.width}x${frame.height}`;
    if(state!==this.shown||size!==this.shownSize){
      const sx=frame.width/SCENE_WIDTH,sy=frame.height/SCENE_HEIGHT;
      this.view={sx,sy,u:Math.sqrt(sx*sy)};
      ({slots:this.slots,nodes:this.nodes,calc:this.calc,guides:this.guides}=layout(state,this.view));
      this.shown=state;this.shownSize=size;this.focus=state.focus;
    }
    this.advance(state,frame);
    ctx.setTransform(1,0,0,1,0,0);ctx.globalCompositeOperation='source-over';ctx.globalAlpha=1;
    ctx.clearRect(0,0,frame.width,frame.height);
    ctx.drawImage(this.backdropFor(frame.width,frame.height),0,0);
    ctx.globalCompositeOperation='lighter';
    this.frame(ctx,state.live);
    this.links(ctx,state.live);
    // Packets run under the slot and node plates, so they read as entering and leaving them.
    this.flight(ctx);
    this.boxes(ctx,state.live);
    this.ripples(ctx);
    ctx.setTransform(1,0,0,1,0,0);ctx.globalCompositeOperation='source-over';ctx.globalAlpha=1;
  }

  // Hovering a source or Session keeps its route bright and dims the rest.
  private weight(id?: string) { return !this.focus||id===this.focus?1:.25; }
  private level(key: string) { return this.levels.get(key)??0; }

  private advance(state: SceneState, {dt,motion,now}: SceneFrame) {
    // Fades run only while animating; paused or reduced-motion frames show the current state at once.
    const ease=(rate: number)=>motion&&state.live?1-Math.exp(-dt*rate):1,keys=new Set<string>(['calc']);
    this.clock+=dt;
    for(const [key,value] of this.flashes)this.flashes.set(key,value*Math.exp(-dt*3.5));
    const step=(flow: Slot|Node,launch: ()=>void)=>{
      flow.running=flow.ready&&!!flow.stamp&&now-flow.stamp<RECENT_MS;
      const old=this.level(flow.key),seen=this.stamps.get(flow.key);
      this.levels.set(flow.key,old+(Number(flow.running)-old)*ease(3));keys.add(flow.key);
      if(flow.stamp===undefined)return;
      this.stamps.set(flow.key,flow.stamp);
      if(motion&&flow.running&&flow.stamp>(seen??0))launch();
    };
    for(const slot of this.slots)step(slot,()=>this.launchInput(slot));
    for(const node of this.nodes)step(node,()=>this.launchOutput(node));
    // A changed evaluation timestamp is the only calculation activity the snapshot reports.
    const evaluated=state.calculation?.active?state.calculation.evaluated:undefined,feeder=this.slots.find((slot)=>slot.preferred);
    if(evaluated&&this.evaluated&&evaluated!==this.evaluated){this.calcAt=now;if(motion&&state.live&&this.calc?.feed&&feeder)this.launchCalc(this.calc.feed,feeder.id);}
    this.evaluated=evaluated;
    const calcLevel=this.level('calc');
    this.levels.set('calc',calcLevel+(Number(state.live&&!!this.calc&&now-this.calcAt<RECENT_MS)-calcLevel)*ease(3));
    for(const map of [this.levels,this.stamps])for(const key of map.keys())if(!keys.has(key))map.delete(key);
    if(!state.live){this.packets=[];this.waves=[];this.sparks=[];return;}
    if(!motion)return;
    const flying: Packet[]=[],spawned: Packet[]=[];
    for(const packet of this.packets){
      const age=this.clock-packet.born;
      if(packet.lead&&!packet.passed&&packet.via>0&&age>=packet.life*packet.via/packet.track.total){packet.passed=true;this.flash(packet.key,1);}
      if(age<packet.life){flying.push(packet);continue;}
      const end=packet.track.pts[packet.track.pts.length-1];
      if(packet.land==='slot')this.flash(packet.key,.35);
      // A landed evaluation sends one pulse up the shared result bus.
      if(packet.land==='calc'){this.flash('calc',1);if(this.calc)spawned.push({track:this.calc.bus,key:'calc',born:this.clock,life:this.calc.bus.total/(380*this.view.u),lead:true,color:INK.data,land:'none',via:0,passed:false});}
      if(packet.land==='client'&&packet.lead){this.waves.push({start:this.clock,color:packet.color,at:end});this.burst(end,packet.color,7);}
    }
    this.packets=flying.concat(spawned);
    this.waves=this.waves.filter((wave)=>this.clock-wave.start<1.2).slice(-32);
    this.sparks=this.sparks.filter((spark)=>this.clock-spark.born<.7).slice(-240);
  }

  private flash(key: string, amount: number) { this.flashes.set(key,Math.min(1,(this.flashes.get(key)??0)+amount)); }

  private spawn(packet: Omit<Packet,'passed'>) {
    this.packets.push({...packet,passed:false});
    if(this.packets.length>900)this.packets.splice(0,this.packets.length-900);
  }

  private launchInput(slot: Slot) {
    const ok=slot.wires.filter((wire)=>wire.tone==='ok'),carriers=ok.length?ok:slot.wires.filter((wire)=>wire.tone==='idle');
    if(!carriers.length)return;
    // Every carrying port wire gets part of the burst, spread over the next snapshot interval.
    const count=Math.max(burstSize(slot.delta)||3,carriers.length),{u}=this.view;
    for(let i=0;i<count;i++){
      const seed=++this.seq,wire=carriers[i%carriers.length];
      this.spawn({track:wire.track,key:slot.key,owner:slot.id,born:this.clock+(i+hash(seed)*.8)/count*.95,life:wire.track.total/(190*u)*(.9+hash(seed*1.7)*.2),lead:i===0,color:INK.data,land:'slot',via:0});
    }
  }

  // Outputs leave the selected source's slot once its inputs have landed.
  private launchOutput(node: Node) {
    const count=burstSize(node.delta)||3,{u}=this.view,via=node.edge?.total??0;
    for(let i=0;i<count;i++){
      const seed=++this.seq;
      this.spawn({track:node.path,key:node.key,owner:node.route,born:this.clock+OUTPUT_DELAY+(i+hash(seed)*.8)/count*.95,life:node.path.total/(340*u)*(.9+hash(seed*1.7)*.2),lead:i===0,color:INK.out,land:'client',via});
    }
  }

  private launchCalc(feed: Track, source: string) {
    for(let i=0;i<2;i++)this.spawn({track:feed,key:`in:${source}`,owner:source,born:this.clock+i*.25,life:feed.total/(240*this.view.u),lead:i===0,color:INK.data,land:i===0?'calc':'none',via:0});
  }

  private burst([x,y]: Point, color: string, count: number) {
    const {u}=this.view;
    for(let i=0;i<count;i++){
      const seed=++this.seq,angle=hash(seed*3.1)*Math.PI*2,speed=(60+hash(seed*4.7)*110)*u;
      this.sparks.push({x,y,vx:Math.cos(angle)*speed,vy:Math.sin(angle)*speed,born:this.clock,color});
    }
  }

  private stroke(ctx: CanvasRenderingContext2D, t: Track, color: string, alpha: number, width: number, dash?: number[]) {
    ctx.beginPath();ctx.moveTo(...t.pts[0]);for(let i=1;i<t.pts.length;i++)ctx.lineTo(...t.pts[i]);
    if(dash)ctx.setLineDash(dash);
    ctx.strokeStyle=color;ctx.globalAlpha=alpha;ctx.lineWidth=width;ctx.stroke();
    if(dash)ctx.setLineDash([]);
  }

  // A live link glows and carries current markers toward the clients.
  private energise(ctx: CanvasRenderingContext2D, t: Track, color: string, level: number, weight: number) {
    const {u}=this.view;
    if(level<.01)return;
    for(const [width,strength] of [[10,.06],[4,.14],[1.4,.85]])this.stroke(ctx,t,color,strength*level*weight,width*u);
    ctx.lineDashOffset=-this.clock*80*u;this.stroke(ctx,t,'#ffffff',.45*level*weight,1.3*u,[1.5*u,14*u]);ctx.lineDashOffset=0;
  }

  // The manager boundary: everything inside is the current OPC manager.
  private frame(ctx: CanvasRenderingContext2D, live: boolean) {
    const {sx,sy,u}=this.view,x0=FRAME.left*sx,y0=FRAME.top*sy,x1=FRAME.right*sx,y1=FRAME.bottom*sy,cut=12*u,accent=live?INK.data:INK.stale;
    ctx.beginPath();ctx.moveTo(x0+cut,y0);ctx.lineTo(x1,y0);ctx.lineTo(x1,y1-cut);ctx.lineTo(x1-cut,y1);ctx.lineTo(x0,y1);ctx.lineTo(x0,y0+cut);ctx.closePath();
    ctx.strokeStyle=INK.rule;ctx.globalAlpha=.6;ctx.lineWidth=u;ctx.stroke();
    // Accent marks only on the two chamfered corners.
    ctx.strokeStyle=accent;ctx.globalAlpha=.9;ctx.lineWidth=1.5*u;ctx.beginPath();
    ctx.moveTo(x0,y0+cut+16*u);ctx.lineTo(x0,y0+cut);ctx.lineTo(x0+cut,y0);ctx.lineTo(x0+cut+16*u,y0);
    ctx.moveTo(x1,y1-cut-16*u);ctx.lineTo(x1,y1-cut);ctx.lineTo(x1-cut,y1);ctx.lineTo(x1-cut-16*u,y1);ctx.stroke();
    // Decorative scan line sweeping the frame.
    const scan=y0+fract(this.clock*.06)*(y1-y0),beam=ctx.createLinearGradient(0,scan-70*u,0,scan);
    beam.addColorStop(0,withAlpha(accent,0));beam.addColorStop(1,withAlpha(accent,.03));
    const edge=ctx.createLinearGradient(x0,0,x1,0);edge.addColorStop(0,withAlpha(accent,0));edge.addColorStop(.5,withAlpha(accent,.16));edge.addColorStop(1,withAlpha(accent,0));
    ctx.globalAlpha=1;ctx.fillStyle=beam;ctx.fillRect(x0,scan-70*u,x1-x0,70*u);ctx.fillStyle=edge;ctx.fillRect(x0,scan-.5*u,x1-x0,u);
  }

  private links(ctx: CanvasRenderingContext2D, live: boolean) {
    const {u}=this.view;
    // Every possible source → Session route, faint like an idle switch matrix.
    for(const guide of this.guides)this.stroke(ctx,guide.track,INK.line,.7*this.weight(guide.owner),.8*u,[1*u,5*u]);
    for(const slot of this.slots){
      const level=this.level(slot.key),weight=this.weight(slot.id);
      slot.wires.forEach((wire,index)=>{
        if(!live||wire.tone==='off'){this.stroke(ctx,wire.track,live?INK.off:INK.stale,.7*weight,u,[1.5*u,5*u]);return;}
        if(wire.tone==='down'){
          // A broken link: the red stub stops short of the cache.
          const stub=track(wire.track.pts.slice(0,Math.ceil(wire.track.pts.length*.55)));
          this.stroke(ctx,stub,INK.red,.7*weight,1.2*u,[4*u,4*u]);
          if(index===slot.wires.length>>1){const [x,y]=stub.pts[stub.pts.length-1],s=3.5*u;ctx.globalAlpha=weight;ctx.lineWidth=1.5*u;ctx.beginPath();ctx.moveTo(x-s,y-s);ctx.lineTo(x+s,y+s);ctx.moveTo(x+s,y-s);ctx.lineTo(x-s,y+s);ctx.stroke();}
          return;
        }
        if(wire.tone==='warn'){this.stroke(ctx,wire.track,INK.amber,.6*weight,1.1*u,[4*u,4*u]);return;}
        if(wire.tone==='idle'){this.stroke(ctx,wire.track,INK.idle,weight,u);return;}
        this.stroke(ctx,wire.track,slot.selected?INK.rule:INK.line,weight,1.2*u);
        this.energise(ctx,wire.track,INK.data,level,weight);
      });
    }
    for(const node of this.nodes){
      const level=this.level(node.key),weight=this.weight(node.route);
      if(!live||!node.attached){const color=live?INK.off:INK.stale;if(node.edge)this.stroke(ctx,node.edge,color,.7*weight,u,[1.5*u,5*u]);this.stroke(ctx,node.exit,color,.7*weight,u,[1.5*u,5*u]);continue;}
      if(node.edge){this.stroke(ctx,node.edge,INK.line,weight,1.3*u);this.energise(ctx,node.edge,INK.out,level,weight);}
      this.stroke(ctx,node.exit,INK.line,weight,1.3*u);this.energise(ctx,node.exit,INK.out,level,weight);
    }
    if(this.calc){
      const level=this.level('calc'),feeder=this.slots.find((slot)=>slot.preferred),weight=this.weight(feeder?.id);
      if(this.calc.feed){this.stroke(ctx,this.calc.feed,INK.line,weight,1.1*u,[3*u,3*u]);this.energise(ctx,this.calc.feed,INK.data,live?level:0,weight);}
      this.stroke(ctx,this.calc.bus,INK.line,1,1.1*u,[3*u,3*u]);this.energise(ctx,this.calc.bus,INK.data,live?level*.6:0,1);
      for(const [x,y] of this.calc.joins){ctx.globalAlpha=1;ctx.fillStyle=INK.rule;ctx.fillRect(x-2*u,y-2*u,4*u,4*u);}
    }
    // Card-side plug of every port wire.
    for(const slot of this.slots){
      const level=this.level(slot.key),weight=this.weight(slot.id);
      for(const wire of slot.wires){
        const [x,y]=wire.track.pts[0],color=live?toneInk(wire.tone):INK.stale;
        if(wire.tone==='ok'&&level>.01){const size=12*u;ctx.globalAlpha=.6*level*weight;ctx.drawImage(this.glow(color),x-size/2,y-size/2,size,size);}
        ctx.globalAlpha=weight;ctx.fillStyle=color;ctx.fillRect(x-1.5*u,y-1.5*u,3*u,3*u);
      }
    }
  }

  // Slot and node plates: neutral until data arrives, then the accent edge and left bar light up.
  private plate(ctx: CanvasRenderingContext2D, box: Box, color: string, level: number, flash: number, weight: number) {
    const {u}=this.view,cut=5*u,{x,y,w,h}=box;
    const outline=()=>{ctx.beginPath();ctx.moveTo(x+cut,y);ctx.lineTo(x+w,y);ctx.lineTo(x+w,y+h-cut);ctx.lineTo(x+w-cut,y+h);ctx.lineTo(x,y+h);ctx.lineTo(x,y+cut);ctx.closePath();};
    ctx.globalCompositeOperation='source-over';outline();
    ctx.globalAlpha=.96*Math.max(weight,.7);ctx.fillStyle=INK.plate;ctx.fill();
    ctx.globalAlpha=weight;ctx.strokeStyle=INK.rule;ctx.lineWidth=u;ctx.stroke();
    ctx.globalCompositeOperation='lighter';
    if(level+flash>.01){outline();ctx.globalAlpha=Math.min(1,.55*level+.6*flash)*weight;ctx.strokeStyle=color;ctx.stroke();}
    if(flash>.02){const halo=Math.max(w,h)*1.3;ctx.globalAlpha=.3*flash*weight;ctx.drawImage(this.glow(color),x+w/2-halo/2,y+h/2-halo/2,halo,halo);}
    ctx.globalAlpha=weight;ctx.fillStyle=level>.01?color:INK.rule;ctx.fillRect(x,y+cut,1.5*u,h-cut*2);
  }

  private text(ctx: CanvasRenderingContext2D, value: string, x: number, y: number, color: string, alpha: number, size: number, max?: number, align: CanvasTextAlign='left') {
    const {u}=this.view;
    ctx.globalCompositeOperation='source-over';ctx.font=`500 ${Math.max(9,Math.round(size*u))}px ui-monospace, "SF Mono", "JetBrains Mono", Consolas, monospace`;ctx.textBaseline='middle';ctx.textAlign=align;
    let label=value;
    if(max)while(label.length>1&&ctx.measureText(label).width>max)label=label.slice(0,-1);
    ctx.globalAlpha=alpha;ctx.fillStyle=color;ctx.fillText(label===value?value:`${label.slice(0,-1)}…`,x,y);
    ctx.globalCompositeOperation='lighter';
  }

  private boxes(ctx: CanvasRenderingContext2D, live: boolean) {
    const {u}=this.view;
    for(const slot of this.slots){
      const level=this.level(slot.key),flash=this.flashes.get(slot.key)??0,weight=this.weight(slot.id),color=live?toneInk(slot.link):INK.stale,{x,y,w,h}=slot.box;
      this.plate(ctx,slot.box,color,level,flash,weight);
      const metered=slot.link==='ok'||slot.link==='warn';
      this.text(ctx,slot.id,x+10*u,y+h/2,live?INK.text:INK.stale,weight,11,w-(metered?46:16)*u);
      // Five-segment meter: lit while samples arrive; absent when the source cannot deliver.
      if(metered)for(let i=0;i<5;i++){ctx.globalAlpha=weight;ctx.fillStyle=i<Math.round(level*5)?color:INK.line;ctx.fillRect(x+w-(34-i*6)*u,y+h/2-4*u+(4-i)*.6*u,3*u,8*u-(4-i)*1.2*u);}
      if(slot.preferred)this.text(ctx,'DEFAULT · 默认来源',x+1*u,y-8*u,live?INK.data:INK.stale,.85*weight,8.5);
    }
    for(const node of this.nodes){
      const level=this.level(node.key),flash=this.flashes.get(node.key)??0,weight=this.weight(node.route),color=live&&node.attached?INK.out:live?INK.off:INK.stale,{x,y,w,h}=node.box;
      this.plate(ctx,node.box,color,level,flash,weight);
      this.text(ctx,node.label,x+10*u,y+h/2,live?INK.text:INK.stale,weight,11,w-18*u);
      // Selected source shown on the node; an off-page selection gets a dashed stub.
      const route=node.route?`← ${node.route}${node.slot?'':' · 其他页'}`:'未选择来源';
      this.text(ctx,route,x+1*u,y-8*u,INK.muted,.9*weight,8.5);
      if(node.route&&!node.slot)this.stroke(ctx,track([[x-60*u,y+h/2],[x,y+h/2]]),INK.rule,weight,u,[3*u,4*u]);
    }
    if(this.calc){
      const {center:[cx,cy],r,good,bad}=this.calc,level=this.level('calc'),flash=this.flashes.get('calc')??0,color=live?INK.data:INK.stale,total=Math.max(1,good+bad),start=-Math.PI/2,split=start+Math.PI*2*good/total;
      ctx.globalCompositeOperation='source-over';ctx.globalAlpha=.96;ctx.fillStyle=INK.plate;ctx.beginPath();ctx.arc(cx,cy,r+3*u,0,Math.PI*2);ctx.fill();
      ctx.globalCompositeOperation='lighter';
      if(flash>.02){const halo=r*4;ctx.globalAlpha=.35*flash;ctx.drawImage(this.glow(color),cx-halo/2,cy-halo/2,halo,halo);}
      // Tick ring, then the real Good / other split as a gauge.
      ctx.strokeStyle=INK.rule;ctx.lineWidth=u;ctx.globalAlpha=.9;ctx.beginPath();
      for(let i=0;i<48;i++){const a=i/48*Math.PI*2,outer=r+(i%12?6:9)*u;ctx.moveTo(cx+Math.cos(a)*(r+4*u),cy+Math.sin(a)*(r+4*u));ctx.lineTo(cx+Math.cos(a)*outer,cy+Math.sin(a)*outer);}
      ctx.stroke();
      ctx.lineWidth=2.5*u;ctx.strokeStyle=INK.line;ctx.globalAlpha=1;ctx.beginPath();ctx.arc(cx,cy,r,0,Math.PI*2);ctx.stroke();
      ctx.strokeStyle=color;ctx.beginPath();ctx.arc(cx,cy,r,start,split);ctx.stroke();
      if(bad){ctx.strokeStyle=live?INK.amber:INK.stale;ctx.beginPath();ctx.arc(cx,cy,r,split,start+Math.PI*2);ctx.stroke();}
      ctx.strokeStyle=level>.01?color:INK.rule;ctx.lineWidth=.8*u;ctx.globalAlpha=.5+.4*level;ctx.setLineDash([1.5*u,3.5*u]);ctx.lineDashOffset=-this.clock*10*u;ctx.beginPath();ctx.arc(cx,cy,r*.66,0,Math.PI*2);ctx.stroke();ctx.setLineDash([]);ctx.lineDashOffset=0;
      this.text(ctx,`${Math.round(good/total*100)}%`,cx,cy,live?INK.text:INK.stale,1,10.5,undefined,'center');
    }
  }

  private flight(ctx: CanvasRenderingContext2D) {
    const {u}=this.view,spark=this.glow('#ffffff');
    for(const packet of this.packets){
      const age=this.clock-packet.born;
      if(age<0||age>=packet.life)continue;
      const head=age/packet.life*packet.track.total,fade=Math.min(1,age*10,(packet.life-age)*8)*(packet.owner?this.weight(packet.owner):1),scale=packet.lead?1.3:1,glow=this.glow(packet.color);
      for(let k=9;k>=0;k--){
        const [x,y]=along(packet.track,head-k*4.5*u),size=13*u*scale*(1-k/10*.7);
        ctx.globalAlpha=fade*(1-k/10)**1.5*(k?.5:.9);ctx.drawImage(glow,x-size/2,y-size/2,size,size);
      }
      const [x,y]=along(packet.track,head),size=6.5*u*scale;
      ctx.globalAlpha=fade;ctx.drawImage(spark,x-size/2,y-size/2,size,size);
    }
  }

  private ripples(ctx: CanvasRenderingContext2D) {
    const {u}=this.view;
    for(const spark of this.sparks){
      const age=this.clock-spark.born,p=age/.7,drag=1-p*.5,x=spark.x+spark.vx*age*drag,y=spark.y+spark.vy*age*drag,size=(5-3*p)*u;
      ctx.globalAlpha=(1-p)**1.5*.8;ctx.strokeStyle=spark.color;ctx.lineWidth=u;
      ctx.beginPath();ctx.moveTo(x-spark.vx*.04,y-spark.vy*.04);ctx.lineTo(x,y);ctx.stroke();
      ctx.drawImage(this.glow(spark.color),x-size/2,y-size/2,size,size);
    }
    for(const wave of this.waves){
      const p=(this.clock-wave.start)/1.2;
      if(p<0||p>=1)continue;
      ctx.strokeStyle=wave.color;ctx.globalAlpha=(1-p)**2*.7;ctx.lineWidth=1.4*u*(1-p*.5);
      ctx.beginPath();ctx.arc(wave.at[0],wave.at[1],(5+p*34)*u,0,Math.PI*2);ctx.stroke();
    }
  }

  // Static layer: dot grid and ruler ticks inside the manager frame.
  private backdropFor(width: number, height: number) {
    const key=`${width}x${height}`;
    if(this.backdrop&&this.backdropKey===key)return this.backdrop;
    const canvas=this.backdrop??sheet(width,height);
    canvas.width=width;canvas.height=height;this.backdrop=canvas;this.backdropKey=key;
    const g=canvas.getContext('2d');
    if(!g)return canvas;
    const {u,sx,sy}=this.view;
    g.fillStyle=INK.rule;g.globalAlpha=.45;
    for(let x=FRAME.left+30;x<FRAME.right;x+=30)for(let y=FRAME.top+30;y<FRAME.bottom;y+=30)g.fillRect(x*sx-.6*u,y*sy-.6*u,1.2*u,1.2*u);
    g.strokeStyle=INK.rule;g.lineWidth=u;g.globalAlpha=.8;g.beginPath();
    for(let x=FRAME.left+30;x<FRAME.right;x+=15){const long=(x-FRAME.left)%150===0;g.moveTo(x*sx,FRAME.top*sy);g.lineTo(x*sx,FRAME.top*sy+(long?7:3)*u);}
    for(let y=FRAME.top+30;y<FRAME.bottom;y+=15){const long=(y-FRAME.top)%150===0;g.moveTo(FRAME.right*sx,y*sy);g.lineTo(FRAME.right*sx-(long?7:3)*u,y*sy);}
    g.stroke();
    return canvas;
  }

  private glow(color: string): HTMLCanvasElement {
    let sprite=this.glows.get(color);
    if(!sprite){sprite=glowSprite(color);this.glows.set(color,sprite);}
    return sprite;
  }
}
