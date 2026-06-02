import { useEffect, useRef, useState } from 'react';
import { Play, Info, Plus, Check, MoreVertical } from 'lucide-react';
import { FadeImage } from './FadeImage';
import { useWatchlist } from '../hooks/useUserFlags';

interface MediaCardProps {
  id?: string;
  title: string;
  image: string;
  year?: string;
  rating?: string;
  type: 'movie' | 'series' | 'music' | 'tv';
  episode?: {
    season: number;
    episode: number;
    title: string;
  };
  progress?: number;
  watchedAt?: string | null;
  // When set, the card renders a hover-only 3-dot menu with a single
  // "Remove from Continue Watching" action. Only the in-progress home
  // rail wires this — other rails leave it undefined and the menu
  // never appears.
  onRemoveFromContinueWatching?: () => void;
}

/**
 * Default click target is the detail page (`/i/<id>`). The hover overlay
 * surfaces:
 *   - Play  → straight to /player/<id>
 *   - Plus  → watchlist (placeholder, disabled for now)
 *   - Info  → detail page (same as body click)
 *
 * The overlay shows via `group-hover` (pure CSS) so it ONLY appears on
 * devices with a real pointer. Touch devices get no overlay — a tap
 * just opens the detail page (no double-tap-then-pick-button dance).
 * Each button uses stopPropagation so a click on it doesn't also fall
 * through to the body's openDetail handler.
 */
export function MediaCard({ id, title, image, year, rating, type, episode, progress, watchedAt, onRemoveFromContinueWatching }: MediaCardProps) {
  const watchlist = useWatchlist();
  const inWatchlist = id ? watchlist.has(id) : false;
  const [menuOpen, setMenuOpen] = useState(false);
  const menuRef = useRef<HTMLDivElement | null>(null);

  // Close the dropdown when the user clicks anywhere outside it.
  // Without this, the menu can linger after a click that landed
  // somewhere else on the row (e.g. on a neighbouring card's body).
  useEffect(() => {
    if (!menuOpen) return;
    const handler = (e: MouseEvent) => {
      if (menuRef.current && !menuRef.current.contains(e.target as Node)) {
        setMenuOpen(false);
      }
    };
    document.addEventListener('mousedown', handler);
    return () => document.removeEventListener('mousedown', handler);
  }, [menuOpen]);
  const openDetail = () => {
    if (id) window.location.assign(`/i/${encodeURIComponent(id)}`);
  };
  const openPlayer = (e: React.MouseEvent) => {
    e.stopPropagation();
    if (!id) return;
    // Continue-watching cards carry `progress` — clicking from there
    // means "resume", so the player should land mid-stream without
    // any "Resume from X?" dialog. The flag is consumed by PlayerPage
    // via the URL query.
    const resumeHint = progress !== undefined && progress > 0 ? '?autoresume=1' : '';
    window.location.assign(`/player/${encodeURIComponent(id)}${resumeHint}`);
  };
  const toggleWatchlist = (e: React.MouseEvent) => {
    e.stopPropagation();
    if (!id) return;
    void watchlist.toggle(id, !inWatchlist);
  };

  return (
    <div
      // Named group (`group/card`) so the overlay's group-hover/card
      // only fires when THIS card is hovered. MediaRow uses an
      // unnamed `group` to drive chevron visibility — without the
      // namespace, hovering anywhere in the row would light up every
      // card's overlay simultaneously.
      // scale-[1.03] is subtle on purpose: at 4K the overview grid is
      // dense enough that a stronger pop (scale-105 was prior) reads
      // as visually disruptive — the hovered card felt "too big" next
      // to its neighbours. 3% still gives the user a clear "this is
      // the active card" cue without making the row jump.
      className="group/card relative rounded-lg overflow-hidden bg-[#161b22] cursor-pointer transition-transform hover:scale-[1.03]"
      onClick={openDetail}
    >
      <div className="aspect-[2/3] relative overflow-hidden">
        {image ? (
          <FadeImage
            src={image}
            alt={title}
            className="w-full h-full object-cover"
            loading="lazy"
            decoding="async"
          />
        ) : (
          <div className="w-full h-full flex items-center justify-center text-[#30363d]">
            <Info className="w-8 h-8" />
          </div>
        )}

        {/* Watched badge — top-right corner. Pure visual; the card stays
            clickable through it. Sits above the hover overlay so it
            remains visible while the overlay fades in. */}
        {watchedAt && (
          <div
            className="absolute top-2 right-2 z-40 flex items-center justify-center w-7 h-7 rounded-full bg-emerald-500 shadow-lg pointer-events-none"
            title="Watched"
          >
            <Check className="w-4 h-4 text-white stroke-[3]" />
          </div>
        )}

        {/* Hover overlay — opacity-gated so touch devices (no :hover)
            never see it. Higher z-index than the MediaRow chevron
            buttons (z-10) so the play button wins clicks on the
            leftmost / rightmost card. */}
        <div className="absolute inset-0 z-30 bg-gradient-to-t from-black via-black/60 to-transparent flex flex-col justify-end p-4 opacity-0 group-hover/card:opacity-100 transition-opacity duration-200 pointer-events-none">
          <div className="flex gap-2 mb-2 pointer-events-auto items-center">
            <button
              className="p-2 bg-[#58a6ff] hover:bg-[#58a6ff]/80 rounded-full transition-colors disabled:opacity-50"
              disabled={!id}
              onClick={openPlayer}
              title="Play"
            >
              <Play className="w-4 h-4 text-white fill-white" />
            </button>
            <button
              className={`p-2 rounded-full transition-colors ${inWatchlist ? 'bg-emerald-500 hover:bg-emerald-500/80' : 'bg-white/20 hover:bg-white/30'}`}
              title={inWatchlist ? 'Remove from watchlist' : 'Add to watchlist'}
              onClick={toggleWatchlist}
              disabled={!id}
            >
              {inWatchlist ? (
                <Check className="w-4 h-4 text-white stroke-[3]" />
              ) : (
                <Plus className="w-4 h-4 text-white" />
              )}
            </button>
            <button
              className="p-2 bg-white/20 hover:bg-white/30 rounded-full transition-colors"
              onClick={(e) => { e.stopPropagation(); openDetail(); }}
              title="Details"
            >
              <Info className="w-4 h-4 text-white" />
            </button>

            {/* Continue-watching dismiss menu — joined into the same
                action row as the other buttons so the alignment reads
                as one cluster. Dropdown opens upward (bottom-full) so
                it doesn't get clipped by the row below. */}
            {onRemoveFromContinueWatching && (
              <div ref={menuRef} className="relative">
                <button
                  className="p-2 bg-white/20 hover:bg-white/30 rounded-full transition-colors"
                  onClick={(e) => {
                    e.stopPropagation();
                    setMenuOpen((v) => !v);
                  }}
                  title="More options"
                  aria-haspopup="menu"
                  aria-expanded={menuOpen}
                >
                  <MoreVertical className="w-4 h-4 text-white" />
                </button>
                {menuOpen && (
                  <div
                    className="absolute left-0 bottom-full mb-1 min-w-[220px] bg-[#161b22] border border-[#30363d] rounded-md shadow-xl py-1 z-50"
                    role="menu"
                  >
                    <button
                      className="w-full text-left px-3 py-2 text-sm text-[#c9d1d9] hover:bg-[#21262d]"
                      onClick={(e) => {
                        e.stopPropagation();
                        setMenuOpen(false);
                        onRemoveFromContinueWatching();
                      }}
                      role="menuitem"
                    >
                      Remove from Continue Watching
                    </button>
                  </div>
                )}
              </div>
            )}
          </div>
        </div>

        {progress !== undefined && progress > 0 && (
          <div className="absolute bottom-0 left-0 right-0 h-1 bg-[#30363d] z-30">
            <div className="h-full bg-[#58a6ff]" style={{ width: `${progress}%` }} />
          </div>
        )}
      </div>

      <div className="p-3">
        <h3 className="text-[#c9d1d9] font-medium truncate">{title}</h3>

        {episode ? (
          // Episode subtitle mirrors the movie year·rating row so the
          // card stays the same height as its siblings in the Continue
          // Watching strip. SxxExx (no space) matches the standard
          // file-naming convention and is tighter than 'S1 E1'.
          <div className="flex items-center gap-2 mt-1 text-sm text-[#8b949e] truncate">
            <span className="text-[#58a6ff] shrink-0">
              S{String(episode.season).padStart(2, '0')}E{String(episode.episode).padStart(2, '0')}
            </span>
            {episode.title ? (
              <>
                <span className="shrink-0">·</span>
                <span className="truncate">{episode.title}</span>
              </>
            ) : null}
          </div>
        ) : (
          <div className="flex items-center gap-2 mt-1 text-sm text-[#8b949e]">
            {year && <span>{year}</span>}
            {rating && (
              <>
                <span>•</span>
                <span className="text-[#58a6ff]">{rating}</span>
              </>
            )}
          </div>
        )}
      </div>
    </div>
  );
}
