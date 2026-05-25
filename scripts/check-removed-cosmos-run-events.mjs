#!/usr/bin/env node
// Migration guard for the cosmos -> Postgres run_events cutover
// (Stage 2c of docs/postgres-migration.md). Blocks reintroduction of
// the cosmos-side event write/read paths that Stage 2c retired.
//
// After Stage 2i deletes the entire cosmos store package, this script's
// patterns are subsumed by a broader check-removed-cosmos.mjs.

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
  "scripts/check-removed-cosmos-run-events.mjs",
  "docs/postgres-migration.md",
]);

const blocked = [
  // cosmos.Store.runEvents.CreateItem / ReadItem inside event paths.
  { name: "cosmos runEvents CreateItem", pattern: /s\.runEvents\.CreateItem\(/ },
  // The sort helper that pg's ORDER BY replaces.
  { name: "sortNativeEventDocs helper", pattern: /\bsortNativeEventDocs\b/ },
  // The Go-side equality check that pg's sameEvent replaces.
  { name: "sameNativeEventDoc helper", pattern: /\bsameNativeEventDoc\b/ },
  // The cosmos run_events query string fragment.
  { name: "cosmos run_events partition query", pattern: /SELECT \* FROM c WHERE c\.run_id = @run_id/ },
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
    continue;
  }
  for (const rule of blocked) {
    if (rule.pattern.test(content)) {
      failures.push({ file: rel, rule: rule.name, pattern: rule.pattern.toString() });
    }
  }
}

if (failures.length > 0) {
  console.error("check-removed-cosmos-run-events: forbidden patterns found:");
  for (const f of failures) {
    console.error(`  ${f.file}: ${f.rule}  (${f.pattern})`);
  }
  console.error(
    "\nThese symbols were retired by Stage 2c of docs/postgres-migration.md.\n" +
      "Use internal/store/pg/run_events.go's RunEventsStore methods instead.\n" +
      "Migration-policy.md prohibits compat shims."
  );
  process.exit(1);
}

console.log(`check-removed-cosmos-run-events: clean (${files.length} files scanned)`);
