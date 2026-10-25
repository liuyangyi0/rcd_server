import { act, fireEvent, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, expect, it, vi } from 'vitest';
import { Drawer } from './Drawer';
import { Modal } from './Modal';

afterEach(() => { vi.restoreAllMocks(); });

it('keeps the focused editor across parent refreshes and calls the latest close handler', async () => {
  let frame: FrameRequestCallback | undefined;
  const request = vi.spyOn(window, 'requestAnimationFrame').mockImplementation((callback) => { frame = callback; return 17; });
  const cancel = vi.spyOn(window, 'cancelAnimationFrame');
  const firstClose = vi.fn();
  const nextClose = vi.fn();
  const { rerender, unmount } = render(<Modal open title="编辑配置" onClose={firstClose}><input aria-label="名称" /></Modal>);
  act(() => frame?.(0));
  const input = screen.getByLabelText('名称');
  const user = userEvent.setup();
  await user.click(input);
  await user.type(input, '保留焦点');
  rerender(<Modal open title="编辑配置" onClose={nextClose}><input aria-label="名称" /></Modal>);
  expect(input).toHaveFocus();
  expect(input).toHaveValue('保留焦点');
  expect(request).toHaveBeenCalledTimes(1);
  fireEvent.keyDown(document, { key: 'Escape' });
  expect(nextClose).toHaveBeenCalledOnce();
  expect(firstClose).not.toHaveBeenCalled();
  unmount();
  expect(cancel).toHaveBeenCalledWith(17);
});

it('cancels pending initial focus and restores the opener only when the modal closes', () => {
  vi.spyOn(window, 'requestAnimationFrame').mockReturnValue(23);
  const cancel = vi.spyOn(window, 'cancelAnimationFrame');
  const opener = document.createElement('button');
  document.body.append(opener);
  opener.focus();
  const { rerender } = render(<Modal open title="待打开" onClose={() => {}}><input /></Modal>);
  rerender(<Modal open={false} title="待打开" onClose={() => {}}><input /></Modal>);
  expect(cancel).toHaveBeenCalledWith(23);
  expect(opener).toHaveFocus();
  opener.remove();
});

it('includes textareas and links in keyboard focus wrapping and excludes hidden controls', () => {
  vi.spyOn(window, 'requestAnimationFrame').mockReturnValue(29);
  render(<Modal open title="编辑配置" onClose={() => {}}>
    <textarea aria-label="配置原文" />
    <a href="#help">使用说明</a>
    <input type="hidden" />
    <a hidden href="#hidden">隐藏链接</a>
    <textarea disabled aria-label="不可编辑" />
  </Modal>);
  const panel = screen.getByRole('dialog');
  const close = screen.getByRole('button', { name: '关闭弹窗' });
  const link = screen.getByRole('link', { name: '使用说明' });
  close.focus();
  fireEvent.keyDown(document, { key: 'Tab', shiftKey: true });
  expect(link).toHaveFocus();
  fireEvent.keyDown(document, { key: 'Tab' });
  expect(close).toHaveFocus();
  panel.focus();
  fireEvent.keyDown(document, { key: 'Tab' });
  expect(close).toHaveFocus();
  screen.getByLabelText('配置原文').focus();
  expect(fireEvent.keyDown(document, { key: 'Tab' })).toBe(true);
});

it('closes only the innermost overlay on Escape', () => {
  const closeDrawer = vi.fn();
  const closeDialog = vi.fn();
  const view = (dialog: boolean) => <Drawer open title="分组详情" onClose={() => closeDrawer()}>{dialog ? <Modal open title="切换来源" onClose={closeDialog}><button type="button">确认</button></Modal> : null}</Drawer>;
  const { rerender } = render(view(true));
  // A drawer that re-renders with a new onClose must not move above its dialog.
  rerender(view(true));
  fireEvent.keyDown(document, { key: 'Escape' });
  expect(closeDialog).toHaveBeenCalledOnce();
  expect(closeDrawer).not.toHaveBeenCalled();
  rerender(view(false));
  fireEvent.keyDown(document, { key: 'Escape' });
  expect(closeDrawer).toHaveBeenCalledOnce();
});
