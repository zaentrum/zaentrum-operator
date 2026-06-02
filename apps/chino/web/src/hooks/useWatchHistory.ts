import { useEffect, useState } from 'react';
import { useAuth } from 'react-oidc-context';
import type { KatalogItem } from './useItems';
import { useStreamToken } from './useStreamToken';

export interface WatchHistoryEntry extends KatalogItem {
  // ISO timestamp of the most recent watch — chino-api stamps this
  // from watched_history.watched_at so re-watches bump to the top of
  // the list (the same row is updated in place, not inserted twice).
  watched_at?: string | null;
}

/**
 * Fetch the user's watch history newest-first from chino-api. Renders
 * on the /me profile page. Limit defaults to 60 — the catalogue stays
 * small enough that one fetch covers the whole history for most
 * accounts, but the endpoint accepts ?offset for pagination if a
 * binge-watcher's row ever overflows.
 */
export function useWatchHistory(limit = 60) {
  const auth = useAuth();
  const streamToken = useStreamToken();
  const [items, setItems] = useState<WatchHistoryEntry[] | null>(null);

  useEffect(() => {
    if (auth.isLoading || !auth.isAuthenticated) return;
    const ctrl = new AbortController();
    fetch(`/api/v1/me/watched?limit=${limit}`, {
      signal: ctrl.signal,
      headers: { Authorization: `Bearer ${auth.user?.access_token ?? ''}` },
    })
      .then((r) => (r.ok ? r.json() : { items: [] }))
      .then((j) => {
        const list = (j.items ?? []) as WatchHistoryEntry[];
        const enc = streamToken ? encodeURIComponent(streamToken) : '';
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
  }, [auth.isAuthenticated, auth.isLoading, auth.user?.access_token, streamToken, limit]);

  return items;
}
