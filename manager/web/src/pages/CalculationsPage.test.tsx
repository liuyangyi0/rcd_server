import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { api } from '../api/client';
import { calculationApi, calculationKeys } from '../api/calculations';
import { operationsApi } from '../api/operations';
import type { CalculationDraft, CalculationResult, CalculationStatus, PageResult, Source } from '../api/types';
import CalculationsPage from './CalculationsPage';

const source: Source = { id: 'lower-a', name: '下级 A', groupId: 'g1', endpoint: '', securityPolicy: 'None', securityMode: 'None', hasPassword: false, enabled: true, connectionState: 'Connected', sessionState: 'Active', subscriptionState: 'Active', catalogState: 'Ready', reconnectCount: 0, monitoredCount: 2, goodPoints: 2, badPoints: 0, totalPoints: 2, revision: 7 };

function setup(route = '/calculations', status: Partial<CalculationStatus> = {}) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  vi.spyOn(calculationApi, 'status').mockResolvedValue({ routingMode: 'DefaultSources', activeRevision: 4, configHash: 'abc', ruleCount: 1, goodCount: 1, badCount: 0, defaultRouteRevision: 9, lastEvaluationDurationMs: 12.5, pendingRestart: false, ...status });
  render(<QueryClientProvider client={client}><MemoryRouter initialEntries={[route]}><CalculationsPage /></MemoryRouter></QueryClientProvider>);
  return userEvent.setup();
}

afterEach(() => { window.dispatchEvent(new Event('rcd-auth-cleared')); vi.restoreAllMocks(); });

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (error: Error) => void;
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}

describe('calculation center', () => {
  it('explains default-source routing and renders live calculation quality', async () => {
    vi.spyOn(calculationApi, 'results').mockResolvedValue({ items: [{ nodeId: 'CALC_A', dataType: 'Double', expression: 'IF(A, B, C)', value: 12.5, statusCode: 0, communicationInputs: [], dependencyCount: 3, configRevision: 4, defaultRouteRevision: 9 }], total: 1, page: 1, pageSize: 50 });
    setup();
    expect(screen.getByText(/所有计算结果均不随上级 OPC Session 单独选源变化/)).toBeInTheDocument();
    const row = await screen.findByRole('row', { name: /CALC_A/ });
    expect(row).toHaveTextContent('12.5');
    expect(row).toHaveTextContent('Good');
    expect(row).toHaveTextContent('IF(A, B, C)');
  });

  it('keeps the calculated result while explaining zero-input communication quality', async () => {
    vi.spyOn(calculationApi, 'results').mockResolvedValue({ items: [{ nodeId: 'CALC_OFFLINE', dataType: 'Double', expression: 'A + 5', value: 5, statusCode: 0x80310000, communicationInputs: ['A'], dependencyCount: 1, configRevision: 4, defaultRouteRevision: 9 }], total: 1, page: 1, pageSize: 50 });
    setup();
    expect(screen.getByText(/原始采集通信中断时按零参与计算/)).toBeInTheDocument();
    const row = await screen.findByRole('row', { name: /CALC_OFFLINE/ });
    expect(row).toHaveTextContent('5');
    expect(row).toHaveTextContent('上游点 A 通信中断，按 0 参与计算');
    expect(row).toHaveTextContent('BadNoCommunication');
    expect(row).not.toHaveTextContent('已清零');
  });

  it('batch changes only selected supported lowers and leaves restart explicit', async () => {
    vi.spyOn(api, 'sources').mockResolvedValue([source]);
    vi.spyOn(operationsApi, 'capabilities').mockResolvedValue({ source_id: 'lower-a', source_revision: 7, supported: true, schema_version: 1, features: ['calculation_mode'], max_config_bytes: 2_097_152, max_log_bytes: 65_536, configuration_apply: 'restart' });
    vi.spyOn(operationsApi, 'status').mockResolvedValue({ source_id: 'lower-a', source_revision: 7, schema_version: 1, revision: 'status-1', ports: [], calculation: { active_mode: 'local', configured_mode: 'local', rule_count: 6683, config_hash: 'abc' } });
    vi.spyOn(operationsApi, 'config').mockResolvedValue({ source_id: 'lower-a', source_revision: 7, name: 'config.toml', kind: 'toml', size_bytes: 10, revision: 'file-r1', active_revision: 'file-r1', restart_required: false, content: '', backups: [] });
    const setMode = vi.spyOn(operationsApi, 'calculationMode').mockResolvedValue({ status: 'Saved' });
    const user = setup('/calculations?tab=lowers');
    await user.click(await screen.findByRole('checkbox', { name: '选择 下级 A' }));
    await user.click(screen.getByRole('button', { name: '批量设为管理端计算' }));
    await waitFor(() => expect(setMode).toHaveBeenCalledWith({ id: 'lower-a', revision: 7 }, 'file-r1', 'manager'));
    expect(screen.getByText(/尚未重启/)).toBeInTheDocument();
    expect(screen.getByRole('link', { name: '前往运维重启' })).toBeInTheDocument();
  });

  it('inserts a digit-leading state input into an expression without rewriting it', async () => {
    const name = '9CYE91GH201_COM1';
    const content = '[[rules]]\nkks_calc="STATE_COPY"\ndata_type="UInt32"\nexpression=""\n';
    vi.spyOn(api, 'sources').mockResolvedValue([]);
    vi.spyOn(calculationApi, 'draft').mockResolvedValue({ revision: 2, content, valid: true, hash: '', problems: [] });
    const save = vi.spyOn(calculationApi, 'saveDraft').mockResolvedValue({ revision: 3, content: content.replace('expression=""', `expression="${name}"`), valid: true, hash: '', problems: [] });
    const user = setup(`/calculations?tab=config&insert=${name}`);
    const editor = await screen.findByRole('textbox', { name: /中央计算 TOML/ }) as HTMLTextAreaElement;
    editor.focus();
    const cursor = content.indexOf('expression="') + 'expression="'.length;
    editor.setSelectionRange(cursor, cursor);
    await user.click(screen.getByRole('button', { name: `插入 ${name}` }));
    const expected = content.replace('expression=""', `expression="${name}"`);
    expect(editor).toHaveValue(expected);
    await user.click(screen.getByRole('button', { name: '保存草稿' }));
    await waitFor(() => expect(save).toHaveBeenCalledWith(2, expected));
  });

  it('keeps unsaved edits across tabs and pins the original revision on background updates', async () => {
    const original = { revision: 10, content: '# original\n', valid: true, hash: 'old', problems: [] };
    const latest = { ...original, revision: 11, content: '# another editor\n', hash: 'new' };
    vi.spyOn(api, 'sources').mockResolvedValue([]);
    const fetchDraft = vi.spyOn(calculationApi, 'draft').mockResolvedValue(original);
    vi.spyOn(calculationApi, 'status').mockResolvedValue({ routingMode: 'DefaultSources', activeRevision: 1, configHash: '', ruleCount: 0, goodCount: 0, badCount: 0, defaultRouteRevision: 1, lastEvaluationDurationMs: 0, pendingRestart: false });
    vi.spyOn(calculationApi, 'results').mockResolvedValue({ items: [], total: 0, page: 1, pageSize: 50 });
    const save = vi.spyOn(calculationApi, 'saveDraft').mockRejectedValue(new Error('revision conflict'));
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    render(<QueryClientProvider client={qc}><MemoryRouter initialEntries={['/calculations?tab=config']}><CalculationsPage /></MemoryRouter></QueryClientProvider>);
    const user = userEvent.setup();
    const editor = await screen.findByRole('textbox', { name: /中央计算 TOML/ });
    fireEvent.change(editor, { target: { value: '# my unsaved edit\n' } });
    act(() => qc.setQueryData(calculationKeys.draft, latest));
    expect(screen.getByRole('textbox', { name: /中央计算 TOML/ })).toHaveValue('# my unsaved edit\n');
    expect(await screen.findByText(/服务器草稿已变为 r11/)).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: '实时结果' }));
    await user.click(screen.getByRole('button', { name: '规则配置' }));
    expect(await screen.findByRole('textbox', { name: /中央计算 TOML/ })).toHaveValue('# my unsaved edit\n');
    await user.click(screen.getByRole('button', { name: '保存草稿' }));
    await waitFor(() => expect(save).toHaveBeenCalledWith(10, '# my unsaved edit\n'));
    expect(screen.getByRole('textbox', { name: /中央计算 TOML/ })).toHaveValue('# my unsaved edit\n');
    fetchDraft.mockResolvedValue(latest); vi.spyOn(window, 'confirm').mockReturnValue(true);
    await user.click(screen.getByRole('button', { name: '重新载入服务器草稿' }));
    await waitFor(() => expect(screen.getByRole('textbox', { name: /中央计算 TOML/ })).toHaveValue('# another editor\n'));
    expect(screen.getByRole('button', { name: '保存草稿' })).toBeDisabled();
  });

  it('does not label old result rows as the new quality filter while its request is pending', async () => {
    const good = { nodeId: 'OLD_GOOD', dataType: 'Double', expression: '1', value: 1, statusCode: 0, communicationInputs: [], dependencyCount: 0, configRevision: 4, defaultRouteRevision: 9 };
    const pending = deferred<PageResult<CalculationResult>>();
    vi.spyOn(calculationApi, 'results').mockResolvedValueOnce({ items: [good], total: 1, page: 1, pageSize: 50 }).mockImplementation(() => pending.promise);
    const user = setup();
    await screen.findByRole('row', { name: /OLD_GOOD/ });
    await user.selectOptions(screen.getByRole('combobox', { name: /质量/ }), 'bad');
    expect(screen.queryByRole('row', { name: /OLD_GOOD/ })).not.toBeInTheDocument();
    expect(screen.getByText('正在读取中央计算结果…')).toBeInTheDocument();
    await act(async () => pending.resolve({ items: [{ ...good, nodeId: 'NEW_BAD', statusCode: 0x80310000 }], total: 1, page: 1, pageSize: 50 }));
    await screen.findByRole('row', { name: /NEW_BAD/ });
  });

  it('keeps a newer editor copy when a save from an unmounted tab finishes', async () => {
    const original: CalculationDraft = { revision: 10, content: '# original\n', valid: true, hash: '', problems: [] };
    const saved = { ...original, revision: 11, content: '# edit A\n' };
    const response = deferred<CalculationDraft>();
    vi.spyOn(api, 'sources').mockResolvedValue([]);
    const draft = vi.spyOn(calculationApi, 'draft').mockResolvedValue(original);
    vi.spyOn(calculationApi, 'results').mockResolvedValue({ items: [], total: 0, page: 1, pageSize: 50 });
    const save = vi.spyOn(calculationApi, 'saveDraft').mockImplementation(() => response.promise);
    const user = setup('/calculations?tab=config');
    fireEvent.change(await screen.findByRole('textbox', { name: /中央计算 TOML/ }), { target: { value: '# edit A\n' } });
    await user.click(screen.getByRole('button', { name: '保存草稿' }));
    await waitFor(() => expect(save).toHaveBeenCalledWith(10, '# edit A\n'));
    await user.click(screen.getByRole('button', { name: '实时结果' }));
    await user.click(screen.getByRole('button', { name: '规则配置' }));
    fireEvent.change(await screen.findByRole('textbox', { name: /中央计算 TOML/ }), { target: { value: '# newer edit B\n' } });
    draft.mockResolvedValue(saved);
    await act(async () => response.resolve(saved));
    await screen.findByText(/服务器草稿已变为 r11/);
    await user.click(screen.getByRole('button', { name: '实时结果' }));
    await user.click(screen.getByRole('button', { name: '规则配置' }));
    expect(await screen.findByRole('textbox', { name: /中央计算 TOML/ })).toHaveValue('# newer edit B\n');
  });

  it('replaces previous lower-mode success with pending and then the current failure', async () => {
    vi.spyOn(api, 'sources').mockResolvedValue([source]);
    vi.spyOn(operationsApi, 'capabilities').mockResolvedValue({ source_id: 'lower-a', source_revision: 7, supported: true, schema_version: 1, features: ['calculation_mode'], max_config_bytes: 2_097_152, max_log_bytes: 65_536, configuration_apply: 'restart' });
    vi.spyOn(operationsApi, 'status').mockResolvedValue({ source_id: 'lower-a', source_revision: 7, schema_version: 1, revision: 'status-1', ports: [], calculation: { active_mode: 'local', configured_mode: 'local', rule_count: 1, config_hash: 'abc' } });
    vi.spyOn(operationsApi, 'config').mockResolvedValue({ source_id: 'lower-a', source_revision: 7, name: 'config.toml', kind: 'toml', size_bytes: 0, revision: 'file-r1', active_revision: 'file-r1', restart_required: false, content: '', backups: [] });
    const pending = deferred<{ status: string }>();
    vi.spyOn(operationsApi, 'calculationMode').mockResolvedValueOnce({ status: 'Saved' }).mockImplementationOnce(() => pending.promise);
    const user = setup('/calculations?tab=lowers');
    await user.click(await screen.findByRole('button', { name: '设为管理端计算' }));
    await screen.findByText(/已保存为/);
    await waitFor(() => expect(screen.getByRole('button', { name: '设为管理端计算' })).toBeEnabled());
    await user.click(screen.getByRole('button', { name: '设为管理端计算' }));
    expect(screen.queryByText(/已保存为/)).not.toBeInTheDocument();
    expect(screen.getByText('正在保存…')).toBeInTheDocument();
    await act(async () => pending.reject(new Error('第二次保存失败')));
    expect(await screen.findByText('第二次保存失败')).toBeInTheDocument();
    expect(screen.queryByText(/已保存为/)).not.toBeInTheDocument();
  });

  it('restores history with the confirmed zero revision after central calculation was deactivated', async () => {
    vi.spyOn(calculationApi, 'versions').mockResolvedValue({ items: [{ id: 3, revision: 7, hash: 'old', ruleCount: 1, addedCount: 0, removedCount: 0, changedCount: 0, state: 'archived', actor: 'admin' }], total: 1, page: 1, pageSize: 50 });
    const restore = vi.spyOn(calculationApi, 'restore').mockResolvedValue({ id: 4, revision: 8, hash: 'new', ruleCount: 1, addedCount: 0, removedCount: 0, changedCount: 0, state: 'active', actor: 'admin' });
    const user = setup('/calculations?tab=versions', { activeRevision: 0 });
    const button = await screen.findByRole('button', { name: '复制并激活历史版本' });
    await waitFor(() => expect(button).toBeEnabled());
    await user.click(button);
    await user.click(screen.getByRole('button', { name: '确认复制并激活' }));
    await waitFor(() => expect(restore).toHaveBeenCalledWith(3, 0));
  });

  it('never substitutes a newly observed revision into an already open restore confirmation', async () => {
    const status: CalculationStatus = { routingMode: 'DefaultSources', activeRevision: 0, configHash: '', ruleCount: 0, goodCount: 0, badCount: 0, defaultRouteRevision: 0, lastEvaluationDurationMs: 0, pendingRestart: false };
    vi.spyOn(calculationApi, 'status').mockResolvedValue(status);
    vi.spyOn(calculationApi, 'versions').mockResolvedValue({ items: [{ id: 3, revision: 7, hash: 'old', ruleCount: 1, addedCount: 0, removedCount: 0, changedCount: 0, state: 'archived', actor: 'admin' }], total: 1, page: 1, pageSize: 50 });
    const restore = vi.spyOn(calculationApi, 'restore');
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    render(<QueryClientProvider client={qc}><MemoryRouter initialEntries={['/calculations?tab=versions']}><CalculationsPage /></MemoryRouter></QueryClientProvider>);
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: '复制并激活历史版本' }));
    act(() => qc.setQueryData(calculationKeys.status, { ...status, activeRevision: 9 }));
    expect(await screen.findByText(/活动版本已变化/)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: '确认复制并激活' })).toBeDisabled();
    expect(restore).not.toHaveBeenCalled();
  });

  it('keeps restore disabled until the status query explicitly confirms no active version', async () => {
    const pending = deferred<CalculationStatus>();
    vi.spyOn(calculationApi, 'status').mockImplementation(() => pending.promise);
    vi.spyOn(calculationApi, 'versions').mockResolvedValue({ items: [{ id: 3, revision: 7, hash: 'old', ruleCount: 1, addedCount: 0, removedCount: 0, changedCount: 0, state: 'archived', actor: 'admin' }], total: 1, page: 1, pageSize: 50 });
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={qc}><MemoryRouter initialEntries={['/calculations?tab=versions']}><CalculationsPage /></MemoryRouter></QueryClientProvider>);
    expect(await screen.findByRole('button', { name: '回滚为新版本' })).toBeDisabled();
    expect(screen.getByText('尚未确认当前活动版本，暂不能恢复历史版本。')).toBeInTheDocument();
    await act(async () => pending.resolve({ routingMode: 'DefaultSources', activeRevision: 0, configHash: '', ruleCount: 0, goodCount: 0, badCount: 0, defaultRouteRevision: 0, lastEvaluationDurationMs: 0, pendingRestart: false }));
    expect(await screen.findByRole('button', { name: '复制并激活历史版本' })).toBeEnabled();
  });
});
