// Microsoft sign-in is delegated upstream to auth.romaine.life. This SPA
// fetches an RS256 JWT from that service's /api/auth/token endpoint (the
// `.romaine.life` session cookie is auto-attached cross-origin because
// auth.romaine.life mounts CORS with credentials for this origin) and sends
// it directly to the glimmung backend as `Authorization: Bearer`. The backend
// verifies against auth.romaine.life's JWKS and gates on the role claim
// (`admin`/`user` — `pending` returns 403).
//
// We preserve the existing exports (initAuth, currentAccount, signIn, signOut,
// getIdToken, publicConfig, authedFetch) so call sites in App.tsx and the
// per-view fetchers continue to work — the underlying mechanism is
// auth.romaine.life delegation, but the API surface is the same as before.

import { isMockMode, mockAccount } from "./mockApi";

type GlimmungConfig = {
  auth_url: string;
  tank_operator_base_url: string;
};

// Lightweight account shape — preserves `username` and `name` for the App.tsx
// header. Replaces the MSAL `AccountInfo` import.
export type Account = {
  username: string;
  name: string;
};

let _account: Account | null = null;
let _config: GlimmungConfig | null = null;
let _token: string | null = null;
let _tokenExpiry: number = 0;
// Memoize the in-flight initAuth promise so concurrent callers (e.g. the
// Layout effect + a routed view's first authedFetch on a deep-link reload)
// share the same bootstrap instead of racing.
let _initPromise: Promise<void> | null = null;

export function initAuth(): Promise<void> {
  if (isMockMode()) {
    _config = {
      auth_url: "https://auth.mock.local",
      tank_operator_base_url: "https://tank.mock.local",
    };
    return Promise.resolve();
  }
  if (_initPromise) return _initPromise;
  _initPromise = (async () => {
    const cfgRes = await fetch("/v1/config");
    if (!cfgRes.ok) throw new Error(`config fetch failed: ${cfgRes.status}`);
    _config = await cfgRes.json();
    // Try a silent fetch of the upstream JWT — works if the .romaine.life
    // session cookie is still valid (set by a previous auth.romaine.life
    // sign-in, possibly via another app). Returns null cleanly if not.
    await refreshTokenSilently();
  })();
  return _initPromise;
}

async function refreshTokenSilently(): Promise<void> {
  if (!_config) return;
  try {
    const res = await fetch(`${_config.auth_url}/api/auth/token`, {
      credentials: "include",
    });
    if (!res.ok) {
      _token = null;
      _tokenExpiry = 0;
      _account = null;
      return;
    }
    const data = (await res.json()) as { token?: string };
    if (!data.token) {
      _token = null;
      _tokenExpiry = 0;
      _account = null;
      return;
    }
    _token = data.token;
    _account = accountFromJWT(data.token);
    _tokenExpiry = expiryFromJWT(data.token);
  } catch {
    _token = null;
    _tokenExpiry = 0;
    _account = null;
  }
}

function accountFromJWT(token: string): Account | null {
  const claims = decodeJWT(token);
  if (!claims) return null;
  const email = typeof claims.email === "string" ? claims.email : "";
  if (!email) return null;
  const name = typeof claims.name === "string" ? claims.name : email;
  return { username: email, name };
}

function expiryFromJWT(token: string): number {
  const claims = decodeJWT(token);
  if (!claims) return 0;
  const exp = typeof claims.exp === "number" ? claims.exp : 0;
  // Multiply to ms; subtract 60s of leeway so we refresh before the backend
  // would 401 us on the boundary.
  return exp * 1000 - 60_000;
}

function decodeJWT(token: string): Record<string, unknown> | null {
  try {
    const payload = token.split(".")[1];
    if (!payload) return null;
    // base64url -> base64
    const padded = payload.replace(/-/g, "+").replace(/_/g, "/");
    const decoded = atob(padded);
    return JSON.parse(decoded);
  } catch {
    return null;
  }
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
  // auth.romaine.life exposes a GET endpoint at /sign-in/microsoft that takes
  // callbackURL as a query param, kicks off Better Auth's social flow, and
  // 302s back to the callback once Microsoft completes. The Better Auth
  // routes under /api/auth/* are POST-only, so a top-level GET there 404s.
  window.location.href = `${_config!.auth_url}/sign-in/microsoft?callbackURL=${callbackURL}`;
  // Page is navigating away; the post-redirect bootstrap picks up the
  // session via the `.romaine.life` cookie. Return a never-resolving
  // promise so the call site (which `await`s us) doesn't proceed.
  return new Promise<Account>(() => {});
}

export async function signOut(): Promise<void> {
  if (isMockMode()) return;
  _account = null;
  _token = null;
  _tokenExpiry = 0;
  if (!_config) return;
  // Clear the auth.romaine.life session cookie so the next page load doesn't
  // silently re-SSO via the upstream token endpoint.
  try {
    await fetch(`${_config.auth_url}/api/auth/sign-out`, {
      method: "POST",
      credentials: "include",
    });
  } catch {
    // best-effort
  }
}

/** Get a fresh JWT to send to the glimmung backend. Auto-refreshes if
 *  the cached token is within 60s of expiry. */
export async function getIdToken(): Promise<string> {
  if (isMockMode()) return "mock-token";
  if (!_config) await initAuth();
  if (!_token || Date.now() >= _tokenExpiry) {
    await refreshTokenSilently();
  }
  if (!_token) throw new Error("not signed in");
  return _token;
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
  // Auto-init so deep-link reloads work regardless of whether the Layout
  // effect or the routed view's first call wins the mount race.
  await initAuth();
  const token = await getIdToken();
  const headers = new Headers(init.headers);
  headers.set("Authorization", `Bearer ${token}`);
  return fetch(input, { ...init, headers });
}
