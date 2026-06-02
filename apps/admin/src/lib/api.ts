// Typed client for the manage-API.
//
// CONTRACT (katalog-manager-api implements; this UI consumes — keep both
// sides identical):
//
//   GET  /api/manage/setup/status -> SetupStatus
//   POST /api/manage/setup        body SetupRequest -> { configured: true }
//   GET  /api/manage/config       -> ManageConfig (non-secret)
//   PUT  /api/manage/config       body Partial<ManageConfig> -> ManageConfig
//   GET  /api/manage/library      -> { items: LibraryItem[], total: number }
//   POST /api/manage/import/scan  body { path } -> ImportJob
//   GET  /api/manage/jobs         -> { jobs: Job[] }
//
// The base path is configurable so the SPA can run behind /manage with the
// API on the same origin (default) or against a remote dev backend.

const API_BASE: string =
  (import.meta.env.VITE_MANAGE_API_BASE as string | undefined)?.replace(/\/$/, '') ??
  '/api/manage';

// ── Contract types ─────────────────────────────────────────────────────────

export interface SetupChecks {
  database: boolean;
  kafka: boolean;
  library: boolean;
}

export interface SetupStatus {
  configured: boolean;
  version: string;
  checks: SetupChecks;
}

export interface SetupRequest {
  displayName: string;
  oidcIssuer: string;
  oidcClientId: string;
  libraryPath: string;
  /** Optional. If omitted the server generates one and keeps it secret. */
  streamSigningKey?: string;
}

export interface SetupResult {
  configured: true;
}

/** Non-secret config returned by GET /config. The stream signing key is a
 *  secret and is never echoed back — the server only reports whether one is
 *  present via `streamSigningKeySet`. */
export interface ManageConfig {
  displayName: string;
  oidcIssuer: string;
  oidcClientId: string;
  libraryPath: string;
  streamSigningKeySet: boolean;
  version: string;
}

export interface LibraryItem {
  id: string;
  type: 'movie' | 'series' | 'album' | 'episode' | 'track' | string;
  title: string;
  year?: number;
  /** Relative path under the library root, for operator orientation. */
  path?: string;
  durationMs?: number;
  sizeBytes?: number;
  /** ISO timestamp the item was first catalogued. */
  addedAt?: string;
}

export interface LibraryPage {
  items: LibraryItem[];
  total: number;
}

export type JobState = 'queued' | 'running' | 'done' | 'failed';

export interface Job {
  id: string;
  kind: 'scan' | 'enrich' | 'analyze' | 'artwork' | 'transcode' | 'package' | string;
  state: JobState;
  /** 0..100 when the job reports granular progress. */
  progress?: number;
  /** Human label, e.g. "Scanning /library/movies". */
  detail?: string;
  startedAt?: string;
  finishedAt?: string;
  error?: string;
}

export interface JobsPage {
  jobs: Job[];
}

export interface ImportJob {
  jobId: string;
  path: string;
  state: JobState;
}

// ── Error type ───────────────────────────────────────────────────────────

export class ApiError extends Error {
  status: number;
  body?: unknown;
  constructor(status: number, message: string, body?: unknown) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
    this.body = body;
  }
}

// ── Core request helper ─────────────────────────────────────────────────

async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const headers = new Headers(init.headers);
  if (init.body && !headers.has('Content-Type')) {
    headers.set('Content-Type', 'application/json');
  }
  headers.set('Accept', 'application/json');

  let res: Response;
  try {
    res = await fetch(`${API_BASE}${path}`, { ...init, headers });
  } catch (e) {
    // Network / CORS / server-down — surface as a 0-status ApiError so the
    // pages render a coherent "can't reach the server" state.
    throw new ApiError(0, (e as Error).message || 'Network error');
  }

  const text = await res.text();
  let parsed: unknown = undefined;
  if (text) {
    try {
      parsed = JSON.parse(text);
    } catch {
      parsed = text;
    }
  }

  if (!res.ok) {
    const msg =
      (parsed && typeof parsed === 'object' && 'error' in parsed
        ? String((parsed as { error: unknown }).error)
        : undefined) ?? `${res.status} ${res.statusText}`;
    throw new ApiError(res.status, msg, parsed);
  }

  return parsed as T;
}

// ── Typed endpoints ─────────────────────────────────────────────────────

export const api = {
  /** First-run gate. The Launchpad calls this on load and redirects to the
   *  wizard when `configured` is false. */
  setupStatus(signal?: AbortSignal): Promise<SetupStatus> {
    return request<SetupStatus>('/setup/status', { signal });
  },

  /** Persist first-run config. The server generates `streamSigningKey` if the
   *  body omits it. */
  setup(body: SetupRequest, signal?: AbortSignal): Promise<SetupResult> {
    return request<SetupResult>('/setup', {
      method: 'POST',
      body: JSON.stringify(body),
      signal,
    });
  },

  getConfig(signal?: AbortSignal): Promise<ManageConfig> {
    return request<ManageConfig>('/config', { signal });
  },

  updateConfig(body: Partial<ManageConfig>, signal?: AbortSignal): Promise<ManageConfig> {
    return request<ManageConfig>('/config', {
      method: 'PUT',
      body: JSON.stringify(body),
      signal,
    });
  },

  library(
    params: { q?: string; limit?: number; offset?: number } = {},
    signal?: AbortSignal,
  ): Promise<LibraryPage> {
    const qs = new URLSearchParams();
    if (params.q) qs.set('q', params.q);
    if (params.limit != null) qs.set('limit', String(params.limit));
    if (params.offset != null) qs.set('offset', String(params.offset));
    const suffix = qs.toString() ? `?${qs}` : '';
    return request<LibraryPage>(`/library${suffix}`, { signal });
  },

  importScan(path: string, signal?: AbortSignal): Promise<ImportJob> {
    return request<ImportJob>('/import/scan', {
      method: 'POST',
      body: JSON.stringify({ path }),
      signal,
    });
  },

  jobs(signal?: AbortSignal): Promise<JobsPage> {
    return request<JobsPage>('/jobs', { signal });
  },
};

/** Generate a 256-bit hex stream-signing key in the browser for the wizard's
 *  "generate" affordance. The server still generates its own if the field is
 *  left blank; this just lets an operator see/copy a value up front. */
export function generateSigningKey(): string {
  const bytes = new Uint8Array(32);
  crypto.getRandomValues(bytes);
  return Array.from(bytes)
    .map((b) => b.toString(16).padStart(2, '0'))
    .join('');
}
