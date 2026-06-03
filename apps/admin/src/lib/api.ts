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
//   GET    /api/manage/users                      -> User[]
//   POST   /api/manage/users                       body NewUser -> { id }
//   PUT    /api/manage/users/{id}                  body Partial<UserUpdate>
//   DELETE /api/manage/users/{id}
//   POST   /api/manage/users/{id}/reset-password   body { password, temporary? }
//
//   GET    /api/manage/instance   -> InstanceView
//   PATCH  /api/manage/instance   body InstancePatch -> InstanceView
//
// The base path is configurable so the SPA can run behind /manage with the
// API on the same origin (default) or against a remote dev backend.

import { getAccessToken } from '../auth/token';

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
  libraryPath: string;
  /** OIDC issuer the platform validates tokens against. The server REQUIRES
   *  this (rejects the request 400 without it). For the bundled identity
   *  provider the wizard echoes back the issuer it discovered from
   *  GET /api/config; for the advanced path the operator supplies their own. */
  oidcIssuer: string;
  /** Public OIDC client id. Also REQUIRED by the server. For the bundled IdP
   *  this is the discovered web client (`chino-web`); for the advanced path the
   *  operator's own public client. */
  oidcClientId: string;
  /** Optional. If omitted the server generates one and keeps it secret. */
  streamSigningKey?: string;
}

export interface SetupResult {
  configured: true;
}

/** Non-secret config returned by GET /config. The stream signing key is a
 *  secret and is never echoed back — the server only reports whether one is
 *  present via `streamSigningKeySet`. An empty `oidcIssuer` means the bundled
 *  identity provider is in use. */
export interface ManageConfig {
  displayName: string;
  oidcIssuer: string;
  oidcClientId: string;
  libraryPath: string;
  streamSigningKeySet: boolean;
  version: string;
}

// ── Users ───────────────────────────────────────────────────────────────
//
// Backed by the bundled identity provider via the manage-API. The shape
// mirrors the Keycloak user representation the manage-API translates to/from.

export interface User {
  id: string;
  username: string;
  email: string;
  firstName: string;
  lastName: string;
  enabled: boolean;
}

/** Body for POST /users. The password is optional — omit it to create the
 *  account without credentials and set one later via reset-password. */
export interface NewUser {
  username: string;
  email: string;
  firstName: string;
  lastName: string;
  password?: string;
}

/** Body for PUT /users/{id}. Every field is optional — only the provided
 *  fields are updated. Username is immutable server-side and not editable. */
export type UserUpdate = Partial<Omit<User, 'id' | 'username'>>;

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

// ── Instance / updates ─────────────────────────────────────────────────────
//
// Backed by the operator-managed Stube CR (stube.io/v1alpha1) via the
// manage-API, which reads/patches the CR with its in-cluster k8s client.
// The release train and applied tag live in the CR spec; the operator's
// Stage-2 reconciler discovers the newest in-channel tag and reports it back
// in status.

/** Release train consulted by the operator's auto-update logic. */
export type Channel = 'stable' | 'edge';

/** Whether the operator may bump the applied version on its own. */
export type UpdateMode = 'manual' | 'auto';

/** Per-Deployment readiness from status.components. */
export interface ComponentStatus {
  /** Deployment name. */
  name: string;
  /** True when all replicas are available. */
  ready: boolean;
  /** Primary container image (with tag) currently applied. */
  image: string;
}

/** Flattened view of the Stube CR spec + status the Updates page renders. */
export interface InstanceView {
  /** spec.channel — the selected release train. */
  channel: Channel;
  /** spec.version — the image tag the operator is asked to apply
   *  ("latest" or a pinned tag). */
  version: string;
  /** status.currentVersion — the tag most recently rolled out. */
  currentVersion: string;
  /** status.availableUpdate — newest in-channel tag the operator discovered.
   *  When it differs from currentVersion an update is available. */
  availableUpdate: string;
  /** spec.update.mode. */
  updateMode: UpdateMode;
  /** status.phase — coarse lifecycle string. */
  phase: string;
  /** status.components — per-Deployment readiness. */
  components: ComponentStatus[];
}

/** Body for PATCH /instance. Every field is optional.
 *  - channel / updateMode patch the CR spec.
 *  - apply: true sets spec.version = status.availableUpdate, triggering the
 *    operator to roll the discovered update. */
export interface InstancePatch {
  channel?: Channel;
  updateMode?: UpdateMode;
  apply?: boolean;
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

  // Attach the OIDC access token to every manage-API call. The whole app is
  // gated behind login (see AuthGate), so a token is normally present; the one
  // exception, GET /setup/status, is unauthenticated server-side and works with
  // or without it. Sending it when present is harmless and keeps this uniform.
  const token = getAccessToken();
  if (token && !headers.has('Authorization')) {
    headers.set('Authorization', `Bearer ${token}`);
  }

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

  // ── Users ──────────────────────────────────────────────────────────────

  listUsers(signal?: AbortSignal): Promise<User[]> {
    return request<User[]>('/users', { signal });
  },

  createUser(body: NewUser, signal?: AbortSignal): Promise<{ id: string }> {
    return request<{ id: string }>('/users', {
      method: 'POST',
      body: JSON.stringify(body),
      signal,
    });
  },

  updateUser(id: string, body: UserUpdate, signal?: AbortSignal): Promise<void> {
    return request<void>(`/users/${encodeURIComponent(id)}`, {
      method: 'PUT',
      body: JSON.stringify(body),
      signal,
    });
  },

  deleteUser(id: string, signal?: AbortSignal): Promise<void> {
    return request<void>(`/users/${encodeURIComponent(id)}`, {
      method: 'DELETE',
      signal,
    });
  },

  resetUserPassword(
    id: string,
    body: { password: string; temporary?: boolean },
    signal?: AbortSignal,
  ): Promise<void> {
    return request<void>(`/users/${encodeURIComponent(id)}/reset-password`, {
      method: 'POST',
      body: JSON.stringify(body),
      signal,
    });
  },

  // ── Instance / updates ───────────────────────────────────────────────────

  /** Read the flattened view of the Stube CR spec + status. */
  getInstance(signal?: AbortSignal): Promise<InstanceView> {
    return request<InstanceView>('/instance', { signal });
  },

  /** Patch the CR spec: set channel / update.mode, or apply:true to roll the
   *  discovered update. Returns the updated view. */
  patchInstance(body: InstancePatch, signal?: AbortSignal): Promise<InstanceView> {
    return request<InstanceView>('/instance', {
      method: 'PATCH',
      body: JSON.stringify(body),
      signal,
    });
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
