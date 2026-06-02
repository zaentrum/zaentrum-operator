import { Home, Film, Tv, Settings, Zap } from 'lucide-react';

interface ChinoMobileNavProps {
  activeSection: string;
  onSectionChange: (section: string) => void;
}

export function ChinoMobileNav({ activeSection, onSectionChange }: ChinoMobileNavProps) {
  const menuItems = [
    { id: 'home', label: 'Home', icon: Home },
    { id: 'movies', label: 'Movies', icon: Film },
    { id: 'series', label: 'Series', icon: Tv },
    { id: 'zap', label: 'Zap', icon: Zap },
    { id: 'settings', label: 'Settings', icon: Settings },
  ];

  return (
    <nav className="md:hidden fixed bottom-0 left-0 right-0 bg-[#0d1117] border-t border-[#30363d] flex items-center justify-around px-2 py-2 z-40">
      {menuItems.map((item) => {
        const Icon = item.icon;
        const isActive = activeSection === item.id;

        return (
          <button
            key={item.id}
            onClick={() => onSectionChange(item.id)}
            className={`flex flex-col items-center gap-1 px-3 py-2 rounded-lg transition-all ${
              isActive
                ? 'text-[#58a6ff]'
                : 'text-[#8b949e]'
            }`}
          >
            <Icon className="w-5 h-5" />
            <span className="text-xs">{item.label}</span>
          </button>
        );
      })}
    </nav>
  );
}
