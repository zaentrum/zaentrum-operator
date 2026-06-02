import { useEffect, useState } from 'react';
import { AuthGate } from './auth/AuthGate';
import { ChinoApp } from './components/chino/ChinoApp';
import { PlayerPage } from './components/PlayerPage';
import { DetailPage } from './components/DetailPage';
import { ProfilePage } from './components/ProfilePage';
import { InstallHint } from './components/InstallHint';
import { UpdateAvailable } from './components/UpdateAvailable';

// Tiny pathname-based router. Three top-level routes plus the SPA root:
//   /player/:id    full-screen video player
//   /i/:id         movie / show detail page
//   /search?q=…    search results
//   *              the ChinoApp shell (home / movies / shows / etc.)
function pickRoute(path: string, search: string) {
  const playerM = path.match(/^\/player\/([^/]+)\/?$/);
  if (playerM) return <PlayerPage itemId={decodeURIComponent(playerM[1])} />;

  const detailM = path.match(/^\/i\/([^/]+)\/?$/);
  if (detailM) return <DetailPage itemId={decodeURIComponent(detailM[1])} />;

  if (path === '/me' || path === '/me/') return <ProfilePage />;

  if (path === '/search') {
    // Search lives INSIDE the ChinoApp shell so the sidebar, header
    // and the search input itself remain visible while the user is
    // looking at results. ChinoApp picks 'search' as the active
    // section when initialSearchQuery is non-undefined.
    const q = new URLSearchParams(search).get('q') ?? '';
    return <ChinoApp initialSearchQuery={q} />;
  }

  return <ChinoApp />;
}

export function App() {
  // Track pathname + search in state so a browser Back / Forward
  // navigation re-renders the right route (#151). Previously pickRoute
  // ran once at mount; in-page navigations (`window.location.assign`)
  // trigger a full reload so they were fine, but Back/Forward fires
  // `popstate` without a reload, which left the wrong page mounted —
  // the user would click back from /i/X and the player stayed on
  // screen until they hard-reloaded.
  const [route, setRoute] = useState(() => ({
    path: window.location.pathname,
    search: window.location.search,
  }));
  useEffect(() => {
    const onPop = () => setRoute({
      path: window.location.pathname,
      search: window.location.search,
    });
    window.addEventListener('popstate', onPop);
    return () => window.removeEventListener('popstate', onPop);
  }, []);

  // The InstallHint is iOS-only and self-suppresses on the player
  // page (we don't want a banner over the video) by checking the
  // pathname inside the component? Simpler: skip it for /player/*.
  const onPlayer = route.path.startsWith('/player/');
  // React's key forces a full remount when crossing route boundaries.
  // Without it, the PlayerPage's hls.js + <video> would persist across
  // a Back navigation to /i/X (DetailPage doesn't know to tear them
  // down), holding the stream connection open and confusing the
  // audio focus on mobile.
  const routeKey = `${route.path}${route.search}`;
  return (
    <AuthGate>
      {/* h-full so ChinoApp's `size-full` has a sized parent — without
          it the sidebar collapses to content height and the bottom-
          anchored settings cell rides up next to the nav. */}
      <div key={routeKey} className="h-full">{pickRoute(route.path, route.search)}</div>
      {!onPlayer ? <InstallHint /> : null}
      <UpdateAvailable />
    </AuthGate>
  );
}
