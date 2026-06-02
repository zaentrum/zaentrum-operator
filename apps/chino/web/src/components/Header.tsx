import { Search, Bell, LogOut, UserCircle } from 'lucide-react';
import { useEffect, useMemo, useRef, useState } from 'react';
import { useAuth } from 'react-oidc-context';
import { useStreamToken } from '../hooks/useStreamToken';
import { FadeImage } from './FadeImage';
import { Avatar } from './Avatar';

// Per-suggestion shape from /api/v1/items?q=…. Picks the fields we
// actually render so we don't carry the whole catalogue payload around.
interface Suggestion {
  id: string;
  title: string;
  year?: number;
  type: 'movie' | 'series' | 'music' | string;
  poster_url?: string;
}

export function Header() {
  const auth = useAuth();
  const streamToken = useStreamToken();
  // Initialise the search input from the URL on the /search page so a
  // refresh keeps the query visible.
  const [q, setQ] = useState(() => {
    if (typeof window === 'undefined') return '';
    return new URLSearchParams(window.location.search).get('q') ?? '';
  });
  const [open, setOpen] = useState(false);
  const [suggestions, setSuggestions] = useState<Suggestion[]>([]);
  const [active, setActive] = useState<number>(-1);
  const wrapRef = useRef<HTMLDivElement | null>(null);
  // Profile-icon dropdown (Profile + Sign out). Separate state from
  // the search suggestions popover so the two don't fight each other.
  const [accountOpen, setAccountOpen] = useState(false);
  const accountRef = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    if (!accountOpen) return;
    const onClick = (e: MouseEvent) => {
      if (!accountRef.current?.contains(e.target as Node)) setAccountOpen(false);
    };
    window.addEventListener('mousedown', onClick);
    return () => window.removeEventListener('mousedown', onClick);
  }, [accountOpen]);

  // Sync on browser back/forward (the SearchPage reads location.search
  // directly; we mirror it here for the input).
  useEffect(() => {
    const onPop = () => setQ(new URLSearchParams(window.location.search).get('q') ?? '');
    window.addEventListener('popstate', onPop);
    return () => window.removeEventListener('popstate', onPop);
  }, []);

  // Click outside → close dropdown. Re-bind whenever the dropdown is
  // open so the listener doesn't run during the (much more common)
  // closed state.
  useEffect(() => {
    if (!open) return;
    const onClick = (e: MouseEvent) => {
      if (!wrapRef.current?.contains(e.target as Node)) setOpen(false);
    };
    window.addEventListener('mousedown', onClick);
    return () => window.removeEventListener('mousedown', onClick);
  }, [open]);

  // Debounced fetch of /v1/items?q=… as the user types. 200 ms is the
  // Nielsen-style "feels instant" cutoff and matches what most other
  // streamers use. Aborts in-flight requests when the query changes so
  // a stale response can't overwrite the current suggestions.
  const token = auth.user?.access_token;
  useEffect(() => {
    const trimmed = q.trim();
    if (trimmed.length < 2) {
      setSuggestions([]);
      return;
    }
    if (!token) return;
    const ctrl = new AbortController();
    const t = window.setTimeout(() => {
      fetch(`/api/v1/items?q=${encodeURIComponent(trimmed)}&limit=8`, {
        signal: ctrl.signal,
        headers: { Authorization: `Bearer ${token}` },
      })
        .then((r) => (r.ok ? r.json() : { items: [] }))
        .then((j) => {
          const list = (j.items ?? []) as Suggestion[];
          setSuggestions(list);
          setActive(-1);
          if (document.activeElement && wrapRef.current?.contains(document.activeElement)) {
            setOpen(list.length > 0);
          }
        })
        .catch(() => undefined);
    }, 200);
    return () => {
      window.clearTimeout(t);
      ctrl.abort();
    };
  }, [q, token]);

  // Rewrite poster URLs with the stream token so they don't 401 once
  // OIDC silent-renew rotates the bearer. Stream-token TTL is 6 h.
  const enc = useMemo(() => (streamToken ? encodeURIComponent(streamToken) : ''), [streamToken]);
  const suggestionImg = (s: Suggestion) => {
    if (!s.poster_url) return '';
    return enc ? `${s.poster_url}?stream=${enc}` : s.poster_url;
  };

  const pick = (s: Suggestion) => {
    setOpen(false);
    window.location.assign(`/i/${encodeURIComponent(s.id)}`);
  };

  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    if (active >= 0 && suggestions[active]) {
      pick(suggestions[active]);
      return;
    }
    const trimmed = q.trim();
    if (!trimmed) return;
    setOpen(false);
    window.location.assign(`/search?q=${encodeURIComponent(trimmed)}`);
  };

  const onKey = (e: React.KeyboardEvent<HTMLInputElement>) => {
    if (!open || suggestions.length === 0) return;
    if (e.key === 'ArrowDown') {
      e.preventDefault();
      setActive((i) => Math.min(suggestions.length - 1, i + 1));
    } else if (e.key === 'ArrowUp') {
      e.preventDefault();
      setActive((i) => Math.max(-1, i - 1));
    } else if (e.key === 'Escape') {
      setOpen(false);
    }
  };

  return (
    <header className="h-16 bg-[#0d1117] border-b border-[#30363d] flex items-center justify-between px-4">
      <div ref={wrapRef} className="flex-1 max-w-xl relative">
        <form onSubmit={submit}>
          <div className="relative">
            <Search className="absolute left-3 top-1/2 -translate-y-1/2 w-5 h-5 text-[#8b949e]" />
            <input
              type="search"
              value={q}
              onChange={(e) => { setQ(e.target.value); setOpen(true); }}
              onFocus={() => suggestions.length > 0 && setOpen(true)}
              onKeyDown={onKey}
              placeholder="Search movies, shows…"
              className="w-full bg-[#161b22] border border-[#30363d] rounded-lg pl-10 pr-4 py-2 text-[#c9d1d9] placeholder-[#8b949e] focus:outline-none focus:border-[#58a6ff] focus:ring-1 focus:ring-[#58a6ff]"
              autoComplete="off"
            />
          </div>
        </form>

        {open && suggestions.length > 0 && (
          <div className="absolute top-full left-0 right-0 mt-1 bg-[#161b22] border border-[#30363d] rounded-lg shadow-2xl overflow-hidden z-50">
            {suggestions.map((s, i) => (
              <button
                key={s.id}
                type="button"
                onMouseEnter={() => setActive(i)}
                onClick={() => pick(s)}
                className={`flex items-center gap-3 w-full px-3 py-2 text-left transition-colors ${i === active ? 'bg-white/10' : 'hover:bg-white/5'}`}
              >
                <div className="shrink-0 w-10 h-14 rounded bg-[#0d1117] overflow-hidden">
                  {s.poster_url ? (
                    <FadeImage src={suggestionImg(s)} alt={s.title} className="w-full h-full object-cover" loading="lazy" decoding="async" />
                  ) : null}
                </div>
                <div className="flex-1 min-w-0">
                  <div className="text-[#c9d1d9] truncate">{s.title}</div>
                  <div className="text-xs text-[#8b949e]">
                    {s.year ? <>{s.year} · </> : null}
                    {s.type === 'series' ? 'Series' : s.type === 'movie' ? 'Movie' : s.type}
                  </div>
                </div>
              </button>
            ))}
          </div>
        )}
      </div>

      <div className="flex items-center gap-4 ml-6">
        <button className="p-2 text-[#c9d1d9] hover:bg-[#161b22] rounded-lg transition-colors" title="Notifications (coming soon)">
          <Bell className="w-5 h-5" />
        </button>
        <div ref={accountRef} className="relative">
          <button
            onClick={() => setAccountOpen((v) => !v)}
            className="rounded-full hover:ring-2 hover:ring-[#58A6FF]/40 transition-shadow"
            title="Account"
            aria-haspopup="menu"
            aria-expanded={accountOpen}
          >
            <Avatar size={36} />
          </button>
          {accountOpen && (
            <div
              role="menu"
              className="absolute right-0 top-full mt-1 min-w-[180px] bg-[#161b22] border border-[#30363d] rounded-md shadow-2xl py-1 z-50"
            >
              <button
                role="menuitem"
                onClick={() => { setAccountOpen(false); window.location.assign('/me'); }}
                className="w-full text-left px-3 py-2 text-sm text-[#c9d1d9] hover:bg-[#21262d] flex items-center gap-2"
              >
                <UserCircle className="w-4 h-4" />
                Profile
              </button>
              <button
                role="menuitem"
                onClick={() => { setAccountOpen(false); void auth.signoutRedirect(); }}
                className="w-full text-left px-3 py-2 text-sm text-[#c9d1d9] hover:bg-[#21262d] flex items-center gap-2"
              >
                <LogOut className="w-4 h-4" />
                Sign out
              </button>
            </div>
          )}
        </div>
      </div>
    </header>
  );
}
