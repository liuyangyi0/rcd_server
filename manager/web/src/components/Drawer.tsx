import { useEffect, useId, useRef, type ReactNode } from 'react';
import { X } from 'lucide-react';
import { isTopOverlay, popOverlay, pushOverlay } from './overlayStack';

export function Drawer({ open, title, onClose, children, className = '' }: { open: boolean; title: string; onClose: () => void; children: ReactNode; className?: string }) {
  const titleId = useId();
  // A ref keeps the overlay registration stable when callers pass a new onClose each render.
  const closeRef = useRef(onClose);
  closeRef.current = onClose;

  useEffect(() => {
    if (!open) return;
    const token = pushOverlay('drawer');
    const listener = (event: KeyboardEvent) => { if (event.key === 'Escape' && isTopOverlay(token)) closeRef.current(); };
    document.addEventListener('keydown', listener);
    return () => { document.removeEventListener('keydown', listener); popOverlay(token); };
  }, [open]);

  return (
    <aside className={`detail-drawer${open ? ' is-open' : ''} ${className}`} aria-hidden={!open} aria-labelledby={titleId}>
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
