import type { ReactNode } from 'react';

type Tone = 'ok' | 'warn' | 'danger' | 'info' | 'neutral';

const tones: Record<Tone, string> = {
  // signal-green / signal-amber per the token sheet.
  ok: 'bg-signal-green/15 text-signal-green border-signal-green/30',
  warn: 'bg-signal-amber/15 text-signal-amber border-signal-amber/30',
  danger: 'bg-[#ff7b72]/15 text-[#ff7b72] border-[#ff7b72]/30',
  info: 'bg-cloud-blue/15 text-cloud-blue border-cloud-blue/30',
  neutral: 'bg-surface-2 text-fg-muted border-border-2',
};

export function Badge({
  tone = 'neutral',
  children,
  icon,
}: {
  tone?: Tone;
  children: ReactNode;
  icon?: ReactNode;
}) {
  return (
    <span
      className={`inline-flex items-center gap-s-1 rounded-full border px-s-2 py-0.5 text-xs font-medium ${tones[tone]}`}
    >
      {icon}
      {children}
    </span>
  );
}
