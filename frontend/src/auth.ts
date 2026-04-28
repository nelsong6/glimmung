import { PublicClientApplication, type AccountInfo, type Configuration } from "@azure/msal-browser";

type GlimmungConfig = {
  entra_client_id: string;
  authority: string;
};

let _msal: PublicClientApplication | null = null;
let _config: GlimmungConfig | null = null;

const SCOPES = ["openid", "profile", "email"];

export async function initAuth(): Promise<void> {
  if (_msal) return;
  const cfgRes = await fetch("/v1/config");
  if (!cfgRes.ok) throw new Error(`config fetch failed: ${cfgRes.status}`);
  _config = await cfgRes.json();
  if (!_config!.entra_client_id) {
    throw new Error("backend has no entra_client_id configured");
  }
  const msalConfig: Configuration = {
    auth: {
      clientId: _config!.entra_client_id,
      authority: _config!.authority,
      redirectUri: window.location.origin + "/",
    },
    cache: { cacheLocation: "sessionStorage" },
  };
  _msal = new PublicClientApplication(msalConfig);
  await _msal.initialize();
}

export function currentAccount(): AccountInfo | null {
  if (!_msal) return null;
  const accs = _msal.getAllAccounts();
  return accs[0] ?? null;
}

export async function signIn(): Promise<AccountInfo> {
  if (!_msal) throw new Error("auth not initialized");
  const result = await _msal.loginPopup({ scopes: SCOPES });
  _msal.setActiveAccount(result.account);
  return result.account;
}

export async function signOut(): Promise<void> {
  if (!_msal) return;
  const account = currentAccount();
  if (!account) return;
  await _msal.logoutPopup({ account });
}

/** Get a fresh ID token. Backend validates with audience=entra_client_id;
 *  matches the tank-operator pattern. */
export async function getIdToken(): Promise<string> {
  if (!_msal) throw new Error("auth not initialized");
  const account = currentAccount();
  if (!account) throw new Error("not signed in");
  const result = await _msal.acquireTokenSilent({ scopes: SCOPES, account });
  return result.idToken;
}

export async function authedFetch(input: RequestInfo, init: RequestInit = {}): Promise<Response> {
  const token = await getIdToken();
  const headers = new Headers(init.headers);
  headers.set("Authorization", `Bearer ${token}`);
  return fetch(input, { ...init, headers });
}
