import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter, useNavigate } from 'react-router-dom';
import { afterEach, expect, it, vi } from 'vitest';
import HelpPage from './HelpPage';
import { atomicExample } from '../features/help/content';

afterEach(() => { vi.restoreAllMocks(); vi.unstubAllGlobals(); });

function Back() { const navigate = useNavigate(); return <button onClick={() => navigate(-1)}>返回上一篇</button>; }
function setup(entry = '/help') {
  const user = userEvent.setup();
  render(<MemoryRouter initialEntries={[entry]}><HelpPage /><Back /></MemoryRouter>);
  return user;
}

it('shows relationship and maintenance documentation without an API request or execution button', () => {
  const fetch = vi.spyOn(globalThis, 'fetch');
  setup();
  expect(screen.getByRole('figure', { name: /软件关系图/ })).toBeInTheDocument();
  expect(screen.getByRole('link', { name: '软件维护、升级与恢复' })).toHaveAttribute('href', '/help?topic=platform');
  expect(screen.getByText(/Agent 是采集进程唯一守护者/)).toBeInTheDocument();
  expect(screen.queryByRole('button', { name: /确认升级|启动程序|发送命令|保存到下级/ })).not.toBeInTheDocument();
  expect(fetch).not.toHaveBeenCalled();
});

it('filters all topics, clears empty results and supports browser history and keyboard navigation', async () => {
  const user = setup();
  const input = screen.getByRole('searchbox', { name: '搜索说明、节点或参数' });
  await user.type(input, '上传 软件包');
  await user.selectOptions(screen.getByRole('combobox', { name: '主题分类' }), '软件维护升级');
  expect(screen.getByRole('heading', { name: '找到 1 个主题' })).toBeInTheDocument();
  await user.click(within(screen.getByRole('region', { name: '找到 1 个主题' })).getByRole('link', { name: /软件维护、升级与恢复/ }));
  expect(screen.getByRole('heading', { name: '软件维护、升级与恢复', level: 2 })).toHaveFocus();
  await user.click(screen.getByRole('button', { name: '返回上一篇' }));
  expect(screen.getByRole('heading', { name: '找到 1 个主题' })).toBeInTheDocument();
  await user.clear(input);
  await user.type(input, '不存在的帮助123');
  expect(screen.getByRole('heading', { name: '找到 0 个主题' })).toBeInTheDocument();
  await user.click(screen.getByRole('button', { name: '清除筛选' }));
  const link = screen.getByRole('link', { name: '下发命令、队列与结果' });
  link.focus(); await user.keyboard('{Enter}');
  expect(screen.getByRole('heading', { name: '下发命令、队列与结果', level: 2 })).toHaveFocus();
});

it('copies the exact NodeId and complete atomic JSON, rather than rendered labels', async () => {
  const user = setup('/help?topic=communication');
  const write = vi.spyOn(navigator.clipboard, 'writeText');
  const code = screen.getByText('ns=2;s=DATA.Buildings.91UBH.Fire', { selector: 'code' });
  await user.click(within(code.parentElement!).getByRole('button', { name: '复制节点' }));
  expect(write).toHaveBeenLastCalledWith('ns=2;s=DATA.Buildings.91UBH.Fire');
  expect(within(code.parentElement!).getByText('已复制')).toBeInTheDocument();
  await user.click(screen.getByRole('button', { name: '复制完整 39 项 JSON 格式示例' }));
  expect(write).toHaveBeenLastCalledWith(atomicExample);
  expect(JSON.parse(write.mock.calls.at(-1)![0]).fields).toHaveProperty('DATA.Buildings.1JMF34.Fire');
});

it('keeps a selectable manual copy field when the browser denies both copy APIs', async () => {
  const user = setup('/help?topic=connect');
  vi.spyOn(navigator.clipboard, 'writeText').mockRejectedValue(new Error('denied'));
  Object.defineProperty(document, 'execCommand', { configurable: true, value: vi.fn(() => false) });
  const row = screen.getByText('ns=2;s=DATA.Buildings.91UBH.Fire', { selector: 'code' }).parentElement!;
  await user.click(within(row).getByRole('button', { name: '复制节点' }));
  await waitFor(() => expect(within(row).getByRole('textbox', { name: '手动复制：复制节点' })).toHaveValue('ns=2;s=DATA.Buildings.91UBH.Fire'));
  expect(document.querySelectorAll('textarea')).toHaveLength(0);
  Reflect.deleteProperty(document, 'execCommand');
});

it('handles stale topic links without silently showing the wrong article', () => {
  setup('/help?topic=removed');
  expect(screen.getByRole('heading', { name: '未找到这个帮助主题' })).toBeInTheDocument();
  expect(screen.getByRole('link', { name: '返回软件关系与入口' })).toHaveAttribute('href', '/help');
  fireEvent.change(screen.getByRole('searchbox'), { target: { value: '<img src=x onerror=alert(1)>' } });
  expect(screen.getByRole('heading', { name: '找到 0 个主题' })).toBeInTheDocument();
});

it('uses in-document buttons for the table of contents without corrupting the app hash route', async () => {
  const user = setup('/help?topic=configuration');
  const scroll = vi.fn();
  Object.defineProperty(HTMLElement.prototype, 'scrollIntoView', { configurable: true, value: scroll });
  try {
    await user.click(within(screen.getByRole('navigation', { name: '本篇目录' })).getByRole('button', { name: '通过网页修改下级配置' }));
    expect(scroll).toHaveBeenCalledWith({ behavior: 'auto', block: 'start' });
    expect(screen.getByRole('heading', { name: '通过网页修改下级配置', level: 3 })).toHaveFocus();
  } finally { Reflect.deleteProperty(HTMLElement.prototype, 'scrollIntoView'); }
});
