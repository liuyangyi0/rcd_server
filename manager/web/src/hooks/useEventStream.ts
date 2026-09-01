import { useEffect, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { API_BASE, isMockMode } from '../api/client';
import { queryKeys } from '../api/queries';
import type { EventEnvelope } from '../api/types';

export type StreamState = 'connecting' | 'connected' | 'disconnected' | 'mock';

const EVENT_NAMES = ['source.status', 'source.sample', 'source.event', 'source.operation', 'station_versions_changed', 'lower_logs_changed', 'calculation_config_changed', 'calculation_results_changed', 'group.changed', 'selection.changed', 'session.changed', 'command.changed', 'catalog.changed', 'alarm.changed', 'management.audit', 'configuration.changed', 'resync'];

export function eventInvalidatesPoints(type: string): boolean {
  return type.includes('sample') || type.includes('catalog') || type.includes('selection') ||
    type === 'source.status' || type === 'group.changed' || type === 'session.changed';
}

export function eventInvalidatesSourceEvents(type: string): boolean {
  return type === 'source.event';
}

export function eventInvalidatesManagementAudit(type: string): boolean {
  return type === 'management.audit';
}

export function eventInvalidatesOperations(type: string, key: readonly unknown[]): boolean {
  if (key[0] !== 'source-operations') return false;
  return type === 'source.operation' || type === 'source.status' || (type === 'station_versions_changed' && key[3] === 'status') || (type === 'command.changed' && key[3] === 'status');
}

export function useEventStream(): StreamState {
  const queryClient = useQueryClient();
  const [state, setState] = useState<StreamState>(isMockMode ? 'mock' : 'connecting');

  useEffect(() => {
    if (isMockMode) return;
    const source = new EventSource(`${API_BASE}/events`, { withCredentials: true });

    const invalidate = (event: MessageEvent<string>) => {
      let envelope: EventEnvelope = { type: event.type || 'message' };
      try {
        const raw = JSON.parse(event.data) as EventEnvelope & { source_id?: string; group_id?: string; session_id?: string; data?: { source_id?: unknown } };
        const nestedSource = typeof raw.data?.source_id === 'string' ? raw.data.source_id : undefined;
        envelope = { ...envelope, ...raw, sourceId: raw.sourceId ?? raw.source_id ?? nestedSource, groupId: raw.groupId ?? raw.group_id, sessionId: raw.sessionId ?? raw.session_id };
      } catch {
        // Event name is still enough to choose the affected query families.
      }

      const type = envelope.type || event.type;
      if (type === 'resync') {
        void queryClient.invalidateQueries();
        return;
      }
      void queryClient.invalidateQueries({ queryKey: queryKeys.overview });
      if (type === 'source.status' || type.includes('catalog')) {
        void queryClient.invalidateQueries({ queryKey: queryKeys.allSources });
        void queryClient.invalidateQueries({ queryKey: queryKeys.groups });
        void queryClient.invalidateQueries({ queryKey: queryKeys.allAlarms });
      }
      if (type === 'source.operation' || type === 'source.status' || type === 'station_versions_changed' || type === 'command.changed') {
        // Keep the maintenance feature lazy: only its key prefix is needed.
        // Never fan sample traffic into config/log reads. Command events affect
        // the physical port lock, not file contents, and refresh status only.
        void queryClient.invalidateQueries({
          queryKey: envelope.sourceId ? ['source-operations', envelope.sourceId] : ['source-operations'],
          predicate: (query) => eventInvalidatesOperations(type, query.queryKey),
          refetchType: 'active',
        });
      }
      if (type === 'station_versions_changed' || type === 'source.status') {
        // Connection changes alter source_online/cached/stale and whether a
        // physical version operation is currently reachable, even when the
        // station revision itself does not change.
        void queryClient.invalidateQueries({ queryKey: queryKeys.allDeviceVersions, refetchType: 'active' });
      }
      if (type === 'lower_logs_changed') {
        void queryClient.invalidateQueries({ queryKey: envelope.sourceId ? ['lower-logs', envelope.sourceId] : ['lower-logs'], refetchType: 'active' });
      }
	  if (type === 'calculation_config_changed') {
		void queryClient.invalidateQueries({ queryKey: ['calculations'] });
		void queryClient.invalidateQueries({ queryKey: queryKeys.allPoints, refetchType: 'active' });
	  } else if (type === 'calculation_results_changed') {
		// Results are server-paged. Invalidate only active result/status views;
		// never copy thousands of values into a global event payload or store.
		void queryClient.invalidateQueries({ queryKey: ['calculations', 'status'], refetchType: 'active' });
		void queryClient.invalidateQueries({ queryKey: ['calculations', 'results'], refetchType: 'active' });
	  }
      if (type.includes('catalog')) {
        void queryClient.invalidateQueries({ queryKey: envelope.sourceId ? queryKeys.sourceTopology(envelope.sourceId) : queryKeys.allTopologies });
      }
      if (eventInvalidatesSourceEvents(type)) void queryClient.invalidateQueries({ queryKey: queryKeys.allSourceEvents });
      // Connection state changes alter every affected point's synthesized
      // quality (for example Good -> BadNoCommunication), while a group change
      // can replace the default route. Both must refresh the active point view
      // even when no further sample arrives after a disconnect. Session close
      // also invalidates its cached point view instead of waiting for samples.
      if (eventInvalidatesPoints(type)) {
        void queryClient.invalidateQueries({ queryKey: queryKeys.allPoints, refetchType: 'active' });
      }
      if (type.includes('group') || type.includes('selection')) {
        void queryClient.invalidateQueries({ queryKey: queryKeys.groups });
      }
      if (type === 'group.changed') {
        // Membership includes device mappings; group changes refresh the
        // source list without assuming that physical sources are deleted.
        void queryClient.invalidateQueries({ queryKey: queryKeys.allSources });
        void queryClient.invalidateQueries({ queryKey: queryKeys.allTopologies });
      }
      if (type.includes('session') || type.includes('selection')) {
        void queryClient.invalidateQueries({ queryKey: queryKeys.sessions });
      }
      if (type.includes('command')) {
        void queryClient.invalidateQueries({ queryKey: queryKeys.allCommands, refetchType: 'active' });
        // Pending commands lock both defaults and Session routes immediately.
        void queryClient.invalidateQueries({ queryKey: queryKeys.groups });
      }
      if (type.includes('alarm')) {
        void queryClient.invalidateQueries({ queryKey: queryKeys.allAlarms });
      }
      if (eventInvalidatesManagementAudit(type)) {
        void queryClient.invalidateQueries({ queryKey: queryKeys.allManagementAudit, refetchType: 'active' });
      }
      if (type.includes('configuration')) {
        void queryClient.invalidateQueries();
      }
    };

    source.onopen = () => {
      setState('connected');
      // EventSource has no replay buffer. A full refresh on every successful
      // (re)connect closes the gap for changes that happened while offline.
      void queryClient.invalidateQueries();
    };
    source.onerror = () => setState('disconnected');
    source.onmessage = invalidate;
    for (const name of EVENT_NAMES) source.addEventListener(name, invalidate as EventListener);

    return () => {
      for (const name of EVENT_NAMES) source.removeEventListener(name, invalidate as EventListener);
      source.close();
    };
  }, [queryClient]);

  return state;
}
