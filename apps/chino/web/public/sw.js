/* Chino service worker — minimal PWA shell + artwork cache.
 *
 * Design:
 *   - The Vite build emits content-hashed bundles; we don't try to
 *     manually pre-cache them. Instead we use a runtime "stale-while-
 *     revalidate" cache for navigations + a long-lived cache for
 *     artwork (posters/backdrops) which are immutable per item id.
 *   - API calls are NOT cached. Auth tokens expire and a stale
 *     /me/continue-watching would mislead the user. Pass through.
 *   - Streaming endpoints (/play, /play/info, /play/subtitles) are
 *     also pass-through — large bodies + range requests we don't
 *     want in the cache.
 *
 * Versioning: bumping CACHE_VERSION invalidates old caches on next
 * activate. Required when this file's shape changes meaningfully.
 */
const CACHE_VERSION = 'chino-v3';
const SHELL_CACHE = `${CACHE_VERSION}-shell`;
const ART_CACHE = `${CACHE_VERSION}-artwork`;

self.addEventListener('install', (event) => {
  event.waitUntil(caches.open(SHELL_CACHE));
});

// When the client posts {type:'SKIP_WAITING'}, leave the 'waiting'
// state immediately and become the active SW. The UpdateAvailable
// toast in chino-web triggers this via reg.waiting.postMessage().
self.addEventListener('message', (event) => {
  if (event.data && event.data.type === 'SKIP_WAITING') {
    self.skipWaiting();
  }
});

self.addEventListener('activate', (event) => {
  // Drop caches from previous versions on activation.
  event.waitUntil(
    caches.keys().then((keys) =>
      Promise.all(
        keys
          .filter((k) => !k.startsWith(CACHE_VERSION))
          .map((k) => caches.delete(k))
      ).then(() => self.clients.claim())
    )
  );
});

self.addEventListener('fetch', (event) => {
  const req = event.request;
  if (req.method !== 'GET') return;
  const url = new URL(req.url);

  // Streaming + telemetry + auth — always network, no caching.
  if (url.pathname.startsWith('/api/v1/items/') &&
      (url.pathname.endsWith('/play') ||
       url.pathname.includes('/play/') ||
       url.pathname.includes('/subtitles'))) {
    return; // default: pass through
  }
  if (url.pathname.startsWith('/api/v1/play/events') ||
      url.pathname.startsWith('/api/v1/me/') ||
      url.pathname.startsWith('/api/v1/play/events')) {
    return;
  }

  // Artwork — cache-first, long-lived. Item posters/backdrops are
  // tied to immutable item ids; on update the id changes.
  if (url.pathname.startsWith('/api/v1/items/') &&
      (url.pathname.includes('/poster') || url.pathname.includes('/backdrop'))) {
    event.respondWith(cacheFirst(req, ART_CACHE));
    return;
  }

  // Navigations + assets — stale-while-revalidate so the shell
  // loads instantly on a flaky connection but updates in the
  // background when the network responds.
  if (req.destination === 'document' ||
      req.destination === 'script' ||
      req.destination === 'style' ||
      req.destination === 'font') {
    event.respondWith(staleWhileRevalidate(req, SHELL_CACHE));
    return;
  }
});

async function cacheFirst(req, cacheName) {
  const cache = await caches.open(cacheName);
  const cached = await cache.match(req);
  if (cached) return cached;
  try {
    const res = await fetch(req);
    if (res.ok) cache.put(req, res.clone());
    return res;
  } catch (e) {
    return cached || new Response('', { status: 504 });
  }
}

async function staleWhileRevalidate(req, cacheName) {
  const cache = await caches.open(cacheName);
  const cached = await cache.match(req);
  const fetchPromise = fetch(req).then((res) => {
    if (res.ok) cache.put(req, res.clone());
    return res;
  }).catch(() => cached);
  return cached || fetchPromise;
}
