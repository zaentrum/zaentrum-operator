import { useCallback, useMemo, useRef } from 'react';

/** Inputs we score against. Genres + cast names are pulled from
 *  /api/v1/items/{id} (ItemDetail). type is the macro-bucket
 *  (movie/series/episode) — useful when one user clearly prefers TV. */
export interface ZapItemFeatures {
  type?: string;
  genres?: string[];
  castNames?: string[];
}

/** Strength of an observed signal. Positive = liked, negative =
 *  disliked. Used by update() to bump weights. */
export type ZapSignalStrength = number;

/** Score returned by score() — higher means "more likely to be liked".
 *  Capped softly via tanh so a few strong signals don't dominate the
 *  rest of the catalog. */
export interface ZapScore {
  raw: number;
  normalized: number;
}

/** Exponential decay applied to every weight on every update. Without
 *  this, a strong initial like can permanently dominate the vector.
 *  0.92 ≈ "half-life" of 8 updates, which feels responsive but not
 *  amnesiac. */
const DECAY = 0.92;

/** Floor below which a weight is dropped to keep the map small. */
const MIN_WEIGHT = 0.05;

interface UseZapPreferences {
  /** Bump the user's preference vector using one item's features. */
  update: (features: ZapItemFeatures, strength: ZapSignalStrength) => void;
  /** Score an item against the current vector. Items with no features
   *  yet (genres/cast not loaded) return 0 — caller should treat that
   *  as neutral, not negative. */
  score: (features: ZapItemFeatures) => ZapScore;
  /** Snapshot for telemetry — small + JSON-serializable. */
  snapshot: () => { genres: Record<string, number>; cast: Record<string, number>; type: Record<string, number> };
}

/**
 * In-memory preference vector for the Zap feature. Three maps:
 *   - genres  → weight  ("Sci-Fi" 1.4, "Drama" -0.3)
 *   - cast    → weight  (actor names from /items/{id})
 *   - type    → weight  ("movie", "series", "episode")
 *
 * V1 has no persistence: the vector dies with the tab. That's a
 * deliberate scope cut — landing the feature without committing to a
 * schema. V2 persists to chino-api Postgres.
 */
export function useZapPreferences(): UseZapPreferences {
  const genres = useRef(new Map<string, number>());
  const cast = useRef(new Map<string, number>());
  const types = useRef(new Map<string, number>());

  const decay = (m: Map<string, number>) => {
    for (const [k, v] of m) {
      const next = v * DECAY;
      if (Math.abs(next) < MIN_WEIGHT) m.delete(k);
      else m.set(k, next);
    }
  };

  const bump = (m: Map<string, number>, key: string, delta: number) => {
    if (!key) return;
    const cur = m.get(key) ?? 0;
    m.set(key, cur + delta);
  };

  const update: UseZapPreferences['update'] = useCallback((features, strength) => {
    // Decay first so this update genuinely shifts the vector instead
    // of just being added to a fossilised history.
    decay(genres.current);
    decay(cast.current);
    decay(types.current);

    const gs = features.genres ?? [];
    const perGenre = gs.length ? strength / gs.length : 0;
    for (const g of gs) bump(genres.current, g, perGenre);

    const cs = (features.castNames ?? []).slice(0, 5);
    const perCast = cs.length ? (strength * 0.5) / cs.length : 0;
    for (const c of cs) bump(cast.current, c, perCast);

    if (features.type) bump(types.current, features.type, strength * 0.3);
  }, []);

  const score: UseZapPreferences['score'] = useCallback((features) => {
    let raw = 0;
    for (const g of features.genres ?? []) raw += genres.current.get(g) ?? 0;
    for (const c of (features.castNames ?? []).slice(0, 5)) raw += cast.current.get(c) ?? 0;
    if (features.type) raw += types.current.get(features.type) ?? 0;
    // tanh squashes to (-1, 1) so a runaway weight can't bury other
    // candidates by orders of magnitude. We don't need fine-grained
    // discrimination above the saturation point.
    return { raw, normalized: Math.tanh(raw) };
  }, []);

  const snapshot: UseZapPreferences['snapshot'] = useCallback(() => ({
    genres: Object.fromEntries(genres.current),
    cast: Object.fromEntries(cast.current),
    type: Object.fromEntries(types.current),
  }), []);

  // Wrap in useMemo so consumers see a stable handle. The three
  // inner callables are already useCallback-stable; this just keeps
  // the wrapping object's identity from churning every render and
  // cascading into downstream useEffect re-runs.
  return useMemo<UseZapPreferences>(() => ({
    update,
    score,
    snapshot,
  }), [update, score, snapshot]);
}
