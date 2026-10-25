import { createContext, useContext, useEffect, useRef, useState } from 'react';
import { STATION_COLORS, type FlowEvent } from './model';

export const clock = (time?: string) => time ? new Date(time).toLocaleTimeString('zh-CN',{hour12:false}) : '—';
export const shortPort = (port: string) => port.replace(/^\/dev\//,'');
export const stationAlert = (state: number) => state===2 || state===3 || state===5;
export const EVENT_KIND: Record<FlowEvent['kind'],string> = { source:'来源', station:'站点', route:'选源', session:'会话' };
export const STATION_GROUPS=[{label:'正常',color:STATION_COLORS[1],states:[1]},{label:'异常',color:STATION_COLORS[2],states:[2,3]},{label:'离线',color:STATION_COLORS[5],states:[5]},{label:'未知 / 停用',color:STATION_COLORS[0],states:[0,4]}];
// Activity lasts about 3 s, so an active card keeps showing its last non-zero count between snapshots.
export const latestValue=(delta:number,history?:number[])=>delta || [...(history ?? [])].reverse().find((v)=>v>0);

// True for a moment after signature changes, never on first render: fault cues flash only for
// real changes between snapshots, not when a card mounts on a page, group or view switch.
export function usePulse(signature: string, ms=1800) {
  const [on,setOn]=useState(false), last=useRef(signature);
  useEffect(()=>{
    if(last.current===signature)return;
    last.current=signature;setOn(true);
    const timer=window.setTimeout(()=>setOn(false),ms);
    return ()=>window.clearTimeout(timer);
  },[signature,ms]);
  return on;
}

// Numbers roll to their new value and glow briefly; they stay still when motion is off.
export const MotionContext=createContext(false);
export function Ticker({value,prefix='',bare}:{value:number;prefix?:string;bare?:boolean}) {
  const motion=useContext(MotionContext), [shown,setShown]=useState(motion?0:value), [changed,setChanged]=useState(false), from=useRef(shown), first=useRef(true);
  useEffect(()=>{
    if(!motion){from.current=value;setShown(value);return;}
    const origin=from.current,start=performance.now();let frame=0;
    const step=(now:number)=>{const p=Math.min(1,(now-start)/650),next=Math.round(origin+(value-origin)*(1-(1-p)**3));from.current=next;setShown(next);if(p<1)frame=requestAnimationFrame(step);};
    frame=requestAnimationFrame(step);
    if(!first.current && origin!==value)setChanged(true);
    first.current=false;
    const timer=window.setTimeout(()=>setChanged(false),900);
    return ()=>{cancelAnimationFrame(frame);window.clearTimeout(timer);};
  },[value,motion]);
  const text=`${prefix}${shown.toLocaleString()}`;
  return bare?<>{text}</>:<span className={`flow-tick${changed?' is-changed':''}`}>{text}</span>;
}

export function Spark({values}:{values?:number[]}) {
  const list=values?.length?values:[0], max=Math.max(1,...list);
  return <svg className="flow-spark" viewBox="0 0 56 16" aria-hidden="true"><polyline points={list.map((v,i)=>`${(56-(list.length-1-i)*56/29).toFixed(1)},${(14-v/max*12).toFixed(1)}`).join(' ')}/></svg>;
}
