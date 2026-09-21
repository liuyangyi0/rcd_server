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

it('saves selected station to revision-guarded draft without activating', async () => {
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
 const user = userEvent.setup();
 await waitFor(() => expect(screen.getByText('新增状态输入', { selector: 'button' })).toBeEnabled());
 await user.click(screen.getByText('新增状态输入', { selector: 'button' }));
 await user.type(screen.getByLabelText('变量名'), 'STATE');
 await user.selectOptions(screen.getByLabelText('取源模式'), 'fixed');
 await user.selectOptions(screen.getByLabelText('下级来源'), 'a');
 await screen.findByRole('option', { name: /COM1 \/ 11/ });
 await user.selectOptions(screen.getByLabelText('站点 a'), '["COM1","11"]');
 await user.click(screen.getByRole('checkbox'));
 await user.click(screen.getByText('保存草稿', { selector: 'button' }));
 await waitFor(() => expect(calls.mock.calls.some(([, init]) => init?.method === 'PUT')).toBe(true));
 const saved = calls.mock.calls.find(([, init]) => init?.method === 'PUT')!;
 expect(saved[1]?.headers).toEqual({ 'If-Match': '"2"' });
 expect(JSON.parse(String(saved[1]?.body)).state_inputs[0]).toEqual({ name: 'STATE', mode: 'fixed', targets: [{ source_id: 'a', port_id: 'COM1', device_id: '11' }] });
 expect(calls.mock.calls.some(([path]) => path.includes('/activate'))).toBe(false);
});
