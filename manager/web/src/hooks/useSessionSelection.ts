import { useState } from 'react';
import type { UaSession } from '../api/types';

/** Capture the first available Session once; later changes must be explicit. */
export function useSessionSelection(sessions: UaSession[]) {
  const [selectedId, setSessionId] = useState<string | undefined>(() => sessions[0]?.id);
  const firstId = sessions[0]?.id;
  if (selectedId === undefined && firstId !== undefined) {
    // Queries may finish after the page mounts. Capture that initial choice
    // before rendering children, not a fallback that follows list reordering.
    setSessionId(firstId);
  }
  const sessionId = selectedId ?? firstId ?? '';
  return { sessionId, setSessionId, session: sessions.find((item) => item.id === sessionId) };
}
