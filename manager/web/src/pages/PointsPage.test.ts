import { describe, expect, it } from 'vitest';
import { comparisonReady } from './PointsPage';

describe('point comparison loading state', () => {
  it('waits for both source pages before comparing', () => {
    expect(comparisonReady({ items: [] }, undefined)).toBe(false);
    expect(comparisonReady(undefined, { items: [] })).toBe(false);
    expect(comparisonReady({ items: [] }, { items: [] })).toBe(true);
  });
});
