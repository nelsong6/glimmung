#!/usr/bin/env node
// Migration guard for the cosmos -> Postgres lock primitive cutover
// (Stage 2b of docs/postgres-migration.md). Per migration-policy.md, the
// retired cosmos-side lock code paths must stay deleted; this script
// blocks them from creeping back in via copy-paste from a stale branch.
//
// After Stage 2i deletes the entire cosmos store package, this script's
// patterns are subsumed by check-removed-cosmos.mjs (broader guard).
// Until then, keep this narrowly scoped to the symbols Stage 2b actually
// removed.

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
  // This script itself names the patterns it blocks.
  "scripts/check-removed-cosmos-locks.mjs",
  // The plan doc mentions the symbols being retired.
  "docs/postgres-migration.md",
]);

// Symbols the Stage 2b PR deleted from cosmos.Store. Re-introducing any of
// these would mean either resurrecting the cosmos lock path (forbidden by
// migration-policy.md) or accidentally name-colliding with the cleanly
// retired surface.
//
// pg.LocksStore methods have similar names (ClaimLock, ReleaseLock, etc.)
// but live on a different receiver type, so the patterns below anchor to
// the cosmos.Store receiver `(s *Store)` to avoid false positives in
// internal/store/pg/.
const blocked = [
  // Cosmos-side public lock methods.
  { name: "cosmos Store.ClaimLock", pattern: /\(s \*Store\) ClaimLock\(/ },
  { name: "cosmos Store.ClaimIssueLock", pattern: /\(s \*Store\) ClaimIssueLock\(/ },
  { name: "cosmos Store.ReleaseLock", pattern: /\(s \*Store\) ReleaseLock\(/ },
  { name: "cosmos Store.ReleaseIssueLock", pattern: /\(s \*Store\) ReleaseIssueLock\(/ },
  { name: "cosmos Store.AnyLockHeld", pattern: /\(s \*Store\) AnyLockHeld\(/ },
  // Cosmos-side private lock helpers.
  { name: "cosmos issueLockHeld helper", pattern: /\(s \*Store\) issueLockHeld\(/ },
  { name: "cosmos prLockHeld helper", pattern: /\(s \*Store\) prLockHeld\(/ },
  { name: "cosmos releaseLock helper", pattern: /\(s \*Store\) releaseLock\(/ },
  { name: "cosmos listIssueLockDocs", pattern: /\blistIssueLockDocs\b/ },
  { name: "cosmos buildPRLockIndex", pattern: /\bbuildPRLockIndex\b/ },
  { name: "cosmos lockDocID encoder", pattern: /\blockDocID\b/ },
];

async function walk(dir) {
  const out = [];
  const entries = await fs.readdir(dir, { withFileTypes: true });
  for (const entry of entries) {
    if (ignoredDirs.has(entry.name)) continue;
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) {
      out.push(...(await walk(full)));
    } else if (entry.isFile()) {
      if (ignoredFiles.has(entry.name)) continue;
      out.push(full);
    }
  }
  return out;
}

const files = await walk(repoRoot);
const failures = [];

for (const file of files) {
  const rel = path.relative(repoRoot, file).replaceAll("\\", "/");
  if (ignoredRelativePaths.has(rel)) continue;
  let content;
  try {
    content = await fs.readFile(file, "utf8");
  } catch (e) {
    // Binary or unreadable; skip.
    continue;
  }
  for (const rule of blocked) {
    if (rule.pattern.test(content)) {
      failures.push({ file: rel, rule: rule.name, pattern: rule.pattern.toString() });
    }
  }
}

if (failures.length > 0) {
  console.error("check-removed-cosmos-locks: forbidden patterns found:");
  for (const f of failures) {
    console.error(`  ${f.file}: ${f.rule}  (${f.pattern})`);
  }
  console.error(
    "\nThese symbols were retired by Stage 2b of docs/postgres-migration.md.\n" +
      "Use internal/store/pg/locks.go's LocksStore methods instead, or delete\n" +
      "the offending file. Migration-policy.md prohibits compat shims."
  );
  process.exit(1);
}

console.log(`check-removed-cosmos-locks: clean (${files.length} files scanned)`);
