import { describe, expect, it } from 'vitest';
import type { Source } from '../api/types';
import { deviceKey, sameBindings, sourceGroupIds, sourceHasGroup, sourceSelectableForGroup, sourceUnavailableReason } from './sourceGroups';

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

describe('group-scoped source availability', () => {
  const source: Source = { id: 'lower-a', name: 'Lower A', groupId: 'shared', groupBindings: [{ groupId: 'device-a', portId: 'ttyS0', deviceId: '1' }], availableGroupIds: ['shared'], endpoint: '', securityPolicy: 'None', securityMode: 'None', hasPassword: false, enabled: true, connectionState: 'Connected', sessionState: 'Active', subscriptionState: 'Active', catalogState: 'Ready', reconnectCount: 0, monitoredCount: 1, goodPoints: 1, badPoints: 0, totalPoints: 1, revision: 1 };

  it('distinguishes a configured membership from a discovered selectable directory', () => {
    expect(sourceHasGroup(source, 'device-a')).toBe(true);
    expect(sourceSelectableForGroup(source, 'device-a')).toBe(false);
    expect(sourceUnavailableReason(source, 'device-a')).toBe('未发现本组目录');
    expect(sourceSelectableForGroup(source, 'shared')).toBe(true);
    expect(sourceSelectableForGroup(source, 'other')).toBe(false);
  });

  it('allows discovered offline cached members but rejects disabled and conflicting sources', () => {
    expect(sourceSelectableForGroup({ ...source, connectionState: 'Disconnected', catalogState: 'Cached' }, 'shared')).toBe(true);
    expect(sourceSelectableForGroup({ ...source, catalogState: 'CatalogConflict' }, 'shared')).toBe(false);
    expect(sourceUnavailableReason({ ...source, enabled: false }, 'shared')).toBe('来源已停用');
  });
});
