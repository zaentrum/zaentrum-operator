import { useEffect, useState } from 'react';
import { Check, FolderTree, KeyRound, Save, ShieldCheck, Sparkles } from 'lucide-react';
import { api, ApiError } from '../lib/api';
import type { ManageConfig } from '../lib/api';
import { useAsync } from '../hooks/useAsync';
import { PageHeader } from '../components/Layout';
import {
  Badge,
  Button,
  Card,
  CardBody,
  CardHeader,
  ErrorState,
  Field,
  Loading,
} from '../components/ui';

type Draft = Pick<ManageConfig, 'displayName' | 'oidcIssuer' | 'oidcClientId' | 'libraryPath'>;

export function SettingsPage() {
  const { data, loading, error, reload } = useAsync<ManageConfig>(
    (signal) => api.getConfig(signal),
    [],
  );

  const [draft, setDraft] = useState<Draft | null>(null);
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);

  // Seed the editable draft once the config loads.
  useEffect(() => {
    if (data && !draft) {
      setDraft({
        displayName: data.displayName,
        oidcIssuer: data.oidcIssuer,
        oidcClientId: data.oidcClientId,
        libraryPath: data.libraryPath,
      });
    }
  }, [data, draft]);

  useEffect(() => {
    if (!saved) return;
    const t = setTimeout(() => setSaved(false), 2000);
    return () => clearTimeout(t);
  }, [saved]);

  const set = (patch: Partial<Draft>) => setDraft((d) => (d ? { ...d, ...patch } : d));

  const dirty =
    !!data &&
    !!draft &&
    (draft.displayName !== data.displayName ||
      draft.oidcIssuer !== data.oidcIssuer ||
      draft.oidcClientId !== data.oidcClientId ||
      draft.libraryPath !== data.libraryPath);

  const save = async () => {
    if (!draft) return;
    setSaving(true);
    setSaveError(null);
    try {
      await api.updateConfig(draft);
      setSaved(true);
      reload();
    } catch (e) {
      setSaveError(
        e instanceof ApiError && e.status === 0
          ? 'Cannot reach the manage-API. Is the server running?'
          : (e as Error).message,
      );
    } finally {
      setSaving(false);
    }
  };

  return (
    <>
      <PageHeader
        title="Settings"
        description="Identity, library path and streaming configuration."
        action={
          <Button
            onClick={save}
            loading={saving}
            disabled={!dirty}
            icon={saved ? <Check size={16} /> : <Save size={16} />}
          >
            {saved ? 'Saved' : 'Save changes'}
          </Button>
        }
      />

      {loading ? (
        <Loading label="Loading configuration…" />
      ) : error ? (
        <ErrorState message={error} onRetry={reload} />
      ) : data && draft ? (
        <div className="flex flex-col gap-s-5">
          {saveError ? (
            <p className="rounded-md border border-[#ff7b72]/30 bg-[#ff7b72]/10 px-s-3 py-s-2 text-sm text-[#ff7b72]">
              {saveError}
            </p>
          ) : null}

          <Card>
            <CardHeader
              icon={<Sparkles size={18} />}
              title="Identity"
              action={<Badge tone="neutral">v{data.version || '—'}</Badge>}
            />
            <CardBody>
              <Field
                label="Server name"
                value={draft.displayName}
                onChange={(e) => set({ displayName: e.target.value })}
              />
            </CardBody>
          </Card>

          <Card>
            <CardHeader icon={<ShieldCheck size={18} />} title="Sign-in (OIDC)" />
            <CardBody className="flex flex-col gap-s-4">
              <Field
                label="OIDC issuer"
                mono
                value={draft.oidcIssuer}
                hint="Discovery base URL."
                onChange={(e) => set({ oidcIssuer: e.target.value })}
              />
              <Field
                label="Client ID"
                mono
                value={draft.oidcClientId}
                onChange={(e) => set({ oidcClientId: e.target.value })}
              />
            </CardBody>
          </Card>

          <Card>
            <CardHeader icon={<FolderTree size={18} />} title="Library" />
            <CardBody>
              <Field
                label="Library path"
                mono
                value={draft.libraryPath}
                hint="Absolute path the server reads media from."
                onChange={(e) => set({ libraryPath: e.target.value })}
              />
            </CardBody>
          </Card>

          <Card>
            <CardHeader icon={<KeyRound size={18} />} title="Streaming" />
            <CardBody className="flex items-center justify-between gap-s-4">
              <div className="text-sm text-fg-muted">
                Stream signing key
                <p className="mt-s-1 text-xs text-fg-dim">
                  The signing key is a secret and is never displayed. Rotate it from the
                  server if you need to.
                </p>
              </div>
              {data.streamSigningKeySet ? (
                <Badge tone="ok" icon={<Check size={12} />}>
                  configured
                </Badge>
              ) : (
                <Badge tone="warn">not set</Badge>
              )}
            </CardBody>
          </Card>
        </div>
      ) : null}
    </>
  );
}
