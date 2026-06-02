import { useState } from 'react';
import { Link } from 'react-router-dom';
import { Boxes, FolderInput, Search } from 'lucide-react';
import { api } from '../lib/api';
import type { LibraryItem, LibraryPage as LibraryPageData } from '../lib/api';
import { useAsync } from '../hooks/useAsync';
import { formatBytes, formatDate, formatDuration } from '../lib/format';
import { PageHeader } from '../components/Layout';
import { Badge, Button, Empty, ErrorState, Field, Loading, Stat, Table } from '../components/ui';
import type { Column } from '../components/ui';

const typeTone = (t: string): 'info' | 'ok' | 'warn' | 'neutral' => {
  switch (t) {
    case 'movie':
      return 'info';
    case 'series':
      return 'ok';
    case 'album':
      return 'warn';
    default:
      return 'neutral';
  }
};

const columns: Column<LibraryItem>[] = [
  {
    key: 'title',
    header: 'Title',
    cell: (r) => (
      <div className="flex flex-col">
        <span className="font-medium text-fg">{r.title}</span>
        {r.path ? <span className="font-mono text-xs text-fg-dim">{r.path}</span> : null}
      </div>
    ),
  },
  {
    key: 'type',
    header: 'Type',
    cell: (r) => <Badge tone={typeTone(r.type)}>{r.type}</Badge>,
  },
  { key: 'year', header: 'Year', cell: (r) => <span>{r.year ?? '—'}</span> },
  {
    key: 'duration',
    header: 'Duration',
    cell: (r) => <span className="font-mono text-xs">{formatDuration(r.durationMs)}</span>,
  },
  {
    key: 'size',
    header: 'Size',
    cell: (r) => <span className="font-mono text-xs">{formatBytes(r.sizeBytes)}</span>,
  },
  {
    key: 'added',
    header: 'Added',
    cell: (r) => <span className="text-xs text-fg-muted">{formatDate(r.addedAt)}</span>,
  },
];

export function LibraryPage() {
  const [q, setQ] = useState('');
  const [submitted, setSubmitted] = useState('');

  const { data, loading, error, reload } = useAsync<LibraryPageData>(
    (signal) => api.library({ q: submitted || undefined, limit: 100 }, signal),
    [submitted],
  );

  const items = data?.items ?? [];

  return (
    <>
      <PageHeader
        title="Library"
        description="Everything Stube has catalogued from your media folders."
        action={
          <Link to="/import">
            <Button variant="secondary" icon={<FolderInput size={16} />}>
              Import folder
            </Button>
          </Link>
        }
      />

      <form
        className="mb-s-4 flex max-w-md items-end gap-s-2"
        onSubmit={(e) => {
          e.preventDefault();
          setSubmitted(q.trim());
        }}
      >
        <Field
          label="Search"
          placeholder="Filter by title…"
          value={q}
          onChange={(e) => setQ(e.target.value)}
          className="flex-1"
        />
        <Button type="submit" variant="secondary" icon={<Search size={16} />}>
          Search
        </Button>
      </form>

      {!loading && !error && data ? (
        <div className="mb-s-4 grid grid-cols-2 gap-s-4 sm:grid-cols-4">
          <Stat label="Items shown" value={items.length} icon={<Boxes size={16} />} />
          <Stat label="Total catalogued" value={data.total} note="across all types" />
        </div>
      ) : null}

      {loading ? (
        <Loading label="Loading library…" />
      ) : error ? (
        <ErrorState message={error} onRetry={reload} />
      ) : items.length === 0 ? (
        <Empty
          title={submitted ? 'No matches' : 'Your library is empty'}
          description={
            submitted
              ? `Nothing matched "${submitted}".`
              : 'Scan a folder of media you already own to populate the catalog.'
          }
          icon={<Boxes size={28} />}
          action={
            submitted ? (
              <Button
                variant="ghost"
                onClick={() => {
                  setQ('');
                  setSubmitted('');
                }}
              >
                Clear search
              </Button>
            ) : (
              <Link to="/import">
                <Button icon={<FolderInput size={16} />}>Go to Import</Button>
              </Link>
            )
          }
        />
      ) : (
        <Table columns={columns} rows={items} rowKey={(r) => r.id} />
      )}
    </>
  );
}
