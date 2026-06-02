import { useAuth } from 'react-oidc-context';
import { ArrowLeft, LogOut } from 'lucide-react';
import { Avatar } from './Avatar';
import { MediaCard } from './MediaCard';
import { LoadingState } from './LoadingState';
import { useWatchHistory } from '../hooks/useWatchHistory';

/**
 * Profile page at /me. Shows the signed-in user's name + email (from
 * the OIDC userinfo claims), a sign-out button, and a grid of items
 * they've watched, newest first.
 *
 * The watch history is everything in watched_history — both auto-
 * stamped rows (player crossing 95 % of duration) and manual rows
 * from the watched-toggle button on DetailPage / EpisodesList.
 */
export function ProfilePage() {
  const auth = useAuth();
  const history = useWatchHistory(60);

  const profile = auth.user?.profile;
  const name = (profile?.name as string | undefined)
    || (profile?.preferred_username as string | undefined)
    || (profile?.email as string | undefined)
    || 'Account';
  const email = profile?.email as string | undefined;

  const signOut = () => {
    void auth.signoutRedirect();
  };

  return (
    <div className="min-h-screen bg-[#0d1117] text-white">
      <div className="max-w-6xl mx-auto px-6 py-8">
        <div className="flex items-center gap-3 mb-8">
          <button
            onClick={() => { if (window.history.length > 1) window.history.back(); else window.location.assign('/'); }}
            className="p-2 rounded-full bg-black/50 hover:bg-black/70 transition-colors"
            title="Back"
          >
            <ArrowLeft className="w-5 h-5" />
          </button>
          <h1 className="text-3xl font-bold">Profile</h1>
        </div>

        {/* Identity card. Keycloak gives us name / email / preferred_username;
            we render whatever's available so the page is meaningful even on
            minimal claim sets. */}
        <div className="bg-[#161b22] border border-[#30363d] rounded-lg p-6 mb-10 flex items-center gap-4">
          <Avatar size={64} className="shrink-0" />
          <div className="flex-1 min-w-0">
            <div className="text-xl font-medium truncate">{name}</div>
            {email && email !== name ? (
              <div className="text-sm text-[#8b949e] truncate">{email}</div>
            ) : null}
          </div>
          <button
            onClick={signOut}
            className="px-4 py-2 rounded-full bg-white/10 hover:bg-white/20 transition-colors flex items-center gap-2"
            title="Sign out"
          >
            <LogOut className="w-4 h-4" />
            <span className="text-sm">Sign out</span>
          </button>
        </div>

        <h2 className="text-2xl font-semibold mb-4">Watch history</h2>
        {history === null ? (
          <LoadingState />
        ) : history.length === 0 ? (
          <p className="text-[#8b949e]">
            Nothing watched yet. Watched items will appear here once you finish a movie
            or episode (or mark one watched via the eye-button on a detail page).
          </p>
        ) : (
          <div className="grid grid-cols-2 sm:grid-cols-[repeat(auto-fill,minmax(190px,1fr))] gap-4">
            {history.map((it) => {
              const isEpisode = it.type === 'episode';
              return (
                <MediaCard
                  key={it.id}
                  id={it.id}
                  title={it.title}
                  image={it.poster_url || ''}
                  year={it.year ? String(it.year) : undefined}
                  rating={it.rating ? it.rating.toFixed(1) : undefined}
                  type={isEpisode ? 'series' : (it.type === 'album' || it.type === 'track' ? 'music' : 'movie')}
                  watchedAt={it.watched_at}
                />
              );
            })}
          </div>
        )}
      </div>
    </div>
  );
}
