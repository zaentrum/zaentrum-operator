import { useEffect } from 'react';
import { useNavigate } from 'react-router-dom';
import {
  Boxes,
  CheckCircle2,
  Database,
  FolderInput,
  ListChecks,
  Radio,
  RefreshCw,
  Settings,
  Tags,
  Users,
  XCircle,
} from 'lucide-react';
import { api } from '../lib/api';
import type { SetupStatus } from '../lib/api';
import { useAsync } from '../hooks/useAsync';
import { PageHeader } from '../components/Layout';
import { Badge, Card, ErrorState, Loading, Stat } from '../components/ui';

interface Tile {
  to?: string;
  title: string;
  blurb: string;
  icon: typeof Boxes;
  /** Static status pill for tiles whose backing feature is planned. */
  planned?: boolean;
}

const TILES: Tile[] = [
  {
    to: '/library',
    title: 'Library',
    blurb: 'Browse what has been catalogued from your media folders.',
    icon: Boxes,
  },
  {
    to: '/import',
    title: 'Import',
    blurb: 'Scan a folder so Stube catalogs the files already on disk.',
    icon: FolderInput,
  },
  {
    to: '/jobs',
    title: 'Jobs',
    blurb: 'Track scans, enrichment, artwork and packaging work.',
    icon: ListChecks,
  },
  {
    title: 'Metadata',
    blurb: 'Review titles, years and artwork the enricher resolved.',
    icon: Tags,
    planned: true,
  },
  {
    to: '/manage/users',
    title: 'Users',
    blurb: 'Manage who can sign in and what they can reach.',
    icon: Users,
  },
  {
    to: '/manage/updates',
    title: 'Updates',
    blurb: 'Track the running version, switch channels and roll out updates.',
    icon: RefreshCw,
  },
  {
    to: '/settings',
    title: 'Settings',
    blurb: 'Identity, library path and streaming configuration.',
    icon: Settings,
  },
];

function CheckPill({ ok, label }: { ok: boolean; label: string }) {
  return (
    <span className="inline-flex items-center gap-s-1 text-sm">
      {ok ? (
        <CheckCircle2 size={15} className="text-signal-green" />
      ) : (
        <XCircle size={15} className="text-signal-amber" />
      )}
      <span className={ok ? 'text-fg-2' : 'text-fg-muted'}>{label}</span>
    </span>
  );
}

export function Launchpad() {
  const navigate = useNavigate();
  const { data, loading, error, reload } = useAsync<SetupStatus>(
    (signal) => api.setupStatus(signal),
    [],
  );

  // First-run gate: bounce to the wizard once we know the server is
  // unconfigured. We wait for a definitive `configured === false` so a
  // failed status fetch shows an error rather than silently redirecting.
  useEffect(() => {
    if (data && data.configured === false) {
      navigate('/setup', { replace: true });
    }
  }, [data, navigate]);

  return (
    <>
      <PageHeader
        title="Launchpad"
        description="Set up and run your Stube server — a media client + server for a library you own."
      />

      {loading ? (
        <Loading label="Checking server status…" />
      ) : error ? (
        <ErrorState message={error} onRetry={reload} />
      ) : data ? (
        <div className="flex flex-col gap-s-5">
          <div className="grid grid-cols-2 gap-s-4 sm:grid-cols-4">
            <Stat
              label="Status"
              value={data.configured ? 'Ready' : 'Setup'}
              icon={<CheckCircle2 size={16} />}
              note={data.configured ? 'First-run complete' : 'Run first-run setup'}
            />
            <Stat label="Version" value={data.version || '—'} note="Manage API" />
            <div className="col-span-2 rounded-lg border border-border bg-surface px-s-4 py-s-4">
              <span className="text-sm text-fg-muted">Dependency checks</span>
              <div className="mt-s-3 flex flex-wrap gap-x-s-5 gap-y-s-2">
                <CheckPill ok={data.checks.database} label="Database" />
                <CheckPill ok={data.checks.kafka} label="Event bus" />
                <CheckPill ok={data.checks.library} label="Library path" />
              </div>
            </div>
          </div>

          <div className="grid grid-cols-1 gap-s-4 sm:grid-cols-2 lg:grid-cols-3">
            {TILES.map((tile) => {
              const Icon = tile.icon;
              const inner = (
                <div className="flex h-full flex-col p-s-5">
                  <div className="flex items-center justify-between">
                    <span className="flex h-9 w-9 items-center justify-center rounded-md bg-surface-2 text-cloud-blue">
                      <Icon size={18} />
                    </span>
                    {tile.planned ? (
                      <Badge tone="neutral">Planned</Badge>
                    ) : data.configured ? (
                      <Badge tone="ok">Ready</Badge>
                    ) : (
                      <Badge tone="warn">Needs setup</Badge>
                    )}
                  </div>
                  <h3 className="mt-s-3 font-ui text-base font-semibold text-fg">
                    {tile.title}
                  </h3>
                  <p className="mt-s-1 text-sm text-fg-muted">{tile.blurb}</p>
                </div>
              );
              return tile.to && !tile.planned ? (
                <Card
                  key={tile.title}
                  interactive
                  onClick={() => navigate(tile.to!)}
                  role="link"
                  tabIndex={0}
                  onKeyDown={(e) => {
                    if (e.key === 'Enter' || e.key === ' ') navigate(tile.to!);
                  }}
                >
                  {inner}
                </Card>
              ) : (
                <Card key={tile.title} className="opacity-80">
                  {inner}
                </Card>
              );
            })}
          </div>

          <p className="flex items-center gap-s-2 text-xs text-fg-dim">
            <Radio size={13} />
            <Database size={13} />
            Stube catalogs and streams a library you already own. How files arrive on
            disk is out of scope.
          </p>
        </div>
      ) : null}
    </>
  );
}
