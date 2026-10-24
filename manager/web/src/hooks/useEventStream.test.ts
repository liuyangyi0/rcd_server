import { createElement, type ReactNode } from 'react';
import { QueryClient, QueryClientProvider, useQuery } from '@tanstack/react-query';
import { act, renderHook, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { queryKeys } from '../api/queries';
import { processApi, processKeys, useProcessBinding, useProcessStatus, type ProcessBinding } from '../api/process';
import { TestEventSource } from '../test/eventSource';
import { eventInvalidatesManagementAudit, eventInvalidatesOperations, eventInvalidatesPoints, eventInvalidatesSourceEvents, useEventStream } from './useEventStream';

afterEach(() => {
  vi.useRealTimers();
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

  it('refreshes process evidence without invalidating configuration drafts or log reads', () => {
    const target = { id: 'lower-a', revision: 1, bindingRevision: 2, instanceId: 'agent-a' };
    for (const key of [processKeys.binding(target), processKeys.status(target), processKeys.history(target), processKeys.operation(target, 'job-a')]) {
      expect(eventInvalidatesOperations('source.process', key)).toBe(true);
    }
    for (const key of [processKeys.log(target, 1024), ['source-operations', 'lower-a', 1, 'config', 'calc.toml'], queryKeys.allPoints]) {
      expect(eventInvalidatesOperations('source.process', key)).toBe(false);
    }
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

  it('receives named process events and removes their listener on unmount', () => {
    const { invalidate, source, hook } = setup();
    act(() => source.emit('source.process', { source_id: 'lower-a', action: 'binding' }));
    expect(invalidate).toHaveBeenCalledWith({ queryKey: ['source-operations', 'lower-a'], predicate: expect.any(Function), refetchType: 'active' }, { cancelRefetch: false });
    expect(invalidate).toHaveBeenCalledWith({ queryKey: ['fleet'], refetchType: 'active' }, { cancelRefetch: false });
    expect(invalidate).not.toHaveBeenCalledWith({ queryKey: queryKeys.allPoints, refetchType: 'active' });
    hook.unmount(); invalidate.mockClear();
    act(() => source.emit('source.process', { source_id: 'lower-a' }));
    expect(invalidate).not.toHaveBeenCalled();
  });

  it('updates an already open process view after another tab changes its binding without OPC events', async () => {
    vi.stubGlobal('EventSource', TestEventSource);
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const target = { id: 'lower-a', revision: 1 };
    let binding: ProcessBinding = { source_id: target.id, source_revision: 1, revision: 2, agent_url: 'http://agent-a:8090', instance_id: 'agent-a' };
    const readBinding = vi.spyOn(processApi, 'binding').mockImplementation(async () => ({ ...binding }));
    vi.spyOn(processApi, 'status').mockImplementation(async (scope) => ({ source_id: scope.id, source_revision: scope.revision, binding_revision: scope.bindingRevision, instance_id: scope.instanceId, revision: 'snapshot-1', state: 'stopped', desired_state: 'stopped' }));
    const wrapper = ({ children }: { children: ReactNode }) => createElement(QueryClientProvider, { client }, children);
    const hook = renderHook(() => {
      useEventStream();
      const current = useProcessBinding(target);
      return { binding: current, status: useProcessStatus(target, current.data) };
    }, { wrapper });
    await waitFor(() => expect(hook.result.current.status.data?.instance_id).toBe('agent-a'));
    binding = { ...binding, revision: 3, agent_url: 'http://agent-b:8090', instance_id: 'agent-b' };
    act(() => TestEventSource.instances[0].emit('source.process', { source_id: 'another-source' }));
    expect(readBinding).toHaveBeenCalledTimes(1);
    act(() => TestEventSource.instances[0].emit('source.process', { source_id: target.id, action: 'binding' }));
    await waitFor(() => expect(hook.result.current.status.data?.instance_id).toBe('agent-b'));
    expect(hook.result.current.binding.data?.revision).toBe(3);
    expect(readBinding).toHaveBeenCalledTimes(2);
    hook.unmount(); client.clear();
  });

  it('refreshes station inventory when a lower goes online or offline', () => {
    const { invalidate, source } = setup();
    act(() => source.emit('source.status', { source_id: 'lower-a', connection_state: 'Disconnected' }));
    expect(invalidate).toHaveBeenCalledWith({ queryKey: queryKeys.allDeviceVersions, refetchType: 'active' });
  });

  it('refreshes only the matching central lower log query', () => {
    const { invalidate, source } = setup();
    act(() => source.emit('lower_logs_changed', { source_id: 'lower-a', accepted_through: 10 }));
    expect(invalidate).toHaveBeenCalledWith({ queryKey: ['lower-logs', 'lower-a'], refetchType: 'active' }, { cancelRefetch: false });
    expect(invalidate).not.toHaveBeenCalledWith({ queryKey: queryKeys.allPoints, refetchType: 'active' });
  });

  it('coalesces a source log burst without delaying another source and clears pending refreshes on unmount', async () => {
    vi.useFakeTimers();
    const { invalidate, source, hook } = setup();
    let resolveRead!: () => void;
    const reading = new Promise<void>((resolve) => { resolveRead = resolve; });
    invalidate.mockReturnValueOnce(reading);
    act(() => {
      source.emit('lower_logs_changed', { source_id: 'lower-a' });
      for (let i = 0; i < 20; i++) source.emit('lower_logs_changed', { source_id: 'lower-a' });
      source.emit('lower_logs_changed', { source_id: 'lower-b' });
    });
    expect(invalidate).toHaveBeenCalledTimes(2);
    expect(invalidate).toHaveBeenLastCalledWith({ queryKey: ['lower-logs', 'lower-b'], refetchType: 'active' }, { cancelRefetch: false });
    await act(async () => { resolveRead(); });
    await act(async () => { await vi.advanceTimersByTimeAsync(999); });
    expect(invalidate).toHaveBeenCalledTimes(2);
    await act(async () => { await vi.advanceTimersByTimeAsync(1); });
    expect(invalidate).toHaveBeenCalledTimes(3);
    expect(invalidate).toHaveBeenLastCalledWith({ queryKey: ['lower-logs', 'lower-a'], refetchType: 'active' }, { cancelRefetch: false });
    act(() => source.emit('lower_logs_changed', { source_id: 'lower-a' }));
    hook.unmount();
    await act(async () => { await vi.advanceTimersByTimeAsync(2000); });
    expect(invalidate).toHaveBeenCalledTimes(3);
  });

  it('does not abort a slow cached log read and performs a final read after its event burst', async () => {
    vi.stubGlobal('EventSource', TestEventSource);
    const client = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
    let resolveRead!: (value: string) => void;
    const reading = new Promise<string>((resolve) => { resolveRead = resolve; });
    const read = vi.fn().mockResolvedValueOnce('old snapshot').mockReturnValueOnce(reading).mockResolvedValue('latest snapshot');
    const signals: AbortSignal[] = [];
    const wrapper = ({ children }: { children: ReactNode }) => createElement(QueryClientProvider, { client }, children);
    const hook = renderHook(() => {
      useEventStream();
      return useQuery({ queryKey: ['lower-logs', 'lower-a', { page: 1 }], queryFn: ({ signal }) => { signals.push(signal); return read() as Promise<string>; } });
    }, { wrapper });
    await waitFor(() => expect(hook.result.current.data).toBe('old snapshot'));
    act(() => TestEventSource.instances[0].emit('lower_logs_changed', { source_id: 'lower-a' }));
    await waitFor(() => expect(read).toHaveBeenCalledTimes(2));
    act(() => { for (let i = 0; i < 20; i++) TestEventSource.instances[0].emit('lower_logs_changed', { source_id: 'lower-a' }); });
    expect(read).toHaveBeenCalledTimes(2);
    expect(signals[1].aborted).toBe(false);
    await act(async () => { resolveRead('intermediate snapshot'); });
    await waitFor(() => expect(hook.result.current.data).toBe('latest snapshot'), { timeout: 2500 });
    expect(read).toHaveBeenCalledTimes(3);
    hook.unmount();
    client.clear();
  });

  it('refreshes group command locks on both command start and completion events', () => {
    const { invalidate, source } = setup();
    for (const state of ['Pending', 'Completed']) {
      invalidate.mockClear();
      act(() => source.emit('command.changed', { group_id: 'device-a', state }));
      expect(invalidate).toHaveBeenCalledWith({ queryKey: queryKeys.groups });
      expect(invalidate).toHaveBeenCalledWith({ queryKey: queryKeys.allCommands, refetchType: 'active' }, { cancelRefetch: false });
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
