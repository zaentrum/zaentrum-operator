import { useEffect, useMemo, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import {
  ArrowLeft,
  ArrowRight,
  Check,
  Copy,
  FolderTree,
  KeyRound,
  RefreshCw,
  ShieldCheck,
  Sparkles,
} from 'lucide-react';
import { api, generateSigningKey } from '../lib/api';
import type { SetupRequest } from '../lib/api';
import { fetchResolvedOidc } from '../auth/runtimeConfig';
import { Wordmark } from '../components/Brand';
import { Button, Field, Stepper } from '../components/ui';
import type { Step } from '../components/ui';

// LOGIN-FIRST wizard.
//
// By the time this renders the operator is already signed in to the bundled
// identity provider (AuthGate gated the app behind a Keycloak login, where
// Keycloak's own UPDATE_PASSWORD action set the `admin` password on first
// login). So the wizard does NOT collect an admin password — that is owned by
// Keycloak, not Zaentrum. It collects the server display name + library path and
// POSTs them with the operator's bearer token already attached.
//
// The server's POST /api/manage/setup REQUIRES oidcIssuer + oidcClientId. For
// the bundled IdP we echo back the issuer + public web client the app
// discovered from GET /api/config; an operator can switch to the advanced path
// and point Zaentrum at their own external OIDC provider instead.
const STEPS: Step[] = [
  { id: 'welcome', title: 'Welcome' },
  { id: 'library', title: 'Library' },
  { id: 'streaming', title: 'Streaming' },
  { id: 'review', title: 'Review' },
];

interface Form {
  displayName: string;
  /** When true, point Zaentrum at an external OIDC provider instead of the
   *  bundled one. */
  useExternalOidc: boolean;
  oidcIssuer: string;
  oidcClientId: string;
  libraryPath: string;
  streamSigningKey: string;
}

const EMPTY: Form = {
  displayName: '',
  useExternalOidc: false,
  oidcIssuer: '',
  oidcClientId: '',
  libraryPath: '',
  streamSigningKey: '',
};

// Per-step validation. Returns a field->message map; empty = valid.
function validate(step: number, f: Form): Record<string, string> {
  const e: Record<string, string> = {};
  if (step === 0) {
    if (!f.displayName.trim()) e.displayName = 'Give your server a name.';
    if (f.useExternalOidc) {
      if (!f.oidcIssuer.trim()) {
        e.oidcIssuer = 'The OIDC issuer URL is required.';
      } else if (!/^https?:\/\//i.test(f.oidcIssuer.trim())) {
        e.oidcIssuer = 'Must be an absolute URL (https://…).';
      }
      if (!f.oidcClientId.trim()) e.oidcClientId = 'The client ID is required.';
    }
  }
  if (step === 1) {
    if (!f.libraryPath.trim()) {
      e.libraryPath = 'Where your media lives on disk.';
    } else if (!f.libraryPath.startsWith('/')) {
      e.libraryPath = 'Use an absolute path (starts with /).';
    }
  }
  if (step === 2 && f.streamSigningKey) {
    // Optional, but if provided it should look like a key (hex, >= 16 chars).
    if (f.streamSigningKey.length < 16) {
      e.streamSigningKey = 'Looks too short — leave blank to auto-generate.';
    }
  }
  return e;
}

export function SetupWizard() {
  const navigate = useNavigate();
  const [step, setStep] = useState(0);
  const [form, setForm] = useState<Form>(EMPTY);
  const [errors, setErrors] = useState<Record<string, string>>({});
  const [submitting, setSubmitting] = useState(false);
  const [submitError, setSubmitError] = useState<string | null>(null);
  const [copied, setCopied] = useState(false);

  // Discover the bundled identity provider's issuer + public web client so we
  // can echo them back on submit (the server requires both). This is the same
  // unauthenticated /api/config the app booted from. Failures are non-fatal:
  // the operator can still use the advanced path to supply an issuer manually.
  const [bundled, setBundled] = useState<{ issuer: string; clientId: string } | null>(
    null,
  );
  useEffect(() => {
    let active = true;
    fetchResolvedOidc()
      .then((r) => {
        if (!active || !r.issuer) return;
        setBundled({ issuer: r.issuer, clientId: r.clientId });
      })
      .catch(() => undefined);
    return () => {
      active = false;
    };
  }, []);

  const set = (patch: Partial<Form>) => setForm((f) => ({ ...f, ...patch }));

  const next = () => {
    const e = validate(step, form);
    setErrors(e);
    if (Object.keys(e).length === 0) {
      setStep((s) => Math.min(s + 1, STEPS.length - 1));
    }
  };
  const back = () => setStep((s) => Math.max(s - 1, 0));

  const finish = async () => {
    setSubmitting(true);
    setSubmitError(null);
    try {
      // Resolve the issuer + client id to persist. Bundled IdP: reuse what we
      // discovered from /api/config. Advanced: the operator's own provider.
      const issuer = form.useExternalOidc ? form.oidcIssuer.trim() : bundled?.issuer ?? '';
      const clientId = form.useExternalOidc
        ? form.oidcClientId.trim()
        : bundled?.clientId ?? '';

      if (!issuer || !clientId) {
        setSubmitError(
          form.useExternalOidc
            ? 'An OIDC issuer and client ID are required.'
            : "Couldn't determine the bundled identity provider's issuer. Try the advanced option to enter it manually.",
        );
        return;
      }

      const body: SetupRequest = {
        displayName: form.displayName.trim(),
        libraryPath: form.libraryPath.trim(),
        oidcIssuer: issuer,
        oidcClientId: clientId,
        // Omit the key when blank so the server generates and keeps it.
        ...(form.streamSigningKey ? { streamSigningKey: form.streamSigningKey } : {}),
      };
      await api.setup(body);
      navigate('/', { replace: true });
    } catch (e) {
      setSubmitError((e as Error).message);
    } finally {
      setSubmitting(false);
    }
  };

  const onGenerate = () => set({ streamSigningKey: generateSigningKey() });

  const copyKey = async () => {
    if (!form.streamSigningKey) return;
    try {
      await navigator.clipboard.writeText(form.streamSigningKey);
      setCopied(true);
    } catch {
      /* clipboard blocked — non-fatal */
    }
  };
  useEffect(() => {
    if (!copied) return;
    const t = setTimeout(() => setCopied(false), 1500);
    return () => clearTimeout(t);
  }, [copied]);

  const isLast = step === STEPS.length - 1;

  return (
    <div className="flex min-h-full flex-col">
      <header className="border-b border-border bg-bg-2">
        <div className="mx-auto flex h-14 max-w-3xl items-center px-s-5">
          <Wordmark subtitle="Setup" />
        </div>
      </header>

      <div className="mx-auto w-full max-w-3xl px-s-5 py-s-6">
        <div className="mb-s-6">
          <Stepper steps={STEPS} current={step} />
        </div>

        <div className="rounded-lg border border-border bg-surface p-s-6">
          {step === 0 ? <WelcomeStep form={form} errors={errors} set={set} /> : null}
          {step === 1 ? <LibraryStep form={form} errors={errors} set={set} /> : null}
          {step === 2 ? (
            <StreamingStep
              form={form}
              errors={errors}
              set={set}
              onGenerate={onGenerate}
              onCopy={copyKey}
              copied={copied}
            />
          ) : null}
          {step === 3 ? <ReviewStep form={form} bundledIssuer={bundled?.issuer} /> : null}

          {submitError ? (
            <p className="mt-s-4 rounded-md border border-[#ff7b72]/30 bg-[#ff7b72]/10 px-s-3 py-s-2 text-sm text-[#ff7b72]">
              Setup failed: {submitError}
            </p>
          ) : null}
        </div>

        <div className="mt-s-5 flex items-center justify-between">
          <Button variant="ghost" onClick={back} disabled={step === 0} icon={<ArrowLeft size={16} />}>
            Back
          </Button>
          {isLast ? (
            <Button onClick={finish} loading={submitting} icon={<Check size={16} />}>
              Finish setup
            </Button>
          ) : (
            <Button onClick={next} icon={<ArrowRight size={16} />}>
              Continue
            </Button>
          )}
        </div>
      </div>
    </div>
  );
}

// ── Step bodies ─────────────────────────────────────────────────────────

interface StepProps {
  form: Form;
  errors: Record<string, string>;
  set: (patch: Partial<Form>) => void;
}

function StepHeading({ icon, title, blurb }: { icon: React.ReactNode; title: string; blurb: string }) {
  return (
    <div className="mb-s-5 flex items-start gap-s-3">
      <span className="mt-px flex h-9 w-9 items-center justify-center rounded-md bg-surface-2 text-cloud-blue">
        {icon}
      </span>
      <div>
        <h2 className="font-ui text-lg font-semibold text-fg">{title}</h2>
        <p className="mt-s-1 text-sm text-fg-muted">{blurb}</p>
      </div>
    </div>
  );
}

function WelcomeStep({ form, errors, set }: StepProps) {
  return (
    <div>
      <StepHeading
        icon={<Sparkles size={18} />}
        title="Welcome to Zaentrum"
        blurb="A neutral media client + server for a library you own and are entitled to stream. You're signed in — let's finish configuring your server."
      />
      <Field
        label="Server name"
        placeholder="Living Room Library"
        value={form.displayName}
        error={errors.displayName}
        hint="Shown to people who sign in. Purely cosmetic."
        onChange={(e) => set({ displayName: e.target.value })}
        autoFocus
      />

      <div className="mt-s-5 rounded-md border border-border bg-bg-2 px-s-4 py-s-3">
        <div className="flex items-start gap-s-2">
          <ShieldCheck size={16} className="mt-px shrink-0 text-cloud-blue" />
          <p className="text-sm text-fg-muted">
            Sign-in is handled by Zaentrum's bundled identity provider — the one you
            just logged in with. Your <span className="font-mono text-fg-2">admin</span>{' '}
            password is managed there, not here.
          </p>
        </div>

        {!form.useExternalOidc ? (
          <button
            type="button"
            onClick={() => set({ useExternalOidc: true })}
            className="focus-ring mt-s-3 rounded text-sm text-cloud-blue hover:underline"
          >
            Advanced: use my own OIDC provider →
          </button>
        ) : (
          <div className="mt-s-4 flex flex-col gap-s-4">
            <Field
              label="OIDC issuer"
              mono
              placeholder="https://id.example.com/realms/main"
              value={form.oidcIssuer}
              error={errors.oidcIssuer}
              hint="The discovery base URL. Zaentrum resolves /.well-known/openid-configuration under it."
              onChange={(e) => set({ oidcIssuer: e.target.value })}
            />
            <Field
              label="Client ID"
              mono
              placeholder="zaentrum"
              value={form.oidcClientId}
              error={errors.oidcClientId}
              hint="The public client your clients (web / mobile / TV) authenticate as."
              onChange={(e) => set({ oidcClientId: e.target.value })}
            />
            <button
              type="button"
              onClick={() => set({ useExternalOidc: false, oidcIssuer: '', oidcClientId: '' })}
              className="focus-ring self-start rounded text-sm text-cloud-blue hover:underline"
            >
              ← Use the bundled identity provider
            </button>
          </div>
        )}
      </div>
    </div>
  );
}

function LibraryStep({ form, errors, set }: StepProps) {
  return (
    <div>
      <StepHeading
        icon={<FolderTree size={18} />}
        title="Library"
        blurb="Where your media files already live, as the server sees them. Zaentrum reads this path — it never downloads or moves content."
      />
      <Field
        label="Library path"
        mono
        placeholder="/var/lib/zaentrum/media"
        value={form.libraryPath}
        error={errors.libraryPath}
        hint="An absolute path inside the server container / host. You scan it from the Import page after setup."
        onChange={(e) => set({ libraryPath: e.target.value })}
        autoFocus
      />
    </div>
  );
}

function StreamingStep({
  form,
  errors,
  set,
  onGenerate,
  onCopy,
  copied,
}: StepProps & { onGenerate: () => void; onCopy: () => void; copied: boolean }) {
  return (
    <div>
      <StepHeading
        icon={<KeyRound size={18} />}
        title="Streaming"
        blurb="A signing key authenticates playback URLs between the API and the stream origin. Leave it blank and the server generates a strong one for you."
      />
      <Field
        label="Stream signing key (optional)"
        mono
        placeholder="leave blank to auto-generate"
        value={form.streamSigningKey}
        error={errors.streamSigningKey}
        hint="Keep this secret. It is shared between the product API and the stream origin; a mismatch causes playback 403s."
        onChange={(e) => set({ streamSigningKey: e.target.value })}
        trailing={
          <div className="flex gap-s-1">
            <Button
              type="button"
              variant="secondary"
              size="md"
              icon={<RefreshCw size={14} />}
              onClick={onGenerate}
            >
              Generate
            </Button>
            {form.streamSigningKey ? (
              <Button
                type="button"
                variant="ghost"
                size="md"
                icon={copied ? <Check size={14} /> : <Copy size={14} />}
                onClick={onCopy}
              >
                {copied ? 'Copied' : 'Copy'}
              </Button>
            ) : null}
          </div>
        }
      />
    </div>
  );
}

function ReviewRow({ label, value, mono }: { label: string; value: string; mono?: boolean }) {
  return (
    <div className="flex items-baseline justify-between gap-s-4 border-b border-border py-s-3 last:border-0">
      <span className="text-sm text-fg-muted">{label}</span>
      <span className={`text-right text-sm text-fg ${mono ? 'font-mono break-all' : ''}`}>
        {value}
      </span>
    </div>
  );
}

function ReviewStep({ form, bundledIssuer }: { form: Form; bundledIssuer?: string }) {
  const keyDisplay = useMemo(() => {
    if (!form.streamSigningKey) return 'Auto-generated by the server';
    return `${form.streamSigningKey.slice(0, 8)}…${form.streamSigningKey.slice(-4)}`;
  }, [form.streamSigningKey]);

  return (
    <div>
      <StepHeading
        icon={<Check size={18} />}
        title="Review & finish"
        blurb="Confirm the configuration. You can change any of this later from Settings."
      />
      <div className="rounded-md border border-border bg-bg-2 px-s-4">
        <ReviewRow label="Server name" value={form.displayName || '—'} />
        {form.useExternalOidc ? (
          <>
            <ReviewRow label="Sign-in" value="External OIDC provider" />
            <ReviewRow label="OIDC issuer" value={form.oidcIssuer || '—'} mono />
            <ReviewRow label="Client ID" value={form.oidcClientId || '—'} mono />
          </>
        ) : (
          <>
            <ReviewRow label="Sign-in" value="Bundled identity provider" />
            <ReviewRow label="OIDC issuer" value={bundledIssuer || 'Discovered from server'} mono />
          </>
        )}
        <ReviewRow label="Library path" value={form.libraryPath || '—'} mono />
        <ReviewRow label="Stream signing key" value={keyDisplay} mono />
      </div>
    </div>
  );
}
