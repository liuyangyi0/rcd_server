import { useEffect, useState } from 'react';
import { NavLink, Outlet, useLocation } from 'react-router-dom';
import { Bell, Boxes, ChevronLeft, ClipboardList, Cpu, DatabaseZap, Gauge, Menu, ScrollText, ServerCog, Settings, Sigma, Tags, TriangleAlert, X } from 'lucide-react';
import { useOverview } from '../api/queries';
import { useEventStream } from '../hooks/useEventStream';
import { formatNumber } from '../lib/format';
import { AuthAccount } from './AuthGate';

const NAV_SECTIONS = [
  { label: '运行', items: [
    { to: '/', label: '运行总览', icon: Gauge, end: true },
    { to: '/groups', label: '分组与选源', icon: Boxes },
    { to: '/sources', label: '下级 OPC', icon: ServerCog },
  ] },
  { label: '数据', items: [
    { to: '/points', label: '点位浏览', icon: Tags },
    { to: '/device-versions', label: '设备版本', icon: Cpu },
	{ to: '/calculations', label: '计算中心', icon: Sigma },
    { to: '/commands', label: '命令记录', icon: ClipboardList },
    { to: '/lower-logs', label: '下级日志', icon: ScrollText },
  ] },
  { label: '维护', items: [
    { to: '/operations', label: '告警与审计', icon: TriangleAlert },
    { to: '/settings', label: '系统信息', icon: Settings },
  ] },
] as const;

export function AppShell() {
  const location = useLocation();
  useEffect(() => { window.scrollTo(0, 0); }, [location.pathname]);
  const [collapsed, setCollapsed] = useState(false);
  const [mobileOpen, setMobileOpen] = useState(false);
  const overview = useOverview();
  const streamState = useEventStream();

  const data = overview.data;
  const streamLabel = streamState === 'connected' ? '实时' : streamState === 'mock' ? '演示数据' : streamState === 'connecting' ? '连接中' : '已断开';
  const managerGood = !overview.isError && data?.managerState === 'running';
  const managerPending = overview.isPending;
  const managerLabel = managerPending ? '正在读取' : overview.isError ? '状态获取失败' : managerGood ? '运行中' : '异常';

  return (
    <div className={`app-shell${collapsed ? ' sidebar-collapsed' : ''}`}>
      <header className="topbar">
        <button className="mobile-menu-button icon-button" type="button" onClick={() => setMobileOpen((value) => !value)} aria-label={mobileOpen ? '关闭导航' : '打开导航'}>{mobileOpen ? <X size={21} /> : <Menu size={21} />}</button>
        <div className="brand"><span className="brand-mark" aria-hidden="true"><DatabaseZap size={24} strokeWidth={2.1} /></span><span>RCD OPC 管理中心</span></div>
        <div className="topbar-actions">
          <span className={`global-service-state ${managerPending ? 'state-pending' : managerGood ? 'state-online' : 'state-offline'}`}><i />管理 OPC {managerLabel}</span>
          <Clock />
          <AuthAccount />
          <NavLink className="icon-button notification-button" to="/operations?tab=alarms&state=Open" aria-label="查看未确认告警"><Bell size={19} />{data && data.unacknowledgedAlarms > 0 ? <span className="notification-count">{data.unacknowledgedAlarms}</span> : null}</NavLink>
        </div>
      </header>

      {mobileOpen ? <button className="mobile-backdrop" type="button" aria-label="关闭导航" onClick={() => setMobileOpen(false)} /> : null}
      <aside className={`sidebar${mobileOpen ? ' mobile-open' : ''}`}>
        <nav aria-label="主导航">{NAV_SECTIONS.map((section) => <div className="nav-section" key={section.label}><span className="nav-section-label">{section.label}</span>{section.items.map(({ to, label, icon: Icon, ...item }) => <NavLink key={to} to={to} end={'end' in item ? item.end : undefined} onClick={() => setMobileOpen(false)} title={collapsed ? label : undefined}><Icon size={19} strokeWidth={1.8} /><span>{label}</span></NavLink>)}</div>)}</nav>
        <div className="sidebar-footer"><div className="sidebar-mini-state"><span><i className={managerPending ? 'mini-pending' : managerGood ? 'mini-good' : 'mini-bad'} />管理 OPC {managerLabel}</span><span><i className={streamState === 'connected' ? 'mini-blue' : 'mini-pending'} />实时通知 {streamLabel}</span></div><button className="collapse-button" type="button" aria-label={collapsed ? '展开导航' : '收起导航'} onClick={() => setCollapsed((value) => !value)}><ChevronLeft size={18} /><span>{collapsed ? '展开' : '收起导航'}</span></button></div>
      </aside>

      <main className="main-content"><Outlet /></main>
      <footer className="statusbar"><span><i className={`status-light ${managerPending ? 'light-pending' : managerGood ? 'light-good' : 'light-bad'}`} />OPC：{data?.opcEndpoint ?? (managerPending ? '正在读取' : '无法获取')}</span><span>实时通知：{streamLabel}</span><span>客户端会话：{data ? formatNumber(data.sessionCount) : '—'}</span><span>订阅：{data ? formatNumber(data.subscriptionCount) : '—'}</span><span>变量：{data ? formatNumber(data.totalPoints) : '—'}</span></footer>
    </div>
  );
}

function Clock() {
  const [now, setNow] = useState(() => new Date());
  useEffect(() => { const timer = setInterval(() => setNow(new Date()), 1000); return () => clearInterval(timer); }, []);
  return <span className="topbar-time">{now.toLocaleString('zh-CN', { hour12: false })}</span>;
}
