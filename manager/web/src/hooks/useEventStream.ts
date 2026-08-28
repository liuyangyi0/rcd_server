import { useEffect, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { API_BASE, isMockMode } from '../api/client';
import { queryKeys } from '../api/queries';
import type { EventEnvelope } from '../api/types';

export type StreamState = 'connecting' | 'connected' | 'disconnected' | 'mock';

const EVENT_NAMES = ['source.status', 'source.sample', 'source.event', 'group.changed', 'selection.changed', 'session.changed', 'command.changed', 'catalog.changed', 'alarm.changed', 'management.audit', 'configuration.changed', 'resync'];

export function eventInvalidatesPoints(type: string): boolean {
  return type.includes('sample') || type.includes('catalog') || type.includes('selection') ||
    type === 'source.status' || type === 'group.changed';
}

export function eventInvalidatesSourceEvents(type: string): boolean {
  return type === 'source.event';
}

export function eventInvalidatesManagementAudit(type: string): boolean {
  return type === 'management.audit';
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
        envelope = { ...envelope, ...(JSON.parse(event.data) as EventEnvelope) };
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
      if (eventInvalidatesSourceEvents(type)) void queryClient.invalidateQueries({ queryKey: queryKeys.allSourceEvents });
      // Connection state changes alter every affected point's synthesized
      // quality (for example Good -> BadNoCommunication), while a group change
      // can replace the default route. Both must refresh the active point view
      // even when no further sample arrives after a disconnect.
      if (eventInvalidatesPoints(type)) {
        void queryClient.invalidateQueries({ queryKey: queryKeys.allPoints, refetchType: 'active' });
      }
      if (type.includes('group') || type.includes('selection')) {
        void queryClient.invalidateQueries({ queryKey: queryKeys.groups });
      }
      if (type === 'group.changed') {
        // Group deletion cascades its sources in the backend.
        void queryClient.invalidateQueries({ queryKey: queryKeys.allSources });
      }
      if (type.includes('session') || type.includes('selection')) {
        void queryClient.invalidateQueries({ queryKey: queryKeys.sessions });
      }
      if (type.includes('command')) {
        void queryClient.invalidateQueries({ queryKey: queryKeys.allCommands, refetchType: 'active' });
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
