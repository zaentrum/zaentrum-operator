import type { ReactNode } from 'react';
import { AlertTriangle, Inbox, Loader2, RefreshCw } from 'lucide-react';
import { Button } from './Button';

/** Centered spinner for loading regions. */
export function Loading({ label = 'Loading…' }: { label?: string }) {
  return (
    <div className="flex items-center justify-center gap-s-2 py-s-8 text-fg-muted">
      <Loader2 size={18} className="animate-spin text-cloud-blue" />
      <span className="text-sm">{label}</span>
    </div>
  );
}

/** Empty state with an icon, message, and optional action. */
export function Empty({
  title,
  description,
  icon,
  action,
}: {
  title: string;
  description?: ReactNode;
  icon?: ReactNode;
  action?: ReactNode;
}) {
  return (
    <div className="flex flex-col items-center justify-center gap-s-3 rounded-lg border border-dashed border-border-2 px-s-5 py-s-8 text-center">
      <span className="text-fg-dim">{icon ?? <Inbox size={28} />}</span>
      <div>
        <p className="font-medium text-fg">{title}</p>
        {description ? <p className="mt-s-1 text-sm text-fg-muted">{description}</p> : null}
      </div>
      {action}
    </div>
  );
}

/** Error state with a retry affordance. */
export function ErrorState({
  message,
  onRetry,
}: {
  message: string;
  onRetry?: () => void;
}) {
  return (
    <div className="flex flex-col items-center justify-center gap-s-3 rounded-lg border border-[#ff7b72]/30 bg-[#ff7b72]/5 px-s-5 py-s-8 text-center">
      <AlertTriangle size={26} className="text-[#ff7b72]" />
      <p className="max-w-md text-sm text-fg-2">{message}</p>
      {onRetry ? (
        <Button variant="secondary" size="sm" icon={<RefreshCw size={14} />} onClick={onRetry}>
          Retry
        </Button>
      ) : null}
    </div>
  );
}
