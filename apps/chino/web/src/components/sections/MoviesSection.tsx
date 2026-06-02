import { useEffect, useRef, useState } from 'react';
import { MediaCard } from '../MediaCard';
import { BrowseFilters, type BrowseQuery } from '../BrowseFilters';
import { LoadingState } from '../LoadingState';
import { usePagedItems } from '../../hooks/useItems';

const PAGE_SIZE = 48;

export function MoviesSection() {
  const [filter, setFilter] = useState<BrowseQuery>({});
  const { items, loading, error, hasMore, loadMore } = usePagedItems('movie', PAGE_SIZE, filter);

  // IntersectionObserver-driven infinite scroll. The sentinel sits at
  // the bottom of the grid; whenever it enters the viewport we
  // request the next page. rootMargin pre-loads ~one viewport of
  // content before the user actually hits the bottom so scrolling
  // feels continuous.
  const sentinelRef = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const el = sentinelRef.current;
    if (!el) return;
    const obs = new IntersectionObserver(
      (entries) => {
        for (const e of entries) {
          if (e.isIntersecting) loadMore();
        }
      },
      { rootMargin: '600px 0px' },
    );
    obs.observe(el);
    return () => obs.disconnect();
  }, [loadMore]);

  return (
    <div>
      <h1 className="text-4xl font-bold text-white mb-6">Movies</h1>
      <BrowseFilters value={filter} onChange={setFilter} />
      {error ? (
        <p className="text-red-400 text-sm mb-4">Failed to load: {error.message}</p>
      ) : null}

      {items.length === 0 && loading ? (
        <LoadingState variant="full" />
      ) : items.length === 0 ? (
        <p className="text-[#8b949e]">No movies match the current filters.</p>
      ) : (
        <>
          <div className="grid grid-cols-2 sm:grid-cols-[repeat(auto-fill,minmax(190px,1fr))] gap-4">
            {items.map((it) => (
              <MediaCard
                key={it.id}
                id={it.id}
                title={it.title}
                image={it.poster_url || ''}
                year={it.year ? String(it.year) : undefined}
                rating={it.rating ? it.rating.toFixed(1) : undefined}
                type="movie"
                watchedAt={it.watched_at}
              />
            ))}
          </div>
          {/* Sentinel + lazy-load footer */}
          <div ref={sentinelRef} className="h-1" aria-hidden />
          {hasMore ? (
            <div className="mt-6 flex justify-center">
              <LoadingState />
            </div>
          ) : (
            <div className="mt-6 text-center text-xs text-[#8b949e]">
              You've reached the end of the catalogue — {items.length} movies.
            </div>
          )}
        </>
      )}
    </div>
  );
}
