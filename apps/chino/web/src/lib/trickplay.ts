// Trickplay (scrub-preview thumbnails) parser + lookup helpers.
//
// The analyzer writes a WebVTT cue file alongside a set of sprite
// sheet JPGs at /api/v1/items/{id}/play/trickplay/. Each cue maps a
// time range to one tile inside one sprite sheet:
//
//   00:01:20.000 --> 00:01:30.000
//   sprite-0001.jpg#xywh=320,180,320,180
//
// On scrub-bar hover we want to: (1) translate the cursor X to a
// timestamp, (2) find the matching cue, (3) render the sprite with
// the right tile cropped in. This file owns the parse + lookup; the
// PlayerPage owns the DOM.

export interface TrickplayCue {
  /** Inclusive start, in seconds. */
  startSec: number;
  /** Exclusive end, in seconds. */
  endSec: number;
  /** Sprite-sheet filename, relative to the trickplay/ directory. */
  sprite: string;
  /** Pixel offsets of the tile inside its sprite sheet. */
  x: number;
  y: number;
  w: number;
  h: number;
}

const CUE_TIMING_RE = /^(\d+):(\d{2}):(\d{2}(?:\.\d+)?)\s+-->\s+(\d+):(\d{2}):(\d{2}(?:\.\d+)?)/;
const PAYLOAD_RE = /^(.+?)#xywh=(\d+),(\d+),(\d+),(\d+)\s*$/;

/** Parses a WebVTT thumbnails file into a list of cues. Tolerant of
 * missing blank lines + leading BOM; ignores cues whose payload
 * doesn't have the expected `sprite.jpg#xywh=x,y,w,h` shape. */
export function parseTrickplayVTT(text: string): TrickplayCue[] {
  const cues: TrickplayCue[] = [];
  // Strip optional BOM, normalise line endings, drop the WEBVTT
  // header line if present. We don't enforce WEBVTT being present —
  // some tooling omits it and the cues still parse.
  const lines = text.replace(/^﻿/, '').split(/\r?\n/);
  for (let i = 0; i < lines.length; i++) {
    const m = CUE_TIMING_RE.exec(lines[i]);
    if (!m) continue;
    const startSec = (+m[1]) * 3600 + (+m[2]) * 60 + parseFloat(m[3]);
    const endSec = (+m[4]) * 3600 + (+m[5]) * 60 + parseFloat(m[6]);
    // The payload sits on the very next non-empty line.
    let payload = '';
    for (let j = i + 1; j < lines.length; j++) {
      const t = lines[j].trim();
      if (t) { payload = t; break; }
    }
    const p = PAYLOAD_RE.exec(payload);
    if (!p) continue;
    cues.push({
      startSec,
      endSec,
      sprite: p[1],
      x: +p[2], y: +p[3], w: +p[4], h: +p[5],
    });
  }
  return cues;
}

/** Find the cue covering `tSec`. O(log n) via binary search — the
 * hover handler fires every mousemove, so the search has to be cheap
 * even when the cue list runs into the thousands. */
export function findTrickplayCue(cues: TrickplayCue[], tSec: number): TrickplayCue | null {
  if (cues.length === 0) return null;
  let lo = 0;
  let hi = cues.length - 1;
  while (lo <= hi) {
    const mid = (lo + hi) >>> 1;
    const c = cues[mid];
    if (tSec < c.startSec) hi = mid - 1;
    else if (tSec >= c.endSec) lo = mid + 1;
    else return c;
  }
  return null;
}
