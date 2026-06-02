import type { CSSProperties } from 'react';

/**
 * The Stube chevron mark. A single cloud-blue chevron ">" drawn as an SVG so
 * it scales crisply at any header size. Attribute-only styling (fill/stroke)
 * keeps it portable.
 */
export function Chevron({ size = 18, className }: { size?: number; className?: string }) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      className={className}
      aria-hidden="true"
    >
      <path
        d="M8 5l8 7-8 7"
        stroke="var(--cloud-blue)"
        strokeWidth="3"
        strokeLinecap="round"
        strokeLinejoin="round"
      />
    </svg>
  );
}

/**
 * The umbrella wordmark: "> stube" in JetBrains Mono. The chevron is the brand
 * mark; "stube" is set in the mono face per the design system. `subtitle`
 * renders a muted product/context label after the wordmark (e.g. "Manage").
 */
export function Wordmark({
  subtitle,
  size = 18,
}: {
  subtitle?: string;
  size?: number;
}) {
  const style: CSSProperties = { fontSize: size };
  return (
    <span className="inline-flex items-baseline gap-s-2">
      <span className="wordmark text-fg" style={style}>
        <span className="text-cloud-blue">&gt;</span> stube
      </span>
      {subtitle ? (
        <span className="font-ui text-fg-dim" style={{ fontSize: size * 0.78 }}>
          {subtitle}
        </span>
      ) : null}
    </span>
  );
}
