import { useEffect, useState } from 'react';
import { useAuth } from 'react-oidc-context';
import type { KatalogItem } from './useItems';
import { useStreamToken } from './useStreamToken';

export interface CastEntry {
  name: string;
  role?: string;
}

export interface SubtitleRef {
  id: string;
  lang: string;
  label?: string;
  format?: string;
  default?: boolean;
}

export interface TrailerRef {
  site?: string;
  external_id?: string;
  url: string;
  title?: string;
}

export interface SegmentSummary {
  has_intro: boolean;
  has_credits: boolean;
  has_recap: boolean;
  count: number;
}

export interface ItemDetail extends KatalogItem {
  tagline?: string;
  season_number?: number;
  episode_number?: number;
  parent_id?: string;
  genres?: string[];
  cast?: CastEntry[];
  subtitles?: SubtitleRef[];
  trailers?: TrailerRef[];
  segments?: SegmentSummary;
}

/**
 * Fetch a single catalogue entry by id. Used by the detail page —
 * separate from `useItems` so each item lookup is independent and the
 * player page can reuse it.
 */
export function useItem(itemId: string | undefined) {
  const auth = useAuth();
  const streamToken = useStreamToken();
  const [data, setData] = useState<ItemDetail | null>(null);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    if (!itemId || auth.isLoading || !auth.isAuthenticated) return;
    const ctrl = new AbortController();
    setLoading(true);
    fetch(`/api/v1/items/${itemId}`, {
      signal: ctrl.signal,
      headers: { Authorization: `Bearer ${auth.user?.access_token ?? ''}` },
    })
      .then((r) => (r.ok ? (r.json() as Promise<ItemDetail>) : null))
      .then((j) => {
        if (!j) {
          setData(null);
          return;
        }
        // Long-lived stream token in image URLs so silent renews
        // don't refetch the hero / poster.
        const enc = streamToken ? encodeURIComponent(streamToken) : '';
        setData({
          ...j,
          poster_url: j.poster_url && enc ? `${j.poster_url}?stream=${enc}` : j.poster_url,
          backdrop_url: j.backdrop_url && enc ? `${j.backdrop_url}?stream=${enc}` : j.backdrop_url,
        });
      })
      .finally(() => setLoading(false));
    return () => ctrl.abort();
  }, [itemId, auth.isAuthenticated, auth.isLoading, streamToken]);

  return { data, loading };
}
