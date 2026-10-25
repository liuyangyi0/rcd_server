import { afterAll, beforeAll, describe, expect, it } from 'vitest';
import { MatrixScene, MX, mxColX, traceX, TRACE_CELLS, type MatrixState } from './MatrixScene';
import { flowFixture } from './fixture';
import type { TraceFrame } from './model';

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
const frame={width:1680,height:750,dt:0,motion:false,now:2000},moving={...frame,motion:true,dt:.05};
const recent={sources:{'rcd-46':1500},sessions:{'public-alias':1500},updates:5,sourceDeltas:{'rcd-46':5},sessionDeltas:{'public-alias':3}};
const idle={sources:{},sessions:{},updates:0,sourceDeltas:{},sessionDeltas:{}};
const frames: TraceFrame[]=[{at:'2026-10-24T10:00:00Z',notes:3,sources:{'rcd-46':{delta:5,tone:'ok'}}},{at:'2026-10-24T10:00:01Z',notes:0,sources:{'rcd-46':{delta:0,tone:'down'}}}];
const original=HTMLCanvasElement.prototype.getContext;
beforeAll(()=>{HTMLCanvasElement.prototype.getContext=()=>null;});
afterAll(()=>{HTMLCanvasElement.prototype.getContext=original;});
function twoSources() {
  const data=flowFixture(),other=structuredClone(data.sources[0]);
  other.id='rcd-58';data.sources.push(other);data.groups[0].members.push('rcd-58');
  return data;
}

describe('flow matrix scene',()=>{
  it('repeats a still frame exactly while paused',()=>{
    const data=flowFixture(),scene=new MatrixScene(),first=recorder(),second=recorder();
    const state: MatrixState={sources:data.sources,sessions:data.sessions,frames,activity:recent,live:true,paused:true,group:'serial-acquisition',defaultSource:'rcd-46',calculation:{active:true,good:9,bad:1}};
    scene.draw(first.ctx,state,frame);scene.draw(second.ctx,state,{...frame,now:2400});
    expect(first.calls.length).toBeGreaterThan(100);
    expect(second.calls).toEqual(first.calls);
  });
  it('launches packets only for live, recent updates',()=>{
    const data=flowFixture(),base={sources:data.sources,sessions:data.sessions,frames,paused:false,group:'serial-acquisition'};
    const flying=(state: MatrixState)=>{const scene=new MatrixScene();for(let i=0;i<3;i++)scene.draw(recorder().ctx,state,moving);return scene.inFlight;};
    expect(flying({...base,activity:recent,live:true})).toBeGreaterThan(0);
    expect(flying({...base,activity:recent,live:false})).toBe(0);
    expect(flying({...base,activity:idle,live:true})).toBe(0);
    expect(flying({...base,activity:{...recent,sources:{'rcd-46':-3000},sessions:{'public-alias':-3000}},live:true})).toBe(0);
  });
  it('does not trace notifications of a Session with no source in this group',()=>{
    const data=flowFixture();data.sessions[0].routes={};
    const scene=new MatrixScene(),state: MatrixState={sources:data.sources,sessions:data.sessions,frames,activity:{...recent,sources:{},sourceDeltas:{}},live:true,paused:false,group:'serial-acquisition'};
    for(let i=0;i<3;i++)scene.draw(recorder().ctx,state,moving);
    expect(scene.inFlight).toBe(0);
  });
  it('slides a crosspoint only when a Session really changes source',()=>{
    const data=twoSources(),scene=new MatrixScene(),state: MatrixState={sources:data.sources,sessions:data.sessions,frames,activity:idle,live:true,paused:false,group:'serial-acquisition'};
    scene.draw(recorder().ctx,state,moving);scene.draw(recorder().ctx,{...state},moving);
    expect(scene.switching).toBe(0);
    const moved=structuredClone(data.sessions);moved[0].routes['serial-acquisition']='rcd-58';
    scene.draw(recorder().ctx,{...state,sessions:moved},moving);
    expect(scene.switching).toBe(1);
    for(let i=0;i<20;i++)scene.draw(recorder().ctx,{...state,sessions:moved},moving);
    expect(scene.switching).toBe(0);
  });
  it('drops packets of a source whose link has just broken',()=>{
    const data=flowFixture(),scene=new MatrixScene(),state: MatrixState={sources:data.sources,sessions:data.sessions,frames,activity:recent,live:true,paused:false,group:'serial-acquisition'};
    scene.draw(recorder().ctx,state,moving);
    expect(scene.inFlight).toBeGreaterThan(0);
    const down=structuredClone(data);down.sources[0].connected=false;
    scene.draw(recorder().ctx,{...state,sources:down.sources},moving);
    expect(scene.inFlight).toBe(0);
  });
  it('keeps the newest trace column at the NOW line and the calculation in its own slot',()=>{
    expect(traceX(TRACE_CELLS-1,TRACE_CELLS)).toBeLessThan(MX.traceRight);
    expect(MX.traceRight-traceX(TRACE_CELLS-1,TRACE_CELLS)).toBeCloseTo(MX.traceRight-traceX(4,5));
    expect(traceX(0,TRACE_CELLS)).toBeGreaterThan(MX.traceLeft);
    const sessions=12,last=mxColX(sessions-1,sessions,true),calc=mxColX(sessions,sessions,true);
    expect(calc-last).toBeGreaterThan(60);
    expect(calc).toBeLessThan(MX.gridRight);
  });
});
