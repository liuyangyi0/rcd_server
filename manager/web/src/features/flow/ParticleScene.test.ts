import { afterAll, beforeAll, describe, expect, it } from 'vitest';
import { burstSize, FlowScene, type SceneState } from './ParticleScene';
import { flowFixture } from './fixture';

// Records every context call so frames can be compared without a real canvas.
function recorder() {
  const calls: string[]=[],target: Record<string,unknown>={};
  const show=(value: unknown)=>typeof value==='number'?value.toFixed(3):typeof value==='string'?value:typeof value;
  const ctx=new Proxy(target,{
    get:(_,key)=>(...args: unknown[])=>{calls.push(`${String(key)}(${args.map(show).join(',')})`);return key==='measureText'?{width:48}:{addColorStop:()=>undefined};},
    set:(_,key,value)=>{calls.push(`${String(key)}=${show(value)}`);return true;},
  });
  return {ctx:ctx as unknown as CanvasRenderingContext2D,calls};
}
const frame={width:1680,height:750,dt:0,motion:false,now:2000};
const recent={sources:{'rcd-46':1500},sessions:{'public-alias':1500},updates:5,sourceDeltas:{'rcd-46':5},sessionDeltas:{'public-alias':3}};
const original=HTMLCanvasElement.prototype.getContext;
beforeAll(()=>{HTMLCanvasElement.prototype.getContext=()=>null;});
afterAll(()=>{HTMLCanvasElement.prototype.getContext=original;});

describe('flow particle scene',()=>{
  it('repeats a still frame exactly while paused',()=>{
    const data=flowFixture(),scene=new FlowScene(),first=recorder(),second=recorder();
    const state: SceneState={sources:data.sources,sessions:data.sessions,activity:recent,live:true,paused:true,cacheActive:true,group:'serial-acquisition',focus:'rcd-46'};
    scene.draw(first.ctx,state,frame);scene.draw(second.ctx,state,{...frame,now:2400});
    expect(first.calls.length).toBeGreaterThan(100);
    expect(second.calls).toEqual(first.calls);
  });
  it('launches data packets only for live, recent updates',()=>{
    const data=flowFixture(),base={sources:data.sources,sessions:data.sessions,paused:false,group:'serial-acquisition'};
    const flying=(state: SceneState)=>{const scene=new FlowScene();for(let i=0;i<3;i++)scene.draw(recorder().ctx,state,{...frame,motion:true,dt:.05});return scene.inFlight;};
    expect(flying({...base,activity:recent,live:true,cacheActive:true})).toBeGreaterThan(0);
    expect(flying({...base,activity:recent,live:false,cacheActive:true})).toBe(0);
    expect(flying({...base,activity:{sources:{},sessions:{},updates:0,sourceDeltas:{},sessionDeltas:{}},live:true,cacheActive:false})).toBe(0);
    expect(flying({...base,activity:{...recent,sources:{'rcd-46':-3000},sessions:{'public-alias':-3000}},live:true,cacheActive:false})).toBe(0);
  });
  it('pulses the calculation only when its evaluation time changes',()=>{
    const data=flowFixture(),scene=new FlowScene(),idle={sources:{},sessions:{},updates:0,sourceDeltas:{},sessionDeltas:{}},calculation={active:true,good:9,bad:1,evaluated:'10:00:00'};
    const state: SceneState={sources:data.sources,sessions:[],activity:idle,live:true,paused:false,group:'serial-acquisition',defaultSource:'rcd-46',calculation};
    scene.draw(recorder().ctx,state,{...frame,motion:true,dt:.05});scene.draw(recorder().ctx,{...state},{...frame,motion:true,dt:.05});
    expect(scene.inFlight).toBe(0);
    scene.draw(recorder().ctx,{...state,calculation:{...calculation,evaluated:'10:00:01'}},{...frame,motion:true,dt:.05});
    expect(scene.inFlight).toBeGreaterThan(0);
  });
  it('sizes bursts by the logarithm of real updates',()=>{
    expect(burstSize(0)).toBe(0);
    expect(burstSize(1)).toBeLessThan(burstSize(100));
    expect(burstSize(100)).toBeLessThan(burstSize(10000));
    expect(burstSize(1e9)).toBe(18);
  });
});
