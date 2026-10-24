import { AlertTriangle, Info } from 'lucide-react';

export function InlineNotice({ tone = 'info', children }: { tone?: 'info' | 'warning' | 'error'; children: React.ReactNode }) {
  const Icon = tone === 'warning' || tone === 'error' ? AlertTriangle : Info;
  return (
    <div className={`inline-notice notice-${tone}`}>
      <Icon aria-hidden="true" size={18} />
      <div>{children}</div>
    </div>
  );
}
