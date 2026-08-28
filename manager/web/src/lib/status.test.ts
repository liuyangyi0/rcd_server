import { describe, expect, it } from 'vitest';
import type { Source } from '../api/types';
import { isActiveState, isBadStatus, sourceHealth } from './status';

const source = (overrides: Partial<Source> = {}): Source => ({
  id: 'source-a', name: 'source-a', groupId: 'group-a', endpoint: 'opc.tcp://127.0.0.1:4840',
  securityPolicy: 'None', securityMode: 'None', hasPassword: false, enabled: true,
  connectionState: 'Connected', sessionState: 'Active', subscriptionState: 'Active', catalogState: 'Ready',
  reconnectCount: 0, monitoredCount: 31, goodPoints: 1, badPoints: 30, totalPoints: 31, revision: 1,
  ...overrides,
});

describe('source status', () => {
  it('accepts the backend Active state as a healthy subscription', () => {
    expect(isActiveState('Active')).toBe(true);
    expect(isActiveState('Running')).toBe(true);
    expect(isActiveState('Inactive')).toBe(false);
    expect(sourceHealth(source())).toBe('good');
  });

  it('does not report a connected source as healthy when its Session or subscription is inactive', () => {
    expect(sourceHealth(source({ sessionState: 'Inactive' }))).toBe('bad');
    expect(sourceHealth(source({ subscriptionState: 'Inactive' }))).toBe('bad');
    expect(sourceHealth(source({ subscriptionState: 'Creating' }))).toBe('warning');
  });

  it('classifies named and previously unknown hexadecimal Bad status codes', () => {
    expect(isBadStatus('BadNoCommunication')).toBe(true);
    expect(isBadStatus('0x80010000')).toBe(true);
    expect(isBadStatus('0xC0010000')).toBe(true);
    expect(isBadStatus('0x40000000')).toBe(true);
    expect(isBadStatus('0x40920000')).toBe(true);
    expect(isBadStatus('0x00000000')).toBe(false);
  });
});
