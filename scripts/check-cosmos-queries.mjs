#!/usr/bin/env node
// Migration guard for the Cosmos query contract.
//
// Per docs/migration-policy.md ("compatibility is prohibited") and
// docs/cosmos-partition-strategy.md: every Cosmos query in
// internal/store/cosmos/ must go through one of the three primitives
// declared in query.go — singlePartitionQuery, crossPartitionQuery, or
// fanOutByProject. The retired path was a pair of helpers
// (queryAll / queryAllWhere) that defaulted to an empty partition key,
// which silently turned every query into a cross-partition scan. With
// ORDER BY / DISTINCT / GROUP BY / OFFSET / TOP that shape required the
// client-side query-plan handshake the Azure Go SDK does not implement,
// and the Cosmos gateway returned a 400 that surfaced as a 5xx in the
// handler layer (see glimmung's `GET /v1/touchpoints` once-per-minute
// 5xx incident).
//
// This script fails CI if:
//   1. The retired helpers (queryAll / queryAllWhere) reappear anywhere.
//   2. A bare azcosmos.NewPartitionKey() call appears outside query.go.
//   3. A direct NewQueryItemsPager call appears outside query.go,
//      slot.go, or slot_history.go (the two slot files use the pager
//      directly with explicit partition keys for the slot allocation
//      flow; they predate this guard and are inspected by reviewers).

import fs from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");

const cosmosDir = path.join(repoRoot, "internal/store/cosmos");

// query.go is allowed to reference the retired surface in prose
// (doc-comments motivating the new primitives) and is the only file
// permitted to call NewPartitionKey() or NewQueryItemsPager directly.
const allowEmptyPK = new Set([
  "internal/store/cosmos/query.go",
]);

// slot.go and slot_history.go use NewQueryItemsPager directly with
// explicit, project-scoped partition keys for the slot allocation flow.
// They predate this guard. New direct pager calls must instead go
// through query.go.
const allowDirectPager = new Set([
  "internal/store/cosmos/query.go",
  "internal/store/cosmos/slot.go",
  "internal/store/cosmos/slot_history.go",
]);

// Files allowed to mention the retired symbol names in prose (doc
// comments, migration notes, or this script's own description).
const allowRetiredSymbolMentions = new Set([
  "internal/store/cosmos/cosmos.go", // comment-only stub explaining the deletion
  "internal/store/cosmos/query.go",  // doc-comment motivation
  "scripts/check-cosmos-queries.mjs",
  "docs/cosmos-partition-strategy.md",
]);

const failures = [];

for await (const filePath of walk(cosmosDir)) {
  const relativePath = toRepoPath(filePath);
  if (!filePath.endsWith(".go")) continue;
  const bytes = await fs.readFile(filePath);
  if (bytes.includes(0)) continue;
  const text = bytes.toString("utf8");
  const lines = text.split(/\r\n|\r|\n/);
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i];
    const trimmed = line.trim();
    // Skip line comments — the retired symbols are deliberately
    // mentioned in prose in a few places.
    const isComment = trimmed.startsWith("//");

    if (!isComment && /\bqueryAll\s*\(/.test(line)) {
      failures.push(
        `${relativePath}:${i + 1} retired queryAll(...) — use singlePartitionQuery / crossPartitionQuery / fanOutByProject (see docs/cosmos-partition-strategy.md).`,
      );
    }
    if (!isComment && /\bqueryAllWhere\s*\(/.test(line)) {
      failures.push(
        `${relativePath}:${i + 1} retired queryAllWhere(...) — use singlePartitionQuery / crossPartitionQuery / fanOutByProject (see docs/cosmos-partition-strategy.md).`,
      );
    }
    if (
      !allowEmptyPK.has(relativePath) &&
      /azcosmos\.NewPartitionKey\s*\(\s*\)/.test(line) &&
      !isComment
    ) {
      failures.push(
        `${relativePath}:${i + 1} empty azcosmos.NewPartitionKey() — pass a real partition key value via NewPartitionKeyString/Bool/Number, or route through crossPartitionQuery in query.go.`,
      );
    }
    if (
      !allowDirectPager.has(relativePath) &&
      /\.NewQueryItemsPager\s*\(/.test(line) &&
      !isComment
    ) {
      failures.push(
        `${relativePath}:${i + 1} direct NewQueryItemsPager — call the helpers in query.go instead so partition strategy is explicit.`,
      );
    }
  }
}

// Also scan the prose-permitted files only to verify the symbol mention
// rule is consistent — purely informational, never fails.
const proseMentions = [];
for (const rel of allowRetiredSymbolMentions) {
  const abs = path.join(repoRoot, rel);
  try {
    const text = await fs.readFile(abs, "utf8");
    if (/queryAll(Where)?\b/.test(text)) {
      proseMentions.push(rel);
    }
  } catch {
    // Allowed file may not exist (e.g. doc not yet created) — ignore.
  }
}

if (failures.length > 0) {
  console.error("Retired Cosmos query patterns detected:");
  for (const failure of failures) console.error(`- ${failure}`);
  process.exit(1);
}

console.log(
  `No retired Cosmos query patterns found. (Prose mentions in: ${proseMentions.join(", ") || "none"}.)`,
);

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
