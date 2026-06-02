import { useEffect, useState } from 'react';
import { Share, X } from 'lucide-react';

const DISMISS_KEY = 'chino:ios-install-hint-dismissed';

/**
 * iOS-only "Add to Home Screen" hint. Apple never implemented the
 * beforeinstallprompt event, so iOS Safari users have no system
 * prompt to install a PWA — the flow is manual via Share →
 * Add to Home Screen. This banner explains the steps so the user
 * doesn't have to hunt for them.
 *
 * Shown when ALL of:
 *  - UA is iOS Safari (not Chrome iOS, not in-app webview)
 *  - Not already running standalone (window.navigator.standalone)
 *  - User hasn't dismissed it in localStorage
 *
 * The banner is non-blocking (small bottom-sheet style) and remembers
 * dismissal so it doesn't re-nag.
 */
export function InstallHint() {
  const [show, setShow] = useState(false);

  useEffect(() => {
    if (typeof window === 'undefined') return;
    if (window.localStorage.getItem(DISMISS_KEY)) return;
    const ua = navigator.userAgent;
    const isIOS = /iPad|iPhone|iPod/.test(ua) && !(window as { MSStream?: unknown }).MSStream;
    if (!isIOS) return;
    // iOS Safari sets navigator.standalone=true when launched from
    // home screen; skip then.
    const standalone =
      ('standalone' in navigator && (navigator as Navigator & { standalone?: boolean }).standalone === true) ||
      window.matchMedia('(display-mode: standalone)').matches;
    if (standalone) return;
    // Chrome iOS embeds 'CriOS' / 'FxiOS' in UA — they don't support
    // Add to Home Screen, hide for them too.
    if (/CriOS|FxiOS|EdgiOS/.test(ua)) return;
    // Brief defer so the banner doesn't slam in during page load.
    const t = window.setTimeout(() => setShow(true), 1500);
    return () => window.clearTimeout(t);
  }, []);

  if (!show) return null;

  const dismiss = () => {
    window.localStorage.setItem(DISMISS_KEY, '1');
    setShow(false);
  };

  return (
    <div
      // safe-area-inset-bottom + a little extra so it sits above the
      // iOS Safari bottom toolbar that hosts the Share button.
      style={{ bottom: 'calc(env(safe-area-inset-bottom, 0px) + 5rem)' }}
      className="fixed left-3 right-3 z-50 rounded-xl bg-[#161b22] border border-white/15 shadow-2xl backdrop-blur p-4 flex items-start gap-3"
    >
      <div className="flex-1 text-sm">
        <div className="font-medium text-white mb-1">Install Chino</div>
        <div className="text-[#c9d1d9] leading-relaxed">
          Tap the <Share className="inline w-4 h-4 -mt-0.5 mx-0.5 text-[#58a6ff]" /> Share button below,
          then <span className="text-white">Add to Home Screen</span> to launch
          Chino full-screen without the Safari bar.
        </div>
      </div>
      <button
        onClick={dismiss}
        aria-label="Dismiss"
        className="p-1 -m-1 text-[#8b949e] hover:text-white"
      >
        <X className="w-5 h-5" />
      </button>
    </div>
  );
}
