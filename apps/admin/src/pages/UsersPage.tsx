import { useState } from 'react';
import {
  KeyRound,
  Pencil,
  Trash2,
  UserPlus,
  Users as UsersIcon,
} from 'lucide-react';
import { api, ApiError } from '../lib/api';
import type { NewUser, User, UserUpdate } from '../lib/api';
import { useAsync } from '../hooks/useAsync';
import { PageHeader } from '../components/Layout';
import {
  Badge,
  Button,
  Empty,
  ErrorState,
  Field,
  Loading,
  Modal,
  Table,
} from '../components/ui';
import type { Column } from '../components/ui';

// Normalise any thrown value into a human message for the modal banners.
function errMessage(e: unknown): string {
  if (e instanceof ApiError) {
    return e.status === 0
      ? 'Cannot reach the manage-API. Is the server running?'
      : e.message;
  }
  return (e as Error).message || 'Unexpected error';
}

function fullName(u: User): string {
  const name = `${u.firstName} ${u.lastName}`.trim();
  return name || '—';
}

// ── Add / Edit dialog ─────────────────────────────────────────────────────

interface UserFormProps {
  /** Present when editing; absent when adding. */
  user?: User;
  onClose: () => void;
  onSaved: () => void;
}

function UserFormModal({ user, onClose, onSaved }: UserFormProps) {
  const editing = !!user;
  const [username, setUsername] = useState(user?.username ?? '');
  const [email, setEmail] = useState(user?.email ?? '');
  const [firstName, setFirstName] = useState(user?.firstName ?? '');
  const [lastName, setLastName] = useState(user?.lastName ?? '');
  const [enabled, setEnabled] = useState(user?.enabled ?? true);
  const [password, setPassword] = useState('');
  const [errors, setErrors] = useState<Record<string, string>>({});
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<string | null>(null);

  const validate = (): boolean => {
    const e: Record<string, string> = {};
    if (!editing && !username.trim()) e.username = 'A username is required.';
    if (!email.trim()) {
      e.email = 'An email address is required.';
    } else if (!/^[^@\s]+@[^@\s]+\.[^@\s]+$/.test(email.trim())) {
      e.email = 'Enter a valid email address.';
    }
    setErrors(e);
    return Object.keys(e).length === 0;
  };

  const submit = async () => {
    if (!validate()) return;
    setSaving(true);
    setSaveError(null);
    try {
      if (editing) {
        const patch: UserUpdate = {
          email: email.trim(),
          firstName: firstName.trim(),
          lastName: lastName.trim(),
          enabled,
        };
        await api.updateUser(user.id, patch);
      } else {
        const body: NewUser = {
          username: username.trim(),
          email: email.trim(),
          firstName: firstName.trim(),
          lastName: lastName.trim(),
          ...(password ? { password } : {}),
        };
        await api.createUser(body);
      }
      onSaved();
    } catch (e) {
      setSaveError(errMessage(e));
    } finally {
      setSaving(false);
    }
  };

  return (
    <Modal
      open
      icon={editing ? <Pencil size={18} /> : <UserPlus size={18} />}
      title={editing ? `Edit ${user.username}` : 'Add user'}
      description={
        editing
          ? 'Update profile details and access for this account.'
          : 'Create a new account in the bundled identity provider.'
      }
      onClose={onClose}
      footer={
        <>
          <Button variant="ghost" onClick={onClose} disabled={saving}>
            Cancel
          </Button>
          <Button onClick={submit} loading={saving} icon={<UserPlus size={16} />}>
            {editing ? 'Save changes' : 'Create user'}
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-s-4">
        {saveError ? (
          <p className="rounded-md border border-[#ff7b72]/30 bg-[#ff7b72]/10 px-s-3 py-s-2 text-sm text-[#ff7b72]">
            {saveError}
          </p>
        ) : null}

        <Field
          label="Username"
          mono
          placeholder="jane"
          value={username}
          error={errors.username}
          disabled={editing}
          hint={editing ? 'The username cannot be changed.' : 'Used to sign in. Lowercase, no spaces.'}
          onChange={(e) => setUsername(e.target.value)}
          autoFocus={!editing}
        />
        <Field
          label="Email"
          placeholder="jane@example.com"
          value={email}
          error={errors.email}
          onChange={(e) => setEmail(e.target.value)}
          autoFocus={editing}
        />
        <div className="grid grid-cols-1 gap-s-4 sm:grid-cols-2">
          <Field
            label="First name"
            placeholder="Jane"
            value={firstName}
            onChange={(e) => setFirstName(e.target.value)}
          />
          <Field
            label="Last name"
            placeholder="Doe"
            value={lastName}
            onChange={(e) => setLastName(e.target.value)}
          />
        </div>

        {!editing ? (
          <Field
            label="Initial password (optional)"
            type="password"
            placeholder="leave blank to set later"
            value={password}
            hint="Leave blank to create the account without credentials and set one via Reset password."
            onChange={(e) => setPassword(e.target.value)}
          />
        ) : null}

        <label className="flex cursor-pointer items-center gap-s-2 text-sm text-fg-2">
          <input
            type="checkbox"
            checked={enabled}
            onChange={(e) => setEnabled(e.target.checked)}
            className="focus-ring h-4 w-4 rounded border-border-2 bg-bg-2 accent-cloud-blue"
          />
          Account enabled
        </label>
      </div>
    </Modal>
  );
}

// ── Reset-password dialog ──────────────────────────────────────────────────

function ResetPasswordModal({
  user,
  onClose,
  onDone,
}: {
  user: User;
  onClose: () => void;
  onDone: () => void;
}) {
  const [password, setPassword] = useState('');
  const [temporary, setTemporary] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);

  const submit = async () => {
    if (password.length < 8) {
      setError('Use at least 8 characters.');
      return;
    }
    setSaving(true);
    setError(null);
    try {
      await api.resetUserPassword(user.id, { password, temporary });
      onDone();
    } catch (e) {
      setError(errMessage(e));
    } finally {
      setSaving(false);
    }
  };

  return (
    <Modal
      open
      icon={<KeyRound size={18} />}
      title={`Reset password — ${user.username}`}
      description="Set a new password for this account."
      onClose={onClose}
      footer={
        <>
          <Button variant="ghost" onClick={onClose} disabled={saving}>
            Cancel
          </Button>
          <Button onClick={submit} loading={saving} icon={<KeyRound size={16} />}>
            Set password
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-s-4">
        {error ? (
          <p className="rounded-md border border-[#ff7b72]/30 bg-[#ff7b72]/10 px-s-3 py-s-2 text-sm text-[#ff7b72]">
            {error}
          </p>
        ) : null}
        <Field
          label="New password"
          type="password"
          placeholder="At least 8 characters"
          value={password}
          onChange={(e) => setPassword(e.target.value)}
          autoFocus
        />
        <label className="flex cursor-pointer items-center gap-s-2 text-sm text-fg-2">
          <input
            type="checkbox"
            checked={temporary}
            onChange={(e) => setTemporary(e.target.checked)}
            className="focus-ring h-4 w-4 rounded border-border-2 bg-bg-2 accent-cloud-blue"
          />
          Require a change at next sign-in
        </label>
      </div>
    </Modal>
  );
}

// ── Delete confirmation ────────────────────────────────────────────────────

function DeleteUserModal({
  user,
  onClose,
  onDone,
}: {
  user: User;
  onClose: () => void;
  onDone: () => void;
}) {
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const confirm = async () => {
    setBusy(true);
    setError(null);
    try {
      await api.deleteUser(user.id);
      onDone();
    } catch (e) {
      setError(errMessage(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Modal
      open
      icon={<Trash2 size={18} />}
      title={`Delete ${user.username}?`}
      description="This removes the account from the identity provider. It cannot be undone."
      onClose={onClose}
      footer={
        <>
          <Button variant="ghost" onClick={onClose} disabled={busy}>
            Cancel
          </Button>
          <Button variant="danger" onClick={confirm} loading={busy} icon={<Trash2 size={16} />}>
            Delete user
          </Button>
        </>
      }
    >
      {error ? (
        <p className="rounded-md border border-[#ff7b72]/30 bg-[#ff7b72]/10 px-s-3 py-s-2 text-sm text-[#ff7b72]">
          {error}
        </p>
      ) : (
        <p className="text-sm text-fg-2">
          <span className="font-medium text-fg">{fullName(user)}</span>
          {user.email ? ` (${user.email})` : ''} will lose access immediately.
        </p>
      )}
    </Modal>
  );
}

// ── Page ───────────────────────────────────────────────────────────────────

type Dialog =
  | { kind: 'add' }
  | { kind: 'edit'; user: User }
  | { kind: 'reset'; user: User }
  | { kind: 'delete'; user: User }
  | null;

export function UsersPage() {
  const { data, loading, error, reload } = useAsync<User[]>(
    (signal) => api.listUsers(signal),
    [],
  );
  const [dialog, setDialog] = useState<Dialog>(null);

  const users = data ?? [];

  const closeAndReload = () => {
    setDialog(null);
    reload();
  };

  const columns: Column<User>[] = [
    {
      key: 'user',
      header: 'User',
      cell: (u) => (
        <div className="flex flex-col">
          <span className="font-medium text-fg">{fullName(u)}</span>
          <span className="font-mono text-xs text-fg-dim">{u.username}</span>
        </div>
      ),
    },
    {
      key: 'email',
      header: 'Email',
      cell: (u) => <span className="text-fg-2">{u.email || '—'}</span>,
    },
    {
      key: 'enabled',
      header: 'Status',
      cell: (u) =>
        u.enabled ? (
          <Badge tone="ok">Enabled</Badge>
        ) : (
          <Badge tone="neutral">Disabled</Badge>
        ),
    },
    {
      key: 'actions',
      header: <span className="sr-only">Actions</span>,
      className: 'text-right',
      cell: (u) => (
        <div className="flex justify-end gap-s-1">
          <Button
            variant="ghost"
            size="sm"
            icon={<Pencil size={14} />}
            onClick={() => setDialog({ kind: 'edit', user: u })}
          >
            Edit
          </Button>
          <Button
            variant="ghost"
            size="sm"
            icon={<KeyRound size={14} />}
            onClick={() => setDialog({ kind: 'reset', user: u })}
          >
            Reset
          </Button>
          <Button
            variant="ghost"
            size="sm"
            icon={<Trash2 size={14} />}
            onClick={() => setDialog({ kind: 'delete', user: u })}
          >
            Delete
          </Button>
        </div>
      ),
    },
  ];

  return (
    <>
      <PageHeader
        title="Users"
        description="Accounts in the bundled identity provider — who can sign in to your Stube."
        action={
          <Button icon={<UserPlus size={16} />} onClick={() => setDialog({ kind: 'add' })}>
            Add user
          </Button>
        }
      />

      {loading ? (
        <Loading label="Loading users…" />
      ) : error ? (
        <ErrorState message={error} onRetry={reload} />
      ) : users.length === 0 ? (
        <Empty
          title="No users yet"
          description="Add an account so someone can sign in to your Stube."
          icon={<UsersIcon size={28} />}
          action={
            <Button icon={<UserPlus size={16} />} onClick={() => setDialog({ kind: 'add' })}>
              Add user
            </Button>
          }
        />
      ) : (
        <Table columns={columns} rows={users} rowKey={(u) => u.id} />
      )}

      {dialog?.kind === 'add' ? (
        <UserFormModal onClose={() => setDialog(null)} onSaved={closeAndReload} />
      ) : null}
      {dialog?.kind === 'edit' ? (
        <UserFormModal
          user={dialog.user}
          onClose={() => setDialog(null)}
          onSaved={closeAndReload}
        />
      ) : null}
      {dialog?.kind === 'reset' ? (
        <ResetPasswordModal
          user={dialog.user}
          onClose={() => setDialog(null)}
          onDone={() => setDialog(null)}
        />
      ) : null}
      {dialog?.kind === 'delete' ? (
        <DeleteUserModal
          user={dialog.user}
          onClose={() => setDialog(null)}
          onDone={closeAndReload}
        />
      ) : null}
    </>
  );
}
