export interface ZapSegment {
  kind: string;
  start_ms: number;
  end_ms: number;
  label?: string;
}

export interface MidpointInput {
  durationMs?: number;
  segments?: ZapSegment[];
  /** Optional 0..1 ratio that decides where inside the act-2 window we
   *  land. Pass a fresh Math.random() per mount to get a different
   *  scene every zap — that's the channel-flip feel. Omit to fall back
   *  to a deterministic 0.40 ratio (early act 2, useful for tests). */
  randomRatio?: number;
}

export interface MidpointResult {
  /** Seconds into the source where the zap should start playback. */
  seekSec: number;
  /** How we picked it — kept for telemetry. */
  source: 'segments' | 'percent' | 'fallback';
}

/** Lower clamp on how far we'll skip into ANY content — never start in
 *  the first minute. Otherwise short episodes / movies feel like they
 *  haven't moved past the opening logo. */
const MIN_SEEK_SEC = 60;

/** Upper clamp: keep at least 90 s of runway after the seek point so
 *  even an aggressive credits-detector hasn't already taken us into
 *  the curtain call. For 22-min episodes this dominates over the
 *  percent heuristic, which is what we want. */
const TAIL_BUFFER_SEC = 90;

/** Deterministic fallback ratio when the caller doesn't pass a random
 *  one. 0.40 = roughly the midpoint of act 2 for a 3-act movie. */
const DEFAULT_RATIO = 0.40;

/** When we have segments, sample uniformly inside the
 *  [intro_end, credits_start] window. When we don't, sample uniformly
 *  inside [0, durationSec] (then clamp to runway).
 *
 *  Constrain the random range so we don't always land in act 1 or
 *  the closing scene — 0.10..0.80 of the available window covers the
 *  meat of the content with a bit of edge variety. Without this
 *  squeeze, ~10 % of zaps would land in the first 60 s after the
 *  intro (still feels like the start) or the last 90 s before
 *  credits (feels like the end). */
const RAND_LOW = 0.10;
const RAND_HIGH = 0.80;

function ratioFromRandom(r: number | undefined): number {
  if (r === undefined) return DEFAULT_RATIO;
  // Clamp to [0,1] just in case the caller passes a weird value, then
  // squeeze into [RAND_LOW, RAND_HIGH].
  const r01 = Math.max(0, Math.min(1, r));
  return RAND_LOW + r01 * (RAND_HIGH - RAND_LOW);
}

/**
 * Pick a "good" mid-content seek point for a zap. Prefers explicit
 * intro/credits segments when the analyzer has produced them, falls
 * back to a percentage of runtime otherwise. Pure function — easy to
 * unit-test, no React state.
 *
 * Returns 0 + source='fallback' when we don't even have a duration to
 * work with (caller should treat as "skip this card" or just start
 * from 0). Callers should NOT use this result without checking source.
 */
export function pickZapMidpoint(input: MidpointInput): MidpointResult {
  const durationSec = Math.floor((input.durationMs ?? 0) / 1000);
  if (durationSec <= MIN_SEEK_SEC + TAIL_BUFFER_SEC) {
    // Too short to safely pick a midpoint — start at the floor anyway,
    // the player can deal with overshoots via end-of-stream teardown.
    return { seekSec: Math.max(0, Math.min(MIN_SEEK_SEC, Math.floor(durationSec / 2))), source: 'fallback' };
  }

  const lower = MIN_SEEK_SEC;
  const upper = durationSec - TAIL_BUFFER_SEC;
  const ratio = ratioFromRandom(input.randomRatio);

  // Segment-aware path: land somewhere inside the inner window
  // bounded by the latest intro end and the earliest credits start.
  const segs = input.segments ?? [];
  if (segs.length > 0) {
    const introEndSec = segs
      .filter((s) => s.kind === 'intro')
      .reduce((max, s) => Math.max(max, Math.floor(s.end_ms / 1000)), 0);
    const creditsStartSec = segs
      .filter((s) => s.kind === 'credits')
      .reduce((min, s) => Math.min(min, Math.floor(s.start_ms / 1000)), durationSec);
    // Both ends usable → pick inside the inner window using the
    // caller's random ratio.
    if (introEndSec > 0 && creditsStartSec < durationSec && creditsStartSec > introEndSec + 60) {
      const windowSec = creditsStartSec - introEndSec;
      const seekSec = clamp(Math.round(introEndSec + windowSec * ratio), lower, upper);
      return { seekSec, source: 'segments' };
    }
    // Only intro known → land somewhere in the post-intro half of
    // the remaining runtime. Scale the random ratio across that
    // half so each zap still gets a different scene.
    if (introEndSec > 0) {
      const windowSec = Math.max(0, durationSec - TAIL_BUFFER_SEC - introEndSec);
      const seekSec = clamp(Math.round(introEndSec + windowSec * ratio), lower, upper);
      return { seekSec, source: 'segments' };
    }
    // Only credits known → sample randomly in the pre-credits window.
    //
    // Sanity check: an analyzer that flags credits before the halfway
    // point on a long runtime is almost certainly wrong (e.g., a cold
    // open marked as credits). Trust the percent fallback in that
    // case instead of squeezing the user into a tiny window.
    if (creditsStartSec < durationSec) {
      const creditsLooksTrustworthy =
        creditsStartSec >= durationSec * 0.5 ||
        creditsStartSec >= lower + TAIL_BUFFER_SEC * 2;
      if (!creditsLooksTrustworthy) {
        const seekSec = clamp(Math.round(durationSec * ratio), lower, upper);
        return { seekSec, source: 'percent' };
      }
      const hi = Math.max(lower, Math.min(upper, creditsStartSec - TAIL_BUFFER_SEC));
      const seekSec = clamp(Math.round(durationSec * ratio), lower, hi);
      return { seekSec, source: 'percent' };
    }
  }

  // No segments at all: sample uniformly across the runtime,
  // clamped to runway.
  const seekSec = clamp(Math.round(durationSec * ratio), lower, upper);
  return { seekSec, source: 'percent' };
}

function clamp(v: number, lo: number, hi: number): number {
  return Math.max(lo, Math.min(hi, v));
}
