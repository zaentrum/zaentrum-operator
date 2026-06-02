import { useCallback, useEffect, useState } from 'react';
import { useAuth } from 'react-oidc-context';
import type { KatalogItem } from './useItems';
import { useStreamToken } from './useStreamToken';

export interface ContinueWatchingEntry extends KatalogItem {
  position_sec: number;
  duration_sec: number;
  // Set by chino-api when the underlying item is an episode of a
  // series — carries the parent series' title so the card can show
  // "Show — S01E05 — Episode title".
  series_title?: string;
  season_number?: number;
  episode_number?: number;
  // True when chino-api substituted this row for a *finished* episode
  // — i.e. it's the next episode in the series, not something the
  // user has actually started. The home page renders these in a
  // separate "Next Up" rail with no progress bar.
  up_next?: boolean;
}

export interface UseContinueWatching {
  items: ContinueWatchingEntry[] | null;
  /**
   * Remove an item from Continue Watching by stamping its saved
   * position at the end of the timeline. The server's ListContinueWatching
   * SQL treats `position_sec >= duration_sec - 60` as "finished" — movies
   * are dropped from the feed, episodes are replaced by the series'
   * next-up card. No new schema/flag introduced; we ride on the existing
   * /progress endpoint the player uses.
   */
  dismiss: (id: string) => void;
}

/**
 * Fetch the user's recently-watched items (server joins
 * playback_progress with katalog metadata). Items are ordered most-
 * recently-watched first; only ones with > 30 s of progress and not
 * yet at the end are returned.
 */
export function useContinueWatching(): UseContinueWatching {
  const auth = useAuth();
  const streamToken = useStreamToken();
  const [items, setItems] = useState<ContinueWatchingEntry[] | null>(null);

  useEffect(() => {
    if (auth.isLoading || !auth.isAuthenticated) return;
    const ctrl = new AbortController();
    fetch('/api/v1/me/continue-watching', {
      signal: ctrl.signal,
      headers: { Authorization: `Bearer ${auth.user?.access_token ?? ''}` },
    })
      .then((r) => (r.ok ? r.json() : { items: [] }))
      .then((j) => {
        const enc = streamToken ? encodeURIComponent(streamToken) : '';
        const list = (j.items ?? []) as ContinueWatchingEntry[];
        // Mirror useItems(): rewrite artwork URLs with the long-lived
        // stream token so silent renews don't refetch the row.
        setItems(
          list.map((it) => ({
            ...it,
            poster_url: it.poster_url && enc ? `${it.poster_url}?stream=${enc}` : it.poster_url,
            backdrop_url: it.backdrop_url && enc ? `${it.backdrop_url}?stream=${enc}` : it.backdrop_url,
          })),
        );
      })
      .catch(() => setItems([]));
    return () => ctrl.abort();
  }, [auth.isAuthenticated, auth.isLoading, streamToken]);

  const dismiss = useCallback(
    (id: string) => {
      const token = auth.user?.access_token ?? '';
      // Optimistic: drop the row immediately so the card disappears even
      // if the POST is slow. On failure we leave the optimistic state
      // alone — the row is still on the server, the next page load will
      // bring it back if the dismiss didn't land.
      setItems((prev) => (prev ? prev.filter((it) => it.id !== id) : prev));
      const target = items?.find((it) => it.id === id);
      // duration_sec from the server is the authoritative timeline length;
      // pushing exactly that satisfies the >= duration - 60 finished test.
      // Fallback to 24h when the row never recorded a duration (live items,
      // mis-scanned files) — still well past the finished cutoff.
      const dur = target?.duration_sec && target.duration_sec > 0 ? target.duration_sec : 86400;
      fetch(`/api/v1/items/${encodeURIComponent(id)}/progress`, {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          Authorization: `Bearer ${token}`,
        },
        body: JSON.stringify({ position_sec: dur, duration_sec: dur }),
        keepalive: true,
      }).catch(() => undefined);
    },
    [auth.user?.access_token, items],
  );

  return { items, dismiss };
}
