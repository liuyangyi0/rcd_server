import { Circle } from 'lucide-react';
import type { HealthState } from '../api/types';
import { statusLabel } from '../lib/status';

interface StatusBadgeProps {
  state: HealthState;
  label?: string;
  compact?: boolean;
}

export function StatusBadge({ state, label, compact = false }: StatusBadgeProps) {
  return (
    <span className={`status-badge status-${state}${compact ? ' status-compact' : ''}`}>
      <Circle aria-hidden="true" size={compact ? 8 : 10} fill="currentColor" strokeWidth={0} />
      <span>{label ?? statusLabel(state)}</span>
    </span>
  );
}
