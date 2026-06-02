// OIDC configuration is no longer baked in at build time. On a fresh
// appliance the issuer is unknown until the operator finishes /manage/setup,
// so the web app learns it at runtime from GET /api/config. See
// runtimeConfig.ts for the fetch + config-building logic and
// RuntimeAuthProvider.tsx for the mount/poll behaviour.
//
// This module is kept as the public entry point for auth configuration so
// importers have a stable path; it simply re-exports the runtime pieces.
export {
  fetchRuntimeConfig,
  hasBuildTimeIssuer,
  type ServerConfig,
  type ResolvedConfig,
} from './runtimeConfig';
