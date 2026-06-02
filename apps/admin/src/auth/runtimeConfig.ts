import type { AuthProviderProps } from 'react-oidc-context';
import { WebStorageStateStore } from 'oidc-client-ts';

// Runtime self-configuration for the admin SPA.
//
// The admin UI authenticates against the SAME bundled identity provider the
// product web client (chino-web) uses, reusing the public `chino-web` OIDC
// client (Auth Code + PKCE, wildcard redirect URIs / web origins). The issuer
// is not known at build time on a fresh appliance, so — exactly like
// chino-web — the app asks the server who its IdP is at boot via the
// unauthenticated, CORS-open discovery document GET /api/config served by
// chino-api.
//
// WHY /api/config and not the manage-API: the manage-API's own config endpoint
// (GET /api/manage/config) is behind the OIDC verifier, which is a
// chicken-and-egg problem — we cannot read the issuer there without already
// holding a token. chino-api's /api/config is the only unauthenticated,
// same-origin source of the issuer + the public web client id, and it is the
// same document chino-web and the neutral self-host clients consume. The admin
// SPA is served at /manage behind the platform ingress where /api routes to the
// backend on the same origin, so the relative URL resolves without a base URL.

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
   * Per-platform PUBLIC client ids. We pick `.web` (the `chino-web` client),
   * which the admin SPA reuses. Older/edge servers may still send a bare
   * string, so accept both shapes defensively.
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
 *
 * Redirect / post-logout URIs stay derived from window.location.origin so the
 * same artifact works on any hostname, with optional build-time overrides. The
 * admin SPA lives under /manage (router basename + nginx location), so the
 * callback lands at <origin>/manage/callback — the `chino-web` client accepts
 * it via its wildcard redirect URIs.
 */
function buildOidcConfig(authority: string, clientId: string): AuthProviderProps {
  const redirectUri =
    (env.VITE_OIDC_REDIRECT_URI ?? '').trim() ||
    `${window.location.origin}/manage/callback`;
  const postLogoutRedirectUri =
    (env.VITE_OIDC_POST_LOGOUT_REDIRECT_URI ?? '').trim() ||
    `${window.location.origin}/manage/`;

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
      // Strip the /manage/callback path + the code/state query so the URL is
      // clean once the redirect completes, landing back at the SPA root. The
      // router basename is /manage, so we normalise to /manage/.
      window.history.replaceState(
        null,
        '',
        window.location.pathname.replace(/\/manage\/callback\/?$/, '/manage/') ||
          '/manage/',
      );
    },
  };
}

/**
 * Fetch GET /api/config once and resolve into something the app can act on.
 *
 * Same-origin relative URL: the admin app is served from the same host as the
 * API (nginx SPA at /manage + ingress /api route), so no base URL is needed. On
 * any network/parse error we report "not configured" rather than throwing — the
 * caller renders the setup screen and keeps polling, which also covers the
 * brief window where the API pod isn't up yet on a fresh appliance.
 */
export async function fetchRuntimeConfig(): Promise<ResolvedConfig> {
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
  return {
    configured: true,
    oidc,
    server:
      server ?? ({ oidcIssuer: authority, oidcEnabled: true } as ServerConfig),
  };
}

/** Whether a build-time issuer pin exists (lets us skip polling if so). */
export const hasBuildTimeIssuer = ENV_AUTHORITY.length > 0;

/** The resolved OIDC issuer + public client id, independent of the
 *  react-oidc-context config union shape. Used by the first-run wizard to echo
 *  the bundled identity provider's issuer + client id back to POST /setup
 *  (which the server requires). Either field may be empty if the server hasn't
 *  reported an issuer yet. */
export interface ResolvedOidc {
  issuer: string;
  clientId: string;
}

/** Fetch /api/config and resolve just the issuer + client id (build-time pins
 *  win). Convenience for callers that need the raw values rather than a mounted
 *  AuthProvider config. */
export async function fetchResolvedOidc(): Promise<ResolvedOidc> {
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
    // ignore — empty issuer signals "not known yet".
  }
  return { issuer: issuerFor(server), clientId: clientIdFor(server) };
}
