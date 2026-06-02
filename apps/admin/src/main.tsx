import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { BrowserRouter } from 'react-router-dom';
import './styles/global.css';
import { App } from './App';
import { RuntimeAuthProvider } from './auth/RuntimeAuthProvider';
import { AuthGate } from './auth/AuthGate';
import { AuthTokenBridge } from './auth/AuthTokenBridge';
import { fetchRuntimeConfig } from './auth/runtimeConfig';

// The admin UI is served under /manage (nginx location + Vite base). The
// router basename keeps client-side paths in sync with that mount point.
//
// Bootstrap: learn the bundled identity provider from the server BEFORE
// mounting the OIDC AuthProvider. On a fresh appliance the issuer is unknown at
// build time — GET /api/config (the same unauthenticated discovery document
// chino-web uses) tells us the issuer + the public web client id to reuse.
// Until that's reachable the resolved config is "not configured" and
// RuntimeAuthProvider shows a brief "waiting for the server" screen + polls.
// react-oidc-context's AuthProvider throws on an empty authority, so we resolve
// the config first rather than rendering optimistically.
//
// AuthGate then gates the whole app behind a Keycloak login (Auth Code + PKCE,
// redirect_uri <origin>/manage/callback). The router's basename "/manage" turns
// that into the in-app "/callback" path; the AuthProvider auto-detects the code
// + state on the callback URL and completes the exchange. AuthTokenBridge feeds
// the access token to the manage-API client so /api/manage/* calls are
// authorized.
async function bootstrap() {
  const root = createRoot(document.getElementById('root')!);
  const initial = await fetchRuntimeConfig();
  root.render(
    <StrictMode>
      <BrowserRouter basename="/manage">
        <RuntimeAuthProvider initial={initial}>
          <AuthTokenBridge />
          <AuthGate>
            <App />
          </AuthGate>
        </RuntimeAuthProvider>
      </BrowserRouter>
    </StrictMode>,
  );
}

void bootstrap();
