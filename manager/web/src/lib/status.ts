import type { HealthState, Source } from '../api/types';

export function sourceHealth(source?: Source): HealthState {
  if (!source) return 'unknown';
  const connection = source.connectionState.toLowerCase();
	const session = source.sessionState.toLowerCase();
	const subscription = source.subscriptionState.toLowerCase();
  const catalog = source.catalogState.toLowerCase();
  const lastError = source.lastError?.toLowerCase() ?? '';
  if (lastError.startsWith('bad') || lastError.includes('communication')) return 'bad';
  if (connection.includes('connected') && !connection.includes('disconnected') && isActiveState(session) && isActiveState(subscription) && ['ready', 'valid', 'complete'].some((part) => catalog.includes(part))) {
    return 'good';
  }
  if (connection.includes('connecting') || connection.includes('reconnect') || subscription.includes('creating')) return 'warning';
  return 'bad';
}

export function isActiveState(value: string): boolean {
  const normalized = value.trim().toLowerCase();
  return normalized === 'active' || normalized === 'running';
}

export function statusLabel(state: HealthState): string {
  if (state === 'good') return '正常';
  if (state === 'bad') return '异常';
  if (state === 'warning') return '警告';
  return '未知';
}

export function isBadStatus(code: string): boolean {
  const normalized = code.trim().toLowerCase();
  if (normalized.startsWith('bad') || ['failed', 'error'].some((part) => normalized.includes(part))) return true;
  // The backend's operational quality totals accept only severity 00 as Good.
  // Keep the point row consistent: Uncertain (01), Bad (10), and reserved (11)
  // must never be rendered in Good green.
  if (/^0x[0-9a-f]{8}$/.test(normalized)) {
    return (Number.parseInt(normalized.slice(2), 16) & 0xc0000000) !== 0;
  }
  return false;
}

export function isGoodStatus(code: string): boolean {
  const normalized = code.trim().toLowerCase();
  if (normalized === 'good' || normalized.startsWith('good')) return true;
  if (/^0x[0-9a-f]{8}$/.test(normalized)) {
    return (Number.parseInt(normalized.slice(2), 16) & 0xc0000000) === 0;
  }
  return false;
}
