import { useEffect, useState } from 'react';
import { useAuth } from 'react-oidc-context';
import { useStreamToken } from './useStreamToken';

export interface KatalogItem {
  id: string;
  type: string;
  title: string;
  year?: number;
  rating?: number;
  description?: string;
  duration_ms?: number;
  poster_url?: string;
  backdrop_url?: string;
  // Set by chino-api when the current user has marked this item
  // watched (entered credits OR crossed 95% of duration). The presence
  // of this field is what MediaCard reads to render the "Watched" pill.
  watched_at?: string | null;
}

interface ItemsResponse {
  product: string;
  items: KatalogItem[];
  source: 'katalog' | 'fallback';
  katalogErr?: string;
}

/**
 * Fetch `/api/v1/items` from chino-api. Returns the items along with a
 * `source` flag so the caller can decide whether to show real data or fall
 * back to its own static array.
 */
export interface BrowseFilter {
  genre?: string;
  yearMin?: number;
  yearMax?: number;
  ratingMin?: number;
  sort?: 'rating' | 'year' | 'title' | 'newest';
}

export function useItems(
  q?: string,
  limit = 50,
  type?: 'movie' | 'series' | 'album',
  filter?: BrowseFilter,
) {
  const auth = useAuth();
  const streamToken = useStreamToken();
  const [data, setData] = useState<ItemsResponse | null>(null);
  const [error, setError] = useState<Error | null>(null);
  const [loading, setLoading] = useState(true);

  // Stable key for the filter so the effect doesn't refetch on every render.
  const fKey = JSON.stringify(filter ?? {});

  useEffect(() => {
    if (auth.isLoading || !auth.isAuthenticated) {
      return;
    }
    const ctrl = new AbortController();
    const params = new URLSearchParams();
    if (q) params.set('q', q);
    if (limit) params.set('limit', String(limit));
    if (type) params.set('type', type);
    const f: BrowseFilter = filter ?? {};
    if (f.genre) params.set('genre', f.genre);
    if (f.yearMin) params.set('year_min', String(f.yearMin));
    if (f.yearMax) params.set('year_max', String(f.yearMax));
    if (f.ratingMin) params.set('rating_min', String(f.ratingMin));
    if (f.sort) params.set('sort', f.sort);
    const url = `/api/v1/items${params.toString() ? `?${params}` : ''}`;
    setLoading(true);
    fetch(url, {
      signal: ctrl.signal,
      headers: { Authorization: `Bearer ${auth.user?.access_token ?? ''}` },
    })
      .then((r) => {
        if (!r.ok) throw new Error(`chino-api ${r.status}`);
        return r.json() as Promise<ItemsResponse>;
      })
      .then((j) => {
        // `<img src>` can't set Authorization headers — encode the
        // long-lived stream token in the URL so the artwork proxy
        // accepts it via `?stream=`. Using the stream token (6 h TTL)
        // instead of the OIDC access token (rotates every ~5 min)
        // means the image URLs stay stable across silent renewals and
        // the browser doesn't refetch every poster on the grid every
        // few minutes.
        if (streamToken && j.items) {
          const enc = encodeURIComponent(streamToken);
          j.items = j.items.map((it) => ({
            ...it,
            poster_url: it.poster_url ? `${it.poster_url}?stream=${enc}` : undefined,
            backdrop_url: it.backdrop_url ? `${it.backdrop_url}?stream=${enc}` : undefined,
          }));
        }
        setData(j);
      })
      .catch((e) => setError(e))
      .finally(() => setLoading(false));
    return () => ctrl.abort();
    // fKey captures every filter field, so the effect refires when any of
    // them change. Stream token is included so the first page loads as
    // soon as it's available (initial mount races: items fetch can
    // resolve before the token mint does).
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [q, limit, type, fKey, auth.isAuthenticated, auth.isLoading, streamToken]);

  return { data, error, loading };
}

/**
 * Paged variant of useItems for the Movies / Series browse grids.
 * Accumulates pages of items, exposes loadMore() the IntersectionObserver
 * at the grid's tail can call, and tracks `hasMore` so we stop hammering
 * the endpoint once the catalogue is exhausted.
 *
 * Resets the accumulated list whenever the filter or query string
 * changes — page 0 of a different filter should not be glued onto
 * page 5 of the previous one.
 */
export function usePagedItems(
  type: 'movie' | 'series' | 'album',
  pageSize: number,
  filter?: BrowseFilter,
  q?: string,
) {
  const auth = useAuth();
  const streamToken = useStreamToken();
  const [items, setItems] = useState<KatalogItem[]>([]);
  const [error, setError] = useState<Error | null>(null);
  const [loading, setLoading] = useState(true);
  const [hasMore, setHasMore] = useState(true);
  const [offset, setOffset] = useState(0);

  // Reset key — any change here drops the accumulated list and starts
  // fresh at offset 0.
  const fKey = JSON.stringify({ filter: filter ?? {}, q: q ?? '', type, pageSize });

  useEffect(() => {
    setItems([]);
    setOffset(0);
    setHasMore(true);
    setError(null);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [fKey]);

  useEffect(() => {
    if (auth.isLoading || !auth.isAuthenticated) return;
    if (!hasMore) {
      setLoading(false);
      return;
    }
    const ctrl = new AbortController();
    const params = new URLSearchParams();
    if (q) params.set('q', q);
    params.set('limit', String(pageSize));
    params.set('offset', String(offset));
    params.set('type', type);
    const f: BrowseFilter = filter ?? {};
    if (f.genre) params.set('genre', f.genre);
    if (f.yearMin) params.set('year_min', String(f.yearMin));
    if (f.yearMax) params.set('year_max', String(f.yearMax));
    if (f.ratingMin) params.set('rating_min', String(f.ratingMin));
    if (f.sort) params.set('sort', f.sort);
    setLoading(true);
    fetch(`/api/v1/items?${params}`, {
      signal: ctrl.signal,
      headers: { Authorization: `Bearer ${auth.user?.access_token ?? ''}` },
    })
      .then((r) => {
        if (!r.ok) throw new Error(`chino-api ${r.status}`);
        return r.json() as Promise<ItemsResponse>;
      })
      .then((j) => {
        // Use the long-lived stream token in image URLs so silent
        // renews don't reload every poster on the grid.
        const page = (j.items ?? []).map((it) => {
          if (!streamToken) return it;
          const enc = encodeURIComponent(streamToken);
          return {
            ...it,
            poster_url: it.poster_url ? `${it.poster_url}?stream=${enc}` : undefined,
            backdrop_url: it.backdrop_url ? `${it.backdrop_url}?stream=${enc}` : undefined,
          };
        });
        // A short page = end of stream. Definitive signal regardless
        // of whether the server emits a count.
        if (page.length < pageSize) setHasMore(false);
        // Dedupe in case the server re-emits a boundary row (shouldn't,
        // but cheap guard).
        setItems((prev) => {
          const seen = new Set(prev.map((p) => p.id));
          return [...prev, ...page.filter((p) => !seen.has(p.id))];
        });
      })
      .catch((e) => {
        if ((e as Error).name !== 'AbortError') setError(e as Error);
      })
      .finally(() => setLoading(false));
    return () => ctrl.abort();
    // streamToken in deps so the page lands once the token is ready
    // (first mount races: the items fetch can resolve before the
    // /me/stream-token mint does). OIDC token is NOT in deps — the
    // image URLs use stream token, and the bearer header is read at
    // fetch time so a renewal doesn't need a refetch.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [offset, fKey, auth.isAuthenticated, auth.isLoading, streamToken]);

  const loadMore = () => {
    if (loading || !hasMore) return;
    setOffset((o) => o + pageSize);
  };

  return { items, error, loading, hasMore, loadMore };
}

/**
 * Build a one-shot play URL for an item. Browsers cannot attach
 * Authorization headers to `<video src>`, so we encode the bearer in a
 * query string. The chino-api `/api/v1/items/{id}/play` handler accepts
 * either header or `?token=`.
 */
export function usePlayUrl(itemId?: string) {
  const auth = useAuth();
  if (!itemId) return null;
  const token = auth.user?.access_token;
  if (!token) return `/api/v1/items/${itemId}/play`;
  return `/api/v1/items/${itemId}/play?token=${encodeURIComponent(token)}`;
}
