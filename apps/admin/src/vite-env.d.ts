/// <reference types="vite/client" />

interface ImportMetaEnv {
  /** Base URL of the manage-API. Defaults to "/api/manage" (same origin). */
  readonly VITE_MANAGE_API_BASE?: string;
}

interface ImportMeta {
  readonly env: ImportMetaEnv;
}
