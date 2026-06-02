import { Settings, RefreshCw } from 'lucide-react';
import chinoIcon from '../imports/chino_icon.svg';

interface ServerNotConfiguredProps {
  /** True while a /api/config poll is in flight, to show a subtle spinner. */
  checking?: boolean;
}

// Shown on a fresh appliance when GET /api/config reports an empty oidcIssuer:
// the server is up but the operator hasn't finished /manage/setup, so there is
// no identity provider to sign in against yet. We never mount the OIDC
// AuthProvider in this state (it would throw on an empty authority). The app
// polls /api/config in the background and auto-advances to the login redirect
// the moment setup completes — no manual refresh needed.
export function ServerNotConfigured({ checking }: ServerNotConfiguredProps) {
  return (
    <div className="min-h-dvh bg-[#0d1117] text-[#c9d1d9] flex items-center justify-center p-6">
      <div className="max-w-md w-full text-center">
        <div className="w-16 h-16 mx-auto mb-4 rounded-2xl bg-[#161b22] border border-[#30363d] flex items-center justify-center">
          <img src={chinoIcon} alt="Chino" className="w-10 h-10" />
        </div>
        <h1 className="text-2xl font-semibold mb-2 text-white">Server not configured</h1>
        <div className="mx-auto max-w-sm text-left bg-[#161b22] border border-[#30363d] rounded-lg p-4 mb-5">
          <div className="flex items-start gap-2">
            <Settings className="w-4 h-4 text-[#58a6ff] mt-0.5 shrink-0" />
            <p className="text-sm text-[#c9d1d9]">
              This server hasn't been set up yet. Finish the one-time setup to
              connect your identity provider, then sign-in will appear here
              automatically.
            </p>
          </div>
        </div>
        <a
          href="/manage/setup"
          className="inline-block px-5 py-2 bg-[#58a6ff] hover:bg-[#58a6ff]/80 text-white rounded-lg font-medium"
        >
          Finish setup
        </a>
        <p className="mt-6 flex items-center justify-center gap-2 text-xs text-[#8b949e]">
          <RefreshCw className={`w-3.5 h-3.5 ${checking ? 'animate-spin' : ''}`} />
          Waiting for setup to complete…
        </p>
      </div>
    </div>
  );
}
