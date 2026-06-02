import { HeroSection } from '../HeroSection';
import { MediaRow } from '../MediaRow';
import { LoadingState } from '../LoadingState';
import { useItems } from '../../hooks/useItems';
import { useContinueWatching, type ContinueWatchingEntry } from '../../hooks/useContinueWatching';

// cwToCard maps a continue-watching / next-up row to the MediaCard
// props shape MediaRow consumes. Both rails share the same mapping —
// the only differences are the row title and which entries land in
// each (split by `up_next`). For episodes, the card title flips to
// the series so the user reads "How I Met Your Mother / S05E01 /
// Definitions" instead of just the episode title. The progress bar
// only renders when there's a saved position (`up_next` rows have 0).
function cwToCard(it: ContinueWatchingEntry, onRemove?: (id: string) => void) {
  const isEpisode = it.type === 'episode';
  return {
    id: it.id,
    title: isEpisode && it.series_title ? it.series_title : it.title,
    image: it.poster_url || '',
    year: it.year ? String(it.year) : undefined,
    rating: it.rating ? it.rating.toFixed(1) : undefined,
    type: (isEpisode
      ? 'series'
      : it.type === 'album' || it.type === 'track'
        ? 'music'
        : 'movie') as 'series' | 'music' | 'movie',
    episode:
      isEpisode && it.season_number != null && it.episode_number != null
        ? {
            season: it.season_number,
            episode: it.episode_number,
            title: it.title,
          }
        : undefined,
    progress:
      it.duration_sec > 0
        ? Math.min(100, (it.position_sec / it.duration_sec) * 100)
        : undefined,
    watchedAt: it.watched_at,
    // Only the in-progress (non-up_next) rail surfaces the dismiss
    // affordance — "Next Up" cards are server-substituted from a
    // finished episode and there's nothing to dismiss client-side.
    onRemoveFromContinueWatching:
      onRemove && !it.up_next ? () => onRemove(it.id) : undefined,
  };
}

interface HomeSectionProps {
  /** Lifted from ChinoApp so each "See All" button can switch the
      active section without going through URL routing (the SPA shell
      manages section selection in component state). */
  onNavigate?: (section: 'home' | 'movies' | 'series') => void;
}

/**
 * Home page. Three sections, all driven by real catalogue + per-user
 * data (no Unsplash placeholders):
 *
 *   1. Hero pulled from the first item in `Recently added` (so it
 *      changes as the library grows).
 *   2. Continue watching — items with saved progress > 30 s.
 *   3. Recently added — newest movies + shows, mixed.
 *   4. Top rated — sorted by rating server-side.
 */
export function HomeSection({ onNavigate }: HomeSectionProps) {
  // Home rails: each capped at 20 items + a "See All →" tile at the
  // end of the row that jumps to the full overview. 20 is the per-#150
  // contract — the previous looping carousel rendered each shelf twice
  // in the DOM and made it look like the catalogue was bigger than it
  // was, which read as a UI bug.
  // sort=newest exposes the catalogue's createdAt order so "Recently
  // added" actually reflects the scanner's latest cohort rather than
  // primary-key order.
  const recent = useItems(undefined, 20, 'movie', { sort: 'newest' });
  const recentSeries = useItems(undefined, 20, 'series', { sort: 'newest' });
  // Top-rated row: rating>=8 + sort by rating, so the home page surfaces
  // the user's likely highlights even when the catalogue is large.
  const topRated = useItems(undefined, 20, 'movie', { sort: 'rating', ratingMin: 8 });
  const { items: cw, dismiss: dismissCW } = useContinueWatching();

  const recentMovies = recent.data?.source === 'katalog' ? recent.data.items : [];
  const recentShows  = recentSeries.data?.source === 'katalog' ? recentSeries.data.items : [];
  const topRatedMovies = topRated.data?.source === 'katalog' ? topRated.data.items : [];

  const hero = recentMovies[0];

  return (
    <>
      {hero ? (
        <HeroSection
          itemId={hero.id}
          title={hero.title}
          description={hero.description || 'No description available.'}
          image={hero.backdrop_url || hero.poster_url || ''}
          year={hero.year ? String(hero.year) : undefined}
          rating={hero.rating ? hero.rating.toFixed(1) : undefined}
        />
      ) : null}

      {/* Split chino-api's continue-watching feed into two shelves:
            * Continue Watching — rows where the user is mid-episode /
              mid-movie (progress bar always renders).
            * Next Up         — rows where chino-api substituted the
              series' next episode after the user finished one
              (server stamps `up_next: true`; no progress bar).
          Both rails share one MediaCard mapper — only the title +
          filter predicate differ. */}
      {cw && cw.some((it) => !it.up_next) ? (
        <MediaRow
          title="Continue watching"
          noLoop
          items={cw.filter((it) => !it.up_next).map((it) => cwToCard(it, dismissCW))}
        />
      ) : null}

      {cw && cw.some((it) => it.up_next) ? (
        <MediaRow
          title="Next Up"
          noLoop
          items={cw.filter((it) => it.up_next).map((it) => cwToCard(it))}
        />
      ) : null}

      {recentMovies.length > 0 ? (
        <MediaRow
          title="Recently added — Movies"
          onSeeAll={onNavigate ? () => onNavigate('movies') : undefined}
          items={recentMovies.map((it) => ({
            id: it.id,
            title: it.title,
            image: it.poster_url || '',
            year: it.year ? String(it.year) : undefined,
            rating: it.rating ? it.rating.toFixed(1) : undefined,
            type: 'movie',
            watchedAt: it.watched_at,
          }))}
        />
      ) : null}

      {recentShows.length > 0 ? (
        <MediaRow
          title="Recently added — Shows"
          onSeeAll={onNavigate ? () => onNavigate('series') : undefined}
          items={recentShows.map((it) => ({
            id: it.id,
            title: it.title,
            image: it.poster_url || '',
            year: it.year ? String(it.year) : undefined,
            rating: it.rating ? it.rating.toFixed(1) : undefined,
            type: 'series',
            watchedAt: it.watched_at,
          }))}
        />
      ) : null}

      {topRatedMovies.length > 0 ? (
        <MediaRow
          title="Top rated"
          onSeeAll={onNavigate ? () => onNavigate('movies') : undefined}
          items={topRatedMovies.map((it) => ({
            id: it.id,
            title: it.title,
            image: it.poster_url || '',
            year: it.year ? String(it.year) : undefined,
            rating: it.rating ? it.rating.toFixed(1) : undefined,
            type: 'movie',
            watchedAt: it.watched_at,
          }))}
        />
      ) : null}

      {recent.loading && recentMovies.length === 0 ? (
        <LoadingState variant="full" />
      ) : null}
    </>
  );
}
