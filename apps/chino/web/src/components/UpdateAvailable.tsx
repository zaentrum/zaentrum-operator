import { useEffect, useState } from 'react';
import { RefreshCw, X } from 'lucide-react';

/**
 * Service-worker update notifier. Listens for a registered SW becoming
 * `installed` after the page already had a controller — that's the
 * classic "new version is sitting there, waiting" signal. Pops a
 * dismissible toast with a Reload button that activates the waiting
 * SW (skipWaiting) and reloads the page once the new SW takes over.
 *
 * No banner when the SW is the first one ever (fresh install) — only
 * when an update is genuinely available.
 */
export function UpdateAvailable() {
  const [show, setShow] = useState(false);

  useEffect(() => {
    if (!('serviceWorker' in navigator)) return;
    let cancelled = false;

    const wire = (reg: ServiceWorkerRegistration) => {
      // 1) A new SW was already waiting when this tab opened.
      if (reg.waiting && navigator.serviceWorker.controller) {
        setShow(true);
      }
      // 2) A new SW shows up while this tab is alive.
      reg.addEventListener('updatefound', () => {
        const sw = reg.installing;
        if (!sw) return;
        sw.addEventListener('statechange', () => {
          if (sw.state === 'installed' && navigator.serviceWorker.controller && !cancelled) {
            setShow(true);
          }
        });
      });
    };

    navigator.serviceWorker.getRegistration().then((reg) => {
      if (reg) wire(reg);
    });
    // When the new SW activates and starts controlling this client,
    // reload so the user runs the new bundle immediately.
    let reloaded = false;
    navigator.serviceWorker.addEventListener('controllerchange', () => {
      if (reloaded) return;
      reloaded = true;
      window.location.reload();
    });
    return () => {
      cancelled = true;
    };
  }, []);

  if (!show) return null;

  const apply = async () => {
    const reg = await navigator.serviceWorker.getRegistration();
    if (!reg) return;
    if (reg.waiting) reg.waiting.postMessage({ type: 'SKIP_WAITING' });
    // controllerchange (above) will fire next → page reloads.
  };

  return (
    <div
      style={{ bottom: 'calc(env(safe-area-inset-bottom, 0px) + 1rem)' }}
      className="fixed left-3 right-3 md:left-auto md:right-6 md:max-w-sm z-50 rounded-xl bg-[#161b22] border border-white/15 shadow-2xl backdrop-blur p-4 flex items-start gap-3"
    >
      <RefreshCw className="w-5 h-5 mt-0.5 text-[#58a6ff] shrink-0" />
      <div className="flex-1 text-sm">
        <div className="font-medium text-white">Update available</div>
        <div className="text-[#c9d1d9]">Reload to get the latest Chino.</div>
      </div>
      <button
        onClick={apply}
        className="px-3 py-1.5 rounded-full bg-[#58a6ff] hover:bg-[#58a6ff]/80 text-white text-sm font-medium"
      >
        Reload
      </button>
      <button
        onClick={() => setShow(false)}
        aria-label="Dismiss"
        className="p-1 text-[#8b949e] hover:text-white"
      >
        <X className="w-4 h-4" />
      </button>
    </div>
  );
}
