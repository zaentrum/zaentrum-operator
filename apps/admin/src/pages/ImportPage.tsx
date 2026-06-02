import { useState } from 'react';
import { Link } from 'react-router-dom';
import { FolderInput, Info, ListChecks, ScanLine } from 'lucide-react';
import { api, ApiError } from '../lib/api';
import type { ImportJob } from '../lib/api';
import { PageHeader } from '../components/Layout';
import { Badge, Button, Card, CardBody, CardHeader, Field } from '../components/ui';

export function ImportPage() {
  const [path, setPath] = useState('');
  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);
  const [started, setStarted] = useState<ImportJob | null>(null);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setError(null);
    setStarted(null);
    const value = path.trim();
    if (!value) {
      setError('Enter a folder path to scan.');
      return;
    }
    if (!value.startsWith('/')) {
      setError('Use an absolute path (starts with /).');
      return;
    }
    setSubmitting(true);
    try {
      const job = await api.importScan(value);
      setStarted(job);
    } catch (err) {
      setError(
        err instanceof ApiError && err.status === 0
          ? 'Cannot reach the manage-API. Is the server running?'
          : (err as Error).message,
      );
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <>
      <PageHeader
        title="Import"
        description="Scan a folder so Stube catalogs the media files already on disk."
      />

      <div className="grid grid-cols-1 gap-s-5 lg:grid-cols-3">
        <Card className="lg:col-span-2">
          <CardHeader
            icon={<ScanLine size={18} />}
            title="Scan a folder"
            description="Point Stube at a directory it can read. It indexes the files it finds — it never downloads or moves anything."
          />
          <CardBody>
            <form onSubmit={submit} className="flex flex-col gap-s-4">
              <Field
                label="Folder path"
                mono
                placeholder="/var/lib/stube/media/movies"
                value={path}
                error={error ?? undefined}
                hint="An absolute path inside the server, under your configured library root."
                onChange={(e) => setPath(e.target.value)}
                autoFocus
              />
              <div className="flex items-center gap-s-3">
                <Button type="submit" loading={submitting} icon={<FolderInput size={16} />}>
                  Start scan
                </Button>
                {started ? (
                  <span className="flex items-center gap-s-2 text-sm text-fg-2">
                    <Badge tone="ok">{started.state}</Badge>
                    Job <span className="font-mono text-xs">{started.jobId}</span> queued for{' '}
                    <span className="font-mono text-xs">{started.path}</span>
                  </span>
                ) : null}
              </div>
            </form>

            {started ? (
              <div className="mt-s-4 flex items-center gap-s-2 rounded-md border border-border bg-bg-2 px-s-3 py-s-2 text-sm text-fg-muted">
                <ListChecks size={15} className="text-cloud-blue" />
                Track progress on the{' '}
                <Link to="/jobs" className="text-cloud-blue hover:underline">
                  Jobs
                </Link>{' '}
                page.
              </div>
            ) : null}
          </CardBody>
        </Card>

        <Card>
          <CardHeader icon={<Info size={18} />} title="How import works" />
          <CardBody className="text-sm text-fg-muted">
            <ol className="flex list-decimal flex-col gap-s-2 pl-s-4">
              <li>You give Stube a folder it can read.</li>
              <li>It catalogs each media file it discovers.</li>
              <li>Enrichment resolves titles, years and artwork.</li>
              <li>Analysis and packaging prepare items for playback.</li>
            </ol>
            <p className="mt-s-4 text-xs text-fg-dim">
              Stube is content-neutral: it reads files you already own and are entitled to
              stream. Acquiring content is out of scope.
            </p>
          </CardBody>
        </Card>
      </div>
    </>
  );
}
