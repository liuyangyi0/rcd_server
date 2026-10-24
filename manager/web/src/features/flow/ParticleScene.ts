import { sourceCanFlow, type Activity, type FlowSession, type FlowSource } from './model';

export const SCENE_WIDTH = 1680, SCENE_HEIGHT = 750;
const CX = -480, CY = 375, INNER = 1200, OUTER = 1535;
export const laneY = (index: number, count: number) => 48 + (index + 0.5) * (654 / Math.max(count, 1));
const arcX = (y: number, r: number) => CX + Math.sqrt(r*r-(y-CY)**2);
type Point = [number,number];
export interface SceneState { sources: FlowSource[]; sessions: FlowSession[]; activity: Activity; live: boolean; paused: boolean; selected?: string; cacheActive?: boolean }
function position(a: Point,b: Point,c: Point,d: Point,t: number): Point {
  const u=1-t;
  return [u*u*u*a[0]+3*u*u*t*b[0]+3*u*t*t*c[0]+t*t*t*d[0],u*u*u*a[1]+3*u*u*t*b[1]+3*u*t*t*c[1]+t*t*t*d[1]];
}

export function makeParticleSprite(): HTMLCanvasElement {
  const sprite=document.createElement('canvas');sprite.width=sprite.height=48;
  const ctx=sprite.getContext('2d');
  if(ctx){const glow=ctx.createRadialGradient(24,24,0,24,24,24);glow.addColorStop(0,'#eafff9');glow.addColorStop(.12,'#bbfff2');glow.addColorStop(.3,'#70e8dc88');glow.addColorStop(1,'#70e8dc00');ctx.fillStyle=glow;ctx.fillRect(0,0,48,48);}
  return sprite;
}

// Compositing is limited to the canvas. No DOM geometry changes while rings
// rotate; displayed labels remain stable and all activity comes from counters.
export function drawScene(ctx: CanvasRenderingContext2D, state: SceneState, seconds: number, now: number, sprite: HTMLCanvasElement) {
  const active=state.live&&!state.paused;
  ctx.clearRect(0,0,SCENE_WIDTH,SCENE_HEIGHT);
  ctx.globalCompositeOperation='source-over';ctx.globalAlpha=1;
  const shade=ctx.createRadialGradient(855,375,20,855,375,850);shade.addColorStop(0,'#17302b44');shade.addColorStop(.55,'#0b191e18');shade.addColorStop(1,'#03070b00');ctx.fillStyle=shade;ctx.fillRect(0,0,SCENE_WIDTH,SCENE_HEIGHT);
  ctx.strokeStyle='#688d8b';ctx.lineWidth=.45;ctx.globalAlpha=.06;
  for(let y=30;y<SCENE_HEIGHT;y+=32){ctx.beginPath();ctx.moveTo(0,y);ctx.lineTo(SCENE_WIDTH,y);ctx.stroke();}
  // Ambient curved surface and counter-rotating tracks are visual decoration.
  ctx.globalAlpha=1;
  const band=ctx.createLinearGradient(650,0,1080,0);band.addColorStop(0,'#0b171c');band.addColorStop(.2,'#162629');band.addColorStop(.58,'#0c141a');band.addColorStop(.9,'#172a2a');band.addColorStop(1,'#102522');
  ctx.beginPath();ctx.arc(CX,CY,OUTER,-.34,.34);ctx.arc(CX,CY,INNER,.34,-.34,true);ctx.closePath();ctx.fillStyle=band;ctx.fill();
  for(const [index,r] of [INNER-12,INNER,INNER+8,INNER+31,OUTER-38,OUTER-17,OUTER,OUTER+7].entries()) {
    ctx.globalAlpha=index===1||index===6?.9:.25;ctx.strokeStyle=index===1||index===6?'#9df5d8':'#77a5a2';ctx.lineWidth=index===1?2.4:index===6?3:1;
    ctx.beginPath();ctx.arc(CX,CY,r,-.34,.34);ctx.stroke();
    if(index!==1&&index!==6)continue;
    ctx.globalCompositeOperation='lighter';ctx.shadowColor='#9effd6';ctx.shadowBlur=11;
    ctx.globalAlpha=.38;ctx.lineWidth=6;ctx.beginPath();ctx.arc(CX,CY,r,-.34,.34);ctx.stroke();ctx.shadowBlur=0;ctx.globalCompositeOperation='source-over';
    for(let n=0;n<180;n++){
      const a=((n/180*.7+seconds*(index===1?.023:-.017)+7)%.7)-.35;
      const r1=r+(index===1?11:-11),r2=r1+(index===1?1:-1)*(n%6===0?15:7);
      ctx.globalAlpha=n%6===0?.9:.34;ctx.lineWidth=n%6===0?2.5:1;
      ctx.beginPath();ctx.moveTo(CX+Math.cos(a)*r1,CY+Math.sin(a)*r1);ctx.lineTo(CX+Math.cos(a)*r2,CY+Math.sin(a)*r2);ctx.stroke();
    }
  }
  for(let column=0;column<5;column++){
    const r=INNER+48+column*53;
    for(let row=0;row<62;row++){
      const a=((row/62*.69+seconds*(column%2?.009:-.012)+6.9)%.69)-.345;
      ctx.save();ctx.translate(CX+Math.cos(a)*r,CY+Math.sin(a)*r);ctx.rotate(a);ctx.globalAlpha=column===4?.34:.075;ctx.fillStyle='#b5ffe7';
      const length=10+((row*17+column*7)%27);ctx.fillRect(0,-1,length,1.5);
      if(row%3===0)ctx.fillRect(length+3,-1,3,1.5);
      ctx.restore();
    }
  }
  function stream(a:Point,b:Point,c:Point,d:Point,color:string,running:boolean,seed:number,strong:boolean,output=false){
    ctx.globalCompositeOperation='source-over';ctx.lineWidth=strong?1.1:.55;ctx.strokeStyle=color;ctx.globalAlpha=running?(strong?.37:.17):.095;
    ctx.beginPath();ctx.moveTo(...a);ctx.bezierCurveTo(...b,...c,...d);ctx.stroke();
    if(!running)return;
    ctx.globalCompositeOperation='lighter';
    const speed=(output?.22:.17)+(seed%7)*.007,count=output?3:2;
    for(let n=0;n<count;n++){
      const t=(seconds*speed+seed*.073+n/count)%1,size=seed%7===0?2.6:1.25,tail=output?.065:.042;
      const start=position(a,b,c,d,Math.max(0,t-tail)),head=position(a,b,c,d,t);
      const gradient=ctx.createLinearGradient(...start,...head);gradient.addColorStop(0,'#79edd000');gradient.addColorStop(.65,color);gradient.addColorStop(1,'#f3fff8');
      ctx.globalAlpha=strong?.95:.74;ctx.strokeStyle=gradient;ctx.lineWidth=size;
      ctx.beginPath();ctx.moveTo(...start);
      for(let k=1;k<=5;k++)ctx.lineTo(...position(a,b,c,d,Math.max(0,t-tail+tail*k/5)));
      ctx.stroke();
      ctx.globalAlpha=seed%4===0?.9:.5;ctx.drawImage(sprite,head[0]-size*5,head[1]-size*5,size*10,size*10);
      ctx.globalAlpha=.95;ctx.fillStyle='#edfff6';ctx.fillRect(head[0]-size/2,head[1]-.5,size,1);
    }
  }
  state.sources.forEach((source,index)=>{
    const y=laneY(index,state.sources.length),x=arcX(y,INNER);
    const recent=!!state.activity.sources[source.id]&&now-state.activity.sources[source.id]<3200;
    const running=active&&sourceCanFlow(source)&&recent;
    const devices=source.ports.flatMap((p)=>p.devices).slice(0,12);
    const color=!source.enabled?'#6e828c':!source.connected?'#dc797f':source.catalog_state==='CatalogConflict'?'#e0b164':'#8ae4c9';
    for(let lane=0;lane<68;lane++){
      const spread=(lane-34)*2.35,startY=y+spread*.85,device=devices.length?devices[lane%devices.length]:undefined;
      const stationActive=!device||device.state===1,tint=device&&(device.state===2||device.state===3)?'#e4ac66':color;
      stream([306,startY],[415,startY+(lane%2?-40:40)],[x-155,y+spread*.8],[x,y],tint,running&&stationActive,lane+index*79,state.selected===source.id||running);
    }
    ctx.globalCompositeOperation='lighter';ctx.globalAlpha=.85;ctx.strokeStyle=color;ctx.lineWidth=2;ctx.beginPath();ctx.arc(x,y,8,0,Math.PI*2);ctx.stroke();ctx.globalAlpha=.24;ctx.lineWidth=4;ctx.beginPath();ctx.arc(x,y,13,0,Math.PI*2);ctx.stroke();ctx.drawImage(sprite,x-18,y-18,36,36);
    ctx.globalCompositeOperation='source-over';ctx.globalAlpha=1;ctx.fillStyle='#0a181c';ctx.strokeStyle='#5b8a87';ctx.lineWidth=.7;ctx.fillRect(x-87,y-12,71,24);ctx.strokeRect(x-87,y-12,71,24);ctx.font='11px ui-monospace, monospace';ctx.fillStyle='#d3ede3';ctx.fillText(source.id.slice(0,13),x-81,y+4);
  });
  const outputs=state.sessions.length?state.sessions.map((session,index)=>({y:laneY(index,state.sessions.length),running:active&&session.attached&&!!state.activity.sessions[session.id]&&now-state.activity.sessions[session.id]<3200,attached:session.attached})): [{y:CY,running:active&&!!state.cacheActive,attached:true}];
  outputs.forEach((output,index)=>{
    const x=arcX(output.y,OUTER);
    for(let lane=0;lane<92;lane++){
      const spread=(lane-46)*(state.sessions.length?2.1:4.5),endY=output.y+spread;
      stream([x,output.y+spread*.15],[x+95,output.y+spread*.28],[1260,endY],[1470,endY],output.attached?'#8cebd3':'#6e828c',output.running,lane+index*103,false,true);
    }
  });
  ctx.globalCompositeOperation='source-over';ctx.globalAlpha=1;
}
