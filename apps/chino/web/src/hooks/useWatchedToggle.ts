import { useCallback } from 'react';
import { useAuth } from 'react-oidc-context';

/**
 * Returns a single `toggle(itemId, watched)` mutator that flips the
 * server-side watched_history row for the current user. Unlike
 * useWatchlist/useLikes, this hook does NOT keep a flat id set —
 * per-item watched state already arrives stamped on the catalogue
 * payload (`watched_at`), so consumers seed local state from that and
 * just call this hook to push the change.
 *
 * Broadcasts a `chino:flag-changed` event with kind="watched" so
 * other surfaces (Continue Watching, Recently added) can refetch if
 * they care. Network errors are swallowed — the optimistic UI flip
 * stays; a stale view will resolve on the next page load.
 */
export function useWatchedToggle() {
  const auth = useAuth();
  return useCallback(
    async (itemId: string, watched: boolean) => {
      const token = auth.user?.access_token;
      if (!token || !itemId) return;
      try {
        await fetch(`/api/v1/me/items/${encodeURIComponent(itemId)}/watched`, {
          method: watched ? 'POST' : 'DELETE',
          headers: { Authorization: `Bearer ${token}` },
        });
      } catch {
        // Swallow — caller handles UI optimism.
      }
      window.dispatchEvent(new CustomEvent('chino:flag-changed', { detail: { kind: 'watched' } }));
    },
    [auth.user?.access_token],
  );
}
