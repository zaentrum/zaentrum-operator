import type { ReactNode } from 'react';

/** A single metric tile: big value, small label, optional icon + trend note. */
export function Stat({
  label,
  value,
  icon,
  note,
}: {
  label: string;
  value: ReactNode;
  icon?: ReactNode;
  note?: ReactNode;
}) {
  return (
    <div className="rounded-lg border border-border bg-surface px-s-4 py-s-4">
      <div className="flex items-center justify-between">
        <span className="text-sm text-fg-muted">{label}</span>
        {icon ? <span className="text-fg-dim">{icon}</span> : null}
      </div>
      <div className="mt-s-2 font-mono text-2xl font-bold text-fg">{value}</div>
      {note ? <div className="mt-s-1 text-xs text-fg-dim">{note}</div> : null}
    </div>
  );
}
