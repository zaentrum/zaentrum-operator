/// <reference types="vite/client" />

interface ImportMetaEnv {
  /** Base URL of the manage-API. Defaults to "/api/manage" (same origin). */
  readonly VITE_MANAGE_API_BASE?: string;

  // ── OIDC build-time pins (optional) ──────────────────────────────────────
  // The admin SPA normally learns its identity provider at runtime from
  // GET /api/config (see src/auth/runtimeConfig.ts). These let a single-tenant
  // deployment hard-pin a realm at `vite build` time; when set they win over
  // the server-reported values. Empty / unset means "defer to the server".

  /** OIDC issuer / authority (discovery base URL). */
  readonly VITE_OIDC_AUTHORITY?: string;
  /** Public OIDC client id to authenticate as (default: the server's web client). */
  readonly VITE_OIDC_CLIENT_ID?: string;
  /** Override the redirect URI (default: <origin>/manage/callback). */
  readonly VITE_OIDC_REDIRECT_URI?: string;
  /** Override the post-logout redirect URI (default: <origin>/manage/). */
  readonly VITE_OIDC_POST_LOGOUT_REDIRECT_URI?: string;
}

interface ImportMeta {
  readonly env: ImportMetaEnv;
}
