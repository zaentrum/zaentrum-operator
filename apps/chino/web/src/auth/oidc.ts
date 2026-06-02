import type { AuthProviderProps } from 'react-oidc-context';
import { WebStorageStateStore } from 'oidc-client-ts';

const env = import.meta.env;

const authority = env.VITE_OIDC_AUTHORITY ?? '';
const clientId = env.VITE_OIDC_CLIENT_ID ?? 'chino';
const redirectUri =
  env.VITE_OIDC_REDIRECT_URI ?? `${window.location.origin}/auth/callback`;
const postLogoutRedirectUri =
  env.VITE_OIDC_POST_LOGOUT_REDIRECT_URI ?? window.location.origin;

export const oidcConfig: AuthProviderProps = {
  authority,
  client_id: clientId,
  redirect_uri: redirectUri,
  post_logout_redirect_uri: postLogoutRedirectUri,
  response_type: 'code',
  scope: 'openid profile email',
  automaticSilentRenew: true,
  userStore: new WebStorageStateStore({ store: window.localStorage }),
  onSigninCallback: () => {
    window.history.replaceState(null, '', window.location.pathname.replace(/\/auth\/callback$/, '/') || '/');
  },
};
