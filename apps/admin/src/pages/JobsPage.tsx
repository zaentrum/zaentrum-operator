import { CheckCircle2, Clock, Loader2, RefreshCw, XCircle } from 'lucide-react';
import { api } from '../lib/api';
import type { Job, JobsPage as JobsPageData, JobState } from '../lib/api';
import { useAsync } from '../hooks/useAsync';
import { formatDate } from '../lib/format';
import { PageHeader } from '../components/Layout';
import { Badge, Button, Empty, ErrorState, Loading, Table } from '../components/ui';
import type { Column } from '../components/ui';

function StateBadge({ state }: { state: JobState }) {
  switch (state) {
    case 'running':
      return (
        <Badge tone="info" icon={<Loader2 size={12} className="animate-spin" />}>
          running
        </Badge>
      );
    case 'done':
      return (
        <Badge tone="ok" icon={<CheckCircle2 size={12} />}>
          done
        </Badge>
      );
    case 'failed':
      return (
        <Badge tone="danger" icon={<XCircle size={12} />}>
          failed
        </Badge>
      );
    default:
      return (
        <Badge tone="neutral" icon={<Clock size={12} />}>
          queued
        </Badge>
      );
  }
}

function ProgressBar({ value }: { value?: number }) {
  if (value == null) return <span className="text-xs text-fg-dim">—</span>;
  const pct = Math.max(0, Math.min(100, value));
  return (
    <div className="flex items-center gap-s-2">
      <div className="h-1.5 w-24 overflow-hidden rounded-full bg-surface-2">
        <div className="h-full rounded-full bg-cloud-blue" style={{ width: `${pct}%` }} />
      </div>
      <span className="font-mono text-xs text-fg-muted">{pct}%</span>
    </div>
  );
}

const columns: Column<Job>[] = [
  {
    key: 'kind',
    header: 'Job',
    cell: (r) => (
      <div className="flex flex-col">
        <span className="font-medium text-fg">{r.kind}</span>
        {r.detail ? <span className="text-xs text-fg-muted">{r.detail}</span> : null}
        {r.error ? <span className="text-xs text-[#ff7b72]">{r.error}</span> : null}
      </div>
    ),
  },
  { key: 'state', header: 'State', cell: (r) => <StateBadge state={r.state} /> },
  { key: 'progress', header: 'Progress', cell: (r) => <ProgressBar value={r.progress} /> },
  {
    key: 'started',
    header: 'Started',
    cell: (r) => <span className="text-xs text-fg-muted">{formatDate(r.startedAt)}</span>,
  },
  {
    key: 'finished',
    header: 'Finished',
    cell: (r) => <span className="text-xs text-fg-muted">{formatDate(r.finishedAt)}</span>,
  },
];

export function JobsPage() {
  const { data, loading, error, reload } = useAsync<JobsPageData>(
    (signal) => api.jobs(signal),
    [],
  );
  const jobs = data?.jobs ?? [];

  return (
    <>
      <PageHeader
        title="Jobs"
        description="Scans, enrichment, analysis, artwork and packaging work."
        action={
          <Button variant="secondary" icon={<RefreshCw size={16} />} onClick={reload}>
            Refresh
          </Button>
        }
      />

      {loading ? (
        <Loading label="Loading jobs…" />
      ) : error ? (
        <ErrorState message={error} onRetry={reload} />
      ) : jobs.length === 0 ? (
        <Empty
          title="No jobs yet"
          description="Background work shows up here once you start an import scan."
          icon={<Clock size={28} />}
        />
      ) : (
        <Table columns={columns} rows={jobs} rowKey={(r) => r.id} />
      )}
    </>
  );
}
