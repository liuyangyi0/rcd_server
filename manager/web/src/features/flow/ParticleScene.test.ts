import { afterAll, beforeAll, describe, expect, it } from 'vitest';
import { FlowScene, type SceneState } from './ParticleScene';
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
const identity='setTransform(1.000,0.000,0.000,1.000,0.000,0.000)';
const original=HTMLCanvasElement.prototype.getContext;
beforeAll(()=>{HTMLCanvasElement.prototype.getContext=()=>null;});
afterAll(()=>{HTMLCanvasElement.prototype.getContext=original;});

describe('flow particle scene',()=>{
  it('repeats a still frame exactly while paused',()=>{
    const data=flowFixture(),scene=new FlowScene(),first=recorder(),second=recorder();
    const state: SceneState={sources:data.sources,sessions:data.sessions,activity:{sources:{'rcd-46':1500},sessions:{'public-alias':1500},updates:5},live:true,paused:true,cacheActive:true};
    scene.draw(first.ctx,state,frame);scene.draw(second.ctx,state,{...frame,now:2400});
    expect(first.calls.length).toBeGreaterThan(100);
    expect(second.calls).toEqual(first.calls);
  });
  it('draws moving data marks only for live, recent activity',()=>{
    const data=flowFixture(),base={sources:data.sources,sessions:data.sessions,paused:false};
    const marks=(state: SceneState)=>{const r=recorder();new FlowScene().draw(r.ctx,state,frame);return r.calls.filter((call)=>call.startsWith('setTransform(')&&call!==identity).length;};
    const recent={sources:{'rcd-46':1500},sessions:{'public-alias':1500},updates:5};
    expect(marks({...base,activity:recent,live:true,cacheActive:true})).toBeGreaterThan(0);
    expect(marks({...base,activity:recent,live:false,cacheActive:true})).toBe(0);
    expect(marks({...base,activity:{sources:{},sessions:{},updates:0},live:true,cacheActive:false})).toBe(0);
    expect(marks({...base,activity:{sources:{'rcd-46':-3000},sessions:{'public-alias':-3000},updates:0},live:true,cacheActive:false})).toBe(0);
  });
});
