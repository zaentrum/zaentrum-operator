import { useEffect, type ReactNode } from 'react';
import { useAuth } from 'react-oidc-context';
import { AlertTriangle, Loader2 } from 'lucide-react';
import { Wordmark } from '../components/Brand';
import { Button } from '../components/ui';

interface AuthGateProps {
  children: ReactNode;
}

/**
 * Gates the entire admin app behind a Keycloak login.
 *
 * Unauthenticated visitors are redirected to the bundled identity provider
 * (Auth Code + PKCE). The bundled `admin` account's password is set by
 * Keycloak's own UPDATE_PASSWORD action on first login — the operator picks it
 * there, then lands back here authenticated. Once signed in, the children (the
 * management chrome + first-run wizard) render.
 */
export function AuthGate({ children }: AuthGateProps) {
  const auth = useAuth();

  // Auto-redirect to the IdP when we're not in flight and not signed in. Skip
  // if there's a pending error — let the user click "Try again" instead of
  // looping back into the failing redirect.
  useEffect(() => {
    if (
      !auth.isLoading &&
      !auth.isAuthenticated &&
      !auth.error &&
      !auth.activeNavigator
    ) {
      void auth.signinRedirect();
    }
  }, [auth.isLoading, auth.isAuthenticated, auth.error, auth.activeNavigator]);

  // When the tab regains focus near token expiry, silently renew so the next
  // /api/manage call carries a fresh token without a "Signing you in…" splash.
  useEffect(() => {
    const onVisible = () => {
      if (document.visibilityState !== 'visible') return;
      if (!auth.isAuthenticated) return;
      const expiresAt = auth.user?.expires_at; // unix seconds
      if (expiresAt && expiresAt * 1000 - Date.now() < 60_000) {
        auth.signinSilent().catch(() => undefined);
      }
    };
    document.addEventListener('visibilitychange', onVisible);
    return () => document.removeEventListener('visibilitychange', onVisible);
  }, [auth]);

  if (auth.error) {
    return (
      <div className="flex min-h-dvh items-center justify-center bg-bg p-s-6">
        <div className="w-full max-w-md text-center">
          <div className="mb-s-4 flex justify-center">
            <Wordmark subtitle="Manage" size={20} />
          </div>
          <h1 className="mb-s-3 font-ui text-2xl font-semibold text-fg">
            Couldn't sign you in
          </h1>
          <div className="mx-auto mb-s-5 flex max-w-sm items-start gap-s-2 rounded-lg border border-[#ff7b72]/30 bg-[#ff7b72]/5 p-s-3 text-left">
            <AlertTriangle size={16} className="mt-px shrink-0 text-[#ff7b72]" />
            <p className="text-sm text-fg-2">
              {auth.error.message || 'The identity provider returned an error.'}
            </p>
          </div>
          <div className="flex justify-center gap-s-2">
            <Button onClick={() => auth.signinRedirect()}>Try again</Button>
            <Button variant="secondary" onClick={() => window.location.assign('/manage/')}>
              Reset
            </Button>
          </div>
          <p className="mt-s-6 text-xs text-fg-dim">
            If this keeps happening, check your network connection or try a fresh
            browser tab — redirect cookies can go stale.
          </p>
        </div>
      </div>
    );
  }

  if (auth.isLoading || !auth.isAuthenticated) {
    return (
      <div className="flex min-h-dvh flex-col items-center justify-center gap-s-4 bg-bg p-s-6 text-fg-muted">
        <Wordmark subtitle="Manage" size={20} />
        <span className="flex items-center gap-s-2 text-sm">
          <Loader2 size={18} className="animate-spin text-cloud-blue" />
          Signing you in…
        </span>
      </div>
    );
  }

  return <>{children}</>;
}
