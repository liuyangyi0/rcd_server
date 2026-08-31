import { act, renderHook } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import type { UaSession } from '../api/types';
import { useSessionSelection } from './useSessionSelection';

const first: UaSession = { id: 'first', clientName: 'First', remoteAddress: '', selectedSources: {}, routeRevisions: {}, selectionRevision: 1 };
const second: UaSession = { ...first, id: 'second', clientName: 'Second' };

describe('sticky Session context', () => {
  it('captures the first loaded Session once and never follows reordering or replacement', () => {
    const view = renderHook(({ sessions }) => useSessionSelection(sessions), { initialProps: { sessions: [] as UaSession[] } });
    expect(view.result.current.sessionId).toBe('');
    view.rerender({ sessions: [first, second] });
    expect(view.result.current.sessionId).toBe('first');
    view.rerender({ sessions: [second, first] });
    expect(view.result.current.sessionId).toBe('first');
    view.rerender({ sessions: [second] });
    expect(view.result.current.sessionId).toBe('first');
    expect(view.result.current.session).toBeUndefined();
    view.rerender({ sessions: [] });
    view.rerender({ sessions: [second] });
    expect(view.result.current.sessionId).toBe('first');
    act(() => view.result.current.setSessionId('second'));
    expect(view.result.current.session).toEqual(second);
  });

  it('does not undo an explicitly cleared selection on a background refresh', () => {
    const view = renderHook(({ sessions }) => useSessionSelection(sessions), { initialProps: { sessions: [first] } });
    act(() => view.result.current.setSessionId(''));
    view.rerender({ sessions: [first, second] });
    expect(view.result.current.sessionId).toBe('');
    expect(view.result.current.session).toBeUndefined();
  });
});
