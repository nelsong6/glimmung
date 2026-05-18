#!/usr/bin/env node
// Migration guard for the deliberate-503 observability contract.
//
// writeProblem(w, http.StatusServiceUnavailable, ...) is the right shape
// for configuration-absence 503s — the "X store not configured" pattern
// that fires when a dependency is not wired at boot. Those have no
// operational signal worth logging.
//
// writeUnavailable(w, r, summary, reason) is the right shape for
// runtime / operational 503s — saturation, retryable transient
// unavailability. It emits slog.Warn with route/summary/reason and
// increments glimmung_unavailable_total{route,reason}, so the event
// surfaces on a dashboard. Test-slot saturation
// ("no ready test environment slots available") is the canonical
// example and the only operational 503 in the codebase today.
//
// This script fails CI if a writeProblem call passes
// http.StatusServiceUnavailable with a literal that does not match the
// "not configured" pattern. New operational 503s must use
// writeUnavailable. Callsites that pass an error string through
// (e.g. err.Error()) must be listed in allowDynamicMessage; the
// allowlist starts with the one existing such site and any addition
// requires reviewer acknowledgement that the upstream err carries the
// "not configured" semantics.

import fs from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const serverDir = path.join(repoRoot, "internal/server");

// Files that document the patterns in prose (the helpers themselves
// and this script) are skipped.
const ignoredRelativePaths = new Set([
  "internal/server/read_api.go", // declares writeProblem and writeUnavailable
  "scripts/check-503-observability.mjs",
]);

// Callsites where writeProblem-503 receives a dynamic argument rather
// than a literal "X not configured" string. These pass an upstream
// err.Error() that is itself a "not configured" message. Adding to
// this list requires reviewing the upstream return to confirm the
// shape.
const allowDynamicMessage = new Set([
  // internal/server/signal_drain.go: DrainSignals returns
  // errors.New("signal drain store not configured") on the only path
  // that produces a 503; the err.Error() is a pass-through.
  "internal/server/signal_drain.go:writeProblem(w, http.StatusServiceUnavailable, err.Error())",
]);

const NOT_CONFIGURED = /not configured/i;

const failures = [];

for await (const filePath of walk(serverDir)) {
  const relativePath = toRepoPath(filePath);
  if (ignoredRelativePaths.has(relativePath)) continue;
  if (!filePath.endsWith(".go")) continue;
  const bytes = await fs.readFile(filePath);
  if (bytes.includes(0)) continue;
  const text = bytes.toString("utf8");
  const lines = text.split(/\r\n|\r|\n/);
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i];
    const trimmed = line.trim();
    if (trimmed.startsWith("//")) continue;
    if (!line.includes("writeProblem(")) continue;
    if (!line.includes("StatusServiceUnavailable")) continue;
    // Extract the message argument. We match either a quoted literal
    // or a bare identifier expression; the regex is intentionally
    // lenient because Go formatting may or may not break args across
    // lines.
    const literalMatch = line.match(/writeProblem\([^,]+,\s*http\.StatusServiceUnavailable,\s*"([^"]*)"\s*\)/);
    if (literalMatch) {
      if (NOT_CONFIGURED.test(literalMatch[1])) continue;
      failures.push(
        `${relativePath}:${i + 1} writeProblem 503 with literal ${JSON.stringify(literalMatch[1])} — operational 503s must use writeUnavailable(w, r, summary, reason). See docs/quality-timeframes.md and the writeProblem comment in internal/server/read_api.go.`,
      );
      continue;
    }
    // Dynamic argument (e.g. err.Error()). The greedy capture +
    // trailing `\)\s*$` lets the closing paren count come out right
    // even for arguments that contain their own parens.
    const dynamicMatch = line.match(/writeProblem\([^,]+,\s*http\.StatusServiceUnavailable,\s*(.+)\)\s*$/);
    if (dynamicMatch) {
      const key = `${relativePath}:writeProblem(w, http.StatusServiceUnavailable, ${dynamicMatch[1].trim()})`;
      if (allowDynamicMessage.has(key)) continue;
      failures.push(
        `${relativePath}:${i + 1} writeProblem 503 with dynamic argument (${dynamicMatch[1].trim()}) — must be in allowDynamicMessage with a comment explaining the upstream "not configured" guarantee, or migrated to writeUnavailable.`,
      );
      continue;
    }
    failures.push(
      `${relativePath}:${i + 1} writeProblem 503 in an unrecognized shape — refactor or migrate to writeUnavailable.`,
    );
  }
}

if (failures.length > 0) {
  console.error("Deliberate 503 observability contract violations:");
  for (const failure of failures) console.error(`- ${failure}`);
  process.exit(1);
}

console.log("No retired 503 observability patterns found.");

async function* walk(dir) {
  const entries = await fs.readdir(dir, { withFileTypes: true });
  for (const entry of entries) {
    const absolutePath = path.join(dir, entry.name);
    if (entry.isDirectory()) {
      yield* walk(absolutePath);
      continue;
    }
    if (!entry.isFile()) continue;
    yield absolutePath;
  }
}

function toRepoPath(filePath) {
  return path.relative(repoRoot, filePath).split(path.sep).join("/");
}
