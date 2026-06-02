import { useEffect, useMemo, useRef, useState } from 'react';
import Hls from 'hls.js';
import { Bookmark, BookmarkCheck, Maximize2, Volume2, VolumeX, Info } from 'lucide-react';
import { useAuth } from 'react-oidc-context';
import { useStreamToken } from '../../hooks/useStreamToken';
import { useItem } from '../../hooks/useItem';
import { useWatchlist } from '../../hooks/useUserFlags';
import { FadeImage } from '../FadeImage';
import { pickZapMidpoint, type ZapSegment } from '../../hooks/useZapMidpoint';
import type { KatalogItem } from '../../hooks/useItems';

interface ZapCardProps {
  item: KatalogItem;
  /** True when this card is the visible one. Drives playback. Prev/next
   *  cards are inactive and only render their backdrop. */
  active: boolean;
  /** Session-wide mute state, owned by the parent. Single source of
   *  truth across all cards so unmuting once doesn't have to be
   *  repeated on every swipe. */
  muted: boolean;
  /** Setter for the session mute state — wired to the per-card mute
   *  button and to the autoplay-blocked fallback. */
  setMuted: (m: boolean) => void;
  /** Fires once per item-becomes-active transition, deduped against
   *  the session-wide set the parent maintains. Used to log a
   *  zap_impression event without flicker-induced double-counting. */
  onImpression: (info: { itemId: string; midSec: number; midSource: string; features: ZapFeatures }) => void;
  /** Continuous-dwell timer flush. Parent stops the timer on card
   *  swap-out and converts dwellMs → zap_dwell / zap_skip_fast / zap_skip. */
  onDwellEnd: (info: { itemId: string; dwellMs: number; features: ZapFeatures }) => void;
  /** Video ended naturally (we reached the end of the source). Rare for
   *  zap (we start mid-content) but it's how we know "saw the whole
   *  scene". */
  onComplete: (info: { itemId: string; features: ZapFeatures }) => void;
  /** User pressed the expand button — open full PlayerPage at the
   *  current playhead. Parent does the navigation. */
  onExpand: (info: { itemId: string; resumeSec: number; features: ZapFeatures }) => void;
  /** User pressed save/unsave. */
  onSaveToggle: (info: { itemId: string; saved: boolean; features: ZapFeatures }) => void;
}

export interface ZapFeatures {
  type?: string;
  genres?: string[];
  castNames?: string[];
}

/** Caps we declare to the server for the preview pipeline. Kept
 *  static + permissive: the server still gates real codec selection on
 *  the source codecs. */
const ZAP_CAPS = 'avc,hvc,aac,opus,mp3';

/** Below this many milliseconds of accumulated wall-clock dwell we
 *  treat the cleanup as noise (StrictMode double-invoke, fast prop
 *  re-renders) and skip the onDwellEnd callback entirely. Any genuine
 *  human swipe takes longer than 100 ms from card-mount to gesture
 *  completion. */
const DWELL_NOISE_FLOOR_MS = 100;

/** Adapt quality to the network. The server's medium rung is ~3 Mbps
 *  and low is ~1.5 Mbps; on cellular / save-data we drop to low. */
function pickQuality(): 'low' | 'medium' {
  type NavConn = { effectiveType?: string; saveData?: boolean };
  const c = (navigator as Navigator & { connection?: NavConn }).connection;
  if (!c) return 'medium';
  if (c.saveData) return 'low';
  if (c.effectiveType && /^(slow-2g|2g|3g)$/.test(c.effectiveType)) return 'low';
  return 'medium';
}

export function ZapCard({
  item,
  active,
  muted,
  setMuted,
  onImpression,
  onDwellEnd,
  onComplete,
  onExpand,
  onSaveToggle,
}: ZapCardProps) {
  const auth = useAuth();
  const streamToken = useStreamToken();
  const watchlist = useWatchlist();
  const detail = useItem(item.id);

  const [segments, setSegments] = useState<ZapSegment[]>([]);
  // Marks the segments fetch as having SETTLED (success OR failure)
  // so we can gate playUrl below. Without this gate, playUrl is built
  // with segments=[] (→ percent midpoint), then rebuilt a moment
  // later when segments arrive with intro/credits markers (→
  // different seekSec). Each rebuild tears down hls.js and spawns a
  // fresh ffmpeg -ss on the server. The flag flips true on a 1.5 s
  // timeout too so a slow /segments endpoint can't block playback
  // indefinitely.
  // Read the bearer through a ref so silent OIDC renews don't
  // re-trigger the segments fetch on every renewal. The segments
  // endpoint is stable for a session — once we have the data we
  // don't want to repeat the request.
  const bearerRef = useRef(auth.user?.access_token);
  useEffect(() => { bearerRef.current = auth.user?.access_token; }, [auth.user?.access_token]);

  // Best-effort fetch — when segments land they upgrade midpoint from
  // 'percent' to 'segments'. The fetch DOES NOT gate playUrl: a CDP
  // probe (2026-06-02) found this fetch added 30-120ms (p95 3.3s) to
  // every card while never actually populating anything because the
  // server returns {items:[...]} and we used to read j.segments. Fix
  // the shape AND stop blocking playback on it.
  useEffect(() => {
    const token = bearerRef.current;
    if (!token) return;
    const ctrl = new AbortController();
    fetch(`/api/v1/items/${item.id}/segments`, {
      signal: ctrl.signal,
      headers: { Authorization: `Bearer ${token}` },
    })
      .then((r) => (r.ok ? r.json() : null))
      .then((j: { items?: ZapSegment[]; segments?: ZapSegment[] } | null) => {
        // The endpoint emits `items` but accept either shape so this
        // hook is robust if the server contract is ever clarified.
        const segs = j?.items ?? j?.segments;
        if (Array.isArray(segs)) setSegments(segs);
      })
      .catch(() => undefined);
    return () => ctrl.abort();
  }, [item.id]);

  // Fresh random ratio per item mount → each card lands at a
  // different scene. Cached in state so re-renders don't churn the
  // playUrl (which would tear down hls.js and double-transcode).
  // We re-roll only when item.id changes — same card on revisit
  // keeps its seek point, but swiping to a fresh card rolls a new one.
  const [randomRatio, setRandomRatio] = useState(() => Math.random());
  useEffect(() => {
    setRandomRatio(Math.random());
  }, [item.id]);

  // Pure derivation: same item + same segments + same ratio → same
  // seek point.
  const midpoint = useMemo(() => {
    const durationMs = detail.data?.duration_ms ?? item.duration_ms;
    return pickZapMidpoint({ durationMs, segments, randomRatio });
  }, [detail.data?.duration_ms, item.duration_ms, segments, randomRatio]);

  const features: ZapFeatures = useMemo(() => ({
    type: item.type,
    genres: detail.data?.genres,
    castNames: (detail.data?.cast ?? [])
      .filter((c) => !c.role || c.role === 'actor')
      .slice(0, 5)
      .map((c) => c.name),
  }), [item.type, detail.data?.genres, detail.data?.cast]);

  // Keep the latest features + callbacks in refs so the dwell effect
  // only re-fires on genuine active/item transitions. Without the
  // refs, useItem resolving (which flips features identity) would
  // tear the dwell effect down mid-card, fire onDwellEnd with a
  // tiny elapsed-ms, and the parent would classify that as a
  // zap_skip_fast — silently removing the active card from the queue.
  const featuresRef = useRef(features);
  useEffect(() => { featuresRef.current = features; }, [features]);
  const onDwellEndRef = useRef(onDwellEnd);
  useEffect(() => { onDwellEndRef.current = onDwellEnd; }, [onDwellEnd]);
  const onImpressionRef = useRef(onImpression);
  useEffect(() => { onImpressionRef.current = onImpression; }, [onImpression]);
  const onCompleteRef = useRef(onComplete);
  useEffect(() => { onCompleteRef.current = onComplete; }, [onComplete]);

  const [unmuteBlocked, setUnmuteBlocked] = useState(false);
  // True once the video has painted at least one frame for this src.
  // Until then we keep the video element invisible and let the
  // backdrop poster fill the screen — otherwise the user stares at a
  // black rectangle for the 1-3 s ffmpeg -ss cold start. Reset on
  // every active→true / item.id transition so a re-mount on revisit
  // gets the same hide-then-fade-in treatment.
  const [hasFirstFrame, setHasFirstFrame] = useState(false);
  useEffect(() => {
    setHasFirstFrame(false);
  }, [item.id, active]);

  // Backdrop URL fallback chain — backdrop_url → poster_url → neutral
  // gradient. Without this, a 404 on the chosen URL would let FadeImage
  // render the broken-image icon + alt text in the corner (verified
  // visually on items like "Blue Eye Samurai" where backdrop_url 404s).
  // alt="" keeps the title from leaking into the UI as alt text even
  // mid-transition.
  const initialBg = item.backdrop_url || item.poster_url || '';
  const [bgSrc, setBgSrc] = useState(initialBg);
  const [bgFailed, setBgFailed] = useState(false);
  useEffect(() => {
    setBgSrc(item.backdrop_url || item.poster_url || '');
    setBgFailed(false);
  }, [item.id, item.backdrop_url, item.poster_url]);
  const onBgError = () => {
    // First failure: if we were on backdrop_url and a poster_url
    // exists, try the poster. Otherwise give up and let the gradient
    // do the work.
    if (bgSrc === item.backdrop_url && item.poster_url && item.poster_url !== bgSrc) {
      setBgSrc(item.poster_url);
      return;
    }
    setBgFailed(true);
  };

  // The video element. We always render it (so it can be the target of
  // hls.attachMedia when we go active) but only start the HLS pipeline
  // when active=true. Inactive cards show poster + title overlay only.
  const videoRef = useRef<HTMLVideoElement>(null);
  const hlsRef = useRef<Hls | null>(null);

  // Build the master.m3u8 URL once we know the stream token. We used
  // to also wait on a segments-settled gate to avoid rebuilding hls.js
  // when /segments arrived late and shifted the midpoint, but in
  // practice the gate stalled every card by the full segments RTT
  // (median 400ms, p95 3.3s) without changing the seek — the response
  // shape was broken so segments stayed empty regardless. With the
  // shape fixed (see segments fetch above), a late arrival can still
  // shift the midpoint, but: (i) most packaged items have no analyzer
  // segments so this is a no-op; (ii) when it does shift, hls.js
  // rebuilds for one card — a small cost on the rare positive path,
  // vs the fetch-RTT cost we used to pay on every card.
  //
  // NOTE: we deliberately do NOT pass ?t=<seekSec> on the URL. The
  // chino-stream master handler honours ?t= for on-demand transcode
  // items but routes packaged (pre-built CMAF) items by a
  // HasCompletedPackage() check BEFORE caps/seek routing — packaged
  // items silently ignore ?t= and serve from source-time zero, which
  // would land every card on the opening logo. Instead we drive the
  // seek client-side: Hls.startPosition + a loadedmetadata
  // currentTime fallback. That works uniformly across packaged AND
  // transcoded streams: hls.js fetches segments containing seekSec,
  // and chino-stream produces them on demand for the transcode path.
  const playUrl = useMemo(() => {
    if (!streamToken) return '';
    if (midpoint.source === 'fallback' || midpoint.seekSec <= 0) return '';
    const q = pickQuality();
    const params = new URLSearchParams({
      stream: streamToken,
      q,
      caps: ZAP_CAPS,
    });
    return `/api/v1/items/${item.id}/play/master.m3u8?${params.toString()}`;
  }, [streamToken, item.id, midpoint.seekSec, midpoint.source]);

  // ---- Impression: parent owns session-wide dedup, so we just fire
  // once per (item.id, became-active) transition. ----
  const impressionFiredRef = useRef<string | null>(null);
  useEffect(() => {
    if (!active) return;
    // detail.loading flips false on success AND on error (.finally in
    // useItem), so this gate eventually releases for every card —
    // including ones where /items/{id} returns 5xx and detail.data
    // stays null. Without this, a permanent detail-fetch failure used
    // to swallow the impression silently.
    const detailSettled = !detail.loading;
    if (!detailSettled && midpoint.source === 'fallback') return;
    if (impressionFiredRef.current === item.id) return;
    impressionFiredRef.current = item.id;
    onImpressionRef.current({
      itemId: item.id,
      midSec: midpoint.seekSec,
      midSource: midpoint.source,
      features: featuresRef.current,
    });
  }, [active, detail.loading, midpoint.seekSec, midpoint.source, item.id]);

  // Reset the impression dedup only on item.id change, NOT on
  // active=false. The pager can briefly oscillate the active card as
  // the IntersectionObserver crosses 0.5 ratios during snap-y
  // momentum; resetting on inactive would double-fire impressions on
  // every flicker.
  useEffect(() => {
    impressionFiredRef.current = null;
  }, [item.id]);

  // ---- Dwell timer: counts wall-clock ms while card is active ----
  // Deps deliberately limited to [active, item.id] so identity churn
  // on parent callbacks doesn't trigger spurious cleanups. The
  // callback is read through a ref at cleanup time.
  useEffect(() => {
    if (!active) return undefined;
    const startedAt = performance.now();
    const startedItemId = item.id;
    return () => {
      const elapsedMs = performance.now() - startedAt;
      // StrictMode dev double-invokes effects: mount → cleanup →
      // mount. Without this floor, the synchronous cleanup would fire
      // onDwellEnd with elapsedMs ≈ 0, which the parent classifies as
      // a zap_skip_fast (poisoning the preference vector AND removing
      // the card from the queue before the user even sees it).
      if (elapsedMs < DWELL_NOISE_FLOOR_MS) return;
      onDwellEndRef.current({
        itemId: startedItemId,
        dwellMs: Math.round(elapsedMs),
        features: featuresRef.current,
      });
    };
  }, [active, item.id]);

  // ---- HLS lifecycle: mount only when active. Eager teardown on
  // active→false to free transcoder slots and MSE memory. ----
  useEffect(() => {
    const v = videoRef.current;
    if (!v || !active || !playUrl) return;
    // Seek-on-metadata: regardless of how HLS is delivered (hls.js
    // or Safari native), once the video reports it knows its
    // duration we set currentTime = midpoint.seekSec. This is what
    // makes packaged items actually land mid-content instead of at
    // t=0 — the server-side ?t= path is ignored for those.
    const seekSec = midpoint.seekSec;
    const seekIfNeeded = () => {
      if (seekSec > 0 && Math.abs(v.currentTime - seekSec) > 0.5) {
        v.currentTime = seekSec;
      }
    };
    v.addEventListener('loadedmetadata', seekIfNeeded, { once: true });

    if (!Hls.isSupported()) {
      // Safari / iOS native HLS path. Far simpler — set src, play().
      v.src = playUrl;
      v.muted = muted;
      v.play().catch(() => {
        // Autoplay-with-sound rejected. Fall back to muted and surface
        // the "tap to unmute" hint.
        v.muted = true;
        setMuted(true);
        setUnmuteBlocked(true);
        v.play().catch(() => undefined);
      });
      return () => {
        v.removeEventListener('loadedmetadata', seekIfNeeded);
        v.removeAttribute('src');
        v.load();
      };
    }

    const hls = new Hls({
      // Tight buffer: a zap card is ~10-30 s of viewing on average.
      // Anything more is wasted transcoder work if the user swipes
      // away. backBufferLength stays small for the same reason.
      maxBufferLength: 20,
      maxMaxBufferLength: 30,
      maxBufferSize: 50 * 1000 * 1000,
      backBufferLength: 4,
      fragLoadingMaxRetry: 4,
      manifestLoadingMaxRetry: 2,
      // Tell hls.js to start fetching segments at the zap midpoint
      // rather than at the playlist start. For packaged items this
      // means we skip downloading segments 0..seekSec entirely; for
      // on-demand transcode items the server only produces segments
      // covering seekSec onward. Either way the user sees the
      // mid-content scene instead of the opening logo.
      startPosition: seekSec,
    });
    hls.attachMedia(v);
    hls.on(Hls.Events.MEDIA_ATTACHED, () => {
      hls.loadSource(playUrl);
    });
    hls.on(Hls.Events.ERROR, (_, data) => {
      if (!data.fatal) return;
      // No recovery loop here — if a zap card errors, just leave the
      // poster visible. The user will swipe past it.
      hls.destroy();
      hlsRef.current = null;
    });
    hlsRef.current = hls;
    v.muted = muted;
    // Try autoplay with sound. On a sidebar-click entry the gesture
    // satisfies most browsers, but Safari/iOS sometimes still block
    // — fall back to muted-with-banner so the video at least plays.
    v.play().catch(() => {
      v.muted = true;
      setMuted(true);
      setUnmuteBlocked(true);
      v.play().catch(() => undefined);
    });
    return () => {
      v.removeEventListener('loadedmetadata', seekIfNeeded);
      hls.destroy();
      hlsRef.current = null;
    };
    // muted is intentionally not in deps — toggling mute should NOT
    // tear down the HLS pipeline (handled by a separate effect below).
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [active, playUrl, midpoint.seekSec]);

  // Apply mute changes without rebuilding HLS.
  useEffect(() => {
    const v = videoRef.current;
    if (!v) return;
    v.muted = muted;
    if (!muted) setUnmuteBlocked(false);
  }, [muted]);

  // ---- onComplete (natural end of the source) ----
  useEffect(() => {
    const v = videoRef.current;
    if (!v) return;
    const onEnded = () => onCompleteRef.current({ itemId: item.id, features: featuresRef.current });
    v.addEventListener('ended', onEnded);
    return () => v.removeEventListener('ended', onEnded);
  }, [item.id]);

  // ---- First-frame detection so the backdrop can fade out only once
  // the video actually has something to show. `playing` fires after
  // the browser has decoded enough to start the playhead moving —
  // before that, the video element is just a black rectangle on top
  // of the backdrop. `loadeddata` is an extra safety net for native
  // HLS (Safari/iOS) where the playing event timing can lag. ----
  useEffect(() => {
    const v = videoRef.current;
    if (!v) return;
    const onFirstFrame = () => setHasFirstFrame(true);
    v.addEventListener('playing', onFirstFrame);
    v.addEventListener('loadeddata', onFirstFrame);
    return () => {
      v.removeEventListener('playing', onFirstFrame);
      v.removeEventListener('loadeddata', onFirstFrame);
    };
  }, [item.id]);

  const inWatchlist = watchlist.has(item.id);
  const toggleSave = () => {
    void watchlist.toggle(item.id, !inWatchlist);
    onSaveToggle({ itemId: item.id, saved: !inWatchlist, features });
  };

  const expand = () => {
    const v = videoRef.current;
    // v.currentTime is the absolute wall-clock position in the source
    // because we seek client-side (Hls.startPosition / loadedmetadata
    // → currentTime). No midpoint offset to add — that double-counts
    // (and was the reason the full player was restarting from a
    // weird earlier point). Fall back to the picked seek when the
    // video element hasn't laid out yet.
    const resumeSec = v?.currentTime && v.currentTime > 0 ? v.currentTime : midpoint.seekSec;
    onExpand({ itemId: item.id, resumeSec, features });
  };

  return (
    <div className="relative w-full h-full bg-black snap-start overflow-hidden">
      {/* Neutral gradient: ALWAYS rendered so even a totally
          imageless item has a colour wash behind the title instead
          of pure black. Sits under the FadeImage so the image
          covers it whenever it loads successfully. */}
      <div className="absolute inset-0 bg-gradient-to-br from-[#161b22] via-[#0d1117] to-black" />

      {/* Backdrop / poster: rendered only if we have a candidate URL
          and it hasn't failed. Stays fully visible until the video
          produces its first frame, then crossfades out under the
          video. alt="" prevents the broken-image fallback from
          leaking the title text when the URL 404s. */}
      {bgSrc && !bgFailed ? (
        <FadeImage
          src={bgSrc}
          alt=""
          onError={onBgError}
          className={`absolute inset-0 w-full h-full object-cover transition-opacity duration-500 ${
            active && hasFirstFrame ? 'opacity-30' : 'opacity-100'
          }`}
          loading="eager"
          decoding="async"
        />
      ) : null}

      {/* Video element. Always in the DOM (hls.attachMedia needs a
          target) but only fed when active. Stays at opacity-0 until
          the first frame paints — without this, the user stares at a
          black rectangle for the 1-3 s ffmpeg -ss cold start. */}
      <video
        ref={videoRef}
        playsInline
        muted={muted}
        autoPlay={false}
        className={`absolute inset-0 w-full h-full object-cover transition-opacity duration-300 ${
          active && hasFirstFrame ? 'opacity-100' : 'opacity-0'
        }`}
      />

      {/* Cold-start spinner so the user knows tuning is happening
          instead of wondering whether the card is broken. Hidden once
          the first frame paints. */}
      {active && !hasFirstFrame ? (
        <div className="absolute top-1/2 left-1/2 -translate-x-1/2 -translate-y-1/2 z-10 text-white/80 text-xs tracking-wide animate-pulse">
          TUNING…
        </div>
      ) : null}

      {/* Top-right floating controls: mute, save, expand, info */}
      <div className="absolute top-4 right-4 z-10 flex flex-col gap-3">
        <button
          onClick={() => setMuted(!muted)}
          className="w-10 h-10 rounded-full bg-black/60 backdrop-blur hover:bg-black/80 flex items-center justify-center text-white"
          title={muted ? 'Unmute' : 'Mute'}
          aria-label={muted ? 'Unmute' : 'Mute'}
        >
          {muted ? <VolumeX className="w-5 h-5" /> : <Volume2 className="w-5 h-5" />}
        </button>
        <button
          onClick={toggleSave}
          className={`w-10 h-10 rounded-full backdrop-blur flex items-center justify-center text-white ${
            inWatchlist ? 'bg-emerald-500/80 hover:bg-emerald-500' : 'bg-black/60 hover:bg-black/80'
          }`}
          title={inWatchlist ? 'Remove from watchlist' : 'Add to watchlist'}
          aria-label={inWatchlist ? 'Remove from watchlist' : 'Add to watchlist'}
        >
          {inWatchlist ? <BookmarkCheck className="w-5 h-5" /> : <Bookmark className="w-5 h-5" />}
        </button>
        <button
          onClick={expand}
          className="w-10 h-10 rounded-full bg-black/60 backdrop-blur hover:bg-black/80 flex items-center justify-center text-white"
          title="Open full player"
          aria-label="Open full player"
        >
          <Maximize2 className="w-5 h-5" />
        </button>
        <button
          onClick={() => window.location.assign(`/i/${encodeURIComponent(item.id)}`)}
          className="w-10 h-10 rounded-full bg-black/60 backdrop-blur hover:bg-black/80 flex items-center justify-center text-white"
          title="Details"
          aria-label="Details"
        >
          <Info className="w-5 h-5" />
        </button>
      </div>

      {/* Bottom-left title block — the channel-flip moment-of-arrival
          tells the user "you're in $title, episode S01E04, mid-scene". */}
      <div className="absolute left-0 right-0 bottom-0 z-10 p-6 pb-10 bg-gradient-to-t from-black via-black/70 to-transparent">
        <h2 className="text-white text-2xl font-semibold mb-1 drop-shadow">
          {item.title}
        </h2>
        <div className="text-[#c9d1d9] text-sm flex flex-wrap items-center gap-2">
          {/* Episode badge — shown when the catalog detail has season +
              episode numbers populated (only true for type=episode
              items). Uses the same SxxExx convention as MediaCard so
              the user sees a consistent format across surfaces. */}
          {detail.data?.season_number != null && detail.data.episode_number != null ? (
            <span className="text-[#58a6ff] font-medium tracking-wide">
              S{String(detail.data.season_number).padStart(2, '0')}E{String(detail.data.episode_number).padStart(2, '0')}
            </span>
          ) : null}
          {item.year ? (
            <>
              {detail.data?.season_number != null && detail.data.episode_number != null ? (
                <span className="text-[#8b949e]">·</span>
              ) : null}
              <span>{item.year}</span>
            </>
          ) : null}
          {item.rating ? (
            <>
              <span className="text-[#8b949e]">·</span>
              <span className="text-[#58a6ff]">{Number(item.rating).toFixed(1)}</span>
            </>
          ) : null}
          {item.type ? (
            <>
              <span className="text-[#8b949e]">·</span>
              <span className="uppercase tracking-wide text-xs">{item.type}</span>
            </>
          ) : null}
          {midpoint.source === 'segments' ? (
            <>
              <span className="text-[#8b949e]">·</span>
              <span className="text-xs text-[#8b949e]">mid-scene</span>
            </>
          ) : null}
        </div>
        {detail.data?.description ? (
          <p className="mt-3 text-[#c9d1d9] text-sm line-clamp-2 max-w-2xl">
            {detail.data.description}
          </p>
        ) : null}
      </div>

      {/* Tap-to-unmute banner — only when the browser blocked the
          autoplay-with-sound and we had to mute. Tapping anywhere on
          the banner unmutes via the existing toggle. */}
      {active && unmuteBlocked ? (
        <button
          onClick={() => setMuted(false)}
          className="absolute top-1/2 left-1/2 -translate-x-1/2 -translate-y-1/2 z-20 px-4 py-2 rounded-full bg-white/90 text-black text-sm font-medium shadow-lg"
        >
          Tap to unmute
        </button>
      ) : null}
    </div>
  );
}
