import type { ReactNode } from 'react';
import { AlertCircle, Database, LoaderCircle, RefreshCw } from 'lucide-react';
import { apiErrorMessage } from '../api/client';

export function LoadingState({ label = '正在加载…' }: { label?: string }) {
  return (
    <div className="page-state" role="status">
      <LoaderCircle className="spin" aria-hidden="true" size={24} />
      <p>{label}</p>
    </div>
  );
}

export function ErrorState({ error, onRetry, title = '无法获取数据' }: { error: unknown; onRetry?: () => void; title?: string }) {
  return (
    <div className="page-state page-state-error" role="alert">
      <AlertCircle aria-hidden="true" size={28} />
      <strong>{title}</strong>
      <p>{apiErrorMessage(error)}</p>
      {onRetry ? (
        <button className="button button-secondary" type="button" onClick={onRetry}>
          <RefreshCw aria-hidden="true" size={16} />
          重试
        </button>
      ) : null}
    </div>
  );
}

export function EmptyState({ title = '暂无数据', description, action }: { title?: string; description?: string; action?: ReactNode }) {
  return (
    <div className="page-state page-state-empty">
      <Database aria-hidden="true" size={28} />
      <strong>{title}</strong>
      {description ? <p>{description}</p> : null}
      {action}
    </div>
  );
}
