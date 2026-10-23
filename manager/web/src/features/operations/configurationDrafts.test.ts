import { describe, expect, it } from 'vitest';
import type { ConfigDocument } from '../../api/operations';
import { discardConfigurationDrafts, readConfigurationDraft, rememberConfigurationDraft, rememberedConfigurationFile } from './configurationDrafts';

const baseline: ConfigDocument = { source_id: 'source-a', source_revision: 3, name: 'serial.csv', kind: 'csv', content: 'original', revision: 'file-1', active_revision: 'file-1', restart_required: false, size_bytes: 8, backups: [] };
const draft = { baseline, content: 'unsaved', loadedBackup: '', parameterEdits: { retry_count: '5' } };
const target = { id: 'source-a', revision: 3 };

describe('temporary lower configuration recovery', () => {
  it('isolates source, revision, file and query client instead of reusing an edit for another target', () => {
    const client = {}; rememberConfigurationDraft(client, target, draft);
    expect(readConfigurationDraft(client, target, 'serial.csv')).toBe(draft);
    expect(rememberedConfigurationFile(client, target)).toBe('serial.csv');
    expect(readConfigurationDraft({}, target, 'serial.csv')).toBeUndefined();
    expect(readConfigurationDraft(client, { ...target, revision: 4 }, 'serial.csv')).toBeUndefined();
    expect(readConfigurationDraft(client, { id: 'source-b', revision: 3 }, 'serial.csv')).toBeUndefined();
    expect(readConfigurationDraft(client, target, 'config.toml')).toBeUndefined();
  });
  it('clears an explicitly discarded target and clears all recovery data at logout', () => {
    const client = {}; rememberConfigurationDraft(client, target, draft);
    discardConfigurationDrafts(client, target);
    expect(readConfigurationDraft(client, target, 'serial.csv')).toBeUndefined();
    rememberConfigurationDraft(client, target, draft);
    window.dispatchEvent(new Event('rcd-auth-cleared'));
    expect(readConfigurationDraft(client, target, 'serial.csv')).toBeUndefined();
  });
});
