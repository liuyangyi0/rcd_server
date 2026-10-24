import { sourceCanFlow, type Activity, type FlowSession, type FlowSource } from './model';

export const SCENE_WIDTH = 1680, SCENE_HEIGHT = 750;
// DOM cards are placed in percentages of this 1680×750 scene. Geometry stays in
// scene units and is mapped per axis; sprites and strokes are drawn in device
// pixels so particles stay round when the scene is stretched.
const CX = -480, CY = 375, INNER = 1200, OUTER = 1535, SPAN = .34;
const INGRESS_X = 304, EGRESS_X = 1448, RECENT_MS = 3200, STREAK_HEAD = 88;
const INK = { data: '#5ef2e0', out: '#8dffc8', hub: '#9df5d8', dust: '#7fd9d0', amber: '#f5b951', red: '#f17479', off: '#6e828c', conflict: '#e0b164', stale: '#62747b' };
export const laneY = (index: number, count: number) => 48 + (index + 0.5) * (654 / Math.max(count, 1));
const arcX = (y: number, r: number) => CX + Math.sqrt(r*r-(y-CY)**2);
const fract = (value: number) => value - Math.floor(value);
// Deterministic noise: paused and reduced-motion frames must be pixel-stable.
const hash = (seed: number) => fract(Math.sin(seed * 127.1 + 311.7) * 43758.5453);
const withAlpha = (hex: string, alpha: number) => hex + Math.round(Math.max(0, Math.min(1, alpha)) * 255).toString(16).padStart(2, '0');
type Point = [number,number];
export interface SceneState { sources: FlowSource[]; sessions: FlowSession[]; activity: Activity; live: boolean; paused: boolean; selected?: string; cacheActive?: boolean }
export interface SceneFrame { width: number; height: number; dt: number; motion: boolean; now: number }
interface Lane { a: Point; b: Point; c: Point; d: Point; color: string; healthy: boolean; seed: number }
interface Flow { key: string; out: boolean; lanes: Lane[]; colors: string[]; anchor: Point; port: Point; half: number; color: string; ready: boolean; always: boolean; stamp?: number; running: boolean; selected: boolean; label?: string }

function bezier(a: Point,b: Point,c: Point,d: Point,t: number): Point {
  const u=1-t;
  return [u*u*u*a[0]+3*u*u*t*b[0]+3*u*t*t*c[0]+t*t*t*d[0],u*u*u*a[1]+3*u*u*t*b[1]+3*u*t*t*c[1]+t*t*t*d[1]];
}
function tangent(a: Point,b: Point,c: Point,d: Point,t: number): Point {
  const u=1-t;
  return [3*u*u*(b[0]-a[0])+6*u*t*(c[0]-b[0])+3*t*t*(d[0]-c[0]),3*u*u*(b[1]-a[1])+6*u*t*(c[1]-b[1])+3*t*t*(d[1]-c[1])];
}

function sheet(width: number, height: number) {
  const canvas=document.createElement('canvas');canvas.width=width;canvas.height=height;
  return canvas;
}
function glowSprite(color: string) {
  const sprite=sheet(64,64),g=sprite.getContext('2d');
  if(g){const glow=g.createRadialGradient(32,32,0,32,32,32);glow.addColorStop(0,'#ffffff');glow.addColorStop(.12,withAlpha(color,1));glow.addColorStop(.35,withAlpha(color,.32));glow.addColorStop(1,withAlpha(color,0));g.fillStyle=glow;g.fillRect(0,0,64,64);}
  return sprite;
}
// Comet with a white-hot head at STREAK_HEAD and a tail fading to the left.
function streakSprite(color: string) {
  const sprite=sheet(96,16),g=sprite.getContext('2d');
  if(g){
    const body=g.createLinearGradient(0,0,96,0);body.addColorStop(0,withAlpha(color,0));body.addColorStop(.62,withAlpha(color,.45));body.addColorStop(.9,'#ffffff');body.addColorStop(.94,'#ffffff');body.addColorStop(1,withAlpha(color,0));
    g.fillStyle=body;g.fillRect(0,0,96,16);
    const profile=g.createLinearGradient(0,0,0,16);profile.addColorStop(0,'#0000');profile.addColorStop(.3,'#0003');profile.addColorStop(.5,'#000');profile.addColorStop(.7,'#0003');profile.addColorStop(1,'#0000');
    g.globalCompositeOperation='destination-in';g.fillStyle=profile;g.fillRect(0,0,96,16);
  }
  return sprite;
}

// Lane geometry and readiness only change with the snapshot or page; recency is
// re-evaluated per frame from the activity timestamps.
function layout(state: SceneState): Flow[] {
  const flows: Flow[]=[],sessions=state.sessions;
  state.sources.forEach((source,index)=>{
    const y=laneY(index,state.sources.length),x=arcX(y,INNER),half=Math.min(78,274/Math.max(state.sources.length,1));
    const color=!source.enabled?INK.off:!source.connected?INK.red:source.catalog_state==='CatalogConflict'?INK.conflict:INK.data;
    const devices=source.ports.flatMap((p)=>p.devices).slice(0,12),lanes: Lane[]=[];
    for(let lane=0;lane<34;lane++){
      const spread=(lane/33*2-1)*half,device=devices.length?devices[lane%devices.length]:undefined;
      lanes.push({a:[INGRESS_X,y+spread],b:[INGRESS_X+140,y+spread*1.15],c:[x-210,y+spread*.22],d:[x,y],color:device&&(device.state===2||device.state===3)?INK.amber:color,healthy:!device||device.state===1,seed:lane*7.31+index*97.3});
    }
    flows.push({key:`in:${source.id}`,out:false,lanes,colors:[...new Set(lanes.map((lane)=>lane.color))],anchor:[x,y],port:[INGRESS_X,y],half,color,ready:state.live&&sourceCanFlow(source),always:false,stamp:state.activity.sources[source.id],running:false,selected:state.selected===source.id,label:source.id});
  });
  const outputs: {key: string; y: number; attached: boolean; stamp?: number}[]=sessions.length
    ?sessions.map((session,index)=>({key:`out:${session.id}`,y:laneY(index,sessions.length),attached:session.attached,stamp:state.activity.sessions[session.id]}))
    :[{key:'out:cache',y:CY,attached:true}];
  outputs.forEach((output,index)=>{
    const x=arcX(output.y,OUTER),half=sessions.length?Math.min(74,262/sessions.length):150,color=output.attached?INK.out:INK.off,lanes: Lane[]=[];
    for(let lane=0;lane<38;lane++){
      const spread=(lane/37*2-1)*half;
      lanes.push({a:[x,output.y+spread*.05],b:[x+150,output.y+spread*.15],c:[EGRESS_X-180,output.y+spread],d:[EGRESS_X,output.y+spread],color,healthy:true,seed:4000+lane*5.17+index*131.9});
    }
    flows.push({key:output.key,out:true,lanes,colors:[color],anchor:[x,output.y],port:[EGRESS_X,output.y],half,color,ready:state.live&&output.attached&&(sessions.length>0||!!state.cacheActive),always:!sessions.length,stamp:output.stamp,running:false,selected:false});
  });
  return flows;
}

// The arc band, ticks, scan line and drifting dust are decoration. Particles,
// arrival ripples and the band's energy follow only the real activity counters;
// a stale monitor turns the whole scene grey and stops every data effect.
export class FlowScene {
  private clock=0; private spin=0; private energy=0; private sx=1; private sy=1; private u=1;
  private flows: Flow[]=[]; private shown?: SceneState;
  private levels=new Map<string,number>(); private stamps=new Map<string,number>(); private pulses: {key: string; start: number}[]=[];
  private glows=new Map<string,HTMLCanvasElement>(); private streaks=new Map<string,HTMLCanvasElement>();
  private backdrop?: HTMLCanvasElement; private backdropKey='';

  draw(ctx: CanvasRenderingContext2D, state: SceneState, frame: SceneFrame) {
    this.sx=frame.width/SCENE_WIDTH;this.sy=frame.height/SCENE_HEIGHT;this.u=Math.sqrt(this.sx*this.sy);
    if(state!==this.shown){this.flows=layout(state);this.shown=state;}
    this.advance(state,frame);
    ctx.setTransform(1,0,0,1,0,0);ctx.globalCompositeOperation='source-over';ctx.globalAlpha=1;
    ctx.clearRect(0,0,frame.width,frame.height);
    ctx.drawImage(this.backdropFor(frame.width,frame.height),0,0);
    ctx.globalCompositeOperation='lighter';
    this.ambient(ctx,state.live);
    this.hub(ctx,state.live);
    for(const flow of this.flows)this.lanes(ctx,flow,state.live);
    for(const flow of this.flows)this.particles(ctx,flow);
    this.ripples(ctx);
    for(const flow of this.flows)this.nodes(ctx,flow,state.live);
    ctx.setTransform(1,0,0,1,0,0);ctx.globalCompositeOperation='source-over';ctx.globalAlpha=1;
  }

  private advance(state: SceneState, {dt,motion,now}: SceneFrame) {
    // Fades run only while animating; paused or reduced-motion frames show the current state at once.
    const ease=(rate: number)=>motion&&state.live?1-Math.exp(-dt*rate):1,keys=new Set<string>();
    let total=0;
    this.clock+=dt;this.spin+=dt*(.4+.9*this.energy);
    for(const flow of this.flows){
      flow.running=flow.ready&&(flow.always||(!!flow.stamp&&now-flow.stamp<RECENT_MS));
      const old=this.levels.get(flow.key)??0,level=old+(Number(flow.running)-old)*ease(3.5),seen=this.stamps.get(flow.key);
      this.levels.set(flow.key,level);total+=level;keys.add(flow.key);
      if(flow.stamp===undefined)continue;
      this.stamps.set(flow.key,flow.stamp);
      if(motion&&flow.running&&flow.stamp>(seen??0))this.pulses.push({key:flow.key,start:this.clock});
    }
    for(const map of [this.levels,this.stamps])for(const key of map.keys())if(!keys.has(key))map.delete(key);
    const target=state.live?Math.min(1,total/Math.max(1,this.flows.length)*1.3+(state.cacheActive?.2:0)):0;
    this.energy+=(target-this.energy)*ease(1.2);
    this.pulses=state.live?this.pulses.filter((pulse)=>this.clock-pulse.start<1.5).slice(-48):[];
  }

  private hub(ctx: CanvasRenderingContext2D, live: boolean) {
    const {u,sx,sy}=this,e=this.energy,ink=live?INK.hub:INK.stale,dim=live?1:.45,cx=this.X(CX),cy=this.Y(CY);
    const polar=(a: number,r: number): Point=>[this.X(CX+Math.cos(a)*r),this.Y(CY+Math.sin(a)*r)];
    // Rails glow brighter and the band spins faster while real updates arrive.
    ctx.strokeStyle=ink;
    for(const r of [INNER,OUTER])for(const [width,strength] of [[16,.04],[6,.1],[1.8,.6]]){
      ctx.globalAlpha=strength*(.5+.5*e)*dim;ctx.lineWidth=width*u;
      ctx.beginPath();ctx.ellipse(cx,cy,r*sx,r*sy,0,-SPAN,SPAN);ctx.stroke();
    }
    for(const [r,dir,speed] of [[INNER+11,1,.023],[OUTER-11,-1,-.017]])for(const major of [false,true]){
      ctx.beginPath();
      for(let n=0;n<180;n++){
        if((n%6===0)!==major)continue;
        const a=fract(n/180+this.spin*speed/.7)*.7-.35;
        ctx.moveTo(...polar(a,r));ctx.lineTo(...polar(a,r+dir*(major?15:7)));
      }
      ctx.globalAlpha=(major?.75:.28)*(.6+.4*e)*dim;ctx.lineWidth=(major?2.2:1)*u;ctx.stroke();
    }
    // Radial data columns with bright packets travelling through them.
    const strengths=[.06,.14,.3,.7];
    for(let bucket=0;bucket<4;bucket++){
      ctx.beginPath();
      for(let column=0;column<5;column++){
        const r=INNER+48+column*53,drift=this.spin*(column%2?.009:-.012)/.69;
        for(let row=0;row<62;row++){
          const wave=fract(row/31-this.spin*.3+column*.37),size=(row*17+column*7)%27;
          if((wave<.06?3:wave<.16?2:size%5===0?1:0)!==bucket)continue;
          const a=fract(row/62+drift)*.69-.345;
          ctx.moveTo(...polar(a,r));ctx.lineTo(...polar(a,r+10+size));
        }
      }
      ctx.globalAlpha=strengths[bucket]*(bucket>1?.3+.7*e:1)*dim;ctx.strokeStyle=live?bucket>1?'#e2fff6':'#b5ffe7':INK.stale;ctx.lineWidth=1.5*u;ctx.stroke();
    }
    ctx.save();
    ctx.beginPath();ctx.ellipse(cx,cy,OUTER*sx,OUTER*sy,0,-SPAN,SPAN);ctx.ellipse(cx,cy,INNER*sx,INNER*sy,0,SPAN,-SPAN,true);ctx.closePath();ctx.clip();
    const scan=this.Y(CY+Math.sin(this.spin*.5)*330),reach=48*sy,beam=ctx.createLinearGradient(0,scan-reach,0,scan+reach);
    beam.addColorStop(0,withAlpha(ink,0));beam.addColorStop(.5,withAlpha(ink,.14));beam.addColorStop(1,withAlpha(ink,0));
    ctx.globalAlpha=(.5+.5*e)*dim;ctx.fillStyle=beam;ctx.fillRect(this.X(620),scan-reach,480*sx,reach*2);
    ctx.globalAlpha=(.25+.35*e)*dim;ctx.fillStyle=ink;ctx.fillRect(this.X(620),scan-.5*u,480*sx,u);
    ctx.restore();
    if(live){
      const glow=this.glow(ink);
      for(const [r,dir] of [[INNER,1],[OUTER,-1]])for(let k=0;k<2;k++){
        const head=(fract(this.spin*.14*dir+k*.5+(dir>0?0:.25))-.5)*2*SPAN;
        for(let i=0;i<12;i++){
          const [x,y]=polar(head-dir*i*.0045,r),size=(22-i*1.2)*u;
          ctx.globalAlpha=(1-i/12)**1.6*(.25+.75*e);ctx.drawImage(glow,x-size/2,y-size/2,size,size);
        }
      }
    }
    // Sparks crossing the band scale with the share of flows that are really running.
    const sparks=28*e,streak=this.streak(ink);
    for(let i=0;i<Math.ceil(sparks);i++){
      const a=(hash(i*3.17)-.5)*.56,p=fract(this.clock*(.4+hash(i*5.3)*.35)+hash(i*1.91)),[x,y]=polar(a,INNER+8+p*(OUTER-INNER-16));
      const angle=Math.atan2(Math.sin(a)*sy,Math.cos(a)*sx),cos=Math.cos(angle)*.55*u,sin=Math.sin(angle)*.55*u;
      ctx.globalAlpha=Math.sin(p*Math.PI)*Math.min(1,sparks-i)*.85;
      ctx.setTransform(cos,sin,-sin,cos,x,y);ctx.drawImage(streak,-STREAK_HEAD,-8);
    }
    ctx.setTransform(1,0,0,1,0,0);
  }

  private lanes(ctx: CanvasRenderingContext2D, flow: Flow, live: boolean) {
    const level=this.levels.get(flow.key)??0,u=this.u;
    const trace=(lane: Lane)=>{ctx.moveTo(this.X(lane.a[0]),this.Y(lane.a[1]));ctx.bezierCurveTo(this.X(lane.b[0]),this.Y(lane.b[1]),this.X(lane.c[0]),this.Y(lane.c[1]),this.X(lane.d[0]),this.Y(lane.d[1]));};
    for(const color of flow.colors){
      ctx.beginPath();for(const lane of flow.lanes)if(lane.color===color)trace(lane);
      ctx.globalAlpha=live?.05+.1*level+(flow.selected?.08:0):.035;ctx.strokeStyle=live?color:INK.stale;ctx.lineWidth=(flow.selected?1:.7)*u;ctx.stroke();
    }
    if(level<.01)return;
    // A soft beam along the bundle centre keeps active routes readable from a distance.
    ctx.strokeStyle=flow.color;
    for(const [width,strength] of [[26,.03],[8,.06]]){
      ctx.beginPath();trace(flow.lanes[flow.lanes.length>>1]);ctx.globalAlpha=strength*level;ctx.lineWidth=width*u;ctx.stroke();
    }
  }

  private particles(ctx: CanvasRenderingContext2D, flow: Flow) {
    const level=this.levels.get(flow.key)??0,u=this.u;
    if(level<.01)return;
    for(const lane of flow.lanes){
      if(!lane.healthy)continue;
      const streak=this.streak(lane.color),speed=(flow.out?.32:.22)+hash(lane.seed)*.1;
      for(let n=0;n<2;n++){
        // Inputs accelerate into the hub; outputs leave fast and settle at the client.
        const phase=fract(this.clock*speed+hash(lane.seed+n*17.17)),big=hash(lane.seed*3.3+n)>.9;
        const t=flow.out?1-(1-phase)**1.7:phase**1.45,pace=flow.out?1.7*(1-phase)**.7:1.45*phase**.45;
        const [x,y]=bezier(lane.a,lane.b,lane.c,lane.d,t),[tx,ty]=tangent(lane.a,lane.b,lane.c,lane.d,t);
        const norm=Math.hypot(tx,ty)||1,wobble=Math.sin(phase*9+lane.seed)*2.4*(flow.out?t:1-t);
        const px=this.X(x-ty/norm*wobble),py=this.Y(y+tx/norm*wobble),angle=Math.atan2(ty*this.sy,tx*this.sx);
        const along=(.25+.45*pace)*(big?1.3:.85)*u,across=(big?1.1:.7)*u,cos=Math.cos(angle),sin=Math.sin(angle);
        ctx.globalAlpha=Math.min(1,phase*10,(1-phase)*12)*level*(big?1:.72);
        ctx.setTransform(cos*along,sin*along,-sin*across,cos*across,px,py);ctx.drawImage(streak,-STREAK_HEAD,-8);
        if(!big)continue;
        const size=18*u;
        ctx.setTransform(1,0,0,1,0,0);ctx.drawImage(this.glow(lane.color),px-size/2,py-size/2,size,size);
      }
    }
    ctx.setTransform(1,0,0,1,0,0);
  }

  // One ripple per snapshot whose sample or notification counter advanced.
  private ripples(ctx: CanvasRenderingContext2D) {
    const u=this.u;
    for(const pulse of this.pulses){
      const flow=this.flows.find((item)=>item.key===pulse.key);
      if(!flow)continue;
      const [x,y]=flow.out?flow.port:flow.anchor,age=(this.clock-pulse.start)/1.5,px=this.X(x),py=this.Y(y);
      ctx.strokeStyle=flow.color;
      for(const lag of [0,.18]){
        const p=age-lag;
        if(p<=0||p>=1)continue;
        ctx.globalAlpha=(1-p)**2*.8;ctx.lineWidth=(2.4*(1-p)+.5)*u;
        ctx.beginPath();ctx.arc(px,py,(6+p*64)*u,0,Math.PI*2);ctx.stroke();
      }
      if(age<.3){const size=(70-age*120)*u;ctx.globalAlpha=1-age/.3;ctx.drawImage(this.glow(flow.color),px-size/2,py-size/2,size,size);}
    }
  }

  private nodes(ctx: CanvasRenderingContext2D, flow: Flow, live: boolean) {
    const level=this.levels.get(flow.key)??0,u=this.u,color=live?flow.color:INK.stale,glow=this.glow(color);
    const x=this.X(flow.anchor[0]),y=this.Y(flow.anchor[1]),px=this.X(flow.port[0]),py=this.Y(flow.port[1]),top=this.Y(flow.port[1]-flow.half),bottom=this.Y(flow.port[1]+flow.half),tick=(flow.out?-5:5)*u;
    ctx.globalCompositeOperation='lighter';ctx.strokeStyle=color;
    // Port bracket where lanes leave a source card or reach a client card.
    ctx.globalAlpha=.2+.35*level;ctx.lineWidth=u;
    ctx.beginPath();ctx.moveTo(px+tick,top);ctx.lineTo(px,top);ctx.lineTo(px,bottom);ctx.lineTo(px+tick,bottom);ctx.stroke();
    if(level>.01){const halo=34*u;ctx.globalAlpha=.55*level;ctx.drawImage(glow,px-halo/2,py-halo/2,halo,halo);}
    const size=(flow.out?26:40)*u*(.7+.3*level);
    ctx.globalAlpha=.3+.6*level;ctx.drawImage(glow,x-size/2,y-size/2,size,size);
    ctx.globalAlpha=.85;ctx.lineWidth=1.8*u;ctx.beginPath();ctx.arc(x,y,(flow.out?5:8)*u,0,Math.PI*2);ctx.stroke();
    if(!flow.out){ctx.globalAlpha=.22;ctx.lineWidth=4*u;ctx.beginPath();ctx.arc(x,y,13*u,0,Math.PI*2);ctx.stroke();}
    if(flow.selected){
      ctx.globalAlpha=.9;ctx.lineWidth=1.2*u;ctx.setLineDash([5*u,4*u]);ctx.lineDashOffset=-this.spin*14*u;
      ctx.beginPath();ctx.arc(x,y,21*u,0,Math.PI*2);ctx.stroke();ctx.setLineDash([]);ctx.lineDashOffset=0;
    }
    if(!flow.label)return;
    const text=flow.label.slice(0,13);
    ctx.globalCompositeOperation='source-over';ctx.font=`${Math.round(11*u)}px ui-monospace, SFMono-Regular, Consolas, monospace`;ctx.textBaseline='middle';
    const width=ctx.measureText(text).width+16*u,height=22*u,left=x-22*u-width,labelTop=y-height/2;
    ctx.globalAlpha=.9;ctx.fillStyle='#061217';ctx.fillRect(left,labelTop,width,height);
    ctx.globalAlpha=.45;ctx.lineWidth=u;ctx.strokeRect(left,labelTop,width,height);
    ctx.globalAlpha=1;ctx.fillStyle=color;ctx.fillRect(left,labelTop,2*u,height);
    ctx.fillStyle='#d3ede3';ctx.fillText(text,left+9*u,y);
  }

  private ambient(ctx: CanvasRenderingContext2D, live: boolean) {
    const mote=this.glow(live?INK.dust:INK.stale);
    for(let i=0;i<64;i++){
      // Dust drifts upward so it is never read as left-to-right data flow.
      const depth=hash(i*4.7),size=(3+depth*7)*this.u;
      const x=hash(i*1.3)*SCENE_WIDTH+Math.sin(this.clock*.23+i)*16,y=(1-fract(hash(i*2.9)+this.clock*(5+depth*16)/SCENE_HEIGHT))*SCENE_HEIGHT;
      ctx.globalAlpha=(.05+.13*(.5+.5*Math.sin(this.clock*(.6+depth)+i*2.3)))*(live?1:.5);
      ctx.drawImage(mote,this.X(x)-size/2,this.Y(y)-size/2,size,size);
    }
  }

  private backdropFor(width: number, height: number) {
    const key=`${width}x${height}`;
    if(this.backdrop&&this.backdropKey===key)return this.backdrop;
    const canvas=this.backdrop??sheet(width,height);
    canvas.width=width;canvas.height=height;this.backdrop=canvas;this.backdropKey=key;
    const g=canvas.getContext('2d');
    if(!g)return canvas;
    const cx=this.X(CX),cy=this.Y(CY),halo=g.createRadialGradient(this.X(880),cy,0,this.X(880),cy,560*this.u);
    halo.addColorStop(0,'#1f514a40');halo.addColorStop(1,'#1f514a00');g.fillStyle=halo;g.fillRect(0,0,width,height);
    const band=g.createLinearGradient(this.X(650),0,this.X(1080),0);
    band.addColorStop(0,'#0b171ce6');band.addColorStop(.2,'#162629e6');band.addColorStop(.58,'#0c141ae6');band.addColorStop(.9,'#172a2ae6');band.addColorStop(1,'#102522e6');
    g.beginPath();g.ellipse(cx,cy,OUTER*this.sx,OUTER*this.sy,0,-SPAN,SPAN);g.ellipse(cx,cy,INNER*this.sx,INNER*this.sy,0,SPAN,-SPAN,true);g.closePath();g.fillStyle=band;g.fill();
    g.strokeStyle='#77a5a2';g.lineWidth=this.u;g.globalAlpha=.22;
    for(const r of [INNER-12,INNER+8,INNER+31,OUTER-38,OUTER-17,OUTER+7]){g.beginPath();g.ellipse(cx,cy,r*this.sx,r*this.sy,0,-SPAN,SPAN);g.stroke();}
    return canvas;
  }

  private glow(color: string): HTMLCanvasElement {
    let sprite=this.glows.get(color);
    if(!sprite){sprite=glowSprite(color);this.glows.set(color,sprite);}
    return sprite;
  }
  private streak(color: string): HTMLCanvasElement {
    let sprite=this.streaks.get(color);
    if(!sprite){sprite=streakSprite(color);this.streaks.set(color,sprite);}
    return sprite;
  }
  private X(x: number) { return x*this.sx; }
  private Y(y: number) { return y*this.sy; }
}
