import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { api } from '../../api/client';
import type { Group, Source } from '../../api/types';
import { SetupWizard } from './SetupWizard';

const group: Group = { id: 'fire', name: 'fire', defaultSourceId: '', selectionRevision: 1, sourceCount: 0, goodPoints: 0, badPoints: 0, totalPoints: 0, pendingCommandCount: 0, status: 'good' };
const source: Source = { id: 'lower-a', name: 'lower-a', groupId: 'fire', endpoint: 'opc.tcp://127.0.0.1:4841', securityPolicy: 'None', securityMode: 'None', hasPassword: false, enabled: true, connectionState: 'Disconnected', sessionState: 'Inactive', subscriptionState: 'Inactive', catalogState: 'Discovering', reconnectCount: 0, monitoredCount: 0, goodPoints: 0, badPoints: 0, totalPoints: 0, revision: 1 };

afterEach(() => vi.restoreAllMocks());

function setup() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  const onClose = vi.fn();
  const onComplete = vi.fn();
  render(<QueryClientProvider client={client}><SetupWizard open onClose={onClose} onComplete={onComplete} /></QueryClientProvider>);
  return { user: userEvent.setup(), onClose, onComplete };
}

describe('first configuration transaction', () => {
  it('freezes the confirmed parameters and prevents closing or resubmitting during creation', async () => {
    let finish!: (value: Group) => void;
    const pending = new Promise<Group>((resolve) => { finish = resolve; });
    const createGroup = vi.spyOn(api, 'createGroup').mockReturnValue(pending);
    const createSource = vi.spyOn(api, 'createSource').mockResolvedValue(source);
    const { user, onClose, onComplete } = setup();
    await user.type(screen.getByLabelText('Group ID'), 'fire');
    await user.type(screen.getByLabelText('Source ID'), 'lower-a');
    await user.click(screen.getByRole('button', { name: '创建并开始连接' }));
    await waitFor(() => expect(createGroup).toHaveBeenCalledOnce());
    expect(screen.getByLabelText('Group ID')).toBeDisabled();
    expect(screen.getByLabelText('Source ID')).toBeDisabled();
    expect(screen.getByLabelText('Endpoint')).toBeDisabled();
    expect(screen.getByRole('button', { name: '取消' })).toBeDisabled();
    await user.click(screen.getByRole('button', { name: '关闭弹窗' }));
    await user.keyboard('{Escape}');
    fireEvent.submit(document.getElementById('setup-wizard-form')!);
    expect(onClose).not.toHaveBeenCalled();
    expect(createGroup).toHaveBeenCalledOnce();
    await act(async () => { finish(group); await pending; });
    await waitFor(() => expect(onComplete).toHaveBeenCalledOnce());
    expect(createSource).toHaveBeenCalledOnce();
    expect(createSource).toHaveBeenCalledWith(expect.objectContaining({ id: 'lower-a', groupId: 'fire' }));
    expect(onClose).toHaveBeenCalledOnce();
  });

  it('preserves the created group and allows correcting only the failed source before retry', async () => {
    const createGroup = vi.spyOn(api, 'createGroup').mockResolvedValue(group);
    const createSource = vi.spyOn(api, 'createSource').mockRejectedValueOnce(new Error('初次来源失败')).mockResolvedValueOnce({ ...source, id: 'lower-b' });
    const { user, onComplete } = setup();
    await user.type(screen.getByLabelText('Group ID'), 'fire');
    await user.type(screen.getByLabelText('Source ID'), 'lower-a');
    await user.click(screen.getByRole('button', { name: '创建并开始连接' }));
    await screen.findByText(/初次来源失败/);
    expect(screen.getByLabelText('Group ID')).toBeDisabled();
    expect(screen.getByLabelText('Source ID')).toBeEnabled();
    await user.clear(screen.getByLabelText('Source ID'));
    await user.type(screen.getByLabelText('Source ID'), 'lower-b');
    await user.click(screen.getByRole('button', { name: '创建并开始连接' }));
    await waitFor(() => expect(onComplete).toHaveBeenCalledOnce());
    expect(createGroup).toHaveBeenCalledOnce();
    expect(createSource).toHaveBeenCalledTimes(2);
    expect(createSource).toHaveBeenLastCalledWith(expect.objectContaining({ id: 'lower-b', groupId: 'fire' }));
  });
});
