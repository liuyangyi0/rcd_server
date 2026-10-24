export interface FlowDevice { id: string; name: string; group_id: string; state: number; observed_at?: string; point_count: number }
export interface FlowPort { id: string; name: string; devices: FlowDevice[] }
export interface FlowSource { id: string; name: string; address: string; enabled: boolean; connected: boolean; subscribed: boolean; catalog_state: string; last_data_at?: string; samples: string; ports: FlowPort[]; truncated: boolean }
export interface FlowGroup { id: string; name: string; default_source: string; members: string[] }
export interface FlowSession { id: string; name: string; attached: boolean; routes: Record<string, string>; subscriptions: number; monitored_items: number; notifications: Record<string, number> }
export interface FlowSnapshot { generated_at: string; instance: string; endpoint: string; running: boolean; pending_commands: number; sources: FlowSource[]; groups: FlowGroup[]; sessions: FlowSession[]; calculation: { active: boolean; rules: number; good: number; bad: number; last_evaluation_at?: string; duration_ms: number } }
export interface FlowPoint { id: string; type: string; value: unknown; status: number; source_timestamp?: string; value_omitted: boolean }
export interface FlowPointPage { items: FlowPoint[]; total: number; offset: number }
export const STATION_LABELS = ['状态未知 / 已过期', '通信正常', '通信异常', '串口未打开', '已停用', '下级离线'];
export const STATION_COLORS = ['#83949d', '#53e3bb', '#f5b951', '#f5b951', '#748591', '#f17479'];

export function sourceCanFlow(source: FlowSource) {
  return source.enabled && source.connected && source.subscribed && source.catalog_state !== 'CatalogConflict';
}

export function notificationDelta(previous: FlowSession, current: FlowSession): number {
  let total = 0;
  for (const [id, count] of Object.entries(current.notifications)) {
    const old = previous.notifications[id];
    // New subscription starts a new baseline. The uint32 OPC diagnostic can wrap.
    if (old !== undefined) total += (count - old) >>> 0;
  }
  return total;
}

export function sourceDelta(previous: FlowSource, current: FlowSource): number {
  try { const delta = BigInt(current.samples) - BigInt(previous.samples); return delta > 0n ? Number(delta > 1000000n ? 1000000n : delta) : 0; }
  catch { return 0; }
}

export interface Activity { sources: Record<string, number>; sessions: Record<string, number>; updates: number }
export function advanceActivity(previous: FlowSnapshot | undefined, current: FlowSnapshot, activity: Activity, now: number): Activity {
  const next: Activity = { sources: {}, sessions: {}, updates: 0 };
  if (!previous || previous.instance !== current.instance) return next;
  for (const source of current.sources) {
    const before = previous.sources.find((s) => s.id === source.id);
    const delta = before ? sourceDelta(before, source) : 0;
    if (sourceCanFlow(source)) next.sources[source.id] = delta > 0 ? now : activity.sources[source.id] ?? 0;
    next.updates += delta;
  }
  for (const session of current.sessions) {
    const before = previous.sessions.find((s) => s.id === session.id);
    if (session.attached) next.sessions[session.id] = before && notificationDelta(before, session) > 0 ? now : activity.sessions[session.id] ?? 0;
  }
  return next;
}

export function visibleEvents(previous: FlowSnapshot | undefined, current: FlowSnapshot): string[] {
  if (!previous || previous.instance !== current.instance) return [];
  const events: string[] = [];
  for (const source of current.sources) {
    const before = previous.sources.find((s) => s.id === source.id);
    if (!before) { events.push(`${source.name || source.id} 已加入`); continue; }
    if (before.connected !== source.connected) events.push(`${source.name || source.id}：OPC ${source.connected ? '连接恢复' : '连接断开'}`);
    const oldDevices = new Map(before.ports.flatMap((p) => p.devices.map((d) => [`${p.id}\0${d.id}`, d.state] as const)));
    for (const port of source.ports) for (const device of port.devices) {
      const old = oldDevices.get(`${port.id}\0${device.id}`);
      if (old !== undefined && old !== device.state) events.push(`${source.id} / ${port.name || port.id} / 站${device.id}：${STATION_LABELS[device.state] ?? '未知'}`);
      if (events.length >= 30) return events;
    }
  }
  for (const session of current.sessions) {
    const before = previous.sessions.find((s) => s.id === session.id);
    if (!before) { events.push(`客户端 ${session.name || session.id.slice(0, 6)} 已接入`); continue; }
    for (const [group, source] of Object.entries(session.routes)) if (before.routes[group] !== source) events.push(`${session.name || session.id.slice(0, 6)}：${group} 选源 → ${source || '未选择'}`);
  }
  for (const session of previous.sessions) if (!current.sessions.some((s) => s.id === session.id)) events.push(`客户端 ${session.name || session.id.slice(0, 6)} 会话已结束`);
  return events.slice(0, 30);
}

export async function publicRead<T>(path: string, signal?: AbortSignal): Promise<T> {
  const controller = new AbortController();
  const cancel = () => controller.abort();
  if (signal?.aborted) cancel();
  else signal?.addEventListener('abort', cancel, { once: true });
  const timeout = setTimeout(cancel, 4000);
  try {
    const response = await fetch(`/api/v1/public/flow${path}`, { signal: controller.signal, credentials: 'omit', cache: 'no-store' });
    if (!response.ok) throw new Error(`读取失败（HTTP ${response.status}）`);
    const value: unknown = await response.json();
    if (!value || typeof value !== 'object') throw new Error('监视数据格式错误');
    return value as T;
  } finally {
    clearTimeout(timeout);
    signal?.removeEventListener('abort', cancel);
  }
}
