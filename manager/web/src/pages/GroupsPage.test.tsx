import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { api } from '../api/client';
import { queryKeys } from '../api/queries';
import type { Group, Source, UaSession } from '../api/types';
import GroupsPage from './GroupsPage';

const group: Group = { id: 'fire', name: 'Fire', defaultSourceId: 'rcd-1', selectionRevision: 4, sourceCount: 2, goodPoints: 1, badPoints: 0, totalPoints: 1, pendingCommandCount: 0, status: 'good' };
const sources: Source[] = ['rcd-1', 'rcd-2'].map((id) => ({
  id, name: id, groupId: 'fire', endpoint: `opc.tcp://${id}:4840`, securityPolicy: 'None', securityMode: 'None',
  hasPassword: false, enabled: true, connectionState: 'Connected', sessionState: 'Active', subscriptionState: 'Active',
  catalogState: 'Ready', availableGroupIds: ['fire'], reconnectCount: 0, monitoredCount: 1, goodPoints: 1, badPoints: 0, totalPoints: 1, revision: 1,
}));
const sessions: UaSession[] = [
  { id: 'session-1', clientName: 'SCADA A', remoteAddress: '10.0.0.1', selectionRevision: 9, selectedSources: { fire: 'rcd-1' }, routeRevisions: { fire: 9 } },
  { id: 'session-2', clientName: 'SCADA B', remoteAddress: '10.0.0.2', selectionRevision: 12, selectedSources: { fire: 'rcd-2' }, routeRevisions: { fire: 12 } },
];

afterEach(() => vi.restoreAllMocks());

function setup(nextSources = sources) {
  const client = new QueryClient({ defaultOptions: { queries: { staleTime: Infinity, retry: false }, mutations: { retry: false } } });
  client.setQueryData(queryKeys.groups, [group]);
  client.setQueryData(queryKeys.sources(), nextSources);
  client.setQueryData(queryKeys.sessions, sessions);
  vi.spyOn(api, 'groups').mockImplementation(async () => client.getQueryData<Group[]>(queryKeys.groups) ?? []);
  vi.spyOn(api, 'sources').mockImplementation(async () => client.getQueryData<Source[]>(queryKeys.sources()) ?? []);
  vi.spyOn(api, 'sessions').mockImplementation(async () => client.getQueryData<UaSession[]>(queryKeys.sessions) ?? []);
  const select = vi.spyOn(api, 'selectSessionSource').mockResolvedValue(undefined);
  render(<QueryClientProvider client={client}><MemoryRouter><GroupsPage /></MemoryRouter></QueryClientProvider>);
  return { client, select, user: userEvent.setup() };
}

function drawer() {
  const element = document.querySelector<HTMLElement>('aside.detail-drawer.is-open');
  expect(element).not.toBeNull();
  return within(element!);
}

describe('one Session context across group controls', () => {
  it('opens the drawer and submits its switch for the second Session chosen at the top', async () => {
    const { user, select } = setup();
    await user.selectOptions(screen.getByRole('combobox', { name: /^当前查看会话/ }), 'session-2');
    await user.click(screen.getByRole('button', { name: '查看 fire' }));
    expect(drawer().getByRole('combobox', { name: /^查看上级会话/ })).toHaveValue('session-2');
    await user.click(drawer().getByRole('button', { name: '切换当前 Session 的整组来源' }));
    const dialog = within(screen.getByRole('dialog'));
    expect(dialog.getByText(/SCADA B/)).toBeInTheDocument();
    expect(dialog.queryByText(/SCADA A/)).not.toBeInTheDocument();
    await user.click(dialog.getByRole('radio', { name: /rcd-1/ }));
    await user.click(dialog.getByRole('checkbox', { name: '确认已核对目标下级状态' }));
    await user.click(dialog.getByRole('button', { name: '确认切换' }));
    await waitFor(() => expect(select).toHaveBeenCalledTimes(1));
    expect(select).toHaveBeenCalledWith('session-2', 'fire', { sourceId: 'rcd-1', revision: 12 });
  });

  it('shares drawer changes with the top and preserves them when closing and reopening', async () => {
    const { user } = setup();
    await user.click(screen.getByRole('button', { name: '查看 fire' }));
    await user.selectOptions(drawer().getByRole('combobox', { name: /^查看上级会话/ }), 'session-2');
    expect(screen.getByRole('combobox', { name: /^当前查看会话/ })).toHaveValue('session-2');
    await user.click(drawer().getByRole('button', { name: '关闭详情' }));
    await user.click(screen.getByRole('button', { name: '查看 fire' }));
    expect(drawer().getByRole('combobox', { name: /^查看上级会话/ })).toHaveValue('session-2');
    await user.click(drawer().getByRole('button', { name: '关闭详情' }));
    await user.click(screen.getByRole('button', { name: '切源' }));
    expect(within(screen.getByRole('dialog')).getByText(/SCADA B/)).toBeInTheDocument();
  });

  it('keeps the expired target in both pickers and disables the open dialog even when another Session lives', async () => {
    const { user, client, select } = setup();
    await user.selectOptions(screen.getByRole('combobox', { name: /^当前查看会话/ }), 'session-2');
    await user.click(screen.getByRole('button', { name: '查看 fire' }));
    await user.click(drawer().getByRole('button', { name: '切换当前 Session 的整组来源' }));
    await user.click(screen.getByRole('checkbox', { name: '确认已核对目标下级状态' }));
    act(() => { client.setQueryData(queryKeys.sessions, [sessions[0]]); });
    await waitFor(() => expect(screen.getByRole('button', { name: '确认切换' })).toBeDisabled());
    expect(within(screen.getByRole('dialog')).getByText('目标 Session 已结束')).toBeInTheDocument();
    expect(screen.getByRole('combobox', { name: /^当前查看会话/ })).toHaveValue('session-2');
    expect(drawer().getByRole('combobox', { name: /^查看上级会话/ })).toHaveValue('session-2');
    await user.click(screen.getByRole('button', { name: '关闭弹窗' }));
    expect(drawer().getByRole('button', { name: '切换当前 Session 的整组来源' })).toBeDisabled();
    await user.click(drawer().getByRole('button', { name: '关闭详情' }));
    await user.click(screen.getByRole('button', { name: '查看 fire' }));
    expect(drawer().getByRole('button', { name: '切换当前 Session 的整组来源' })).toBeDisabled();
    expect(screen.getByRole('button', { name: '切源' })).toBeDisabled();
    expect(select).not.toHaveBeenCalled();
    await user.selectOptions(drawer().getByRole('combobox', { name: /^查看上级会话/ }), 'session-1');
    expect(drawer().getByRole('button', { name: '切换当前 Session 的整组来源' })).toBeEnabled();
    expect(screen.getByRole('combobox', { name: /^当前查看会话/ })).toHaveValue('session-1');
  });

  it('does not label a healthy source usable before its group directory is discovered', async () => {
    const { user } = setup([sources[0], { ...sources[1], availableGroupIds: [] }]);
    await user.click(screen.getByRole('button', { name: '查看 fire' }));
    expect(drawer().getByText('rcd-2（不可选）')).toBeInTheDocument();
    expect(drawer().getByText('未发现本组目录')).toBeInTheDocument();
    expect(drawer().queryByText('rcd-2（可用）')).not.toBeInTheDocument();
  });

  it('locks and unlocks the page and drawer from fresh pending-command counts', async () => {
    const { user, client } = setup();
    await user.click(screen.getByRole('button', { name: '查看 fire' }));
    act(() => { client.setQueryData(queryKeys.groups, [{ ...group, pendingCommandCount: 1 }]); });
    await waitFor(() => expect(screen.getByRole('button', { name: '切源' })).toBeDisabled());
    expect(drawer().getByRole('button', { name: '切换当前 Session 的整组来源' })).toBeDisabled();
    expect(drawer().getByRole('button', { name: '设置新 Session 默认源' })).toBeDisabled();
    act(() => { client.setQueryData(queryKeys.groups, [group]); });
    await waitFor(() => expect(screen.getByRole('button', { name: '切源' })).toBeEnabled());
    expect(drawer().getByRole('button', { name: '设置新 Session 默认源' })).toBeEnabled();
  });
});
