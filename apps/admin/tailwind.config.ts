import type { Config } from 'tailwindcss';

// Tailwind theme mapped onto the nalet.cloud design-system tokens
// (src/styles/tokens.css). Colours are wired to the CSS custom
// properties so a single source of truth (the token sheet) drives both
// raw CSS and Tailwind utilities — change a token, both follow.
export default {
  content: ['./index.html', './src/**/*.{js,ts,jsx,tsx}'],
  theme: {
    extend: {
      colors: {
        // canvas
        bg: 'var(--bg)',
        'bg-2': 'var(--bg-2)',
        surface: 'var(--surface)',
        'surface-2': 'var(--surface-2)',
        border: 'var(--border)',
        'border-2': 'var(--border-2)',
        // foreground
        fg: 'var(--fg)',
        'fg-2': 'var(--fg-2)',
        'fg-muted': 'var(--fg-muted)',
        'fg-dim': 'var(--fg-dim)',
        // brand & signal
        'cloud-blue': 'var(--cloud-blue)',
        'cloud-cyan': 'var(--cloud-cyan)',
        'signal-green': 'var(--signal-green)',
        'signal-amber': 'var(--signal-amber)',
      },
      fontFamily: {
        ui: ['Inter', 'system-ui', 'sans-serif'],
        mono: ['JetBrains Mono', 'ui-monospace', 'monospace'],
      },
      // 4px base spacing scale (4/8/12/16/24/32/48/64).
      spacing: {
        's-1': '4px',
        's-2': '8px',
        's-3': '12px',
        's-4': '16px',
        's-5': '24px',
        's-6': '32px',
        's-7': '48px',
        's-8': '64px',
      },
      borderColor: {
        DEFAULT: 'var(--border)',
      },
    },
  },
  plugins: [],
} satisfies Config;
