import { useCallback, useEffect, useMemo, useRef } from 'react';
import { useAuth } from 'react-oidc-context';

// Zap-specific event kinds we POST to the existing /api/v1/play/events
// sink. The server accepts arbitrary `kind` strings and bumps a
// Prometheus counter per kind, so adding new kinds requires no chino-api
// changes. Keep this list bounded — every value becomes a permanent
// label on the chino_player_events_total CounterVec.
export type ZapKind =
  | 'zap_impression'
  | 'zap_dwell'
  | 'zap_skip_fast'
  | 'zap_skip'
  | 'zap_complete'
  | 'zap_expand'
  | 'zap_save'
  | 'zap_mute_toggle'
  | 'zap_session_start'
  | 'zap_session_end';

interface QueuedEvent {
  ts: number;
  kind: ZapKind;
  itemId?: string;
  payload?: Record<string, unknown>;
}

interface UseZapTelemetry {
  report: (kind: ZapKind, itemId: string | undefined, payload?: Record<string, unknown>) => void;
  flushNow: () => void;
}

/**
 * Telemetry queue + flusher for the Zap feature. Mirrors PlayerPage's
 * own batched-events setup: queue locally, POST every 30 s, fetch with
 * keepalive on pagehide. Reuses POST /api/v1/play/events (the server
 * treats `kind` as opaque, so we don't need a new endpoint for V1).
 *
 * sessionId is generated once per mount and reset only on a real
 * unmount, so silent OIDC renews inside a single Zap session keep the
 * same sessionId across many flushes.
 *
 * History (don't reintroduce):
 *   - We previously used sendBeacon on pagehide with `?token=<bearer>`
 *     in the URL. That leaked the OIDC access token into ingress
 *     access logs. fetch() with keepalive:true survives pagehide
 *     across all modern browsers and lets us keep the bearer in the
 *     Authorization header.
 *   - The session lifecycle (start/end) used to live in the same
 *     effect as the flush interval, deps=[flush, report]. Because
 *     flush identity rotates with the OIDC bearer (~5 min), every
 *     silent renew emitted a spurious end/start pair and inflated
 *     Prometheus by an order of magnitude. Now the lifecycle effect
 *     is empty-deps mount-once, and the interval reads `flush`
 *     through a ref so token rotations still get the freshest bearer
 *     without rebuilding the effect.
 */
export function useZapTelemetry(): UseZapTelemetry {
  const auth = useAuth();
  const token = auth.user?.access_token;
  const sessionIdRef = useRef<string>(crypto.randomUUID());
  const queueRef = useRef<QueuedEvent[]>([]);

  const flush = useCallback((final: boolean) => {
    // Peek-then-splice: if we can't actually send (no token), leave
    // the queue intact so the next flush after silent-renew completes
    // can carry the events. Previously we spliced first and silently
    // dropped everything on a brief auth gap.
    if (!queueRef.current.length || !token) return;
    const events = queueRef.current.splice(0, queueRef.current.length);
    const body = JSON.stringify({ sessionId: sessionIdRef.current, events });
    // fetch with keepalive:true is the modern replacement for
    // sendBeacon — it carries headers (so the bearer goes in
    // Authorization, not the URL) and the browser still flushes it
    // post-unload. The `final` flag is kept as a hint in case we ever
    // want a different code path, but both branches use the same
    // safe call shape today.
    fetch('/api/v1/play/events', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        Authorization: `Bearer ${token}`,
      },
      body,
      keepalive: true,
    }).catch(() => {
      // Network blip — put the events back so the next flush retries.
      // Only do this for non-final flushes; on `final` the component
      // is unmounting and the next mount would replay stale events
      // with a stale sessionId.
      if (!final) queueRef.current.unshift(...events);
    });
  }, [token]);

  // Ref-backed mirror of `flush`. The mount-once lifecycle/interval
  // effect reads through this so we don't have to put `flush` in its
  // dependency array — eliminating the OIDC-rotation re-fire bug.
  const flushRef = useRef(flush);
  useEffect(() => { flushRef.current = flush; }, [flush]);

  const report: UseZapTelemetry['report'] = useCallback((kind, itemId, payload) => {
    queueRef.current.push({ ts: Date.now(), kind, itemId, payload });
  }, []);

  // Mount-once session lifecycle. Deps:[] so a silent renew (which
  // rotates auth.user.access_token, hence `token`, hence `flush`)
  // doesn't tear this down and emit phantom start/end pairs.
  useEffect(() => {
    report('zap_session_start', undefined);
    const id = window.setInterval(() => flushRef.current(false), 30_000);
    const onHide = () => flushRef.current(true);
    window.addEventListener('pagehide', onHide);
    return () => {
      window.clearInterval(id);
      window.removeEventListener('pagehide', onHide);
      report('zap_session_end', undefined);
      flushRef.current(true);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // Stable wrapper object so consumers don't re-render-cascade when
  // anything inside changes. report is already stable (empty deps);
  // flushNow closes over the latest flush via ref.
  return useMemo<UseZapTelemetry>(() => ({
    report,
    flushNow: () => flushRef.current(false),
  }), [report]);
}
