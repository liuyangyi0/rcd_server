import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, expect, it, vi } from 'vitest';
import * as client from '../../api/client';
import { calculationApi } from '../../api/calculations';
import StateInputsTab, { bindingOnly, copyStateName } from './StateInputsTab';

afterEach(() => vi.restoreAllMocks());

it('removes runtime fields from persisted bindings', () => {
 const value = { name: 'STATE', mode: 'fixed' as const, group_id: 'unused', targets: [{ source_id: 'a', port_id: 'COM1', device_id: '11' }], value: 5, status_code: 0 };
 expect(bindingOnly(value)).toEqual({ name: 'STATE', mode: 'fixed', targets: value.targets });
});

it('copies variable names on intranet HTTP and restores focus', async () => {
 const original = Object.getOwnPropertyDescriptor(document, 'execCommand');
 const clipboard = Object.getOwnPropertyDescriptor(navigator, 'clipboard');
 const button = document.createElement('button'); document.body.appendChild(button); button.focus();
 const copy = vi.fn(() => true);
 Object.defineProperty(document, 'execCommand', { configurable: true, value: copy });
 Object.defineProperty(navigator, 'clipboard', { configurable: true, value: undefined });
 try {
  await copyStateName('STATE'); expect(copy).toHaveBeenCalledWith('copy');
  expect(document.activeElement).toBe(button); expect(document.querySelector('textarea')).toBeNull();
  copy.mockReturnValue(false); await expect(copyStateName('STATE')).rejects.toThrow('手动复制');
 } finally {
  button.remove();
  if (original) Object.defineProperty(document, 'execCommand', original); else Reflect.deleteProperty(document, 'execCommand');
  if (clipboard) Object.defineProperty(navigator, 'clipboard', clipboard); else Reflect.deleteProperty(navigator, 'clipboard');
 }
});

it.each(['STATE', '9CYE91GH201_COM1', '9_11'])('saves state input %s to revision-guarded draft without activating', async (name) => {
 vi.spyOn(client.api, 'groups').mockResolvedValue([]);
 vi.spyOn(client.api, 'sources').mockResolvedValue([{ id: 'a', name: '来源 A', groupId: 'g' } as never]);
 vi.spyOn(calculationApi, 'draft').mockResolvedValue({ revision: 2, content: '', valid: true, hash: '', problems: [] });
 const calls = vi.spyOn(client, 'request').mockImplementation(async (path, init) => {
  if (path.includes('state-input-candidates')) return { items: [{ source_id: 'a', port_id: 'COM1', device_id: '11', device_name: '站点11' }], total: 1 };
  if (init?.method === 'PUT') return { valid: true };
  return { items: [], total: 0, revision: 2 };
 });
 const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
 render(<QueryClientProvider client={qc}><MemoryRouter><StateInputsTab /></MemoryRouter></QueryClientProvider>);
 expect(screen.getByText(/激活后直接生成同名只读 UInt32 OPC 节点/)).toBeInTheDocument();
 const user = userEvent.setup();
 await waitFor(() => expect(screen.getByText('新增状态输入', { selector: 'button' })).toBeEnabled());
 await user.click(screen.getByText('新增状态输入', { selector: 'button' }));
 await user.type(screen.getByLabelText('变量名'), name);
 await user.selectOptions(screen.getByLabelText('取源模式'), 'fixed');
 await user.selectOptions(screen.getByLabelText('下级来源'), 'a');
 await screen.findByRole('option', { name: /COM1 \/ 11/ });
 await user.selectOptions(screen.getByLabelText('站点 a'), '["COM1","11"]');
 await user.click(screen.getByRole('checkbox'));
 for (const invalid of ['11', 'IF', 'false', '9CYE.STATE']) {
  await user.clear(screen.getByLabelText('变量名'));
  await user.type(screen.getByLabelText('变量名'), invalid);
  expect(screen.getByText('保存草稿', { selector: 'button' })).toBeDisabled();
 }
 await user.clear(screen.getByLabelText('变量名'));
 await user.type(screen.getByLabelText('变量名'), name);
 expect(screen.getByText('保存草稿', { selector: 'button' })).toBeEnabled();
 await user.click(screen.getByText('保存草稿', { selector: 'button' }));
 await waitFor(() => expect(calls.mock.calls.some(([, init]) => init?.method === 'PUT')).toBe(true));
 const saved = calls.mock.calls.find(([, init]) => init?.method === 'PUT')!;
 expect(saved[1]?.headers).toEqual({ 'If-Match': '"2"' });
 expect(JSON.parse(String(saved[1]?.body)).state_inputs[0]).toEqual({ name, mode: 'fixed', targets: [{ source_id: 'a', port_id: 'COM1', device_id: '11' }] });
 expect(calls.mock.calls.some(([path]) => path.includes('/activate'))).toBe(false);
});

it('edits mapping content from the same complete draft snapshot as its revision, not an older displayed row', async () => {
 vi.spyOn(client.api, 'groups').mockResolvedValue([]);
 vi.spyOn(client.api, 'sources').mockResolvedValue([{ id: 'a', name: '来源 A', groupId: 'g' } as never]);
 vi.spyOn(calculationApi, 'draft').mockResolvedValue({ revision: 2, content: '# current bundle', valid: true, hash: '', problems: [] });
 const old = { name: 'STATE', mode: 'fixed', targets: [{ source_id: 'a', port_id: 'COM1', device_id: '11' }] };
 const latest = { ...old, targets: [{ source_id: 'a', port_id: 'COM2', device_id: '12' }] };
 const calls = vi.spyOn(client, 'request').mockImplementation(async (path, init) => {
  if (path.includes('state-input-candidates')) return { items: [{ ...latest.targets[0], device_name: '站点12' }], total: 1 };
  if (init?.method === 'PUT') return { valid: true };
  return path.includes('limit=500') ? { items: [latest], total: 1, revision: 2 }
   : { items: [{ ...old, value: 1, state_text: '通信正常', status_code: 0 }], total: 1, revision: 1 };
 });
 const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
 render(<QueryClientProvider client={qc}><MemoryRouter><StateInputsTab /></MemoryRouter></QueryClientProvider>);
 const user = userEvent.setup();
 await user.selectOptions(screen.getByLabelText('配置视图'), 'draft');
 const edit = await screen.findByRole('button', { name: '编辑' });
 await waitFor(() => expect(edit).toBeEnabled());
 await user.click(edit);
 expect(screen.getByLabelText('站点 a')).toHaveValue('["COM2","12"]');
 expect(screen.getByText(/已载入草稿 r2 中的最新映射/)).toBeInTheDocument();
 await user.click(screen.getByRole('checkbox'));
 await user.click(screen.getByRole('button', { name: '保存草稿' }));
 await waitFor(() => expect(calls.mock.calls.some(([, init]) => init?.method === 'PUT')).toBe(true));
 const saved = calls.mock.calls.find(([, init]) => init?.method === 'PUT')!;
 expect(saved[1]?.headers).toEqual({ 'If-Match': '"2"' });
 expect(JSON.parse(String(saved[1]?.body)).state_inputs[0]).toEqual(latest);
});

it('does not recreate a binding removed from the newer draft when an old row is still visible', async () => {
 vi.spyOn(client.api, 'groups').mockResolvedValue([]);
 vi.spyOn(client.api, 'sources').mockResolvedValue([]);
 vi.spyOn(calculationApi, 'draft').mockResolvedValue({ revision: 3, content: '# removed', valid: true, hash: '', problems: [] });
 const calls = vi.spyOn(client, 'request').mockImplementation(async (path) => path.includes('limit=500')
  ? { items: [], total: 0, revision: 3 }
  : { items: [{ name: 'REMOVED', mode: 'fixed', targets: [{ source_id: 'a', port_id: 'COM1', device_id: '11' }], value: 1, state_text: '通信正常', status_code: 0 }], total: 1, revision: 2 });
 const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
 render(<QueryClientProvider client={qc}><MemoryRouter><StateInputsTab /></MemoryRouter></QueryClientProvider>);
 const user = userEvent.setup();
 await user.selectOptions(screen.getByLabelText('配置视图'), 'draft');
 const edit = await screen.findByRole('button', { name: '编辑' });
 await waitFor(() => expect(edit).toBeEnabled());
 await user.click(edit);
 expect(screen.getByText('该状态输入已从草稿移除，请刷新列表后再编辑。')).toBeInTheDocument();
 expect(screen.queryByLabelText('变量名')).not.toBeInTheDocument();
 expect(calls.mock.calls.some(([, init]) => init?.method === 'PUT')).toBe(false);
});
