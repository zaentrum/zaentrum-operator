import { useEffect, useState } from 'react';
import { Loader2 } from 'lucide-react';

/**
 * Friendly loading messages reused across the app. Most loads finish
 * before the first message rotation, so the funny one is just a soft
 * brand touch when the network is slow. Ordered roughly easy→cute so
 * the first one a user sees on a fast pipe doesn't feel like a
 * desperate joke.
 */
export const LOADING_MESSAGES: string[] = [
  'Polishing the popcorn…',
  'Rounding up the cast…',
  'Untangling reel tape…',
  'Buffering atmosphere…',
  'Dimming the house lights…',
  'Cataloging your shelves…',
  'Asking the projectionist nicely…',
  'Whispering to the codec gods…',
  'Re-aligning the satellite dish…',
  'Mood-lighting your living room…',
  'Cueing the orchestra…',
  'Almost there, just one more reel…',
];

interface LoadingStateProps {
  /** Optional override; otherwise rotates through LOADING_MESSAGES. */
  message?: string;
  /** Compact = inline single line; full = centred block (for first-paint). */
  variant?: 'inline' | 'full';
  /** Rotation interval in ms. */
  intervalMs?: number;
}

export function LoadingState({ message, variant = 'inline', intervalMs = 2200 }: LoadingStateProps) {
  const [idx, setIdx] = useState(0);
  useEffect(() => {
    if (message) return;
    const t = window.setInterval(() => setIdx((i) => (i + 1) % LOADING_MESSAGES.length), intervalMs);
    return () => window.clearInterval(t);
  }, [message, intervalMs]);

  const text = message ?? LOADING_MESSAGES[idx];

  if (variant === 'full') {
    return (
      <div className="min-h-[40vh] flex flex-col items-center justify-center gap-3 text-[#8b949e]">
        <Loader2 className="w-8 h-8 animate-spin text-[#58a6ff]" />
        <p className="text-sm italic">{text}</p>
      </div>
    );
  }
  return (
    <div className="flex items-center gap-2 text-sm text-[#8b949e]">
      <Loader2 className="w-4 h-4 animate-spin text-[#58a6ff]" />
      <span className="italic">{text}</span>
    </div>
  );
}
