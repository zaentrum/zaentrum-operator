import { useEffect, type ReactNode } from 'react';
import { useAuth } from 'react-oidc-context';
import { AlertTriangle } from 'lucide-react';
import { LoadingState } from '../components/LoadingState';
import chinoIcon from '../imports/chino_icon.svg';

interface AuthGateProps {
  children: ReactNode;
}

export function AuthGate({ children }: AuthGateProps) {
  const auth = useAuth();

  // Auto-redirect to the IdP when we're not in flight and not signed
  // in. Same guard as before, plus we skip if there's a pending error
  // — let the user click "Try again" instead of looping back to the
  // failing redirect.
  useEffect(() => {
    if (!auth.isLoading && !auth.isAuthenticated && !auth.error && !auth.activeNavigator) {
      auth.signinRedirect();
    }
  }, [auth.isLoading, auth.isAuthenticated, auth.error, auth.activeNavigator]);

  // When the tab comes back into focus after being backgrounded, the
  // access token may have expired silently. Trigger a silent renew so
  // the next fetch carries a fresh token without showing the user a
  // "Signing you in…" splash. react-oidc-context wires its own
  // automatic renew before expiry, but visibility-change is the
  // belt-and-suspenders case where the user opens the tab a minute
  // before expiry and starts clicking immediately.
  useEffect(() => {
    const onVisible = () => {
      if (document.visibilityState !== 'visible') return;
      if (!auth.isAuthenticated) return;
      // expires_at is unix seconds; treat <60s as "renew now".
      const expiresAt = auth.user?.expires_at;
      if (expiresAt && expiresAt * 1000 - Date.now() < 60_000) {
        auth.signinSilent().catch(() => undefined);
      }
    };
    document.addEventListener('visibilitychange', onVisible);
    return () => document.removeEventListener('visibilitychange', onVisible);
  }, [auth]);

  if (auth.error) {
    return (
      <div className="min-h-dvh bg-[#0d1117] text-[#c9d1d9] flex items-center justify-center p-6">
        <div className="max-w-md w-full text-center">
          <div className="w-16 h-16 mx-auto mb-4 rounded-2xl bg-[#161b22] border border-[#30363d] flex items-center justify-center">
            <img src={chinoIcon} alt="Chino" className="w-10 h-10" />
          </div>
          <h1 className="text-2xl font-semibold mb-2 text-white">Couldn't sign you in</h1>
          <div className="flex items-start gap-2 mx-auto max-w-sm text-left bg-[#161b22] border border-rose-500/30 rounded-lg p-3 mb-5">
            <AlertTriangle className="w-4 h-4 text-rose-300 mt-0.5 shrink-0" />
            <p className="text-sm text-[#c9d1d9]">{auth.error.message || 'The identity provider returned an error.'}</p>
          </div>
          <div className="flex gap-2 justify-center">
            <button
              onClick={() => auth.signinRedirect()}
              className="px-5 py-2 bg-[#58a6ff] hover:bg-[#58a6ff]/80 text-white rounded-lg font-medium"
            >
              Try again
            </button>
            <button
              onClick={() => window.location.assign('/')}
              className="px-5 py-2 bg-white/10 hover:bg-white/20 text-white rounded-lg"
            >
              Reset
            </button>
          </div>
          <p className="mt-6 text-xs text-[#8b949e]">
            If this keeps happening, check your network connection or try a fresh browser
            tab — sometimes the redirect cookies get stale.
          </p>
        </div>
      </div>
    );
  }

  if (auth.isLoading || !auth.isAuthenticated) {
    return (
      <div className="min-h-dvh bg-[#0d1117] text-[#8b949e] flex flex-col items-center justify-center p-6">
        <div className="w-16 h-16 mb-4 rounded-2xl bg-[#161b22] border border-[#30363d] flex items-center justify-center">
          <img src={chinoIcon} alt="Chino" className="w-10 h-10" />
        </div>
        <LoadingState message="Signing you in…" />
      </div>
    );
  }

  return <>{children}</>;
}
