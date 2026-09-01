import { createElement, type ReactNode } from 'react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, renderHook } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { queryKeys } from '../api/queries';
import { TestEventSource } from '../test/eventSource';
import { eventInvalidatesManagementAudit, eventInvalidatesOperations, eventInvalidatesPoints, eventInvalidatesSourceEvents, useEventStream } from './useEventStream';

afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  TestEventSource.instances = [];
});

describe('point query invalidation', () => {
  it('refreshes points when route, catalog, sample, or connection quality changes', () => {
    for (const type of ['source.sample', 'source.status', 'catalog.changed', 'selection.changed', 'group.changed', 'session.changed']) {
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

  it('only invalidates maintenance reads for their matching event kind', () => {
    const status = ['source-operations', 'lower-a', 1, 'status'];
    const config = ['source-operations', 'lower-a', 1, 'config', 'calc.toml'];
    expect(eventInvalidatesOperations('source.operation', config)).toBe(true);
    expect(eventInvalidatesOperations('source.status', status)).toBe(true);
    expect(eventInvalidatesOperations('command.changed', status)).toBe(true);
    expect(eventInvalidatesOperations('command.changed', config)).toBe(false);
    expect(eventInvalidatesOperations('source.sample', status)).toBe(false);
    expect(eventInvalidatesOperations('station_versions_changed', status)).toBe(true);
    expect(eventInvalidatesOperations('station_versions_changed', config)).toBe(false);
    expect(eventInvalidatesOperations('source.operation', queryKeys.allPoints)).toBe(false);
  });
});

describe('SSE query refreshes', () => {
  function setup() {
    vi.stubGlobal('EventSource', TestEventSource);
    const client = new QueryClient();
    const invalidate = vi.spyOn(client, 'invalidateQueries').mockResolvedValue(undefined);
    const wrapper = ({ children }: { children: ReactNode }) => createElement(QueryClientProvider, { client, children });
    const hook = renderHook(() => useEventStream(), { wrapper });
    expect(TestEventSource.instances).toHaveLength(1);
    return { invalidate, source: TestEventSource.instances[0], hook };
  }

  it('refreshes active Session point views even if no further lower samples arrive', () => {
    const { invalidate, source } = setup();
    act(() => source.emit('session.changed', { closed_id: 'session-2' }));
    expect(invalidate).toHaveBeenCalledWith({ queryKey: queryKeys.sessions });
    expect(invalidate).toHaveBeenCalledWith({ queryKey: queryKeys.allPoints, refetchType: 'active' });
  });

  it('receives named operations events and scopes refreshes to the physical source', () => {
    const { invalidate, source } = setup();
    act(() => source.emit('source.operation', { source_id: 'lower-a', action: 'write_config' }));
    expect(invalidate).toHaveBeenCalledWith({ queryKey: ['source-operations', 'lower-a'], predicate: expect.any(Function), refetchType: 'active' });
    expect(invalidate).not.toHaveBeenCalledWith({ queryKey: ['source-operations', 'lower-b'], predicate: expect.any(Function), refetchType: 'active' });
    expect(invalidate).not.toHaveBeenCalledWith({ queryKey: queryKeys.allPoints, refetchType: 'active' });
  });

  it('refreshes only active station inventory and the matching lower status on a version event', () => {
    const { invalidate, source } = setup();
    act(() => source.emit('station_versions_changed', { source_id: 'lower-a', station_revision: '12' }));
    expect(invalidate).toHaveBeenCalledWith({ queryKey: queryKeys.allDeviceVersions, refetchType: 'active' });
    expect(invalidate).toHaveBeenCalledWith({ queryKey: ['source-operations', 'lower-a'], predicate: expect.any(Function), refetchType: 'active' });
    expect(invalidate).not.toHaveBeenCalledWith({ queryKey: queryKeys.allPoints, refetchType: 'active' });
  });

  it('refreshes station inventory when a lower goes online or offline', () => {
    const { invalidate, source } = setup();
    act(() => source.emit('source.status', { source_id: 'lower-a', connection_state: 'Disconnected' }));
    expect(invalidate).toHaveBeenCalledWith({ queryKey: queryKeys.allDeviceVersions, refetchType: 'active' });
  });

  it('refreshes only the matching central lower log query', () => {
    const { invalidate, source } = setup();
    act(() => source.emit('lower_logs_changed', { source_id: 'lower-a', accepted_through: 10 }));
    expect(invalidate).toHaveBeenCalledWith({ queryKey: ['lower-logs', 'lower-a'], refetchType: 'active' });
    expect(invalidate).not.toHaveBeenCalledWith({ queryKey: queryKeys.allPoints, refetchType: 'active' });
  });

  it('refreshes group command locks on both command start and completion events', () => {
    const { invalidate, source } = setup();
    for (const state of ['Pending', 'Completed']) {
      invalidate.mockClear();
      act(() => source.emit('command.changed', { group_id: 'device-a', state }));
      expect(invalidate).toHaveBeenCalledWith({ queryKey: queryKeys.groups });
      expect(invalidate).toHaveBeenCalledWith({ queryKey: queryKeys.allCommands, refetchType: 'active' });
      expect(invalidate).not.toHaveBeenCalledWith({ queryKey: queryKeys.allPoints, refetchType: 'active' });
    }
  });

  it('does not fan high-frequency samples into group/history queries and removes its listener on unmount', () => {
    const { invalidate, source, hook } = setup();
    act(() => source.emit('source.sample', { source_id: 'lower-a' }));
    expect(invalidate).not.toHaveBeenCalledWith({ queryKey: queryKeys.groups });
    expect(invalidate).not.toHaveBeenCalledWith({ queryKey: queryKeys.allCommands, refetchType: 'active' });
    expect(invalidate).not.toHaveBeenCalledWith({ queryKey: queryKeys.allManagementAudit, refetchType: 'active' });
    hook.unmount();
    expect(source.closed).toBe(true);
    invalidate.mockClear();
    act(() => source.emit('session.changed'));
    expect(invalidate).not.toHaveBeenCalled();
  });
});
