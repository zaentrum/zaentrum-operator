import { useEffect, useRef, useState } from 'react';
import { MediaCard } from './MediaCard';
import { ChevronLeft, ChevronRight } from 'lucide-react';

interface Media {
  id: string;
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
}

interface MediaRowProps {
  title: string;
  items: Media[];
  /** Called when the user clicks "See All". Omit to hide the button. */
  onSeeAll?: () => void;
  /**
   * Opt out of the infinite-loop carousel mode. Use for finite,
   * user-personal lists (Continue Watching) where wrapping past the
   * last item back to the first reads as a UI bug — "I just saw
   * that, why is it appearing again?". Defaults to false (loop
   * enabled when the row is long enough).
   */
  noLoop?: boolean;
}

/**
 * Horizontal infinite-loop carousel. Items are rendered twice in the
 * scroll container; when the user scrolls past the first copy we
 * silently re-anchor scrollLeft by one row's width so they appear to
 * keep going forever. Same trick on the way back.
 *
 * Hover-only prev/next buttons step by roughly one viewport at a time;
 * the gradient masks at each edge hint that there's more off-screen.
 */
export function MediaRow({ title, items, onSeeAll, noLoop = false }: MediaRowProps) {
  // Rows that expose an explicit overview link should never loop. The
  // "See All →" affordance is the escape hatch — wrapping around to
  // the first item would make the row look infinite (#150). Treat
  // onSeeAll as an implicit noLoop.
  const finite = noLoop || !!onSeeAll;
  const scrollRef = useRef<HTMLDivElement>(null);
  // Three layout states based on actual overflow vs viewport width:
  //  - 'none'    items fit, no scroll affordances at all
  //  - 'scroll'  there's overflow but not enough for the loop trick to
  //              be invisible (would duplicate too obviously)
  //  - 'loop'    enough items that 2x render is genuinely seamless
  const [layout, setLayout] = useState<'none' | 'scroll' | 'loop'>('none');
  // Edge affordances (gradient + chevron) only appear in the direction
  // there's actually more content to scroll into. At rest at the start
  // the left edge is hidden; at the end the right edge is hidden. For
  // 'loop' layout the user can always wrap either way, so both edges
  // stay available.
  const [canScrollLeft, setCanScrollLeft] = useState(false);
  const [canScrollRight, setCanScrollRight] = useState(false);

  // Loop only when one full copy of the row is at least 1.5× the
  // visible viewport, so the duplicated content is far enough off-
  // screen that the user never sees identical posters side-by-side.
  // Below that threshold, plain scroll feels more honest.
  const LOOP_RATIO = 1.5;

  useEffect(() => {
    const el = scrollRef.current;
    if (!el || items.length === 0) return;

    const recompute = () => {
      // scrollWidth at this point is for the *current* render. We
      // detect overflow against the single-copy size by measuring
      // the actual children: if items are looped we'd double-count.
      // Cleanest: use the unduplicated baseline computed off
      // clientWidth + a per-card width estimate from the first
      // rendered child.
      const card = el.firstElementChild instanceof HTMLElement
        ? el.firstElementChild.offsetWidth + 16
        : 200;
      const oneCopyWidth = items.length * card;
      if (oneCopyWidth <= el.clientWidth) {
        setLayout('none');
      } else if (!finite && oneCopyWidth >= el.clientWidth * LOOP_RATIO) {
        setLayout('loop');
      } else {
        setLayout('scroll');
      }
    };

    recompute();
    const onResize = () => recompute();
    window.addEventListener('resize', onResize);

    // Edge affordances follow actual scrollLeft, irrespective of
    // layout. Loop mode's wrap-around is a side effect of the
    // boundary re-anchor (handled below) — the chevron just calls
    // scrollBy() which clamps at the real DOM edges, so it can't
    // actually wrap on its own. Showing a "go back" chevron when
    // there's nothing to go back to is the bug we're chasing.
    const recomputeEdges = () => {
      if (layout === 'none') {
        setCanScrollLeft(false);
        setCanScrollRight(false);
        return;
      }
      // 4 px slack so a subtle scroll position doesn't flicker the edge.
      setCanScrollLeft(el.scrollLeft > 4);
      setCanScrollRight(el.scrollLeft + el.clientWidth < el.scrollWidth - 4);
    };
    recomputeEdges();

    const onAnyScroll = () => recomputeEdges();
    el.addEventListener('scroll', onAnyScroll, { passive: true });

    let onScroll: (() => void) | undefined;
    if (layout === 'loop') {
      // Two copies in DOM. Re-anchor scrollLeft when the user crosses
      // the boundary so the carousel appears to loop forever in
      // either direction. Setting scrollLeft directly is synchronous
      // and silent — the duplicated content is identical so the
      // re-anchor isn't visible.
      onScroll = () => {
        const half = el.scrollWidth / 2;
        if (half <= 0) return;
        if (el.scrollLeft >= half) {
          el.scrollLeft = el.scrollLeft - half;
        } else if (el.scrollLeft < 1) {
          el.scrollLeft = el.scrollLeft + half;
        }
      };
      el.addEventListener('scroll', onScroll, { passive: true });
    }
    // Trackpad / mouse-wheel hand-off. A horizontal scroll container
    // captures the entire wheel event by default; when a trackpad
    // gesture produces |deltaY| > |deltaX| (the user is trying to
    // scroll the page down), the carousel still receives the event,
    // tries to scroll its own Y axis, finds no overflow there, and
    // silently swallows the gesture. To the user the page just
    // freezes mid-scroll. Intercept those events here, find the
    // nearest scrollable ancestor, and forward the delta manually.
    // Non-passive listener because we preventDefault.
    const onWheel = (e: WheelEvent) => {
      const horiz = Math.abs(e.deltaX);
      const vert = Math.abs(e.deltaY);
      if (vert <= horiz) return; // horizontal-dominant — let native scroll handle
      // Walk up the DOM to the first ancestor that can actually
      // scroll vertically; that's the <main> on ChinoApp or the
      // document root.
      let p: HTMLElement | null = el.parentElement;
      while (p) {
        const cs = getComputedStyle(p);
        const scrollable = /(auto|scroll)/.test(cs.overflowY);
        if (scrollable && p.scrollHeight > p.clientHeight) {
          e.preventDefault();
          p.scrollBy({ top: e.deltaY });
          return;
        }
        p = p.parentElement;
      }
      // Last resort: scroll the window itself.
      const root = document.scrollingElement as HTMLElement | null;
      if (root && root.scrollHeight > root.clientHeight) {
        e.preventDefault();
        root.scrollBy({ top: e.deltaY });
      }
    };
    el.addEventListener('wheel', onWheel, { passive: false });

    return () => {
      window.removeEventListener('resize', onResize);
      el.removeEventListener('scroll', onAnyScroll);
      if (onScroll) el.removeEventListener('scroll', onScroll);
      el.removeEventListener('wheel', onWheel);
    };
  }, [items, layout, finite]);

  const step = (dir: -1 | 1) => {
    const el = scrollRef.current;
    if (!el) return;
    // Page by visible-width minus one card so successive pages share
    // an anchor item — feels less disorienting than a hard page turn.
    const card = el.firstElementChild instanceof HTMLElement
      ? el.firstElementChild.offsetWidth + 16
      : 200;
    el.scrollBy({ left: dir * Math.max(card, el.clientWidth - card), behavior: 'smooth' });
  };

  if (items.length === 0) return null;
  const visible = layout === 'loop' ? [...items, ...items] : items;
  const hasOverflow = layout !== 'none';

  return (
    <div className="mb-8 group">
      <div className="flex items-center justify-between mb-4">
        <h2 className="text-2xl font-semibold text-white">{title}</h2>
        {onSeeAll ? (
          <button
            onClick={onSeeAll}
            className="flex items-center gap-1 text-[#58a6ff] hover:text-[#58a6ff]/80 transition-colors"
          >
            <span className="text-sm">See All</span>
            <ChevronRight className="w-4 h-4" />
          </button>
        ) : null}
      </div>

      <div className="relative">
        {hasOverflow && (
          <>
            {/* Edge gradient is a separate (non-clickable) layer so
                the chevron's hit-target is icon-sized and doesn't
                cover the play button on the leftmost / rightmost
                MediaCard. Each edge only renders when there's
                actually content to scroll into in that direction —
                at rest at the start, the left edge is hidden; at the
                end, the right edge is hidden. */}
            {canScrollLeft && (
              <>
                <div
                  aria-hidden
                  className="absolute left-0 top-0 bottom-0 w-16 z-10 bg-gradient-to-r from-[#0d1117] via-[#0d1117]/80 to-transparent rounded-lg opacity-0 group-hover:opacity-100 transition-opacity pointer-events-none"
                />
                <button
                  aria-label="Scroll left"
                  onClick={() => step(-1)}
                  className="absolute left-2 top-1/2 -translate-y-1/2 z-50 p-2 rounded-full bg-black/70 hover:bg-black/90 transition-colors opacity-0 group-hover:opacity-100"
                >
                  <ChevronLeft className="w-5 h-5" />
                </button>
              </>
            )}
            {canScrollRight && (
              <>
                <div
                  aria-hidden
                  className="absolute right-0 top-0 bottom-0 w-16 z-10 bg-gradient-to-l from-[#0d1117] via-[#0d1117]/80 to-transparent rounded-lg opacity-0 group-hover:opacity-100 transition-opacity pointer-events-none"
                />
                <button
                  aria-label="Scroll right"
                  onClick={() => step(1)}
                  className="absolute right-2 top-1/2 -translate-y-1/2 z-50 p-2 rounded-full bg-black/70 hover:bg-black/90 transition-colors opacity-0 group-hover:opacity-100"
                >
                  <ChevronRight className="w-5 h-5" />
                </button>
              </>
            )}
          </>
        )}

        <div
          ref={scrollRef}
          // No touch-action restriction here. We tried pan-x and it
          // backfired: once a finger landed on the row the browser
          // refused to handle vertical pans at all, even on a
          // clearly-vertical swipe. Default touch-action lets the
          // browser axis-lock per-gesture — horizontal-leading
          // swipes scroll the row, vertical-leading swipes scroll
          // the page. The wheel handler above continues to handle
          // the trackpad case where vertical and horizontal arrive
          // mixed in a single wheel event.
          // py-3 reserves vertical breathing room so the cards'
          // `hover:scale-105` transform has space to grow into. CSS
          // spec quirk: `overflow-x: auto` implicitly computes
          // `overflow-y` to `auto` as well, so the scaled card's top
          // and bottom edges otherwise get clipped at the content box
          // — visually that reads as the card's rounded corners
          // turning square on hover for any shelf long enough to
          // overflow. The shelves that don't overflow (e.g. Next Up
          // with 1-2 items) escape the clip and look fine.
          className={`flex gap-4 py-3 [scrollbar-width:none] [&::-webkit-scrollbar]:hidden [overscroll-behavior-x:contain] ${
            hasOverflow ? 'overflow-x-auto' : ''
          }`}
        >
          {visible.map((item, i) => (
            <div
              key={`${item.id}-${i}`}
              className="shrink-0 w-32 sm:w-40 md:w-44 lg:w-48 xl:w-52"
            >
              <MediaCard {...item} />
            </div>
          ))}
          {/* End-of-row "See All →" tile. Renders only when the row
              opts into a finite layout AND wires an onSeeAll handler.
              Same dimensions as a MediaCard so the row keeps a flush
              right edge — feels like a button, not an empty card. */}
          {finite && onSeeAll && items.length > 0 ? (
            <button
              type="button"
              onClick={onSeeAll}
              className="shrink-0 w-32 sm:w-40 md:w-44 lg:w-48 xl:w-52 rounded-lg bg-[#161b22] border border-[#30363d] hover:bg-[#21262d] hover:border-[#58a6ff] transition-colors flex flex-col items-center justify-center gap-2 text-[#58a6ff] aspect-[2/3]"
              aria-label={`See all ${title}`}
            >
              <ChevronRight className="w-8 h-8" />
              <span className="text-sm font-medium">See all</span>
            </button>
          ) : null}
        </div>
      </div>
    </div>
  );
}
