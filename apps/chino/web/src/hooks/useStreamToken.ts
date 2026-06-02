import { useEffect, useState } from 'react';
import { useAuth } from 'react-oidc-context';

// Minted server-side via POST /api/v1/me/stream-token. Lives 6 h. Used
// in the URL of any media asset that must survive an OIDC silent
// renew without a refetch: <video src>, <img src> for posters /
// backdrops, <track src> for embedded subtitles. Without this, every
// renewal rotates the URL of every <img> on screen and the browser
// re-fetches them all — visible as a multi-second flicker on the
// catalog grid every ~5 min.
//
// The token is cached in sessionStorage so a page reload reuses the
// same value (the browser's HTTP cache then serves the same artwork
// URLs straight from cache). The hook never fetches a new token while
// one is still valid; it only re-mints if the cached token is gone
// or expired.

const STORAGE_KEY = 'chino:streamToken';

interface CachedToken {
  token: string;
  expiresAt: number; // epoch ms
}

let inflight: Promise<string | null> | null = null;

function readCache(): CachedToken | null {
  try {
    const raw = sessionStorage.getItem(STORAGE_KEY);
    if (!raw) return null;
    const parsed = JSON.parse(raw) as CachedToken;
    if (!parsed.token || typeof parsed.expiresAt !== 'number') return null;
    // 60 s safety margin so we refresh before the server-side
    // validator starts rejecting.
    if (Date.now() > parsed.expiresAt - 60_000) return null;
    return parsed;
  } catch {
    return null;
  }
}

function writeCache(c: CachedToken) {
  try { sessionStorage.setItem(STORAGE_KEY, JSON.stringify(c)); } catch { /* quota */ }
}

async function mintStreamToken(bearer: string): Promise<string | null> {
  if (inflight) return inflight;
  inflight = (async () => {
    try {
      const r = await fetch('/api/v1/me/stream-token', {
        method: 'POST',
        headers: { Authorization: `Bearer ${bearer}` },
      });
      if (!r.ok) return null;
      const j = await r.json() as { stream_token?: string; expires_at?: string };
      if (!j.stream_token) return null;
      const expiresAt = j.expires_at ? new Date(j.expires_at).getTime() : Date.now() + 5 * 60 * 60 * 1000;
      writeCache({ token: j.stream_token, expiresAt });
      return j.stream_token;
    } catch {
      return null;
    } finally {
      inflight = null;
    }
  })();
  return inflight;
}

/**
 * Returns the current stream token, minting one on first call (and
 * after expiry). Returns null while loading or unauthenticated; the
 * caller should fall back gracefully (omit the asset, show a
 * placeholder, etc).
 */
export function useStreamToken(): string | null {
  const auth = useAuth();
  const [token, setToken] = useState<string | null>(() => readCache()?.token ?? null);

  useEffect(() => {
    if (token) return;
    const bearer = auth.user?.access_token;
    if (!bearer) return;
    let cancelled = false;
    mintStreamToken(bearer).then(t => {
      if (!cancelled && t) setToken(t);
    });
    return () => { cancelled = true; };
  }, [auth.user?.access_token, token]);

  // Proactive expiry: schedule a one-shot timer to null the state
  // shortly before the cached entry expires so the mint effect above
  // re-runs. Without this, a tab left open past the 6 h TTL hangs on
  // to a stale token forever — the cache's expiry check only runs on
  // mount. `visibilitychange` covers the suspended-tab case where
  // browser timer throttling could overshoot the expiry while hidden.
  useEffect(() => {
    if (!token) return;
    const cached = readCache();
    if (!cached) {
      // Cache says expired-or-missing but state still holds a string —
      // drop it so the mint effect re-fires.
      setToken(null);
      return;
    }
    const refreshMs = cached.expiresAt - Date.now() - 60_000;
    const timer = window.setTimeout(() => setToken(null), Math.max(0, refreshMs));
    const onVis = () => {
      if (document.visibilityState !== 'visible') return;
      if (!readCache()) setToken(null);
    };
    document.addEventListener('visibilitychange', onVis);
    return () => {
      window.clearTimeout(timer);
      document.removeEventListener('visibilitychange', onVis);
    };
  }, [token]);

  return token;
}
