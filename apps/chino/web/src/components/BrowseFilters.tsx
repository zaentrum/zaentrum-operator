import { useEffect, useState } from 'react';
import { useAuth } from 'react-oidc-context';
import { ChevronDown } from 'lucide-react';

export interface BrowseQuery {
  genre?: string;
  yearMin?: number;
  yearMax?: number;
  ratingMin?: number;
  sort?: 'rating' | 'year' | 'title' | 'newest';
}

interface BrowseFiltersProps {
  value: BrowseQuery;
  onChange: (q: BrowseQuery) => void;
}

const DECADES: { label: string; min: number; max: number }[] = [
  { label: '2020s', min: 2020, max: 2099 },
  { label: '2010s', min: 2010, max: 2019 },
  { label: '2000s', min: 2000, max: 2009 },
  { label: '1990s', min: 1990, max: 1999 },
  { label: '1980s', min: 1980, max: 1989 },
  { label: 'Older', min: 1900, max: 1979 },
];

const RATINGS: { label: string; min: number }[] = [
  { label: '8.0+', min: 8.0 },
  { label: '7.0+', min: 7.0 },
  { label: '6.0+', min: 6.0 },
];

/**
 * Lightweight filter chip strip rendered above the Movies / Shows grids.
 * Loads genres from chino-api so the chip set reflects the actual library.
 * Genre, decade, and rating are independent filters — apply them all in
 * the URL state via onChange.
 */
export function BrowseFilters({ value, onChange }: BrowseFiltersProps) {
  const auth = useAuth();
  const [genres, setGenres] = useState<string[]>([]);

  useEffect(() => {
    if (auth.isLoading || !auth.isAuthenticated) return;
    fetch('/api/v1/genres', {
      headers: { Authorization: `Bearer ${auth.user?.access_token ?? ''}` },
    })
      .then((r) => (r.ok ? r.json() : null))
      .then((j) => setGenres(Array.isArray(j?.genres) ? j.genres : []))
      .catch(() => setGenres([]));
  }, [auth.isAuthenticated, auth.isLoading, auth.user?.access_token]);

  const activeDecade = DECADES.find((d) => d.min === value.yearMin && d.max === value.yearMax);
  const activeRating = RATINGS.find((r) => r.min === value.ratingMin);

  return (
    <div className="mb-6 space-y-3">
      <div className="flex flex-wrap items-center gap-2">
        <span className="text-[#8b949e] text-sm mr-1 w-16">Genre</span>
        <Dropdown
          label={value.genre ?? 'All'}
          items={['All', ...genres]}
          onPick={(v) => onChange({ ...value, genre: v === 'All' ? undefined : v })}
        />
        <span className="text-[#8b949e] text-sm ml-4 mr-1 w-16">Decade</span>
        {DECADES.map((d) => {
          const isActive = activeDecade?.label === d.label;
          return (
            <Chip
              key={d.label}
              active={isActive}
              onClick={() =>
                onChange({
                  ...value,
                  yearMin: isActive ? undefined : d.min,
                  yearMax: isActive ? undefined : d.max,
                })
              }
            >
              {d.label}
            </Chip>
          );
        })}
      </div>
      <div className="flex flex-wrap items-center gap-2">
        <span className="text-[#8b949e] text-sm mr-1 w-16">Rating</span>
        {RATINGS.map((r) => {
          const isActive = activeRating?.label === r.label;
          return (
            <Chip
              key={r.label}
              active={isActive}
              onClick={() =>
                onChange({ ...value, ratingMin: isActive ? undefined : r.min })
              }
            >
              {r.label}
            </Chip>
          );
        })}
        <span className="text-[#8b949e] text-sm ml-4 mr-1 w-16">Sort</span>
        <Dropdown
          label={sortLabel(value.sort)}
          items={['Title', 'Rating', 'Year (newest)', 'Newest added']}
          onPick={(v) => {
            const m: Record<string, BrowseQuery['sort']> = {
              Title: 'title',
              Rating: 'rating',
              'Year (newest)': 'year',
              'Newest added': 'newest',
            };
            onChange({ ...value, sort: m[v] });
          }}
        />
        {hasAnyFilter(value) ? (
          <button
            onClick={() => onChange({})}
            className="ml-auto text-sm text-[#8b949e] hover:text-white underline-offset-2 hover:underline"
          >
            Clear filters
          </button>
        ) : null}
      </div>
    </div>
  );
}

function hasAnyFilter(q: BrowseQuery): boolean {
  return !!(q.genre || q.yearMin || q.yearMax || q.ratingMin || q.sort);
}

function sortLabel(s?: BrowseQuery['sort']): string {
  switch (s) {
    case 'rating': return 'Rating';
    case 'year': return 'Year (newest)';
    case 'newest': return 'Newest added';
    // Default (undefined) and explicit 'title' both render as the
    // catalogue's natural alphabetical order (katalog-api falls
    // through to ORDER BY sorttitle ASC when sort is empty). Labelling
    // the empty state "Title" makes the implicit behaviour explicit
    // per #113.
    case 'title':
    default: return 'Title';
  }
}

function Chip({
  children,
  active,
  onClick,
}: {
  children: React.ReactNode;
  active: boolean;
  onClick: () => void;
}) {
  return (
    <button
      onClick={onClick}
      className={`px-3 py-1 rounded-full text-xs border transition-colors ${
        active
          ? 'bg-[#58a6ff] border-[#58a6ff] text-white'
          : 'bg-[#161b22] border-[#30363d] text-[#c9d1d9] hover:bg-[#21262d]'
      }`}
    >
      {children}
    </button>
  );
}

function Dropdown({
  label,
  items,
  onPick,
}: {
  label: string;
  items: string[];
  onPick: (v: string) => void;
}) {
  const [open, setOpen] = useState(false);
  return (
    <div className="relative">
      <button
        onClick={() => setOpen((o) => !o)}
        // leading-none collapses the text-xs line-box (1rem tall) down
        // to the font-size (12px) so the 12px chevron and the text
        // glyph share the same vertical center. Without it, the V is
        // bottom-heavy in its bbox while the text line-box has extra
        // leading, and the chevron reads as sitting below the label.
        className="px-3 py-1 rounded-full text-xs leading-none border border-[#30363d] bg-[#161b22] hover:bg-[#21262d] text-[#c9d1d9] flex items-center gap-1"
      >
        {label}
        <ChevronDown className="w-3 h-3" />
      </button>
      {open ? (
        <div className="absolute z-20 mt-1 left-0 min-w-[8rem] max-h-72 overflow-y-auto rounded-md border border-[#30363d] bg-[#0d1117] shadow-2xl py-1">
          {items.map((v) => (
            <button
              key={v}
              onClick={() => { onPick(v); setOpen(false); }}
              className="block w-full text-left px-3 py-1.5 text-sm text-[#c9d1d9] hover:bg-[#21262d]"
            >
              {v}
            </button>
          ))}
        </div>
      ) : null}
    </div>
  );
}
