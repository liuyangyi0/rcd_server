export interface FlowDevice { id: string; name: string; group_id: string; state: number; observed_at?: string; point_count: number }
export interface FlowPort { id: string; name: string; devices: FlowDevice[] }
export interface FlowSource { id: string; name: string; address: string; enabled: boolean; connected: boolean; subscribed: boolean; catalog_state: string; last_data_at?: string; samples: string; ports: FlowPort[]; truncated: boolean }
export interface FlowGroup { id: string; name: string; default_source: string; members: string[] }
export interface FlowSession { id: string; name: string; attached: boolean; routes: Record<string, string>; subscriptions: number; monitored_items: number; notifications: Record<string, number> }
export interface FlowSnapshot { generated_at: string; instance: string; endpoint: string; running: boolean; pending_commands: number; sources: FlowSource[]; groups: FlowGroup[]; sessions: FlowSession[]; calculation: { active: boolean; rules: number; good: number; bad: number; last_evaluation_at?: string; duration_ms: number } }
export interface FlowPoint { id: string; type: string; value: unknown; status: number; source_timestamp?: string; value_omitted: boolean }
export interface FlowPointPage { items: FlowPoint[]; total: number; offset: number }
export const STATION_LABELS = ['状态未知 / 已过期', '通信正常', '通信异常', '串口未打开', '已停用', '下级离线'];
export const STATION_COLORS = ['#6b7a8c', '#3fd6a0', '#f0b45a', '#f0b45a', '#56657a', '#ff6f6f'];

// Port rows shown on a source card and drawn as wires; the last row folds any remaining ports.
export function portRows(ports: FlowPort[], max: number): { key: string; ports: FlowPort[] }[] {
  if (ports.length <= max) return ports.map((port) => ({ key: port.id, ports: [port] }));
  return [...ports.slice(0, max - 1).map((port) => ({ key: port.id, ports: [port] })), { key: '+', ports: ports.slice(max - 1) }];
}

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

// Timestamps mark the last snapshot with real updates; deltas hold that snapshot's counts.
export interface Activity { sources: Record<string, number>; sessions: Record<string, number>; updates: number; sourceDeltas: Record<string, number>; sessionDeltas: Record<string, number> }
export function advanceActivity(previous: FlowSnapshot | undefined, current: FlowSnapshot, activity: Activity, now: number): Activity {
  const next: Activity = { sources: {}, sessions: {}, updates: 0, sourceDeltas: {}, sessionDeltas: {} };
  if (!previous || previous.instance !== current.instance) return next;
  for (const source of current.sources) {
    const before = previous.sources.find((s) => s.id === source.id);
    const delta = before ? sourceDelta(before, source) : 0;
    if (sourceCanFlow(source)) next.sources[source.id] = delta > 0 ? now : activity.sources[source.id] ?? 0;
    if (sourceCanFlow(source) && delta > 0) next.sourceDeltas[source.id] = delta;
    next.updates += delta;
  }
  for (const session of current.sessions) {
    const before = previous.sessions.find((s) => s.id === session.id);
    const delta = session.attached && before ? notificationDelta(before, session) : 0;
    if (session.attached) next.sessions[session.id] = delta > 0 ? now : activity.sessions[session.id] ?? 0;
    if (delta > 0) next.sessionDeltas[session.id] = delta;
  }
  return next;
}

// One trace column per received snapshot: each source's real sample count and link state,
// plus the notifications all Sessions generated in the same interval.
export type TraceTone = 'ok' | 'warn' | 'down' | 'off';
export interface TraceFrame { at: string; notes: number; sources: Record<string, { delta: number; tone: TraceTone }> }
export function traceFrame(snapshot: FlowSnapshot, activity: Activity): TraceFrame {
  const tone = (s: FlowSource): TraceTone => !s.enabled ? 'off' : !s.connected ? 'down' : s.catalog_state === 'CatalogConflict' ? 'warn' : 'ok';
  const notes = Object.values(activity.sessionDeltas).reduce((sum, value) => sum + value, 0);
  return { at: snapshot.generated_at, notes, sources: Object.fromEntries(snapshot.sources.map((s) => [s.id, { delta: activity.sourceDeltas[s.id] ?? 0, tone: tone(s) }])) };
}

// kind groups events for the event list; tone says whether the change is a recovery, a fault or neutral.
export interface FlowEvent { kind: 'source' | 'station' | 'route' | 'session'; tone: 'good' | 'bad' | 'info'; text: string }
export function visibleEvents(previous: FlowSnapshot | undefined, current: FlowSnapshot): FlowEvent[] {
  if (!previous || previous.instance !== current.instance) return [];
  const events: FlowEvent[] = [];
  for (const source of current.sources) {
    const before = previous.sources.find((s) => s.id === source.id);
    if (!before) { events.push({ kind: 'source', tone: 'info', text: `${source.name || source.id} 已加入` }); continue; }
    if (before.connected !== source.connected) events.push({ kind: 'source', tone: source.connected ? 'good' : 'bad', text: `${source.name || source.id}：OPC ${source.connected ? '连接恢复' : '连接断开'}` });
    const oldDevices = new Map(before.ports.flatMap((p) => p.devices.map((d) => [`${p.id}\0${d.id}`, d.state] as const)));
    for (const port of source.ports) for (const device of port.devices) {
      const old = oldDevices.get(`${port.id}\0${device.id}`);
      if (old !== undefined && old !== device.state) events.push({ kind: 'station', tone: device.state === 1 ? 'good' : [2, 3, 5].includes(device.state) ? 'bad' : 'info', text: `${source.id} / ${port.name || port.id} / 站${device.id}：${STATION_LABELS[device.state] ?? '未知'}` });
      if (events.length >= 30) return events;
    }
  }
  for (const session of current.sessions) {
    const before = previous.sessions.find((s) => s.id === session.id);
    if (!before) { events.push({ kind: 'session', tone: 'good', text: `客户端 ${session.name || session.id.slice(0, 6)} 已接入` }); continue; }
    for (const [group, source] of Object.entries(session.routes)) if (before.routes[group] !== source) events.push({ kind: 'route', tone: 'info', text: `${session.name || session.id.slice(0, 6)}：${group} 选源 → ${source || '未选择'}` });
  }
  for (const session of previous.sessions) if (!current.sessions.some((s) => s.id === session.id)) events.push({ kind: 'session', tone: 'info', text: `客户端 ${session.name || session.id.slice(0, 6)} 会话已结束` });
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
