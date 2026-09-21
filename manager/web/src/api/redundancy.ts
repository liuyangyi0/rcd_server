import { request } from './client';

export interface RedundancyPolicy {
  group_id: string; revision: number; enabled: boolean; default_automatic: boolean;
  priority: string[]; failure_seconds: number; healthy_seconds: number; cooldown_seconds: number;
  compare_enabled: boolean; left_source_id: string; right_source_id: string;
  difference_seconds: number; absolute_tolerance: number; relative_tolerance: number; max_skew_ms: number;
}
export interface RedundancyView {
  policy: RedundancyPolicy;
  health: Array<{ source_id: string; healthy: boolean; reason: string; since: string }>;
  routes: Record<string, { source_id: string; revision: number; automatic: boolean }>;
  comparison_state: string; compared_count: number; skipped_count: number; difference_count: number;
  difference_alarm_active?: boolean;
  differences: Array<{ node_id: string; left: unknown; right: unknown; since: string }>;
  evaluated_at?: string; last_switch_at?: string; last_switch_reason: string; last_error: string;
}
export const redundancyKey = (group: string) => ['redundancy', group] as const;
export async function getRedundancy(group: string): Promise<RedundancyView> {
  return await request(`/groups/${encodeURIComponent(group)}/redundancy`) as RedundancyView;
}
export async function saveRedundancy(policy: RedundancyPolicy): Promise<RedundancyPolicy> {
  return await request(`/groups/${encodeURIComponent(policy.group_id)}/redundancy`, { method: 'PUT', headers: { 'If-Match': String(policy.revision) }, body: JSON.stringify(policy) }) as RedundancyPolicy;
}
export async function setSessionAutomatic(group: string, session: string, automatic: boolean, revision: number) {
  return request(`/sessions/${encodeURIComponent(session)}/groups/${encodeURIComponent(group)}/automatic`, { method: 'PUT', headers: { 'If-Match': String(revision) }, body: JSON.stringify({ automatic, revision }) });
}
