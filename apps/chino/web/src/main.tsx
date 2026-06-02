import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import './index.css';
import { App } from './App';
import { RuntimeAuthProvider } from './auth/RuntimeAuthProvider';
import { fetchRuntimeConfig } from './auth/runtimeConfig';

// Bootstrap: learn our identity provider from the server BEFORE mounting the
// OIDC AuthProvider. On a fresh appliance the issuer is unknown at build time
// — GET /api/config tells us the issuer + the public web client id once the
// operator has finished /manage/setup. Until then the resolved config is
// "not configured" and RuntimeAuthProvider shows the setup screen + polls.
//
// react-oidc-context's AuthProvider would throw on an empty authority, so we
// must resolve the config first rather than rendering optimistically.
async function bootstrap() {
  const root = createRoot(document.getElementById('root')!);
  const initial = await fetchRuntimeConfig();
  root.render(
    <StrictMode>
      <RuntimeAuthProvider initial={initial}>
        <App />
      </RuntimeAuthProvider>
    </StrictMode>,
  );
}

void bootstrap();

// Register the PWA service worker — enables "Add to Home Screen" on
// iOS/Android with standalone chrome, caches the app shell + artwork
// for fast cold starts on flaky connections, and is the criterion
// Chrome needs to surface the Install prompt. Failures are
// non-fatal; the app works fine without the SW.
if ('serviceWorker' in navigator) {
  window.addEventListener('load', () => {
    navigator.serviceWorker.register('/sw.js').catch(() => undefined);
  });
}
