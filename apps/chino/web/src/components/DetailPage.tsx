import { useEffect, useState } from 'react';
import { useAuth } from 'react-oidc-context';
import { ArrowLeft, Check, Eye, Heart, Loader2, Play, Plus, Star, Youtube } from 'lucide-react';
import { useLikes, useWatchlist } from '../hooks/useUserFlags';
import { useWatchedToggle } from '../hooks/useWatchedToggle';
import { useItem } from '../hooks/useItem';
import { useSeriesEpisodes } from '../hooks/useSeriesEpisodes';
import { useSimilarItems } from '../hooks/useSimilarItems';
import { EpisodesList } from './EpisodesList';
import { FadeImage } from './FadeImage';
import { MediaRow } from './MediaRow';

interface DetailPageProps {
  itemId: string;
}

/**
 * Movie / show detail page. Renders backdrop hero + poster + metadata +
 * play / resume buttons. For series, also renders an Episodes accordion.
 * Fetches catalogue metadata and the saved playback position in parallel
 * so the "Resume from X" button shows immediately if relevant.
 */
export function DetailPage({ itemId }: DetailPageProps) {
  const auth = useAuth();
  const { data, loading } = useItem(itemId);
  const [resumeSec, setResumeSec] = useState<number>(0);

  // Episodes are fetched only for type=series — the hook short-circuits
  // when the id is undefined.
  const isSeries = data?.type === 'series';
  const { seasons } = useSeriesEpisodes(isSeries ? itemId : undefined);
  // "More like this" — up to 12 recommendations scored on shared
  // genre + cast (OpenProject #115). Hook returns [] when nothing
  // scored above zero; the row below short-circuits in that case.
  const { items: similar } = useSimilarItems(itemId, 12);
  const watchlist = useWatchlist();
  const likes = useLikes();
  const inWatchlist = watchlist.has(itemId);
  const liked = likes.has(itemId);

  // Watched state: seed from the catalogue payload's watched_at stamp,
  // override locally on toggle so the button reflects the user's most
  // recent click without a refetch. resetting when itemId changes
  // matters because <DetailPage> is reused across navigations.
  const toggleWatched = useWatchedToggle();
  const [watchedOverride, setWatchedOverride] = useState<boolean | null>(null);
  useEffect(() => {
    setWatchedOverride(null);
  }, [itemId]);
  const watched = watchedOverride ?? !!data?.watched_at;

  // Resume position
  useEffect(() => {
    if (auth.isLoading || !auth.isAuthenticated) return;
    const ctrl = new AbortController();
    fetch(`/api/v1/items/${itemId}/progress`, {
      signal: ctrl.signal,
      headers: { Authorization: `Bearer ${auth.user?.access_token ?? ''}` },
    })
      .then((r) => (r.ok ? r.json() : null))
      .then((j) => setResumeSec(typeof j?.position_sec === 'number' ? j.position_sec : 0))
      .catch(() => undefined);
    return () => ctrl.abort();
  }, [itemId, auth.isAuthenticated, auth.isLoading, auth.user?.access_token]);

  if (loading || !data) {
    return (
      <div className="min-h-screen bg-[#0d1117] text-[#8b949e] flex items-center justify-center">
        <Loader2 className="w-8 h-8 animate-spin" />
      </div>
    );
  }

  const goPlayer = (resume?: boolean) => {
    // The player auto-resumes by default. Pass ?startover=1 to force a
    // clean start, ?resume=<sec> as a hint for the resume-from path.
    const qp = resume ? `?resume=${resumeSec}` : resumeSec > 30 ? '?startover=1' : '';
    window.location.assign(`/player/${encodeURIComponent(itemId)}${qp}`);
  };

  const runtimeMin = data.duration_ms ? Math.round(data.duration_ms / 60_000) : 0;
  const runtimeText = runtimeMin
    ? runtimeMin >= 60
      ? `${Math.floor(runtimeMin / 60)}h ${runtimeMin % 60}m`
      : `${runtimeMin}m`
    : null;

  const directors = (data.cast ?? []).filter((c) => c.role === 'director');
  const actors = (data.cast ?? []).filter((c) => !c.role || c.role === 'actor').slice(0, 5);
  const trailer = pickTrailer(data.trailers);

  return (
    <div className="min-h-screen bg-[#0d1117] text-white">
      {/* Hero: full-width backdrop with a top-to-bottom gradient that
          fades into the page bg so the content below sits flush.
          aspect-[21/9] gives a cinematic shape on phones; max-h cap
          (60vh) keeps the title + Play button above the fold on
          typical desktop monitors so the user doesn't have to scroll
          to find the actionable controls. */}
      <div className="relative">
        <div className="aspect-[21/9] max-h-[60vh] w-full overflow-hidden">
          {data.backdrop_url ? (
            <FadeImage src={data.backdrop_url} alt="" className="w-full h-full object-cover opacity-70" />
          ) : (
            <div className="w-full h-full bg-[#161b22]" />
          )}
          <div className="absolute inset-0 bg-gradient-to-t from-[#0d1117] via-[#0d1117]/40 to-transparent" />
        </div>
        <button
          onClick={() => { if (window.history.length > 1) window.history.back(); else window.location.assign('/'); }}
          className="absolute top-4 left-4 p-2 rounded-full bg-black/50 hover:bg-black/70 transition-colors"
          title="Back"
        >
          <ArrowLeft className="w-5 h-5" />
        </button>
      </div>

      {/* Content overlapping the backdrop. */}
      <div className="max-w-6xl mx-auto px-6 -mt-32 relative z-10 pb-16">
        <div className="flex flex-col md:flex-row gap-8">
          {data.poster_url ? (
            <FadeImage
              src={data.poster_url}
              alt={data.title}
              className="w-48 md:w-64 aspect-[2/3] rounded-lg shadow-2xl object-cover shrink-0"
            />
          ) : (
            <div className="w-48 md:w-64 aspect-[2/3] rounded-lg bg-[#161b22] shrink-0" />
          )}
          <div className="flex-1 pt-4">
            <h1 className="text-3xl md:text-4xl font-bold mb-2">{data.title}</h1>
            {data.tagline ? (
              <p className="italic text-[#8b949e] mb-4">{data.tagline}</p>
            ) : null}

            <div className="flex flex-wrap items-center gap-3 text-sm text-[#c9d1d9] mb-4">
              {data.year ? <span>{data.year}</span> : null}
              {runtimeText ? (
                <>
                  <span className="text-[#8b949e]">•</span>
                  <span>{runtimeText}</span>
                </>
              ) : null}
              {data.rating ? (
                <>
                  <span className="text-[#8b949e]">•</span>
                  <span className="inline-flex items-center gap-1">
                    <Star className="w-4 h-4 fill-[#58a6ff] text-[#58a6ff]" />
                    {data.rating.toFixed(1)}
                  </span>
                </>
              ) : null}
              {data.type ? (
                <span className="px-2 py-0.5 rounded-full bg-white/10 text-xs uppercase tracking-wide">
                  {data.type}
                </span>
              ) : null}
            </div>

            {data.genres && data.genres.length > 0 ? (
              <div className="flex flex-wrap gap-2 mb-5">
                {data.genres.map((g) => (
                  <span
                    key={g}
                    className="px-3 py-1 rounded-full bg-[#21262d] text-[#c9d1d9] text-xs border border-[#30363d]"
                  >
                    {g}
                  </span>
                ))}
              </div>
            ) : null}

            <div className="flex flex-wrap gap-3 mb-6">
              {resumeSec > 30 ? (
                <>
                  <button
                    onClick={() => goPlayer(true)}
                    className="px-5 py-2.5 rounded-full bg-[#58a6ff] hover:bg-[#58a6ff]/80 text-white font-medium flex items-center gap-2"
                  >
                    <Play className="w-5 h-5 fill-white" />
                    Resume {fmtDur(resumeSec)}
                  </button>
                  <button
                    onClick={() => goPlayer(false)}
                    className="px-5 py-2.5 rounded-full bg-white/10 hover:bg-white/20 text-white font-medium flex items-center gap-2"
                  >
                    Start over
                  </button>
                </>
              ) : !isSeries ? (
                <button
                  onClick={() => goPlayer(false)}
                  className="px-5 py-2.5 rounded-full bg-[#58a6ff] hover:bg-[#58a6ff]/80 text-white font-medium flex items-center gap-2"
                >
                  <Play className="w-5 h-5 fill-white" />
                  Play
                </button>
              ) : null}
              {trailer ? (
                <a
                  href={trailer.url}
                  target="_blank"
                  rel="noopener noreferrer"
                  className="px-4 py-2.5 rounded-full bg-white/10 hover:bg-white/20 text-white font-medium flex items-center gap-2"
                  title="Watch trailer on YouTube"
                >
                  <Youtube className="w-5 h-5" />
                  Trailer
                </a>
              ) : null}
              <button
                className={`p-2.5 rounded-full transition-colors ${inWatchlist ? 'bg-emerald-500 hover:bg-emerald-500/80' : 'bg-white/10 hover:bg-white/20'}`}
                onClick={() => void watchlist.toggle(itemId, !inWatchlist)}
                title={inWatchlist ? 'Remove from watchlist' : 'Add to watchlist'}
              >
                {inWatchlist ? <Check className="w-5 h-5 stroke-[3]" /> : <Plus className="w-5 h-5" />}
              </button>
              <button
                className={`p-2.5 rounded-full transition-colors ${watched ? 'bg-emerald-500 hover:bg-emerald-500/80' : 'bg-white/10 hover:bg-white/20'}`}
                onClick={() => {
                  const next = !watched;
                  setWatchedOverride(next);
                  void toggleWatched(itemId, next);
                }}
                title={watched ? 'Mark as unwatched' : 'Mark as watched'}
                aria-pressed={watched}
              >
                <Eye className={`w-5 h-5 ${watched ? 'stroke-[2.5]' : ''}`} />
              </button>
              <button
                className={`p-2.5 rounded-full transition-colors ${liked ? 'bg-rose-500/90 hover:bg-rose-500' : 'bg-white/10 hover:bg-white/20'}`}
                onClick={() => void likes.toggle(itemId, !liked)}
                title={liked ? 'Unlike' : 'Like'}
              >
                <Heart className={`w-5 h-5 ${liked ? 'fill-white' : ''}`} />
              </button>
            </div>

            {data.description ? (
              <p className="text-[#c9d1d9] leading-relaxed max-w-3xl whitespace-pre-line">
                {data.description}
              </p>
            ) : (
              <p className="text-[#8b949e] italic">No description available.</p>
            )}

            {/* Meta strip: cast + subtitles */}
            <div className="mt-6 grid sm:grid-cols-2 gap-4 max-w-3xl text-sm">
              {actors.length > 0 ? (
                <div>
                  <div className="text-[#8b949e] mb-1">Starring</div>
                  <div className="text-[#c9d1d9]">{actors.map((a) => a.name).join(', ')}</div>
                </div>
              ) : null}
              {directors.length > 0 ? (
                <div>
                  <div className="text-[#8b949e] mb-1">{directors.length > 1 ? 'Directors' : 'Director'}</div>
                  <div className="text-[#c9d1d9]">{directors.map((d) => d.name).join(', ')}</div>
                </div>
              ) : null}
              {data.subtitles && data.subtitles.length > 0 ? (
                <div>
                  <div className="text-[#8b949e] mb-1">Subtitles</div>
                  <div className="text-[#c9d1d9]">
                    {Array.from(new Set(data.subtitles.map((s) => s.label || s.lang).filter(Boolean))).join(', ')}
                  </div>
                </div>
              ) : null}
              {data.segments && data.segments.count > 0 ? (
                <div>
                  <div className="text-[#8b949e] mb-1">Analyzed</div>
                  <div className="text-[#c9d1d9]">
                    {[data.segments.has_intro && 'Intro', data.segments.has_credits && 'Credits', data.segments.has_recap && 'Recap']
                      .filter(Boolean)
                      .join(' · ') || 'Segments available'}
                  </div>
                </div>
              ) : null}
            </div>
          </div>
        </div>

        {isSeries ? <EpisodesList seasons={seasons} /> : null}

        {/* "More like this" — only renders when the backend scored at
            least one candidate (#115). Episode detail pages don't
            mount this — the row is for the top-level title only,
            which matches the backend filter (it short-circuits when
            the source is an episode). */}
        {similar.length > 0 ? (
          <div className="mt-10">
            <MediaRow
              title="More like this"
              noLoop
              items={similar.map((it) => ({
                id: it.id,
                title: it.title,
                image: it.poster_url || '',
                year: it.year ? String(it.year) : undefined,
                rating: it.rating ? it.rating.toFixed(1) : undefined,
                type: (it.type === 'series' ? 'series' : 'movie') as 'series' | 'movie',
                watchedAt: it.watched_at,
              }))}
            />
          </div>
        ) : null}
      </div>
    </div>
  );
}

function fmtDur(s: number): string {
  if (!isFinite(s) || s < 0) return '0:00';
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  const sec = Math.floor(s % 60).toString().padStart(2, '0');
  if (h > 0) return `${h}:${m.toString().padStart(2, '0')}:${sec}`;
  return `${m}:${sec}`;
}

// pickTrailer prefers YouTube + "Official Trailer"-style titles.
function pickTrailer(trailers?: { site?: string; url: string; title?: string }[]) {
  if (!trailers || !trailers.length) return null;
  const yt = trailers.filter((t) => (t.site || '').toLowerCase().includes('youtube'));
  const pool = yt.length ? yt : trailers;
  const official = pool.find((t) => /official/i.test(t.title ?? '') && /trailer/i.test(t.title ?? ''));
  if (official) return official;
  const anyTrailer = pool.find((t) => /trailer/i.test(t.title ?? ''));
  return anyTrailer ?? pool[0];
}
