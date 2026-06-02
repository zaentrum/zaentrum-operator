import { useEffect, useState } from 'react';
import { useAuth } from 'react-oidc-context';
import type { KatalogItem } from './useItems';
import { useStreamToken } from './useStreamToken';

interface SimilarResponse {
  items: KatalogItem[];
  total: number;
}

/**
 * Fetch "more like this" recommendations for a single catalogue item.
 * Backed by chino-api `/api/v1/items/{id}/similar` → katalog-api's
 * shared-genre + shared-cast score (OpenProject #115).
 *
 * Returns an empty array (not an error) when nothing scored above
 * zero — the caller should hide the row in that case. Errors are
 * swallowed silently because the row is a non-essential addition to
 * the detail page; surfacing a banner would be a regression vs. the
 * previous detail-page behaviour where the row didn't exist at all.
 */
export function useSimilarItems(itemId: string | undefined, limit = 12) {
  const auth = useAuth();
  const streamToken = useStreamToken();
  const [items, setItems] = useState<KatalogItem[]>([]);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    if (!itemId || auth.isLoading || !auth.isAuthenticated) return;
    const ctrl = new AbortController();
    setLoading(true);
    fetch(`/api/v1/items/${encodeURIComponent(itemId)}/similar?limit=${limit}`, {
      signal: ctrl.signal,
      headers: { Authorization: `Bearer ${auth.user?.access_token ?? ''}` },
    })
      .then((r) => (r.ok ? (r.json() as Promise<SimilarResponse>) : null))
      .then((j) => {
        if (!j) {
          setItems([]);
          return;
        }
        // Same stream-token swap as useItems / useItem: encode the
        // long-lived stream token in poster/backdrop URLs so the
        // browser doesn't refetch the row's posters on every OIDC
        // silent renewal.
        const enc = streamToken ? encodeURIComponent(streamToken) : '';
        const next = (j.items ?? []).map((it) => ({
          ...it,
          poster_url:
            it.poster_url && enc ? `${it.poster_url}?stream=${enc}` : it.poster_url,
          backdrop_url:
            it.backdrop_url && enc ? `${it.backdrop_url}?stream=${enc}` : it.backdrop_url,
        }));
        setItems(next);
      })
      .catch(() => setItems([]))
      .finally(() => setLoading(false));
    return () => ctrl.abort();
  }, [itemId, limit, auth.isAuthenticated, auth.isLoading, streamToken]);

  return { items, loading };
}
