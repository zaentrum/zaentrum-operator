import { Home, Film, Tv, Settings, Zap } from 'lucide-react';
import chinoIcon from '../../imports/chino_icon.svg';

interface ChinoSidebarProps {
  activeSection: string;
  onSectionChange: (section: string) => void;
}

export function ChinoSidebar({ activeSection, onSectionChange }: ChinoSidebarProps) {
  const menuItems = [
    { id: 'home', label: 'Home', icon: Home },
    { id: 'movies', label: 'Movies', icon: Film },
    { id: 'series', label: 'Series', icon: Tv },
    { id: 'zap', label: 'Zap', icon: Zap },
  ];

  // Single button cell shape, reused for logo + nav + settings so they
  // all align vertically. 64×64 outer (matches nav button inside p-2),
  // 24×24 inner icon, 8px corner radius.
  const cellBase =
    'w-12 h-12 mx-auto flex items-center justify-center rounded-lg transition-all';

  return (
    <aside className="w-20 bg-[#0d1117] border-r border-[#30363d] flex-col h-full hidden md:flex">
      {/* Logo cell mirrors the header's h-16 so the icon and the search
          bar sit on the same baseline; border-b separates it from the
          nav, matching the design reference. */}
      <div className="h-16 flex items-center justify-center border-b border-[#30363d]">
        <button
          onClick={() => onSectionChange('home')}
          title="Chino — Home"
          className={`${cellBase} overflow-hidden hover:opacity-90`}
        >
          <img src={chinoIcon} alt="Chino" className="w-full h-full" />
        </button>
      </div>

      <nav className="flex-1 px-4 pt-4 space-y-2">
        {menuItems.map((item) => {
          const Icon = item.icon;
          const isActive = activeSection === item.id;
          return (
            <button
              key={item.id}
              onClick={() => onSectionChange(item.id)}
              title={item.label}
              className={`${cellBase} ${
                isActive
                  ? 'bg-[#161b22] text-[#58a6ff]'
                  : 'text-[#8b949e] hover:bg-[#161b22] hover:text-white'
              }`}
            >
              <Icon className="w-6 h-6" />
            </button>
          );
        })}
      </nav>

      <div className="p-4">
        <button
          onClick={() => onSectionChange('settings')}
          title="Settings"
          className={`${cellBase} ${
            activeSection === 'settings'
              ? 'bg-[#161b22] text-[#58a6ff]'
              : 'text-[#8b949e] hover:bg-[#161b22] hover:text-white'
          }`}
        >
          <Settings className="w-6 h-6" />
        </button>
      </div>
    </aside>
  );
}
