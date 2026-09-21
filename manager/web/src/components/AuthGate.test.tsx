import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, expect, it, vi } from 'vitest';
import { AuthAccount, AuthGate } from './AuthGate';

afterEach(() => { vi.restoreAllMocks(); vi.unstubAllGlobals(); });
it('gates content, reports incorrect credentials, logs in and logs out', async () => {
  const fetchMock = vi.fn(async (path: string, init?: RequestInit) => {
    if (path.endsWith('/session')) return json({ authenticated: false });
    if (path.endsWith('/logout')) return new Response(null, { status: 204 });
    if (path.endsWith('/login')) return JSON.parse(String(init?.body)).password === 'admin' ? json({ authenticated: true, username: 'admin' }) : json({ error: { code: 'invalid_credentials', message: '账号或密码错误' } }, 401);
    throw new Error(path);
  });
  vi.stubGlobal('fetch', fetchMock); vi.spyOn(window, 'confirm').mockReturnValue(true);
  const user = userEvent.setup();
  render(<QueryClientProvider client={new QueryClient()}><AuthGate><AuthAccount /><p>受保护的内容</p></AuthGate></QueryClientProvider>);
  expect(screen.queryByText('受保护的内容')).not.toBeInTheDocument();
  await user.type(await screen.findByLabelText('密码'), 'wrong'); await user.click(screen.getByRole('button', { name: '登录' }));
  expect(await screen.findByText('账号或密码错误')).toBeInTheDocument();
  await user.clear(screen.getByLabelText('密码')); await user.type(screen.getByLabelText('密码'), 'admin'); await user.click(screen.getByRole('button', { name: '登录' }));
  expect(await screen.findByText('受保护的内容')).toBeInTheDocument();
  await user.click(screen.getByRole('button', { name: '退出登录' }));
  await waitFor(() => expect(screen.queryByText('受保护的内容')).not.toBeInTheDocument());
});
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
