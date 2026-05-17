#!/usr/bin/env node
// Migration guard for the handler 5xx observability contract (glimmung#514).
//
// Per tank-operator/docs/migration-policy.md ("no fallback defaults") and
// docs/quality-timeframes.md ("Observability exists for the bugs a user
// would otherwise have to guess about"): every 5xx response in the server
// package must go through writeInternalError(w, r, err, summary) so the
// underlying error survives as a structured slog.Error record. The prior
// path — writeProblem(w, http.StatusInternalServerError, "summary") —
// discarded `err` at the call site, leaving the response body's abstract
// summary as the only signal in logs.
//
// This script fails CI on any reintroduction of the swallow pattern. If a
// 500 emitter genuinely has no underlying error (e.g. server-capability
// check like an http.Flusher cast), construct an explicit errors.New(...)
// at the call site and pass it to writeInternalError — the type system
// then makes the synthetic-error decision visible to reviewers.

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

const ignoredRelativePaths = new Set([
  // This file documents the retired pattern in prose; the literals are
  // intentional examples, not live code.
  "scripts/check-handler-5xx-migration.mjs",
  // The contract test for writeInternalError documents the retired
  // pattern in its docstring (motivation + failure mode it guards
  // against). The literal in the comment is intentional and not a
  // live callsite.
  "internal/server/internal_error_test.go",
]);

// Block writeProblem with any 5xx status. 500 (InternalServerError) is the
// hot case — the bug that motivated this guard — but 502/503/504 emitters
// equally need the err. ServiceUnavailable used for "X store not
// configured" callsites is legitimately err-less and uses writeProblem;
// those use cases pre-existed and are not subject to this guard because
// the configuration-absence shape genuinely has no underlying error
// object. The guard fires only when a 5xx number is explicitly threaded
// through writeProblem.
const blocked5xxStatuses = [
  "StatusInternalServerError",
];

const failures = [];

for await (const filePath of walk(repoRoot)) {
  const relativePath = toRepoPath(filePath);
  if (ignoredRelativePaths.has(relativePath)) continue;
  if (!filePath.endsWith(".go")) continue;
  const bytes = await fs.readFile(filePath);
  if (bytes.includes(0)) continue;
  const text = bytes.toString("utf8");
  const lines = text.split(/\r\n|\r|\n/);
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i];
    if (!line.includes("writeProblem(")) continue;
    for (const status of blocked5xxStatuses) {
      if (!line.includes(status)) continue;
      failures.push(
        `${relativePath}:${i + 1} writeProblem with http.${status} — use writeInternalError(w, r, err, summary) instead. See docs/quality-timeframes.md and glimmung#514.`,
      );
    }
  }
}

if (failures.length > 0) {
  console.error("Retired handler 5xx swallow pattern detected:");
  for (const failure of failures) console.error(`- ${failure}`);
  process.exit(1);
}

console.log("No retired handler 5xx swallow patterns found.");

async function* walk(dir) {
  const entries = await fs.readdir(dir, { withFileTypes: true });
  for (const entry of entries) {
    const absolutePath = path.join(dir, entry.name);
    if (entry.isDirectory()) {
      if (!ignoredDirs.has(entry.name)) yield* walk(absolutePath);
      continue;
    }
    if (!entry.isFile()) continue;
    yield absolutePath;
  }
}

function toRepoPath(filePath) {
  return path.relative(repoRoot, filePath).split(path.sep).join("/");
}
