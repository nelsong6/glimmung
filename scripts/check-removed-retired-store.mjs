#!/usr/bin/env node
// Repo-wide guard for the completed Cosmos -> Postgres migration.
// Per docs/migration-policy.md, the retired store must not return through
// code, docs, Terraform, metrics, or filenames.

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

const allowedRelativePaths = new Set([
  "scripts/check-removed-retired-store.mjs",
  "docs/postgres-migration.md",
]);

const blockedContent = [
  { name: "Cosmos name", pattern: /\b[Cc]osmos\b|\bCOSMOS\b/ },
  { name: "azcosmos SDK", pattern: /\bazcosmos\b|sdk\/data\/azcosmos/ },
  { name: "Cosmos Terraform", pattern: /azurerm_cosmosdb_|data\.azurerm_cosmosdb_account/ },
  { name: "retired shared account", pattern: /infra-cosmos-serverless/ },
  { name: "retired Cosmos metrics", pattern: /glimmung_cosmos_/ },
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
  if (allowedRelativePaths.has(rel)) continue;

  if (rel.toLowerCase().includes("cosmos")) {
    failures.push({ file: rel, rule: "Cosmos filename", match: rel });
    continue;
  }

  let content;
  try {
    content = await fs.readFile(file, "utf8");
  } catch {
    continue;
  }

  for (const rule of blockedContent) {
    const match = content.match(rule.pattern);
    if (match) {
      failures.push({ file: rel, rule: rule.name, match: match[0] });
    }
  }
}

if (failures.length > 0) {
  console.error("check-removed-retired-store: forbidden retired-store references found:");
  for (const f of failures) {
    console.error(`  ${f.file}: ${f.rule} (${f.match})`);
  }
  console.error("\nUse the Postgres store path. Migration-policy.md prohibits compatibility and fallback paths.");
  process.exit(1);
}

console.log(`check-removed-retired-store: clean (${files.length} files scanned)`);
