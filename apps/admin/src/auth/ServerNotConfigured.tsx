import { RefreshCw, ServerCog } from 'lucide-react';
import { Wordmark } from '../components/Brand';

interface ServerNotConfiguredProps {
  /** True while a /api/config poll is in flight, to show a subtle spinner. */
  checking?: boolean;
}

// Shown when GET /api/config reports an empty oidcIssuer: the bundled identity
// provider / API pod is still coming up (typically only on a fresh appliance,
// for the few seconds before the API is reachable). We never mount the OIDC
// AuthProvider in this state — it would throw on an empty authority. The app
// polls /api/config in the background and auto-advances to the sign-in redirect
// the moment an issuer appears, so the operator never has to refresh.
export function ServerNotConfigured({ checking }: ServerNotConfiguredProps) {
  return (
    <div className="flex min-h-dvh items-center justify-center bg-bg p-s-6">
      <div className="w-full max-w-md text-center">
        <div className="mx-auto mb-s-5 flex h-16 w-16 items-center justify-center rounded-2xl border border-border bg-surface text-cloud-blue">
          <ServerCog size={28} />
        </div>
        <div className="mb-s-4 flex justify-center">
          <Wordmark subtitle="Manage" size={20} />
        </div>
        <h1 className="mb-s-2 font-ui text-2xl font-semibold text-fg">
          Waiting for the server
        </h1>
        <div className="mx-auto mb-s-5 max-w-sm rounded-lg border border-border bg-surface px-s-4 py-s-4 text-left">
          <p className="text-sm text-fg-2">
            The Stube API and its bundled identity provider are starting up.
            Sign-in will appear here automatically once they are reachable — no
            need to refresh.
          </p>
        </div>
        <p className="flex items-center justify-center gap-s-2 text-xs text-fg-dim">
          <RefreshCw size={14} className={checking ? 'animate-spin' : ''} />
          Checking…
        </p>
      </div>
    </div>
  );
}
