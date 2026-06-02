import { useEffect, useState } from 'react';
import { useAuth } from 'react-oidc-context';
import type { KatalogItem } from './useItems';

export interface EpisodeItem extends KatalogItem {
  season_number?: number;
  episode_number?: number;
  parent_id?: string;
  watched_at?: string | null;
}

export interface Season {
  season: number;
  episodes: EpisodeItem[];
}

export function useSeriesEpisodes(seriesId: string | undefined) {
  const auth = useAuth();
  const [seasons, setSeasons] = useState<Season[]>([]);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    if (!seriesId || auth.isLoading || !auth.isAuthenticated) return;
    const ctrl = new AbortController();
    setLoading(true);
    fetch(`/api/v1/series/${seriesId}/episodes`, {
      signal: ctrl.signal,
      headers: { Authorization: `Bearer ${auth.user?.access_token ?? ''}` },
    })
      .then((r) => (r.ok ? r.json() : { seasons: [] }))
      .then((j) => {
        const token = auth.user?.access_token;
        const enc = token ? encodeURIComponent(token) : '';
        const out: Season[] = (j.seasons ?? []).map((s: Season) => ({
          season: s.season,
          episodes: (s.episodes ?? []).map((e) => ({
            ...e,
            poster_url: e.poster_url && enc ? `${e.poster_url}?token=${enc}` : e.poster_url,
            backdrop_url: e.backdrop_url && enc ? `${e.backdrop_url}?token=${enc}` : e.backdrop_url,
          })),
        }));
        setSeasons(out);
      })
      .catch(() => setSeasons([]))
      .finally(() => setLoading(false));
    return () => ctrl.abort();
  }, [seriesId, auth.isAuthenticated, auth.isLoading, auth.user?.access_token]);

  return { seasons, loading };
}
