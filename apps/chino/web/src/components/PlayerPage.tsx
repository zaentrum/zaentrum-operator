import { useEffect, useMemo, useRef, useState } from 'react';
import { useAuth } from 'react-oidc-context';
import { Captions, Volume2, VolumeX, Maximize, Minimize, Play, Pause, ArrowLeft, Loader2, Info, X, Settings, SkipForward, ChevronLeft, ChevronRight, Gauge } from 'lucide-react';
import Hls from 'hls.js';
import { useSettings, isBingeContinuation, recordEpisodePlay } from '../lib/settings';
import { useStreamToken } from '../hooks/useStreamToken';
import { parseTrickplayVTT, findTrickplayCue, type TrickplayCue } from '../lib/trickplay';

interface Subtitle {
  id: string;
  label: string;
  lang: string;
  url: string;
  default?: boolean;
}

// Numeric → name decoders for the diagnostic log. Kept module-level so the
// effect closure stays small.
function readyStateName(n: number): string {
  return ['HAVE_NOTHING', 'HAVE_METADATA', 'HAVE_CURRENT_DATA', 'HAVE_FUTURE_DATA', 'HAVE_ENOUGH_DATA'][n] ?? String(n);
}
function networkStateName(n: number): string {
  return ['NETWORK_EMPTY', 'NETWORK_IDLE', 'NETWORK_LOADING', 'NETWORK_NO_SOURCE'][n] ?? String(n);
}
function mediaErrorName(n: number): string {
  return ['', 'ABORTED', 'NETWORK', 'DECODE', 'SRC_NOT_SUPPORTED'][n] ?? String(n);
}
function bufferedRanges(v: HTMLMediaElement): string {
  const b = v.buffered;
  if (!b || b.length === 0) return '[]';
  const out: string[] = [];
  for (let i = 0; i < Math.min(b.length, 3); i++) {
    out.push(`${b.start(i).toFixed(1)}-${b.end(i).toFixed(1)}`);
  }
  if (b.length > 3) out.push(`+${b.length - 3} more`);
  return '[' + out.join(', ') + ']';
}

interface SubtitlesResponse {
  subtitles: Subtitle[];
}

interface TrackInfo {
  index: number;
  codec: string;
  language: string;
  title?: string;
  default?: boolean;
  forced?: boolean;
  channels?: number;
}

interface PlayInfo {
  filename: string;
  container: string;
  video_codec: string;
  audio_codec: string;
  width: number;
  height: number;
  duration_ms: number;
  mode: 'passthrough' | 'remux' | 'transcode' | 'packaged';
  reason: string;
  // Encoder the server is actually using. "libx264" when the pod has
  // no GPU; "h264_nvenc" when NVENC is active. Only meaningful when
  // mode === 'transcode'; passthrough/packaged/remux paths don't
  // re-encode so the source codec stays authoritative.
  encoder?: 'libx264' | 'h264_nvenc' | string;
  qualities?: { name: 'high' | 'medium' | 'low'; label: string }[];
  default_quality?: 'high' | 'medium' | 'low';
  audio_tracks?: TrackInfo[];
  subtitle_tracks?: TrackInfo[];
}

// ISO 639-2/B → human-readable label for the language menu. Falls back
// to the raw code so a track tagged "tha" still shows "tha".
const LANG_NAMES: Record<string, string> = {
  eng: 'English', deu: 'German',  fra: 'French',  spa: 'Spanish',
  ita: 'Italian', jpn: 'Japanese', zho: 'Chinese', por: 'Portuguese',
  rus: 'Russian', nld: 'Dutch',   pol: 'Polish',  tur: 'Turkish',
  kor: 'Korean',  ara: 'Arabic',  hin: 'Hindi',   ces: 'Czech',
  swe: 'Swedish', nor: 'Norwegian', dan: 'Danish', fin: 'Finnish',
  ukr: 'Ukrainian', ron: 'Romanian',
  und: 'Unknown',
};
const langLabel = (code: string) => LANG_NAMES[code?.toLowerCase()] || code || 'Unknown';

// localStorage key for the user's preferred subtitle language. Special
// values: 'off' = user explicitly disabled subtitles (don't auto-pick a
// fallback), missing key = first-mount user, default to English if a
// matching track exists.
const SUB_LANG_KEY = 'chino:preferredSubLang';

type Quality = 'high' | 'medium' | 'low';
const QUALITY_RUNGS: Quality[] = ['high', 'medium', 'low'];

interface SwitchEntry {
  ts: number;
  label: string;
  reason: 'manual' | 'auto' | 'stall' | 'startup';
  detail?: string;
}

// Per-rung output spec for the transcode ladder, mirrored from the
// stream service's QualityLadder. Used in the Playback info dialog to
// show what the user is actually asking ffmpeg for.
const QUALITY_SPEC: Record<Quality, { vcodec: string; crf: string; abps: string; scale: string }> = {
  high:   { vcodec: 'libx264', crf: '23', abps: '192 kbps', scale: 'source' },
  medium: { vcodec: 'libx264', crf: '26', abps: '128 kbps', scale: '720p'   },
  low:    { vcodec: 'libx264', crf: '28', abps: '96 kbps',  scale: '480p'   },
};

interface TelemetryEvent {
  ts: number;
  kind: string;
  itemId?: string;
  payload?: Record<string, unknown>;
}

// Funny + status-flavoured loading messages. Rotated while the video is
// buffering / not-yet-ready so the user knows something is happening.
const LOADING_MESSAGES: string[] = [
  'Calling the projectionist…',
  'Threading the film reels…',
  'Dimming the house lights…',
  'Buttering the popcorn…',
  'Polishing the lens…',
  'Cueing up the opening credits…',
  'Warming up the decoder…',
  'Adjusting the focus…',
  'Sweeping the popcorn aisle…',
  'Final preparations…',
  'Almost ready, hold tight…',
  'Negotiating with the codec gods…',
  'Convincing HEVC to play nice…',
  'Buffering more cinema magic…',
];

// Map of human label → MIME type with codec parameters, used to probe what
// THIS browser can actually decode. Lets the info popup explain why we're
// transcoding (or remuxing) for the specific client.
const CODEC_PROBES: { label: string; mime: string }[] = [
  { label: 'H.264 (AVC)',  mime: 'video/mp4; codecs="avc1.42E01E"' },
  { label: 'H.265 (HEVC)', mime: 'video/mp4; codecs="hvc1.1.6.L93.B0"' },
  { label: 'VP9',          mime: 'video/webm; codecs="vp9"' },
  { label: 'AV1',          mime: 'video/mp4; codecs="av01.0.05M.08"' },
  { label: 'AAC',          mime: 'audio/mp4; codecs="mp4a.40.2"' },
  { label: 'MP3',          mime: 'audio/mpeg' },
  { label: 'Opus',         mime: 'audio/webm; codecs="opus"' },
  { label: 'AC-3',         mime: 'audio/mp4; codecs="ac-3"' },
  { label: 'E-AC-3',       mime: 'audio/mp4; codecs="ec-3"' },
  { label: 'DTS',          mime: 'audio/mp4; codecs="dts"' },
];

function probeClientCodecs(): { label: string; supported: boolean }[] {
  const v = typeof document !== 'undefined' ? document.createElement('video') : null;
  return CODEC_PROBES.map(({ label, mime }) => {
    let supported = false;
    if (typeof MediaSource !== 'undefined' && MediaSource.isTypeSupported) {
      supported = MediaSource.isTypeSupported(mime);
    }
    // canPlayType returns "" / "maybe" / "probably". Use it as a second
    // signal — MSE can decline things <video> handles natively (e.g.
    // some MP3 / AC-3 paths).
    if (!supported && v) {
      const cpt = v.canPlayType(mime);
      supported = cpt === 'probably' || cpt === 'maybe';
    }
    return { label, supported };
  });
}

interface PlayerPageProps {
  itemId: string;
}

/**
 * Standalone HTML5 player page. Opens in its own tab from MediaCard via
 * /player/<itemId>. Mounts under AuthGate, so the bearer is always present.
 *
 * Subtitle list comes from `/api/v1/items/<id>/subtitles` (best-effort —
 * 404 / empty means we just don't show the captions menu).
 */
export function PlayerPage({ itemId }: PlayerPageProps) {
  const auth = useAuth();
  const videoRef = useRef<HTMLVideoElement | null>(null);
  const wrapRef = useRef<HTMLDivElement | null>(null);
  const hideTimer = useRef<number | null>(null);

  const [subs, setSubs] = useState<Subtitle[]>([]);
  // Multi-select: up to 2 simultaneous subtitle tracks (e.g. learner pairs
  // English + Chinese). Index 0 → native <track mode="showing">, index 1 →
  // <track mode="hidden"> whose cues are mirrored to a custom overlay
  // above the native captions area because the browser only renders ONE
  // showing TextTrack at a time.
  const [activeSubIds, setActiveSubIds] = useState<string[]>([]);
  // Live offset in seconds applied to every cue on the active tracks.
  // Mutating cue.startTime / endTime is destructive, so we track the
  // last-applied delta in a ref and only apply the *difference* on each
  // change — that keeps the cues aligned with the original VTT after
  // multiple nudges without re-fetching the file.
  const [subOffsetSec, setSubOffsetSec] = useState(0);
  const appliedSubOffsetRef = useRef(0);
  // Cues mirrored from the secondary track on every cuechange. Rendered
  // by the overlay below the native captions.
  const [secondaryCueText, setSecondaryCueText] = useState<string>('');
  const [title, setTitle] = useState<string>('');

  const [playing, setPlaying] = useState(false);
  const [muted, setMuted] = useState(false);
  const [volume, setVolume] = useState(1);
  const [current, setCurrent] = useState(0);
  const [duration, setDuration] = useState(0);
  // Buffered-ahead percentage relative to the catalog/effective duration.
  // Updated on the video's onProgress (the spec event for "more bytes
  // arrived"). Drives the lighter overlay on the seek bar so the user
  // can see how far the network has fetched past the playhead.
  const [bufferedPct, setBufferedPct] = useState(0);
  const [fullscreen, setFullscreen] = useState(false);
  const [chromeVisible, setChromeVisible] = useState(true);
  // Single menu-open key — at most one popover is open at any time.
  // Clicking another menu's trigger replaces the current one instead
  // of stacking. Outside-click + Esc set it to null; the auto-hide
  // timer is suspended while it's non-null so a menu can't disappear
  // mid-selection.
  type MenuKey = 'audio' | 'quality' | 'speed' | 'captions';
  const [openMenu, setOpenMenu] = useState<MenuKey | null>(null);
  const toggleMenu = (m: MenuKey) => setOpenMenu((o) => (o === m ? null : m));
  const audioMenuOpen = openMenu === 'audio';
  const qualityMenuOpen = openMenu === 'quality';
  const speedMenuOpen = openMenu === 'speed';
  const showCaptionsMenu = openMenu === 'captions';
  // (legacy showCaptionsMenu/setShowCaptionsMenu removed — driven by `openMenu` above.)
  // Start in the buffering state so the loading overlay is visible the
  // instant PlayerPage mounts — before <video> has had a chance to fire
  // its own `waiting` event. Cleared once the first frame is decoded
  // (onPlaying / onCanPlay).
  const [buffering, setBuffering] = useState(true);
  const [autoplayMuted, setAutoplayMuted] = useState(false);
  const [needsClickToPlay, setNeedsClickToPlay] = useState(false);

  // True movie duration from the catalogue / probe. Fragmented MP4 streams
  // (transcode + remux modes) don't carry a reliable duration in the moov
  // atom, so <video>.duration is often NaN or 0:01 — we fall back to this.
  const [apiDurationSec, setApiDurationSec] = useState(0);
  // Pick the reliable source. Order of preference:
  //   1. video.duration — the HLS playlist sums each segment's
  //      EXTINF, which matches the actually-playable window.
  //   2. apiDurationSec — populated ONLY from /play/info (ffprobe on
  //      the source), not from the item-level duration which TMDB
  //      rounds up to the nearest minute and would misalign the
  //      intro/credits overlays.
  // Fragmented-MP4 modes still hit case 2 because video.duration
  // stays unset until the first manifest parse.
  const effectiveDuration = useMemo(() => {
    const fromVideo = isFinite(duration) && duration > 0 ? duration : 0;
    return fromVideo > 0 ? fromVideo : apiDurationSec;
  }, [duration, apiDurationSec]);

  // Refs mirroring apiDurationSec / streamOffsetSec — handlers
  // registered in useEffects with `[]` deps read these to get
  // always-current values (closures capture mount-time state).
  const apiDurationSecRef = useRef(apiDurationSec);
  useEffect(() => { apiDurationSecRef.current = apiDurationSec; }, [apiDurationSec]);
  const streamOffsetSecRef = useRef(0);

  const [info, setInfo] = useState<PlayInfo | null>(null);
  const [infoOpen, setInfoOpen] = useState(false);
  const clientCodecs = useMemo(probeClientCodecs, []);

  // Adaptive-bitrate state. streamQuality is the rung we're currently
  // asking ffmpeg for; streamOffsetSec is how many seconds into the movie
  // the current stream STARTS — set when we switch quality mid-playback
  // so the displayed clock + slider stay correct.
  const [streamQuality, setStreamQuality] = useState<Quality>('high');
  const [streamOffsetSec, setStreamOffsetSec] = useState(0);
  // Mirror into the ref declared above so closures in []-deps effects
  // see live values.
  useEffect(() => { streamOffsetSecRef.current = streamOffsetSec; }, [streamOffsetSec]);
  // (legacy qualityMenuOpen/setQualityMenuOpen removed — driven by `openMenu` above.)
  const [qualityNotice, setQualityNotice] = useState<string | null>(null);
  const stallCountRef = useRef<{ count: number; firstAt: number }>({ count: 0, firstAt: 0 });
  // Wall-clock timestamp of the last auto-downgrade. Prevents
  // back-to-back downgrades when the new (lower) quality also stalls
  // a few times before its own buffer fills — keeps each downgrade
  // distinct rather than cascading high → medium → low in one minute.
  const lastAutoDowngradeAtRef = useRef(0);
  // Snapshot of v.currentTime at the moment of the last `waiting`
  // event. If currentTime advanced between the previous waiting and
  // this one, the buffer underrun was followed by recovery — not a
  // real stall worth counting toward a downgrade.
  const lastWaitingCtRef = useRef(-1);
  // forceTranscode flips on when a passthrough/remux stream stalls badly
  // on a flaky network — we ask the stream service to upgrade to a
  // transcoded pipeline so ffmpeg can input-seek to the resume position
  // (passthrough can't seek by time) AND the quality switcher becomes
  // available to step bandwidth down.
  const [forceTranscode, setForceTranscode] = useState(false);

  // When the user switches BACK to Direct mode from a transcoded
  // pipeline, the browser resets currentTime to 0 because the <video
  // src> changes. Stash the pre-switch wall-clock time here and replay
  // it via the loadedmetadata handler — for passthrough that triggers
  // a single Range request to the right byte offset.
  const pendingSeekRef = useRef<number | null>(null);

  // User-intent gate for the loadedmetadata autoplay path. Every <video
  // src> change refires loadedmetadata, and tryAutoplay() there will
  // resume the video — that's wrong when the user had explicitly hit
  // pause and the src change was internal (token silent-renew, quality
  // switch, audio switch). Defaults to true (page opens with the intent
  // to watch); explicit user pause flips it false, explicit user play
  // flips it back. NOT touched by the native onPause/onPlay listeners
  // because those fire on browser-initiated transitions too (buffering,
  // stall, src reload), which would defeat the gate.
  const userWantsPlayingRef = useRef(true);
  const userTogglePlay = () => {
    const v = videoRef.current;
    if (!v) return;
    if (v.paused) {
      userWantsPlayingRef.current = true;
      v.play().catch(() => undefined);
    } else {
      userWantsPlayingRef.current = false;
      v.pause();
    }
  };

  // Switch history: every mode / quality transition gets a timestamp +
  // reason. Displayed in the Playback info dialog so the user can see
  // when and why the player flipped pipelines (e.g. bandwidth-triggered
  // auto-downgrade). Capped at 16 entries.
  const [switchHistory, setSwitchHistory] = useState<SwitchEntry[]>([]);
  const recordSwitch = (label: string, reason: SwitchEntry['reason'], detail?: string) => {
    setSwitchHistory((h) => [...h, { ts: Date.now(), label, reason, detail }].slice(-16));
  };

  // Cycle through funny messages while we're waiting for bytes.
  const [loadingMsgIdx, setLoadingMsgIdx] = useState(0);
  // One-off label that overrides the cycling LOADING_MESSAGES while the
  // player is reacting to a user action (seek, quality switch) or the
  // initial mount. Cleared by the next `playing` event.
  const [actionLabel, setActionLabel] = useState<string | null>('Preparing your movie…');

  // ---- Telemetry batcher + resume + stall recovery state ----
  // sessionId stays stable for the whole player mount so server-side
  // analytics can group events.
  const sessionIdRef = useRef<string>(crypto.randomUUID());
  // Buffered telemetry events queued by reportEvent(); flushed every
  // 30s (interval) or on pagehide / unmount.
  const telemetryQueueRef = useRef<TelemetryEvent[]>([]);
  // Single-shot guard on the resume fetch.
  const [resumeChecked, setResumeChecked] = useState(false);
  // Force document body to pure black while the player is mounted so
  // any sliver around the wrap (notch / cutout / browser address-bar
  // gap when collapsing) shows as black instead of the chino dark
  // #0d1117. Also forbid scrolling — touch drags shouldn't move the
  // page. Restored on unmount.
  useEffect(() => {
    const html = document.documentElement;
    const body = document.body;
    const prev = {
      htmlBg: html.style.backgroundColor,
      bodyBg: body.style.backgroundColor,
      bodyOverflow: body.style.overflow,
    };
    html.style.backgroundColor = '#000';
    body.style.backgroundColor = '#000';
    body.style.overflow = 'hidden';
    return () => {
      html.style.backgroundColor = prev.htmlBg;
      body.style.backgroundColor = prev.bodyBg;
      body.style.overflow = prev.bodyOverflow;
    };
  }, []);

  // .chino-player-fill toggles object-fit: cover when the viewport
  // aspect doesn't match the source aspect — typical scenario is
  // phone-portrait viewport (9:21) playing a landscape source (16:9):
  // default contain leaves giant letterbox bars top/bottom; cover
  // crops left/right a little but fills the screen. Re-evaluates on
  // resize + orientation change + when the video's intrinsic
  // dimensions become known.
  const [videoFill, setVideoFill] = useState(false);
  useEffect(() => {
    const v = videoRef.current;
    if (!v) return;
    const recompute = () => {
      const vw = v.videoWidth;
      const vh = v.videoHeight;
      if (!vw || !vh) { setVideoFill(false); return; }
      const sourceAspect = vw / vh;
      const viewportAspect = window.innerWidth / window.innerHeight;
      // Container portrait + source landscape → letterbox would
      // happen, switch to cover. Otherwise keep contain (which is
      // the right call for matched-aspect or wider-container cases).
      setVideoFill(viewportAspect < 1 && sourceAspect > 1.1);
    };
    recompute();
    v.addEventListener('loadedmetadata', recompute);
    window.addEventListener('resize', recompute);
    window.addEventListener('orientationchange', recompute);
    return () => {
      v.removeEventListener('loadedmetadata', recompute);
      window.removeEventListener('resize', recompute);
      window.removeEventListener('orientationchange', recompute);
    };
  }, []);

  // Cache-bust counter mixed into playUrl after a long tab suspension.
  // Bumping it changes the URL → useEffect tears hls down + rebuilds
  // → fresh master.m3u8 + init + segments. Mobile browsers freeze
  // backgrounded tabs and segments served before the freeze are gone
  // from chino-stream's cache by the time the user comes back; hls.js
  // can't recover that on its own.
  const [reloadKey, setReloadKey] = useState(0);
  // Hard-stall detector: every 2s we sample currentTime; if it hasn't
  // advanced for ~8s while we think we're playing, show the
  // "reconnecting" overlay and force a reload of the src.
  const stallWatchRef = useRef<{ lastT: number; lastChange: number }>({ lastT: 0, lastChange: 0 });
  const [reconnecting, setReconnecting] = useState(false);

  // ---- Audio track selection ----
  // streamAudioIdx is the audio-stream ordinal currently being mapped by
  // ffmpeg (0-based within audio streams). 0 = file's default audio
  // track. Changing it restarts the stream just like a quality switch.
  const [streamAudioIdx, setStreamAudioIdx] = useState(0);
  // (legacy audioMenuOpen/setAudioMenuOpen removed — driven by `openMenu` above.)

  // ---- Playback speed ----
  // Pure client-side: setting video.playbackRate doesn't touch the
  // stream pipeline (no re-fetch, no quality drop, no transcode work).
  // 1 = normal. preservesPitch defaults true in Chromium 100+, which
  // is what we want — sped-up voice stays intelligible.
  const PLAYBACK_RATES = [0.5, 0.75, 1, 1.25, 1.5, 1.75, 2] as const;
  const [playbackRate, setPlaybackRate] = useState(1);
  // (legacy speedMenuOpen/setSpeedMenuOpen removed — driven by `openMenu` above.)
  // Push the current rate onto the live <video> whenever the user
  // picks a new value. The onLoadedMetadata handler also re-applies
  // it after an internal src swap (quality switch / token rotation /
  // tab-resume reload) so a sticky 1.5x carries through.
  useEffect(() => {
    const v = videoRef.current;
    if (!v) return;
    try { v.playbackRate = playbackRate; } catch { /* ignore */ }
  }, [playbackRate]);

  // ---- Segments + next-episode state ----
  // Loaded for every item; the "Skip Intro" / "Skip Credits" UI only
  // surfaces when there's data to back it up.
  const [segments, setSegments] = useState<{ kind: string; start_ms: number; end_ms: number; label?: string }[]>([]);
  // Series id is set when the player learns this item is an episode.
  // next-episode is fetched lazily, the first time we enter the credits
  // segment, so we don't blow a request on every play.
  const [parentSeriesId, setParentSeriesId] = useState<string | null>(null);
  // Sibling episodes within the same series, ordered by season+episode.
  // Populated when the current item has a parent_id; powers the
  // prev/next episode controls in the player chrome and the "previous"
  // counterpart to the existing auto-play-next.
  const [siblingEpisodes, setSiblingEpisodes] = useState<{ id: string; season: number; episode: number; title: string }[]>([]);
  const [nextEp, setNextEp] = useState<{ id: string; title: string; season_number?: number; episode_number?: number } | null>(null);
  const [nextEpFetched, setNextEpFetched] = useState(false);
  // Auto-play-next countdown. Active while we're inside a credits segment
  // AND we have a next episode AND the user hasn't cancelled.
  const [autoNextSec, setAutoNextSec] = useState<number | null>(null);
  const [autoNextDismissed, setAutoNextDismissed] = useState(false);

  // Binge-mode auto-skip intro state. Same shape as autoNext: a
  // descending second counter that fires the skip on 0, plus a flag
  // for the user's "Watch intro" cancel.
  const [autoSkipIntroSec, setAutoSkipIntroSec] = useState<number | null>(null);
  const [autoSkipIntroDismissed, setAutoSkipIntroDismissed] = useState(false);

  // Pre-skip-at-prepare. Captured once at mount from `?binge=1`; the
  // resume effect strips the URL flag, so we cache it here for the
  // segments effect below to consume. Cleared after the pre-skip
  // applies so a later segments refresh doesn't double-skip.
  const bingePreSkipPendingRef = useRef<boolean>(false);
  useEffect(() => {
    bingePreSkipPendingRef.current = new URLSearchParams(window.location.search).get('binge') === '1';
  }, []);

  // User-tuned binge settings (read once at mount; the hook re-fires
  // on chino:settings-changed so the player picks up updates without
  // a reload). isBingeContin gates the auto-skip behaviour to *only*
  // the "you're watching another episode of this series within the
  // 6-hour window" case — fresh sessions still see the intro.
  const [settings] = useSettings();
  const isBingeContin = useMemo(
    () => isBingeContinuation(parentSeriesId ?? undefined, itemId),
    [parentSeriesId, itemId],
  );

  // Displayed playhead lives below the segments effect block (it needs
  // streamOffsetSec, which is in scope there). Adding a here-only stub
  // would shadow it.

  const token = auth.user?.access_token;

  // Shared stream token — minted once per session via
  // /me/stream-token, cached in sessionStorage (see useStreamToken).
  // Used in <video src>, <track src> for embedded subs, and (by
  // useItems / useItem / useContinueWatching) in <img src> for
  // posters / backdrops. The 6 h TTL means OIDC silent renewals
  // don't rotate any of those URLs.
  const streamToken = useStreamToken() ?? undefined;

  // Build the capability beacon the server uses to pick a pipeline.
  // Comma-separated tokens; missing token means "not supported".
  // Server's ParseCaps recognises the same vocabulary
  // (chino-stream/internal/play/ffprobe.go::ParseCaps).
  //
  // Strategy: optimistic-include synchronously based on
  // MediaSource.isTypeSupported, then asynchronously refine via
  // MediaCapabilities. Optimistic-include avoids the race where the
  // first /info + master.m3u8 fetch goes out without hvc, server
  // picks the wrong pipeline (remux instead of packaged for HEVC),
  // and the badge briefly shows the wrong mode before the async
  // capability check catches up.
  //
  // Two intentional omissions:
  //   * `aacmc` (multi-channel AAC LC) is NEVER advertised. Chrome
  //     returns true from `isTypeSupported('mp4a.40.2; channels="6"')`
  //     but its MSE pipeline rejects 5.1/7.1 fmp4 segments with
  //     bufferAppendError. Always-downmix is the safe default; a
  //     ~5% audio re-encode tax beats a forced libx264 fallback.
  //   * For `hvc`: if isTypeSupported says yes (most Android phones,
  //     Safari, modern Chromebooks) we include it immediately. If the
  //     async MediaCapabilities probe later says supported=false (the
  //     Chrome/Windows lie scenario) we strip it and the player
  //     re-requests with the corrected caps.
  const isCodecSupported = (mime: string) =>
    typeof MediaSource !== 'undefined' && MediaSource.isTypeSupported?.(mime);
  const initialCaps = useMemo(() => {
    const t: string[] = [];
    if (isCodecSupported('video/mp4; codecs="avc1.640028"')) t.push('avc');
    if (isCodecSupported('video/mp4; codecs="hvc1.1.6.L120.B0"')) t.push('hvc');
    if (isCodecSupported('video/mp4; codecs="av01.0.05M.08"')) t.push('av1');
    if (isCodecSupported('video/webm; codecs="vp9"')) t.push('vp9');
    if (isCodecSupported('audio/mp4; codecs="mp4a.40.2"')) t.push('aac');
    if (isCodecSupported('audio/mpeg')) t.push('mp3');
    if (isCodecSupported('audio/mp4; codecs="opus"')) t.push('opus');
    if (isCodecSupported('audio/mp4; codecs="ac-3"')) t.push('ac3');
    if (isCodecSupported('audio/mp4; codecs="ec-3"')) t.push('eac3');
    return t;
  }, []);
  const [capsParam, setCapsParam] = useState(() => initialCaps.join(','));
  useEffect(() => {
    if (!initialCaps.includes('hvc')) return;
    const mc = (navigator as Navigator & {
      mediaCapabilities?: { decodingInfo(cfg: object): Promise<{ supported: boolean; smooth: boolean }> };
    }).mediaCapabilities;
    if (!mc?.decodingInfo) return;
    mc.decodingInfo({
      type: 'media-source',
      video: {
        contentType: 'video/mp4; codecs="hvc1.1.6.L120.B0"',
        width: 1920,
        height: 1080,
        bitrate: 5_000_000,
        framerate: 24,
      },
    })
      .then((res) => {
        // Only DOWNGRADE — if the deeper probe says the browser can't
        // actually decode HEVC, strip the optimistic include. Don't
        // gate on `smooth`; on real devices HEVC plays fine even when
        // MediaCapabilities marks it not-smooth (conservative heuristic).
        if (!res.supported) {
          setCapsParam(initialCaps.filter((c) => c !== 'hvc').join(','));
        }
      })
      .catch(() => undefined);
  }, [initialCaps]);

  // playUrl points at the HLS master playlist — hls.js fetches the
  // playlists + segments from there, and stitches the segments into a
  // single seekable timeline on the <video> element.
  //
  // The server's master.m3u8 emits a SINGLE video variant matching
  // ?q=. Changing streamQuality flips the URL, which forces hls.js to
  // tear down + reload. switchQuality stashes the current position in
  // pendingSeekRef so loadedmetadata can replay it. The URL stays
  // stable across token refreshes (the stream token has a 6 h TTL).
  const playUrl = useMemo(() => {
    if (!streamToken) return '';
    const params = new URLSearchParams({ stream: streamToken, q: streamQuality });
    if (capsParam) params.set('caps', capsParam);
    if (reloadKey > 0) params.set('_r', String(reloadKey));
    const url = `/api/v1/items/${itemId}/play/master.m3u8?${params.toString()}`;
    // eslint-disable-next-line no-console
    console.log('[playUrl] recomputed', { itemId, q: streamQuality, caps: capsParam, reload: reloadKey, tokenHash: streamToken.slice(0, 8) });
    return url;
  }, [itemId, streamToken, streamQuality, capsParam, reloadKey]);

  // Trickplay (scrub-preview thumbnails). The analyzer writes a
  // thumbnails.vtt + sprite-NNNN.jpg set for every packaged item; the
  // player fetches the VTT once and uses the cues to render a small
  // sprite-cropped thumbnail above the scrub-bar cursor on hover.
  // Items that haven't been packaged yet (or that the analyzer
  // skipped, e.g. too-short videos) just return 404 here — we treat
  // an empty cue list as "no preview available" and render nothing.
  const [trickplayCues, setTrickplayCues] = useState<TrickplayCue[]>([]);
  useEffect(() => {
    if (!streamToken) return;
    // Trickplay assets only exist for packaged items (the analyzer
    // emits them alongside the CMAF tree). Skip the fetch entirely
    // for on-demand transcode / passthrough / remux items — otherwise
    // every play page logs a 404 on /trickplay/thumbnails.vtt for
    // items that simply don't have it yet. Wait for /play/info to
    // tell us the mode before deciding.
    if (info && info.mode !== 'packaged') {
      setTrickplayCues([]);
      return;
    }
    if (!info) return; // wait for the info probe to resolve
    const enc = encodeURIComponent(streamToken);
    const url = `/api/v1/items/${itemId}/play/trickplay/thumbnails.vtt?stream=${enc}`;
    const ctrl = new AbortController();
    fetch(url, { signal: ctrl.signal })
      .then((r) => (r.ok ? r.text() : ''))
      .then((text) => {
        if (text) setTrickplayCues(parseTrickplayVTT(text));
        else setTrickplayCues([]);
      })
      .catch(() => setTrickplayCues([]));
    return () => ctrl.abort();
  }, [itemId, streamToken, info]);
  const trickplayBaseUrl = useMemo(() => {
    if (!streamToken) return '';
    return `/api/v1/items/${itemId}/play/trickplay`;
  }, [itemId, streamToken]);

  // Scrub-bar hover state. mouseX is the cursor's offset inside the
  // bar; hoveredSec is the corresponding playhead position. Both null
  // when the cursor isn't over the bar (so the preview hides).
  const seekBarRef = useRef<HTMLDivElement>(null);
  const [hover, setHover] = useState<{ mouseX: number; sec: number } | null>(null);

  // Hls instance attached to videoRef. Lives as long as the player
  // mount; rebuilt when playUrl changes (token rotation forces a new
  // master URL → new playlist load). Safari has native HLS so we
  // bypass hls.js there and set v.src directly.
  const hlsRef = useRef<Hls | null>(null);

  // Attach hls.js (or Safari's native HLS) to the <video> element when
  // playUrl is ready. Tears down on unmount or playUrl change.
  useEffect(() => {
    const v = videoRef.current;
    if (!v || !playUrl) return;
    // eslint-disable-next-line no-console
    console.log('[hls-effect] SETUP', playUrl.slice(0, 100));
    // Prefer hls.js (MSE-based) over native HLS — Chrome's
    // canPlayType('application/vnd.apple.mpegurl') returns "maybe"
    // but doesn't actually decode the playlist correctly. Only fall
    // back to native HLS when hls.js isn't supported AND the browser
    // reports it CAN play HLS (Safari / iOS).
    if (!Hls.isSupported()) {
      if (v.canPlayType('application/vnd.apple.mpegurl')) {
        v.src = playUrl;
        return () => { v.removeAttribute('src'); v.load(); };
      }
      // eslint-disable-next-line no-console
      console.error('[player] no HLS support: hls.js unsupported AND no native HLS');
      return;
    }
    const hls = new Hls({
      // 5-minute buffer ahead of the playhead with a 500 MB memory cap.
      // Generous enough to ride out WiFi roams, brief upstream stalls,
      // or a katalog-stream pod restart without rebuffering — on a fast
      // LAN the byte cap is what bounds growth, not the time cap. The
      // tradeoff: katalog-stream transcodes segments on demand, so
      // pre-buffering 5 min worth pays the transcode cost up front. If
      // the user bails right after starting, that work is wasted (the
      // disk cache amortizes it only for re-watches).
      maxBufferLength: 300,
      maxMaxBufferLength: 600,
      maxBufferSize: 500 * 1000 * 1000,
      backBufferLength: 60,
      // Retry knobs — segments may briefly 502 if a katalog-stream pod
      // is restarting; hls.js's defaults are reasonable but bump retry
      // count slightly for our HPA fan-out.
      manifestLoadingRetryDelay: 1000,
      manifestLoadingMaxRetry: 4,
      fragLoadingRetryDelay: 1000,
      fragLoadingMaxRetry: 6,
    });
    hls.attachMedia(v);
    let loadCount = 0;
    hls.on(Hls.Events.MEDIA_ATTACHED, () => {
      loadCount += 1;
      // eslint-disable-next-line no-console
      console.log('[hls] MEDIA_ATTACHED → loadSource #' + loadCount);
      hls.loadSource(playUrl);
    });
    hls.on(Hls.Events.MEDIA_DETACHED, () => {
      // eslint-disable-next-line no-console
      console.log('[hls] MEDIA_DETACHED');
    });
    // Circuit breaker — without this, a chronic codec / MSE-append
    // error sends recoverMediaError into a hot loop that hammers
    // master.m3u8 30+ times per second. Cap at 3 attempts inside a
    // 10 s window; after that, fall back to forced transcode (a
    // freshly libx264-encoded h264 stream is the safest fallback) or
    // surface a visible error if we're already on transcode.
    let mediaRecoverCount = 0;
    let mediaRecoverFirst = 0;
    let networkRetryCount = 0;
    let networkRetryFirst = 0;
    hls.on(Hls.Events.ERROR, (_, data) => {
      // Log every error (fatal and non-fatal) so we can see what hls.js
      // is silently retrying. The verbose channel is rate-limited by
      // browser console deduplication anyway.
      // eslint-disable-next-line no-console
      console.warn('[hls] err', data.fatal ? 'FATAL' : 'soft', data.type, data.details, data.reason || '', { frag: data.frag?.sn });
      if (!data.fatal) {
        // bufferAppendError on init or fragment is a permanent failure
        // for this src — Chrome rejects the MSE append, the segment
        // can't be played, and hls.js retries forever in the background
        // without firing fatal. Treat it as fatal-equivalent and let
        // the circuit breaker below force a transcode fallback.
        if (data.details === 'bufferAppendError' || data.details === 'bufferAppendingError') {
          // Cast to mutable so we can re-enter the fatal branch below.
          (data as { fatal?: boolean }).fatal = true;
        } else {
          return;
        }
      }
      // eslint-disable-next-line no-console
      console.error('[hls] fatal', data.type, data.details, data.reason || '', { frag: data.frag?.sn });
      // Telemetry — every fatal hls error gets reported with type,
      // details, fragment number and the failing URL. Lets us
      // correlate client-visible playback failures with chino-stream
      // 502/404 spikes (e.g. the OOMKill window).
      reportEvent('hls_fatal', {
        type: data.type,
        details: data.details,
        reason: data.reason || '',
        frag: data.frag?.sn ?? null,
        url: (data.frag as { url?: string } | undefined)?.url || (data as { url?: string }).url || null,
        httpStatus: (data.response as { code?: number } | undefined)?.code ?? null,
      });
      const now = performance.now();
      switch (data.type) {
        case Hls.ErrorTypes.NETWORK_ERROR:
          if (now - networkRetryFirst > 10_000) { networkRetryCount = 0; networkRetryFirst = now; }
          networkRetryCount += 1;
          if (networkRetryCount > 3) {
            hls.destroy();
            setActionLabel('Stream unreachable — please check connection');
            reportEvent('circuit_breaker', { kind: 'network', attempts: networkRetryCount, lastDetails: data.details });
            return;
          }
          hls.startLoad();
          break;
        case Hls.ErrorTypes.MEDIA_ERROR:
          if (now - mediaRecoverFirst > 10_000) { mediaRecoverCount = 0; mediaRecoverFirst = now; }
          mediaRecoverCount += 1;
          if (mediaRecoverCount > 3) {
            hls.destroy();
            if (!forceTranscode) {
              setForceTranscode(true);
              setStreamQuality('medium');
              setActionLabel('Codec issue — switching to transcode…');
              reportEvent('circuit_breaker', { kind: 'media_to_transcode', attempts: mediaRecoverCount, lastDetails: data.details });
            } else {
              setActionLabel('Playback failed: media decode error');
              reportEvent('circuit_breaker', { kind: 'media_terminal', attempts: mediaRecoverCount, lastDetails: data.details });
            }
            return;
          }
          hls.recoverMediaError();
          break;
        default:
          hls.destroy();
          reportEvent('circuit_breaker', { kind: 'other', type: data.type, lastDetails: data.details });
      }
    });
    hlsRef.current = hls;
    return () => {
      // eslint-disable-next-line no-console
      console.log('[hls-effect] CLEANUP', playUrl.slice(0, 100));
      hls.destroy();
      hlsRef.current = null;
    };
  }, [playUrl]);

  // What the server is ACTUALLY doing right now. info.mode is the
  // initial probe; once we've requested ?t=<sec> or set
  // forceTranscode, the server upgrades the pipeline to transcode
  // regardless of codec compatibility. The quality switcher and
  // auto-downgrade logic key off this, not info.mode.
  const effectiveMode: PlayInfo['mode'] = useMemo(() => {
    if (forceTranscode || streamOffsetSec > 0) return 'transcode';
    return info?.mode ?? 'passthrough';
  }, [info?.mode, forceTranscode, streamOffsetSec]);

  // Fetch item title + subtitles list.
  useEffect(() => {
    if (!token) return;
    const ctrl = new AbortController();
    fetch(`/api/v1/items/${itemId}`, {
      signal: ctrl.signal,
      headers: { Authorization: `Bearer ${token}` },
    })
      .then((r) => (r.ok ? r.json() : null))
      .then((j) => {
        if (j?.title) setTitle(j.title);
        // NOTE: we do NOT call setApiDurationSec here. The item-level
        // duration_ms comes from TMDB enrichment and is rounded up to
        // the nearest minute (24:00 instead of the actual 23:40 etc).
        // Using it would inflate effectiveDuration and misalign the
        // credits / intro overlays against the actual playable
        // window. /play/info reports the ffprobe-derived true value;
        // hls.js fills in video.duration once the manifest parses.
        // Series episodes carry a parent_id we use for /next-episode.
        if (j?.type === 'episode' && j?.parent_id) setParentSeriesId(j.parent_id);
      })
      .catch(() => undefined);

    // Segments for Skip-Intro / Skip-Credits overlays + scrub-bar ticks.
    fetch(`/api/v1/items/${itemId}/segments`, {
      signal: ctrl.signal,
      headers: { Authorization: `Bearer ${token}` },
    })
      .then((r) => (r.ok ? r.json() : null))
      .then((j) => {
        if (Array.isArray(j?.segments)) setSegments(j.segments);
      })
      .catch(() => undefined);

    // ?caps= mirrors what playUrl sends so the pipeline /play/info
    // describes matches what /play/master.m3u8 will actually
    // dispatch. Skip the fetch entirely until capsParam is computed
    // — an empty-caps /info call falls back to DefaultCaps server-
    // side (no hvc), reports mode=transcode for HEVC items, and the
    // race against the with-caps response can stick mode=transcode
    // even though master.m3u8 ends up serving the packaged HEVC
    // stream correctly.
    if (!capsParam) return () => ctrl.abort();
    const infoQs = `?caps=${encodeURIComponent(capsParam)}`;
    fetch(`/api/v1/items/${itemId}/play/info${infoQs}`, {
      signal: ctrl.signal,
      headers: { Authorization: `Bearer ${token}` },
    })
      .then((r) => (r.ok ? (r.json() as Promise<PlayInfo>) : null))
      .then((j) => {
        if (!j) return;
        setInfo(j);
        if (j.duration_ms) setApiDurationSec(j.duration_ms / 1000);
        // ffprobe marks the file's default audio track via the
        // disposition flag. Honour it on first mount so a file whose
        // primary audio isn't index-0 (rare but happens with
        // commentary-first MKVs) starts on the intended track.
        const defaultAudio = (j.audio_tracks ?? []).find((t) => t.default);
        if (defaultAudio && defaultAudio.index !== 0) {
          setStreamAudioIdx(defaultAudio.index);
        }
        // Seed the switch history with the initial pipeline so the
        // Playback info dialog has a baseline entry even before any
        // user-initiated or auto-recover switch happens.
        const initialLabel =
          j.mode === 'transcode' ? `Transcode (${j.video_codec.toUpperCase()} → H.264)` :
          j.mode === 'remux'     ? `Remux (${j.container} → MP4)` :
          j.mode === 'packaged'  ? `Packaged (${j.video_codec.toUpperCase()})` :
                                    `Direct (${j.video_codec.toUpperCase()})`;
        recordSwitch(initialLabel, 'startup', j.reason);
      })
      .catch(() => undefined);

    fetch(`/api/v1/items/${itemId}/subtitles`, {
      signal: ctrl.signal,
      headers: { Authorization: `Bearer ${token}` },
    })
      .then((r) => (r.ok ? (r.json() as Promise<SubtitlesResponse>) : { subtitles: [] }))
      .then((j) => {
        const list = (j?.subtitles ?? []).map((s) => ({
          ...s,
          url: s.url.includes('?') ? `${s.url}&token=${encodeURIComponent(token)}` : `${s.url}?token=${encodeURIComponent(token)}`,
        }));
        // Leave activeSubId untouched here — the merged-subs effect
        // below decides whether to apply a sidecar default, an
        // embedded track, or a user preference once both sources have
        // landed.
        setSubs(list);
      })
      .catch(() => setSubs([]));

    return () => ctrl.abort();
  }, [itemId, token, capsParam]);

  // Merge sidecar subs (from /subtitles) and embedded subs (from
  // /play/info.subtitle_tracks) into a single list the menu can render.
  // Each entry gets a stable id ('emb-N' for embedded, the sidecar's
  // raw id otherwise) so React reconciles them across renders without
  // collisions.
  const mergedSubs = useMemo<Subtitle[]>(() => {
    const out: Subtitle[] = subs.map((s) => ({ ...s }));
    if (info?.subtitle_tracks && streamToken) {
      const enc = encodeURIComponent(streamToken);
      // Mirror the play URL's `t=` so embedded subtitle cues stay in
      // sync after a quality switch (which restarts ffmpeg at -ss N).
      // Depending on streamOffsetSec also forces the <track> src to
      // change, which is what triggers the browser to refetch.
      const tParam = streamOffsetSec > 0
        ? `&t=${Math.floor(streamOffsetSec)}`
        : '';
      // Skip bitmap/image subtitle codecs (PGS, DVB-sub, DVD-sub,
      // XSUB). ffmpeg can't transmux those to WebVTT — the encoder
      // bails with "Subtitle encoding currently only possible from
      // text to text or bitmap to bitmap" and chino-stream returns
      // 502 on every fetch attempt. Filtering them out of the menu
      // means the user never selects an unplayable track AND we
      // never spam 502 retries during scrubbing.
      const BITMAP_SUB_CODECS = new Set([
        'hdmv_pgs_subtitle', 'pgssub',
        'dvd_subtitle', 'dvdsub',
        'dvb_subtitle', 'dvbsub',
        'xsub',
      ]);
      for (const t of info.subtitle_tracks) {
        if (BITMAP_SUB_CODECS.has((t.codec || '').toLowerCase())) continue;
        out.push({
          id: `emb-${t.index}`,
          lang: t.language || 'und',
          label:
            t.title?.trim() ||
            `${langLabel(t.language)} (embedded)`,
          url: `/api/v1/items/${itemId}/play/subtitles/${t.index}.vtt?stream=${enc}${tParam}`,
          default: t.default,
        });
      }
    }
    return out;
  }, [subs, info?.subtitle_tracks, itemId, streamToken, streamOffsetSec]);

  // Preferred-language auto-pick. Runs once per merged-list change.
  // The localStorage key honours:
  //   'off'   → user explicitly disabled subs, leave selection empty.
  //   'eng' / 'deu' / etc. → pick the first track in that language.
  //   missing → default to English; if no English track exists, leave off.
  // Multi-select state, but the auto-pick only ever seeds a single
  // track — second tracks are an explicit user opt-in via the picker.
  const autoPickedRef = useRef(false);
  useEffect(() => {
    if (mergedSubs.length === 0) return;
    if (autoPickedRef.current) return;
    if (activeSubIds.length > 0) { autoPickedRef.current = true; return; }
    const stored =
      typeof window !== 'undefined' ? window.localStorage.getItem(SUB_LANG_KEY) : null;
    if (stored === 'off') { autoPickedRef.current = true; return; }
    const wantedLang = stored || 'eng';
    const match =
      mergedSubs.find((s) => s.lang?.toLowerCase() === wantedLang.toLowerCase()) ||
      mergedSubs.find((s) => s.default);
    if (match) {
      setActiveSubIds([match.id]);
      autoPickedRef.current = true;
    }
  }, [mergedSubs, activeSubIds]);

  // Apply active subtitle selection to the <track> mode.
  //
  // Three pitfalls this guards against:
  //   1. `TextTrack.id` does NOT reliably reflect the `<track id=…>`
  //      HTML attribute — most browsers populate TextTrack.id only
  //      when the cue file explicitly sets it, and otherwise leave it
  //      as an empty string. Iterating HTMLTrackElement gives us the
  //      attribute we actually wrote.
  //   2. We render every track on every change to mergedSubs, so
  //      whenever new tracks land (e.g. embedded subs arrive after
  //      sidecars) we must reassert the mode — otherwise newly-
  //      mounted tracks default-show themselves.
  //   3. Default cue position is the bottom of the video, which sits
  //      on top of our custom controls bar when it's visible. Re-
  //      anchoring each cue at line 85 (% from top, snapToLines off)
  //      lifts subtitles into the safe zone above the controls.
  //
  // Multi-select: activeSubIds[0] is the primary track and gets
  // mode="showing" (native rendering). activeSubIds[1] is the secondary
  // — kept mode="hidden" so its cues populate but the UA doesn't paint
  // them, then mirrored to the secondaryCueText overlay below.
  useEffect(() => {
    const v = videoRef.current;
    if (!v) return;
    const liftCue = (cue: TextTrackCue) => {
      const vc = cue as VTTCue;
      // Only override the UA default ("auto"). If a cue file already
      // pins a specific line, respect that. Primary track gets lifted
      // higher to make room for the secondary overlay underneath when
      // two tracks are active.
      if (vc.line === 'auto') {
        vc.snapToLines = false;
        vc.line = activeSubIds.length > 1 ? 80 : 85;
      }
    };
    const liftAll = (track: TextTrack) => {
      const cues = track.cues;
      if (!cues) return;
      for (let i = 0; i < cues.length; i++) liftCue(cues[i]);
    };
    const primary = activeSubIds[0];
    const secondary = activeSubIds[1];
    const apply = () => {
      const trackEls = v.querySelectorAll('track');
      for (let i = 0; i < trackEls.length; i++) {
        const trackEl = trackEls[i] as HTMLTrackElement;
        if (!trackEl.track) continue;
        if (trackEl.id === primary) {
          trackEl.track.mode = 'showing';
        } else if (trackEl.id === secondary) {
          trackEl.track.mode = 'hidden';
        } else {
          trackEl.track.mode = 'disabled';
        }
        liftAll(trackEl.track);
      }
    };
    apply();
    const onAdd = (e: TrackEvent) => {
      apply();
      if (e.track) {
        e.track.addEventListener('cuechange', () => liftAll(e.track!));
      }
    };
    v.textTracks.addEventListener('addtrack', onAdd);
    return () => v.textTracks.removeEventListener('addtrack', onAdd);
  }, [activeSubIds, mergedSubs]);

  // Secondary-track cue mirror. Read `activeCues` from the hidden
  // TextTrack on each `cuechange` event and stash the joined text in
  // state — the overlay renders it. The handler re-binds whenever the
  // secondary id changes (different track) or the merged list grows
  // (new <track> element mounted).
  useEffect(() => {
    const v = videoRef.current;
    if (!v) { setSecondaryCueText(''); return; }
    const secondary = activeSubIds[1];
    if (!secondary) { setSecondaryCueText(''); return; }
    const trackEls = v.querySelectorAll('track');
    let target: TextTrack | null = null;
    for (let i = 0; i < trackEls.length; i++) {
      const trackEl = trackEls[i] as HTMLTrackElement;
      if (trackEl.id === secondary && trackEl.track) {
        target = trackEl.track;
        break;
      }
    }
    if (!target) { setSecondaryCueText(''); return; }
    const tt = target;
    const onChange = () => {
      const cues = tt.activeCues;
      if (!cues || cues.length === 0) { setSecondaryCueText(''); return; }
      const parts: string[] = [];
      for (let i = 0; i < cues.length; i++) {
        const c = cues[i] as VTTCue;
        if (c.text) parts.push(c.text);
      }
      setSecondaryCueText(parts.join('\n'));
    };
    tt.addEventListener('cuechange', onChange);
    onChange();
    return () => {
      tt.removeEventListener('cuechange', onChange);
      setSecondaryCueText('');
    };
  }, [activeSubIds, mergedSubs]);

  // Apply / re-apply the user-tuned cue time offset to every active
  // track. We mutate cue.startTime / endTime directly because there's
  // no per-cue offset API on TextTrack — the *difference* between the
  // requested offset and the last-applied offset is what gets added so
  // repeated nudges don't compound. Reset to 0 whenever the active
  // track selection changes; we first unwind any existing offset from
  // still-mounted tracks so they return to pristine VTT timings before
  // any new track joins / leaves.
  useEffect(() => {
    const v = videoRef.current;
    if (!v) { appliedSubOffsetRef.current = 0; setSubOffsetSec(0); return; }
    const prev = appliedSubOffsetRef.current;
    if (prev !== 0) {
      const trackEls = v.querySelectorAll('track');
      for (let i = 0; i < trackEls.length; i++) {
        const trackEl = trackEls[i] as HTMLTrackElement;
        if (!trackEl.track || !trackEl.track.cues) continue;
        const cues = trackEl.track.cues;
        for (let j = 0; j < cues.length; j++) {
          const c = cues[j];
          c.startTime = Math.max(0, c.startTime - prev);
          c.endTime = Math.max(c.startTime + 0.001, c.endTime - prev);
        }
      }
    }
    appliedSubOffsetRef.current = 0;
    setSubOffsetSec(0);
  }, [activeSubIds]);
  useEffect(() => {
    const v = videoRef.current;
    if (!v) return;
    const delta = subOffsetSec - appliedSubOffsetRef.current;
    if (delta === 0) return;
    const shift = (track: TextTrack) => {
      const cues = track.cues;
      if (!cues) return;
      for (let i = 0; i < cues.length; i++) {
        const c = cues[i];
        c.startTime = Math.max(0, c.startTime + delta);
        c.endTime = Math.max(c.startTime + 0.001, c.endTime + delta);
      }
    };
    const trackEls = v.querySelectorAll('track');
    for (let i = 0; i < trackEls.length; i++) {
      const trackEl = trackEls[i] as HTMLTrackElement;
      if (!trackEl.track) continue;
      if (activeSubIds.includes(trackEl.id)) shift(trackEl.track);
    }
    appliedSubOffsetRef.current = subOffsetSec;
  }, [subOffsetSec, activeSubIds]);

  // Multi-select toggle. Picking the same id twice removes it; picking
  // a third clears the oldest and slots the new one. Picking `null`
  // clears everything (the "Off" entry in the picker). The persisted
  // preference stays single-language — it's only a seed for the next
  // session, second-subtitle selection is an explicit per-mount opt-in.
  const MAX_ACTIVE_SUBS = 2;
  const chooseSub = (s: Subtitle | null) => {
    if (s === null) {
      if (typeof window !== 'undefined') {
        window.localStorage.setItem(SUB_LANG_KEY, 'off');
      }
      setActiveSubIds([]);
      return;
    }
    setActiveSubIds((prev) => {
      const idx = prev.indexOf(s.id);
      let next: string[];
      if (idx >= 0) {
        next = prev.filter((id) => id !== s.id);
      } else if (prev.length >= MAX_ACTIVE_SUBS) {
        next = [...prev.slice(1), s.id];
      } else {
        next = [...prev, s.id];
      }
      if (typeof window !== 'undefined') {
        window.localStorage.setItem(
          SUB_LANG_KEY,
          next[0] ? (mergedSubs.find((m) => m.id === next[0])?.lang || 'und') : 'off',
        );
      }
      return next;
    });
  };

  // Chrome reveal handling. Desktop: mousemove + keydown each
  // re-arm a 3 s hide. Mobile: pointerdown on the wrap toggles
  // chrome (Netflix pattern — tap once to reveal, tap once more to
  // hide). The bottom + top bars catch their own clicks so taps on
  // a button don't trigger the toggle; the underlying overlay sits
  // at z-0 below them. Without the touch path mobile users had to
  // tap blind, hope the chrome appeared, then tap again on the
  // seek bar — exactly the symptom the user reported.
  // Read chromeVisible via a ref so the effect deps don't include
  // it. The previous version had [playing, chromeVisible] AND called
  // showChrome() in its body — every auto-hide flipped chromeVisible
  // false → effect re-ran → body called showChrome() → flipped it
  // true again → 3 s later, hide → loop. Controls never stayed
  // hidden. Ref-read keeps the toggle's read-of-current-value while
  // the effect itself only re-binds on `playing` changes.
  const chromeVisibleRef = useRef(chromeVisible);
  useEffect(() => { chromeVisibleRef.current = chromeVisible; }, [chromeVisible]);
  // openMenu read via ref inside the auto-hide handler so we don't
  // have to re-bind the listeners every time a menu opens. Same anti-
  // loop pattern as chromeVisibleRef.
  const openMenuRef = useRef(openMenu);
  useEffect(() => { openMenuRef.current = openMenu; }, [openMenu]);
  useEffect(() => {
    const showChrome = () => {
      setChromeVisible(true);
      if (hideTimer.current) window.clearTimeout(hideTimer.current);
      // Don't arm a hide while a menu is open — a popover that
      // vanishes mid-pick is the most-reported UX wart on streaming
      // UIs. The menu-open effect below re-arms once the menu closes.
      if (openMenuRef.current !== null) return;
      // 3 s while playing (Netflix-style snap-away), 8 s while paused
      // (user might be reading the title / picking a chapter — but
      // shouldn't have to look at the bars forever).
      const hideAfter = playing ? 3000 : 8000;
      hideTimer.current = window.setTimeout(() => {
        setChromeVisible(false);
        setOpenMenu(null);
      }, hideAfter);
    };
    const el = wrapRef.current;
    if (!el) return;
    const onPointer = (e: PointerEvent) => {
      // Mouse hover → just reveal (don't toggle).
      if (e.pointerType === 'mouse') { showChrome(); return; }
      // Touch / pen: if chrome already visible AND the tap is on the
      // bare backdrop (not a control), hide it. Otherwise reveal.
      const target = e.target as HTMLElement | null;
      const onControl = target?.closest('button, input, [role="slider"], [role="menu"], [data-chrome-zone]');
      if (!onControl) {
        // Tapping the bare video frame would otherwise trigger the
        // browser's built-in tap-to-pause on <video> on mobile Chrome,
        // which silently pauses playback every time the user wakes
        // the chrome. preventDefault stops that — chrome
        // visibility is handled entirely by this handler.
        e.preventDefault();
      }
      if (chromeVisibleRef.current && !onControl) {
        setChromeVisible(false);
        setOpenMenu(null);
        if (hideTimer.current) window.clearTimeout(hideTimer.current);
        return;
      }
      showChrome();
    };
    // Mouse-hover reveal. Throttled to one fire per animation frame so
    // sliding the cursor across the player doesn't run setState 60 ×
    // per second.
    let moveScheduled = false;
    const onPointerMove = (e: PointerEvent) => {
      if (e.pointerType === 'touch') return;
      if (moveScheduled) return;
      moveScheduled = true;
      window.requestAnimationFrame(() => {
        moveScheduled = false;
        showChrome();
      });
    };
    // Esc closes any open menu first; otherwise just reveals chrome
    // so the user can act on a keyboard navigation prompt.
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape' && openMenuRef.current !== null) {
        setOpenMenu(null);
        e.preventDefault();
        return;
      }
      showChrome();
    };
    el.addEventListener('pointerdown', onPointer);
    el.addEventListener('pointermove', onPointerMove);
    el.addEventListener('keydown', onKey);
    showChrome();
    return () => {
      el.removeEventListener('pointerdown', onPointer);
      el.removeEventListener('pointermove', onPointerMove);
      el.removeEventListener('keydown', onKey);
      if (hideTimer.current) window.clearTimeout(hideTimer.current);
    };
  }, [playing]);

  // Re-arm the auto-hide timer whenever the menu state flips. Opening
  // a menu clears any pending hide so the popover doesn't disappear
  // mid-browse; closing it re-arms the 3-/8-second countdown so the
  // chrome doesn't sit forever after the user dismisses a menu.
  useEffect(() => {
    if (openMenu !== null) {
      if (hideTimer.current) window.clearTimeout(hideTimer.current);
      return;
    }
    if (hideTimer.current) window.clearTimeout(hideTimer.current);
    const hideAfter = playing ? 3000 : 8000;
    hideTimer.current = window.setTimeout(() => {
      setChromeVisible(false);
    }, hideAfter);
  }, [openMenu, playing]);

  // Document-level outside-click → closes any open menu when the
  // click lands outside the chrome zone. Bound only while a menu is
  // open so the listener doesn't sit in the document event path
  // during normal playback.
  useEffect(() => {
    if (openMenu === null) return;
    const onDocClick = (e: MouseEvent) => {
      const target = e.target as HTMLElement | null;
      if (!target) return;
      if (!target.closest('[data-chrome-zone], [role="menu"]')) {
        setOpenMenu(null);
      }
    };
    document.addEventListener('mousedown', onDocClick);
    return () => document.removeEventListener('mousedown', onDocClick);
  }, [openMenu]);

  useEffect(() => {
    const onFs = () => setFullscreen(!!document.fullscreenElement);
    document.addEventListener('fullscreenchange', onFs);
    return () => document.removeEventListener('fullscreenchange', onFs);
  }, []);

  // Mobile auto-fullscreen: rotating to landscape on a phone-sized
  // viewport requests fullscreen on the player wrapper; rotating
  // back to portrait exits. Skipped on tablets/desktops so a
  // landscape laptop session doesn't get hijacked. The Screen
  // Orientation API is best-effort — Safari iOS doesn't dispatch
  // 'change' on the orientation object reliably, so we listen to
  // matchMedia AND the legacy 'orientationchange' event.
  useEffect(() => {
    const wrap = wrapRef.current;
    if (!wrap) return;
    const apply = () => {
      // Mobile heuristic: short side ≤ ~600 CSS px (phones in either
      // orientation; tablets fall above this).
      const short = Math.min(window.innerWidth, window.innerHeight);
      if (short > 600) return;
      const isLandscape = window.innerWidth > window.innerHeight;
      const fs = document.fullscreenElement;
      if (isLandscape && !fs) {
        wrap.requestFullscreen?.().catch(() => undefined);
        // Lock to landscape if the API is available — keeps things
        // sane when the user re-rotates inside fullscreen.
        const orient = (screen as Screen & { orientation?: ScreenOrientation }).orientation as
          (ScreenOrientation & { lock?: (o: string) => Promise<void> }) | undefined;
        orient?.lock?.('landscape').catch(() => undefined);
      } else if (!isLandscape && fs === wrap) {
        document.exitFullscreen?.().catch(() => undefined);
      }
    };
    const mm = window.matchMedia('(orientation: landscape)');
    mm.addEventListener('change', apply);
    window.addEventListener('orientationchange', apply);
    // Don't auto-enter on initial mount — only on actual user
    // rotation. apply() runs only via event.
    return () => {
      mm.removeEventListener('change', apply);
      window.removeEventListener('orientationchange', apply);
    };
  }, []);

  // ---- Segment detection: which segment is the playhead inside? ----
  // displayedCurrent is in seconds; segments are in ms. Recompute on
  // every tick — the array is short (typically ≤10 segments) so this
  // costs nothing.
  // Under HLS, v.currentTime IS the wall-clock movie position — hls.js
  // maps segments to the global timeline via per-segment tfdt. The old
  // streamOffsetSec offset (used when the URL itself carried `?t=`)
  // stays at 0 in the HLS path.
  const displayedCurrent = current;
  const activeSegment = useMemo(() => {
    if (!segments.length) return null;
    const ms = displayedCurrent * 1000;
    return segments.find((s) => ms >= s.start_ms && ms < s.end_ms) ?? null;
  }, [segments, displayedCurrent]);
  const inIntro = activeSegment?.kind === 'intro';
  const inCredits = activeSegment?.kind === 'credits';
  const inRecap = activeSegment?.kind === 'recap';

  // Fetch next-episode lazily on first credits entry. Don't refetch on
  // subsequent re-entries (the user could seek backwards over credits).
  useEffect(() => {
    if (!inCredits || nextEpFetched || !parentSeriesId || !token) return;
    setNextEpFetched(true);
    fetch(`/api/v1/series/${parentSeriesId}/next-episode?after=${encodeURIComponent(itemId)}`, {
      headers: { Authorization: `Bearer ${token}` },
    })
      .then((r) => (r.ok ? r.json() : null))
      .then((j) => {
        if (j?.next?.id) setNextEp(j.next);
      })
      .catch(() => undefined);
  }, [inCredits, nextEpFetched, parentSeriesId, itemId, token]);

  // Sibling episode fetch — once we know we're inside a series, hit
  // /v1/series/{id}/episodes and flatten to a single ordered list so
  // prev/next navigation is a constant-time index lookup. Episodes are
  // already ordered by (season, episode) on the server side; we just
  // remove the season-bucket wrapping.
  useEffect(() => {
    if (!parentSeriesId || !token) return;
    const ctrl = new AbortController();
    fetch(`/api/v1/series/${encodeURIComponent(parentSeriesId)}/episodes`, {
      signal: ctrl.signal,
      headers: { Authorization: `Bearer ${token}` },
    })
      .then((r) => (r.ok ? r.json() : null))
      .then((j) => {
        if (!j?.seasons) return;
        const flat: { id: string; season: number; episode: number; title: string }[] = [];
        for (const s of j.seasons as { season: number; episodes?: { id: string; episode_number?: number; title?: string }[] }[]) {
          for (const e of s.episodes ?? []) {
            flat.push({ id: e.id, season: s.season, episode: e.episode_number ?? 0, title: e.title ?? '' });
          }
        }
        setSiblingEpisodes(flat);
      })
      .catch(() => undefined);
    return () => ctrl.abort();
  }, [parentSeriesId, token]);

  // Prev / next derived purely from the sibling list. Memoised so the
  // chrome doesn't re-render every interval tick.
  const episodeNav = useMemo(() => {
    if (siblingEpisodes.length === 0) return { prev: null, next: null };
    const idx = siblingEpisodes.findIndex((e) => e.id === itemId);
    if (idx < 0) return { prev: null, next: null };
    return {
      prev: idx > 0 ? siblingEpisodes[idx - 1] : null,
      next: idx < siblingEpisodes.length - 1 ? siblingEpisodes[idx + 1] : null,
    };
  }, [siblingEpisodes, itemId]);

  // Binge pre-warm: as soon as we know which episode is next AND we're
  // inside the credits, ping the next episode's /play/master.m3u8 from
  // THIS page. The chino-stream Master() handler runs warmTranscode in
  // a goroutine on every hit, so the segment + init are produced in
  // parallel with the user reading the "Next episode" card. By the
  // time they (or the auto-countdown) actually navigate, the new
  // PlayerPage's first fetch lands on cached files instead of waiting
  // for a fresh ffmpeg run — eliminating the "00:00 loading…" gap the
  // user reported between episodes.
  const prewarmedNextRef = useRef<string | null>(null);
  useEffect(() => {
    if (!inCredits || !nextEp || !token || !streamToken) return;
    if (prewarmedNextRef.current === nextEp.id) return;
    prewarmedNextRef.current = nextEp.id;
    const enc = encodeURIComponent(streamToken);
    void fetch(`/api/v1/items/${encodeURIComponent(nextEp.id)}/play/master.m3u8?stream=${enc}&q=high`, {
      headers: { Authorization: `Bearer ${token}` },
    }).catch(() => undefined);
    reportEvent('binge_prewarm', { next_item: nextEp.id });
  }, [inCredits, nextEp, token, streamToken]);

  // Mark the item watched once playback hits the credits segment OR
  // crosses 95 % of duration (whichever fires first). Guarded by a ref
  // so the POST runs at most once per mount — re-entries via seek-
  // backwards-then-forward don't re-fire. Drives the "Watched" pill on
  // MediaCard and the next-episode substitution in Continue Watching.
  const markedWatchedRef = useRef(false);
  useEffect(() => {
    if (markedWatchedRef.current || !token || !itemId) return;
    const dur = effectiveDuration;
    const near95 = dur > 0 && displayedCurrent / dur >= 0.95;
    if (!inCredits && !near95) return;
    markedWatchedRef.current = true;
    fetch(`/api/v1/me/items/${encodeURIComponent(itemId)}/watched`, {
      method: 'POST',
      headers: { Authorization: `Bearer ${token}` },
    }).catch(() => undefined);
    reportEvent('mark_watched', { at: displayedCurrent, via: inCredits ? 'credits' : 'p95' });
  }, [inCredits, displayedCurrent, effectiveDuration, token, itemId]);

  // Auto-play-next countdown: starts when we enter credits AND have a
  // next episode AND haven't been dismissed. Each second decrement; on
  // 0, navigate to the next episode. Resets if user seeks out of credits.
  // Gated by the binge setting — if the user turned auto-play-next off
  // (or the whole binge mode), the Next Episode card still shows but
  // without an auto-firing countdown.
  useEffect(() => {
    const wantAuto =
      settings.binge.enabled && settings.binge.autoPlayNext;
    if (!inCredits || !nextEp || autoNextDismissed || !wantAuto) {
      setAutoNextSec(null);
      return;
    }
    if (autoNextSec == null) setAutoNextSec(settings.binge.countdownSec);
    const t = window.setInterval(() => {
      setAutoNextSec((n) => {
        if (n == null) return n;
        if (n <= 1) {
          window.clearInterval(t);
          // replace, not assign — keep ONE /player entry in history so
          // the Back button doesn't walk through the binge chain.
          window.location.replace(`/player/${encodeURIComponent(nextEp.id)}?binge=1`);
          return 0;
        }
        return n - 1;
      });
    }, 1000);
    return () => window.clearInterval(t);
  }, [inCredits, nextEp, autoNextDismissed, autoNextSec, settings.binge.enabled, settings.binge.autoPlayNext, settings.binge.countdownSec]);

  // Pre-skip-at-prepare. Mirrors chino-androidtv's `preSkippedIntro`
  // path: when the player launches with `?binge=1` (the auto-advance
  // chain set this), jump past the intro before the user ever sees
  // it. The countdown auto-skip below is the in-playback fallback for
  // cases this misses (e.g. user pauses inside the intro, segments
  // arrive after metadata). We treat the URL flag as a one-shot —
  // cleared after the first apply so segment refreshes don't re-skip.
  useEffect(() => {
    if (!bingePreSkipPendingRef.current) return;
    if (segments.length === 0) return;
    // Intro detection: a segment of kind=intro that starts in the
    // first 2 s. Filters out mid-episode intro reprises (rare but
    // they exist in some anime), which we don't want to pre-skip.
    const intro = segments.find((s) => s.kind === 'intro' && s.start_ms < 2000);
    if (!intro) {
      bingePreSkipPendingRef.current = false;
      return;
    }
    const target = intro.end_ms / 1000 + 0.25;
    const v = videoRef.current;
    if (v && isFinite(v.duration) && v.duration > 0) {
      // Video is already loaded — seek directly. Common when segments
      // arrive after loadedmetadata fires.
      try {
        v.currentTime = target;
        reportEvent('binge_pre_skip', { target, mode: 'direct' });
      } catch {
        /* SecurityError on cross-origin sources is harmless here. */
      }
    } else {
      // Not loaded yet — stash for the loadedmetadata handler.
      pendingSeekRef.current = target;
      reportEvent('binge_pre_skip', { target, mode: 'pending' });
    }
    bingePreSkipPendingRef.current = false;
  }, [segments]);

  // Binge auto-skip intro: when the playhead enters an intro segment
  // AND this is a continuation of an active series session AND the
  // user has the setting on, run a settings.binge.countdownSec count
  // and then jump to the end of the intro. Cancellable with the
  // "Watch intro" button on the overlay.
  useEffect(() => {
    const wantAuto =
      settings.binge.enabled &&
      settings.binge.autoSkipIntro &&
      isBingeContin;
    if (!inIntro || !wantAuto || autoSkipIntroDismissed) {
      setAutoSkipIntroSec(null);
      return;
    }
    if (autoSkipIntroSec == null) setAutoSkipIntroSec(settings.binge.countdownSec);
    const t = window.setInterval(() => {
      setAutoSkipIntroSec((n) => {
        if (n == null) return n;
        if (n <= 1) {
          window.clearInterval(t);
          skipSegment('intro');
          return 0;
        }
        return n - 1;
      });
    }, 1000);
    return () => window.clearInterval(t);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [inIntro, isBingeContin, autoSkipIntroDismissed, autoSkipIntroSec, settings.binge.enabled, settings.binge.autoSkipIntro, settings.binge.countdownSec]);

  // Record the binge session the first time the player actually
  // starts producing frames (not just on mount). Half-a-second
  // glances at the player shouldn't count as "watched the episode"
  // when computing the next session's binge continuation.
  const bingeRecordedRef = useRef(false);
  useEffect(() => {
    if (!playing || bingeRecordedRef.current) return;
    if (parentSeriesId) recordEpisodePlay(parentSeriesId, itemId);
    bingeRecordedRef.current = true;
  }, [playing, parentSeriesId, itemId]);

  // Skip-* handlers.
  const skipSegment = (kind: 'intro' | 'credits' | 'recap') => {
    if (!activeSegment || activeSegment.kind !== kind) return;
    const v = videoRef.current;
    if (!v) return;
    const targetSec = activeSegment.end_ms / 1000 + 0.25;
    reportEvent('skip_segment', { kind, from: displayedCurrent, to: targetSec });
    // Under HLS, v.currentTime is the canonical seek — hls.js will
    // fetch the right segments. The old transcode-mode branch
    // (setStreamOffsetSec + v.currentTime=0 so the URL rebuilt with
    // -ss) is dead because the URL no longer encodes the offset.
    v.currentTime = targetSec;
  };

  // ---- Telemetry: queue + flush ----
  // reportEvent is dependency-free (uses refs), so it can be referenced
  // from media-event handlers without re-binding the listeners every
  // render. Each event has client-side timestamp + the item context.
  const reportEvent = (kind: string, payload?: Record<string, unknown>) => {
    telemetryQueueRef.current.push({ ts: Date.now(), kind, itemId, payload });
  };

  // Unified stream-fault recovery. Called by BOTH onError (decode
  // failures) and onEnded (premature EOF when ffmpeg truncated the
  // output). Without this, the two handlers could race: onPause sets
  // the "Reconnecting…" label, onError tries to set "Skipping bad
  // frame…", onEnded tries to setStreamOffsetSec to (wall-1) — the
  // last setState in the batch wins and the others appear to have
  // never fired. Funneling both paths through one helper makes the
  // sequencing explicit and dedups rapid re-firings (we observed
  // onError firing repeatedly while a recovery was already pending,
  // burning the recovery on a stale wall position).
  //
  // Reads `apiDurationSec` / `streamOffsetSec` through refs because
  // it's called from useEffect-installed handlers whose closures
  // capture mount-time state.
  const lastFaultRecoveryAtRef = useRef(0);
  const recoverFault = (
    reason: 'decode_error' | 'premature_eof',
    detail?: Record<string, unknown>,
  ) => {
    const v = videoRef.current;
    if (!v) return;
    const now = Date.now();
    if (now - lastFaultRecoveryAtRef.current < 2000) {
      reportEvent('fault_dedup', { reason, ...detail });
      // eslint-disable-next-line no-console
      console.log(`[player] fault_dedup: ${reason}`);
      return;
    }
    const apiDur = apiDurationSecRef.current;
    const off = streamOffsetSecRef.current;
    // Pick the best duration ceiling we have. apiDur (catalog/ffprobe)
    // is preferred but a real decode error can fire mid-playback before
    // it's populated (today: 2026-05-30 09:18 UTC, item def80e8e — the
    // catalog probe hadn't returned a duration when the decode error
    // hit, so fault recovery was aborting with fault_no_apidur and the
    // user got stuck stalling). Falling back to the browser's reported
    // video.duration covers that case; only as a last resort do we
    // recover with no ceiling at all (small jump distances make this
    // safe — at worst we land past EOS and the natural end-of-stream
    // teardown kicks in).
    let ceiling = apiDur;
    if (ceiling <= 0 && Number.isFinite(v.duration) && v.duration > 0) {
      ceiling = v.duration + off;
      reportEvent('fault_apidur_fallback', { reason, source: 'video.duration', ceiling });
    } else if (ceiling <= 0) {
      reportEvent('fault_apidur_fallback', { reason, source: 'none', ceiling: 0 });
    }
    const wall = (v.currentTime || 0) + off;
    // decode_error → skip 1 s past the bad frame.
    // premature_eof → restart at wall - 1 s (small overlap so the
    // new ffmpeg seek catches the next keyframe).
    const targetRaw = reason === 'decode_error'
      ? Math.floor(wall) + 1
      : Math.max(0, Math.floor(wall) - 1);
    const target = ceiling > 0 ? Math.min(targetRaw, Math.floor(ceiling) - 2) : targetRaw;
    if (target <= off) {
      // Would jump backwards or to the same position — that means we
      // already recovered to here. Don't loop.
      reportEvent('fault_skip', { reason, wall, target, off });
      // eslint-disable-next-line no-console
      console.log(`[player] fault_skip: ${reason} target=${target} <= off=${off}`);
      return;
    }
    lastFaultRecoveryAtRef.current = now;
    setBuffering(true);
    setActionLabel(reason === 'decode_error' ? 'Skipping bad frame…' : 'Reconnecting…');
    reportEvent('fault_recover', { reason, wall, target, off, ...detail });
    // eslint-disable-next-line no-console
    console.log(`[player] fault_recover: ${reason} wall=${wall.toFixed(2)} off=${off} → t=${target}`);
    setStreamOffsetSec(target);
  };

  const flushTelemetry = (final = false) => {
    const q = telemetryQueueRef.current;
    if (!q.length || !token) return;
    const events = q.splice(0, q.length);
    const body = JSON.stringify({ sessionId: sessionIdRef.current, events });
    // Use `sendBeacon` on pagehide so the request actually leaves the
    // browser; regular fetch is unreliable during unload. sendBeacon
    // cannot set custom headers, so we carry the bearer in the query
    // string — the chino-api auth middleware already accepts ?token=
    // for <video src> / <img src> requests.
    if (final && navigator.sendBeacon) {
      const url = `/api/v1/play/events?token=${encodeURIComponent(token)}`;
      const ok = navigator.sendBeacon(
        url,
        new Blob([body], { type: 'application/json' }),
      );
      if (!ok) {
        // sendBeacon can refuse if it would push past the user-agent
        // quota; put the events back so the next mount can retry.
        telemetryQueueRef.current.unshift(...events);
      }
      return;
    }
    fetch(`/api/v1/play/events`, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        Authorization: `Bearer ${token}`,
      },
      body,
      keepalive: true,
    }).catch(() => {
      telemetryQueueRef.current.unshift(...events);
    });
  };

  useEffect(() => {
    reportEvent('mount', { ua: navigator.userAgent });
    const id = window.setInterval(() => flushTelemetry(false), 30_000);
    const onHide = () => flushTelemetry(true);
    window.addEventListener('pagehide', onHide);
    return () => {
      window.clearInterval(id);
      window.removeEventListener('pagehide', onHide);
      flushTelemetry(true);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [token]);

  // Mobile-tab-resume handler. The browser freezes a backgrounded tab
  // on Android/iOS; the segments hls.js was streaming through expire
  // out of chino-stream's on-demand cache, the MSE buffer is stale,
  // and resuming "from where we were" usually just spinners forever.
  // On visibility-becomes-visible after >15 s hidden:
  //   1. Stash the wall-clock playhead in pendingSeekRef so the new
  //      pipeline's loadedmetadata seeks back to where the user left
  //      off (instead of restarting at 0).
  //   2. Bump reloadKey → playUrl changes → the hls.js useEffect tears
  //      down + rebuilds with a fresh master.m3u8 + init + segment 0.
  // The 15 s threshold filters out short Tab Switcher previews — the
  // user shouldn't see the player reload just for flipping to chrome's
  // tab list and back.
  useEffect(() => {
    let hiddenAt = 0;
    const onVisibility = () => {
      if (document.visibilityState === 'hidden') {
        hiddenAt = Date.now();
        return;
      }
      if (hiddenAt === 0) return;
      const hiddenMs = Date.now() - hiddenAt;
      hiddenAt = 0;
      if (hiddenMs < 15_000) return;
      const v = videoRef.current;
      if (!v) return;
      const wall = (v.currentTime || 0) + streamOffsetSecRef.current;
      if (wall > 1) pendingSeekRef.current = wall;
      // eslint-disable-next-line no-console
      console.log(`[player] tab-resume reload after ${(hiddenMs/1000).toFixed(1)}s @ wall=${wall.toFixed(1)}`);
      reportEvent('tab_resume_reload', { hiddenMs, wall });
      setReloadKey((k) => k + 1);
    };
    document.addEventListener('visibilitychange', onVisibility);
    return () => document.removeEventListener('visibilitychange', onVisibility);
  }, []);

  // ---- Resume: fetch last saved position, auto-seek ----
  //
  // Always resume if there's a usable saved position. Callers that
  // want a clean start pass ?startover=1 (DetailPage "Start over")
  // or ?binge=1 (auto-advance to the next episode — the user just
  // watched the previous one to credits, so any old partial progress
  // on this id is stale and would jump them mid-stream). Positions
  // ≤ 30 s in or within the last 60 s of the runtime are treated as
  // "effectively at start / done" and ignored.
  //
  // After we consume those flags ONCE, strip them from the URL via
  // history.replaceState. Mobile browsers suspend a backgrounded tab
  // and re-execute the page when the user comes back; without this
  // cleanup, a ?binge=1 from a prior auto-advance would survive the
  // tab resume and slam the saved position back to 0. The replace
  // keeps the same /player/<id> path so React-router state doesn't
  // care.
  useEffect(() => {
    if (!token || resumeChecked) return;
    const ctrl = new AbortController();
    const qp = new URLSearchParams(window.location.search);
    const startover = qp.get('startover') === '1' || qp.get('binge') === '1';
    const hadOneShotFlag = qp.has('startover') || qp.has('binge') || qp.has('resume') || qp.has('autoresume');
    // Explicit ?resume=<sec> handoff (used by the Zap pager's
    // expand-to-fullplayer button). Honour the caller's exact playhead
    // and skip the server-progress fetch entirely — the user just
    // watched up to this second inside the zap card and expects to
    // keep going from here, not from wherever they last paused this
    // title days ago.
    const resumeParamRaw = qp.get('resume') ?? qp.get('autoresume');
    const resumeParam = resumeParamRaw != null ? Number(resumeParamRaw) : NaN;
    if (Number.isFinite(resumeParam) && resumeParam > 1) {
      pendingSeekRef.current = resumeParam;
      setResumeChecked(true);
      if (hadOneShotFlag) {
        try {
          window.history.replaceState(null, '', window.location.pathname);
        } catch {
          // SecurityError on cross-origin / sandboxed frames is harmless here.
        }
      }
      return;
    }
    fetch(`/api/v1/items/${itemId}/progress`, {
      signal: ctrl.signal,
      headers: { Authorization: `Bearer ${token}` },
    })
      .then((r) => (r.ok ? r.json() : null))
      .then((j) => {
        const pos = typeof j?.position_sec === 'number' ? j.position_sec : 0;
        const total = apiDurationSec;
        if (startover || pos <= 30 || (total > 0 && pos >= total - 60)) {
          return;
        }
        // The loadedmetadata listener consumes pendingSeekRef once the
        // <video> is ready, so stash the offset and let it pick up.
        pendingSeekRef.current = pos;
      })
      .catch(() => undefined)
      .finally(() => {
        setResumeChecked(true);
        if (hadOneShotFlag) {
          try {
            window.history.replaceState(null, '', window.location.pathname);
          } catch {
            // SecurityError on cross-origin / sandboxed frames is harmless here.
          }
        }
      });
    return () => ctrl.abort();
  }, [token, itemId, apiDurationSec, resumeChecked]);

  // Throttled save every 10s while we're actually playing. Uses
  // displayedCurrent (account for stream offset after a quality switch
  // or seek) so the resume value matches the wall-clock position.
  useEffect(() => {
    if (!token) return;
    const id = window.setInterval(() => {
      const v = videoRef.current;
      if (!v || v.paused || v.ended) return;
      const pos = Math.floor((v.currentTime || 0) + streamOffsetSec);
      const total = Math.floor(effectiveDuration);
      if (pos <= 0) return;
      fetch(`/api/v1/items/${itemId}/progress`, {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          Authorization: `Bearer ${token}`,
        },
        body: JSON.stringify({ position_sec: pos, duration_sec: total }),
        keepalive: true,
      }).catch(() => undefined);
    }, 10_000);
    return () => window.clearInterval(id);
  }, [token, itemId, streamOffsetSec, effectiveDuration]);

  // ---- Hard-stall watcher: video says "playing" but currentTime stays put ----
  useEffect(() => {
    const id = window.setInterval(() => {
      const v = videoRef.current;
      if (!v || v.paused || v.ended || !v.duration) return;
      const now = performance.now();
      const s = stallWatchRef.current;
      if (Math.abs(v.currentTime - s.lastT) > 0.05) {
        s.lastT = v.currentTime;
        s.lastChange = now;
        if (reconnecting) setReconnecting(false);
        return;
      }
      if (s.lastChange === 0) {
        s.lastChange = now;
        return;
      }
      // 8 seconds without forward progress while not paused — assume
      // the pipeline got wedged. Toggle the reconnecting overlay, log
      // the event, and force-reload the source. Re-setting
      // streamOffsetSec to its current value triggers a playUrl
      // rebuild. The server treats any ?t>0 as a transcode request
      // (passthrough can't seek by time), so this also upgrades a
      // bandwidth-starved passthrough/remux to a transcoded pipeline
      // — that's what the user needs on an unstable connection
      // because (a) ffmpeg input-seek resumes at the right wall time
      // and (b) the quality switcher becomes available to step the
      // bitrate down.
      if (now - s.lastChange > 8000 && !reconnecting) {
        setReconnecting(true);
        reportEvent('stall_recover', {
          position: v.currentTime + streamOffsetSec,
          quality: streamQuality,
          fromMode: info?.mode ?? 'unknown',
        });
        const wall = (v.currentTime || 0) + streamOffsetSec;
        // Treat "info not loaded yet" the same as "mode != transcode":
        // if we don't know we're already transcoding, the safe move is
        // to downgrade. Skipping the downgrade branch when info is
        // undefined left users stuck on the failing high-quality
        // pipeline (today: 2026-05-30 09:17 UTC — stall_recover fired
        // 5× in 3 minutes with fromMode="unknown" and the player never
        // stepped down).
        const knownTranscoding = info?.mode === 'transcode';
        if (!knownTranscoding && !forceTranscode) {
          // Flip the explicit flag too so the auto-downgrade onStall
          // handler treats subsequent waits as transcode-mode stalls.
          setForceTranscode(true);
          // Start the recovery at a lower rung — the user has already
          // hit a hard stall on the original bitrate.
          setStreamQuality('medium');
          setQualityNotice('Unstable connection — switching to adaptive streaming');
          window.setTimeout(() => setQualityNotice(null), 5000);
          recordSwitch(`→ Medium (auto, ${info?.mode ?? 'unknown'} → transcode)`, 'stall', `8s no progress @ ${fmt(wall)}`);
        } else {
          recordSwitch(`reconnecting`, 'stall', `8s no progress @ ${fmt(wall)}`);
        }
        setStreamOffsetSec(Math.max(0, wall - 2));
        s.lastChange = now; // reset; the new src will start producing.
      }
    }, 2000);
    return () => window.clearInterval(id);
    // reconnecting included so the cleared state takes effect; offset/
    // quality so the closure picks up fresh values.
  }, [reconnecting, streamQuality, streamOffsetSec]);

  // Cycle the funny loading message every 2s while we don't have enough
  // buffered data. Stops cycling once we're playing smoothly.
  useEffect(() => {
    if (!buffering && duration > 0) return;
    const id = window.setInterval(() => {
      setLoadingMsgIdx((i) => (i + 1) % LOADING_MESSAGES.length);
    }, 2000);
    return () => window.clearInterval(id);
  }, [buffering, duration]);

  // Cold-prep escalation. The default actionLabel cycles through the
  // funny LOADING_MESSAGES every 2 s, which is fine for sub-second
  // startups. When the player has been buffering > 5 s on the
  // transcode path (HEVC source, no GPU, or NVENC saturated by 4K),
  // swap the message for an honest "this can take a while — preparing
  // your movie" so the user knows it's not stuck. Reverts to the
  // cycling messages once the player reaches canplay.
  useEffect(() => {
    if (!buffering || duration > 0) return;
    if (effectiveMode !== 'transcode') return;
    const t = window.setTimeout(() => {
      setActionLabel('Preparing your movie — first play of an unpackaged title takes a moment…');
    }, 5_000);
    return () => window.clearTimeout(t);
  }, [buffering, duration, effectiveMode]);

  // Auto-downgrade: if the stream stalls (`waiting` fires) three times
  // within ~30 seconds, step down a rung. Works in transcode mode out
  // of the box; for passthrough/remux we flip into forced-transcode
  // mode first (the server upgrades the pipeline, ffmpeg can seek).
  const onStall = () => {
    if (!info) return;
    const now = performance.now();
    const v = videoRef.current;
    // Filter out "buffer refill" waiting events — if currentTime
    // advanced since the previous waiting, the player IS making
    // progress and this isn't a real stall. Only count waitings that
    // fire WHILE currentTime is genuinely stuck (frequent in headless
    // / software-decode environments where the browser re-buffers
    // between frames). Without this guard the player downgraded
    // quality every ~5 min on normal playback jitter.
    const ct = v?.currentTime ?? 0;
    const prevCt = lastWaitingCtRef.current;
    lastWaitingCtRef.current = ct;
    if (prevCt >= 0 && ct > prevCt + 0.5) {
      // Time advanced — this waiting was a transient refill, ignore.
      return;
    }
    // Cool-down after the previous auto-downgrade so we don't cascade
    // high → medium → low in one burst before the new quality's
    // buffer has had a chance to stabilise.
    if (now - lastAutoDowngradeAtRef.current < 120_000) return;
    const s = stallCountRef.current;
    if (now - s.firstAt > 30_000) {
      s.count = 1;
      s.firstAt = now;
    } else {
      s.count += 1;
    }
    if (s.count < 3) return;
    lastAutoDowngradeAtRef.current = now;
    // In passthrough / remux: upgrade to transcode at medium quality
    // and resume from the current position. No further work needed
    // this tick — the next stall will step down again.
    if (effectiveMode !== 'transcode') {
      const wallTime = (videoRef.current?.currentTime ?? 0) + streamOffsetSec;
      setForceTranscode(true);
      setStreamQuality('medium');
      setStreamOffsetSec(Math.max(0, wallTime - 2));
      reportEvent('quality_switch', { from: info.mode, to: 'medium', manual: false, reason: 'bandwidth' });
      recordSwitch(`→ Medium (auto, ${info.mode} → transcode)`, 'auto', `3 stalls in 30s @ ${fmt(wallTime)}`);
      setQualityNotice('Slow connection — switching to adaptive streaming');
      window.setTimeout(() => setQualityNotice(null), 4000);
      s.count = 0;
      return;
    }
    const idx = QUALITY_RUNGS.indexOf(streamQuality);
    if (idx < 0 || idx >= QUALITY_RUNGS.length - 1) return;
    const next = QUALITY_RUNGS[idx + 1];
    // Resume from where we are. video.currentTime is relative to the
    // current segment; add the offset back to get the wall-clock movie
    // position.
    const wallTime = (videoRef.current?.currentTime ?? 0) + streamOffsetSec;
    setStreamOffsetSec(Math.max(0, wallTime - 2)); // 2s rewind to mask the restart
    reportEvent('quality_switch', { from: streamQuality, to: next, manual: false });
    recordSwitch(`→ ${labelForQuality(next)}`, 'auto', `3 stalls in 30s @ ${fmt(wallTime)}`);
    setStreamQuality(next);
    setQualityNotice(`Slow connection — switching to ${labelForQuality(next)}`);
    window.setTimeout(() => setQualityNotice(null), 4000);
    s.count = 0;
    s.firstAt = 0;
  };

  // Verbose diagnostics. Attaches every standard HTMLMediaElement event,
  // a MediaError decoder, a PerformanceObserver for /api/v1/items/<id>/play
  // requests, and a 2-second state poller so silent stalls (no event fired)
  // are still visible. Toggle via ?debug=0 to suppress.
  useEffect(() => {
    const v = videoRef.current;
    if (!v) return;
    if (new URLSearchParams(window.location.search).get('debug') === '0') return;

    const t0 = performance.now();
    const stamp = () => ((performance.now() - t0) / 1000).toFixed(2) + 's';
    const NS = readyStateName;
    const NN = networkStateName;
    const ERR = mediaErrorName;

    const snapshot = (label: string) => {
      const e = v.error;
      // eslint-disable-next-line no-console
      console.log(
        `[player ${stamp()}] ${label}`,
        {
          src: v.currentSrc.split('?')[0] || v.src.split('?')[0],
          ready: NS(v.readyState),
          net: NN(v.networkState),
          paused: v.paused,
          ended: v.ended,
          seeking: v.seeking,
          t: v.currentTime.toFixed(2),
          duration: isFinite(v.duration) ? v.duration.toFixed(2) : v.duration,
          buffered: bufferedRanges(v),
          dims: v.videoWidth + 'x' + v.videoHeight,
          vol: v.volume.toFixed(2),
          muted: v.muted,
          err: e ? { code: ERR(e.code), msg: e.message || '(no message)' } : null,
          tracks: { audio: (v as unknown as { audioTracks?: { length: number } }).audioTracks?.length ?? 'n/a', text: v.textTracks?.length ?? 0 },
        },
      );
    };

    const events: (keyof HTMLMediaElementEventMap)[] = [
      'loadstart', 'durationchange', 'loadedmetadata', 'loadeddata',
      'canplay', 'canplaythrough', 'play', 'playing', 'pause', 'ended',
      'waiting', 'stalled', 'suspend', 'abort', 'emptied', 'error',
      'ratechange', 'volumechange', 'seeking', 'seeked', 'progress',
      'resize',
    ];
    const handlers: Array<[keyof HTMLMediaElementEventMap, EventListener]> = [];
    for (const ev of events) {
      // `progress` fires constantly while bytes arrive — log only every 5th hit.
      let count = 0;
      const handler: EventListener = () => {
        if (ev === 'progress' && ++count % 5 !== 0) return;
        snapshot(ev);
      };
      v.addEventListener(ev, handler);
      handlers.push([ev, handler]);
    }

    // Decode + surface MediaError separately — the `error` event handler above
    // already snapshots, but a dedicated decode line is easier to grep for.
    //
    // PIPELINE_ERROR_DECODE (code 3) on AAC packets is a chronic
    // Chromium issue with normal-size frames our ffmpeg flags can't
    // appease. Retrying from the same wall position would just hit
    // the same packet and loop. Skip 1 s forward so the next
    // transcode starts past the broken frame — user sees a tiny
    // audio glitch instead of a stuck video.
    const onError = () => {
      const e = v.error;
      if (!e) return;
      // eslint-disable-next-line no-console
      console.error(
        `[player ${stamp()}] MediaError ${ERR(e.code)} (code=${e.code}):`,
        e.message || '(no message provided by browser)',
      );
      reportEvent('media_error', {
        code: e.code, name: ERR(e.code), message: e.message,
        t: v.currentTime, q: streamQuality,
      });
      if (e.code === 3) {
        recoverFault('decode_error', { msg: e.message });
      }
    };
    v.addEventListener('error', onError);

    // Catch sub-resource failures the browser issued for <video src> (range
    // requests, retries, subtitle fetches).
    let perfObserver: PerformanceObserver | null = null;
    if ('PerformanceObserver' in window) {
      try {
        perfObserver = new PerformanceObserver((list) => {
          for (const e of list.getEntries()) {
            if (!e.name.includes('/api/v1/items/') && !e.name.includes('/api/v1/subtitles/')) continue;
            const r = e as PerformanceResourceTiming;
            // eslint-disable-next-line no-console
            console.log(
              `[player ${stamp()}] net`,
              {
                url: r.name.split('?')[0],
                size: r.transferSize,
                dur: r.duration.toFixed(0) + 'ms',
                type: r.initiatorType,
              },
            );
          }
        });
        perfObserver.observe({ type: 'resource', buffered: true });
      } catch {
        // PerformanceObserver may not support 'resource' on every browser.
      }
    }

    // 2s state poll so we catch "stays black, no events fire" cases. Stops
    // polling once we're actively playing AND have a decoded frame.
    let lastSnapshot = '';
    const poll = window.setInterval(() => {
      const key = `${v.readyState}|${v.networkState}|${v.paused}|${v.videoWidth}|${v.currentTime.toFixed(0)}|${v.error?.code ?? '_'}`;
      if (key === lastSnapshot) return;
      lastSnapshot = key;
      snapshot('poll');
    }, 2000);

    snapshot('mount');
    return () => {
      for (const [ev, h] of handlers) v.removeEventListener(ev, h);
      v.removeEventListener('error', onError);
      perfObserver?.disconnect();
      window.clearInterval(poll);
    };
  }, []);

  // Belt-and-braces autoplay. The bare `autoPlay` attribute often loses the
  // gesture chain across `window.location.assign` — try unmuted first, fall
  // back to muted on rejection, and finally surface a "click to start"
  // overlay if even muted autoplay is denied.
  const tryAutoplay = () => {
    const v = videoRef.current;
    if (!v) return;
    v.muted = false;
    const p = v.play();
    if (!p || typeof p.then !== 'function') return;
    p.catch(() => {
      v.muted = true;
      setAutoplayMuted(true);
      v.play().catch(() => setNeedsClickToPlay(true));
    });
  };

  const startUnmuted = () => {
    const v = videoRef.current;
    if (!v) return;
    v.muted = false;
    setAutoplayMuted(false);
    userWantsPlayingRef.current = true;
    v.play().catch(() => undefined);
  };

  // Keyboard: space=play/pause, F=fullscreen, M=mute, arrows=seek/volume.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const v = videoRef.current;
      if (!v) return;
      switch (e.key) {
        case ' ':
        case 'k':
          e.preventDefault();
          userTogglePlay();
          break;
        case 'f':
          toggleFullscreen();
          break;
        case 'm':
          v.muted = !v.muted;
          break;
        case 'ArrowRight':
          v.currentTime = Math.min(v.duration || 0, v.currentTime + 10);
          break;
        case 'ArrowLeft':
          v.currentTime = Math.max(0, v.currentTime - 10);
          break;
        case 'ArrowUp':
          v.volume = Math.min(1, v.volume + 0.1);
          break;
        case 'ArrowDown':
          v.volume = Math.max(0, v.volume - 0.1);
          break;
      }
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, []);

  const toggleFullscreen = () => {
    if (!wrapRef.current) return;
    if (document.fullscreenElement) document.exitFullscreen();
    else wrapRef.current.requestFullscreen().catch(() => undefined);
  };

  const fmt = (s: number) => {
    if (!isFinite(s) || s < 0) return '0:00';
    const h = Math.floor(s / 3600);
    const m = Math.floor((s % 3600) / 60);
    const sec = Math.floor(s % 60).toString().padStart(2, '0');
    if (h > 0) return `${h}:${m.toString().padStart(2, '0')}:${sec}`;
    return `${m}:${sec}`;
  };

  const seekTo = (targetSec: number) => {
    const v = videoRef.current;
    if (!v || !effectiveDuration) return;
    const clamped = Math.max(0, Math.min(effectiveDuration, targetSec));
    if (effectiveMode === 'transcode') {
      // We can't seek arbitrarily inside a fragmented MP4 that's still
      // being produced. Restart the stream from the new offset — ffmpeg
      // input-seeks the source file, so this is fast. Show the loading
      // overlay IMMEDIATELY so the user sees feedback the moment they
      // release the slider; onWaiting wouldn't fire until the new src
      // is set + the first chunk arrives.
      setBuffering(true);
      setActionLabel(`Seeking to ${fmt(clamped)}…`);
      setStreamOffsetSec(clamped);
      v.currentTime = 0;
    } else {
      v.currentTime = clamped;
    }
  };
  const onSeek = (e: React.ChangeEvent<HTMLInputElement>) => {
    if (!effectiveDuration) return;
    seekTo((parseFloat(e.target.value) / 100) * effectiveDuration);
  };

  // Manual audio-track switch from the audio menu. Same restart-on-change
  // story as quality: ffmpeg is producing a fragmented MP4 from a single
  // audio map, so a different audio map needs a fresh process. Reuse the
  // streamOffset / startSec mechanic so the user resumes at the same
  // wall-clock time after the swap.
  const switchAudio = (idx: number) => {
    if (idx === streamAudioIdx) {
      setOpenMenu(null);
      return;
    }
    setActionLabel('Switching audio track…');
    reportEvent('audio_switch', { from: streamAudioIdx, to: idx });
    // hls.js: each audio rendition shows up with a `name` matching
    // our master playlist's NAME attribute. We mapped each source
    // audio track to its own rendition; look up by the same source
    // index we used in the URL pattern.
    const hls = hlsRef.current;
    if (hls && hls.audioTracks?.length) {
      const target = hls.audioTracks.findIndex(t => {
        // hls.js's AudioTrack `id` defaults to its position in the
        // audioTracks list; we tagged each rendition with the source
        // index via the NAME attribute (e.g. "English") but hls.js
        // doesn't expose it as `id`. Match by `name` falling back to
        // position parity with info.audio_tracks.
        const ours = info?.audio_tracks?.[idx];
        if (!ours) return false;
        const ourName = ours.title?.trim() || ours.language || `Track ${ours.index}`;
        return t.name === ourName || t.lang === ours.language;
      });
      if (target >= 0) {
        hls.audioTrack = target;
      }
    }
    setStreamAudioIdx(idx);
    setOpenMenu(null);
  };

  // Switch back to the server's preferred Direct pipeline (passthrough
  // or remux). The browser was playing a fragmented MP4 at some wall-
  // clock offset; changing the src to the direct URL resets
  // currentTime to 0, so stash the offset in pendingSeekRef and
  // replay it in onLoadedMetadata. For passthrough, the seek issues a
  // single Range request to the right byte offset.
  const switchToDirect = () => {
    const v = videoRef.current;
    if (!v) return;
    const wallTime = (v.currentTime ?? 0) + streamOffsetSec;
    setBuffering(true);
    setActionLabel('Switching to Direct…');
    reportEvent('quality_switch', { from: streamQuality, to: 'direct', manual: true });
    recordSwitch(`→ Direct (${info?.video_codec?.toUpperCase() ?? '?'})`, 'manual', `at ${fmt(wallTime)}`);
    pendingSeekRef.current = wallTime;
    setForceTranscode(false);
    setStreamOffsetSec(0);
    setOpenMenu(null);
  };

  // Manual quality switch from the settings menu. The server's master
  // playlist now exposes a SINGLE variant matching ?q=, so changing
  // streamQuality flips playUrl which triggers a hls.js teardown +
  // reload. Stash currentTime so onLoadedMetadata can replay it once
  // the new ladder rung's segments arrive.
  const switchQuality = (q: Quality) => {
    if (q === streamQuality) {
      setOpenMenu(null);
      return;
    }
    const v = videoRef.current;
    const wallTime = v?.currentTime ?? 0;
    setBuffering(true);
    setActionLabel(`Switching to ${labelForQuality(q)}…`);
    reportEvent('quality_switch', { from: streamQuality, to: q, manual: true });
    recordSwitch(`→ ${labelForQuality(q)}`, 'manual', `at ${fmt(wallTime)}`);
    pendingSeekRef.current = wallTime;
    setStreamQuality(q);
    setOpenMenu(null);
  };

  const onVolume = (e: React.ChangeEvent<HTMLInputElement>) => {
    const v = videoRef.current;
    if (!v) return;
    const nv = parseFloat(e.target.value);
    v.volume = nv;
    if (nv > 0 && v.muted) v.muted = false;
  };

  if (!playUrl) {
    return (
      <div className="min-h-screen bg-black text-white flex items-center justify-center">
        <Loader2 className="w-8 h-8 animate-spin" />
      </div>
    );
  }

  return (
    <div
      ref={wrapRef}
      // Fixed positioning + insets-zero pins the wrap to the visual
      // viewport edge regardless of which viewport unit the browser
      // uses (lvh / svh / dvh). Without this, the dynamic-viewport
      // dance during URL-bar collapse left a 1 px sliver of body
      // background (#0d1117) showing at the top edge in mobile
      // Chrome — the user reported it as "1 px wide bar at the top
      // in fullscreen". inset-0 is equivalent to top/right/bottom/
      // left:0 so the wrap snaps to whatever the OS / browser
      // currently considers "all the way to the edge".
      className="fixed inset-0 bg-black overflow-hidden text-white"
    >
      <video
        ref={videoRef}
        // No `src` attribute — hls.js attaches and drives the source.
        // (On Safari we fall back to native HLS and DO set v.src
        // directly from the useEffect, not via React props.)
        className={`absolute inset-0 w-full h-full chino-player ${videoFill ? 'chino-player-fill' : ''}`}
        crossOrigin="anonymous"
        // `autoPlay` attribute intentionally omitted: the browser's
        // built-in autoplay fires on EVERY src change (including
        // token-renew / quality-switch / audio-switch), which defeats
        // the userWantsPlayingRef pause-survives-src-change gate. We
        // drive initial autoplay through tryAutoplay() in
        // onLoadedMetadata, which IS gated.
        playsInline
        onPlay={() => { setPlaying(true); reportEvent('play', { t: videoRef.current?.currentTime }); }}
        onPause={(e) => {
          setPlaying(false);
          reportEvent('pause', { t: e.currentTarget.currentTime });
          // Browser-initiated pauses (network underrun, decode hiccup)
          // do NOT fire `waiting` first, so the buffer overlay never
          // appears and the user sees a frozen video with no signal.
          // If the user wanted playback at this moment, treat the
          // pause as a stall: show the spinner. onCanPlay below
          // resumes once enough data has buffered.
          // Don't overwrite the label if a decode error is pending —
          // recoverFault sets a more specific label ("Skipping bad
          // frame…") and gets cleared by onPlaying/onCanPlay.
          // Without this guard, the pause event (which also fires
          // when error fires) raced ahead of recoverFault and the
          // user only ever saw the generic "Reconnecting…" message.
          if (userWantsPlayingRef.current && !e.currentTarget.ended && !e.currentTarget.error) {
            setBuffering(true);
            setActionLabel('Reconnecting…');
          }
        }}
        onTimeUpdate={(e) => {
          const v = e.currentTarget;
          setCurrent(v.currentTime);
          // Refresh the buffered overlay too — onProgress doesn't fire
          // when the buffer trails the playhead (network steady-state
          // playback) but timeupdate fires ~4x/s, which is more than
          // enough to keep the indicator alive.
          const dur = apiDurationSecRef.current || v.duration || 0;
          if (dur > 0 && v.buffered && v.buffered.length > 0) {
            let endSec = 0;
            for (let i = 0; i < v.buffered.length; i++) {
              if (v.currentTime >= v.buffered.start(i) && v.currentTime <= v.buffered.end(i)) {
                endSec = v.buffered.end(i) + streamOffsetSecRef.current;
                break;
              }
            }
            if (endSec > 0) {
              setBufferedPct(Math.min(100, (endSec / dur) * 100));
            }
          }
        }}
        onProgress={(e) => {
          const v = e.currentTarget;
          const dur = apiDurationSecRef.current || v.duration || 0;
          if (dur <= 0 || !v.buffered || v.buffered.length === 0) return;
          let endSec = 0;
          for (let i = 0; i < v.buffered.length; i++) {
            if (v.currentTime >= v.buffered.start(i) && v.currentTime <= v.buffered.end(i)) {
              endSec = v.buffered.end(i) + streamOffsetSecRef.current;
              break;
            }
          }
          if (endSec > 0) {
            setBufferedPct(Math.min(100, (endSec / dur) * 100));
          }
        }}
        onLoadedMetadata={(e) => {
          setDuration(e.currentTarget.duration);
          // If we asked the browser to land at a specific wall-clock
          // time after a Direct-mode switch, replay it now that the
          // metadata is loaded — for passthrough this issues one
          // Range request to the right byte offset.
          const pending = pendingSeekRef.current;
          if (pending != null && isFinite(pending) && pending > 0) {
            try { e.currentTarget.currentTime = pending; } catch { /* ignore */ }
            pendingSeekRef.current = null;
          }
          // Re-apply the user-chosen playback rate after a src swap —
          // the browser resets video.playbackRate to 1 whenever the
          // src changes (quality switch, token rotation, tab-resume
          // reload), and onLoadedMetadata is the right point to put
          // it back so a user watching at 1.5x stays at 1.5x across
          // those internal swaps.
          try { e.currentTarget.playbackRate = playbackRate; } catch { /* ignore */ }
          // Honour user-initiated pause: don't auto-resume on internal
          // src changes (token rotation, quality switch). Explicitly
          // pause to override any other autoplay path (browser default,
          // canplay handler, etc) that might have kicked in between
          // src swap and this handler.
          if (userWantsPlayingRef.current) {
            tryAutoplay();
          } else {
            e.currentTarget.pause();
          }
        }}
        onVolumeChange={(e) => {
          setVolume(e.currentTarget.volume);
          setMuted(e.currentTarget.muted);
          if (!e.currentTarget.muted) setAutoplayMuted(false);
        }}
        onEnded={(e) => {
          const v = e.currentTarget;
          // Fragmented MP4 + empty_moov makes the browser compute
          // duration from "last fragment end_time seen". When ffmpeg
          // is killed (client disconnect, OOM, etc) the stream
          // truncates mid-source, the watched-time catches up to that
          // truncated duration, and the browser fires `ended`. Defer
          // to recoverFault, which de-dupes with any onError that
          // fired in the same moment.
          const wall = (v.currentTime || 0) + streamOffsetSec;
          if (apiDurationSec > 0 && apiDurationSec - wall > 60) {
            recoverFault('premature_eof', { seen: v.duration, expected: apiDurationSec });
          }
        }}
        onWaiting={() => { setBuffering(true); onStall(); reportEvent('waiting', { t: videoRef.current?.currentTime, q: streamQuality }); }}
        onPlaying={() => { setBuffering(false); setActionLabel(null); }}
        onCanPlay={(e) => {
          setBuffering(false);
          setActionLabel(null);
          // Pair with the onPause stall recovery above: when the
          // browser silently paused on a buffer underrun and now has
          // data again, kick playback off ourselves. Skip if the user
          // had explicitly paused (userWantsPlayingRef === false).
          if (userWantsPlayingRef.current && e.currentTarget.paused && !e.currentTarget.ended) {
            e.currentTarget.play().catch(() => undefined);
          }
        }}
        onClick={() => {
          userTogglePlay();
        }}
      >
        {/* Only render ACTIVE subtitles as <track> elements. Chrome
            prefetches every track on mount regardless of mode, so a
            mkv with 26 embedded subtitle streams would fire 26
            simultaneous ffmpeg subtitle extracts on chino-stream and
            OOM the pod. Render-on-demand keeps the spawn count
            bounded (max 2 — the multi-select cap) and the menu still
            lists every option (mergedSubs feeds the switcher UI
            elsewhere). Modes are driven by activeSubIds via the
            effect above; `default` is intentionally omitted. */}
        {activeSubIds.map((id) => {
          const s = mergedSubs.find((m) => m.id === id);
          if (!s) return null;
          return (
            <track
              key={`${s.id}|${s.url}`}
              id={s.id}
              kind="subtitles"
              src={s.url}
              srcLang={s.lang}
              label={s.label}
            />
          );
        })}
      </video>

      {/* Secondary subtitle overlay. The native <track mode="showing">
          paints the primary track at line=80; this overlay stacks the
          secondary track's cues two rows below, matching standard
          dual-subtitle players (e.g. language-learning apps). */}
      {secondaryCueText && (
        <div
          className="pointer-events-none absolute inset-x-0 z-20 text-center px-4"
          style={{ bottom: 'calc(7rem + env(safe-area-inset-bottom, 0px))' }}
        >
          <span className="inline-block px-2 py-0.5 rounded bg-black/70 text-white text-base md:text-lg whitespace-pre-line drop-shadow">
            {secondaryCueText}
          </span>
        </div>
      )}

      {buffering && (
        <div className="absolute inset-0 flex flex-col items-center justify-center pointer-events-none bg-black/30">
          <Loader2 className="w-12 h-12 animate-spin text-white/80" />
          <p className="mt-4 text-white/90 text-sm">
            {actionLabel ?? LOADING_MESSAGES[loadingMsgIdx]}
          </p>
          {effectiveMode === 'transcode' && info && (
            <p className="mt-2 text-white/50 text-xs">
              Transcoding {info.video_codec.toUpperCase()} → H.264 · {labelForQuality(streamQuality)}
            </p>
          )}
        </div>
      )}

      {qualityNotice && (
        <div className="absolute top-20 left-1/2 -translate-x-1/2 px-4 py-2 rounded-full bg-black/80 text-sm text-white shadow-lg">
          {qualityNotice}
        </div>
      )}

      {needsClickToPlay && (
        <button
          onClick={() => {
            setNeedsClickToPlay(false);
            userWantsPlayingRef.current = true;
            videoRef.current?.play().catch(() => undefined);
          }}
          className="absolute inset-0 flex items-center justify-center bg-black/40"
        >
          <span className="px-6 py-3 rounded-full bg-[#58a6ff] text-white font-medium shadow-xl">
            Click to start
          </span>
        </button>
      )}

      {reconnecting && (
        <div className="absolute top-20 left-1/2 -translate-x-1/2 px-4 py-2 rounded-full bg-rose-500/20 text-rose-200 text-sm border border-rose-500/40 shadow-lg">
          Stream paused — reconnecting…
        </div>
      )}

      {autoplayMuted && !needsClickToPlay && (
        <button
          onClick={startUnmuted}
          className="absolute top-20 left-1/2 -translate-x-1/2 px-4 py-2 rounded-full bg-black/70 hover:bg-black/85 text-sm flex items-center gap-2 transition-colors"
        >
          <VolumeX className="w-4 h-4" />
          <span>Tap to unmute</span>
        </button>
      )}

      {/* Top chrome */}
      <div
        // Pad top by safe-area-inset so the back arrow + title clear
        // the iOS notch / Android status bar when launched as a PWA or
        // when the iOS Safari status bar is overlaid on the page in
        // landscape. Without this the title was crashing into the
        // 'HH:MM 4G' system bar on iPhone (see #player-mobile-top).
        data-chrome-zone
        style={{ paddingTop: 'calc(1rem + env(safe-area-inset-top, 0px))' }}
        className={`absolute top-0 inset-x-0 px-4 pb-4 bg-gradient-to-b from-black/80 to-transparent transition-opacity ${chromeVisible ? 'opacity-100' : 'opacity-0 pointer-events-none'}`}
      >
        <div className="flex items-center gap-3">
          <button
            onClick={() => {
              // Don't trust history.back() — a binge session leaves a
              // trail of /player/<id> entries, so the user would skip
              // back to the previous episode (or further back through
              // the chain) instead of escaping the player. Jump to the
              // natural parent context: series detail for episodes,
              // item detail for movies/shows, home as fallback.
              const dest = parentSeriesId
                ? `/i/${encodeURIComponent(parentSeriesId)}`
                : itemId
                  ? `/i/${encodeURIComponent(itemId)}`
                  : '/';
              // Replace so the player URL drops out of history. Otherwise
              // the browser's Back from the series page lands right back
              // on the episode the user just left.
              window.location.replace(dest);
            }}
            className="p-2 bg-white/10 hover:bg-white/20 rounded-full transition-colors"
            title="Back"
          >
            <ArrowLeft className="w-5 h-5" />
          </button>
          <h1 className="text-lg font-medium truncate">{title || 'Playing'}</h1>
          {info && effectiveMode !== 'passthrough' && (
            <span
              className="ml-2 shrink-0 px-2.5 py-1 rounded-full text-xs bg-white/10 text-white/80 border border-white/10"
              title={
                effectiveMode === 'transcode'
                  ? `Transcoding ${info.video_codec.toUpperCase()} → H.264 at ${labelForQuality(streamQuality)}`
                  : `Remuxing ${info.container} → MP4 (stream-copy, no re-encode)`
              }
            >
              {effectiveMode === 'transcode'
                ? `${info.video_codec.toUpperCase()} → H.264 · ${labelForQuality(streamQuality)}`
                : `Remux ${info.container}`}
            </span>
          )}
        </div>
      </div>

      {/* Skip-Intro / Skip-Recap pill: floats above the bottom chrome,
          shown whenever the playhead is inside an intro/recap segment.
          In binge-continuation mode WITH auto-skip enabled, this is
          replaced by the countdown card below — clicking "Watch intro"
          cancels the auto-skip and lets the segment play through. */}
      {inIntro && autoSkipIntroSec != null && !autoSkipIntroDismissed ? (
        <div
          style={{ bottom: 'calc(7rem + env(safe-area-inset-bottom, 0px))' }}
          className="absolute right-4 md:right-6 z-10 max-w-sm rounded-xl bg-[#161b22]/95 backdrop-blur border border-white/10 shadow-2xl p-4"
        >
          <div className="text-xs uppercase tracking-wider text-[#8b949e]">Binge mode</div>
          <div className="mt-1 text-white font-medium">
            Skipping intro in {autoSkipIntroSec}…
          </div>
          <div className="mt-3 flex items-center gap-2">
            <button
              onClick={() => skipSegment('intro')}
              className="px-3 py-1.5 rounded-full bg-[#58a6ff] hover:bg-[#58a6ff]/80 text-white text-sm font-medium flex items-center gap-1"
            >
              <SkipForward className="w-4 h-4" />
              Skip now
            </button>
            <button
              onClick={() => setAutoSkipIntroDismissed(true)}
              className="px-3 py-1.5 rounded-full bg-white/10 hover:bg-white/20 text-white text-sm"
            >
              Watch intro
            </button>
          </div>
        </div>
      ) : (inIntro || inRecap) ? (
        <button
          onClick={() => skipSegment(inIntro ? 'intro' : 'recap')}
          style={{ bottom: 'calc(7rem + env(safe-area-inset-bottom, 0px))' }}
          className="absolute right-4 md:right-6 z-10 px-4 py-2 rounded-full bg-white text-black font-medium shadow-2xl hover:bg-white/90 transition-colors"
        >
          {inIntro ? 'Skip Intro' : 'Skip Recap'}
        </button>
      ) : null}

      {/* Next-episode card with countdown — auto-plays when we hit 0
          unless the user dismisses it. */}
      {inCredits && nextEp && !autoNextDismissed ? (
        <div
          style={{ bottom: 'calc(7rem + env(safe-area-inset-bottom, 0px))' }}
          className="absolute right-4 md:right-6 z-10 max-w-sm rounded-xl bg-[#161b22]/95 backdrop-blur border border-white/10 shadow-2xl p-4"
        >
          <div className="text-xs uppercase tracking-wider text-[#8b949e]">Up next</div>
          <div className="mt-1 text-white font-medium truncate">{nextEp.title || 'Next episode'}</div>
          {nextEp.season_number != null && nextEp.episode_number != null ? (
            <div className="text-sm text-[#8b949e]">
              S{String(nextEp.season_number).padStart(2, '0')}
              E{String(nextEp.episode_number).padStart(2, '0')}
            </div>
          ) : null}
          <div className="mt-3 flex items-center gap-2">
            <button
              onClick={() => window.location.replace(`/player/${encodeURIComponent(nextEp.id)}?binge=1`)}
              className="px-3 py-1.5 rounded-full bg-[#58a6ff] hover:bg-[#58a6ff]/80 text-white text-sm font-medium flex items-center gap-1"
            >
              <Play className="w-4 h-4 fill-white" />
              Play now {autoNextSec != null ? `(${autoNextSec})` : ''}
            </button>
            <button
              onClick={() => setAutoNextDismissed(true)}
              className="px-3 py-1.5 rounded-full bg-white/10 hover:bg-white/20 text-white text-sm"
            >
              Cancel
            </button>
          </div>
        </div>
      ) : null}
      {inCredits && !nextEp ? (
        <button
          onClick={() => skipSegment('credits')}
          style={{ bottom: 'calc(7rem + env(safe-area-inset-bottom, 0px))' }}
          className="absolute right-4 md:right-6 z-10 px-4 py-2 rounded-full bg-white text-black font-medium shadow-2xl hover:bg-white/90 transition-colors"
        >
          Skip Credits
        </button>
      ) : null}

      {/* Bottom chrome */}
      <div
        // Pad bottom by safe-area-inset so the scrub bar + chrome
        // controls clear the iOS home indicator / Android nav bar
        // (and the URL-bar dead-zone when h-dvh is at its 'small'
        // size mid-scroll).
        data-chrome-zone
        style={{ paddingBottom: 'calc(1rem + env(safe-area-inset-bottom, 0px))' }}
        className={`absolute bottom-0 inset-x-0 px-4 pt-4 bg-gradient-to-t from-black/90 via-black/60 to-transparent transition-opacity ${chromeVisible ? 'opacity-100' : 'opacity-0 pointer-events-none'}`}
      >
        <div className="flex items-center gap-3">
          <span className="text-sm tabular-nums w-16 text-right">{fmt(displayedCurrent)}</span>
          <div
            ref={seekBarRef}
            className="relative flex-1 py-3 -my-3"
            onMouseMove={(e) => {
              if (!effectiveDuration || !seekBarRef.current) return;
              const rect = seekBarRef.current.getBoundingClientRect();
              const x = Math.max(0, Math.min(rect.width, e.clientX - rect.left));
              setHover({ mouseX: x, sec: (x / rect.width) * effectiveDuration });
            }}
            onMouseLeave={() => setHover(null)}
          >
            {/* Muted track background — segments only colour the
                stretches they cover, so an item with no segments would
                otherwise show a transparent gap (the input's track is
                made transparent in index.css under .chino-seek so
                segments behind it are visible). This thin bar gives
                the unplayed portion a visible base colour. */}
            <div
              aria-hidden
              className="pointer-events-none absolute inset-x-0 top-1/2 -translate-y-1/2 h-[6px] rounded-full bg-white/15"
            />
            {/* Buffered overlay — lighter band showing how far the
                network has fetched past the playhead. Width is the %
                of the catalog/effective duration covered by the
                buffered range containing currentTime. Sits between
                the muted track and the played-progress fill so the
                played fill paints over it. */}
            {effectiveDuration > 0 && bufferedPct > 0 && (
              <div
                aria-hidden
                className="pointer-events-none absolute left-0 top-1/2 -translate-y-1/2 h-[6px] rounded-full bg-white/30 transition-[width] duration-200 ease-linear"
                style={{ width: `${bufferedPct}%` }}
              />
            )}
            {/* Played-progress fill — bright accent band from 0 to the
                playhead. Width tracks displayedCurrent so the fill
                follows the thumb. Above the buffered overlay, below
                the segment chips so segment colours still paint over
                the played portion (so a played intro stays purple,
                etc.). */}
            {effectiveDuration > 0 && (
              <div
                aria-hidden
                className="pointer-events-none absolute left-0 top-1/2 -translate-y-1/2 h-[6px] rounded-full bg-[#58a6ff]"
                style={{ width: `${Math.min(100, (displayedCurrent / effectiveDuration) * 100)}%` }}
              />
            )}
            <input
              type="range"
              min={0}
              max={100}
              step={0.01}
              value={effectiveDuration ? (displayedCurrent / effectiveDuration) * 100 : 0}
              onChange={onSeek}
              // relative + z-30 lifts the thumb above the segment
              // overlays (z-20) so the playhead is always visible on
              // top of intro / credits / recap / sponsor spans. The
              // .chino-seek class kills the native track background
              // so segments paint through the input — the thumb stays
              // visible because it's styled separately. pointer-events-
              // none keeps the chapter-tick buttons (z-20) clickable —
              // the full-area click overlay below (z-10) handles
              // mouse-driven seeking; this input is for keyboard
              // accessibility + visual state only.
              className="chino-seek relative z-30 w-full pointer-events-none"
              aria-label="Seek"
            />
            {/* Full-height click target. The native <input type="range">
                is visually thin (~12 px) and you can only click directly
                on the track or thumb — annoying when the trickplay
                preview makes the whole area feel interactive. This
                overlay covers the entire wrapper (including the py-3
                hover padding above) and translates clicks + drags into
                seekTo() calls. The native input still drives keyboard
                accessibility + visual state; this is purely the mouse
                hit area widened. */}
            {effectiveDuration > 0 ? (
              <div
                // Bigger touch target on phones: the visible bar is
                // only ~8px tall but the hit area extends 14px above
                // and below so a finger can land on the bar without
                // pixel-perfect precision. touch-none disables the
                // browser's pan-y gesture so a drag-to-scrub stays a
                // scrub instead of scrolling the (already non-
                // scrollable) page on iOS Safari.
                className="absolute inset-x-0 -inset-y-3 cursor-pointer z-10 touch-none"
                onPointerDown={(e) => {
                  if (!seekBarRef.current) return;
                  if (e.pointerType === 'mouse' && e.button !== 0) return;
                  const bar = seekBarRef.current;
                  const target = e.currentTarget;
                  const updateAt = (clientX: number) => {
                    const rect = bar.getBoundingClientRect();
                    const x = Math.max(0, Math.min(rect.width, clientX - rect.left));
                    const sec = (x / rect.width) * effectiveDuration;
                    // Drive hover state too so the trickplay preview
                    // shows during a touch scrub — mouse uses
                    // onMouseMove on the outer div, but touch never
                    // fires mouse events and the preview was
                    // invisible on phones. Same path for both.
                    setHover({ mouseX: x, sec });
                    seekTo(sec);
                  };
                  updateAt(e.clientX);
                  // setPointerCapture keeps subsequent pointermove /
                  // pointerup events targeted at this element even if
                  // the finger leaves the bar — works on touch where
                  // window-level mouse listeners wouldn't fire.
                  try { target.setPointerCapture(e.pointerId); } catch { /* unsupported */ }
                  const onMove = (ev: PointerEvent) => updateAt(ev.clientX);
                  const onUp = (ev: PointerEvent) => {
                    target.removeEventListener('pointermove', onMove);
                    target.removeEventListener('pointerup', onUp);
                    target.removeEventListener('pointercancel', onUp);
                    try { target.releasePointerCapture(ev.pointerId); } catch { /* noop */ }
                    // Hide the trickplay overlay once the touch ends.
                    // Mouse hovers clear it via onMouseLeave on the
                    // outer div; touch has no equivalent.
                    if (ev.pointerType !== 'mouse') setHover(null);
                  };
                  target.addEventListener('pointermove', onMove);
                  target.addEventListener('pointerup', onUp);
                  target.addEventListener('pointercancel', onUp);
                  e.preventDefault();
                }}
              />
            ) : null}
            {/* Scrub-bar segment overlay. Two layers:
                  - intro / credits / recap / sponsor render as full
                    coloured spans so the range jumps out at a glance;
                  - chapter boundaries render as thin vertical ticks
                    with the chapter title in a tooltip (clickable to
                    seek to that chapter). */}
            {effectiveDuration > 0 ? (
              <>
                {/* Coloured segment chips. z-20 keeps them above the
                    click-hit overlay (z-10) and the muted track bg,
                    while the input's z-30 thumb still paints over the
                    top so the playhead circle is never hidden behind
                    a span. Height matches the track height (6 px) so
                    the chips read as the bar's filled segments
                    rather than a separate stripe below it. */}
                <div className="pointer-events-none absolute inset-x-0 top-1/2 -translate-y-1/2 h-[6px] z-20">
                  {segments.filter((s) => s.kind !== 'chapter').map((s, i) => {
                    const left = Math.max(0, Math.min(100, (s.start_ms / 1000 / effectiveDuration) * 100));
                    const width = Math.max(
                      0,
                      Math.min(100 - left, ((s.end_ms - s.start_ms) / 1000 / effectiveDuration) * 100),
                    );
                    const color =
                      s.kind === 'intro'   ? '#a371f7' :
                      s.kind === 'credits' ? '#fb8500' :
                      s.kind === 'recap'   ? '#7ee787' :
                      s.kind === 'sponsor' ? '#f85149' :
                                              '#58a6ff';
                    return (
                      <span
                        key={`seg-${i}`}
                        title={`${s.kind} ${fmt(s.start_ms / 1000)}–${fmt(s.end_ms / 1000)}`}
                        className="absolute top-0 h-full rounded-full opacity-90"
                        style={{ left: `${left}%`, width: `${width}%`, background: color }}
                      />
                    );
                  })}
                </div>
                {/* Chapter ticks layer — taller than the range overlay,
                    full-pointer-events so the tick is clickable. z-20
                    sits above the seek-bar click overlay below so
                    chapter clicks aren't swallowed by the wider hit
                    target. */}
                <div className="absolute inset-x-0 -top-1 h-4 z-20">
                  {segments.filter((s) => s.kind === 'chapter').map((s, i) => {
                    const left = Math.max(0, Math.min(100, (s.start_ms / 1000 / effectiveDuration) * 100));
                    return (
                      <button
                        key={`ch-${i}`}
                        onClick={() => {
                          const v = videoRef.current;
                          if (!v || !effectiveDuration) return;
                          const targetSec = s.start_ms / 1000;
                          if (effectiveMode === 'transcode') {
                            setBuffering(true);
                            setActionLabel(`Seeking to chapter…`);
                            setStreamOffsetSec(targetSec);
                            v.currentTime = 0;
                          } else {
                            v.currentTime = targetSec;
                          }
                        }}
                        title={(() => { const lbl = segmentDisplayLabel(s); return (lbl ? `${lbl} · ` : '') + fmt(s.start_ms / 1000); })()}
                        className="absolute top-0 h-full w-[2px] bg-white/80 hover:bg-white hover:w-[3px] transition-all"
                        style={{ left: `${left}%` }}
                      />
                    );
                  })}
                </div>
              </>
            ) : null}
            {/* Trickplay hover preview. The cue + sprite live behind
                a server-side ?stream= token; we crop the right tile
                out of the sprite using background-image with negative
                background-position. The thumbnail is centered on the
                cursor and clamped to stay inside the seek bar's
                horizontal extent. Whichever segment of the seek bar
                the cursor is over also surfaces the corresponding
                chapter / intro / credits label underneath the time so
                the user knows what they're scrubbing into. */}
            {hover && trickplayCues.length > 0 && streamToken ? (() => {
              const cue = findTrickplayCue(trickplayCues, hover.sec);
              if (!cue) return null;
              const bar = seekBarRef.current?.getBoundingClientRect();
              if (!bar) return null;
              const left = Math.max(cue.w / 2, Math.min(bar.width - cue.w / 2, hover.mouseX));
              const seg = segments.find((s) =>
                hover.sec * 1000 >= s.start_ms && hover.sec * 1000 < s.end_ms,
              );
              const segLabel = seg ? segmentDisplayLabel(seg) : '';
              const enc = encodeURIComponent(streamToken);
              return (
                <div
                  className="pointer-events-none absolute -translate-x-1/2 bottom-6 z-30 flex flex-col items-center"
                  style={{ left: `${left}px` }}
                >
                  <div
                    className="rounded-md border border-white/20 shadow-2xl bg-black"
                    style={{
                      width: cue.w,
                      height: cue.h,
                      backgroundImage: `url(${trickplayBaseUrl}/${cue.sprite}?stream=${enc})`,
                      backgroundPosition: `-${cue.x}px -${cue.y}px`,
                      backgroundRepeat: 'no-repeat',
                    }}
                  />
                  <div className="mt-1 px-2 py-0.5 rounded bg-black/80 text-xs tabular-nums text-white text-center">
                    {segLabel ? <span className="text-white/70 mr-1">{segLabel} ·</span> : null}
                    {fmt(hover.sec)}
                  </div>
                </div>
              );
            })() : null}
          </div>
          <span className="text-sm tabular-nums w-16">{fmt(effectiveDuration)}</span>
        </div>

        <div className="flex items-center gap-2 mt-2">
          <button
            onClick={() => userTogglePlay()}
            className="p-2 bg-white/10 hover:bg-white/20 rounded-full transition-colors"
            title={playing ? 'Pause' : 'Play'}
          >
            {playing ? <Pause className="w-5 h-5" /> : <Play className="w-5 h-5" />}
          </button>

          <button
            onClick={() => {
              const v = videoRef.current;
              if (v) v.muted = !v.muted;
            }}
            className="p-2 bg-white/10 hover:bg-white/20 rounded-full transition-colors"
            title={muted ? 'Unmute' : 'Mute'}
          >
            {muted || volume === 0 ? <VolumeX className="w-5 h-5" /> : <Volume2 className="w-5 h-5" />}
          </button>
          <input
            type="range"
            min={0}
            max={1}
            step={0.01}
            value={muted ? 0 : volume}
            onChange={onVolume}
            className="hidden md:block w-24 accent-[#58a6ff]"
            aria-label="Volume"
          />

          <div className="flex-1" />

          {/* Audio track switcher — only show when the file actually has
              more than one audio stream (one-track files are typical for
              most movies; multi-track shows up on bilingual releases). */}
          {info?.audio_tracks && info.audio_tracks.length > 1 && (
            <div className="relative">
              <button
                onClick={() => toggleMenu('audio')}
                className="px-3 py-2 rounded-full bg-white/10 hover:bg-white/20 transition-colors text-xs uppercase tracking-wide"
                title="Audio language"
              >
                {langLabel(info.audio_tracks.find((t) => t.index === streamAudioIdx)?.language ?? 'und').slice(0, 3).toUpperCase()}
              </button>
              {audioMenuOpen && (
                <div className="absolute right-0 bottom-full mb-2 min-w-[220px] bg-[#161b22] border border-white/10 rounded-lg shadow-xl py-1 z-50">
                  <div className="px-4 pt-2 pb-1 text-xs uppercase tracking-wide text-[#8b949e]">Audio</div>
                  {info.audio_tracks.map((t) => (
                    <button
                      key={t.index}
                      onClick={() => switchAudio(t.index)}
                      className={`block w-full text-left px-4 py-2 hover:bg-white/10 ${streamAudioIdx === t.index ? 'text-[#58a6ff]' : ''}`}
                    >
                      {(t.title?.trim() || langLabel(t.language))}
                      <span className="ml-2 text-xs text-[#8b949e]">
                        {t.codec?.toUpperCase()}{t.channels ? ` · ${t.channels}ch` : ''}
                      </span>
                    </button>
                  ))}
                </div>
              )}
            </div>
          )}

          <div className="relative">
            <button
              onClick={() => toggleMenu('captions')}
              className={`p-2 rounded-full transition-colors ${activeSubIds.length > 0 ? 'bg-[#58a6ff]/30 hover:bg-[#58a6ff]/40' : 'bg-white/10 hover:bg-white/20'}`}
              title="Subtitles"
              disabled={mergedSubs.length === 0}
            >
              <Captions className="w-5 h-5" />
            </button>
            {showCaptionsMenu && mergedSubs.length > 0 && (
              <div className="absolute right-0 bottom-full mb-3 w-72 bg-[#161b22] border border-white/10 rounded-lg shadow-xl py-1 z-50">
                <div className="px-4 pt-2 pb-1 text-xs uppercase tracking-wide text-[#8b949e] flex items-center justify-between">
                  <span>Subtitles</span>
                  <span className="text-[10px] normal-case tracking-normal text-[#8b949e]/70">
                    {activeSubIds.length}/{MAX_ACTIVE_SUBS}
                  </span>
                </div>
                {/* Scrollable list — 10+ embedded subtitle streams
                    used to overflow the player chrome. max-h-72 keeps
                    the picker capped at ~10 visible rows. */}
                <div className="max-h-72 overflow-y-auto">
                  <button
                    onClick={() => chooseSub(null)}
                    className={`block w-full text-left px-4 py-2 hover:bg-white/10 ${activeSubIds.length === 0 ? 'text-[#58a6ff]' : ''}`}
                  >
                    None / Off
                  </button>
                  {mergedSubs.map((s) => {
                    const idx = activeSubIds.indexOf(s.id);
                    const selected = idx >= 0;
                    return (
                      <button
                        key={s.id}
                        onClick={() => chooseSub(s)}
                        className={`flex w-full items-center gap-2 text-left px-4 py-2 hover:bg-white/10 ${selected ? 'text-[#58a6ff]' : ''}`}
                      >
                        <span
                          aria-hidden
                          className={`inline-flex items-center justify-center w-4 h-4 rounded-sm border ${selected ? 'bg-[#58a6ff] border-[#58a6ff] text-white' : 'border-white/30'}`}
                        >
                          {selected ? (idx === 0 ? '1' : '2') : ''}
                        </span>
                        <span className="truncate">{s.label}</span>
                      </button>
                    );
                  })}
                </div>
                {/* Cue timing nudge — applies live to the active
                    track(s). Range ±10 s in 0.25 s steps; mutates
                    cue.startTime/endTime so changes survive without
                    re-fetching the VTT. Disabled when no track is
                    active. */}
                <div className="border-t border-white/10 mt-1 px-4 py-2">
                  <div className="text-[10px] uppercase tracking-wide text-[#8b949e] mb-1">
                    Timing offset
                  </div>
                  <div className="flex items-center gap-2">
                    <button
                      onClick={() => setSubOffsetSec((o) => Math.max(-10, +(o - 0.25).toFixed(2)))}
                      disabled={activeSubIds.length === 0 || subOffsetSec <= -10}
                      className="px-2 py-1 rounded bg-white/10 hover:bg-white/20 disabled:opacity-40 text-sm"
                      title="Earlier (−0.25s)"
                    >
                      −
                    </button>
                    <span className="flex-1 text-center text-sm tabular-nums">
                      {subOffsetSec > 0 ? '+' : ''}{subOffsetSec.toFixed(2)}s
                    </span>
                    <button
                      onClick={() => setSubOffsetSec((o) => Math.min(10, +(o + 0.25).toFixed(2)))}
                      disabled={activeSubIds.length === 0 || subOffsetSec >= 10}
                      className="px-2 py-1 rounded bg-white/10 hover:bg-white/20 disabled:opacity-40 text-sm"
                      title="Later (+0.25s)"
                    >
                      +
                    </button>
                    <button
                      onClick={() => setSubOffsetSec(0)}
                      disabled={activeSubIds.length === 0 || subOffsetSec === 0}
                      className="px-2 py-1 rounded bg-white/10 hover:bg-white/20 disabled:opacity-40 text-xs"
                      title="Reset offset"
                    >
                      Reset
                    </button>
                  </div>
                </div>
              </div>
            )}
          </div>

          {/* Quality switcher only makes sense when the active pipeline
              is the transcode ladder — the /copy/ and /v0/ paths don't
              have rungs to pick from, so showing the menu was misleading
              when nothing happened on click. */}
          {info && effectiveMode === 'transcode' && (
            <div className="relative">
              <button
                onClick={() => toggleMenu('quality')}
                className="p-2 bg-white/10 hover:bg-white/20 rounded-full transition-colors"
                title={`Quality (${effectiveMode === 'transcode' ? labelForQuality(streamQuality) : 'Direct'})`}
              >
                <Settings className="w-5 h-5" />
              </button>
              {qualityMenuOpen && (
                <div className="absolute right-0 bottom-full mb-2 min-w-[240px] bg-[#161b22] border border-white/10 rounded-lg shadow-xl py-1 z-50">
                  <div className="px-4 pt-2 pb-1 text-xs uppercase tracking-wide text-[#8b949e]">Quality</div>
                  {QUALITY_RUNGS.map((q) => (
                    <button
                      key={q}
                      onClick={() => switchQuality(q)}
                      className={`block w-full text-left px-4 py-2 hover:bg-white/10 ${streamQuality === q ? 'text-[#58a6ff]' : ''}`}
                    >
                      {labelForQuality(q)}
                    </button>
                  ))}
                </div>
              )}
            </div>
          )}

          {/* Playback speed — purely client-side, no pipeline impact.
              Always available regardless of mode (transcode/copy/
              passthrough/packaged), since playbackRate works on every
              HTMLMediaElement. */}
          <div className="relative">
            <button
              onClick={() => toggleMenu('speed')}
              className="p-2 bg-white/10 hover:bg-white/20 rounded-full transition-colors"
              title={`Playback speed (${playbackRate}x)`}
              aria-haspopup="menu"
              aria-expanded={speedMenuOpen}
            >
              <Gauge className="w-5 h-5" />
            </button>
            {speedMenuOpen && (
              <div
                className="absolute right-0 bottom-full mb-2 min-w-[180px] bg-[#161b22] border border-white/10 rounded-lg shadow-xl py-1 z-50"
                role="menu"
              >
                <div className="px-4 pt-2 pb-1 text-xs uppercase tracking-wide text-[#8b949e]">Speed</div>
                {PLAYBACK_RATES.map((rate) => (
                  <button
                    key={rate}
                    role="menuitem"
                    onClick={() => {
                      setPlaybackRate(rate);
                      setOpenMenu(null);
                      reportEvent('playback_rate_change', { rate });
                    }}
                    className={`block w-full text-left px-4 py-2 hover:bg-white/10 ${playbackRate === rate ? 'text-[#58a6ff]' : ''}`}
                  >
                    {rate === 1 ? 'Normal (1x)' : `${rate}x`}
                  </button>
                ))}
              </div>
            )}
          </div>

          {episodeNav.prev && (
            <button
              onClick={() => window.location.replace(`/player/${encodeURIComponent(episodeNav.prev!.id)}`)}
              className="p-2 bg-white/10 hover:bg-white/20 rounded-full transition-colors"
              title={`Previous episode: S${String(episodeNav.prev.season).padStart(2, '0')}E${String(episodeNav.prev.episode).padStart(2, '0')} — ${episodeNav.prev.title}`}
            >
              <ChevronLeft className="w-5 h-5" />
            </button>
          )}
          {episodeNav.next && (
            <button
              onClick={() => window.location.replace(`/player/${encodeURIComponent(episodeNav.next!.id)}?binge=1`)}
              className="p-2 bg-white/10 hover:bg-white/20 rounded-full transition-colors"
              title={`Next episode: S${String(episodeNav.next.season).padStart(2, '0')}E${String(episodeNav.next.episode).padStart(2, '0')} — ${episodeNav.next.title}`}
            >
              <ChevronRight className="w-5 h-5" />
            </button>
          )}

          <button
            onClick={() => setInfoOpen(true)}
            className="p-2 bg-white/10 hover:bg-white/20 rounded-full transition-colors"
            title="Playback info"
          >
            <Info className="w-5 h-5" />
          </button>

          <button
            onClick={toggleFullscreen}
            className="p-2 bg-white/10 hover:bg-white/20 rounded-full transition-colors"
            title={fullscreen ? 'Exit fullscreen' : 'Fullscreen'}
          >
            {fullscreen ? <Minimize className="w-5 h-5" /> : <Maximize className="w-5 h-5" />}
          </button>
        </div>
      </div>

      {infoOpen && (
        <PlaybackInfoDialog
          info={info}
          clientCodecs={clientCodecs}
          effectiveMode={effectiveMode}
          streamQuality={streamQuality}
          switchHistory={switchHistory}
          videoEl={videoRef.current}
          displayedCurrent={displayedCurrent}
          onClose={() => setInfoOpen(false)}
        />
      )}
    </div>
  );
}

function PlaybackInfoDialog({
  info,
  clientCodecs,
  effectiveMode,
  streamQuality,
  switchHistory,
  videoEl,
  displayedCurrent,
  onClose,
}: {
  info: PlayInfo | null;
  clientCodecs: { label: string; supported: boolean }[];
  effectiveMode: PlayInfo['mode'];
  streamQuality: Quality;
  switchHistory: SwitchEntry[];
  videoEl: HTMLVideoElement | null;
  displayedCurrent: number;
  onClose: () => void;
}) {
  const modeColor: Record<PlayInfo['mode'], string> = {
    passthrough: 'bg-emerald-500/20 text-emerald-300 border-emerald-500/40',
    remux:       'bg-amber-500/20 text-amber-300 border-amber-500/40',
    transcode:   'bg-rose-500/20 text-rose-300 border-rose-500/40',
    packaged:    'bg-emerald-500/20 text-emerald-300 border-emerald-500/40',
  };
  const modeLabel: Record<PlayInfo['mode'], string> = {
    passthrough: 'Passthrough (no transcode)',
    remux:       'Remux (stream-copy, repackaged into MP4)',
    transcode:   'Transcode (re-encoding required)',
    packaged:    'Packaged (pre-segmented CMAF on disk, no ffmpeg)',
  };

  // Recompute the live-state block on every render of the dialog
  // (open=>close cycle). The dialog is short-lived; no need to poll.
  const buffered = videoEl?.buffered;
  let bufferedAheadSec = 0;
  if (buffered && buffered.length > 0 && videoEl) {
    for (let i = 0; i < buffered.length; i++) {
      if (videoEl.currentTime >= buffered.start(i) && videoEl.currentTime <= buffered.end(i)) {
        bufferedAheadSec = buffered.end(i) - videoEl.currentTime;
        break;
      }
    }
  }
  const networkStateNames = ['EMPTY', 'IDLE', 'LOADING', 'NO_SOURCE'];
  const readyStateNames   = ['NOTHING', 'METADATA', 'CURRENT', 'FUTURE', 'ENOUGH'];
  const fmtClock = (ts: number) => {
    const d = new Date(ts);
    return `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}:${String(d.getSeconds()).padStart(2, '0')}`;
  };
  return (
    <div
      onClick={onClose}
      className="absolute inset-0 z-50 flex items-center justify-center bg-black/70"
    >
      <div
        onClick={(e) => e.stopPropagation()}
        className="w-full max-w-2xl mx-4 max-h-[85vh] overflow-y-auto rounded-xl bg-[#161b22] border border-white/10 shadow-2xl"
      >
        <div className="flex items-center justify-between px-6 py-4 border-b border-white/10">
          <h2 className="text-lg font-medium">Playback info</h2>
          <button onClick={onClose} className="p-1 rounded hover:bg-white/10" title="Close">
            <X className="w-5 h-5" />
          </button>
        </div>

        <div className="p-6 space-y-6 text-sm">
          {!info ? (
            <p className="text-[#8b949e]">Probing source file&hellip;</p>
          ) : (
            <>
              <div>
                <div className={`inline-flex items-center px-3 py-1 rounded-full border ${modeColor[info.mode]}`}>
                  {modeLabel[info.mode]}
                </div>
                <p className="mt-2 text-[#c9d1d9]">{info.reason}</p>
              </div>

              <div>
                <h3 className="font-medium mb-2">Source file</h3>
                <dl className="grid grid-cols-[8rem_1fr] gap-y-1">
                  <dt className="text-[#8b949e]">Container</dt>
                  <dd>{info.container || '—'}</dd>
                  <dt className="text-[#8b949e]">Video</dt>
                  <dd>{info.video_codec || '—'} {info.width > 0 && info.height > 0 ? `(${info.width}×${info.height})` : ''}</dd>
                  <dt className="text-[#8b949e]">Audio</dt>
                  <dd>{info.audio_codec || '—'}</dd>
                  <dt className="text-[#8b949e]">Duration</dt>
                  <dd>{info.duration_ms ? fmtDur(info.duration_ms / 1000) : '—'}</dd>
                </dl>
              </div>

              <div>
                <h3 className="font-medium mb-2">Live pipeline</h3>
                <dl className="grid grid-cols-[8rem_1fr] gap-y-1">
                  <dt className="text-[#8b949e]">Effective mode</dt>
                  <dd>
                    <span className={`inline-flex items-center px-2 py-0.5 rounded-full border text-xs ${modeColor[effectiveMode]}`}>
                      {modeLabel[effectiveMode]}
                    </span>
                  </dd>
                  {effectiveMode === 'transcode' ? (
                    <>
                      <dt className="text-[#8b949e]">Quality</dt>
                      <dd>{labelForQuality(streamQuality)}</dd>
                      <dt className="text-[#8b949e]">Encoder</dt>
                      <dd>
                        {info.encoder === 'h264_nvenc'
                          ? 'h264_nvenc (NVIDIA NVENC, GPU)'
                          : `${info.encoder || 'libx264'} (CPU)`}
                      </dd>
                      <dt className="text-[#8b949e]">Video target</dt>
                      <dd>H.264 High@4.0 · {QUALITY_SPEC[streamQuality].scale} · {info.encoder === 'h264_nvenc' ? `CQ ${22}` : `CRF ${QUALITY_SPEC[streamQuality].crf}`}</dd>
                      <dt className="text-[#8b949e]">Audio target</dt>
                      <dd>AAC stereo · {QUALITY_SPEC[streamQuality].abps}</dd>
                    </>
                  ) : (
                    <>
                      <dt className="text-[#8b949e]">Quality</dt>
                      <dd>Direct ({info.video_codec.toUpperCase()})</dd>
                      <dt className="text-[#8b949e]">Encoder</dt>
                      <dd>None — source bytes pass through unmodified</dd>
                    </>
                  )}
                  <dt className="text-[#8b949e]">Position</dt>
                  <dd>{fmtDur(displayedCurrent)} / {info.duration_ms ? fmtDur(info.duration_ms / 1000) : '—'}</dd>
                  <dt className="text-[#8b949e]">Buffered ahead</dt>
                  <dd>{bufferedAheadSec > 0 ? `${bufferedAheadSec.toFixed(1)}s` : '—'}</dd>
                  <dt className="text-[#8b949e]">Element state</dt>
                  <dd>
                    {videoEl ? `${readyStateNames[videoEl.readyState] ?? '?'} · ${networkStateNames[videoEl.networkState] ?? '?'}` : '—'}
                  </dd>
                </dl>
              </div>

              {switchHistory.length > 0 ? (
                <div>
                  <h3 className="font-medium mb-2">Pipeline switches</h3>
                  <ul className="space-y-1 text-xs font-mono">
                    {switchHistory.slice().reverse().map((e, i) => (
                      <li key={i} className="flex items-start gap-3">
                        <span className="text-[#8b949e] tabular-nums shrink-0">{fmtClock(e.ts)}</span>
                        <span className={`shrink-0 uppercase tracking-wider w-14 ${
                          e.reason === 'manual'  ? 'text-sky-300' :
                          e.reason === 'auto'    ? 'text-amber-300' :
                          e.reason === 'stall'   ? 'text-rose-300' :
                                                    'text-[#8b949e]'
                        }`}>{e.reason}</span>
                        <span className="text-[#c9d1d9]">{e.label}</span>
                        {e.detail ? <span className="text-[#8b949e]">— {e.detail}</span> : null}
                      </li>
                    ))}
                  </ul>
                </div>
              ) : null}
            </>
          )}

          <div>
            <h3 className="font-medium mb-2">This browser can decode</h3>
            <ul className="grid grid-cols-2 gap-y-1">
              {clientCodecs.map((c) => (
                <li key={c.label} className="flex items-center gap-2">
                  <span className={`inline-block w-2 h-2 rounded-full ${c.supported ? 'bg-emerald-400' : 'bg-rose-400'}`} />
                  <span className={c.supported ? '' : 'text-[#8b949e]'}>{c.label}</span>
                </li>
              ))}
            </ul>
            <p className="mt-3 text-xs text-[#8b949e]">
              When the source file uses a codec the browser can't decode, katalog-stream
              re-encodes it on the fly to H.264 + AAC inside a fragmented MP4.
            </p>
          </div>
        </div>
      </div>
    </div>
  );
}

function labelForQuality(q: Quality): string {
  switch (q) {
    case 'high':   return 'High (source)';
    case 'medium': return 'Medium (720p)';
    case 'low':    return 'Low (480p)';
  }
}

// Friendly display string for a media segment. katalog stores `label` as
// detector evidence (introskipper, chromaprint xN, silence N.Ns,
// text:<srt cue>) which leaks into the player chrome. Strategy: kind-based
// friendly map first; trust label verbatim only for kind=chapter (genuine
// atom titles) or when the string looks human. Title-case fallback for
// unknown kinds.
const SEGMENT_KIND_LABELS: Record<string, string> = {
  intro: 'Intro',
  credits: 'Credits',
  recap: 'Recap',
  preview: 'Preview',
  sponsor: 'Sponsor',
  commercial: 'Commercial',
  chapter: 'Chapter',
};
const DETECTOR_LABEL_PREFIX_RE = /^(chromaprint|silence|black|text:|no dialogue)\b/i;
const NUMERIC_LABEL_TAG_RE = /^\S+\s+(x\d+|\d+(\.\d+)?s)\b/i;
function segmentDisplayLabel(seg: { kind: string; label?: string }): string {
  const friendlyKind =
    SEGMENT_KIND_LABELS[seg.kind] ||
    (seg.kind ? seg.kind.charAt(0).toUpperCase() + seg.kind.slice(1) : '');
  if (seg.kind === 'chapter' && seg.label && seg.label.trim()) return seg.label.trim();
  const raw = (seg.label || '').trim();
  if (!raw) return friendlyKind;
  if (raw.includes('-')) return friendlyKind;
  if (DETECTOR_LABEL_PREFIX_RE.test(raw)) return friendlyKind;
  if (NUMERIC_LABEL_TAG_RE.test(raw)) return friendlyKind;
  return raw;
}

function fmtDur(s: number): string {
  if (!isFinite(s) || s < 0) return '0:00';
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  const sec = Math.floor(s % 60).toString().padStart(2, '0');
  if (h > 0) return `${h}:${m.toString().padStart(2, '0')}:${sec}`;
  return `${m}:${sec}`;
}
