import type { InputHTMLAttributes, ReactNode } from 'react';

interface FieldProps extends InputHTMLAttributes<HTMLInputElement> {
  label: string;
  hint?: ReactNode;
  error?: string;
  /** Render in the JetBrains Mono face — for paths, keys, issuer URLs. */
  mono?: boolean;
  /** Optional element rendered at the right edge of the input row. */
  trailing?: ReactNode;
}

const inputBase =
  'focus-ring w-full rounded-md border bg-bg-2 px-s-3 py-s-2 text-sm text-fg ' +
  'placeholder:text-fg-dim disabled:opacity-60';

export function Field({
  label,
  hint,
  error,
  mono,
  trailing,
  className = '',
  id,
  ...rest
}: FieldProps) {
  const inputId = id ?? `f-${label.toLowerCase().replace(/[^a-z0-9]+/g, '-')}`;
  return (
    <div className={className}>
      <label htmlFor={inputId} className="mb-s-1 block text-sm font-medium text-fg-2">
        {label}
      </label>
      <div className="flex items-stretch gap-s-2">
        <input
          id={inputId}
          className={[
            inputBase,
            mono ? 'font-mono' : 'font-ui',
            error ? 'border-[#ff7b72]' : 'border-border-2',
          ].join(' ')}
          aria-invalid={error ? true : undefined}
          {...rest}
        />
        {trailing}
      </div>
      {error ? (
        <p className="mt-s-1 text-xs text-[#ff7b72]">{error}</p>
      ) : hint ? (
        <p className="mt-s-1 text-xs text-fg-dim">{hint}</p>
      ) : null}
    </div>
  );
}
