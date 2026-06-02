import type { AuthProviderProps } from 'react-oidc-context';
import { WebStorageStateStore } from 'oidc-client-ts';

// Runtime self-configuration.
//
// On a fresh appliance the operator has not yet run /manage/setup, so the
// OIDC issuer is unknown at build time. Instead of baking it in, the web app
// asks the server who its identity provider is at boot via GET /api/config,
// the same unauthenticated discovery document the neutral self-host clients
// use. The server reports an empty `oidcIssuer` until setup completes; we
// treat that as "not configured yet" and keep polling.

const env = import.meta.env;

// Build-time overrides. Present only if someone set them at `vite build`
// time (e.g. a pinned single-tenant deployment). When set they win over the
// server-reported values so an operator can hard-pin a realm. Empty string /
// undefined means "defer to the server".
const ENV_AUTHORITY = (env.VITE_OIDC_AUTHORITY ?? '').trim();
const ENV_CLIENT_ID = (env.VITE_OIDC_CLIENT_ID ?? '').trim();

/** Shape of the relevant fields from GET /api/config (chino-api appconfig.go). */
export interface ServerConfig {
  /** External "<origin>/api" this server is reached at. */
  apiBase?: string;
  /** OIDC issuer URL; empty until the operator finishes /manage/setup. */
  oidcIssuer: string;
  /** Resource/audience the API validates tokens against. */
  oidcAudience?: string;
  /** Whether the server enforces OIDC at all. */
  oidcEnabled: boolean;
  /**
   * Per-platform PUBLIC client ids. We pick `.web`. Older/edge servers may
   * still send a bare string, so accept both shapes defensively.
   */
  oidcClientId?: { tv?: string; mobile?: string; web?: string } | string;
}

/** Result of resolving the runtime config: either ready to mount auth, or not. */
export type ResolvedConfig =
  | { configured: true; oidc: AuthProviderProps; server: ServerConfig }
  | { configured: false; server: ServerConfig | null };

function clientIdFor(server: ServerConfig | null): string {
  // Build-time pin wins.
  if (ENV_CLIENT_ID) return ENV_CLIENT_ID;
  const c = server?.oidcClientId;
  if (typeof c === 'string' && c.trim()) return c.trim();
  if (c && typeof c === 'object' && c.web && c.web.trim()) return c.web.trim();
  // Documented convention default (matches chino-api OIDC_CLIENT_ID_WEB).
  return 'chino-web';
}

function issuerFor(server: ServerConfig | null): string {
  // Build-time pin wins; otherwise the server's reported issuer.
  if (ENV_AUTHORITY) return ENV_AUTHORITY;
  return (server?.oidcIssuer ?? '').trim();
}

/**
 * Build the react-oidc-context config from the resolved issuer + client id.
 * Redirect / post-logout URIs stay derived from window.location.origin so the
 * same artifact works on any hostname, with optional build-time overrides.
 */
function buildOidcConfig(authority: string, clientId: string): AuthProviderProps {
  const redirectUri =
    (env.VITE_OIDC_REDIRECT_URI ?? '').trim() ||
    `${window.location.origin}/auth/callback`;
  const postLogoutRedirectUri =
    (env.VITE_OIDC_POST_LOGOUT_REDIRECT_URI ?? '').trim() ||
    window.location.origin;

  return {
    authority,
    client_id: clientId,
    redirect_uri: redirectUri,
    post_logout_redirect_uri: postLogoutRedirectUri,
    response_type: 'code',
    scope: 'openid profile email',
    automaticSilentRenew: true,
    userStore: new WebStorageStateStore({ store: window.localStorage }),
    onSigninCallback: () => {
      window.history.replaceState(
        null,
        '',
        window.location.pathname.replace(/\/auth\/callback$/, '/') || '/',
      );
    },
  };
}

/**
 * Fetch GET /api/config once and resolve into something the app can act on.
 *
 * Same-origin relative URL: the web app is served from the same host as the
 * API (nginx SPA + Ingress /api route), so no base URL is needed. On any
 * network/parse error we report "not configured" rather than throwing — the
 * caller renders the setup screen and keeps polling, which also covers the
 * brief window where the API pod isn't up yet on a fresh appliance.
 */
export async function fetchRuntimeConfig(): Promise<ResolvedConfig> {
  // A build-time issuer pin makes us self-sufficient even if /api/config is
  // momentarily unreachable; still try to read the rest of the doc.
  let server: ServerConfig | null = null;
  try {
    const res = await fetch('/api/config', {
      headers: { Accept: 'application/json' },
      cache: 'no-store',
    });
    if (res.ok) {
      server = (await res.json()) as ServerConfig;
    }
  } catch {
    // ignore — treated as not-configured below unless we have an env pin.
  }

  const authority = issuerFor(server);
  if (!authority) {
    return { configured: false, server };
  }
  const oidc = buildOidcConfig(authority, clientIdFor(server));
  return { configured: true, oidc, server: server ?? ({ oidcIssuer: authority, oidcEnabled: true } as ServerConfig) };
}

/** Whether a build-time issuer pin exists (lets us skip polling if so). */
export const hasBuildTimeIssuer = ENV_AUTHORITY.length > 0;
