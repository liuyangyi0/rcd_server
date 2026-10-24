import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { processKeys, type ProcessBinding, type ProcessOperation, type SourceProcessStatus } from '../../api/process';
import { operationsApi, type ConfigDocument } from '../../api/operations';
import type { Source } from '../../api/types';
import { ProcessTab } from './ProcessTab';
import SourceOperationsDrawer from './SourceOperationsDrawer';
import { agentIdentityFixture } from './processIdentityFixture';

const target = { id: 'lower-process', revision: 2 };
const source: Source = { id: target.id, name: '物理下级', groupId: 'group-1', endpoint: 'opc.tcp://lower:4840', securityPolicy: 'None', securityMode: 'None', hasPassword: false, enabled: true, connectionState: 'Connected', sessionState: 'Active', subscriptionState: 'Active', catalogState: 'Ready', reconnectCount: 0, monitoredCount: 1, goodPoints: 1, badPoints: 0, totalPoints: 1, revision: 2 };
const originalHash = 'a'.repeat(64);
const savedHash = 'b'.repeat(64);
const at = '2026-08-28T01:00:00Z';
function json(body: unknown, status = 200) { return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } }); }
function deferredAction() {
  let release!: () => void;
  const promise = new Promise<void>((resolve) => { release = resolve; });
  return { promise, release };
}

function server() {
  const binding: ProcessBinding = { source_id: target.id, source_revision: 2, revision: 4, agent_url: 'http://agent:4851', instance_id: 'fixture-a' };
  const status: SourceProcessStatus = { source_id: target.id, source_revision: 2, binding_revision: 4, instance_id: 'fixture-a', schema_version: 1, revision: 'agent-r1', state: 'stopped', desired_state: 'stopped', executable: '/fixture/a/rcd_server', work_dir: '/fixture/a', restart_policy: 'on-failure', restart_count: 1, last_exit_at: at, exit_code: 0 };
  const file: ConfigDocument = { source_id: target.id, source_revision: 2, name: 'calc.toml', kind: 'calculation', revision: originalHash, active_revision: originalHash, restart_required: false, content: '# original', size_bytes: 10, backups: [] };
  status.lifecycle_supported = true; status.gateway_instance_id = 'gateway-fixture';
  const state: { binding: ProcessBinding; status: SourceProcessStatus; file: ConfigDocument; job?: ProcessOperation; supported: boolean; statusFailure: number; actionFailure: number; actionError: string; actionGate?: Promise<void>; wrongResponseIdentity: boolean; bindingFailure: number; missingJob: boolean; resolveUnknown: boolean; valid: boolean; saveFailure: number; log: string } = {
    binding, status, file, supported: true, statusFailure: 0, actionFailure: 0, actionError: 'timeout', wrongResponseIdentity: false, bindingFailure: 0, missingJob: false, resolveUnknown: false, valid: true, saveFailure: 0, log: '[INFO] fixture stdout\n[ERROR] 中文 stderr\n',
  };
  const calls = vi.fn().mockImplementation(async (raw: string, init: RequestInit = {}) => {
    const url = new URL(raw, 'http://localhost');
    const body = init.body ? JSON.parse(String(init.body)) as Record<string, unknown> : {};
    const makeJob = (phase: ProcessOperation['state']): ProcessOperation => ({ source_id: target.id, source_revision: 2, binding_revision: Number(body.binding_revision), instance_id: state.binding.instance_id, request_id: String(body.request_id), action: body.action as ProcessOperation['action'], state: phase, created_at: at, updated_at: at });
    if (url.pathname.endsWith('/process/binding')) {
      if (init.method === 'PUT') {
        if (state.bindingFailure) return json({ error: { code: 'conflict', message: 'binding changed' } }, state.bindingFailure);
        state.binding = { ...state.binding, revision: state.binding.revision + 1, agent_url: String(body.agent_url), instance_id: String(body.instance_id) };
        state.status = { ...state.status, binding_revision: state.binding.revision, instance_id: state.binding.instance_id };
      }
      return json(state.binding);
    }
    if (url.pathname.endsWith('/process/status')) return state.statusFailure ? json({ error: { code: 'unavailable', message: 'agent not reachable' } }, state.statusFailure) : json(state.status);
    if (url.pathname.endsWith('/process/actions')) {
      // Hold before durable admission, not merely before the response body.
      // Premature GETs really return 404 and Resolve would seal the request.
      if (state.actionGate) await state.actionGate;
      if (state.actionFailure) return json({ error: { code: state.actionError, message: 'action response unavailable' } }, state.actionFailure);
      state.job = makeJob('accepted');
      if (state.wrongResponseIdentity) return json({ ...state.job, binding_revision: 99 }, 202);
      state.status = { ...state.status, revision: 'agent-r2', state: body.action === 'start' ? 'starting' : 'stopping', pending_request_id: state.job.request_id, pending_operation: state.job };
      return json(state.job, 202);
    }
    if (url.pathname.endsWith('/resolve')) {
      state.job = { ...makeJob(state.resolveUnknown ? 'unknown' : 'failed'), error_code: state.resolveUnknown ? '' : 'unconfirmed_cancelled' };
      state.missingJob = false;
      if (state.resolveUnknown) state.status = { ...state.status, pending_request_id: state.job.request_id, pending_operation: state.job };
      return json(state.job, state.resolveUnknown ? 202 : 200);
    }
    if (/\/process\/operations\/[^/]+$/.test(url.pathname)) return state.job && !state.missingJob ? json(state.job) : json({ error: { code: 'not_found', message: 'not admitted yet' } }, 404);
    if (url.pathname.endsWith('/process/operations')) return json({ source_id: target.id, source_revision: 2, items: state.job ? [state.job] : [], total: state.job ? 1 : 0, limit: 20, offset: Number(url.searchParams.get('offset') || 0) });
    if (url.pathname.endsWith('/process/log')) return json({ source_id: target.id, source_revision: 2, binding_revision: state.binding.revision, instance_id: state.binding.instance_id, text: state.log, size_bytes: 90_000, truncated: true });
    if (url.pathname.endsWith('/operations/capabilities')) return json({ source_id: target.id, source_revision: 2, supported: state.supported, schema_version: 1, features: state.supported ? ['acquisition', 'configuration', 'logs'] : [], max_config_bytes: 2_097_152, max_log_bytes: 65_536, configuration_apply: 'restart' });
    if (url.pathname.endsWith('/operations/status')) return json({ source_id: target.id, source_revision: 2, schema_version: 1, revision: 'lower-r1', ports: [] });
    if (url.pathname.endsWith('/operations/configs')) return json({ source_id: target.id, source_revision: 2, items: [state.file] });
    if (url.pathname.endsWith('/config/validate')) return json({ valid: state.valid, errors: state.valid ? [] : ['bad syntax'], warnings: [], restart_required: true });
    if (url.pathname.endsWith('/operations/config')) {
      if (init.method === 'PUT') {
        if (state.saveFailure) return json({ error: { code: 'conflict', message: 'changed elsewhere' } }, state.saveFailure);
        state.file = { ...state.file, content: String(body.content), revision: savedHash, restart_required: true };
      }
      return json(state.file);
    }
    if (url.pathname.endsWith('/operations/logs')) return json({ source_id: target.id, source_revision: 2, items: [] });
    return json({ error: { code: 'not_found', message: `unexpected ${url.pathname}` } }, 404);
  });
  vi.stubGlobal('fetch', calls);
  const finish = (phase: 'succeeded' | 'failed' = 'succeeded') => {
    if (!state.job) throw new Error('no accepted operation');
    const running = state.job.action !== 'stop';
    state.status = { ...state.status, revision: 'agent-r3', state: running ? 'running' : 'stopped', desired_state: running ? 'running' : 'stopped', pid: running ? 140 : undefined, started_at: running ? at : undefined, pending_request_id: undefined, pending_operation: undefined };
    state.job = { ...state.job, state: phase, updated_at: '2026-08-28T01:00:02Z', snapshot: state.status };
  };
  return { state, calls, finish };
}

function mount(options: { drawer?: boolean; source?: Source; writable?: boolean; connected?: boolean; intent?: { name: string; revision: string } } = {}) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  const onDirtyChange = vi.fn(); const onBusyChange = vi.fn(); const onControlPendingChange = vi.fn(); const onClose = vi.fn();
  const renderContent = (writable = options.writable ?? true) => <QueryClientProvider client={client}>{options.drawer ? <SourceOperationsDrawer source={options.source ?? source} onClose={onClose} /> : <ProcessTab target={target} writable={writable} connected={options.connected ?? false} restartIntent={options.intent} onClearRestartIntent={vi.fn()} onDirtyChange={onDirtyChange} onBusyChange={onBusyChange} onControlPendingChange={onControlPendingChange} onDialogChange={vi.fn()} />}</QueryClientProvider>;
  const view = render(renderContent());
  return { ...view, client, onDirtyChange, onBusyChange, onControlPendingChange, onClose, lock: () => view.rerender(renderContent(false)), updateIntent: (intent: { name: string; revision: string }) => { options.intent = intent; view.rerender(renderContent()); } };
}
async function confirmAction(action: '启动' | '停止' | '重启') {
  const user = userEvent.setup();
  await waitFor(() => expect(screen.getByRole('button', { name: `${action}下级` })).toBeEnabled());
  await user.click(screen.getByRole('button', { name: `${action}下级` }));
  const dialog = screen.getByRole('dialog', { name: `确认${action}下级进程` });
  const submit = within(dialog).getByRole('button', { name: `确认${action}进程` });
  expect(submit).toBeDisabled();
  expect(within(dialog).getByText(/整个进程/)).toBeInTheDocument();
  await user.click(within(dialog).getByRole('checkbox', { name: /我确认此操作影响/ }));
  await user.click(submit);
}
afterEach(() => { vi.unstubAllGlobals(); vi.useRealTimers(); window.sessionStorage.clear(); });

describe('physical process management', () => {
  it('shows the separate running agent identity and keeps it stale on refresh failure', async () => {
    const { state, calls } = server(); state.status.agent_identity = agentIdentityFixture;
    mount();
    const card = await screen.findByRole('region', { name: '守护代理运行版本' });
    expect(within(card).getByText('0.6.1-test')).toBeInTheDocument();
    expect(card.querySelector('details')).not.toHaveAttribute('open');
    await userEvent.setup().click(within(card).getByText('构建与启动详情'));
    expect(card.querySelector('details')).toHaveAttribute('open');
    expect(within(card).getByText('isolated-agent-boot')).toBeInTheDocument();
    expect(within(card).getByText('包含未提交修改（构建自报）')).toBeInTheDocument();
    expect(within(card).getByText(/不是 Rust 采集程序或管理端的版本/)).toBeInTheDocument();
    state.statusFailure = 503;
    await userEvent.setup().click(screen.getByRole('button', { name: '刷新进程与作业' }));
    expect(await screen.findByText('代理版本为旧记录，仅供核对；本次未取得最新状态。')).toBeInTheDocument();
    expect(within(card).getByText('0.6.1-test')).toBeInTheDocument();
    expect(calls.mock.calls.some(([, init]) => init.method === 'POST')).toBe(false);
  });
  it('does not invent an agent identity for legacy snapshots', async () => {
    server(); mount();
    expect(await screen.findByText(/此代理未报告运行版本/)).toBeInTheDocument();
    await waitFor(() => expect(screen.getByRole('button', { name: '启动下级' })).toBeEnabled());
  });
  it('replaces the agent boot identity only after a successful refresh', async () => {
    const { state } = server(); state.status.agent_identity = agentIdentityFixture;
    mount(); await screen.findByText('isolated-agent-boot');
    state.status.agent_identity = { ...agentIdentityFixture, boot_id: 'new-agent-boot' };
    await userEvent.setup().click(screen.getByRole('button', { name: '刷新进程与作业' }));
    expect(await screen.findByText('new-agent-boot')).toBeInTheDocument();
    expect(screen.queryByText('isolated-agent-boot')).not.toBeInTheDocument();
  });
  it.each(['ready', 'stopped', 'verifying', 'unknown', 'future_phase'])('fences active maintenance %s without blocking logs or history', async (phase) => {
    const { state, calls } = server();
    state.status.lifecycle_supported = false;
    state.status = { ...state.status, maintenance_enabled: true, maintenance_deployment_id: 'release-a', maintenance_phase: phase };
    mount();
    expect(await screen.findByText('发布维护未结束')).toBeInTheDocument();
    expect(screen.getByText('release-a')).toBeInTheDocument();
    for (const name of ['启动下级', '停止下级', '重启下级', '编辑守护绑定']) expect(screen.getByRole('button', { name })).toBeDisabled();
    await userEvent.setup().click(screen.getByRole('button', { name: '读取守护日志' }));
    expect(await screen.findByLabelText('守护日志内容')).toHaveTextContent('中文 stderr');
    expect(screen.getByRole('button', { name: '刷新操作历史' })).toBeEnabled();
    expect(calls.mock.calls.some(([, init]) => init.method === 'POST' || init.method === 'PUT')).toBe(false);
  });

  it('blocks ordinary stop/restart when maintenance is configured but idle', async () => {
    const { state } = server();
    state.status.lifecycle_supported = false;
    state.status = { ...state.status, state: 'running', desired_state: 'running', pid: 140, maintenance_enabled: true };
    mount();
    expect(await screen.findByText('已启用发布维护保护')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: '停止下级' })).toBeDisabled();
    expect(screen.getByRole('button', { name: '重启下级' })).toBeDisabled();
  });

  it('allows explicit start when configured but no maintenance is active', async () => {
    const { state, calls } = server(); state.status.maintenance_enabled = true;
    mount(); await confirmAction('启动');
    expect(calls.mock.calls.filter(([url]) => String(url).endsWith('/process/actions'))).toHaveLength(1);
  });

  it('retains stale maintenance information after refresh failure', async () => {
    const { state } = server();
    state.status.lifecycle_supported = false;
    state.status = { ...state.status, maintenance_deployment_id: 'release-a', maintenance_phase: 'stopped' };
    mount(); await screen.findByText('发布维护未结束');
    state.statusFailure = 503;
    await userEvent.setup().click(screen.getByRole('button', { name: '刷新进程与作业' }));
    expect(await screen.findByText('维护旧状态，仅供核对')).toBeInTheDocument();
    expect(screen.getByText('release-a')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: '启动下级' })).toBeDisabled();
  });

  it('blocks a confirmation opened before maintenance arrived even at the same revision', async () => {
    const { state, calls } = server(); const { client } = mount(); const user = userEvent.setup();
    await waitFor(() => expect(screen.getByRole('button', { name: '启动下级' })).toBeEnabled());
    await user.click(screen.getByRole('button', { name: '启动下级' }));
    await user.click(screen.getByRole('checkbox', { name: /我确认此操作影响/ }));
    act(() => client.setQueryData(processKeys.status({ ...target, bindingRevision: 4, instanceId: 'fixture-a' }), { ...state.status, maintenance_enabled: true, maintenance_deployment_id: 'release-a', maintenance_phase: 'ready' }));
    expect(await screen.findByText('维护保护已阻止此动作，请取消并核对发布状态。')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: '确认启动进程' })).toBeDisabled();
    expect(calls.mock.calls.some(([url]) => String(url).endsWith('/process/actions'))).toBe(false);
  });

  it('retains configuration drafts but blocks saving while maintenance is active', async () => {
    const { state, calls } = server(); const view = mount({ drawer: true }); const user = userEvent.setup();
    await user.click(await screen.findByRole('tab', { name: '配置' }));
    const editor = await screen.findByLabelText('配置内容 calc.toml');
    await user.type(editor, '\n# preserve draft');
    await user.click(screen.getByRole('button', { name: '校验当前草稿' }));
    await waitFor(() => expect(screen.getByRole('button', { name: '保存到下级' })).toBeEnabled());
    act(() => view.client.setQueryData(processKeys.status({ ...target, bindingRevision: 4, instanceId: 'fixture-a' }), { ...state.status, maintenance_deployment_id: 'release-a', maintenance_phase: 'ready' }));
    expect(await screen.findByText(/发布维护尚未结束，采集控制和配置保存暂时锁定/)).toBeInTheDocument();
    expect(editor).toHaveValue('# original\n# preserve draft');
    expect(screen.getByRole('button', { name: '保存到下级' })).toBeDisabled();
    expect(calls.mock.calls.some(([, init]) => init.method === 'PUT')).toBe(false);
  });

  it('rechecks maintenance after the asynchronous configuration precondition', async () => {
    const { state, calls } = server(); const gate = deferredAction();
    const configRead = vi.spyOn(operationsApi, 'config').mockImplementationOnce(async () => { await gate.promise; return state.file; });
    try {
      const view = mount({ connected: true, intent: { name: 'calc.toml', revision: originalHash } });
      await confirmAction('启动');
      await waitFor(() => expect(configRead).toHaveBeenCalledOnce());
      act(() => view.client.setQueryData(processKeys.status({ ...target, bindingRevision: 4, instanceId: 'fixture-a' }), { ...state.status, maintenance_deployment_id: 'release-a', maintenance_phase: 'ready' }));
      await act(async () => { gate.release(); await gate.promise; });
      await waitFor(() => expect(screen.getAllByText(/代理状态或维护保护已变化，未发送进程动作/).length).toBeGreaterThan(0));
      expect(calls.mock.calls.some(([url]) => String(url).endsWith('/process/actions'))).toBe(false);
      expect(window.sessionStorage.length).toBe(0);
    } finally { gate.release(); configRead.mockRestore(); }
  });
  it('can start through the independent process tab with OPC offline, disabled and unsupported', async () => {
    const { state, calls } = server(); state.supported = false;
    mount({ drawer: true, source: { ...source, enabled: false, connectionState: 'Disconnected' } });
    expect(await screen.findByText('此下级未提供运维能力')).toBeInTheDocument();
    await userEvent.setup().click(screen.getByRole('tab', { name: '进程' }));
    await screen.findByRole('heading', { name: '下级进程' });
    await confirmAction('启动');
    expect(await screen.findByText('已接受，等待执行', { selector: 'strong' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: '启动下级' })).toBeDisabled();
    const actions = calls.mock.calls.filter(([url]) => String(url).endsWith('/process/actions'));
    expect(actions).toHaveLength(1);
    expect(actions[0][1].headers).toMatchObject({ 'If-Match': '"agent-r1"' });
    expect(JSON.parse(String(actions[0][1].body))).toMatchObject({ source_revision: 2, binding_revision: 4, action: 'start' });
    expect(calls.mock.calls.some(([url]) => /restart|rediscover/.test(String(url)))).toBe(false);
  });

  it('requires explicit binding save and leaves unbound process/status/log endpoints untouched', async () => {
    const { state, calls } = server(); state.binding = { ...state.binding, revision: 0, agent_url: '', instance_id: '' };
    mount(); const user = userEvent.setup();
    expect(await screen.findByText('尚未绑定进程守护代理')).toBeInTheDocument();
    expect(screen.getByPlaceholderText('http://lower-host:8090')).toBeInTheDocument();
    await user.type(screen.getByLabelText('守护代理 URL'), 'http://fixture-agent:4851');
    await user.type(screen.getByLabelText('固定实例 ID'), 'explicit-instance');
    expect(calls.mock.calls.every(([, init]) => init.method !== 'PUT')).toBe(true);
    expect(calls.mock.calls.some(([url]) => /\/process\/(status|log)(\?|$)/.test(String(url)))).toBe(false);
    await user.click(screen.getByRole('button', { name: '保存守护绑定' }));
    expect(await screen.findByText('守护绑定已保存。本次没有启动、停止或重启任何进程。')).toBeInTheDocument();
    const [, init] = calls.mock.calls.find(([, input]) => input.method === 'PUT')!;
    expect(init.headers).toMatchObject({ 'If-Match': '"0"' });
    expect(JSON.parse(String(init.body))).toEqual({ source_revision: 2, agent_url: 'http://fixture-agent:4851', instance_id: 'explicit-instance' });
    expect(calls.mock.calls.some(([url]) => String(url).endsWith('/actions'))).toBe(false);
  });

  it('does not call an unreachable agent stopped and still permits log/history reads', async () => {
    const { state, calls } = server(); state.statusFailure = 503; mount(); const user = userEvent.setup();
    expect(await screen.findByText('代理状态未确认，不能判断进程已停止')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: '启动下级' })).toBeDisabled();
    expect(screen.queryByText('已停止')).not.toBeInTheDocument();
    expect(calls.mock.calls.some(([url]) => String(url).includes('/process/log?'))).toBe(false);
    await user.click(screen.getByRole('button', { name: '读取守护日志' }));
    expect(await screen.findByLabelText('守护日志内容')).toHaveTextContent('中文 stderr');
    expect(screen.getByRole('button', { name: '刷新操作历史' })).toBeEnabled();
    expect(calls.mock.calls.filter(([url]) => String(url).includes('/process/log?'))).toHaveLength(1);
  });

  it('labels restart attempts separately from the automatic restart policy', async () => {
    const { state } = server(); state.status = { ...state.status, restart_policy: 'never', restart_count: 3 };
    mount();
    expect(await screen.findByText('不自动拉起', { exact: true })).toBeInTheDocument();
    expect(screen.getByText('累计重启尝试（含手动）').nextElementSibling).toHaveTextContent('3 次');
    expect(screen.getByText('守护自动拉起策略').nextElementSibling).not.toHaveTextContent('次');
  });

  it('retains a binding draft during refresh/409 and freezes old target writes', async () => {
    const { state, calls } = server(); const view = mount(); const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: '编辑守护绑定' }));
    const input = screen.getByLabelText('守护代理 URL');
    await user.clear(input); await user.type(input, 'http://my-draft:4851');
    state.bindingFailure = 409;
    await user.click(screen.getByRole('button', { name: '保存守护绑定' }));
    expect(await screen.findByText(/绑定草稿已保留/)).toBeInTheDocument();
    expect(input).toHaveValue('http://my-draft:4851');
    state.binding = { ...state.binding, revision: 5, agent_url: 'http://changed-elsewhere:4851' };
    await act(async () => { await view.client.invalidateQueries({ queryKey: processKeys.binding(target) }); });
    expect(await screen.findByText(/绑定已被其他操作修改/)).toBeInTheDocument();
    expect(input).toHaveValue('http://my-draft:4851');
    expect(screen.getByRole('button', { name: '保存守护绑定' })).toBeDisabled();
    view.lock();
    expect(screen.getByRole('button', { name: '启动下级' })).toBeDisabled();
    expect(calls.mock.calls.filter(([, init]) => init.method === 'PUT')).toHaveLength(1);
  });

  it('does not send a stale confirmation or a cancelled stop', async () => {
    const { state, calls } = server(); state.status = { ...state.status, state: 'running', desired_state: 'running', pid: 30 };
    const view = mount(); const user = userEvent.setup();
    await waitFor(() => expect(screen.getByRole('button', { name: '停止下级' })).toBeEnabled());
    await user.click(screen.getByRole('button', { name: '停止下级' }));
    const dialog = screen.getByRole('dialog', { name: '确认停止下级进程' });
    await user.click(within(dialog).getByRole('checkbox'));
    state.status = { ...state.status, revision: 'changed-agent-revision' };
    await act(async () => { await view.client.invalidateQueries({ queryKey: processKeys.source(target) }); });
    expect(await within(dialog).findByText(/代理状态版本已变化/)).toBeInTheDocument();
    expect(within(dialog).getByRole('button', { name: '确认停止进程' })).toBeDisabled();
    await user.click(within(dialog).getByRole('button', { name: '取消' }));
    expect(calls.mock.calls.some(([url]) => String(url).endsWith('/actions'))).toBe(false);
  });

  it('waits for the same accepted ID to reach terminal and keeps pending readable', async () => {
    const { calls, finish } = server(); const view = mount(); await screen.findByRole('heading', { name: '下级进程' });
    await confirmAction('启动');
    expect(await screen.findByText('已接受，等待执行', { selector: 'strong' })).toBeInTheDocument();
    const first = calls.mock.calls.find(([url]) => String(url).endsWith('/actions'))!;
    const id = JSON.parse(String(first[1].body)).request_id as string;
    expect(view.onControlPendingChange).toHaveBeenLastCalledWith(true);
    const user = userEvent.setup();
    await user.click(screen.getByRole('button', { name: '读取守护日志' }));
    await screen.findByLabelText('守护日志内容');
    finish();
    await user.click(screen.getByRole('button', { name: '刷新进程与作业' }));
    expect(await screen.findByText('已完成', { selector: 'strong' })).toBeInTheDocument();
    await waitFor(() => expect(screen.getByRole('button', { name: '停止下级' })).toBeEnabled());
    expect(view.onControlPendingChange).toHaveBeenLastCalledWith(false);
    expect(calls.mock.calls.filter(([url]) => String(url).endsWith('/actions'))).toHaveLength(1);
    expect(calls.mock.calls.some(([url]) => String(url).endsWith(`/operations/${id}`))).toBe(true);
  });

  it.each(['启动', '停止', '重启'] as const)('does not query or resolve %s before its first POST settles, including explicit refresh', async (label) => {
    const { state, calls, finish } = server();
    if (label !== '启动') state.status = { ...state.status, state: 'running', desired_state: 'running', pid: 30 };
    const admission = deferredAction(); state.actionGate = admission.promise;
    const view = mount(); const user = userEvent.setup();
    await screen.findByRole('heading', { name: '下级进程' });
    await confirmAction(label);
    const first = calls.mock.calls.find(([url]) => String(url).endsWith('/actions'))!;
    const id = JSON.parse(String(first[1].body)).request_id as string;
    const jobReads = () => calls.mock.calls.filter(([url]) => String(url).endsWith(`/operations/${id}`));
    const resolves = () => calls.mock.calls.filter(([url]) => String(url).endsWith('/resolve'));
    try {
      expect(await screen.findByText('正在提交，等待受理', { selector: 'strong' })).toBeInTheDocument();
      expect(screen.getByText('正在等待首次提交响应；此时不会查询或封存该请求。')).toBeInTheDocument();
      expect(view.onControlPendingChange).toHaveBeenLastCalledWith(true);
      expect(state.job).toBeUndefined();
      expect(jobReads()).toHaveLength(0); expect(resolves()).toHaveLength(0);

      // SSE-like invalidation and manual refresh may read state/history/logs,
      // but neither may bypass the pending POST's admission gate.
      await act(async () => { await view.client.invalidateQueries({ queryKey: processKeys.source(target) }); });
      const statusReads = calls.mock.calls.filter(([url]) => String(url).endsWith('/process/status')).length;
      await user.click(screen.getByRole('button', { name: '刷新进程与作业' }));
      await waitFor(() => expect(calls.mock.calls.filter(([url]) => String(url).endsWith('/process/status')).length).toBeGreaterThan(statusReads));
      await user.click(screen.getByRole('button', { name: '读取守护日志' }));
      expect(await screen.findByLabelText('守护日志内容')).toHaveTextContent('中文 stderr');
      expect(jobReads()).toHaveLength(0); expect(resolves()).toHaveLength(0);
      expect(calls.mock.calls.filter(([url]) => String(url).endsWith('/actions'))).toHaveLength(1);
    } finally { await act(async () => { admission.release(); }); }

    expect(await screen.findByText('已接受，等待执行', { selector: 'strong' })).toBeInTheDocument();
    await waitFor(() => expect(jobReads().length).toBeGreaterThan(0));
    expect(resolves()).toHaveLength(0);
    finish();
    await user.click(screen.getByRole('button', { name: '刷新进程与作业' }));
    expect(await screen.findByText('已完成', { selector: 'strong' })).toBeInTheDocument();
    expect(calls.mock.calls.filter(([url]) => String(url).endsWith('/actions'))).toHaveLength(1);
  });

  it('does not query or seal a first POST that finishes with a known revision rejection', async () => {
    const { state, calls } = server(); state.actionFailure = 409; state.actionError = 'conflict';
    const admission = deferredAction(); state.actionGate = admission.promise;
    mount(); await screen.findByRole('heading', { name: '下级进程' }); await confirmAction('启动');
    try {
      expect(await screen.findByText('正在提交，等待受理', { selector: 'strong' })).toBeInTheDocument();
      expect(calls.mock.calls.some(([url]) => /\/process\/operations\/[^/?]+/.test(String(url)))).toBe(false);
    } finally { await act(async () => { admission.release(); }); }
    await waitFor(() => expect(screen.getByRole('button', { name: '启动下级' })).toBeEnabled());
    expect(calls.mock.calls.some(([url]) => /\/process\/operations\/[^/?]+/.test(String(url)))).toBe(false);
    expect(calls.mock.calls.filter(([url]) => String(url).endsWith('/actions'))).toHaveLength(1);
    expect(window.sessionStorage.length).toBe(0);
  });

  it('seals a missing unknown request once with its original fingerprint instead of retrying the action', async () => {
    const { state, calls } = server(); state.actionFailure = 504;
    const admission = deferredAction(); state.actionGate = admission.promise;
    mount(); await screen.findByRole('heading', { name: '下级进程' });
    await confirmAction('启动');
    try {
      expect(await screen.findByText('正在提交，等待受理', { selector: 'strong' })).toBeInTheDocument();
      expect(calls.mock.calls.some(([url]) => /\/process\/operations\/[^/?]+/.test(String(url)))).toBe(false);
    } finally { await act(async () => { admission.release(); }); }
    expect(await screen.findByText(/本次未确认作业已封存/)).toBeInTheDocument();
    await waitFor(() => expect(screen.getByRole('button', { name: '启动下级' })).toBeEnabled());
    const actions = calls.mock.calls.filter(([url]) => String(url).endsWith('/actions'));
    const resolves = calls.mock.calls.filter(([url]) => String(url).endsWith('/resolve'));
    expect(actions).toHaveLength(1); expect(resolves).toHaveLength(1);
    expect(resolves[0][1].body).toBe(actions[0][1].body);
    expect(resolves[0][1].headers['If-Match']).toBe(actions[0][1].headers['If-Match']);
  });

  it('does not unlock unknown/202 or silently lose its ID when the tab is reopened', async () => {
    const { state, calls } = server(); state.actionFailure = 504; state.resolveUnknown = true;
    const view = mount(); await screen.findByRole('heading', { name: '下级进程' });
    await confirmAction('启动');
    expect(await screen.findByText('结果未知，待对账', { selector: 'strong' })).toBeInTheDocument();
    const request = JSON.parse(String(calls.mock.calls.find(([url]) => String(url).endsWith('/actions'))![1].body));
    const readsBeforeReopen = calls.mock.calls.filter(([url]) => String(url).endsWith(`/operations/${request.request_id}`)).length;
    view.unmount(); mount();
    expect(await screen.findByText(request.request_id, { selector: '.operations-process-job code' })).toBeInTheDocument();
    await waitFor(() => expect(calls.mock.calls.filter(([url]) => String(url).endsWith(`/operations/${request.request_id}`)).length).toBeGreaterThan(readsBeforeReopen));
    expect(screen.getByRole('button', { name: '启动下级' })).toBeDisabled();
    await userEvent.setup().click(screen.getByRole('button', { name: '刷新进程与作业' }));
    expect(calls.mock.calls.filter(([url]) => String(url).endsWith('/actions'))).toHaveLength(1);
    expect(calls.mock.calls.filter(([url]) => String(url).endsWith('/resolve'))).toHaveLength(1);
  });

  it('retains the original ID when a successful HTTP response has a mismatched binding identity', async () => {
    const { state, calls } = server(); state.wrongResponseIdentity = true;
    mount(); await screen.findByRole('heading', { name: '下级进程' });
    await confirmAction('启动');
    expect(await screen.findByText(/守护代理绑定已变化/)).toBeInTheDocument();
    const body = JSON.parse(String(calls.mock.calls.find(([url]) => String(url).endsWith('/actions'))![1].body));
    expect(await screen.findByText(body.request_id, { selector: '.operations-process-job code' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: '启动下级' })).toBeDisabled();
    expect(calls.mock.calls.filter(([url]) => String(url).endsWith('/actions'))).toHaveLength(1);
  });

  it('keeps binding drafts and the source revision guard when offline in a drawer', async () => {
    const { calls } = server(); mount({ drawer: true, source: { ...source, connectionState: 'Disconnected' } }); const user = userEvent.setup();
    await user.click(await screen.findByRole('tab', { name: '进程' }));
    await user.click(await screen.findByRole('button', { name: '编辑守护绑定' }));
    await user.type(screen.getByLabelText('固定实例 ID'), '-draft');
    expect(screen.getByRole('tab', { name: '配置' })).toBeDisabled();
    await user.click(screen.getByRole('button', { name: '关闭详情' }));
    const dialog = screen.getByRole('dialog', { name: '放弃未保存的配置草稿？' });
    await user.click(within(dialog).getByRole('button', { name: '保留草稿，继续编辑' }));
    expect(screen.getByLabelText('固定实例 ID')).toHaveValue('fixture-a-draft');
    expect(calls.mock.calls.every(([, init]) => init.method !== 'PUT')).toBe(true);
  });

  it('preserves save-only, then explicitly restarts with a file precondition and verifies active revision', async () => {
    const { state, calls, finish } = server(); state.status = { ...state.status, state: 'running', desired_state: 'running', pid: 30 };
    mount({ drawer: true }); const user = userEvent.setup();
    await user.click(await screen.findByRole('tab', { name: '配置' }));
    const editor = await screen.findByLabelText('配置内容 calc.toml');
    await user.type(editor, '\n# saved change');
    await user.click(screen.getByRole('button', { name: '校验当前草稿' }));
    await waitFor(() => expect(screen.getByRole('button', { name: '保存到下级' })).toBeEnabled());
    await user.click(screen.getByRole('button', { name: '保存到下级' }));
    expect(await screen.findByText(/本操作未重启程序/)).toBeInTheDocument();
    expect(calls.mock.calls.some(([url]) => String(url).endsWith('/actions'))).toBe(false);
    await waitFor(() => expect(screen.getByRole('button', { name: '前往进程页应用已保存配置' })).toBeEnabled());
    await user.click(screen.getByRole('button', { name: '前往进程页应用已保存配置' }));
    await screen.findByRole('heading', { name: '下级进程' });
    await confirmAction('重启');
    expect(await screen.findByText('已接受，等待执行', { selector: 'strong' })).toBeInTheDocument();
    const input = JSON.parse(String(calls.mock.calls.find(([url]) => String(url).endsWith('/actions'))![1].body));
    expect(input.expected_config_revisions).toEqual({ 'calc.toml': savedHash });
    state.file = { ...state.file, active_revision: savedHash, restart_required: false }; finish();
    await user.click(screen.getByRole('button', { name: '刷新进程与作业' }));
    expect(await screen.findByText('calc.toml 的已保存版本已由下级确认生效。')).toBeInTheDocument();
    expect(screen.getByText('已核对配置：calc.toml')).toBeInTheDocument();
    expect(screen.queryByText('待核对配置：calc.toml')).not.toBeInTheDocument();
  });

  it('does not apply an old configuration verification to a new file or saved hash', async () => {
    const { state, finish } = server();
    state.status = { ...state.status, state: 'running', desired_state: 'running', pid: 30 };
    state.file = { ...state.file, revision: savedHash, restart_required: true };
    const view = mount({ connected: true, intent: { name: 'calc.toml', revision: savedHash } });
    await screen.findByRole('heading', { name: '下级进程' }); await confirmAction('重启');
    expect(await screen.findByText('已接受，等待执行', { selector: 'strong' })).toBeInTheDocument();
    state.file = { ...state.file, active_revision: savedHash, restart_required: false }; finish();
    await userEvent.setup().click(screen.getByRole('button', { name: '刷新进程与作业' }));
    expect(await screen.findByText('已核对配置：calc.toml')).toBeInTheDocument();

    view.updateIntent({ name: 'other.toml', revision: savedHash });
    expect(await screen.findByText('待核对配置：other.toml')).toBeInTheDocument();
    expect(screen.queryByText(/^已核对配置：/)).not.toBeInTheDocument();
    view.updateIntent({ name: 'calc.toml', revision: 'c'.repeat(64) });
    expect(await screen.findByText('待核对配置：calc.toml')).toBeInTheDocument();
    expect(screen.queryByText(/^已核对配置：/)).not.toBeInTheDocument();
  });

  it('does not restart a changed file after the configuration handoff', async () => {
    const { state, calls } = server(); state.status = { ...state.status, state: 'running', desired_state: 'running', pid: 30 };
    mount({ connected: true, intent: { name: 'calc.toml', revision: savedHash } });
    await screen.findByRole('heading', { name: '下级进程' });
    await confirmAction('重启');
    expect((await screen.findAllByText(/所保存文件已再次变化，未发送进程动作/)).length).toBeGreaterThan(0);
    expect(calls.mock.calls.some(([url]) => String(url).endsWith('/actions'))).toBe(false);
  });

  it('does not restart on invalid validation or a failed save and retains the file draft', async () => {
    const { state, calls } = server(); state.valid = false; state.saveFailure = 409;
    mount({ drawer: true }); const user = userEvent.setup();
    await user.click(await screen.findByRole('tab', { name: '配置' }));
    const editor = await screen.findByLabelText('配置内容 calc.toml');
    await user.type(editor, '\n# my draft');
    await user.click(screen.getByRole('button', { name: '校验当前草稿' }));
    expect(await screen.findByText('当前草稿校验未通过')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: '保存到下级' })).toBeDisabled();
    state.valid = true;
    await user.click(screen.getByRole('button', { name: '校验当前草稿' }));
    await waitFor(() => expect(screen.getByRole('button', { name: '保存到下级' })).toBeEnabled());
    await user.click(screen.getByRole('button', { name: '保存到下级' }));
    expect(await screen.findByText(/保存冲突：草稿及原版本已保留/)).toBeInTheDocument();
    expect(editor).toHaveValue('# original\n# my draft');
    expect(screen.queryByRole('button', { name: '前往进程页应用已保存配置' })).not.toBeInTheDocument();
    expect(calls.mock.calls.some(([url]) => String(url).endsWith('/actions'))).toBe(false);
  });
});
