const STORAGE_KEY = 'rcd-manager.operator.v1';

export function getOperatorName(): string {
  try {
    return window.localStorage.getItem(STORAGE_KEY)?.trim().slice(0, 64) ?? '';
  } catch {
    return '';
  }
}

export function setOperatorName(value: string): void {
  try {
    const normalized = value.trim().slice(0, 64);
    if (normalized) window.localStorage.setItem(STORAGE_KEY, normalized);
    else window.localStorage.removeItem(STORAGE_KEY);
  } catch {
    // The operator label is advisory audit metadata; storage denial must not
    // prevent management operations.
  }
}
