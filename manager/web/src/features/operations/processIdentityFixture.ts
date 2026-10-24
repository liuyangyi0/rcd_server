import type { ProcessAgentIdentity } from '../../api/process';

// Deterministic UI/API test fixture, never a deployment identity.
export const agentIdentityFixture: ProcessAgentIdentity = {
  build: { schema_version: 1, component: 'agent', version: '0.6.1-test', commit: 'a'.repeat(40), built_at: '2026-09-28T00:00:00Z', release_id: 'isolated-agent-build', source_sha256: 'b'.repeat(64), dirty: true, target: 'linux/amd64' },
  boot_id: 'isolated-agent-boot', started_at: '2026-09-28T01:00:00Z', reported_at: '2026-09-28T02:00:00Z',
};
