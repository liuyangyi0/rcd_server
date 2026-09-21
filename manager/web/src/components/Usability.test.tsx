import { fireEvent, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, expect, it, vi } from 'vitest';
import { Pagination } from './Pagination';
import { CopyTextButton } from './CopyTextButton';
import * as clipboard from '../lib/clipboard';

afterEach(() => vi.restoreAllMocks());

it('offers bounded page sizes and reports the selected size', async () => {
  const change = vi.fn();
  render(<Pagination page={1} pageSize={50} total={301} onPageChange={vi.fn()} onPageSizeChange={change} />);
  await userEvent.selectOptions(screen.getByRole('combobox', { name: '每页条数' }), '100');
  expect(change).toHaveBeenCalledWith(100);
});

it('offers page jumps without showing inverted ranges after data shrinks', async () => {
  const change = vi.fn();
  const view = render(<Pagination page={2} pageSize={50} total={401} onPageChange={change} />);
  expect(screen.getByText('2 / 9 页')).toBeInTheDocument();
  fireEvent.change(screen.getByRole('spinbutton', { name: '跳转页码' }), { target: { value: '8' } });
  await userEvent.click(screen.getByRole('button', { name: '跳转' }));
  expect(change).toHaveBeenCalledWith(8);
  view.rerender(<Pagination page={8} pageSize={50} total={3} onPageChange={change} />);
  expect(screen.getByText('显示 0 到 0，共 3 条')).toBeInTheDocument();
  await userEvent.click(screen.getByRole('button', { name: '返回末页' }));
  expect(change).toHaveBeenLastCalledWith(1);
});

it('shows selectable text instead of an unhandled rejection when copy is blocked', async () => {
  vi.spyOn(clipboard, 'copyText').mockRejectedValue(new Error('blocked'));
  render(<CopyTextButton value="ns=2;s=EXAMPLE" label="复制节点" />);
  await userEvent.click(screen.getByRole('button', { name: '复制节点' }));
  expect(await screen.findByRole('textbox', { name: '手动复制：复制节点' })).toHaveValue('ns=2;s=EXAMPLE');
});
