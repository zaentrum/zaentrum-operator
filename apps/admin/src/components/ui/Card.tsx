import type { HTMLAttributes, ReactNode } from 'react';

interface CardProps extends HTMLAttributes<HTMLDivElement> {
  /** Renders the card as an interactive surface (hover lift + pointer). */
  interactive?: boolean;
}

export function Card({ interactive, className = '', children, ...rest }: CardProps) {
  return (
    <div
      className={[
        'rounded-lg border border-border bg-surface',
        interactive
          ? 'cursor-pointer transition-colors hover:border-border-2 hover:bg-surface-2'
          : '',
        className,
      ].join(' ')}
      {...rest}
    >
      {children}
    </div>
  );
}

export function CardHeader({
  title,
  icon,
  action,
  description,
}: {
  title: ReactNode;
  icon?: ReactNode;
  action?: ReactNode;
  description?: ReactNode;
}) {
  return (
    <div className="flex items-start justify-between gap-s-4 border-b border-border px-s-5 py-s-4">
      <div className="flex items-start gap-s-3">
        {icon ? <span className="mt-px text-cloud-blue">{icon}</span> : null}
        <div>
          <h2 className="font-ui text-base font-semibold text-fg">{title}</h2>
          {description ? (
            <p className="mt-0.5 text-sm text-fg-muted">{description}</p>
          ) : null}
        </div>
      </div>
      {action}
    </div>
  );
}

export function CardBody({ className = '', children }: { className?: string; children: ReactNode }) {
  return <div className={`px-s-5 py-s-4 ${className}`}>{children}</div>;
}
