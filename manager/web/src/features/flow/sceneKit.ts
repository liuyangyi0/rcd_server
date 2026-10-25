// Drawing helpers shared by the pipeline and matrix scenes.
export const SCENE_WIDTH = 1680, SCENE_HEIGHT = 750, RECENT_MS = 3200;
// Structure is neutral slate; the accent only appears where real data is moving.
export const INK = { data: '#59c7ff', out: '#8fb4ff', ok: '#3fd6a0', line: '#22324a', rule: '#3a4f68', amber: '#f0b45a', red: '#ff6f6f', off: '#56657a', idle: '#2c3b4f', stale: '#3a4656', text: '#e6edf5', muted: '#7d8ea3', plate: '#0a121c' };
export const fract = (value: number) => value - Math.floor(value);
export const clamp = (value: number, low: number, high: number) => Math.max(low, Math.min(high, value));
export const easeOut = (value: number) => 1 - (1 - clamp(value, 0, 1)) ** 3;
// Deterministic noise: paused and reduced-motion frames must be pixel-stable.
export const hash = (seed: number) => fract(Math.sin(seed * 127.1 + 311.7) * 43758.5453);
export const withAlpha = (hex: string, alpha: number) => hex + Math.round(clamp(alpha, 0, 1) * 255).toString(16).padStart(2, '0');
// Packets per snapshot follow the logarithm of real updates, so 10 and 10 000 samples both stay readable.
export const burstSize = (delta: number) => delta > 0 ? Math.min(18, 3 + Math.round(5 * Math.log10(1 + delta))) : 0;
export type Point = [number,number];
// ok carries data; warn = every station on the port abnormal; idle = no station in normal communication.
export type Tone = 'ok' | 'warn' | 'idle' | 'down' | 'off';
export interface SceneFrame { width: number; height: number; dt: number; motion: boolean; now: number }
export interface SceneCalculation { active: boolean; good: number; bad: number; evaluated?: string }
export interface View { sx: number; sy: number; u: number }
export interface Track { pts: Point[]; acc: number[]; total: number }
export interface Wave { start: number; color: string; at: Point }
export interface Spark { x: number; y: number; vx: number; vy: number; born: number; color: string }
// A real state change between snapshots: a station turning abnormal or recovering, an OPC link breaking or joining.
export interface Shift { key: string; kind: 'warn' | 'recover' | 'break' | 'join'; start: number; landed?: boolean }
export const SHIFT_S = { warn: 1.6, recover: 1.2, break: .8, join: 1 };
// A source or Session whose activity follows the real snapshot counters.
export interface Flow { key: string; ready: boolean; stamp?: number; running: boolean }
const BOOT_S = 2.2;

export function track(pts: Point[]): Track {
  const acc=[0];
  for(let i=1;i<pts.length;i++)acc.push(acc[i-1]+Math.hypot(pts[i][0]-pts[i-1][0],pts[i][1]-pts[i-1][1]));
  return {pts,acc,total:acc[acc.length-1]};
}
export function along(t: Track, distance: number): Point {
  const s=clamp(distance,0,t.total);let low=0,high=t.acc.length-1;
  while(high-low>1){const mid=(low+high)>>1;if(t.acc[mid]<=s)low=mid;else high=mid;}
  const span=t.acc[high]-t.acc[low]||1,k=(s-t.acc[low])/span,a=t.pts[low],b=t.pts[high];
  return [a[0]+(b[0]-a[0])*k,a[1]+(b[1]-a[1])*k];
}
// The leading part of a track, used while links are drawn in or retract.
export function partial(t: Track, portion: number): Track {
  if(portion>=1)return t;
  const end=t.total*Math.max(0,portion),pts: Point[]=[];
  for(let i=0;i<t.pts.length&&t.acc[i]<end;i++)pts.push(t.pts[i]);
  pts.push(along(t,end));
  return track(pts.length>1?pts:[t.pts[0],t.pts[0]]);
}
export function sheet(width: number, height: number) {
  const canvas=document.createElement('canvas');canvas.width=width;canvas.height=height;
  return canvas;
}
function glowSprite(color: string) {
  const sprite=sheet(64,64),g=sprite.getContext('2d');
  if(g){const glow=g.createRadialGradient(32,32,0,32,32,32);glow.addColorStop(0,'#ffffff');glow.addColorStop(.14,withAlpha(color,1));glow.addColorStop(.38,withAlpha(color,.3));glow.addColorStop(1,withAlpha(color,0));g.fillStyle=glow;g.fillRect(0,0,64,64);}
  return sprite;
}

// The lifecycle both scenes share (clock, opening sequence, activity levels, flashes, state
// changes between snapshots and transient effects) plus the line primitives they draw with.
export abstract class SceneBase {
  protected clock=0; protected seq=0; protected focus?: string; protected reveal=1;
  protected view: View={sx:1,sy:1,u:1};
  protected waves: Wave[]=[]; protected sparks: Spark[]=[]; protected shifts: Shift[]=[];
  protected levels=new Map<string,number>(); protected stamps=new Map<string,number>(); protected flashes=new Map<string,number>();
  private memory=new Map<string,string>(); private bootClock=0; private booted=false;
  private glows=new Map<string,HTMLCanvasElement>();

  // Hovering a source or Session keeps its route bright and dims the rest.
  protected weight(id?: string) { return !this.focus||id===this.focus?1:.25; }
  protected level(key: string) { return this.levels.get(key)??0; }
  // Opening sequence progress between two points of the reveal, eased.
  protected phase(from: number, to: number) { return easeOut((this.reveal-from)/(to-from)); }
  protected shift(key: string) { return this.shifts.find((item)=>item.key===key); }
  protected flash(key: string, amount: number) { this.flashes.set(key,Math.min(1,(this.flashes.get(key)??0)+amount)); }

  // Advances the clock, the opening sequence and flash decay. The opening sequence plays once and
  // starts with the first snapshot that has something to show; a still frame skips straight to the end.
  protected tick(dt: number, motion: boolean, content: boolean) {
    this.clock+=dt;
    if(!motion)this.booted=true;
    if(content)this.bootClock+=dt;
    this.reveal=this.booted?1:clamp(this.bootClock/BOOT_S,0,1);
    if(this.reveal>=1)this.booted=true;
    // Faded flashes are dropped, so sources and Sessions that come and go leave nothing behind.
    for(const [key,value] of this.flashes){const next=value*Math.exp(-dt*3.5);if(next<.005)this.flashes.delete(key);else this.flashes.set(key,next);}
  }

  // Remembers a snapshot value; returns the previous one when it changed since the last snapshot.
  protected changed(key: string, value: string, seen: Set<string>) {
    seen.add(key);
    const old=this.memory.get(key);this.memory.set(key,value);
    return old!==undefined&&old!==value?old:undefined;
  }
  protected forget(seen: Set<string>) { for(const key of this.memory.keys())if(!seen.has(key))this.memory.delete(key); }

  // Eases a flow's level toward its running state; launch fires once per new activity stamp while animating.
  protected follow(flow: Flow, now: number, motion: boolean, ease: number, keys: Set<string>, launch: ()=>void) {
    flow.running=flow.ready&&!!flow.stamp&&now-flow.stamp<RECENT_MS;
    const old=this.level(flow.key),seen=this.stamps.get(flow.key);
    this.levels.set(flow.key,old+(Number(flow.running)-old)*ease);keys.add(flow.key);
    if(flow.stamp===undefined)return;
    this.stamps.set(flow.key,flow.stamp);
    if(motion&&flow.running&&flow.stamp>(seen??0))launch();
  }
  // Levels and stamps of flows that left the scene are dropped.
  protected prune(keys: Set<string>) { for(const map of [this.levels,this.stamps])for(const key of map.keys())if(!keys.has(key))map.delete(key); }
  protected clearEffects() { this.waves=[];this.sparks=[];this.shifts=[]; }
  // Drops effects that have played out.
  protected settle() {
    this.waves=this.waves.filter((wave)=>this.clock-wave.start<1.2).slice(-32);
    this.sparks=this.sparks.filter((spark)=>this.clock-spark.born<.7).slice(-240);
    this.shifts=this.shifts.filter((shift)=>this.clock-shift.start<SHIFT_S[shift.kind]).slice(-40);
  }

  protected burst([x,y]: Point, color: string, count: number) {
    const {u}=this.view;
    for(let i=0;i<count;i++){
      const seed=++this.seq,angle=hash(seed*3.1)*Math.PI*2,speed=(60+hash(seed*4.7)*110)*u;
      this.sparks.push({x,y,vx:Math.cos(angle)*speed,vy:Math.sin(angle)*speed,born:this.clock,color});
    }
  }

  protected stroke(ctx: CanvasRenderingContext2D, t: Track, color: string, alpha: number, width: number, dash?: number[]) {
    ctx.beginPath();ctx.moveTo(...t.pts[0]);for(let i=1;i<t.pts.length;i++)ctx.lineTo(...t.pts[i]);
    if(dash)ctx.setLineDash(dash);
    ctx.strokeStyle=color;ctx.globalAlpha=alpha;ctx.lineWidth=width;ctx.stroke();
    if(dash)ctx.setLineDash([]);
  }

  // A soft light segment ending at distance head, used for light-pipe waves and alarm pulses.
  protected comet(ctx: CanvasRenderingContext2D, t: Track, head: number, span: number, color: string, alpha: number, width: number) {
    ctx.strokeStyle=color;ctx.lineWidth=width;
    for(let k=0;k<8;k++){
      const from=head-span*(k+1)/8,to=head-span*k/8;
      if(to<=0||from>=t.total)continue;
      ctx.globalAlpha=alpha*(1-k/8)**1.5;ctx.beginPath();ctx.moveTo(...along(t,from));ctx.lineTo(...along(t,to));ctx.stroke();
    }
  }

  // A live link glows like a light pipe, with waves running toward the clients.
  protected energise(ctx: CanvasRenderingContext2D, t: Track, color: string, level: number, weight: number) {
    const {u}=this.view;
    if(level<.01||t.total<1)return;
    for(const [width,strength] of [[10,.06],[4,.14],[1.4,.85]])this.stroke(ctx,t,color,strength*level*weight,width*u);
    const span=Math.min(80*u,t.total*.5),gap=Math.max(t.total/2,240*u);
    for(let head=fract(this.clock*260*u/gap)*gap;head<t.total+span;head+=gap)this.comet(ctx,t,head,span,'#ffffff',.6*level*weight,2*u);
  }

  protected text(ctx: CanvasRenderingContext2D, value: string, x: number, y: number, color: string, alpha: number, size: number, max?: number, align: CanvasTextAlign='left') {
    const {u}=this.view;
    ctx.globalCompositeOperation='source-over';ctx.font=`500 ${Math.max(9,Math.round(size*u))}px ui-monospace, "SF Mono", "JetBrains Mono", Consolas, monospace`;ctx.textBaseline='middle';ctx.textAlign=align;
    let label=value;
    if(max)while(label.length>1&&ctx.measureText(label).width>max)label=label.slice(0,-1);
    ctx.globalAlpha=alpha;ctx.fillStyle=color;ctx.fillText(label===value?value:`${label.slice(0,-1)}…`,x,y);
    ctx.globalCompositeOperation='lighter';
  }

  protected ripples(ctx: CanvasRenderingContext2D) {
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

  protected glow(color: string): HTMLCanvasElement {
    let sprite=this.glows.get(color);
    if(!sprite){sprite=glowSprite(color);this.glows.set(color,sprite);}
    return sprite;
  }
}
