import { useEffect, useState } from 'react';
import { NavLink, Outlet } from 'react-router-dom';
import { Bell, Boxes, ChevronLeft, ClipboardList, DatabaseZap, Gauge, Menu, ServerCog, Settings, Tags, TriangleAlert, X } from 'lucide-react';
import { useOverview } from '../api/queries';
import { useEventStream } from '../hooks/useEventStream';
import { formatNumber } from '../lib/format';

const NAV_SECTIONS = [
  { label: '运行', items: [
    { to: '/', label: '运行总览', icon: Gauge, end: true },
    { to: '/groups', label: '分组与选源', icon: Boxes },
    { to: '/sources', label: '下级 OPC', icon: ServerCog },
  ] },
  { label: '数据', items: [
    { to: '/points', label: '点位浏览', icon: Tags },
    { to: '/commands', label: '命令记录', icon: ClipboardList },
  ] },
  { label: '维护', items: [
    { to: '/operations', label: '告警与审计', icon: TriangleAlert },
    { to: '/settings', label: '系统信息', icon: Settings },
  ] },
] as const;

export function AppShell() {
  const [collapsed, setCollapsed] = useState(false);
  const [mobileOpen, setMobileOpen] = useState(false);
  const [now, setNow] = useState(() => new Date());
  const overview = useOverview();
  const streamState = useEventStream();

  useEffect(() => {
    const timer = window.setInterval(() => setNow(new Date()), 1000);
    return () => window.clearInterval(timer);
  }, []);

  const data = overview.data;
  const streamLabel = streamState === 'connected' ? '实时' : streamState === 'mock' ? '演示数据' : streamState === 'connecting' ? '连接中' : '已断开';
  const managerGood = !overview.isError && data?.managerState === 'running';

  return (
    <div className={`app-shell${collapsed ? ' sidebar-collapsed' : ''}`}>
      <header className="topbar">
        <button className="mobile-menu-button icon-button" type="button" onClick={() => setMobileOpen((value) => !value)} aria-label="打开导航">{mobileOpen ? <X size={21} /> : <Menu size={21} />}</button>
        <div className="brand"><span className="brand-mark" aria-hidden="true"><DatabaseZap size={24} strokeWidth={2.1} /></span><span>RCD OPC 管理中心</span></div>
        <div className="topbar-actions">
          <span className={`global-service-state ${managerGood ? 'state-online' : 'state-offline'}`}><i />管理 OPC {managerGood ? '运行中' : '异常'}</span>
          <span className="topbar-time">{now.toLocaleString('zh-CN', { hour12: false })}</span>
          <NavLink className="icon-button notification-button" to="/operations?tab=alarms&state=Open" aria-label="查看未确认告警"><Bell size={19} />{data && data.unacknowledgedAlarms > 0 ? <span className="notification-count">{data.unacknowledgedAlarms}</span> : null}</NavLink>
        </div>
      </header>

      {mobileOpen ? <button className="mobile-backdrop" type="button" aria-label="关闭导航" onClick={() => setMobileOpen(false)} /> : null}
      <aside className={`sidebar${mobileOpen ? ' mobile-open' : ''}`}>
        <nav aria-label="主导航">{NAV_SECTIONS.map((section) => <div className="nav-section" key={section.label}><span className="nav-section-label">{section.label}</span>{section.items.map(({ to, label, icon: Icon, ...item }) => <NavLink key={to} to={to} end={'end' in item ? item.end : undefined} onClick={() => setMobileOpen(false)} title={collapsed ? label : undefined}><Icon size={19} strokeWidth={1.8} /><span>{label}</span></NavLink>)}</div>)}</nav>
        <div className="sidebar-footer"><div className="sidebar-mini-state"><span><i className={managerGood ? 'mini-good' : 'mini-bad'} />管理 OPC {managerGood ? '正常' : '异常'}</span><span><i className={streamState === 'connected' ? 'mini-blue' : 'mini-bad'} />SSE {streamLabel}</span></div><button className="collapse-button" type="button" onClick={() => setCollapsed((value) => !value)}><ChevronLeft size={18} /><span>{collapsed ? '展开' : '收起导航'}</span></button></div>
      </aside>

      <main className="main-content"><Outlet /></main>
      <footer className="statusbar"><span><i className={`status-light ${managerGood ? 'light-good' : 'light-bad'}`} />OPC：{data?.opcEndpoint ?? '无法获取'}</span><span>Web API：{overview.isError ? '异常' : data?.webApiState ?? '连接中'}</span><span>SSE：{streamLabel}</span><span>Session：{data ? formatNumber(data.sessionCount) : '—'}</span><span>订阅：{data ? formatNumber(data.subscriptionCount) : '—'}</span><span>点位：{data ? formatNumber(data.totalPoints) : '—'}</span><span className="statusbar-spacer" /><span>v{data?.version ?? '—'} {data?.gitSha && data.gitSha !== '—' ? `(${data.gitSha.slice(0, 7)})` : ''}</span><span>{now.toLocaleString('zh-CN', { hour12: false })}</span></footer>
    </div>
  );
}
