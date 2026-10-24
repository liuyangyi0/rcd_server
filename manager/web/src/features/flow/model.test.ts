import { describe, expect, it } from 'vitest';
import { advanceActivity, notificationDelta, sourceDelta, visibleEvents } from './model';
import { flowFixture } from './fixture';
describe('real flow activity',()=>{
  it('does not invent activity on first load, no changes, or instance restart',()=>{
    const data=flowFixture(),empty={sources:{},sessions:{},updates:0,sourceDeltas:{},sessionDeltas:{}};
    expect(advanceActivity(undefined,data,empty,100)).toEqual(empty);
    expect(advanceActivity(data,structuredClone(data),empty,200)).toEqual({...empty,sources:{'rcd-46':0},sessions:{'public-alias':0}});
    const changed=structuredClone(data);changed.instance='boot2';changed.sources[0].samples='900';
    expect(advanceActivity(data,changed,empty,300)).toEqual(empty);
  });
  it('tracks accepted samples and per-subscription notifications independently',()=>{
    const old=flowFixture(),next=structuredClone(old);next.sources[0].samples='105';next.sessions[0].notifications['7']=103;
    const got=advanceActivity(old,next,{sources:{},sessions:{},updates:0,sourceDeltas:{},sessionDeltas:{}},400);
    expect(got).toEqual({sources:{'rcd-46':400},sessions:{'public-alias':400},updates:5,sourceDeltas:{'rcd-46':5},sessionDeltas:{'public-alias':3}});
    next.sources[0].connected=false;next.sessions[0].attached=false;
    expect(advanceActivity(old,next,got,500)).toEqual({sources:{},sessions:{},updates:5,sourceDeltas:{},sessionDeltas:{}});
  });
  it('preserves uint64 sample precision, handles uint32 wrap and new subscriptions',()=>{
    const old=flowFixture(),next=structuredClone(old);old.sources[0].samples='9007199254740992';next.sources[0].samples='9007199254740993';
    expect(sourceDelta(old.sources[0],next.sources[0])).toBe(1);
    old.sessions[0].notifications={'7':0xffffffff};next.sessions[0].notifications={'7':1,'8':500};
    expect(notificationDelta(old.sessions[0],next.sessions[0])).toBe(2);
  });
  it('reports actual station and routing changes without replaying fabricated events',()=>{
    const old=flowFixture(),next=structuredClone(old);next.sources[0].ports[0].devices[0].state=2;next.sessions[0].routes['serial-acquisition']='rcd-58';
    expect(visibleEvents(undefined,next)).toEqual([]);
    const events=visibleEvents(old,next);expect(events).toHaveLength(2);expect(events[0]).toContain('通信异常');expect(events[1]).toContain('rcd-58');
  });
});
