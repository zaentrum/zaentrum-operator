import type { ReactNode } from 'react';
import { NavLink } from 'react-router-dom';
import {
  Boxes,
  FolderInput,
  LayoutGrid,
  ListChecks,
  Settings,
  Tags,
  Users,
} from 'lucide-react';
import { Wordmark } from './Brand';

interface NavItem {
  to: string;
  label: string;
  icon: ReactNode;
  /** Match only the exact path (used for the Launchpad root). */
  end?: boolean;
}

const NAV: NavItem[] = [
  { to: '/', label: 'Launchpad', icon: <LayoutGrid size={16} />, end: true },
  { to: '/library', label: 'Library', icon: <Boxes size={16} /> },
  { to: '/import', label: 'Import', icon: <FolderInput size={16} /> },
  { to: '/jobs', label: 'Jobs', icon: <ListChecks size={16} /> },
  { to: '/settings', label: 'Settings', icon: <Settings size={16} /> },
];

// Extra destinations surfaced on the Launchpad but not yet first-class routes.
export const PLANNED = [
  { label: 'Metadata', icon: <Tags size={16} /> },
  { label: 'Users', icon: <Users size={16} /> },
];

function Header() {
  return (
    <header className="sticky top-0 z-10 border-b border-border bg-bg-2/90 backdrop-blur">
      <div className="mx-auto flex h-14 max-w-6xl items-center justify-between px-s-5">
        <NavLink to="/" className="focus-ring rounded">
          <Wordmark subtitle="Manage" />
        </NavLink>
        <span className="text-xs text-fg-dim">
          a media client + server for a library you own
        </span>
      </div>
    </header>
  );
}

function SideNav() {
  return (
    <nav className="flex shrink-0 flex-col gap-s-1 md:w-52">
      {NAV.map((item) => (
        <NavLink
          key={item.to}
          to={item.to}
          end={item.end}
          className={({ isActive }) =>
            [
              'focus-ring flex items-center gap-s-2 rounded-md px-s-3 py-s-2 text-sm transition-colors',
              isActive
                ? 'bg-surface-2 text-fg'
                : 'text-fg-muted hover:bg-surface hover:text-fg',
            ].join(' ')
          }
        >
          {item.icon}
          {item.label}
        </NavLink>
      ))}
    </nav>
  );
}

/** App chrome: header + side nav + scrolling content region. */
export function Layout({ children }: { children: ReactNode }) {
  return (
    <div className="flex min-h-full flex-col">
      <Header />
      <div className="mx-auto flex w-full max-w-6xl flex-1 flex-col gap-s-5 px-s-5 py-s-5 md:flex-row">
        <SideNav />
        <main className="min-w-0 flex-1">{children}</main>
      </div>
    </div>
  );
}

/** Page title block reused by the inner pages. */
export function PageHeader({
  title,
  description,
  action,
}: {
  title: string;
  description?: ReactNode;
  action?: ReactNode;
}) {
  return (
    <div className="mb-s-5 flex items-end justify-between gap-s-4">
      <div>
        <h1 className="font-ui text-xl font-semibold text-fg">{title}</h1>
        {description ? <p className="mt-s-1 text-sm text-fg-muted">{description}</p> : null}
      </div>
      {action}
    </div>
  );
}
