import { portRows, sourceCanFlow, type Activity, type FlowPort, type FlowSession, type FlowSource } from './model';
import { along, burstSize, clamp, easeOut, fract, hash, INK, partial, RECENT_MS, SCENE_HEIGHT, SCENE_WIDTH, SceneBase, sheet, track, withAlpha, type Point, type SceneCalculation, type SceneFrame, type Tone, type Track, type View } from './sceneKit';
export { burstSize } from './sceneKit';

// Scene units. Cards and stage labels in FlowPage.css are placed with the same percentages:
// source cards end at CARD_RIGHT, the manager frame spans FRAME, client cards start just after EGRESS_X;
// the result bus runs along BUS_Y under the calculation label.
const CARD_RIGHT = 319, SLOT_X = 548, SLOT_W = 120, SLOT_H = 28, NODE_X = 1012, NODE_W = 120, NODE_H = 28, EGRESS_X = 1403, BUS_X = 1156;
const FRAME = { left: 500, top: 150, right: 1180, bottom: 650 }, CALC = { x: 840, y: 575, r: 22 }, BUS_Y = 622, OUTPUT_DELAY = .9, BOOT_S = 2.2;
// Slots and nodes stack compactly inside the frame; wires fan in from the cards and out to the clients.
const stackY = (index: number, count: number) => 375 + (index - (count - 1) / 2) * Math.min(80, 320 / Math.max(count - 1, 1));
export const laneY = (index: number, count: number) => 48 + (index + 0.5) * (654 / Math.max(count, 1));
export interface SceneState { sources: FlowSource[]; sessions: FlowSession[]; activity: Activity; live: boolean; paused: boolean; selected?: string; focus?: string; cacheActive?: boolean; group?: string; defaultSource?: string; calculation?: SceneCalculation; anchors?: Record<string, Point>; maxPorts?: number }
interface Box { x: number; y: number; w: number; h: number }
// alerts counts the port's stations in an abnormal or offline state.
interface Wire { key: string; track: Track; tone: Tone; alerts: number }
interface Slot { key: string; id: string; box: Box; wires: Wire[]; link: Tone; ready: boolean; stamp?: number; delta: number; running: boolean; selected: boolean; preferred: boolean }
interface Node { key: string; label: string; box: Box; route?: string; slot?: Slot; edge?: Track; path: Track; exit: Track; attached: boolean; ready: boolean; stamp?: number; delta: number; running: boolean }
interface Calc { center: Point; r: number; good: number; bad: number; feed?: Track; bus: Track; joins: Point[] }
// owner is the source id used for hover focus; via is the distance where an output packet enters its Session node.
interface Packet { track: Track; key: string; owner?: string; born: number; life: number; lead: boolean; color: string; land: 'slot' | 'client' | 'calc' | 'none'; via: number; passed: boolean }
// A real state change between snapshots: a station turning abnormal or recovering, an OPC link breaking or joining.
interface Shift { key: string; kind: 'warn' | 'recover' | 'break' | 'join'; start: number; landed?: boolean }
const SHIFT_S = { warn: 1.6, recover: 1.2, break: .8, join: 1 };

function bezier(a: Point,b: Point,c: Point,d: Point,t: number): Point {
  const u=1-t;
  return [u*u*u*a[0]+3*u*u*t*b[0]+3*u*t*t*c[0]+t*t*t*d[0],u*u*u*a[1]+3*u*u*t*b[1]+3*u*t*t*c[1]+t*t*t*d[1]];
}
// Horizontal-tangent S-curve, the shape every link in the scene uses.
const curve=(a: Point,d: Point,steps=28): Point[]=>{const mid=(a[0]+d[0])/2;return Array.from({length:steps+1},(_,i)=>bezier(a,[mid,a[1]],[mid,d[1]],d,i/steps));};
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
      return {key:`${source.id}::${row.key}`,track:track(curve(a,d)),tone:link!=='ok'?link:portTone(row.ports),alerts:row.ports.reduce((total,port)=>total+port.devices.filter((d)=>d.state===2||d.state===3||d.state===5).length,0)};
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
// Alarm and link effects follow real changes between snapshots. The opening sequence,
// frame scan line and matrix guides are decoration. A stale monitor greys out and
// clears every packet and effect.
export class FlowScene extends SceneBase {
  private focus?: string; private evaluated?: string; private calcAt=-1e9; private bootClock=0; private booted=false; private reveal=1;
  private slots: Slot[]=[]; private nodes: Node[]=[]; private calc?: Calc; private guides: {owner: string; track: Track}[]=[]; private shown?: SceneState; private shownSize='';
  private levels=new Map<string,number>(); private stamps=new Map<string,number>(); private flashes=new Map<string,number>(); private memory=new Map<string,string>();
  private packets: Packet[]=[]; private shifts: Shift[]=[];
  private backdrop?: HTMLCanvasElement; private backdropKey='';

  // Packets launched or still travelling.
  get inFlight() { return this.packets.length; }

  draw(ctx: CanvasRenderingContext2D, state: SceneState, frame: SceneFrame) {
    const size=`${frame.width}x${frame.height}`;
    if(state!==this.shown||size!==this.shownSize){
      const sx=frame.width/SCENE_WIDTH,sy=frame.height/SCENE_HEIGHT;
      this.view={sx,sy,u:Math.sqrt(sx*sy)};
      ({slots:this.slots,nodes:this.nodes,calc:this.calc,guides:this.guides}=layout(state,this.view));
      this.shown=state;this.shownSize=size;this.focus=state.focus;
      this.notice(state.live&&frame.motion);
    }
    this.advance(state,frame);
    ctx.setTransform(1,0,0,1,0,0);ctx.globalCompositeOperation='source-over';ctx.globalAlpha=1;
    ctx.clearRect(0,0,frame.width,frame.height);
    ctx.globalAlpha=this.phase(0,.3);ctx.drawImage(this.backdropFor(frame.width,frame.height),0,0);
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
  // Opening sequence progress between two points of the reveal, eased.
  private phase(from: number, to: number) { return easeOut((this.reveal-from)/(to-from)); }
  private shift(key: string) { return this.shifts.find((item)=>item.key===key); }

  // Compare link and station states with the previous snapshot and start the matching effect.
  private notice(active: boolean) {
    const seen=new Set<string>();
    const mark=(key: string,value: string,kind: (old: string)=>Shift['kind']|undefined,at?: Point,count=0)=>{
      seen.add(key);
      const old=this.memory.get(key);this.memory.set(key,value);
      const change=active&&old!==undefined&&old!==value?kind(old):undefined;
      if(!change)return;
      this.shifts.push({key,kind:change,start:this.clock});
      // Packets already travelling on a link that just broke would cross the break, so they are dropped.
      if(change==='break')this.packets=this.packets.filter((packet)=>packet.key!==key);
      const color=change==='warn'?INK.amber:change==='break'?INK.red:change==='recover'?INK.ok:INK.data;
      if(at){this.waves.push({start:this.clock,color,at});if(count)this.burst(at,color,count);}
    };
    for(const slot of this.slots){
      const stub=slot.wires[slot.wires.length>>1];
      mark(slot.key,slot.link,(old)=>slot.link==='down'&&old!=='down'?'break':old==='down'&&slot.link==='ok'?'join':undefined,stub&&along(stub.track,stub.track.total*.55),slot.link==='down'?8:0);
      for(const wire of slot.wires)mark(wire.key,String(wire.alerts),(old)=>wire.alerts>Number(old)?'warn':wire.alerts<Number(old)?'recover':undefined,wire.track.pts[0]);
    }
    for(const key of this.memory.keys())if(!seen.has(key))this.memory.delete(key);
  }

  private advance(state: SceneState, {dt,motion,now}: SceneFrame) {
    // Fades run only while animating; paused or reduced-motion frames show the current state at once.
    const ease=(rate: number)=>motion&&state.live?1-Math.exp(-dt*rate):1,keys=new Set<string>(['calc']);
    this.clock+=dt;
    // The opening sequence plays once; a still frame skips straight to the finished scene.
    if(!motion)this.booted=true;
    // It starts with the first snapshot that has something to show.
    if(this.slots.length||this.nodes.length)this.bootClock+=dt;
    this.reveal=this.booted?1:clamp(this.bootClock/BOOT_S,0,1);
    if(this.reveal>=1)this.booted=true;
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
    if(!state.live){this.packets=[];this.waves=[];this.sparks=[];this.shifts=[];return;}
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
    // A rejoined link flashes its cache slot as the light reaches it.
    for(const shift of this.shifts)if(shift.kind==='join'&&!shift.landed&&this.clock-shift.start>=SHIFT_S.join*.85){shift.landed=true;this.flash(shift.key,1);}
    this.packets=flying.concat(spawned);
    this.waves=this.waves.filter((wave)=>this.clock-wave.start<1.2).slice(-32);
    this.sparks=this.sparks.filter((spark)=>this.clock-spark.born<.7).slice(-240);
    this.shifts=this.shifts.filter((shift)=>this.clock-shift.start<SHIFT_S[shift.kind]).slice(-40);
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

  // The manager boundary: everything inside is the current OPC manager.
  private frame(ctx: CanvasRenderingContext2D, live: boolean) {
    const {sx,sy,u}=this.view,x0=FRAME.left*sx,y0=FRAME.top*sy,x1=FRAME.right*sx,y1=FRAME.bottom*sy,cut=12*u,accent=live?INK.data:INK.stale,drawn=this.phase(0,.35);
    const perimeter=2*(x1-x0+y1-y0);
    ctx.beginPath();ctx.moveTo(x0+cut,y0);ctx.lineTo(x1,y0);ctx.lineTo(x1,y1-cut);ctx.lineTo(x1-cut,y1);ctx.lineTo(x0,y1);ctx.lineTo(x0,y0+cut);ctx.closePath();
    // The outline draws itself in during the opening sequence.
    if(drawn<1)ctx.setLineDash([perimeter*drawn,perimeter]);
    ctx.strokeStyle=INK.rule;ctx.globalAlpha=.6;ctx.lineWidth=u;ctx.stroke();ctx.setLineDash([]);
    // Accent marks only on the two chamfered corners.
    ctx.strokeStyle=accent;ctx.globalAlpha=.9*drawn;ctx.lineWidth=1.5*u;ctx.beginPath();
    ctx.moveTo(x0,y0+cut+16*u);ctx.lineTo(x0,y0+cut);ctx.lineTo(x0+cut,y0);ctx.lineTo(x0+cut+16*u,y0);
    ctx.moveTo(x1,y1-cut-16*u);ctx.lineTo(x1,y1-cut);ctx.lineTo(x1-cut,y1);ctx.lineTo(x1-cut-16*u,y1);ctx.stroke();
    // Decorative scan line sweeping the frame.
    const scan=y0+fract(this.clock*.06)*(y1-y0),beam=ctx.createLinearGradient(0,scan-70*u,0,scan);
    beam.addColorStop(0,withAlpha(accent,0));beam.addColorStop(1,withAlpha(accent,.03));
    const edge=ctx.createLinearGradient(x0,0,x1,0);edge.addColorStop(0,withAlpha(accent,0));edge.addColorStop(.5,withAlpha(accent,.16));edge.addColorStop(1,withAlpha(accent,0));
    ctx.globalAlpha=drawn;ctx.fillStyle=beam;ctx.fillRect(x0,scan-70*u,x1-x0,70*u);ctx.fillStyle=edge;ctx.fillRect(x0,scan-.5*u,x1-x0,u);
  }

  private links(ctx: CanvasRenderingContext2D, live: boolean) {
    const {u}=this.view,wired=this.phase(.25,.65),routed=this.phase(.5,.85),sent=this.phase(.65,1);
    // Every possible source → Session route, faint like an idle switch matrix.
    for(const guide of this.guides)this.stroke(ctx,guide.track,INK.line,.7*this.weight(guide.owner)*routed,.8*u,[1*u,5*u]);
    for(const slot of this.slots){
      const level=this.level(slot.key),weight=this.weight(slot.id),link=this.shift(slot.key);
      slot.wires.forEach((wire,index)=>{
        const t=partial(wire.track,wired),alarm=this.shift(wire.key);
        if(!live||wire.tone==='off'){this.stroke(ctx,t,live?INK.off:INK.stale,.7*weight,u,[1.5*u,5*u]);return;}
        if(wire.tone==='down'){
          // A broken link: the red stub stops short of the cache; on a fresh break the wire retracts to it.
          const p=link?.kind==='break'?(this.clock-link.start)/SHIFT_S.break:1;
          if(p<1)this.energise(ctx,partial(wire.track,1-.45*easeOut(p)),INK.red,1-p,weight);
          const stub=partial(wire.track,.55*wired);
          this.stroke(ctx,stub,INK.red,.7*weight,1.2*u,[4*u,4*u]);
          if(index===slot.wires.length>>1&&wired>=1){const [x,y]=stub.pts[stub.pts.length-1],s=3.5*u;ctx.globalAlpha=weight;ctx.lineWidth=1.5*u;ctx.beginPath();ctx.moveTo(x-s,y-s);ctx.lineTo(x+s,y+s);ctx.moveTo(x+s,y-s);ctx.lineTo(x-s,y+s);ctx.stroke();}
          return;
        }
        if(wire.tone==='warn')this.stroke(ctx,t,INK.amber,.6*weight,1.1*u,[4*u,4*u]);
        else if(wire.tone==='idle')this.stroke(ctx,t,INK.idle,weight,u);
        else{this.stroke(ctx,t,slot.selected?INK.rule:INK.line,weight,1.2*u);this.energise(ctx,t,INK.data,level,weight);}
        // A reconnected link lights up again from the card to the cache.
        if(link?.kind==='join'&&wire.tone==='ok'){const p=easeOut((this.clock-link.start)/SHIFT_S.join),lit=partial(wire.track,p);this.energise(ctx,lit,INK.data,1,weight);this.comet(ctx,wire.track,lit.total,60*u,'#ffffff',weight,2.4*u);}
        // A station turning abnormal (or recovering) sends an alarm pulse down its port wire.
        if(alarm){
          const p=(this.clock-alarm.start)/SHIFT_S[alarm.kind],color=alarm.kind==='warn'?INK.amber:INK.ok,fade=(1-p)**1.5;
          for(const [width,strength] of [[14,.2],[5,.35],[2,.9]])this.stroke(ctx,wire.track,color,strength*fade*weight,width*u);
          this.comet(ctx,wire.track,easeOut(p*1.4)*wire.track.total,110*u,'#ffffff',fade*weight,3*u);
        }
      });
    }
    for(const node of this.nodes){
      const level=this.level(node.key),weight=this.weight(node.route),edge=node.edge&&partial(node.edge,routed),exit=partial(node.exit,sent);
      if(!live||!node.attached){const color=live?INK.off:INK.stale;if(edge)this.stroke(ctx,edge,color,.7*weight,u,[1.5*u,5*u]);this.stroke(ctx,exit,color,.7*weight,u,[1.5*u,5*u]);continue;}
      if(edge){this.stroke(ctx,edge,INK.line,weight,1.3*u);this.energise(ctx,edge,INK.out,level,weight);}
      this.stroke(ctx,exit,INK.line,weight,1.3*u);this.energise(ctx,exit,INK.out,level,weight);
    }
    if(this.calc){
      const level=this.level('calc'),feeder=this.slots.find((slot)=>slot.preferred),weight=this.weight(feeder?.id),feed=this.calc.feed&&partial(this.calc.feed,sent),bus=partial(this.calc.bus,sent);
      if(feed){this.stroke(ctx,feed,INK.line,weight,1.1*u,[3*u,3*u]);this.energise(ctx,feed,INK.data,live?level:0,weight);}
      this.stroke(ctx,bus,INK.line,1,1.1*u,[3*u,3*u]);this.energise(ctx,bus,INK.data,live?level*.6:0,1);
      for(const [x,y] of this.calc.joins){ctx.globalAlpha=sent;ctx.fillStyle=INK.rule;ctx.fillRect(x-2*u,y-2*u,4*u,4*u);}
    }
    // Card-side plug of every port wire.
    const plugged=this.phase(.2,.4);
    for(const slot of this.slots){
      const level=this.level(slot.key),weight=this.weight(slot.id)*plugged;
      for(const wire of slot.wires){
        const [x,y]=wire.track.pts[0],color=live?toneInk(wire.tone):INK.stale;
        if(wire.tone==='ok'&&level>.01){const size=12*u;ctx.globalAlpha=.6*level*weight;ctx.drawImage(this.glow(color),x-size/2,y-size/2,size,size);}
        ctx.globalAlpha=weight;ctx.fillStyle=color;ctx.fillRect(x-1.5*u,y-1.5*u,3*u,3*u);
      }
    }
  }

  // Slot and node plates: neutral until data arrives, then the accent edge and left bar light up;
  // each landing burst sweeps a write line across the plate.
  private plate(ctx: CanvasRenderingContext2D, box: Box, color: string, level: number, flash: number, weight: number) {
    const {u}=this.view,cut=5*u,{x,y,w,h}=box;
    const outline=()=>{ctx.beginPath();ctx.moveTo(x+cut,y);ctx.lineTo(x+w,y);ctx.lineTo(x+w,y+h-cut);ctx.lineTo(x+w-cut,y+h);ctx.lineTo(x,y+h);ctx.lineTo(x,y+cut);ctx.closePath();};
    ctx.globalCompositeOperation='source-over';outline();
    ctx.globalAlpha=.96*Math.max(weight,.7);ctx.fillStyle=INK.plate;ctx.fill();
    ctx.globalAlpha=weight;ctx.strokeStyle=INK.rule;ctx.lineWidth=u;ctx.stroke();
    ctx.globalCompositeOperation='lighter';
    if(level+flash>.01){outline();ctx.globalAlpha=Math.min(1,.55*level+.6*flash)*weight;ctx.strokeStyle=color;ctx.stroke();}
    if(flash>.02){
      const halo=Math.max(w,h)*1.3,sweep=x+w*(1-flash);
      ctx.globalAlpha=.3*flash*weight;ctx.drawImage(this.glow(color),x+w/2-halo/2,y+h/2-halo/2,halo,halo);
      ctx.save();outline();ctx.clip();
      const bar=ctx.createLinearGradient(sweep-24*u,0,sweep,0);bar.addColorStop(0,withAlpha(color,0));bar.addColorStop(1,withAlpha(color,.5));
      ctx.globalAlpha=flash*weight;ctx.fillStyle=bar;ctx.fillRect(sweep-24*u,y,24*u,h);ctx.fillStyle='#ffffff';ctx.fillRect(sweep-u,y,1.5*u,h);
      ctx.restore();
    }
    ctx.globalAlpha=weight;ctx.fillStyle=level>.01?color:INK.rule;ctx.fillRect(x,y+cut,1.5*u,h-cut*2);
  }

  private boxes(ctx: CanvasRenderingContext2D, live: boolean) {
    const {u}=this.view;
    this.slots.forEach((slot,index)=>{
      const shown=this.phase(.15+index*.05,.45+index*.05),level=this.level(slot.key),flash=this.flashes.get(slot.key)??0,weight=this.weight(slot.id)*shown,color=live?toneInk(slot.link):INK.stale,{x,y,w,h}=slot.box;
      if(shown<=0)return;
      this.plate(ctx,slot.box,color,level,flash,weight);
      const metered=slot.link==='ok'||slot.link==='warn';
      this.text(ctx,slot.id,x+10*u,y+h/2,live?INK.text:INK.stale,weight,11,w-(metered?46:16)*u);
      // Five-segment meter: lit while samples arrive; absent when the source cannot deliver.
      if(metered)for(let i=0;i<5;i++){ctx.globalAlpha=weight;ctx.fillStyle=i<Math.round(level*5)?color:INK.line;ctx.fillRect(x+w-(34-i*6)*u,y+h/2-4*u+(4-i)*.6*u,3*u,8*u-(4-i)*1.2*u);}
      if(slot.preferred)this.text(ctx,'DEFAULT · 默认来源',x+1*u,y-8*u,live?INK.data:INK.stale,.85*weight,8.5);
    });
    this.nodes.forEach((node,index)=>{
      const shown=this.phase(.45+index*.05,.75+index*.05),level=this.level(node.key),flash=this.flashes.get(node.key)??0,weight=this.weight(node.route)*shown,color=live&&node.attached?INK.out:live?INK.off:INK.stale,{x,y,w,h}=node.box;
      if(shown<=0)return;
      this.plate(ctx,node.box,color,level,flash,weight);
      this.text(ctx,node.label,x+10*u,y+h/2,live?INK.text:INK.stale,weight,11,w-18*u);
      // Forwarding cue: a pointer slides in from the selected source as each burst passes the node.
      if(flash>.02){const px=x-4*u-12*u*flash,cy=y+h/2;ctx.globalAlpha=flash*weight;ctx.fillStyle=color;ctx.beginPath();ctx.moveTo(px+6*u,cy);ctx.lineTo(px,cy-4*u);ctx.lineTo(px,cy+4*u);ctx.closePath();ctx.fill();}
      // Selected source shown on the node; an off-page selection gets a dashed stub.
      const route=node.route?`← ${node.route}${node.slot?'':' · 其他页'}`:'未选择来源';
      this.text(ctx,route,x+1*u,y-8*u,flash>.05?color:INK.muted,.9*weight,8.5);
      if(node.route&&!node.slot)this.stroke(ctx,track([[x-60*u,y+h/2],[x,y+h/2]]),INK.rule,weight,u,[3*u,4*u]);
    });
    if(this.calc){
      const shown=this.phase(.6,1);
      if(shown<=0)return;
      const {center:[cx,cy],r,good,bad}=this.calc,level=this.level('calc'),flash=this.flashes.get('calc')??0,color=live?INK.data:INK.stale,total=Math.max(1,good+bad),start=-Math.PI/2,split=start+Math.PI*2*good/total*shown;
      ctx.globalCompositeOperation='source-over';ctx.globalAlpha=.96*shown;ctx.fillStyle=INK.plate;ctx.beginPath();ctx.arc(cx,cy,r+3*u,0,Math.PI*2);ctx.fill();
      ctx.globalCompositeOperation='lighter';
      if(flash>.02){const halo=r*4;ctx.globalAlpha=.35*flash;ctx.drawImage(this.glow(color),cx-halo/2,cy-halo/2,halo,halo);}
      // Tick ring (turning briefly on each evaluation), then the real Good / other split as a gauge.
      const turn=flash*.6;
      ctx.strokeStyle=INK.rule;ctx.lineWidth=u;ctx.globalAlpha=.9*shown;ctx.beginPath();
      for(let i=0;i<48;i++){const a=i/48*Math.PI*2+turn,outer=r+(i%12?6:9)*u;ctx.moveTo(cx+Math.cos(a)*(r+4*u),cy+Math.sin(a)*(r+4*u));ctx.lineTo(cx+Math.cos(a)*outer,cy+Math.sin(a)*outer);}
      ctx.stroke();
      ctx.lineWidth=2.5*u;ctx.strokeStyle=INK.line;ctx.globalAlpha=shown;ctx.beginPath();ctx.arc(cx,cy,r,0,Math.PI*2);ctx.stroke();
      ctx.strokeStyle=color;ctx.beginPath();ctx.arc(cx,cy,r,start,split);ctx.stroke();
      if(bad){ctx.strokeStyle=live?INK.amber:INK.stale;ctx.beginPath();ctx.arc(cx,cy,r,split,start+Math.PI*2*shown);ctx.stroke();}
      ctx.strokeStyle=level>.01?color:INK.rule;ctx.lineWidth=.8*u;ctx.globalAlpha=(.5+.4*level)*shown;ctx.setLineDash([1.5*u,3.5*u]);ctx.lineDashOffset=-this.clock*10*u;ctx.beginPath();ctx.arc(cx,cy,r*.66,0,Math.PI*2);ctx.stroke();ctx.setLineDash([]);ctx.lineDashOffset=0;
      this.text(ctx,`${Math.round(good/total*100*shown)}%`,cx,cy,live?INK.text:INK.stale,shown,10.5,undefined,'center');
    }
  }

  private flight(ctx: CanvasRenderingContext2D) {
    const {u}=this.view,spark=this.glow('#ffffff'),shown=this.phase(.6,1);
    for(const packet of this.packets){
      const age=this.clock-packet.born;
      if(age<0||age>=packet.life)continue;
      const head=age/packet.life*packet.track.total,fade=Math.min(1,age*10,(packet.life-age)*8)*(packet.owner?this.weight(packet.owner):1)*shown,scale=packet.lead?1.3:1,glow=this.glow(packet.color);
      for(let k=9;k>=0;k--){
        const [x,y]=along(packet.track,head-k*4.5*u),size=13*u*scale*(1-k/10*.7);
        ctx.globalAlpha=fade*(1-k/10)**1.5*(k?.5:.9);ctx.drawImage(glow,x-size/2,y-size/2,size,size);
      }
      const [x,y]=along(packet.track,head),size=6.5*u*scale;
      ctx.globalAlpha=fade;ctx.drawImage(spark,x-size/2,y-size/2,size,size);
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
}
