import { useState } from 'react';
import { useAuth } from 'react-oidc-context';

interface AvatarProps {
  /** Pixel size. Defaults to 36 — matches the Header's icon hit target. */
  size?: number;
  /** Extra Tailwind classes for the outer element. */
  className?: string;
}

/**
 * Circular user avatar driven by OIDC userinfo claims.
 *   - Prefers the `picture` URL claim (Keycloak emits this when an
 *     account has an uploaded photo or a federated identity that
 *     carries one).
 *   - Falls back to a token-coloured circle with the user's initials
 *     (from `name` / `preferred_username` / `email`).
 *   - On image load error, swaps to the initials fallback so a broken
 *     picture URL never leaves an empty box.
 *
 * Shared by the Header dropdown trigger + the ProfilePage identity
 * card so both surfaces look consistent.
 */
export function Avatar({ size = 36, className = '' }: AvatarProps) {
  const auth = useAuth();
  const [imgFailed, setImgFailed] = useState(false);

  const profile = auth.user?.profile;
  const picture = (profile?.picture as string | undefined) ?? '';
  const name = (profile?.name as string | undefined)
    || (profile?.preferred_username as string | undefined)
    || (profile?.email as string | undefined)
    || '';

  // Initials: first letter of the first two words, uppercased. Falls
  // back to '?' so the circle is never empty.
  const initials = name
    .split(/[\s._-]+/)
    .filter(Boolean)
    .slice(0, 2)
    .map((w) => w[0]?.toUpperCase() ?? '')
    .join('') || '?';

  const dim = { width: `${size}px`, height: `${size}px` };
  const fontSize = `${Math.max(11, Math.round(size * 0.42))}px`;

  if (picture && !imgFailed) {
    return (
      <img
        src={picture}
        alt={name || 'Account'}
        style={dim}
        className={`rounded-full object-cover bg-[#21262d] ${className}`}
        onError={() => setImgFailed(true)}
        referrerPolicy="no-referrer"
      />
    );
  }

  return (
    <div
      style={{ ...dim, fontSize }}
      className={`rounded-full bg-[#21262d] text-[#58A6FF] font-semibold flex items-center justify-center select-none ${className}`}
      aria-label={name || 'Account'}
      title={name || 'Account'}
    >
      {initials}
    </div>
  );
}
