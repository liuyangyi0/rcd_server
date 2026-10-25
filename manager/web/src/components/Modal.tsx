import { useEffect, useId, useRef, type ReactNode } from 'react';
import { createPortal } from 'react-dom';
import { X } from 'lucide-react';
import { isTopOverlay, popOverlay, pushOverlay } from './overlayStack';

interface ModalProps {
  open: boolean;
  title: string;
  description?: string;
  onClose: () => void;
  children: ReactNode;
  footer?: ReactNode;
  width?: 'small' | 'medium' | 'large';
}

export function Modal({ open, title, description, onClose, children, footer, width = 'medium' }: ModalProps) {
  const titleId = useId();
  const panelRef = useRef<HTMLDivElement>(null);
  const closeRef = useRef(onClose);
  closeRef.current = onClose;

  useEffect(() => {
    if (!open) return;
    const previous = document.activeElement as HTMLElement | null;
    const token = pushOverlay('dialog');
    const handleKeyDown = (event: KeyboardEvent) => {
      if (!isTopOverlay(token)) return;
      if (event.key === 'Escape') closeRef.current();
      if (event.key !== 'Tab' || !panelRef.current) return;
      const panel = panelRef.current;
      const focusable = [...panel.querySelectorAll<HTMLElement>('button, input:not([type="hidden"]), select, textarea, a[href], [tabindex]')]
        .filter((element) => element.tabIndex >= 0 && !element.matches(':disabled')
          && !element.closest('[hidden], [inert], [aria-hidden="true"]')
          && window.getComputedStyle(element).display !== 'none'
          && window.getComputedStyle(element).visibility !== 'hidden');
      if (focusable.length === 0) { event.preventDefault(); panel.focus(); return; }
      const first = focusable[0];
      const last = focusable[focusable.length - 1];
      if (event.shiftKey && (document.activeElement === first || !panel.contains(document.activeElement) || document.activeElement === panel)) {
        event.preventDefault();
        last.focus();
      } else if (!event.shiftKey && (document.activeElement === last || !panel.contains(document.activeElement) || document.activeElement === panel)) {
        event.preventDefault();
        first.focus();
      }
    };
    document.addEventListener('keydown', handleKeyDown);
    const frame = window.requestAnimationFrame(() => {
      const panel = panelRef.current;
      if (panel && !panel.contains(document.activeElement)) panel.focus();
    });
    return () => {
      window.cancelAnimationFrame(frame);
      document.removeEventListener('keydown', handleKeyDown);
      popOverlay(token);
      if (previous?.isConnected) previous.focus();
    };
  }, [open]);

  if (!open) return null;
  return createPortal(
    <div className="overlay" onMouseDown={(event) => event.target === event.currentTarget && onClose()}>
      <div ref={panelRef} className={`modal modal-${width}`} role="dialog" aria-modal="true" aria-labelledby={titleId} tabIndex={-1}>
        <div className="modal-header">
          <div>
            <h2 id={titleId}>{title}</h2>
            {description ? <p>{description}</p> : null}
          </div>
          <button className="icon-button" type="button" onClick={onClose} aria-label="关闭弹窗">
            <X aria-hidden="true" size={20} />
          </button>
        </div>
        <div className="modal-body">{children}</div>
        {footer ? <div className="modal-footer">{footer}</div> : null}
      </div>
    </div>,
    document.body,
  );
}
