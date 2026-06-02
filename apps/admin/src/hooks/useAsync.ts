import { useCallback, useEffect, useRef, useState } from 'react';
import { ApiError } from '../lib/api';

interface AsyncState<T> {
  data: T | null;
  loading: boolean;
  error: string | null;
}

/**
 * Run an async loader on mount (and whenever `deps` change), exposing
 * loading/error/data plus a `reload()` for retry buttons. The loader receives
 * an AbortSignal so in-flight requests cancel on unmount / dep change.
 */
export function useAsync<T>(
  loader: (signal: AbortSignal) => Promise<T>,
  deps: unknown[] = [],
): AsyncState<T> & { reload: () => void } {
  const [state, setState] = useState<AsyncState<T>>({
    data: null,
    loading: true,
    error: null,
  });
  const [tick, setTick] = useState(0);
  const loaderRef = useRef(loader);
  loaderRef.current = loader;

  const reload = useCallback(() => setTick((t) => t + 1), []);

  useEffect(() => {
    const ctrl = new AbortController();
    setState((s) => ({ ...s, loading: true, error: null }));
    loaderRef.current(ctrl.signal)
      .then((data) => {
        if (!ctrl.signal.aborted) setState({ data, loading: false, error: null });
      })
      .catch((e: unknown) => {
        if (ctrl.signal.aborted) return;
        const msg =
          e instanceof ApiError
            ? e.status === 0
              ? 'Cannot reach the manage-API. Is the server running?'
              : e.message
            : (e as Error).message || 'Unexpected error';
        setState({ data: null, loading: false, error: msg });
      });
    return () => ctrl.abort();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [...deps, tick]);

  return { ...state, reload };
}
