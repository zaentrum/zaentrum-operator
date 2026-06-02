import { Play, Info } from 'lucide-react';
import { useEffect, useRef, useState } from 'react';
import { useHeroPool, type HeroEntry } from '../hooks/useHeroPool';
import { FadeImage } from './FadeImage';

interface HeroSectionProps {
  /** Fallback entry used when the trailer pool hasn't loaded yet or
   *  when the user's library has no items with usable trailers. The
   *  carousel still renders a static backdrop in that case. */
  title: string;
  description: string;
  image: string;
  rating?: string;
  year?: string;
  itemId?: string;
}

const ROTATE_MS = 20_000; // ~20 s per trailer — long enough to recognise the title, short enough to feel alive.
// Single flag for the trailer-on-hero behaviour. Flip to true to
// reinstate the auto-playing YouTube embed; the surrounding pool /
// dot rotation logic is unchanged either way.
const TRAILERS_ON_HERO = false;

/**
 * Trailer carousel hero. Plays each pool entry's YouTube trailer
 * muted + autoplaying for ~20 s, then advances. Below the trailer:
 * a black-to-transparent gradient stack so the title + buttons stay
 * legible, plus aspect-ratio centering so a tall hero box (typical of
 * desktop layouts) crops a 16:9 video without letterboxing the frame.
 *
 * Falls back to the static backdrop image when:
 *   - The user's network blocks YouTube embeds.
 *   - The pool query hasn't completed yet (first 200 ms).
 *   - No catalogue entry has a usable trailer.
 */
export function HeroSection({ title, description, image, rating, year, itemId }: HeroSectionProps) {
  const pool = useHeroPool();
  const [idx, setIdx] = useState(0);
  // Randomise the start index ONCE when the pool first arrives, so
  // refreshes don't always show the same entry. Subsequent cycles
  // tick monotonically through the pool. Doing this in a layout
  // effect (not during render) avoids the "setState during render"
  // anti-pattern that empties the page when the cascade hits the
  // update-depth limit.
  const seededRef = useRef(false);
  useEffect(() => {
    if (seededRef.current || pool.length === 0) return;
    seededRef.current = true;
    setIdx(Math.floor(Math.random() * pool.length));
  }, [pool.length]);

  // Pause auto-rotation while the user is hovering (mirrors
  // chino-androidtv's focus-pause behaviour). Without this, a user
  // reading the description or about to click Play / More Info loses
  // the card mid-action.
  const [paused, setPaused] = useState(false);
  useEffect(() => {
    if (pool.length < 2 || paused) return;
    const t = window.setInterval(() => setIdx((i) => (i + 1) % pool.length), ROTATE_MS);
    return () => window.clearInterval(t);
  }, [pool.length, paused]);

  // When the pool is empty (or first paint), render the original
  // static hero so the page never has an empty top.
  const entry: HeroEntry | null = pool[idx] ?? null;
  const heroTitle = entry?.title ?? title;
  const heroDesc = entry?.description ?? description;
  const heroImage = entry?.backdrop_url ?? entry?.poster_url ?? image;
  const heroRating = entry?.rating != null ? entry.rating.toFixed(1) : rating;
  const heroYear = entry?.year != null ? String(entry.year) : year;
  const playTargetId = entry?.id ?? itemId;

  const goPlayer = () => {
    if (playTargetId) window.location.assign(`/player/${encodeURIComponent(playTargetId)}`);
  };
  const goDetail = () => {
    if (playTargetId) window.location.assign(`/i/${encodeURIComponent(playTargetId)}`);
  };

  return (
    <div
      className="relative h-[280px] sm:h-[360px] md:h-[460px] lg:h-[500px] rounded-xl overflow-hidden mb-8 bg-black"
      onMouseEnter={() => setPaused(true)}
      onMouseLeave={() => setPaused(false)}
      onFocusCapture={() => setPaused(true)}
      onBlurCapture={(e) => {
        // Only un-pause when focus actually leaves the hero, not on
        // intra-hero focus moves (button → button).
        if (!e.currentTarget.contains(e.relatedTarget as Node | null)) {
          setPaused(false);
        }
      }}
    >
      {/* Backdrop — sits on the right edge of the hero. On ultra-wide
          screens the image is capped at max-w-[1100px] so it stops
          stretching past its natural 16:9 aspect.
          maskImage applies a fade-to-transparent on the image's
          left edge so the image bleeds into the black hero
          background where the title + buttons sit. */}
      <FadeImage
        src={heroImage}
        alt={heroTitle}
        className="absolute inset-y-0 right-0 w-full md:w-[60%] md:max-w-[1100px] h-full object-cover object-center [mask-image:linear-gradient(to_left,black_0%,black_60%,transparent_100%)] [-webkit-mask-image:linear-gradient(to_left,black_0%,black_60%,transparent_100%)]"
      />

      {/* Trailer iframe — temporarily disabled at user request; the
          backdrop image alone is the hero. The constant flag below
          flips the embed back on in one line when we want trailers
          on the hero again. The pool / rotation still target trailer-
          bearing items so the surface is ready. */}
      {TRAILERS_ON_HERO && entry?.ytKey ? (
        <div className="absolute inset-y-0 right-0 w-full md:w-[60%] md:max-w-[1100px] flex items-center justify-center pointer-events-none [mask-image:linear-gradient(to_left,black_0%,black_60%,transparent_100%)] [-webkit-mask-image:linear-gradient(to_left,black_0%,black_60%,transparent_100%)]">
          <div className="w-full h-full max-w-none aspect-video">
            <iframe
              key={entry.ytKey}
              src={`https://www.youtube.com/embed/${entry.ytKey}?autoplay=1&mute=1&controls=0&loop=1&playlist=${entry.ytKey}&modestbranding=1&playsinline=1&rel=0&iv_load_policy=3&disablekb=1`}
              title={`${heroTitle} — trailer`}
              className="w-full h-full"
              allow="autoplay; encrypted-media"
              referrerPolicy="strict-origin-when-cross-origin"
              loading="lazy"
            />
          </div>
        </div>
      ) : null}

      {/* Mobile-only bottom-up gradient so the title text remains
          legible when overlaid on the full-width image. Desktop uses
          the maskImage on the backdrop itself (above) — no extra
          overlay needed since the image already fades to transparent
          on its right edge. */}
      <div className="absolute inset-0 bg-gradient-to-t from-black via-black/70 to-transparent md:hidden" />

      {/* Edge fade — softens the iframe's hard edges so the carousel
          blends with the page background. */}
      <div className="absolute inset-0 pointer-events-none [box-shadow:inset_0_0_120px_60px_rgba(0,0,0,0.55)]" />

      {/* Text column. The container occupies the full vertical extent
          and uses justify-between so the title/meta/description anchor
          to the TOP and the buttons + carousel dots anchor to the
          BOTTOM (per user request). Inner wrapper caps width so long
          descriptions don't run all the way to the gradient
          transition. Mobile keeps the same split — title sits in the
          dark top area, controls in the bottom one. */}
      <div className="absolute inset-0 flex flex-col p-4 sm:p-6 md:p-10 lg:p-12">
        {/* Top group: title + meta + description. */}
        <div className="md:mr-auto md:max-w-[45%] md:text-left">
          <h1 className="text-2xl sm:text-3xl md:text-4xl lg:text-5xl font-bold text-white mb-2 md:mb-4 line-clamp-2 drop-shadow-lg">
            {heroTitle}
          </h1>

          <div className="flex items-center gap-3 mb-2 md:mb-4 text-xs sm:text-sm">
            {heroYear && <span className="text-white drop-shadow">{heroYear}</span>}
            {heroRating && (
              <>
                <span className="text-[#8b949e]">•</span>
                <span className="px-2 py-0.5 bg-[#58a6ff] text-white rounded">{heroRating}</span>
              </>
            )}
          </div>

          {/* hidden on mobile; md+ uses line-clamp-3 directly (display
              is -webkit-box). The prior class set was
              `hidden md:block ... line-clamp-3` — `md:block` set
              display:block on desktop, which raced with line-clamp's
              display:-webkit-box and won depending on stylesheet
              ordering, so the description rendered un-clamped and
              spilled below the hero box for long synopses
              (e.g. Star Wars Rebels). */}
          <p className="hidden md:line-clamp-3 text-[#c9d1d9] text-base lg:text-lg drop-shadow">
            {heroDesc}
          </p>
        </div>

        {/* Spacer pushes the controls group to the bottom. */}
        <div className="flex-1" />

        {/* Bottom group: Play + More Info, then rotation dots. */}
        <div className="md:mr-auto md:max-w-[45%]">
          <div className="flex gap-2 md:gap-4">
            <button
              onClick={goPlayer}
              disabled={!playTargetId}
              className="flex items-center gap-2 px-4 py-2 md:px-6 md:py-3 bg-[#58a6ff] hover:bg-[#58a6ff]/80 disabled:opacity-50 disabled:cursor-not-allowed text-white rounded-lg transition-colors text-sm md:text-base"
            >
              <Play className="w-4 h-4 md:w-5 md:h-5 fill-white" />
              <span>Play</span>
            </button>
            <button
              onClick={goDetail}
              disabled={!playTargetId}
              className="flex items-center gap-2 px-4 py-2 md:px-6 md:py-3 bg-white/20 hover:bg-white/30 disabled:opacity-50 disabled:cursor-not-allowed text-white rounded-lg backdrop-blur-sm transition-colors text-sm md:text-base"
            >
              <Info className="w-4 h-4 md:w-5 md:h-5" />
              <span>More Info</span>
            </button>
          </div>

          {/* Dot indicator: which slot in the carousel we're on.
              Visible only when there's more than one entry. Click to
              jump. */}
          {pool.length > 1 && (
            <div
              className="flex gap-1.5 mt-4"
              title={paused ? 'Auto-rotation paused' : undefined}
            >
              {pool.map((_, i) => (
                <button
                  key={i}
                  onClick={() => setIdx(i)}
                  className={`w-2 h-2 rounded-full transition-all ${
                    i === idx
                      ? paused
                        ? 'bg-white ring-1 ring-white/60 ring-offset-1 ring-offset-black'
                        : 'bg-white'
                      : 'bg-white/30 hover:bg-white/60'
                  }`}
                  title={`Show ${i + 1}/${pool.length}`}
                />
              ))}
            </div>
          )}
        </div>
      </div>
    </div>
  );
}
