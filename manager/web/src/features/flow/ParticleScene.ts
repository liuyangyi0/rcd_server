import { FLOW_HUES, sourceCanFlow, type Activity, type FlowSession, type FlowSource } from './model';

export const SCENE_WIDTH = 1680, SCENE_HEIGHT = 750;
// DOM cards are placed in percentages of this scene. The hub sits between the
// card columns above the processing row (.flow-processing in FlowPage.css).
const HUB_X = 876, HUB_Y = 288, HUB_R = 86, INGRESS_X = 306, EGRESS_X = 1446, RECENT_MS = 3200;
const SPIRAL = .2, TRAIL = 14, OUTPUT_DELAY = 1;
const INK = { hub: '#7ff3ff', out: '#c4f1ff', amber: '#f5b951', red: '#f17479', off: '#6e828c', stale: '#5d6d74', star: '#bfe9ff' };
export const laneY = (index: number, count: number) => 48 + (index + 0.5) * (654 / Math.max(count, 1));
const fract = (value: number) => value - Math.floor(value);
const clamp = (value: number, low: number, high: number) => Math.max(low, Math.min(high, value));
// Deterministic noise: paused and reduced-motion frames must be pixel-stable.
const hash = (seed: number) => fract(Math.sin(seed * 127.1 + 311.7) * 43758.5453);
const withAlpha = (hex: string, alpha: number) => hex + Math.round(clamp(alpha, 0, 1) * 255).toString(16).padStart(2, '0');
// Packets per snapshot follow the logarithm of real updates, so 10 and 10 000 samples both stay readable.
export const burstSize = (delta: number) => delta > 0 ? Math.min(18, 3 + Math.round(5 * Math.log10(1 + delta))) : 0;
type Point = [number,number];
export interface SceneState { sources: FlowSource[]; sessions: FlowSession[]; activity: Activity; live: boolean; paused: boolean; selected?: string; cacheActive?: boolean; group?: string; hues?: Record<string, string> }
export interface SceneFrame { width: number; height: number; dt: number; motion: boolean; now: number }
interface View { sx: number; sy: number; u: number; hx: number; hy: number; hr: number }
interface Channel { key: string; out: boolean; color: string; link: 'ok' | 'down' | 'off'; a: Point; b: Point; c: Point; d: Point; angle: number; ready: boolean; stamp?: number; delta: number; running: boolean; selected: boolean }
interface Packet { key: string; born: number; life: number; spread: number; lead: boolean }
interface Wave { start: number; color: string; at?: Point }
interface Spark { x: number; y: number; vx: number; vy: number; born: number; color: string }

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
  if(g){const glow=g.createRadialGradient(32,32,0,32,32,32);glow.addColorStop(0,'#ffffff');glow.addColorStop(.14,withAlpha(color,1));glow.addColorStop(.38,withAlpha(color,.3));glow.addColorStop(1,withAlpha(color,0));g.fillStyle=glow;g.fillRect(0,0,64,64);}
  return sprite;
}

// Channels run from each source card into the hub rim and from the rim to each
// Session card. Geometry is in device pixels and rebuilt with the snapshot or size.
function layout(state: SceneState, view: View): Channel[] {
  const channels: Channel[]=[],hues=state.hues??{},{sx,sy,hx,hy,hr}=view;
  const rim=(angle: number): Point=>[hx+Math.cos(angle)*hr,hy+Math.sin(angle)*hr];
  state.sources.forEach((source,index)=>{
    const a: Point=[INGRESS_X*sx,laneY(index,state.sources.length)*sy],angle=Math.PI-clamp(Math.atan2(a[1]-hy,hx-a[0]),-1.05,1.05),d=rim(angle),reach=Math.hypot(d[0]-a[0],d[1]-a[1]);
    channels.push({key:`in:${source.id}`,out:false,color:source.catalog_state==='CatalogConflict'?INK.amber:hues[source.id]??FLOW_HUES[index%FLOW_HUES.length],link:!source.enabled?'off':!source.connected?'down':'ok',
      a,b:[a[0]+reach*.42,a[1]],c:[d[0]+Math.cos(angle)*reach*.3,d[1]+Math.sin(angle)*reach*.3],d,angle,
      ready:state.live&&sourceCanFlow(source),stamp:state.activity.sources[source.id],delta:state.activity.sourceDeltas[source.id]??0,running:false,selected:state.selected===source.id});
  });
  const sessions=state.sessions,latest=Math.max(0,...Object.values(state.activity.sources));
  const outputs: {key: string; y: number; attached: boolean; color: string; stamp?: number; delta: number}[]=sessions.length
    ?sessions.map((session,index)=>({key:`out:${session.id}`,y:laneY(index,sessions.length),attached:session.attached,color:hues[session.routes[state.group??'']??'']??INK.out,stamp:state.activity.sessions[session.id],delta:state.activity.sessionDeltas[session.id]??0}))
    // Without Sessions the hub feeds only the latest-sample cache.
    :[{key:'out:cache',y:375,attached:true,color:INK.hub,stamp:latest||undefined,delta:state.activity.updates}];
  outputs.forEach((output)=>{
    const d: Point=[EGRESS_X*sx,output.y*sy],angle=clamp(Math.atan2(d[1]-hy,d[0]-hx),-1.05,1.05),a=rim(angle),reach=Math.hypot(d[0]-a[0],d[1]-a[1]);
    channels.push({key:output.key,out:true,color:output.attached?output.color:INK.off,link:output.attached?'ok':'off',
      a,b:[a[0]+Math.cos(angle)*reach*.3,a[1]+Math.sin(angle)*reach*.3],c:[d[0]-reach*.42,d[1]],d,angle,
      ready:state.live&&output.attached&&(sessions.length>0||!!state.cacheActive),stamp:output.stamp,delta:output.delta,running:false,selected:false});
  });
  return channels;
}

// Data packets are launched only when a snapshot shows new samples or Session
// notifications; their number follows that count. Rings, orbits, the floor grid
// and the drifting dust are decoration. A stale monitor greys out and clears packets.
export class FlowScene {
  private clock=0; private spin=0; private energy=0; private charge=0; private seq=0;
  private view: View={sx:1,sy:1,u:1,hx:0,hy:0,hr:1};
  private channels: Channel[]=[]; private byKey=new Map<string,Channel>(); private shown?: SceneState; private shownSize='';
  private levels=new Map<string,number>(); private stamps=new Map<string,number>();
  private packets: Packet[]=[]; private waves: Wave[]=[]; private sparks: Spark[]=[];
  private glows=new Map<string,HTMLCanvasElement>(); private backdrop?: HTMLCanvasElement; private backdropKey='';

  // Packets launched or still travelling.
  get inFlight() { return this.packets.length; }

  draw(ctx: CanvasRenderingContext2D, state: SceneState, frame: SceneFrame) {
    const size=`${frame.width}x${frame.height}`;
    if(state!==this.shown||size!==this.shownSize){
      const sx=frame.width/SCENE_WIDTH,sy=frame.height/SCENE_HEIGHT;
      this.view={sx,sy,u:Math.sqrt(sx*sy),hx:HUB_X*sx,hy:HUB_Y*sy,hr:HUB_R*Math.min(sx,sy)};
      this.channels=layout(state,this.view);this.byKey=new Map(this.channels.map((channel): [string,Channel]=>[channel.key,channel]));this.shown=state;this.shownSize=size;
    }
    this.advance(state,frame);
    ctx.setTransform(1,0,0,1,0,0);ctx.globalCompositeOperation='source-over';ctx.globalAlpha=1;
    ctx.clearRect(0,0,frame.width,frame.height);
    ctx.drawImage(this.backdropFor(frame.width,frame.height),0,0);
    ctx.globalCompositeOperation='lighter';
    this.ambient(ctx,state.live);
    for(const channel of this.channels)this.channel(ctx,channel,state.live);
    this.hub(ctx,state.live);
    this.flight(ctx);
    this.ripples(ctx);
    this.debris(ctx);
    for(const channel of this.channels)this.ports(ctx,channel,state.live);
    ctx.setTransform(1,0,0,1,0,0);ctx.globalCompositeOperation='source-over';ctx.globalAlpha=1;
  }

  private advance(state: SceneState, {dt,motion,now}: SceneFrame) {
    // Fades run only while animating; paused or reduced-motion frames show the current state at once.
    const ease=(rate: number)=>motion&&state.live?1-Math.exp(-dt*rate):1,keys=new Set<string>();
    let total=0;
    this.clock+=dt;this.spin+=dt*(.35+.9*this.energy);this.charge*=Math.exp(-dt*1.4);
    for(const channel of this.channels){
      channel.running=channel.ready&&!!channel.stamp&&now-channel.stamp<RECENT_MS;
      const old=this.levels.get(channel.key)??0,level=old+(Number(channel.running)-old)*ease(3),seen=this.stamps.get(channel.key);
      this.levels.set(channel.key,level);total+=level;keys.add(channel.key);
      if(channel.stamp===undefined)continue;
      this.stamps.set(channel.key,channel.stamp);
      if(motion&&channel.running&&channel.stamp>(seen??0))this.launch(channel);
    }
    for(const map of [this.levels,this.stamps])for(const key of map.keys())if(!keys.has(key))map.delete(key);
    this.energy+=((state.live?Math.min(1,total/Math.max(1,this.channels.length)*1.4):0)-this.energy)*ease(1.2);
    if(!state.live){this.packets=[];this.waves=[];this.sparks=[];this.charge=0;return;}
    if(!motion)return;
    // Arrivals brighten the core; the first packet of each burst sends a shockwave.
    const flying: Packet[]=[];
    for(const packet of this.packets){
      const channel=this.byKey.get(packet.key);
      if(!channel)continue;
      if(this.clock-packet.born<packet.life){flying.push(packet);continue;}
      if(!channel.out)this.charge=Math.min(1,this.charge+.05);
      if(packet.lead)this.waves.push({start:this.clock,color:channel.color,at:channel.out?channel.d:undefined});
      if(!channel.out||packet.lead)this.burst(channel.out?channel.d:[this.view.hx,this.view.hy],channel.color,channel.out?7:2,channel.out?0:this.view.hr*.3);
    }
    this.packets=flying;
    this.waves=this.waves.filter((wave)=>this.clock-wave.start<1.2).slice(-32);
    this.sparks=this.sparks.filter((spark)=>this.clock-spark.born<.7).slice(-240);
  }

  private launch(channel: Channel) {
    const count=burstSize(channel.delta)||3;
    // Spread the burst over the next snapshot interval; outputs leave once inputs have reached the hub.
    for(let i=0;i<count;i++){
      const seed=++this.seq;
      this.packets.push({key:channel.key,born:this.clock+(i+hash(seed)*.8)/count*.95+(channel.out?OUTPUT_DELAY:0),life:(channel.out?1.7:1.9)*(.88+hash(seed*1.7)*.24),spread:hash(seed*2.3)*2-1,lead:i===0});
    }
    if(this.packets.length>900)this.packets.splice(0,this.packets.length-900);
  }

  // Sparks thrown off where data lands: the core for inputs, the client port for outputs.
  private burst([x,y]: Point, color: string, count: number, offset: number) {
    const {u}=this.view;
    for(let i=0;i<count;i++){
      const seed=++this.seq,angle=hash(seed*3.1)*Math.PI*2,speed=(70+hash(seed*4.7)*120)*u;
      this.sparks.push({x:x+Math.cos(angle)*offset,y:y+Math.sin(angle)*offset,vx:Math.cos(angle)*speed,vy:Math.sin(angle)*speed,born:this.clock,color});
    }
  }

  // Path: channel then an inward spiral for inputs; outward spiral then channel for outputs.
  private at(channel: Channel, p: number, spread: number): Point {
    const {hx,hy,hr,u}=this.view;
    if(!channel.out&&p>1-SPIRAL){const s=(p-1+SPIRAL)/SPIRAL,angle=channel.angle+1.8*s,r=hr*(1-.84*s);return [hx+Math.cos(angle)*r,hy+Math.sin(angle)*r];}
    if(channel.out&&p<SPIRAL){const s=p/SPIRAL,angle=channel.angle-1.8*(1-s),r=hr*(.16+.84*s);return [hx+Math.cos(angle)*r,hy+Math.sin(angle)*r];}
    const t=channel.out?(p-SPIRAL)/(1-SPIRAL):p/(1-SPIRAL),[x,y]=bezier(channel.a,channel.b,channel.c,channel.d,t),[tx,ty]=tangent(channel.a,channel.b,channel.c,channel.d,t);
    const norm=Math.hypot(tx,ty)||1,offset=spread*6*u*Math.sin(Math.PI*t);
    return [x-ty/norm*offset,y+tx/norm*offset];
  }

  private channel(ctx: CanvasRenderingContext2D, channel: Channel, live: boolean) {
    const {u}=this.view,level=this.levels.get(channel.key)??0;
    const path=(until=1)=>{
      ctx.beginPath();ctx.moveTo(...channel.a);
      if(until>=1){ctx.bezierCurveTo(...channel.b,...channel.c,...channel.d);return;}
      for(let i=1;i<=24;i++)ctx.lineTo(...bezier(channel.a,channel.b,channel.c,channel.d,until*i/24));
    };
    if(!live||channel.link==='off'){
      path();ctx.setLineDash([2*u,7*u]);ctx.strokeStyle=live?INK.off:INK.stale;ctx.globalAlpha=.5;ctx.lineWidth=1.2*u;ctx.stroke();ctx.setLineDash([]);
      return;
    }
    if(channel.link==='down'){
      // A broken link: the red stub stops well short of the hub.
      path(.42);ctx.setLineDash([6*u,6*u]);ctx.strokeStyle=INK.red;ctx.globalAlpha=.6;ctx.lineWidth=1.6*u;ctx.stroke();ctx.setLineDash([]);
      const [x,y]=bezier(channel.a,channel.b,channel.c,channel.d,.46),s=5*u;
      ctx.globalAlpha=.95;ctx.lineWidth=2*u;ctx.beginPath();ctx.moveTo(x-s,y-s);ctx.lineTo(x+s,y+s);ctx.moveTo(x+s,y-s);ctx.lineTo(x-s,y+s);ctx.stroke();
      return;
    }
    ctx.strokeStyle=channel.color;
    path();ctx.globalAlpha=.16+(channel.selected?.2:0);ctx.lineWidth=(channel.selected?2.2:1.5)*u;ctx.stroke();
    if(level<.01)return;
    for(const [width,strength] of [[24,.05],[10,.1],[2.8,.55]]){path();ctx.globalAlpha=strength*level;ctx.lineWidth=width*u;ctx.stroke();}
    // Current markers run toward the clients only while real updates arrive.
    path();ctx.setLineDash([2*u,18*u]);ctx.lineDashOffset=-this.clock*95*u;ctx.strokeStyle='#ffffff';ctx.globalAlpha=.6*level;ctx.lineWidth=2*u;ctx.stroke();ctx.setLineDash([]);ctx.lineDashOffset=0;
  }

  private hub(ctx: CanvasRenderingContext2D, live: boolean) {
    const {u,hx,hy,hr:R}=this.view,e=this.energy,c=this.charge,ink=live?INK.hub:INK.stale,dim=live?1:.4,glow=this.glow(ink);
    const ring=(r: number,width: number,alpha: number,from=0,to=Math.PI*2)=>{ctx.globalAlpha=alpha*dim;ctx.lineWidth=width*u;ctx.beginPath();ctx.arc(hx,hy,r,from,to);ctx.stroke();};
    const halo=ctx.createRadialGradient(hx,hy,0,hx,hy,R*3.4);
    halo.addColorStop(0,withAlpha(ink,(.16+.14*e+.3*c)*dim));halo.addColorStop(.4,withAlpha(ink,.05*dim));halo.addColorStop(1,withAlpha(ink,0));
    ctx.globalAlpha=1;ctx.fillStyle=halo;ctx.fillRect(hx-R*3.4,hy-R*3.4,R*6.8,R*6.8);
    // Anamorphic flare widens with each arrival.
    const flareR=R*(3+2.5*c);
    ctx.save();ctx.translate(hx,hy);ctx.scale(1,.045);
    const flare=ctx.createRadialGradient(0,0,0,0,0,flareR);
    flare.addColorStop(0,withAlpha('#ffffff',(.3+.55*c)*dim));flare.addColorStop(.3,withAlpha(ink,(.16+.3*c)*dim));flare.addColorStop(1,withAlpha(ink,0));
    ctx.fillStyle=flare;ctx.beginPath();ctx.arc(0,0,flareR,0,Math.PI*2);ctx.fill();ctx.restore();
    ctx.strokeStyle=ink;
    ring(R,1.6,.75);
    ctx.beginPath();
    for(let n=0;n<72;n++){const a=n/72*Math.PI*2+this.spin*.25,r2=R*(n%6?1.1:1.17);ctx.moveTo(hx+Math.cos(a)*R*1.05,hy+Math.sin(a)*R*1.05);ctx.lineTo(hx+Math.cos(a)*r2,hy+Math.sin(a)*r2);}
    ctx.globalAlpha=.45*dim;ctx.lineWidth=1.2*u;ctx.stroke();
    for(let k=0;k<3;k++){const a=-this.spin*.5+k*Math.PI*2/3;ring(R*1.34,3,.55+.3*e,a,a+1.25);ring(R*1.34,1,.35,a+1.45,a+1.75);}
    ctx.setLineDash([1.2*u,7*u]);ctx.lineDashOffset=-this.spin*10*u;ring(R*1.64,1.6,.5);ctx.setLineDash([]);ctx.lineDashOffset=0;
    for(let k=0;k<4;k++){const a=this.spin*.12+k*Math.PI/2;ring(R*1.98,1.4,.45,a-.16,a+.16);}
    ring(R*1.98,.8,.12);
    // Three tilted orbits give the core depth.
    for(let k=0;k<3;k++){
      const t=this.spin*(1.1+k*.25)+k*2.1,s=12*u;
      ctx.save();ctx.translate(hx,hy);ctx.rotate(k*Math.PI/3+this.spin*.06);
      ctx.globalAlpha=.24*dim;ctx.lineWidth=u;ctx.beginPath();ctx.ellipse(0,0,R*1.18,R*.34,0,0,Math.PI*2);ctx.stroke();
      ctx.globalAlpha=(.55+.4*Math.sin(t))*dim;ctx.drawImage(glow,Math.cos(t)*R*1.18-s/2,Math.sin(t)*R*.34-s/2,s,s);
      ctx.restore();
    }
    const coreR=R*(.52+.16*c+.04*Math.sin(this.spin*2.2)),core=ctx.createRadialGradient(hx,hy,0,hx,hy,coreR);
    core.addColorStop(0,withAlpha('#ffffff',.95*dim));core.addColorStop(.22,withAlpha(ink,.85*dim));core.addColorStop(.6,withAlpha(ink,.22*dim));core.addColorStop(1,withAlpha(ink,0));
    ctx.globalAlpha=1;ctx.fillStyle=core;ctx.beginPath();ctx.arc(hx,hy,coreR,0,Math.PI*2);ctx.fill();
    // Radar sweep inside the rim and rings breathing out of the core.
    const sweep=this.spin*1.6;ctx.fillStyle=ink;
    for(let i=0;i<14;i++){const a=sweep-i*.05;ctx.globalAlpha=.16*(1-i/14)*dim;ctx.beginPath();ctx.moveTo(hx,hy);ctx.arc(hx,hy,R*.97,a-.05,a);ctx.closePath();ctx.fill();}
    ctx.strokeStyle='#ffffff';
    for(let k=0;k<2;k++){const p=fract(this.spin*.6+k*.5);ring(R*(.22+.6*p),1.2,(1-p)*(.35+.4*c));}
  }

  private flight(ctx: CanvasRenderingContext2D) {
    const {u}=this.view,spark=this.glow('#ffffff');
    for(const packet of this.packets){
      const channel=this.byKey.get(packet.key),p=(this.clock-packet.born)/packet.life;
      if(!channel||p<0||p>=1)continue;
      const glow=this.glow(channel.color),fade=Math.min(1,p*12,(1-p)*10),scale=packet.lead?1.45:1;
      for(let k=TRAIL-1;k>=0;k--){
        const q=p-k*.009;
        if(q<0)continue;
        const [x,y]=this.at(channel,q,packet.spread),size=22*u*scale*(1-k/TRAIL*.78);
        ctx.globalAlpha=fade*(1-k/TRAIL)**1.4*(k?.55:1);ctx.drawImage(glow,x-size/2,y-size/2,size,size);
      }
      const [x,y]=this.at(channel,p,packet.spread),size=9*u*scale;
      ctx.globalAlpha=fade;ctx.drawImage(spark,x-size/2,y-size/2,size,size);
    }
  }

  private debris(ctx: CanvasRenderingContext2D) {
    const {u}=this.view;
    for(const spark of this.sparks){
      const age=this.clock-spark.born,p=age/.7,drag=1-p*.5,x=spark.x+spark.vx*age*drag,y=spark.y+spark.vy*age*drag,size=(7-4*p)*u;
      ctx.globalAlpha=(1-p)**1.5;ctx.strokeStyle=spark.color;ctx.lineWidth=1.4*u;
      ctx.beginPath();ctx.moveTo(x-spark.vx*.05,y-spark.vy*.05);ctx.lineTo(x,y);ctx.stroke();
      ctx.drawImage(this.glow(spark.color),x-size/2,y-size/2,size,size);
    }
  }

  private ripples(ctx: CanvasRenderingContext2D) {
    const {u,hx,hy,hr}=this.view;
    for(const wave of this.waves){
      const p=(this.clock-wave.start)/1.2;
      if(p<0||p>=1)continue;
      const [x,y]=wave.at??[hx,hy],r=wave.at?(6+p*46)*u:hr*(.5+p*2.2);
      ctx.strokeStyle=wave.color;ctx.globalAlpha=(1-p)**2*.9;ctx.lineWidth=(wave.at?2:3.2)*u*(1-p*.6);
      ctx.beginPath();ctx.arc(x,y,r,0,Math.PI*2);ctx.stroke();
    }
  }

  private ports(ctx: CanvasRenderingContext2D, channel: Channel, live: boolean) {
    const {u}=this.view,level=this.levels.get(channel.key)??0,color=!live?INK.stale:channel.link==='down'?INK.red:channel.link==='off'?INK.off:channel.color,glow=this.glow(color);
    const [px,py]=channel.out?channel.d:channel.a,[ex,ey]=channel.out?channel.a:channel.d;
    let size=(22+16*level)*u;
    ctx.globalAlpha=.35+.55*level;ctx.drawImage(glow,px-size/2,py-size/2,size,size);
    ctx.strokeStyle=color;ctx.globalAlpha=.85;ctx.lineWidth=1.5*u;ctx.beginPath();ctx.arc(px,py,4.5*u,0,Math.PI*2);ctx.stroke();
    if(channel.selected){
      ctx.lineWidth=1.2*u;ctx.setLineDash([4*u,4*u]);ctx.lineDashOffset=-this.spin*12*u;
      ctx.beginPath();ctx.arc(px,py,12*u,0,Math.PI*2);ctx.stroke();ctx.setLineDash([]);ctx.lineDashOffset=0;
    }
    if(channel.link!=='down'){size=(12+12*level)*u;ctx.globalAlpha=.3+.6*level;ctx.drawImage(glow,ex-size/2,ey-size/2,size,size);}
    // Real samples or notifications counted in the last snapshot.
    if(level<.05||!channel.delta)return;
    ctx.globalCompositeOperation='source-over';ctx.font=`600 ${Math.round(12*u)}px ui-monospace, SFMono-Regular, Consolas, monospace`;ctx.textBaseline='bottom';ctx.textAlign=channel.out?'right':'left';
    ctx.globalAlpha=level;ctx.fillStyle=color;ctx.fillText(`+${channel.delta.toLocaleString()}`,px+(channel.out?-12:12)*u,py-8*u);
    ctx.globalCompositeOperation='lighter';
  }

  private ambient(ctx: CanvasRenderingContext2D, live: boolean) {
    const mote=this.glow(live?INK.star:INK.stale),{u,sx,sy}=this.view;
    for(let i=0;i<48;i++){
      // Dust drifts upward so it is never read as left-to-right data flow.
      const depth=hash(i*4.7),size=(3+depth*7)*u;
      const x=hash(i*1.3)*SCENE_WIDTH+Math.sin(this.clock*.23+i)*16,y=(1-fract(hash(i*2.9)+this.clock*(5+depth*16)/SCENE_HEIGHT))*SCENE_HEIGHT;
      ctx.globalAlpha=(.04+.12*(.5+.5*Math.sin(this.clock*(.6+depth)+i*2.3)))*(live?1:.5);
      ctx.drawImage(mote,x*sx-size/2,y*sy-size/2,size,size);
    }
  }

  private backdropFor(width: number, height: number) {
    const key=`${width}x${height}`;
    if(this.backdrop&&this.backdropKey===key)return this.backdrop;
    const canvas=this.backdrop??sheet(width,height);
    canvas.width=width;canvas.height=height;this.backdrop=canvas;this.backdropKey=key;
    const g=canvas.getContext('2d');
    if(!g)return canvas;
    const {u,hx,hy,hr}=this.view;
    for(let i=0;i<260;i++){g.globalAlpha=.1+hash(i*7.7)*.4;g.fillStyle=INK.star;g.beginPath();g.arc(hash(i*3.7)*width,hash(i*9.1)*height,(.4+hash(i*5.3)*1.1)*u,0,Math.PI*2);g.fill();}
    // Static perspective floor under the hub, faded towards the horizon and the sides.
    const horizon=hy+hr*2.4,floor=sheet(width,height),f=floor.getContext('2d');
    if(f){
      f.strokeStyle=INK.hub;f.lineWidth=u;f.globalAlpha=.16;f.beginPath();
      for(let k=-24;k<=24;k++){f.moveTo(hx+k*width*.004,horizon);f.lineTo(hx+k*width*.06,height);}
      for(let i=1;i<=12;i++){const y=horizon+(height-horizon)*(i/12)**1.8;f.moveTo(0,y);f.lineTo(width,y);}
      f.stroke();
      f.globalCompositeOperation='destination-in';f.globalAlpha=1;
      const down=f.createLinearGradient(0,horizon,0,height);down.addColorStop(0,'#0000');down.addColorStop(.5,'#000c');down.addColorStop(1,'#000');f.fillStyle=down;f.fillRect(0,horizon,width,height-horizon);
      const across=f.createLinearGradient(0,0,width,0);across.addColorStop(0,'#0000');across.addColorStop(.5,'#000');across.addColorStop(1,'#0000');f.fillStyle=across;f.fillRect(0,0,width,height);
      g.globalAlpha=1;g.drawImage(floor,0,0);
    }
    const line=g.createLinearGradient(0,0,width,0);line.addColorStop(0,withAlpha(INK.hub,0));line.addColorStop(.5,withAlpha(INK.hub,.35));line.addColorStop(1,withAlpha(INK.hub,0));
    g.globalAlpha=1;g.fillStyle=line;g.fillRect(0,horizon-.5*u,width,u);
    return canvas;
  }

  private glow(color: string): HTMLCanvasElement {
    let sprite=this.glows.get(color);
    if(!sprite){sprite=glowSprite(color);this.glows.set(color,sprite);}
    return sprite;
  }
}
