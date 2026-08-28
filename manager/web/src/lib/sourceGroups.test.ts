import { describe, expect, it } from 'vitest';
import { deviceKey, sameBindings, sourceGroupIds, sourceHasGroup } from './sourceGroups';

describe('multi-group source membership', () => {
  const source = { groupId: 'unassigned', groupBindings: [
    { portId: '/dev/ttyS0', deviceId: '1', groupId: 'device-a' },
    { portId: '/dev/ttyS0', deviceId: '2', groupId: 'device-b' },
    { portId: '/dev/ttyS1', deviceId: '1', groupId: 'device-a' },
  ] };

  it('includes default and all device groups without duplicate sources', () => {
    expect(sourceGroupIds(source)).toEqual(['unassigned', 'device-a', 'device-b']);
    expect(sourceHasGroup(source, 'device-a')).toBe(true);
    expect(sourceHasGroup(source, 'unassigned')).toBe(true);
    expect(sourceHasGroup(source, 'other')).toBe(false);
  });

  it('retains legacy single-group sources', () => {
    expect(sourceGroupIds({ groupId: 'legacy' })).toEqual(['legacy']);
    expect(sourceHasGroup({ groupId: 'legacy' }, 'legacy')).toBe(true);
  });

  it('uses the physical port with the device address and ignores binding order', () => {
    expect(deviceKey('/dev/ttyS0', '1')).not.toBe(deviceKey('/dev/ttyS1', '1'));
    expect(sameBindings(source.groupBindings, [...source.groupBindings].reverse())).toBe(true);
    expect(sameBindings(source.groupBindings, source.groupBindings.slice(1))).toBe(false);
  });
});
