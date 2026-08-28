import { describe, expect, it } from 'vitest';
import { eventInvalidatesManagementAudit, eventInvalidatesPoints, eventInvalidatesSourceEvents } from './useEventStream';

describe('point query invalidation', () => {
  it('refreshes points when route, catalog, sample, or connection quality changes', () => {
    for (const type of ['source.sample', 'source.status', 'catalog.changed', 'selection.changed', 'group.changed']) {
      expect(eventInvalidatesPoints(type), type).toBe(true);
    }
  });

  it('does not refresh the large point query for unrelated audit-only events', () => {
    expect(eventInvalidatesPoints('source.event')).toBe(false);
    expect(eventInvalidatesPoints('command.changed')).toBe(false);
  });

  it('does not turn a high-frequency sample into SQLite history queries', () => {
    expect(eventInvalidatesSourceEvents('source.sample')).toBe(false);
    expect(eventInvalidatesManagementAudit('source.sample')).toBe(false);
    expect(eventInvalidatesSourceEvents('source.event')).toBe(true);
    expect(eventInvalidatesManagementAudit('management.audit')).toBe(true);
  });
});
