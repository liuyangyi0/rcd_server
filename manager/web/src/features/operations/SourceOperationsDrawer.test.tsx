import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter, Route, Routes, useNavigate } from 'react-router-dom';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { operationKeys, operationsApi, type AcquisitionSnapshot, type ConfigDocument, type ConfigForm, type OperationsCapabilities } from '../../api/operations';
import { queryKeys } from '../../api/queries';
import type { Source } from '../../api/types';
import SourcesPage from '../../pages/SourcesPage';
import SourceOperationsDrawer from './SourceOperationsDrawer';

const source: Source = { id: 'lower-1', name: '下级 1', groupId: 'group-1', endpoint: 'opc.tcp://lower:4840', securityPolicy: 'None', securityMode: 'None', hasPassword: false, enabled: true, connectionState: 'Connected', sessionState: 'Active', subscriptionState: 'Active', catalogState: 'Ready', reconnectCount: 0, monitoredCount: 1, goodPoints: 1, badPoints: 0, totalPoints: 1, revision: 2 };
const target = { id: source.id, revision: source.revision };
function json(body: unknown, status = 200) { return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } }); }

function server() {
  const capabilities: OperationsCapabilities = { source_id: source.id, source_revision: 2, supported: true, schema_version: 1, features: ['acquisition', 'configuration', 'logs'], max_config_bytes: 2_097_152, max_log_bytes: 65_536, configuration_apply: 'restart' };
  const acquisition: AcquisitionSnapshot = { source_id: source.id, source_revision: 2, schema_version: 1, revision: 'lower-r1', ports: [{ port_id: '/dev/ttyS0', port_name: 'COM-A', enabled: true, applied_enabled: true, open: true, pending_commands: 0, control_pending: false, devices: [{ device_id: '1', name: '站点一', read_enabled: true, applied_read_enabled: true, communication_ok: true, last_data_at: '2026-08-28T01:00:00Z' }] }] };
  const document: ConfigDocument = { source_id: source.id, source_revision: 2, name: 'serial.toml', kind: 'serial', revision: 'file-1', active_revision: 'file-1', restart_required: false, size_bytes: 14, content: 'enabled = true', backups: [{ id: 'backup-1', created_at: '2026-08-28T01:00:00Z', size_bytes: 15 }] };
  const state = { capabilities, acquisition, document, sources: [source], sourceRemoved: false, saveFailure: 0, saveFailureCode: 'conflict', controlFailure: 0, controlFailureCode: 'timeout', valid: true, warnings: [] as string[], logGeneration: 'g1', logText: '[INFO] old line', emptyLogs: false };
  const fetchMock = vi.fn().mockImplementation((raw: string, init?: RequestInit) => {
    const url = new URL(raw, 'http://localhost');
    const id = decodeURIComponent(url.pathname.match(/sources\/([^/]+)\/(?:operations|process)/)?.[1] || source.id);
    const body = init?.body ? JSON.parse(String(init.body)) as Record<string, unknown> : {};
    if (url.pathname === '/api/v1/sources') return Promise.resolve(json({ items: state.sources }));
    if (url.pathname === '/api/v1/groups') return Promise.resolve(json({ items: [{ id: source.groupId, name: source.groupId }] }));
    if (url.pathname.endsWith('/process/binding')) return Promise.resolve(json({ source_id: id, source_revision: 2, revision: 0, agent_url: '', instance_id: '' }));
    if (state.sourceRemoved && id === source.id && url.pathname.includes('/operations/')) return Promise.resolve(json({ error: { code: 'not_found', message: 'source deleted' } }, 404));
    if (url.pathname.endsWith('/capabilities')) return Promise.resolve(json({ ...state.capabilities, source_id: id }));
    if (url.pathname.endsWith('/status')) return Promise.resolve(json({ ...state.acquisition, source_id: id }));
    if (url.pathname.endsWith('/port') || url.pathname.endsWith('/device-read')) {
      if (state.controlFailure) return Promise.resolve(json({ error: { code: state.controlFailureCode, message: '下级控制尚未完成' } }, state.controlFailure));
      state.acquisition = { ...state.acquisition, revision: 'lower-r2', ports: state.acquisition.ports.map((port) => url.pathname.endsWith('/port') ? { ...port, enabled: Boolean(body.enabled), applied_enabled: Boolean(body.enabled), open: Boolean(body.enabled) } : { ...port, devices: port.devices.map((device) => ({ ...device, read_enabled: Boolean(body.enabled), applied_read_enabled: Boolean(body.enabled) })) }) };
      return Promise.resolve(json({ ...state.acquisition, status: 'Applied' }));
    }
    if (url.pathname.endsWith('/configs')) return Promise.resolve(json({ source_id: id, source_revision: 2, items: [{ ...state.document, content: undefined, backups: undefined }] }));
    if (url.pathname.endsWith('/config/validate')) return Promise.resolve(json({ valid: state.valid, errors: state.valid ? [] : ['配置语法错误'], warnings: state.warnings, restart_required: true }));
    if (url.pathname.endsWith('/config') && init?.method === 'PUT') {
      if (state.saveFailure) {
        if (state.saveFailure === 409 && state.saveFailureCode === 'conflict') state.document = { ...state.document, revision: 'file-other', content: 'changed elsewhere' };
        return Promise.resolve(json({ error: { code: state.saveFailureCode, message: 'file operation rejected' } }, state.saveFailure));
      }
      state.document = { ...state.document, content: String(body.content), revision: 'file-2', restart_required: true };
      return Promise.resolve(json(state.document));
    }
    if (url.pathname.endsWith('/config')) return Promise.resolve(json({ ...state.document, source_id: id, content: id !== source.id ? 'other source content' : url.searchParams.has('backup_id') ? 'enabled = false' : state.document.content, ...(url.searchParams.has('backup_id') ? { viewing_backup_id: 'backup-1', backup_revision: 'archive-hash' } : {}) }));
    if (url.pathname.endsWith('/logs')) return Promise.resolve(json({ source_id: id, source_revision: 2, items: state.emptyLogs ? [] : [{ name: 'service.log', size_bytes: 200, modified_at: '2026-08-28T01:00:00Z', generation: state.logGeneration }] }));
    if (url.pathname.endsWith('/log')) {
      if (url.searchParams.get('generation') !== state.logGeneration) return Promise.resolve(json({ error: { code: 'conflict', message: 'log rotated' } }, 409));
      return Promise.resolve(json({ source_id: id, source_revision: 2, name: 'service.log', generation: state.logGeneration, offset: url.searchParams.get('tail') ? 100 : Number(url.searchParams.get('offset')), next_offset: 200, size_bytes: 200, eof: true, truncated: false, text: state.logText }));
    }
    return Promise.resolve(json({ items: [] }));
  });
  vi.stubGlobal('fetch', fetchMock);
  return { state, fetchMock };
}

function mount(initial = source) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  const onClose = vi.fn();
  const content = (value: Source) => <QueryClientProvider client={client}><SourceOperationsDrawer source={value} onClose={onClose} /></QueryClientProvider>;
  const view = render(content(initial));
  return { ...view, client, onClose, replaceSource: (value: Source) => view.rerender(content(value)) };
}
async function configuration() {
  await userEvent.setup().click(await screen.findByRole('tab', { name: '配置' }));
  return screen.findByLabelText('配置内容 serial.toml');
}
afterEach(() => { vi.unstubAllGlobals(); vi.restoreAllMocks(); vi.useRealTimers(); });

function RouteHarness() {
  const navigate = useNavigate();
  return <><button onClick={() => navigate('/away')}>离开来源页面</button><button onClick={() => navigate('/')}>返回来源页面</button><Routes><Route path="/" element={<SourceOperationsDrawer source={source} onClose={() => undefined} />} /><Route path="/away" element={<p>其他页面</p>} /></Routes></>;
}

describe('physical lower-source operations drawer', () => {
  it('restores the original config baseline and draft after an SPA route unmount', async () => {
    const { state, fetchMock } = server();
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    render(<QueryClientProvider client={client}><MemoryRouter><RouteHarness /></MemoryRouter></QueryClientProvider>);
    const editor = await configuration(); const user = userEvent.setup();
    await user.clear(editor); await user.type(editor, 'draft retained across browser navigation');
    await user.click(screen.getByRole('button', { name: '离开来源页面' }));
    expect(screen.queryByLabelText('配置内容 serial.toml')).not.toBeInTheDocument();
    state.document = { ...state.document, revision: 'file-new', content: 'changed by another user' };
    client.setQueryData(operationKeys.config(target, 'serial.toml'), state.document);
    await user.click(screen.getByRole('button', { name: '返回来源页面' }));
    expect(await screen.findByLabelText('配置内容 serial.toml')).toHaveValue('draft retained across browser navigation');
    expect(screen.getByText(/已恢复本次网页登录期间离开页面前的未保存草稿和原版本/)).toBeInTheDocument();
    expect(screen.getByText(/下级文件已被其他操作修改/)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: '保存到下级' })).toBeDisabled();
    expect(fetchMock.mock.calls.filter(([, init]) => init?.method === 'PUT')).toHaveLength(0);
    client.clear();
  });

  it('does not disable or blur raw typing while parameter definitions are slow', async () => {
    const { state } = server();
    state.document = { ...state.document, name: 'config.toml' };
    state.capabilities.features.push('configuration_forms');
    const prepare = vi.spyOn(operationsApi, 'prepare').mockImplementation(() => new Promise<ConfigForm>(() => {}));
    const view = mount(); const user = userEvent.setup();
    await user.click(await screen.findByRole('tab', { name: '配置' }));
    await user.click(await screen.findByText('配置原文（高级编辑；点表 CSV 也在此编辑）'));
    const editor = screen.getByLabelText('配置内容 config.toml');
    expect(editor).toBeEnabled();
    await user.clear(editor); await user.type(editor, 'continuous raw configuration edit');
    await waitFor(() => expect(prepare).toHaveBeenCalledTimes(1));
    expect(editor).toHaveValue('continuous raw configuration edit');
    expect(editor).toHaveFocus(); expect(editor).toBeEnabled();
    expect(prepare.mock.calls[0][2]).toBe('continuous raw configuration edit');
    view.client.clear();
  });

  it('recovers ungenerated parameter edits after leaving and returning to the source', async () => {
    const { state } = server();
    state.document = { ...state.document, name: 'config.toml', content: 'command_retry_count=3' };
    state.capabilities.features.push('configuration_forms');
    vi.spyOn(operationsApi, 'prepare').mockResolvedValue({ source_id: source.id, source_revision: 2, content: state.document.content, fields: [{ path: ['command_retry_count'], key: 'command_retry_count', label: '命令下发次数', hint: '', kind: 'integer', value: 3, options: [], inherited: false, readonly: false }] });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    render(<QueryClientProvider client={client}><MemoryRouter><RouteHarness /></MemoryRouter></QueryClientProvider>);
    const user = userEvent.setup(); await user.click(await screen.findByRole('tab', { name: '配置' }));
    const input = await screen.findByLabelText('命令下发次数'); await user.clear(input); await user.type(input, '5');
    await user.click(screen.getByRole('button', { name: '离开来源页面' }));
    await user.click(screen.getByRole('button', { name: '返回来源页面' }));
    expect(await screen.findByLabelText('命令下发次数')).toHaveValue(5);
    expect(screen.getByRole('button', { name: '校验当前草稿' })).toBeEnabled();
    expect(screen.getByRole('button', { name: '保存到下级' })).toBeDisabled();
    client.clear();
  });
  it('sets the lower command interval through draft validation and CAS save without restarting', async () => {
    const { state, fetchMock } = server();
    state.document = { ...state.document, name: 'config.toml', content: '# preserved\n[settings]\ncommand_retry_count=3\n' };
    state.capabilities.features.push('configuration_forms');
    const prepare = vi.spyOn(operationsApi, 'prepare').mockImplementation(async (_target, _name, content, edits = []) => ({
      source_id: source.id, source_revision: 2,
      content: edits.length ? `${content}command_interval_ms = 500\n` : content,
      fields: [{ path: ['settings', 'command_interval_ms'], key: 'settings.command_interval_ms', label: '控制命令最小间隔(ms)', hint: '保存后重启下级生效，0..60000', kind: 'integer', value: edits.length ? 500 : 0, options: [], inherited: !edits.length, readonly: false }],
    }));
    const view = mount(); const user = userEvent.setup();
    await user.click(await screen.findByRole('tab', { name: '配置' }));
    const input = await screen.findByLabelText('控制命令最小间隔(ms)');
    expect(input).toHaveValue(0);
    await user.clear(input); await user.type(input, '500');
    expect(screen.getByRole('button', { name: '保存到下级' })).toBeDisabled();
    await user.click(screen.getByRole('button', { name: '校验当前草稿' }));
    await waitFor(() => expect(screen.getByLabelText('配置内容 config.toml')).toHaveValue('# preserved\n[settings]\ncommand_retry_count=3\ncommand_interval_ms = 500\n'));
    expect(prepare.mock.calls.find((call) => call[3]?.length)?.[3]).toEqual([{ path: ['settings', 'command_interval_ms'], value: 500 }]);
    expect(fetchMock.mock.calls.filter(([, init]) => init?.method === 'PUT')).toHaveLength(0);
    await waitFor(() => expect(screen.getByRole('button', { name: '保存到下级' })).toBeEnabled());
    await user.click(screen.getByRole('button', { name: '保存到下级' }));
    expect(await screen.findByText(/配置已保存到下级文件；需要手动重启下级后生效/)).toBeInTheDocument();
    const writes = fetchMock.mock.calls.filter(([, init]) => init?.method === 'PUT');
    expect(writes).toHaveLength(1);
    expect(new Headers(writes[0][1]?.headers).get('If-Match')).toBe('"file-1"');
    expect(JSON.parse(String(writes[0][1]?.body))).toMatchObject({ source_revision: 2, name: 'config.toml', content: state.document.content });
    expect(fetchMock.mock.calls.some(([url]) => /process\/.*(?:restart|control)/.test(String(url)))).toBe(false);
    view.client.clear();
  });
  it('allows saving with point warnings but blocks application configuration errors', async () => {
    const { state, fetchMock } = server(); mount();
    const editor = await configuration(); const user = userEvent.setup();
    state.warnings = ['rcd_example.csv: 第 83 行 EXAMPLE_AI4: byte_index=20 超出状态数据长度 1..=19'];
    await user.clear(editor); await user.type(editor, 'changed config');
    await user.click(screen.getByRole('button', { name: '校验当前草稿' }));
    expect(await screen.findByText('配置可以保存，以下提示不阻止保存')).toBeInTheDocument();
    expect(screen.getByText(state.warnings[0])).toBeInTheDocument();
    expect(screen.getByRole('button', { name: '保存到下级' })).toBeEnabled();
    await user.click(screen.getByRole('button', { name: '保存到下级' }));
    await screen.findByText(/配置已保存到下级文件/);
    state.valid = false;
    await user.type(editor, ' invalid');
    await user.click(screen.getByRole('button', { name: '校验当前草稿' }));
    expect(await screen.findByText('配置语法错误')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: '保存到下级' })).toBeDisabled();
    expect(fetchMock.mock.calls.filter(([, init]) => init?.method === 'PUT')).toHaveLength(1);
  });

  it('retains dirty content until a backup replacement is explicitly confirmed', async () => {
    const { fetchMock } = server(); mount(); const editor = await configuration(); const user = userEvent.setup();
    await user.clear(editor); await user.type(editor, 'keep unsaved draft');
    await user.click(screen.getByText('导入、下载与备份（可选）'));
    await user.selectOptions(screen.getByLabelText('自动备份版本'), 'backup-1');
    await user.click(screen.getByRole('button', { name: '载入备份为草稿' }));
    const confirm = await screen.findByRole('button', { name: '确认替换草稿' });
    expect(editor).toHaveValue('keep unsaved draft');
    await user.click(confirm);
    expect(editor).toHaveValue('enabled = false');
    expect(screen.getByRole('button', { name: '保存到下级' })).toBeDisabled();
    expect(fetchMock.mock.calls.filter(([, init]) => init?.method === 'PUT')).toHaveLength(0);
  });

  it('does not validate or save when parameter preparation fails', async () => {
    const { state, fetchMock } = server();
    state.document = { ...state.document, name: 'config.toml' };
    state.capabilities.features.push('configuration_forms');
    const prepare = vi.spyOn(operationsApi, 'prepare').mockResolvedValue({ source_id: source.id, source_revision: 2, content: state.document.content, fields: [{ path: ['settings', 'command_interval_ms'], key: 'settings.command_interval_ms', label: '命令间隔', hint: '', kind: 'integer', value: 0, options: [], inherited: true, readonly: false }] });
    mount(); const user = userEvent.setup();
    await user.click(await screen.findByRole('tab', { name: '配置' }));
    await user.type(await screen.findByLabelText('命令间隔'), '500');
    prepare.mockRejectedValue(new Error('prepare offline'));
    await user.click(screen.getByRole('button', { name: '校验当前草稿' }));
    await screen.findByText(/校验未完成/);
    expect(screen.getByLabelText('命令间隔')).toHaveValue(500);
    expect(screen.getByRole('button', { name: '保存到下级' })).toBeDisabled();
    expect(fetchMock.mock.calls.some(([url, init]) => String(url).endsWith('/config/validate') || init?.method === 'PUT')).toBe(false);
  });

  it('opens configuration-only lowers without requesting unsupported acquisition status', async () => {
    const { state, fetchMock } = server();
    state.capabilities.features = ['configuration'];
    mount();
    expect(await screen.findByLabelText('配置内容 serial.toml')).toBeInTheDocument();
    expect(screen.getByRole('tab', { name: '配置' })).toHaveAttribute('aria-selected', 'true');
    expect(fetchMock.mock.calls.some(([url]) => String(url).endsWith('/operations/status'))).toBe(false);
  });
  it('retains a dirty panel when the parent source list deletes its source, until explicit close', async () => {
    const { state, fetchMock } = server();
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    render(<QueryClientProvider client={client}><MemoryRouter><SourcesPage /></MemoryRouter></QueryClientProvider>);
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: '运维 lower-1' }));
    const editor = await configuration();
    await user.clear(editor);
    await user.type(editor, 'unsaved draft survives source deletion');
    await user.click(screen.getByRole('button', { name: '校验当前草稿' }));
    await waitFor(() => expect(screen.getByRole('button', { name: '保存到下级' })).toBeEnabled());

    state.sources = [];
    state.sourceRemoved = true;
    await act(async () => { await Promise.all([
      client.invalidateQueries({ queryKey: queryKeys.allSources }),
      client.invalidateQueries({ queryKey: operationKeys.source(target) }),
    ]); });
    expect(await screen.findByText(/此来源已从管理配置删除/)).toBeInTheDocument();
    expect(screen.getByRole('heading', { name: '下级运维 · lower-1' })).toBeInTheDocument();
    expect(editor).toHaveValue('unsaved draft survives source deletion');
    expect(screen.getByRole('button', { name: '保存到下级' })).toBeDisabled();
    expect(screen.getByRole('button', { name: '校验当前草稿' })).toBeDisabled();
    await user.click(screen.getByText('导入、下载与备份（可选）'));
    expect(screen.getByRole('button', { name: '下载草稿' })).toBeEnabled();
    expect(fetchMock.mock.calls.filter(([, init]) => init?.method === 'PUT')).toHaveLength(0);

    // Recreating an identical ID cannot resurrect authority for the old panel.
    state.sources = [source];
    state.sourceRemoved = false;
    await act(async () => { await Promise.all([
      client.invalidateQueries({ queryKey: queryKeys.allSources }),
      client.invalidateQueries({ queryKey: operationKeys.source(target) }),
    ]); });
    expect(await screen.findByRole('button', { name: '运维 lower-1' })).toBeInTheDocument();
    expect(screen.getByText(/此来源已从管理配置删除/)).toBeInTheDocument();
    expect(editor).toHaveValue('unsaved draft survives source deletion');
    expect(screen.getByRole('button', { name: '保存到下级' })).toBeDisabled();
    await user.click(screen.getByRole('button', { name: '关闭详情' }));
    const dialog = screen.getByRole('dialog', { name: '放弃未保存的配置草稿？' });
    await user.click(within(dialog).getByRole('button', { name: '保留草稿，继续编辑' }));
    expect(editor).toHaveValue('unsaved draft survives source deletion');
    await user.click(screen.getByRole('button', { name: '关闭详情' }));
    await user.click(within(screen.getByRole('dialog', { name: '放弃未保存的配置草稿？' })).getByRole('button', { name: '放弃草稿并关闭' }));
    await waitFor(() => expect(screen.queryByRole('heading', { name: '下级运维 · lower-1' })).not.toBeInTheDocument());
    await user.click(screen.getByRole('button', { name: '运维 lower-1' }));
    const freshEditor = await configuration();
    expect(freshEditor).toHaveValue('enabled = true');
    expect(screen.queryByText(/此来源已从管理配置删除/)).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: '校验当前草稿' })).toBeEnabled();
  });

  it('explains a busy physical control without claiming a revision conflict or retrying it', async () => {
    const { state, fetchMock } = server(); state.controlFailure = 409; state.controlFailureCode = 'pending_command'; mount();
    await userEvent.setup().click(await screen.findByRole('button', { name: '停读 /dev/ttyS0 1' }));
    expect(await screen.findByText(/下级仍有待处理命令或运维操作/)).toBeInTheDocument();
    expect(screen.queryByText(/版本冲突/)).not.toBeInTheDocument();
    await waitFor(() => expect(fetchMock.mock.calls.filter(([url]) => String(url).endsWith('/status')).length).toBeGreaterThan(1));
    expect(fetchMock.mock.calls.filter(([, init]) => init?.method === 'PUT')).toHaveLength(1);
  });

  it('retains a file draft on busy without mislabeling the rejection as a stale version', async () => {
    const { state, fetchMock } = server(); state.saveFailure = 409; state.saveFailureCode = 'busy'; mount();
    const editor = await configuration(); const user = userEvent.setup();
    await user.clear(editor); await user.type(editor, 'desired file after pending operation');
    await user.click(screen.getByRole('button', { name: '校验当前草稿' }));
    await waitFor(() => expect(screen.getByRole('button', { name: '保存到下级' })).toBeEnabled());
    await user.click(screen.getByRole('button', { name: '保存到下级' }));
    expect(await screen.findByText(/下级仍有待处理命令或运维操作/)).toBeInTheDocument();
    expect(screen.queryByText(/保存冲突：/)).not.toBeInTheDocument();
    expect(editor).toHaveValue('desired file after pending operation');
    expect(state.document.revision).toBe('file-1');
    expect(fetchMock.mock.calls.filter(([, init]) => init?.method === 'PUT')).toHaveLength(1);
  });

  it('treats legacy capability absence as a normal empty state', async () => {
    const { state, fetchMock } = server(); state.capabilities.supported = false;
    mount();
    expect(await screen.findByText('此下级未提供运维能力')).toBeInTheDocument();
    expect(fetchMock.mock.calls).toHaveLength(2);
    expect(fetchMock.mock.calls.some(([url]) => String(url).endsWith('/capabilities'))).toBe(true);
    expect(fetchMock.mock.calls.some(([url]) => String(url).endsWith('/process/binding'))).toBe(true);
    expect(screen.getByRole('tab', { name: '进程' })).toBeEnabled();
  });

  it('requests only the active tab and never downloads the live point table', async () => {
    const { fetchMock } = server(); mount();
    await screen.findByRole('button', { name: '关闭串口 COM-A' });
    expect(fetchMock.mock.calls.every(([url]) => !/\/(configs|config|logs|log|points)(\?|$)/.test(String(url)))).toBe(true);
    await configuration();
    expect(fetchMock.mock.calls.some(([url]) => String(url).endsWith('/configs'))).toBe(true);
    expect(fetchMock.mock.calls.every(([url]) => !/\/(logs|log|points)(\?|$)/.test(String(url)))).toBe(true);
  });

  it('requires confirmation before releasing a port and uses the captured revisions', async () => {
    const { fetchMock } = server(); mount(); const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: '关闭串口 COM-A' }));
    const dialog = screen.getByRole('dialog', { name: '确认关闭物理串口' });
    const confirm = within(dialog).getByRole('button', { name: '确认关闭串口' });
    expect(confirm).toBeDisabled();
    expect(fetchMock.mock.calls.every(([, init]) => init?.method !== 'PUT')).toBe(true);
    await user.click(within(dialog).getByRole('checkbox', { name: /我确认停止该串口全部站/ }));
    await user.click(confirm);
    await screen.findByRole('button', { name: '启用串口 COM-A' });
    const [, init] = fetchMock.mock.calls.find(([url, options]) => String(url).endsWith('/port') && options?.method === 'PUT')! as [string, RequestInit];
    expect(init.headers).toMatchObject({ 'If-Match': '"lower-r1"' });
    expect(JSON.parse(String(init.body))).toEqual({ source_revision: 2, port_id: '/dev/ttyS0', enabled: false });
    expect(screen.getByText(/硬件是否就绪/)).toBeInTheDocument();
  });

  it('separates desired state from hardware and locks pending control/commands', async () => {
    const { state } = server(); Object.assign(state.acquisition.ports[0], { open: false, pending_commands: 2, control_pending: true, last_control_error: 'permission denied' });
    mount();
    expect(await screen.findByRole('button', { name: '关闭串口 COM-A' })).toBeDisabled();
    expect(screen.getByText('串口未打开')).toBeInTheDocument();
    expect(screen.getByText(/最近控制错误：permission denied/)).toBeInTheDocument();
    expect(screen.getByText(/待处理命令：2/)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: '停读 /dev/ttyS0 1' })).toBeDisabled();
  });

  it('does not retry an uncertain device control and refreshes its real status', async () => {
    const { state, fetchMock } = server(); state.controlFailure = 504; mount();
    await userEvent.setup().click(await screen.findByRole('button', { name: '停读 /dev/ttyS0 1' }));
    expect(await screen.findByText(/操作结果未确认，不会自动重试/)).toBeInTheDocument();
    await waitFor(() => expect(fetchMock.mock.calls.filter(([url]) => String(url).endsWith('/status')).length).toBeGreaterThan(1));
    expect(fetchMock.mock.calls.filter(([, init]) => init?.method === 'PUT')).toHaveLength(1);
    expect(screen.getByText(/站点停读只停止数据采集，不禁止控制命令/)).toBeInTheDocument();
  });

  it('preserves the draft when background file refresh finds a newer revision', async () => {
    const { state } = server(); const view = mount(); const editor = await configuration(); const user = userEvent.setup();
    await user.clear(editor); await user.type(editor, 'my unsaved content');
    state.document = { ...state.document, revision: 'file-other', content: 'external edit' };
    await act(async () => { await view.client.invalidateQueries({ queryKey: operationKeys.config(target, 'serial.toml') }); });
    expect(editor).toHaveValue('my unsaved content');
    expect(await screen.findByText(/下级文件已被其他操作修改/)).toBeInTheDocument();
    expect(screen.getByRole('tab', { name: '日志' })).toBeDisabled();
    expect(screen.getByRole('button', { name: '保存到下级' })).toBeDisabled();
  });

  it('retains the draft and original CAS revision on HTTP 409', async () => {
    const { state, fetchMock } = server(); state.saveFailure = 409; mount(); const editor = await configuration(); const user = userEvent.setup();
    await user.clear(editor); await user.type(editor, 'desired file');
    await user.click(screen.getByRole('button', { name: '校验当前草稿' }));
    await waitFor(() => expect(screen.getByRole('button', { name: '保存到下级' })).toBeEnabled());
    await user.click(screen.getByRole('button', { name: '保存到下级' }));
    expect(await screen.findByText(/保存冲突：草稿及原版本已保留/)).toBeInTheDocument();
    expect(editor).toHaveValue('desired file');
    const [, init] = fetchMock.mock.calls.find(([, options]) => options?.method === 'PUT')! as [string, RequestInit];
    expect(init.headers).toMatchObject({ 'If-Match': '"file-1"' });
    expect(fetchMock.mock.calls.filter(([, options]) => options?.method === 'PUT')).toHaveLength(1);
  });

  it('loads backups only into drafts and never sends their hash as If-Match', async () => {
    const { fetchMock } = server(); mount(); const editor = await configuration(); const user = userEvent.setup();
    await user.click(screen.getByText('导入、下载与备份（可选）'));
    await user.selectOptions(screen.getByLabelText('自动备份版本'), 'backup-1');
    await user.click(screen.getByRole('button', { name: '载入备份为草稿' }));
    await waitFor(() => expect(editor).toHaveValue('enabled = false'));
    expect(fetchMock.mock.calls.filter(([, options]) => options?.method === 'PUT')).toHaveLength(0);
    expect(screen.getByRole('button', { name: '保存到下级' })).toBeDisabled();
    await user.click(screen.getByRole('button', { name: '校验当前草稿' }));
    await waitFor(() => expect(screen.getByRole('button', { name: '保存到下级' })).toBeEnabled());
    await user.click(screen.getByRole('button', { name: '保存到下级' }));
    expect(await screen.findByText(/配置已保存到下级文件；需要手动重启/)).toBeInTheDocument();
    const [, init] = fetchMock.mock.calls.find(([, options]) => options?.method === 'PUT')! as [string, RequestInit];
    expect(init.headers).toMatchObject({ 'If-Match': '"file-1"' });
    expect(JSON.parse(String(init.body)).content).toBe('enabled = false');
    expect(fetchMock.mock.calls.every(([url]) => !/restart|reconnect|rediscover/.test(String(url)))).toBe(true);
  });

  it('locks an old source revision without erasing its draft and confirms closing', async () => {
    server(); const view = mount(); const editor = await configuration(); const user = userEvent.setup();
    await user.clear(editor); await user.type(editor, 'keep me');
    view.replaceSource({ ...source, revision: 3, endpoint: 'opc.tcp://changed:4840' });
    expect(editor).toHaveValue('keep me');
    expect(screen.getByText(/当前抽屉保留旧目标和草稿/)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: '校验当前草稿' })).toBeDisabled();
    await user.click(screen.getByRole('button', { name: '关闭详情' }));
    expect(screen.getByRole('dialog', { name: '放弃未保存的配置草稿？' })).toBeInTheDocument();
    expect(view.onClose).not.toHaveBeenCalled();
    await user.click(screen.getByRole('button', { name: '保留草稿，继续编辑' }));
    expect(editor).toHaveValue('keep me');
  });

  it('clears old-source draft state when the physical source changes', async () => {
    server(); const view = mount(); const editor = await configuration(); const user = userEvent.setup();
    await user.clear(editor); await user.type(editor, 'old source draft');
    view.replaceSource({ ...source, id: 'lower-2' });
    expect(screen.queryByDisplayValue('old source draft')).not.toBeInTheDocument();
    const nextEditor = await configuration();
    expect(nextEditor).toHaveValue('other source content');
  });

  it('stops on a rotated log generation and replaces rather than appends the next page', async () => {
    const { state, fetchMock } = server(); mount(); const user = userEvent.setup();
    await user.click(await screen.findByRole('tab', { name: '日志' }));
    expect(await screen.findByLabelText('日志内容')).toHaveTextContent('old line');
    state.logGeneration = 'g2'; state.logText = '[INFO] new generation';
    await user.click(screen.getByRole('button', { name: '刷新当前页' }));
    expect(await screen.findByText('日志已轮转或代次冲突。')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: '下一页' })).toBeDisabled();
    await user.click(screen.getByRole('button', { name: '重新从尾部读取' }));
    await waitFor(() => expect(screen.getByLabelText('日志内容')).toHaveTextContent('new generation'));
    expect(screen.getByLabelText('日志内容')).not.toHaveTextContent('old line');
    const logCalls = fetchMock.mock.calls.filter(([url]) => String(url).includes('/log?'));
    expect(logCalls.every(([url]) => Number(new URL(String(url), 'http://localhost').searchParams.get('limit')) <= 65_536)).toBe(true);
    expect(String(logCalls.at(-1)?.[0])).toContain('generation=g2');
  });

  it('shows offline and empty-log states without inventing device connectivity', async () => {
    const { state } = server(); state.emptyLogs = true; mount({ ...source, connectionState: 'Disconnected' });
    expect(screen.getByText(/下级 OPC 当前未连接/)).toBeInTheDocument();
    expect(await screen.findByRole('button', { name: '关闭串口 COM-A' })).toBeDisabled();
    await userEvent.setup().click(screen.getByRole('tab', { name: '日志' }));
    expect(await screen.findByText('暂无日志文件')).toBeInTheDocument();
  });
});
