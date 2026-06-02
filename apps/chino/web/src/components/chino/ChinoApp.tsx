import { useState } from 'react';
import { ChinoSidebar } from './ChinoSidebar';
import { ChinoMobileNav } from './ChinoMobileNav';
import { Header } from '../Header';
import { HomeSection } from '../sections/HomeSection';
import { MoviesSection } from '../sections/MoviesSection';
import { SeriesSection } from '../sections/SeriesSection';
import { SettingsPage } from '../sections/SettingsPage';
import { SearchPage } from '../SearchPage';
import { ZapSection } from '../sections/ZapSection';

interface ChinoAppProps {
  /**
   * When the user lands on /search?q=… we mount ChinoApp with this
   * prop set so the search section is the initial view. The shell
   * (sidebar + header + search input) stays present so the user can
   * refine the query or jump back to Home without losing chrome.
   * undefined → no search context, normal Home landing.
   */
  initialSearchQuery?: string;
}

export function ChinoApp({ initialSearchQuery }: ChinoAppProps = {}) {
  const [activeSection, setActiveSection] = useState(
    initialSearchQuery !== undefined ? 'search' : 'home',
  );

  // Sidebar / mobile-nav navigations always leave the search view —
  // and when they do we strip the /search?q=… off the address bar so a
  // refresh lands on the section the user actually chose. pushState is
  // enough; we don't need to re-render via popstate because the inner
  // state has already advanced.
  const changeSection = (s: string) => {
    setActiveSection(s);
    if (s !== 'search' && window.location.pathname === '/search') {
      window.history.pushState({}, '', '/');
    }
  };

  const renderSection = () => {
    switch (activeSection) {
      case 'home':
        return <HomeSection onNavigate={changeSection} />;
      case 'movies':
        return <MoviesSection />;
      case 'series':
        return <SeriesSection />;
      case 'settings':
        return <SettingsPage />;
      case 'search':
        return <SearchPage query={initialSearchQuery ?? ''} />;
      case 'zap':
        return <ZapSection />;
      default:
        return <HomeSection onNavigate={changeSection} />;
    }
  };

  return (
    <div className="size-full flex bg-[#0d1117] text-white">
      <ChinoSidebar activeSection={activeSection} onSectionChange={changeSection} />

      <div className="flex-1 flex flex-col overflow-hidden">
        <Header />

        <main className="flex-1 overflow-y-auto pb-20 md:pb-0">
          {/* p-4 (1rem) on all breakpoints so the section content
              lines up with the search bar's px-4 (1rem) in Header —
              no jog between header content and main content edges. */}
          <div className="p-4">
            {renderSection()}
          </div>
        </main>

        <ChinoMobileNav activeSection={activeSection} onSectionChange={changeSection} />
      </div>
    </div>
  );
}
