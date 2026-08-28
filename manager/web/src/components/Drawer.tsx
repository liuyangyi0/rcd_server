import { useEffect, useId, type ReactNode } from 'react';
import { X } from 'lucide-react';

export function Drawer({ open, title, onClose, children }: { open: boolean; title: string; onClose: () => void; children: ReactNode }) {
  const titleId = useId();

  useEffect(() => {
    if (!open) return;
    const listener = (event: KeyboardEvent) => event.key === 'Escape' && onClose();
    document.addEventListener('keydown', listener);
    return () => document.removeEventListener('keydown', listener);
  }, [onClose, open]);

  return (
    <aside className={`detail-drawer${open ? ' is-open' : ''}`} aria-hidden={!open} aria-labelledby={titleId}>
      <div className="drawer-header">
        <h2 id={titleId}>{title}</h2>
        <button className="icon-button" type="button" onClick={onClose} aria-label="关闭详情">
          <X aria-hidden="true" size={19} />
        </button>
      </div>
      <div className="drawer-body">{children}</div>
    </aside>
  );
}
