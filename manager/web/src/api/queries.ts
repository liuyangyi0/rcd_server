import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api } from './client';
import type { ListParams, RevisionMutation } from './types';

export const queryKeys = {
  overview: ['overview'] as const,
  groups: ['groups'] as const,
  sources: (params: ListParams = {}) => ['sources', params] as const,
  allSources: ['sources'] as const,
  sourceTopology: (sourceId: string) => ['source-topology', sourceId] as const,
  allTopologies: ['source-topology'] as const,
  sessions: ['sessions'] as const,
  points: (params: ListParams) => ['points', params] as const,
  allPoints: ['points'] as const,
  commands: (params: ListParams) => ['commands', params] as const,
  allCommands: ['commands'] as const,
  alarms: (params: ListParams) => ['alarms', params] as const,
  allAlarms: ['alarms'] as const,
  sourceEvents: (params: ListParams) => ['source-events', params] as const,
  allSourceEvents: ['source-events'] as const,
  managementAudit: (params: ListParams) => ['management-audit', params] as const,
  allManagementAudit: ['management-audit'] as const,
  system: ['system'] as const,
};

export function useOverview() {
  return useQuery({ queryKey: queryKeys.overview, queryFn: api.overview, refetchInterval: 30_000 });
}

export function useGroups() {
  return useQuery({ queryKey: queryKeys.groups, queryFn: api.groups, refetchInterval: 30_000 });
}

export function useSources(params: ListParams = {}) {
  return useQuery({ queryKey: queryKeys.sources(params), queryFn: () => api.sources(params), refetchInterval: 30_000 });
}

export function useSessions() {
  return useQuery({ queryKey: queryKeys.sessions, queryFn: api.sessions, refetchInterval: 30_000 });
}

export function useSourceTopology(sourceId: string) {
  return useQuery({ queryKey: queryKeys.sourceTopology(sourceId), queryFn: () => api.sourceTopology(sourceId), enabled: Boolean(sourceId) });
}

export function usePoints(params: ListParams, enabled = true) {
  return useQuery({
    queryKey: queryKeys.points(params),
    queryFn: () => api.points(params),
    placeholderData: (previous) => previous,
    enabled,
  });
}

export function useCommands(params: ListParams) {
  return useQuery({
    queryKey: queryKeys.commands(params),
    queryFn: () => api.commands(params),
    placeholderData: (previous) => previous,
    refetchInterval: 30_000,
  });
}

export function useAlarms(params: ListParams, enabled = true) {
  return useQuery({ queryKey: queryKeys.alarms(params), queryFn: () => api.alarms(params), placeholderData: (previous) => previous, refetchInterval: enabled ? 30_000 : false, enabled });
}

export function useSourceEvents(params: ListParams, enabled = true) {
  return useQuery({ queryKey: queryKeys.sourceEvents(params), queryFn: () => api.sourceEvents(params), placeholderData: (previous) => previous, enabled });
}

export function useManagementAudit(params: ListParams, enabled = true) {
  return useQuery({ queryKey: queryKeys.managementAudit(params), queryFn: () => api.managementAudit(params), placeholderData: (previous) => previous, enabled });
}

export function useSystemInfo() {
  return useQuery({ queryKey: queryKeys.system, queryFn: api.systemInfo, refetchInterval: 30_000 });
}

export function useAcknowledgeAlarm() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: ({ id, revision }: { id: number; revision: number }) => api.acknowledgeAlarm(id, revision),
    onSuccess: async () => Promise.all([
      client.invalidateQueries({ queryKey: queryKeys.allAlarms }),
      client.invalidateQueries({ queryKey: queryKeys.overview }),
      client.invalidateQueries({ queryKey: queryKeys.allManagementAudit }),
    ]),
  });
}

export function useSilenceAlarm() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: ({ id, revision, until }: { id: number; revision: number; until?: string }) => api.silenceAlarm(id, revision, until),
    onSuccess: async () => Promise.all([
      client.invalidateQueries({ queryKey: queryKeys.allAlarms }),
      client.invalidateQueries({ queryKey: queryKeys.overview }),
      client.invalidateQueries({ queryKey: queryKeys.allManagementAudit }),
    ]),
  });
}

export function useSelectSessionSource() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: ({ sessionId, groupId, input }: { sessionId: string; groupId: string; input: RevisionMutation }) =>
      api.selectSessionSource(sessionId, groupId, input),
    onSuccess: async () => {
      await Promise.all([
        client.invalidateQueries({ queryKey: queryKeys.sessions }),
        client.invalidateQueries({ queryKey: queryKeys.groups }),
        client.invalidateQueries({ queryKey: queryKeys.overview }),
        client.invalidateQueries({ queryKey: queryKeys.allPoints }),
      ]);
    },
  });
}

export function useSetDefaultSource() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: ({ groupId, input }: { groupId: string; input: RevisionMutation }) => api.setDefaultSource(groupId, input),
    onSuccess: async () => {
      await Promise.all([
        client.invalidateQueries({ queryKey: queryKeys.groups }),
        client.invalidateQueries({ queryKey: queryKeys.overview }),
        client.invalidateQueries({ queryKey: queryKeys.allPoints, refetchType: 'active' }),
      ]);
    },
  });
}
