import { useEffect, useRef, useState, type ReactNode } from 'react';
import { AuthProvider } from 'react-oidc-context';
import {
  fetchRuntimeConfig,
  hasBuildTimeIssuer,
  type ResolvedConfig,
} from './runtimeConfig';
import { ServerNotConfigured } from './ServerNotConfigured';

interface RuntimeAuthProviderProps {
  /** Config resolved during the pre-render fetch in main.tsx. */
  initial: ResolvedConfig;
  children: ReactNode;
}

// How often to re-check /api/config while the server reports "not configured".
// A few seconds keeps the auto-advance feeling instant without hammering the
// API during what is normally a once-per-appliance startup window.
const POLL_MS = 4000;

/**
 * Decides whether to mount the OIDC AuthProvider or the "server starting up"
 * screen, based on the runtime config fetched from GET /api/config.
 *
 * The first resolution happens before render (in main.tsx) so there's no flash.
 * If the server hasn't reported an OIDC issuer yet (the bundled identity
 * provider / API pod is still coming up on a fresh appliance), this component
 * polls /api/config and re-renders into the authenticated tree the moment an
 * oidcIssuer appears — so the operator never has to manually refresh.
 *
 * Once the AuthProvider is mounted, AuthGate immediately kicks off the
 * sign-in redirect to the bundled Keycloak. The bundled `admin` account's
 * password is set by Keycloak's own UPDATE_PASSWORD flow on first login — not
 * by this app — which is why login happens BEFORE the first-run wizard.
 */
export function RuntimeAuthProvider({ initial, children }: RuntimeAuthProviderProps) {
  const [config, setConfig] = useState<ResolvedConfig>(initial);
  const [checking, setChecking] = useState(false);
  const stopped = useRef(false);

  useEffect(() => {
    // Once configured, nothing to poll. With a build-time issuer pin we're
    // always configured, so this is a no-op there too.
    if (config.configured || hasBuildTimeIssuer) return;

    stopped.current = false;
    let timer: ReturnType<typeof setTimeout>;

    const tick = async () => {
      if (stopped.current) return;
      setChecking(true);
      let next: ResolvedConfig | null = null;
      try {
        next = await fetchRuntimeConfig();
      } catch {
        next = null;
      }
      if (stopped.current) return;
      setChecking(false);
      if (next && next.configured) {
        // Auto-advance: flipping state remounts into the AuthProvider, which
        // (via AuthGate) immediately kicks off the sign-in redirect.
        setConfig(next);
        return;
      }
      timer = setTimeout(tick, POLL_MS);
    };

    timer = setTimeout(tick, POLL_MS);
    return () => {
      stopped.current = true;
      clearTimeout(timer);
    };
  }, [config.configured]);

  if (!config.configured) {
    return <ServerNotConfigured checking={checking} />;
  }

  return <AuthProvider {...config.oidc}>{children}</AuthProvider>;
}
