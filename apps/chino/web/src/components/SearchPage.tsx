import { useMemo } from 'react';
import { MediaCard } from './MediaCard';
import { useItems, type KatalogItem } from '../hooks/useItems';

interface SearchPageProps {
  query: string;
}

/**
 * Search results. Hits `/api/v1/items?q=…` for both movies and series in
 * parallel, then merges the two sets and orders the merged list by how
 * well each title matches the user's query. katalog-api's default sort
 * for `q=…` queries is alphabetical (`sorttitle ASC`) — it doesn't
 * surface a relevance score — so we rank client-side.
 *
 * Ranking heuristic: exact match > starts-with > word-boundary contains
 * > substring contains > fallback. Ties are broken by rating (higher
 * first) and then by title for stability. The original grouping into
 * Movies / Shows shelves obscured cross-type relevance — searching
 * "watson" would push Spider-Man 2 below Sherlock Holmes inside the
 * Movies block even though both are equally weak matches; mixing makes
 * the strongest hits land at the top regardless of type.
 */
export function SearchPage({ query }: SearchPageProps) {
  const movies = useItems(query, 30, 'movie');
  const series = useItems(query, 30, 'series');

  const movieItems = movies.data?.source === 'katalog' ? movies.data.items : [];
  const seriesItems = series.data?.source === 'katalog' ? series.data.items : [];
  const loading = movies.loading || series.loading;

  const ranked = useMemo(() => {
    const all: Array<KatalogItem & { _kind: 'movie' | 'series' }> = [
      ...movieItems.map((it) => ({ ...it, _kind: 'movie' as const })),
      ...seriesItems.map((it) => ({ ...it, _kind: 'series' as const })),
    ];
    const q = query.toLowerCase().trim();
    if (!q) return all;
    const score = (title: string): number => {
      const t = title.toLowerCase();
      if (t === q) return 100;
      if (t.startsWith(q + ' ')) return 80;
      if (t.startsWith(q)) return 70;
      // Word-boundary contains: token starts with the query somewhere
      // mid-title. Lifts "The Watson Files" over "Sherlock Holmes" for
      // q=watson because the matched word starts the token.
      if (new RegExp(`(^|\\s)${escapeReg(q)}`, 'i').test(t)) return 60;
      if (t.includes(q)) return 40;
      return 0;
    };
    return all.slice().sort((a, b) => {
      const sb = score(b.title) - score(a.title);
      if (sb !== 0) return sb;
      const rb = (b.rating ?? 0) - (a.rating ?? 0);
      if (rb !== 0) return rb;
      return a.title.localeCompare(b.title);
    });
  }, [movieItems, seriesItems, query]);

  const total = ranked.length;

  const headline = useMemo(() => {
    if (!query) return 'Search the library';
    if (loading) return `Searching for "${query}"…`;
    return total > 0 ? `${total} result${total === 1 ? '' : 's'} for "${query}"` : `No results for "${query}"`;
  }, [query, total, loading]);

  return (
    <div>
      <h1 className="text-2xl font-semibold mb-6">{headline}</h1>

      {!query ? (
        <p className="text-[#8b949e]">Type a movie or show title in the search bar to look it up.</p>
      ) : ranked.length === 0 ? null : (
        <div className="grid grid-cols-2 sm:grid-cols-[repeat(auto-fill,minmax(190px,1fr))] gap-4">
          {ranked.map((it) => (
            <MediaCard
              key={`${it._kind}-${it.id}`}
              id={it.id}
              title={it.title}
              image={it.poster_url || ''}
              year={it.year ? String(it.year) : undefined}
              rating={it.rating ? it.rating.toFixed(1) : undefined}
              type={it._kind}
              watchedAt={it.watched_at}
            />
          ))}
        </div>
      )}
    </div>
  );
}

function escapeReg(s: string): string {
  return s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
}
