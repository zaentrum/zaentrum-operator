import { useEffect, useState } from 'react';

/**
 * User-tunable client-side settings. Persisted to localStorage so each
 * device keeps its own preferences without a backend round-trip.
 *
 * The binge block controls auto-skip behaviour during episode runs:
 *   - autoSkipIntro      flips on the "watching intro in N… skipping"
 *                        countdown when the player enters an intro
 *                        segment AND a binge continuation is detected
 *                        (the user already watched a different episode
 *                        of this series recently).
 *   - autoSkipCredits    skips the credits roll at the end of an
 *                        episode (rather than just offering Skip
 *                        Credits / Next episode buttons).
 *   - autoPlayNext       fires the next-episode auto-play when the
 *                        playhead enters credits and the chromaprint
 *                        pipeline identified a sibling episode.
 *   - countdownSec       how long each auto-skip / auto-play overlay
 *                        sits before firing. Capped at 15s.
 */
export interface ChinoSettings {
  binge: {
    enabled: boolean;
    autoSkipIntro: boolean;
    autoSkipCredits: boolean;
    autoPlayNext: boolean;
    countdownSec: number;
  };
  subtitles: {
    preferredLang: string; // 'eng' / 'deu' / … / 'off'
  };
}

export const DEFAULT_SETTINGS: ChinoSettings = {
  binge: {
    enabled: true,
    autoSkipIntro: true,
    autoSkipCredits: true,
    autoPlayNext: true,
    countdownSec: 3,
  },
  subtitles: { preferredLang: 'eng' },
};

const SETTINGS_KEY = 'chino:settings:v1';
const BINGE_SESSION_PREFIX = 'chino:bingeSession:';
// How long an inter-episode pause still counts as a binge continuation.
// 6 hours is the convention Netflix / Apple TV+ use — long enough to
// cover dinner breaks, short enough that "I came back the next day"
// resets the auto-skip behaviour so the user sees the intro again.
export const BINGE_CONTINUATION_WINDOW_MS = 6 * 3600 * 1000;

export function loadSettings(): ChinoSettings {
  if (typeof window === 'undefined') return DEFAULT_SETTINGS;
  try {
    const raw = window.localStorage.getItem(SETTINGS_KEY);
    if (!raw) return DEFAULT_SETTINGS;
    const parsed = JSON.parse(raw);
    return {
      binge: { ...DEFAULT_SETTINGS.binge, ...(parsed.binge ?? {}) },
      subtitles: { ...DEFAULT_SETTINGS.subtitles, ...(parsed.subtitles ?? {}) },
    };
  } catch {
    return DEFAULT_SETTINGS;
  }
}

export function saveSettings(s: ChinoSettings) {
  if (typeof window === 'undefined') return;
  try {
    window.localStorage.setItem(SETTINGS_KEY, JSON.stringify(s));
    // Cross-tab + same-tab listeners pick this up.
    window.dispatchEvent(new CustomEvent('chino:settings-changed'));
  } catch {
    /* quota / private mode — ignore */
  }
}

/** Hook returning the current settings plus an updater that persists. */
export function useSettings(): [ChinoSettings, (s: ChinoSettings) => void] {
  const [s, setS] = useState<ChinoSettings>(loadSettings);
  useEffect(() => {
    const onChange = () => setS(loadSettings());
    window.addEventListener('chino:settings-changed', onChange);
    window.addEventListener('storage', onChange);
    return () => {
      window.removeEventListener('chino:settings-changed', onChange);
      window.removeEventListener('storage', onChange);
    };
  }, []);
  const update = (next: ChinoSettings) => {
    setS(next);
    saveSettings(next);
  };
  return [s, update];
}

// ----------------------------------------------------------------------
// Binge-session tracking. One key per series; the value records the
// last episode the user played and when. isBingeContinuation() returns
// true iff the user is starting a DIFFERENT episode of the SAME series
// inside the inter-episode window.
// ----------------------------------------------------------------------

interface BingeSession {
  episodeId: string;
  ts: number;
}

export function recordEpisodePlay(seriesId: string | undefined, episodeId: string) {
  if (!seriesId || !episodeId || typeof window === 'undefined') return;
  try {
    const data: BingeSession = { episodeId, ts: Date.now() };
    window.localStorage.setItem(BINGE_SESSION_PREFIX + seriesId, JSON.stringify(data));
  } catch {
    /* ignore */
  }
}

export function isBingeContinuation(
  seriesId: string | undefined,
  episodeId: string,
): boolean {
  if (!seriesId || !episodeId || typeof window === 'undefined') return false;
  try {
    const raw = window.localStorage.getItem(BINGE_SESSION_PREFIX + seriesId);
    if (!raw) return false;
    const { episodeId: lastId, ts } = JSON.parse(raw) as BingeSession;
    if (lastId === episodeId) return false;
    if (Date.now() - ts > BINGE_CONTINUATION_WINDOW_MS) return false;
    return true;
  } catch {
    return false;
  }
}
