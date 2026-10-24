import type { ConfigDocument, OperationsTarget } from '../../api/operations';

export interface ConfigurationDraft {
  baseline: ConfigDocument;
  content: string;
  loadedBackup: string;
  parameterEdits: Record<string, string>;
}

// In-memory only: no credentials or configuration are written to browser
// storage. QueryClient and target revision isolate managers, login sessions,
// and source identities. A route unmount is not an explicit discard.
let clients = new WeakMap<object, Map<string, ConfigurationDraft>>();
const key = (target: OperationsTarget, name: string) => JSON.stringify([target.id, target.revision, name]);

export function readConfigurationDraft(client: object, target: OperationsTarget, name: string): ConfigurationDraft | undefined {
  return clients.get(client)?.get(key(target, name));
}

export function rememberedConfigurationFile(client: object, target: OperationsTarget): string | undefined {
  return [...(clients.get(client)?.values() ?? [])].find((draft) =>
    draft.baseline.source_id === target.id && draft.baseline.source_revision === target.revision)?.baseline.name;
}

export function rememberConfigurationDraft(client: object, target: OperationsTarget, draft: ConfigurationDraft): void {
  if (draft.baseline.source_id !== target.id || draft.baseline.source_revision !== target.revision) return;
  let values = clients.get(client);
  if (!values) { values = new Map(); clients.set(client, values); }
  values.set(key(target, draft.baseline.name), draft);
}

export function forgetConfigurationDraft(client: object, target: OperationsTarget, name: string): void {
  clients.get(client)?.delete(key(target, name));
}

export function discardConfigurationDrafts(client: object, target: OperationsTarget): void {
  const values = clients.get(client);
  if (!values) return;
  for (const [entry, draft] of values) {
    if (draft.baseline.source_id === target.id && draft.baseline.source_revision === target.revision) values.delete(entry);
  }
}

if (typeof window !== 'undefined') {
  window.addEventListener('rcd-auth-cleared', () => { clients = new WeakMap(); });
}
