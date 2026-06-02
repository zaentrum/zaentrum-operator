import type { ButtonHTMLAttributes, ReactNode } from 'react';
import { Loader2 } from 'lucide-react';

type Variant = 'primary' | 'secondary' | 'ghost' | 'danger';
type Size = 'sm' | 'md';

interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: Variant;
  size?: Size;
  loading?: boolean;
  icon?: ReactNode;
}

const base =
  'focus-ring inline-flex items-center justify-center gap-s-2 rounded-md font-ui font-medium ' +
  'transition-colors disabled:cursor-not-allowed disabled:opacity-50 select-none';

const sizes: Record<Size, string> = {
  sm: 'h-8 px-s-3 text-sm',
  md: 'h-10 px-s-4 text-sm',
};

const variants: Record<Variant, string> = {
  // cloud-blue is the primary CTA per the token sheet.
  primary: 'bg-cloud-blue text-bg hover:brightness-110 active:brightness-95',
  secondary:
    'bg-surface-2 text-fg border border-border-2 hover:border-cloud-blue hover:text-fg',
  ghost: 'text-fg-2 hover:bg-surface-2 hover:text-fg',
  danger:
    'bg-transparent text-[#ff7b72] border border-[#ff7b72]/40 hover:bg-[#ff7b72]/10',
};

export function Button({
  variant = 'primary',
  size = 'md',
  loading = false,
  icon,
  children,
  className = '',
  disabled,
  ...rest
}: ButtonProps) {
  return (
    <button
      className={`${base} ${sizes[size]} ${variants[variant]} ${className}`}
      disabled={disabled || loading}
      {...rest}
    >
      {loading ? <Loader2 size={16} className="animate-spin" /> : icon}
      {children}
    </button>
  );
}
