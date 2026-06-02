import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { AuthProvider } from 'react-oidc-context';
import './index.css';
import { App } from './App';
import { oidcConfig } from './auth/oidc';

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <AuthProvider {...oidcConfig}>
      <App />
    </AuthProvider>
  </StrictMode>,
);

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
