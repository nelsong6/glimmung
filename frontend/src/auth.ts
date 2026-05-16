// auth.romaine.life owns the session via a cookie scoped to .romaine.life.
// The browser auto-attaches that cookie on every request to
// glimmung.romaine.life, so this SPA holds no token and doesn't manage
// session lifetime.
//
// On boot we ask the backend "who am I?" via /v1/auth/me. The backend
// forwards the cookie to auth.romaine.life's get-session endpoint and
// returns a SignedIn/IsAdmin/email/name shape. All subsequent API calls
// are credentialled same-origin fetches; the cookie attaches itself.
//
// We preserve the existing exports (initAuth, currentAccount, signIn,
// signOut, publicConfig, authedFetch) so App.tsx + the per-view fetchers
// continue to work — but getIdToken is gone since there is no token.

import { isMockMode, mockAccount } from "./mockApi";

type GlimmungConfig = {
  auth_url: string;
  tank_operator_base_url: string;
};

// Lightweight account shape preserves `username` and `name` for the
// App.tsx header.
export type Account = {
  username: string;
  name: string;
};

let _account: Account | null = null;
let _config: GlimmungConfig | null = null;
let _initPromise: Promise<void> | null = null;

export function initAuth(): Promise<void> {
  if (isMockMode()) {
    _config = {
      auth_url: "https://auth.mock.local",
      tank_operator_base_url: "https://tank.mock.local",
    };
    const ms = mockAccount();
    _account = { username: ms.username, name: ms.name ?? ms.username };
    return Promise.resolve();
  }
  if (_initPromise) return _initPromise;
  _initPromise = (async () => {
    const cfgRes = await fetch("/v1/config");
    if (!cfgRes.ok) throw new Error(`config fetch failed: ${cfgRes.status}`);
    _config = await cfgRes.json();
    // Ask the backend whether we're signed in. The backend reads the
    // .romaine.life session cookie and forwards it upstream.
    try {
      const meRes = await fetch("/v1/auth/me", { credentials: "include" });
      if (!meRes.ok) {
        _account = null;
        return;
      }
      const me = (await meRes.json()) as {
        signed_in?: boolean;
        email?: string;
        name?: string;
      };
      if (!me.signed_in || !me.email) {
        _account = null;
        return;
      }
      _account = { username: me.email, name: me.name ?? me.email };
    } catch {
      _account = null;
    }
  })();
  return _initPromise;
}

export function currentAccount(): Account | null {
  if (isMockMode()) {
    const ms = mockAccount();
    return ms ? { username: ms.username, name: ms.name ?? ms.username } : null;
  }
  return _account;
}

/** User-initiated sign-in: redirect to auth.romaine.life's Microsoft flow.
 *  Returns a promise that never resolves — the page navigates away first. */
export async function signIn(): Promise<Account> {
  if (isMockMode()) {
    const ms = mockAccount();
    return { username: ms.username, name: ms.name ?? ms.username };
  }
  if (!_config) await initAuth();
  const callbackURL = encodeURIComponent(window.location.origin + window.location.pathname);
  // auth.romaine.life exposes a GET endpoint at /sign-in/microsoft that
  // takes callbackURL as a query param, kicks off Better Auth's social
  // flow, and 302s back to the callback once Microsoft completes.
  window.location.href = `${_config!.auth_url}/sign-in/microsoft?callbackURL=${callbackURL}`;
  return new Promise<Account>(() => {});
}

export async function signOut(): Promise<void> {
  if (isMockMode()) return;
  _account = null;
  if (!_config) return;
  // Tell auth.romaine.life to invalidate the session so the next page
  // load doesn't silently re-SSO via the still-valid cookie.
  try {
    await fetch(`${_config.auth_url}/api/auth/sign-out`, {
      method: "POST",
      credentials: "include",
    });
  } catch {
    // best-effort
  }
}

export async function publicConfig(): Promise<GlimmungConfig> {
  if (isMockMode()) {
    return {
      auth_url: "https://auth.mock.local",
      tank_operator_base_url: "https://tank.mock.local",
    };
  }
  if (!_config) {
    const cfgRes = await fetch("/v1/config");
    if (!cfgRes.ok) throw new Error(`config fetch failed: ${cfgRes.status}`);
    _config = await cfgRes.json();
  }
  return _config!;
}

export async function authedFetch(input: RequestInfo, init: RequestInit = {}): Promise<Response> {
  // Cookie attaches automatically on same-origin fetches with credentials.
  await initAuth();
  return fetch(input, { ...init, credentials: "include" });
}
