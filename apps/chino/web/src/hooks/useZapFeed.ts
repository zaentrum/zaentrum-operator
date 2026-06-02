import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { useAuth } from 'react-oidc-context';
import type { KatalogItem } from './useItems';
import type { ZapItemFeatures } from './useZapPreferences';

/** ε for ε-greedy sampling: 60 % of picks ignore the score and pick
 *  uniformly from the pool. Higher than a textbook ε because on a
 *  cold Zap session the preference vector is empty — without enough
 *  exploration the "exploit" branch collapses to "always pick the
 *  highest-rated item" and the queue order feels deterministic across
 *  sessions. Preferences still steer the feed once they accumulate
 *  via the 40 % exploit branch. */
const EPSILON = 0.6;

/** Tiny additive jitter to every score in the exploit branch so two
 *  equally-rated candidates (e.g. all the unwatched 9.0+ titles in
 *  the pool) don't always tie-break the same way. ±0.05 is comparable
 *  to the rating-bias delta between 9.5 and 10.0, so it can flip the
 *  pick at the top of the rating distribution but is small enough
 *  that a learned genre preference (which sums to ~0.5+ after a
 *  couple of dwells) will still dominate. */
const SCORE_JITTER = 0.05;

/** Fisher-Yates in place. Returns the same array for chaining
 *  convenience. Math.random() is good enough for "vary the order the
 *  user sees" — we don't need crypto-grade entropy. */
function shuffleInPlace<T>(arr: T[]): T[] {
  for (let i = arr.length - 1; i > 0; i -= 1) {
    const j = Math.floor(Math.random() * (i + 1));
    const tmp = arr[i];
    arr[i] = arr[j];
    arr[j] = tmp;
  }
  return arr;
}

/** How many items we keep in the candidate pool. Larger → more variety,
 *  more upfront API load. 60 is enough that after dedup against watched
 *  + already-shown we still have headroom. */
const POOL_SIZE_PER_TYPE = 30;

interface UseZapFeedOpts {
  /** Score function from useZapPreferences. Default: zero everywhere.
   *  Re-read through a ref so passing a fresh closure each render
   *  doesn't churn the hook's internal callable identity. */
  scoreItem?: (features: ZapItemFeatures) => number;
}

interface UseZapFeedResult {
  /** Items in the order the pager should consume them. Items are POPPED
   *  by markShown() — the array shrinks as the user zaps through. */
  queue: KatalogItem[];
  /** Mark an id as already shown so it never resurfaces this session.
   *  Idempotent — safe to call from React's strict-mode double-invoke. */
  markShown: (id: string) => void;
  /** Top up the queue when it gets short. Called by ZapSection on
   *  every advance. */
  refill: () => void;
  loading: boolean;
  /** Pool is exhausted (no more candidates after dedup). Tell the user. */
  empty: boolean;
}

/**
 * Build a candidate pool for the Zap pager out of existing catalog
 * endpoints. V1 makes three parallel fetches:
 *
 *   - GET /api/v1/items?type=movie&sort=rating
 *   - GET /api/v1/items?type=series&sort=newest
 *   - GET /api/v1/items?type=episode&sort=newest
 *
 * dedups against the user's watch history + a session-local shown-set,
 * then ε-greedy samples a queue of N items using the provided scoring
 * function. When the queue drops below 5 we sample more from the same
 * pool until it's exhausted.
 *
 * Note on scoring: the pool here is KatalogItem (id/title/year/rating
 * only — no genres/cast yet). Scoring keys on `type` for V1; full
 * feature-based scoring (genres + cast) kicks in inside ZapCard once
 * it lazy-loads /api/v1/items/{id}. Re-sorting the unshown tail by the
 * updated vector is V2 work.
 */
export function useZapFeed(opts: UseZapFeedOpts = {}): UseZapFeedResult {
  const auth = useAuth();

  const [pool, setPool] = useState<KatalogItem[]>([]);
  const [queue, setQueue] = useState<KatalogItem[]>([]);
  const [loading, setLoading] = useState(true);
  const [empty, setEmpty] = useState(false);

  // Session-shown set is a ref because we don't want it to trigger
  // re-renders — it's a filter, not display state.
  const shownRef = useRef<Set<string>>(new Set());
  const watchedRef = useRef<Set<string>>(new Set());

  // scoreItem via ref so the hook's internal sample() identity stays
  // stable across renders even when the caller passes a fresh closure.
  const scoreItemRef = useRef(opts.scoreItem);
  useEffect(() => { scoreItemRef.current = opts.scoreItem; }, [opts.scoreItem]);

  const token = auth.user?.access_token;

  useEffect(() => {
    if (auth.isLoading || !auth.isAuthenticated || !token) return;
    const ctrl = new AbortController();
    setLoading(true);

    const fetchList = (qs: string): Promise<KatalogItem[]> =>
      fetch(`/api/v1/items?${qs}`, {
        signal: ctrl.signal,
        headers: { Authorization: `Bearer ${token}` },
      })
        .then((r) => (r.ok ? r.json() : null))
        .then((j: { items?: KatalogItem[] } | null) => j?.items ?? [])
        .catch(() => [] as KatalogItem[]);

    // Watched IDs — used to dedupe. /me/watched returns the full
    // history; we only need the ids so a 200-item ceiling is plenty.
    const fetchWatched = (): Promise<Set<string>> =>
      fetch('/api/v1/me/watched?limit=200', {
        signal: ctrl.signal,
        headers: { Authorization: `Bearer ${token}` },
      })
        .then((r) => (r.ok ? r.json() : null))
        .then((j: { items?: { id?: string }[] } | null) => {
          const s = new Set<string>();
          for (const it of j?.items ?? []) {
            if (it?.id) s.add(it.id);
          }
          return s;
        })
        .catch(() => new Set<string>());

    // Set of item ids that have a finished CMAF package on disk.
    // Packaged items skip ffmpeg entirely and serve in <50ms, so the
    // Zap pager filters its candidate pool to ONLY these — current
    // on-demand transcode cold-start is 1-3s, way outside the
    // channel-flip UX budget. Falls back to "no filter" if the
    // endpoint is missing or 5xx so older deploys still get a Zap
    // pool, just with the cold-start UX.
    const fetchPackagedIDs = (): Promise<Set<string> | null> =>
      fetch('/api/v1/play/packaged-ids', {
        signal: ctrl.signal,
        headers: { Authorization: `Bearer ${token}` },
      })
        .then((r) => (r.ok ? r.json() : null))
        .then((j: { ids?: string[] } | null) => {
          if (!j?.ids) return null;
          return new Set(j.ids);
        })
        .catch(() => null);

    Promise.all([
      fetchList(`type=movie&sort=rating&limit=${POOL_SIZE_PER_TYPE}`),
      fetchList(`type=series&sort=newest&limit=${POOL_SIZE_PER_TYPE}`),
      // Episodes are optional — if the API doesn't yet support
      // type=episode for q-less browse, this returns [] and we keep
      // the movie+series mix. No error path for the user.
      fetchList(`type=episode&sort=newest&limit=${POOL_SIZE_PER_TYPE}`),
      fetchWatched(),
      fetchPackagedIDs(),
    ]).then(([movies, series, episodes, watched, packagedIds]) => {
      watchedRef.current = watched;
      // Stamp watched_at locally too so the dedup catches items that
      // were marked watched in this session by another tab.
      const allByID = new Map<string, KatalogItem>();
      for (const it of [...movies, ...series, ...episodes]) {
        if (!it.id) continue;
        if (allByID.has(it.id)) continue;
        if (watched.has(it.id)) continue;
        if (it.watched_at) continue;
        // Filter to packaged items only when the listing is
        // available. Until packaged coverage expands, this trades
        // pool size for a snappier UX — packaged items serve in
        // tens of ms vs 1-3s cold-start on on-demand transcode.
        if (packagedIds && !packagedIds.has(it.id)) continue;
        allByID.set(it.id, it);
      }
      // Shuffle so that even the "exploit" branch (which iterates
      // candidates in array order looking for highest score) doesn't
      // bias toward a fixed prefix of the original list. Without
      // this, /api/v1/items?sort=rating leaves the pool already
      // sorted, and on a cold preference vector the first pick is
      // always the top-rated title in the catalog — same every
      // session.
      const fresh = shuffleInPlace(Array.from(allByID.values()));
      setPool(fresh);
      setEmpty(fresh.length === 0);
      setLoading(false);
    });

    return () => ctrl.abort();
    // streamToken is NOT a dep: none of the fetches above use it (the
    // bearer-Authorization paths in chino-api handle our needs), and
    // listing it as a dep used to double-fire the pool refetch on
    // cold mount as the token transitioned null → string.
  }, [auth.isLoading, auth.isAuthenticated, token]);

  // Sample a batch out of the pool. Stable across renders because all
  // scoring goes through scoreItemRef — no hook-input churn.
  const sample = useCallback((n: number, currentQueue: KatalogItem[], currentPool: KatalogItem[]): KatalogItem[] => {
    const inQueue = new Set(currentQueue.map((i) => i.id));
    const candidates = currentPool.filter((it) => !shownRef.current.has(it.id) && !inQueue.has(it.id));
    if (candidates.length === 0) return [];

    const picks: KatalogItem[] = [];
    const taken = new Set<string>();
    const scoreItem = scoreItemRef.current;
    for (let i = 0; i < n && candidates.length - taken.size > 0; i += 1) {
      // ε-greedy: with probability ε pick uniformly at random;
      // otherwise pick the highest-scoring remaining candidate.
      const explore = Math.random() < EPSILON;
      let choice: KatalogItem | undefined;
      const available = candidates.filter((c) => !taken.has(c.id));
      if (explore || !scoreItem) {
        choice = available[Math.floor(Math.random() * available.length)];
      } else {
        let bestScore = -Infinity;
        for (const c of available) {
          // Score with only type — genres/cast aren't on KatalogItem.
          // Real feature-based scoring happens once ZapCard fetches
          // ItemDetail; this is just the pre-bias.
          const s = scoreItem({ type: c.type });
          // Tiny rating tilt so two equally-unknown candidates aren't
          // identical and the deterministic .find() always returns
          // the first one in feed order. Stratifies by rating without
          // forming a hard tier. The jitter on top breaks ties
          // randomly so the same "10.0 / 9.8 / 9.5" trio doesn't
          // resolve to the same pick every session.
          const total = s + ((c.rating ?? 0) / 100) + (Math.random() * SCORE_JITTER);
          if (total > bestScore) {
            bestScore = total;
            choice = c;
          }
        }
      }
      if (!choice) break;
      picks.push(choice);
      taken.add(choice.id);
    }
    return picks;
  }, []);

  // Initial fill once the pool is ready.
  useEffect(() => {
    if (pool.length === 0) return;
    setQueue((q) => (q.length > 0 ? q : sample(8, q, pool)));
  }, [pool, sample]);

  const refill: UseZapFeedResult['refill'] = useCallback(() => {
    setQueue((q) => {
      if (q.length >= 5) return q;
      const more = sample(8 - q.length, q, pool);
      if (more.length === 0 && q.length === 0) setEmpty(true);
      return [...q, ...more];
    });
  }, [pool, sample]);

  const markShown: UseZapFeedResult['markShown'] = useCallback((id) => {
    if (!id) return;
    shownRef.current.add(id);
    setQueue((q) => q.filter((it) => it.id !== id));
  }, []);

  // Stable wrapper so consumers in handleDwellEnd etc. don't see a
  // new `feed` identity on every render of ZapSection.
  return useMemo<UseZapFeedResult>(() => ({
    queue,
    markShown,
    refill,
    loading,
    empty,
  }), [queue, markShown, refill, loading, empty]);
}
