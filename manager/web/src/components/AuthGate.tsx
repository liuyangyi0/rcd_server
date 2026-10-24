import { createContext, useContext, useEffect, useState, type ReactNode } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { request, apiErrorMessage } from '../api/client';

interface Session { authenticated: boolean; username?: string; password_change_required?: boolean }
const AuthContext = createContext<{ username: string; logout: () => void; change: () => void } | null>(null);

export function AuthAccount() {
  const auth = useContext(AuthContext);
  return auth ? <div className="auth-account"><span>{auth.username}</span><button type="button" onClick={auth.change}>修改密码</button><button type="button" onClick={auth.logout}>退出登录</button></div> : null;
}

export function AuthGate({ children }: { children: ReactNode }) {
  const client = useQueryClient();
  const [session, setSession] = useState<Session>();
  const [error, setError] = useState('');
  const [changing, setChanging] = useState(false);
  const [busy, setBusy] = useState(false);
  const [username, setUsername] = useState('admin');
  const [password, setPassword] = useState('');
  const [newPassword, setNewPassword] = useState('');
  const [confirm, setConfirm] = useState('');
  const clear = () => { client.clear(); window.dispatchEvent(new Event('rcd-auth-cleared')); };
  const check = async () => {
    try {
      const value = await request('/auth/session') as Session;
      if (!value.authenticated) { clear(); setChanging(false); }
      setSession(value); setError('');
    } catch (reason) { setError(apiErrorMessage(reason)); }
  };
  useEffect(() => {
    void check();
    const expired = () => { clear(); setSession({ authenticated: false }); setPassword(''); setChanging(false); setError('登录已过期，请重新登录。'); };
    window.addEventListener('rcd-auth-expired', expired);
    const timer = window.setInterval(() => void check(), 60_000);
    return () => { window.clearInterval(timer); window.removeEventListener('rcd-auth-expired', expired); };
  }, []);
  const logout = async () => {
    if (!window.confirm('确认退出登录？请先保存未提交的编辑。')) return;
    try { await request('/auth/logout', { method: 'POST' }); clear(); setSession({ authenticated: false }); setChanging(false); setPassword(''); }
    catch (reason) { setError(apiErrorMessage(reason)); }
  };
  const changeMode = changing || session?.password_change_required;
  const submit = async (event: React.FormEvent) => {
    event.preventDefault(); setError('');
    if (changeMode && newPassword !== confirm) { setError('两次输入的新密码不一致。'); return; }
    setBusy(true);
    try {
      if (changeMode) {
        await request('/auth/password', { method: 'POST', body: JSON.stringify({ current_password: password, new_password: newPassword }) });
        clear(); setSession({ authenticated: false }); setChanging(false); setNewPassword(''); setConfirm(''); setError('密码已修改，请重新登录。');
      } else {
        const value = await request('/auth/login', { method: 'POST', body: JSON.stringify({ username, password }) }) as Session;
        clear(); setSession(value);
      }
      setPassword('');
    } catch (reason) { setError(apiErrorMessage(reason)); }
    finally { setBusy(false); }
  };
  if (session?.authenticated && !changeMode) return <AuthContext.Provider value={{ username: session.username ?? '', logout: () => void logout(), change: () => { setPassword(''); setError(''); setChanging(true); } }}>{error ? <div role="alert" className="auth-alert">{error}</div> : null}{children}</AuthContext.Provider>;
  return <main className="login-page"><section className="login-panel"><h1>RCD OPC 管理中心</h1><p>{changeMode ? '修改管理员密码；成功后所有网页会话需重新登录。' : '请使用管理员账号登录。OPC 客户端连接不受网页登录影响。'}</p>
    {!session ? <><p role="status">{error || '正在检查登录状态…'}</p>{error ? <button type="button" className="button button-secondary" onClick={() => void check()}>重试</button> : null}</> : <form onSubmit={(event) => void submit(event)}>
      {!changeMode ? <label className="field"><span>账号</span><input autoComplete="username" required maxLength={64} value={username} disabled={busy} onChange={(e) => setUsername(e.target.value)} /></label> : null}
      <label className="field"><span>{changeMode ? '原密码' : '密码'}</span><input type="password" autoComplete="current-password" required value={password} disabled={busy} onChange={(e) => setPassword(e.target.value)} /></label>
      {changeMode ? <><label className="field"><span>新密码（至少 8 字节）</span><input type="password" autoComplete="new-password" required value={newPassword} disabled={busy} onChange={(e) => setNewPassword(e.target.value)} /></label><label className="field"><span>确认新密码</span><input type="password" autoComplete="new-password" required value={confirm} disabled={busy} onChange={(e) => setConfirm(e.target.value)} /></label></> : null}
      {error ? <p role="alert" className="text-warning">{error}</p> : null}
      <button className="button button-primary" type="submit" disabled={busy}>{busy ? '正在提交…' : changeMode ? '保存密码' : '登录'}</button>
      {changing && !session.password_change_required ? <button className="button button-secondary" type="button" disabled={busy} onClick={() => { setChanging(false); setPassword(''); setNewPassword(''); setConfirm(''); setError(''); }}>取消</button> : null}
    </form>}<a href="#/flow" className="button button-secondary" style={{marginTop:16}}>免登录查看数据流监视 →</a></section></main>;
}
