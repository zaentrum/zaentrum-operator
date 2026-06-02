import type { AuthContextProps } from 'react-oidc-context';

export async function apiFetch(
  auth: AuthContextProps,
  input: RequestInfo | URL,
  init: RequestInit = {},
): Promise<Response> {
  const headers = new Headers(init.headers);
  const token = auth.user?.access_token;
  if (token) {
    headers.set('Authorization', `Bearer ${token}`);
  }
  return fetch(input, { ...init, headers });
}
