import { useEffect } from 'react';
import { useAuth } from 'react-oidc-context';
import { setAccessTokenGetter } from './token';

/**
 * Registers the current OIDC access token with the plain `api` module so that
 * every /api/manage/* request carries an Authorization: Bearer header. Renders
 * nothing; it just keeps the token getter pointed at the live auth context.
 *
 * Mounted inside the AuthProvider tree. We register a getter (rather than push
 * the token string on every render) so api.ts always reads the freshest token
 * — including after a silent renew — at the moment a request is made.
 */
export function AuthTokenBridge() {
  const auth = useAuth();
  useEffect(() => {
    setAccessTokenGetter(() => auth.user?.access_token);
    return () => setAccessTokenGetter(() => undefined);
  }, [auth]);
  return null;
}
