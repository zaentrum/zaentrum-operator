import { useState } from 'react';
import {
  ArrowUpCircle,
  Check,
  CheckCircle2,
  CircleDot,
  Loader2,
  Package,
  RefreshCw,
  Server,
  Settings2,
  XCircle,
} from 'lucide-react';
import { api, ApiError } from '../lib/api';
import type { Channel, InstancePatch, InstanceView, UpdateMode } from '../lib/api';
import { useAsync } from '../hooks/useAsync';
import { PageHeader } from '../components/Layout';
import {
  Badge,
  Button,
  Card,
  CardBody,
  CardHeader,
  ErrorState,
  Loading,
  Table,
} from '../components/ui';
import type { Column } from '../components/ui';

const CHANNELS: { value: Channel; label: string; blurb: string }[] = [
  { value: 'stable', label: 'Stable', blurb: 'Slower-moving, production-ready releases.' },
  { value: 'edge', label: 'Edge', blurb: 'Tracks pre-release builds. Newer, less proven.' },
];

/** Map the operator's phase string to a badge tone. */
function phaseTone(phase: string): 'ok' | 'warn' | 'info' | 'neutral' {
  const p = phase.toLowerCase();
  if (p.includes('ready') || p.includes('running') || p.includes('healthy')) return 'ok';
  if (p.includes('updat') || p.includes('progress') || p.includes('roll') || p.includes('pend'))
    return 'info';
  if (p.includes('degrad') || p.includes('error') || p.includes('fail')) return 'warn';
  return 'neutral';
}

/** Accessible on/off switch for the auto-update toggle. */
function Toggle({
  checked,
  onChange,
  disabled,
  label,
}: {
  checked: boolean;
  onChange: (next: boolean) => void;
  disabled?: boolean;
  label: string;
}) {
  return (
    <button
      type="button"
      role="switch"
      aria-checked={checked}
      aria-label={label}
      disabled={disabled}
      onClick={() => onChange(!checked)}
      className={[
        'focus-ring relative inline-flex h-6 w-11 shrink-0 items-center rounded-full transition-colors',
        'disabled:cursor-not-allowed disabled:opacity-50',
        checked ? 'bg-cloud-blue' : 'bg-surface-2 border border-border-2',
      ].join(' ')}
    >
      <span
        className={[
          'inline-block h-4 w-4 transform rounded-full bg-fg transition-transform',
          checked ? 'translate-x-6' : 'translate-x-1',
        ].join(' ')}
      />
    </button>
  );
}

export function UpdatesPage() {
  const { data, loading, error, reload } = useAsync<InstanceView>(
    (signal) => api.getInstance(signal),
    [],
  );

  // Which control is mid-flight, so we can show a per-action spinner without
  // disabling the whole page.
  const [busy, setBusy] = useState<null | 'apply' | 'channel' | 'mode'>(null);
  const [actionError, setActionError] = useState<string | null>(null);

  const patch = async (body: InstancePatch, kind: 'apply' | 'channel' | 'mode') => {
    setBusy(kind);
    setActionError(null);
    try {
      await api.patchInstance(body);
      reload();
    } catch (e) {
      setActionError(
        e instanceof ApiError && e.status === 0
          ? 'Cannot reach the manage-API. Is the server running?'
          : (e as Error).message,
      );
    } finally {
      setBusy(null);
    }
  };

  const updateAvailable =
    !!data && !!data.availableUpdate && data.availableUpdate !== data.currentVersion;

  const componentColumns: Column<InstanceView['components'][number]>[] = [
    {
      key: 'name',
      header: 'Component',
      cell: (c) => <span className="font-medium text-fg">{c.name}</span>,
    },
    {
      key: 'ready',
      header: 'Status',
      cell: (c) =>
        c.ready ? (
          <Badge tone="ok" icon={<CheckCircle2 size={12} />}>
            Ready
          </Badge>
        ) : (
          <Badge tone="warn" icon={<XCircle size={12} />}>
            Not ready
          </Badge>
        ),
    },
    {
      key: 'image',
      header: 'Image',
      className: 'font-mono text-xs text-fg-muted',
      cell: (c) => c.image || '—',
    },
  ];

  return (
    <>
      <PageHeader
        title="Updates"
        description="Track the running version, switch release channels and roll out updates."
        action={
          <Button
            variant="secondary"
            size="sm"
            icon={<RefreshCw size={14} />}
            onClick={reload}
            disabled={loading || busy !== null}
          >
            Refresh
          </Button>
        }
      />

      {loading ? (
        <Loading label="Loading instance status…" />
      ) : error ? (
        <ErrorState message={error} onRetry={reload} />
      ) : data ? (
        <div className="flex flex-col gap-s-5">
          {actionError ? (
            <p className="rounded-md border border-[#ff7b72]/30 bg-[#ff7b72]/10 px-s-3 py-s-2 text-sm text-[#ff7b72]">
              {actionError}
            </p>
          ) : null}

          {/* Update-available banner */}
          {updateAvailable ? (
            <div className="flex flex-col items-start justify-between gap-s-3 rounded-lg border border-cloud-blue/30 bg-cloud-blue/10 px-s-5 py-s-4 sm:flex-row sm:items-center">
              <div className="flex items-start gap-s-3">
                <ArrowUpCircle size={20} className="mt-0.5 shrink-0 text-cloud-blue" />
                <div>
                  <p className="font-ui text-sm font-semibold text-fg">Update available</p>
                  <p className="mt-s-1 text-sm text-fg-muted">
                    Version{' '}
                    <span className="font-mono text-fg-2">{data.availableUpdate}</span> is
                    available on the{' '}
                    <span className="font-medium text-fg-2">{data.channel}</span> channel
                    (running <span className="font-mono">{data.currentVersion || '—'}</span>).
                  </p>
                </div>
              </div>
              <Button
                onClick={() => patch({ apply: true }, 'apply')}
                loading={busy === 'apply'}
                disabled={busy !== null}
                icon={<ArrowUpCircle size={16} />}
              >
                Apply update
              </Button>
            </div>
          ) : (
            <div className="flex items-center gap-s-3 rounded-lg border border-signal-green/30 bg-signal-green/10 px-s-5 py-s-4">
              <CheckCircle2 size={20} className="shrink-0 text-signal-green" />
              <p className="text-sm text-fg-2">
                You are up to date on the{' '}
                <span className="font-medium">{data.channel}</span> channel.
              </p>
            </div>
          )}

          {/* Version / phase summary */}
          <Card>
            <CardHeader icon={<Server size={18} />} title="Running version" />
            <CardBody className="grid grid-cols-1 gap-s-4 sm:grid-cols-3">
              <div>
                <span className="text-xs uppercase tracking-wide text-fg-dim">
                  Current version
                </span>
                <p className="mt-s-1 font-mono text-sm text-fg">
                  {data.currentVersion || '—'}
                </p>
              </div>
              <div>
                <span className="text-xs uppercase tracking-wide text-fg-dim">
                  Requested version
                </span>
                <p className="mt-s-1 font-mono text-sm text-fg">{data.version || '—'}</p>
              </div>
              <div>
                <span className="text-xs uppercase tracking-wide text-fg-dim">Phase</span>
                <p className="mt-s-1">
                  <Badge tone={phaseTone(data.phase)} icon={<CircleDot size={12} />}>
                    {data.phase || 'unknown'}
                  </Badge>
                </p>
              </div>
            </CardBody>
          </Card>

          {/* Channel selector */}
          <Card>
            <CardHeader
              icon={<Settings2 size={18} />}
              title="Release channel"
              description="Choose how fresh the builds you receive are."
              action={
                busy === 'channel' ? (
                  <Loader2 size={16} className="animate-spin text-cloud-blue" />
                ) : null
              }
            />
            <CardBody className="grid grid-cols-1 gap-s-3 sm:grid-cols-2">
              {CHANNELS.map((c) => {
                const active = data.channel === c.value;
                return (
                  <button
                    key={c.value}
                    type="button"
                    disabled={busy !== null}
                    aria-pressed={active}
                    onClick={() => {
                      if (!active) patch({ channel: c.value }, 'channel');
                    }}
                    className={[
                      'focus-ring flex flex-col items-start gap-s-1 rounded-lg border px-s-4 py-s-3 text-left transition-colors',
                      'disabled:cursor-not-allowed disabled:opacity-60',
                      active
                        ? 'border-cloud-blue bg-cloud-blue/10'
                        : 'border-border bg-surface hover:border-border-2 hover:bg-surface-2',
                    ].join(' ')}
                  >
                    <span className="flex w-full items-center justify-between">
                      <span className="font-ui text-sm font-semibold text-fg">{c.label}</span>
                      {active ? (
                        <Check size={16} className="text-cloud-blue" />
                      ) : null}
                    </span>
                    <span className="text-xs text-fg-muted">{c.blurb}</span>
                  </button>
                );
              })}
            </CardBody>
          </Card>

          {/* Auto-update toggle */}
          <Card>
            <CardHeader icon={<RefreshCw size={18} />} title="Automatic updates" />
            <CardBody className="flex items-center justify-between gap-s-4">
              <div className="text-sm text-fg-muted">
                Auto-apply in-channel updates
                <p className="mt-s-1 text-xs text-fg-dim">
                  When on, the operator rolls out the newest tag on the{' '}
                  <span className="font-medium">{data.channel}</span> channel as soon as it
                  appears. When off, updates wait for you to apply them here.
                </p>
              </div>
              <div className="flex items-center gap-s-3">
                {busy === 'mode' ? (
                  <Loader2 size={16} className="animate-spin text-cloud-blue" />
                ) : null}
                <Toggle
                  label="Automatic updates"
                  checked={data.updateMode === 'auto'}
                  disabled={busy !== null}
                  onChange={(next) =>
                    patch({ updateMode: (next ? 'auto' : 'manual') as UpdateMode }, 'mode')
                  }
                />
              </div>
            </CardBody>
          </Card>

          {/* Component health */}
          <Card>
            <CardHeader
              icon={<Package size={18} />}
              title="Components"
              description="Readiness of each managed deployment."
              action={
                <Badge tone="neutral">
                  {data.components.filter((c) => c.ready).length}/{data.components.length}{' '}
                  ready
                </Badge>
              }
            />
            <CardBody className="!px-0 !py-0">
              <div className="px-s-4 py-s-4">
                <Table
                  columns={componentColumns}
                  rows={data.components}
                  rowKey={(c) => c.name}
                  empty="No components reported yet."
                />
              </div>
            </CardBody>
          </Card>
        </div>
      ) : null}
    </>
  );
}
