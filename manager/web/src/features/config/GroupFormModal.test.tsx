import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { api } from '../../api/client';
import type { Group } from '../../api/types';
import { GroupFormModal } from './GroupFormModal';

const group: Group = { id: 'fire', name: 'Fire', defaultSourceId: 'lower-a', selectionRevision: 3, sourceCount: 1, goodPoints: 1, badPoints: 0, totalPoints: 1, pendingCommandCount: 0, status: 'good' };

afterEach(() => vi.restoreAllMocks());

describe('GroupFormModal edit ownership', () => {
  it('keeps the draft and original revision when the background group changes', async () => {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    const update = vi.spyOn(api, 'updateGroup').mockResolvedValue(group);
    const form = (target: Group) => <QueryClientProvider client={client}><GroupFormModal open group={target} onClose={() => undefined} onSaved={() => undefined} /></QueryClientProvider>;
    const view = render(form(group));
    const user = userEvent.setup();
    await user.clear(screen.getByLabelText('显示名称'));
    await user.type(screen.getByLabelText('显示名称'), '未保存的新名称');
    view.rerender(form({ ...group, name: '其他人的名称', defaultSourceId: 'lower-b', selectionRevision: 4 }));
    expect(screen.getByLabelText('显示名称')).toHaveValue('未保存的新名称');
    await user.click(screen.getByRole('button', { name: '保存' }));
    await waitFor(() => expect(update).toHaveBeenCalledOnce());
    expect(update).toHaveBeenCalledWith('fire', { id: 'fire', name: '未保存的新名称', defaultSourceId: 'lower-a', revision: 3 });
  });

  it('does not close a newly opened form when a previous save resolves', async () => {
    let finish!: (saved: Group) => void;
    const pending = new Promise<Group>((resolve) => { finish = resolve; });
    const update = vi.spyOn(api, 'updateGroup').mockReturnValue(pending);
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    const invalidate = vi.spyOn(client, 'invalidateQueries');
    const onClose = vi.fn();
    const onSaved = vi.fn();
    const form = (open: boolean, target?: Group) => <QueryClientProvider client={client}><GroupFormModal open={open} group={target} onClose={onClose} onSaved={onSaved} /></QueryClientProvider>;
    const view = render(form(true, group));
    const user = userEvent.setup();
    await user.click(screen.getByRole('button', { name: '保存' }));
    await waitFor(() => expect(update).toHaveBeenCalledOnce());
    expect(screen.getByLabelText('显示名称')).toBeDisabled();
    view.rerender(form(false, group));
    view.rerender(form(true));
    await user.type(screen.getByLabelText('Group ID'), 'another');
    await user.type(screen.getByLabelText('显示名称'), '新的分组草稿');
    await act(async () => { finish(group); await pending; });
    await waitFor(() => expect(invalidate).toHaveBeenCalledTimes(2));
    expect(onClose).not.toHaveBeenCalled();
    expect(onSaved).not.toHaveBeenCalled();
    expect(screen.getByRole('dialog')).toBeInTheDocument();
    expect(screen.getByLabelText('显示名称')).toHaveValue('新的分组草稿');
  });
});
