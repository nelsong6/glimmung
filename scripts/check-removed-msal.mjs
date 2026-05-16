#!/usr/bin/env node
// Migration guard for the auth.romaine.life delegation cutover.
// Per tank-operator/docs/migration-policy.md: the old Entra / MSAL / per-app
// allowlist surface is deleted end-to-end and must not creep back via
// copy-paste or new code.

import fs from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");

const ignoredDirs = new Set([
  ".claude",
  ".git",
  ".terraform",
  ".venv",
  "__pycache__",
  "build",
  "coverage",
  "dist",
  "node_modules",
  "venv",
]);

const ignoredFiles = new Set([
  "package-lock.json",
  "pnpm-lock.yaml",
  "yarn.lock",
  ".env",
  ".env.local",
]);

const ignoredRelativePaths = new Set([
  "scripts/check-removed-msal.mjs",
  // Test fixtures that intentionally contain Microsoft URLs to verify the
  // verifier rejects unexpected issuers / unknown signing keys.
  "internal/auth/k8s_test.go",
  "internal/auth/romaine_life_test.go",
]);

const blocked = [
  { name: "MSAL browser package", pattern: /@azure\/msal-browser/ },
  { name: "MSAL PublicClientApplication", pattern: /\bPublicClientApplication\b/ },
  { name: "loginRedirect / loginPopup", pattern: /\blogin(?:Redirect|Popup)\b/ },
  { name: "logoutRedirect / logoutPopup", pattern: /\blogout(?:Redirect|Popup)\b/ },
  { name: "handleRedirectPromise", pattern: /\bhandleRedirectPromise\b/ },
  { name: "acquireTokenSilent", pattern: /\bacquireTokenSilent\b/ },
  { name: "legacy EntraAuthenticator", pattern: /\bEntraAuthenticator\b/ },
  { name: "legacy NewEntraAuthenticator", pattern: /\bNewEntraAuthenticator\b/ },
  { name: "per-app email allowlist env", pattern: /\bALLOWED_EMAILS?\b/ },
  { name: "Entra client ID env", pattern: /\bENTRA_CLIENT_ID\b/ },
  { name: "Entra test client ID env", pattern: /\bENTRA_TEST_CLIENT_ID\b/ },
  { name: "Microsoft login JWKS URL", pattern: /login\.microsoftonline\.com.*discovery/ },
  { name: "Microsoft online issuer pattern", pattern: /login\.microsoftonline\.com\/.*v2\.0/ },
  { name: "native auth redirect reconciler", pattern: /\bnative_auth_redirects\b/ },
  { name: "legacy native standby entra redirects", pattern: /\bnative_standby_entra_redirects\b/ },
  { name: "glimmung-oauth Entra app reg name", pattern: /\bglimmung-oauth(?:-test)?-client-id\b/ },
  { name: "glimmung-admin-emails KV secret", pattern: /\bglimmung-admin-emails\b/ },
  { name: "Application.ReadWrite.All MS Graph permission", pattern: /Application\.ReadWrite\.(?:All|OwnedBy)/ },
  { name: "RomaineLifeAuthenticator (replaced by CookieDelegate)", pattern: /\bRomaineLifeAuthenticator\b/ },
  { name: "NewRomaineLifeAuthenticator", pattern: /\bNewRomaineLifeAuthenticator\b/ },
  { name: "JWKS endpoint URL (cookie-only — no local JWT verify)", pattern: /auth\.romaine\.life\/api\/auth\/jwks/ },
  { name: "auth.romaine.life /api/auth/token (cookie-only — no token fetch)", pattern: /auth\.romaine\.life\/api\/auth\/token/ },
  { name: "createRemoteJWKSet (no JWT verify in app)", pattern: /\bcreateRemoteJWKSet\b/ },
  { name: "localStorage token storage", pattern: /localStorage\.(get|set|remove)Item\(['"`]?token/i },
];

const failures = [];

for await (const filePath of walk(repoRoot)) {
  const relativePath = toRepoPath(filePath);
  if (ignoredRelativePaths.has(relativePath)) continue;
  const bytes = await fs.readFile(filePath);
  if (bytes.includes(0)) continue;
  const text = bytes.toString("utf8");
  for (const rule of blocked) {
    const match = rule.pattern.exec(text);
    if (!match) continue;
    const { line, column } = lineAndColumn(text, match.index);
    failures.push(`${relativePath}:${line}:${column} ${rule.name}: ${JSON.stringify(match[0])}`);
  }
}

if (failures.length > 0) {
  console.error("Retired MSAL/Entra/allowlist surface detected:");
  for (const failure of failures) console.error(`- ${failure}`);
  process.exit(1);
}

console.log("No retired MSAL/Entra/allowlist surfaces found.");

async function* walk(dir) {
  const entries = await fs.readdir(dir, { withFileTypes: true });
  for (const entry of entries) {
    const absolutePath = path.join(dir, entry.name);
    if (entry.isDirectory()) {
      if (!ignoredDirs.has(entry.name)) yield* walk(absolutePath);
      continue;
    }
    if (!entry.isFile()) continue;
    if (ignoredFiles.has(entry.name)) continue;
    yield absolutePath;
  }
}

function toRepoPath(filePath) {
  return path.relative(repoRoot, filePath).split(path.sep).join("/");
}

function lineAndColumn(text, index) {
  const before = text.slice(0, index);
  const lines = before.split(/\r\n|\r|\n/);
  return {
    line: lines.length,
    column: lines[lines.length - 1].length + 1,
  };
}
