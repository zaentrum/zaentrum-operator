import { useCallback, useEffect, useRef, useState } from 'react';
import { Loader2, Zap as ZapIcon } from 'lucide-react';
import { useAuth } from 'react-oidc-context';
import { useStreamToken } from '../../hooks/useStreamToken';
import { useZapFeed } from '../../hooks/useZapFeed';
import { useZapPreferences } from '../../hooks/useZapPreferences';
import { useZapTelemetry } from '../../hooks/useZapTelemetry';
import { ZapCard } from '../zap/ZapCard';
import type { KatalogItem } from '../../hooks/useItems';
import type { ZapFeatures } from '../zap/ZapCard';

/** Caps + quality params to advertise to the prewarm endpoint. Must
 *  match ZapCard's actual play URL so the warm primes the right
 *  pipeline (caps decide passthrough vs transcode, quality picks the
 *  rung). Keeping these in sync is critical — a mismatched warm just
 *  burns ffmpeg cycles on a window the real player will never fetch. */
const PREWARM_CAPS = 'avc,hvc,aac,opus,mp3';
const PREWARM_DEBOUNCE_MS = 500;

/** Signal strengths chosen so a single save outweighs ~2 fast skips and
 *  ~4 short dwells. Calibrated by hand; V2 should learn these. */
const SIGNAL = {
  COMPLETE: 1.0,
  DWELL_LONG: 0.5,
  EXPAND: 1.5,
  SAVE: 2.0,
  SKIP_FAST: -1.0,
  SKIP_NORMAL: -0.3,
} as const;

/** Boundaries (ms) that split a zap exit into fast-skip / skip / dwell.
 *  <2 s = the user didn't even look; 2-8 s = passed without engaging;
 *  ≥8 s = genuine attention. */
const FAST_SKIP_MS = 2000;
const DWELL_MS = 8000;

/**
 * Fullscreen vertical pager — the Zap mode itself.
 *
 * Layout: a `h-full w-full overflow-y-auto snap-y snap-mandatory`
 * container, one ZapCard per child stacked vertically. The active
 * card is whichever one has its top within the top 50 % of the
 * viewport (detected via IntersectionObserver). On every advance we
 * mark the prior card shown and fire signals into the preference
 * vector.
 *
 * V1 deliberately does NOT pre-warm the next card's transcode. An
 * earlier draft fetched the next master.m3u8 with a hard-coded
 * ?t=60, which never matched the real seek the card would later
 * request — every advance spawned two ffmpegs (one wasted warm at
 * t=60, then the real seek). V2 will warm against the next card's
 * actual midpoint, which requires lifting the segments fetch up to
 * this scope; for now we accept the cold start (1-3 s) and rely on
 * the poster overlay during HLS warmup.
 */
export function ZapSection() {
  const auth = useAuth();
  const streamToken = useStreamToken();
  const prefs = useZapPreferences();
  const telemetry = useZapTelemetry();
  const feed = useZapFeed({ scoreItem: (f) => prefs.score(f).normalized });

  // The pager scrolls children into view; we track which one is the
  // "active" card so we only render-and-play one at a time.
  const containerRef = useRef<HTMLDivElement>(null);
  const [activeId, setActiveId] = useState<string | null>(null);

  // When the queue updates, default the active card to the first item.
  useEffect(() => {
    if (!activeId && feed.queue[0]) setActiveId(feed.queue[0].id);
  }, [feed.queue, activeId]);

  // Session-wide impression dedup — keyed by itemId, never reset until
  // unmount. Prevents IntersectionObserver flicker (a card briefly
  // dipping below ratio 0.5 then crossing back as snap-y momentum
  // settles) from double-counting impressions.
  const impressionsFiredRef = useRef<Set<string>>(new Set());

  // Session-wide prewarm dedup — keyed by itemId, never reset until
  // unmount. Each item gets at most one /prewarm POST per Zap
  // session, regardless of how many times it enters distance=1
  // (which can happen if the user scrolls back to a previous card).
  const prewarmedRef = useRef<Set<string>>(new Set());

  // Debounced prewarm: when the active card settles (no further
  // scroll for PREWARM_DEBOUNCE_MS), fire a fire-and-forget POST to
  // /prewarm for the next card in the queue. The server returns 202
  // immediately and warms window 0 in a background goroutine off the
  // dedicated warm-only ffmpeg pool. If the user swipes again before
  // the timer fires we clear it — no DoSing our own pool with 10
  // prewarms in 2 seconds during rapid scrolling.
  useEffect(() => {
    if (!streamToken || !activeId) return;
    const idx = feed.queue.findIndex((it) => it.id === activeId);
    if (idx < 0) return;
    const next = feed.queue[idx + 1];
    if (!next || prewarmedRef.current.has(next.id)) return;
    const token = auth.user?.access_token;
    if (!token) return;
    const id = next.id;
    const tid = window.setTimeout(() => {
      prewarmedRef.current.add(id);
      // q=medium matches ZapCard's pickQuality() default on wifi.
      // Mobile cells will mismatch (card uses q=low) and the warmed
      // medium rung is wasted, but the ffprobe / OS-page-cache /
      // NVENC-context side-effects still benefit either rung — the
      // cold-start win mostly comes from those, not from window 0
      // being the exact rung the card will fetch.
      const url = `/api/v1/items/${encodeURIComponent(id)}/play/prewarm?caps=${encodeURIComponent(PREWARM_CAPS)}&q=medium`;
      fetch(url, {
        method: 'POST',
        headers: { Authorization: `Bearer ${token}` },
        keepalive: true,
      }).catch(() => {
        // Network blip or 404 (older chino-stream without the
        // endpoint) — let it slide. The card still plays, just
        // without the warm.
      });
    }, PREWARM_DEBOUNCE_MS);
    return () => window.clearTimeout(tid);
  }, [activeId, feed.queue, streamToken, auth.user?.access_token]);

  // Session-wide mute state. Lifted out of ZapCard so that unmuting
  // once survives every swipe afterwards — the browser autoplay
  // policy only blocks unmuted autoplay until the user has clicked
  // *something* in the page, and that "something" can absolutely be
  // the unmute banner on card #1. We default to unmuted (channel-zap
  // feel); ZapCard flips this to muted if the browser still rejects
  // play() despite the sidebar-click gesture.
  const [mutedSession, setMutedSession] = useState(false);

  // ---- IntersectionObserver: figure out which card the user is on ----
  useEffect(() => {
    const c = containerRef.current;
    if (!c) return;
    const obs = new IntersectionObserver(
      (entries) => {
        // The card with the largest visible area wins. With
        // snap-mandatory there's almost always exactly one.
        let best: { id: string; ratio: number } | null = null;
        for (const e of entries) {
          const id = (e.target as HTMLElement).dataset.zapId;
          if (!id) continue;
          if (!best || e.intersectionRatio > best.ratio) {
            best = { id, ratio: e.intersectionRatio };
          }
        }
        if (best && best.ratio > 0.5) setActiveId(best.id);
      },
      { root: c, threshold: [0, 0.5, 1] },
    );
    for (const child of Array.from(c.children)) obs.observe(child as Element);
    return () => obs.disconnect();
    // queue.length so a refill re-attaches the observer to new
    // children. Re-observing previously-observed nodes is a no-op so
    // this is safe.
  }, [feed.queue.length]);

  // ---- Signal handlers — passed to every ZapCard ----
  const handleImpression = useCallback<NonNullable<Parameters<typeof ZapCard>[0]['onImpression']>>((info) => {
    if (impressionsFiredRef.current.has(info.itemId)) return;
    impressionsFiredRef.current.add(info.itemId);
    telemetry.report('zap_impression', info.itemId, {
      midSec: info.midSec,
      midSource: info.midSource,
      genres: info.features.genres,
      type: info.features.type,
    });
  }, [telemetry]);

  const handleDwellEnd = useCallback<NonNullable<Parameters<typeof ZapCard>[0]['onDwellEnd']>>((info) => {
    const { dwellMs, features, itemId } = info;
    // Classify the exit and bump the preference vector. Telemetry
    // mirrors the same classification so the dashboards line up.
    let strength = 0;
    let kind: 'zap_skip_fast' | 'zap_skip' | 'zap_dwell';
    if (dwellMs < FAST_SKIP_MS) {
      kind = 'zap_skip_fast';
      strength = SIGNAL.SKIP_FAST;
    } else if (dwellMs < DWELL_MS) {
      kind = 'zap_skip';
      strength = SIGNAL.SKIP_NORMAL;
    } else {
      kind = 'zap_dwell';
      strength = SIGNAL.DWELL_LONG;
    }
    prefs.update(features, strength);
    telemetry.report(kind, itemId, {
      dwellMs,
      genres: features.genres,
      type: features.type,
      vector: prefs.snapshot(),
    });
    // The card we just left has been seen — drop it from the queue
    // and top up if we're running low.
    feed.markShown(itemId);
    feed.refill();
  }, [telemetry, prefs, feed]);

  const handleComplete = useCallback<NonNullable<Parameters<typeof ZapCard>[0]['onComplete']>>((info) => {
    prefs.update(info.features, SIGNAL.COMPLETE);
    telemetry.report('zap_complete', info.itemId, {
      genres: info.features.genres,
      type: info.features.type,
    });
  }, [prefs, telemetry]);

  const handleExpand = useCallback<NonNullable<Parameters<typeof ZapCard>[0]['onExpand']>>((info) => {
    prefs.update(info.features, SIGNAL.EXPAND);
    telemetry.report('zap_expand', info.itemId, {
      resumeSec: info.resumeSec,
      genres: info.features.genres,
      type: info.features.type,
    });
    // Hand off to the full PlayerPage at the current playhead — the
    // wall-clock position is midSec + currentTime, encoded as ?resume.
    window.location.assign(`/player/${encodeURIComponent(info.itemId)}?resume=${Math.floor(info.resumeSec)}`);
  }, [prefs, telemetry]);

  const handleSaveToggle = useCallback<NonNullable<Parameters<typeof ZapCard>[0]['onSaveToggle']>>((info) => {
    if (info.saved) {
      prefs.update(info.features, SIGNAL.SAVE);
    }
    telemetry.report('zap_save', info.itemId, {
      saved: info.saved,
      genres: info.features.genres,
      type: info.features.type,
    });
  }, [prefs, telemetry]);

  // ---- Empty / loading states ----
  if (!auth.isAuthenticated) {
    return <EmptyState title="Sign in to start zapping" />;
  }
  if (feed.loading && feed.queue.length === 0) {
    return (
      <div className="fixed inset-0 bg-black text-white flex items-center justify-center">
        <Loader2 className="w-8 h-8 animate-spin" />
      </div>
    );
  }
  if (feed.empty) {
    return <EmptyState title="Nothing to zap right now" subtitle="Add some movies or shows to the catalogue, or come back after the next ingest cycle." />;
  }

  // Visible-set: render the current card + its immediate neighbours.
  // Anything further away is just a placeholder div so the scroll
  // container has the right total height (keeps scroll-snap honest)
  // without spinning up dozens of hls.js instances.
  const activeIndex = Math.max(0, feed.queue.findIndex((it) => it.id === activeId));

  return (
    <ZapPagerShell containerRef={containerRef}>
      {feed.queue.map((item, i) => {
        const distance = Math.abs(i - activeIndex);
        const visible = distance <= 1;
        const isActive = item.id === activeId;
        return (
          <ZapSlot key={item.id} item={item}>
            {visible ? (
              <ZapCard
                item={item}
                active={isActive}
                muted={mutedSession}
                setMuted={setMutedSession}
                onImpression={handleImpression}
                onDwellEnd={handleDwellEnd}
                onComplete={handleComplete}
                onExpand={handleExpand}
                onSaveToggle={handleSaveToggle}
              />
            ) : (
              <ZapPlaceholder item={item} />
            )}
          </ZapSlot>
        );
      })}
    </ZapPagerShell>
  );
}

interface ShellProps {
  containerRef: React.RefObject<HTMLDivElement>;
  children: React.ReactNode;
}

function ZapPagerShell({ containerRef, children }: ShellProps) {
  return (
    // The Zap pager bleeds outside ChinoApp's `p-4` content padding —
    // the cards want the full viewport. -m-4 cancels the parent's
    // padding, h-[calc(100dvh-4rem)] subtracts the Header's h-16.
    // dvh handles iOS Safari's address-bar collapse.
    <div className="-m-4">
      <div
        ref={containerRef}
        className="h-[calc(100dvh-4rem)] md:h-[calc(100dvh-4rem)] overflow-y-auto snap-y snap-mandatory bg-black [scrollbar-width:none] [&::-webkit-scrollbar]:hidden"
      >
        {children}
      </div>
    </div>
  );
}

interface SlotProps {
  item: KatalogItem;
  children: React.ReactNode;
}

function ZapSlot({ item, children }: SlotProps) {
  return (
    // h-full + snap-start so the IntersectionObserver in the parent
    // can pick out which child is the "current" card. The data-zap-id
    // is how the observer maps DOM nodes back to queue ids.
    <div data-zap-id={item.id} className="h-full w-full snap-start">
      {children}
    </div>
  );
}

function ZapPlaceholder({ item }: { item: KatalogItem }) {
  return (
    <div className="relative w-full h-full bg-black overflow-hidden">
      {item.backdrop_url || item.poster_url ? (
        <img
          src={item.backdrop_url || item.poster_url || ''}
          alt=""
          aria-hidden
          className="absolute inset-0 w-full h-full object-cover opacity-30"
          loading="lazy"
        />
      ) : null}
    </div>
  );
}

function EmptyState({ title, subtitle }: { title: string; subtitle?: string }) {
  return (
    <div className="fixed inset-0 bg-black text-white flex flex-col items-center justify-center px-6 text-center">
      <ZapIcon className="w-12 h-12 text-[#58a6ff] mb-4" />
      <h2 className="text-xl font-semibold">{title}</h2>
      {subtitle ? <p className="mt-2 text-[#8b949e] max-w-md">{subtitle}</p> : null}
    </div>
  );
}

// Re-export so consumers don't have to reach into the component file.
export type { ZapFeatures };
