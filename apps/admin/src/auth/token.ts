// Bridge between the React auth context and the plain `api` module.
//
// The typed manage-API client (src/lib/api.ts) is a singleton object consumed
// directly by pages, not a hook, so it can't call useAuth(). Instead, a small
// component inside the AuthProvider tree (AuthTokenBridge) registers a getter
// here whenever the access token changes, and api.ts reads the current token
// through it. This keeps every existing `api.*` call site unchanged while
// every /api/manage/* request gains an Authorization: Bearer header.

type TokenGetter = () => string | undefined;

let getter: TokenGetter = () => undefined;

/** Register the source of the current OIDC access token. */
export function setAccessTokenGetter(fn: TokenGetter): void {
  getter = fn;
}

/** The current OIDC access token, or undefined when not signed in. */
export function getAccessToken(): string | undefined {
  return getter();
}
