import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, renderHook, waitFor } from '@testing-library/react';
import type { ReactNode } from 'react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { api } from './client';
import { queryKeys, usePointComparison, usePoints } from './queries';
import type { ListParams, PageResult, Point } from './types';

afterEach(() => vi.restoreAllMocks());

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (reason: unknown) => void;
  const promise = new Promise<T>((done, fail) => { resolve = done; reject = fail; });
  return { promise, resolve, reject };
}

function page(nodeId: string, value: number, pageNumber = 1): PageResult<Point> {
  return { items: [{ nodeId, browseName: nodeId, nodeClass: 'Variable', groupId: 'device-a', dataType: 'Double', value, statusCode: 'Good', sourcePresent: true, deleted: false }], page: pageNumber, pageSize: 50, total: 100 };
}

function createWrapper() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
  return { client, wrapper: ({ children }: { children: ReactNode }) => <QueryClientProvider client={client}>{children}</QueryClientProvider> };
}

const leftParams: ListParams = { groupId: 'device-a', page: 1, pageSize: 50, routeSourceId: 'lower-a' };
const rightParams: ListParams = { ...leftParams, routeSourceId: 'lower-b' };
const comparisonChanges: Array<[string, ListParams]> = [
  ['search', { query: 'A_VALUE' }],
  ['page', { page: 2 }],
  ['catalog device', { sourceId: 'lower-a', portId: '/dev/ttyS1', deviceId: '2' }],
  ['source', { routeSourceId: 'lower-b' }],
];

describe('atomic point comparison queries', () => {
  it('starts both reads concurrently with one cancellation signal and publishes only complete pairs', async () => {
    const left = deferred<PageResult<Point>>();
    const right = deferred<PageResult<Point>>();
    const points = vi.spyOn(api, 'points').mockReturnValueOnce(left.promise).mockReturnValueOnce(right.promise);
    const { wrapper } = createWrapper();
    const { result } = renderHook(() => usePointComparison(leftParams, rightParams, true), { wrapper });
    await waitFor(() => expect(points).toHaveBeenCalledTimes(2));
    expect(points.mock.calls[0][0]).toEqual(leftParams);
    expect(points.mock.calls[1][0]).toEqual(rightParams);
    expect(points.mock.calls[0][1]).toBeInstanceOf(AbortSignal);
    expect(points.mock.calls[0][1]).toBe(points.mock.calls[1][1]);
    await act(async () => { left.resolve(page('A_VALUE', 11)); await left.promise; });
    expect(result.current.data).toBeUndefined();
    await act(async () => { right.resolve(page('A_VALUE', 101)); });
    await waitFor(() => expect(result.current.data?.right.items[0].value).toBe(101));
    expect(result.current.data?.left.items[0].value).toBe(11);
    expect(queryKeys.pointComparison(leftParams, rightParams).slice(0, 1)).toEqual(queryKeys.allPoints);
  });

  it.each(comparisonChanges)('never reuses the old pair after changing %s', async (_name, changes) => {
    const nextLeft = deferred<PageResult<Point>>();
    const nextRight = deferred<PageResult<Point>>();
    const points = vi.spyOn(api, 'points')
      .mockResolvedValueOnce(page('OLD', 1)).mockResolvedValueOnce(page('OLD', 2))
      .mockReturnValueOnce(nextLeft.promise).mockReturnValueOnce(nextRight.promise);
    const { wrapper } = createWrapper();
    const view = renderHook(({ left, right }) => usePointComparison(left, right, true), { wrapper, initialProps: { left: leftParams, right: rightParams } });
    await waitFor(() => expect(view.result.current.data?.left.items[0].nodeId).toBe('OLD'));
    const commonChanges = { ...changes, routeSourceId: rightParams.routeSourceId };
    view.rerender({ left: { ...leftParams, ...changes }, right: { ...rightParams, ...commonChanges } });
    await waitFor(() => expect(points).toHaveBeenCalledTimes(4));
    expect(view.result.current.data).toBeUndefined();
    await act(async () => { nextLeft.resolve(page('NEW', 11, changes.page ?? 1)); await nextLeft.promise; });
    expect(view.result.current.data).toBeUndefined();
    await act(async () => { nextRight.resolve(page('NEW', 101, changes.page ?? 1)); });
    await waitFor(() => expect(view.result.current.data?.right.items[0].nodeId).toBe('NEW'));
    expect(view.result.current.data?.left.page).toBe(changes.page ?? 1);
    expect(view.result.current.data?.right.page).toBe(changes.page ?? 1);
  });

  it('keeps the complete old pair during a same-key refresh instead of publishing the first completed side', async () => {
    const nextLeft = deferred<PageResult<Point>>();
    const nextRight = deferred<PageResult<Point>>();
    const points = vi.spyOn(api, 'points')
      .mockResolvedValueOnce(page('A_VALUE', 1)).mockResolvedValueOnce(page('A_VALUE', 2))
      .mockReturnValueOnce(nextLeft.promise).mockReturnValueOnce(nextRight.promise);
    const { client, wrapper } = createWrapper();
    const { result } = renderHook(() => usePointComparison(leftParams, rightParams, true), { wrapper });
    await waitFor(() => expect(result.current.data?.left.items[0].value).toBe(1));
    act(() => { void client.invalidateQueries({ queryKey: queryKeys.allPoints, refetchType: 'active' }); });
    await waitFor(() => expect(points).toHaveBeenCalledTimes(4));
    await act(async () => { nextLeft.resolve(page('A_VALUE', 11)); await nextLeft.promise; });
    expect(result.current.data?.left.items[0].value).toBe(1);
    expect(result.current.data?.right.items[0].value).toBe(2);
    await act(async () => { nextRight.resolve(page('A_VALUE', 101)); });
    await waitFor(() => expect(result.current.data?.left.items[0].value).toBe(11));
    expect(result.current.data?.right.items[0].value).toBe(101);
  });

  it('aborts both obsolete reads when the user changes filters and ignores their late results', async () => {
    const previousLeft = deferred<PageResult<Point>>();
    const previousRight = deferred<PageResult<Point>>();
    const nextLeft = deferred<PageResult<Point>>();
    const nextRight = deferred<PageResult<Point>>();
    const points = vi.spyOn(api, 'points')
      .mockReturnValueOnce(previousLeft.promise).mockReturnValueOnce(previousRight.promise)
      .mockReturnValueOnce(nextLeft.promise).mockReturnValueOnce(nextRight.promise);
    const { wrapper } = createWrapper();
    const view = renderHook(({ query }) => usePointComparison({ ...leftParams, query }, { ...rightParams, query }, true), { wrapper, initialProps: { query: '' } });
    await waitFor(() => expect(points).toHaveBeenCalledTimes(2));
    const oldSignal = points.mock.calls[0][1]!;
    view.rerender({ query: 'A_VALUE' });
    await waitFor(() => expect(points).toHaveBeenCalledTimes(4));
    expect(oldSignal.aborted).toBe(true);
    expect(points.mock.calls[1][1]?.aborted).toBe(true);
    expect(points.mock.calls[2][1]?.aborted).toBe(false);
    await act(async () => { previousLeft.resolve(page('OLD', 1)); previousRight.resolve(page('OLD', 2)); });
    expect(view.result.current.data).toBeUndefined();
    await act(async () => { nextLeft.resolve(page('A_VALUE', 11)); nextRight.resolve(page('A_VALUE', 101)); });
    await waitFor(() => expect(view.result.current.data?.left.items[0].nodeId).toBe('A_VALUE'));
  });

  it('reports a failed side without exposing the successful side as a comparison', async () => {
    const right = deferred<PageResult<Point>>();
    const points = vi.spyOn(api, 'points').mockResolvedValueOnce(page('A_VALUE', 11)).mockReturnValueOnce(right.promise);
    const { wrapper } = createWrapper();
    const { result } = renderHook(() => usePointComparison(leftParams, rightParams, true), { wrapper });
    await waitFor(() => expect(points).toHaveBeenCalledTimes(2));
    await act(async () => { right.reject(new Error('Right source API unavailable')); });
    await waitFor(() => expect(result.current.isError).toBe(true));
    expect(result.current.data).toBeUndefined();
  });

  it('does not display one Session or source page under another single-view route while loading', async () => {
    const next = deferred<PageResult<Point>>();
    vi.spyOn(api, 'points').mockResolvedValueOnce(page('A_VALUE', 11)).mockReturnValueOnce(next.promise);
    const { wrapper } = createWrapper();
    const view = renderHook(({ sessionId }) => usePoints({ ...leftParams, routeSourceId: undefined, sessionId }), { wrapper, initialProps: { sessionId: 'first' } });
    await waitFor(() => expect(view.result.current.data?.items[0].value).toBe(11));
    view.rerender({ sessionId: 'second' });
    expect(view.result.current.data).toBeUndefined();
    await act(async () => { next.resolve(page('A_VALUE', 101)); });
    await waitFor(() => expect(view.result.current.data?.items[0].value).toBe(101));
  });
});
