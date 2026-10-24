import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, expect, it, vi } from 'vitest';
import * as api from '../../api/redundancy';
import type { Group, Source, UaSession } from '../../api/types';
import { RedundancyDialog } from './RedundancyDialog';

afterEach(() => vi.restoreAllMocks());
const group: Group = { id: 'g', name: '组', defaultSourceId: 'a', selectionRevision: 1, sourceCount: 2, goodPoints: 1, badPoints: 0, totalPoints: 1, pendingCommandCount: 0, status: 'good' };
const sources = ['a', 'b'].map((id) => ({ id, groupId: 'g', name: id })) as Source[];
const sessions: UaSession[] = [{ id: 's', clientName: 'Test', remoteAddress: '127.0.0.1', selectionRevision: 3, selectedSources: { g: 'a' }, routeRevisions: { g: 3 } }];
function setup() {
  const view: api.RedundancyView = { policy: { group_id: 'g', revision: 2, enabled: false, default_automatic: false, priority: ['a', 'b'], failure_seconds: 5, healthy_seconds: 5, cooldown_seconds: 30, compare_enabled: false, left_source_id: 'a', right_source_id: 'b', difference_seconds: 5, absolute_tolerance: 0, relative_tolerance: 0, max_skew_ms: 2000 }, routes: { s: { source_id: 'a', revision: 3, automatic: false } }, health: [], comparison_state: 'Disabled', compared_count: 0, skipped_count: 0, difference_count: 0, differences: [], last_error: '', last_switch_reason: '' };
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity }, mutations: { retry: false } } });
  client.setQueryData(api.redundancyKey('g'), view);
  vi.spyOn(api, 'getRedundancy').mockImplementation(async () => client.getQueryData<api.RedundancyView>(api.redundancyKey('g'))!);
  const save = vi.spyOn(api, 'saveRedundancy').mockImplementation(async (p) => p);
  const mode = vi.spyOn(api, 'setSessionAutomatic').mockResolvedValue(undefined);
  render(<QueryClientProvider client={client}><RedundancyDialog group={group} sources={sources} sessions={sessions} onClose={() => {}} /></QueryClientProvider>);
  return { client, view, save, mode, user: userEvent.setup() };
}
it('keeps both features off by default and requires explicit confirmation', async () => {
  const { user, save } = setup();
  expect(screen.getByRole('checkbox', { name: /开启本组自动切源/ })).not.toBeChecked();
  expect(screen.getByRole('checkbox', { name: /开启后台持续差异告警/ })).not.toBeChecked();
  expect(screen.getByRole('button', { name: '保存策略' })).toBeDisabled();
  await user.click(screen.getByRole('checkbox', { name: /开启本组自动切源/ }));
  await user.click(screen.getByRole('checkbox', { name: /开启后台持续差异告警/ }));
  await user.click(screen.getByRole('checkbox', { name: /已确认本组物理设备/ }));
  await user.click(screen.getByRole('button', { name: '保存策略' }));
  await waitFor(() => expect(save).toHaveBeenCalledWith(expect.objectContaining({ revision: 2, enabled: true, compare_enabled: true, default_automatic: false }), expect.anything()));
});
it('does not silently replace dirty policy when a newer revision arrives', async () => {
  const { client, view, user, save } = setup();
  await user.click(screen.getByRole('checkbox', { name: /开启本组自动切源/ }));
  act(() => client.setQueryData(api.redundancyKey('g'), { ...view, policy: { ...view.policy, revision: 3 } }));
  expect(await screen.findByText(/策略已被其他操作修改/)).toBeInTheDocument();
  expect(screen.getByRole('checkbox', { name: /开启本组自动切源/ })).toBeChecked();
  expect(screen.getByRole('button', { name: '保存策略' })).toBeDisabled();
  expect(save).not.toHaveBeenCalled();
});
it('changes only the selected Session participation with its route revision', async () => {
  const { client, view, mode, user } = setup();
  act(() => client.setQueryData(api.redundancyKey('g'), { ...view, policy: { ...view.policy, enabled: true } }));
  await waitFor(() => expect(screen.getByRole('option', { name: '跟随自动' })).not.toBeDisabled());
  await user.selectOptions(screen.getByRole('combobox', { name: 's 参与方式' }), 'auto');
  await waitFor(() => expect(mode).toHaveBeenCalledWith('g', 's', true, 3));
});
