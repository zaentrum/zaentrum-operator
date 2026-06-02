import { useEffect } from 'react';
import type { ReactNode } from 'react';
import { X } from 'lucide-react';

interface ModalProps {
  open: boolean;
  title: ReactNode;
  /** Optional sub-line under the title. */
  description?: ReactNode;
  icon?: ReactNode;
  onClose: () => void;
  children: ReactNode;
  /** Footer actions, rendered right-aligned below the body. */
  footer?: ReactNode;
}

/**
 * Centered dialog over a dimmed backdrop. Closes on Escape and on backdrop
 * click. Used by the Users page for the add / edit / reset-password forms.
 */
export function Modal({ open, title, description, icon, onClose, children, footer }: ModalProps) {
  useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose();
    };
    document.addEventListener('keydown', onKey);
    return () => document.removeEventListener('keydown', onKey);
  }, [open, onClose]);

  if (!open) return null;

  return (
    <div
      className="fixed inset-0 z-50 flex items-start justify-center overflow-y-auto bg-bg/70 px-s-4 py-s-7 backdrop-blur-sm"
      role="presentation"
      onMouseDown={(e) => {
        if (e.target === e.currentTarget) onClose();
      }}
    >
      <div
        role="dialog"
        aria-modal="true"
        className="w-full max-w-lg rounded-lg border border-border bg-surface shadow-xl"
        onMouseDown={(e) => e.stopPropagation()}
      >
        <div className="flex items-start justify-between gap-s-4 border-b border-border px-s-5 py-s-4">
          <div className="flex items-start gap-s-3">
            {icon ? (
              <span className="mt-px flex h-9 w-9 shrink-0 items-center justify-center rounded-md bg-surface-2 text-cloud-blue">
                {icon}
              </span>
            ) : null}
            <div>
              <h2 className="font-ui text-base font-semibold text-fg">{title}</h2>
              {description ? (
                <p className="mt-0.5 text-sm text-fg-muted">{description}</p>
              ) : null}
            </div>
          </div>
          <button
            type="button"
            onClick={onClose}
            aria-label="Close"
            className="focus-ring -mr-s-1 -mt-s-1 rounded-md p-s-1 text-fg-muted hover:bg-surface-2 hover:text-fg"
          >
            <X size={18} />
          </button>
        </div>

        <div className="px-s-5 py-s-5">{children}</div>

        {footer ? (
          <div className="flex items-center justify-end gap-s-2 border-t border-border px-s-5 py-s-4">
            {footer}
          </div>
        ) : null}
      </div>
    </div>
  );
}
