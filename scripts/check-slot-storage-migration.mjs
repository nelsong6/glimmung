#!/usr/bin/env node

// Migration guard for the slot-storage rework (PR #518) cutover.
//
// Background: PR #518 moved per-slot durable state out of
// `project.metadata.native_standby_dns.slots[]` (embedded array on the
// project doc) into a dedicated `slots` Cosmos container fronted by the
// `SlotStore` interface. Slot history moved to a parallel `slot_history`
// container. PR #518 also renamed the state vocabulary:
//
//   warming  -> provisioning
//   ready    -> provisioned
//   active   -> running
//
// PR #518 landed the WRITE side and added a one-shot boot migration that
// copies the legacy array into the new collection and then strips the
// array. The READ side wasn't fully migrated; ~8 production read sites
// continued to dereference `standbyDNS["slots"]` after the migration ran,
// silently returning empty data — including `nativeReadySlots` (which
// gates `/v1/test-slots/checkout` and was producing live 503s) and the
// boot recovery sweep (which silently became a no-op).
//
// This guard enforces the rest of the cutover per docs/migration-policy.md
// ("Compatibility is prohibited", "no fallback defaults", "no runtime
// reads whose purpose is to keep old behavior working", and
// docs/quality-timeframes.md "Unacceptable chunking: Keeping fallback
// reads or old routes as a safety blanket after a migration").
//
// Forbidden patterns may NOT appear in production code (the migration
// itself is the one canonical exception; the chart files describe the
// project doc shape for tofu and are also exempt).
//
// Required patterns MUST appear in named anchor files. Missing an anchor
// is a regression — e.g., the canonical SlotStore-backed reader has been
// renamed or deleted without a replacement.

import fs from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");

const ignoredDirs = new Set([
  ".claude",
  ".git",
  ".terraform",
  ".vite",
  ".next",
  ".venv",
  "__pycache__",
  "build",
  "coverage",
  "dist",
  "node_modules",
  "target",
  "venv",
]);

const ignoredFiles = new Set([
  "package-lock.json",
  "pnpm-lock.yaml",
  "yarn.lock",
  "go.sum",
]);

// Paths that legitimately reference the legacy shape:
//
//  - This guard script itself (it names the forbidden patterns).
//  - `slot_migration.go` is the one-shot boot migrator: reading the
//    legacy embedded array is its job (copy it into the new collection
//    then strip it).
//  - `slot_migration_test.go` tests the migrator end-to-end on legacy
//    input.
//  - `retired_symbols_test.go` is the symbol-inventory test that keeps
//    retired names from re-entering the codebase; naming them is its job.
//  - `cmd/glimmung-go/main.go` wires the migrator at boot and references
//    the legacy shape by name in its log line.
//  - `docs/test-slot-lifecycle.md` documents the cutover and names the
//    retired shape by spelling it out.
//  - Each entry below is a hole in the guard; adding new entries should
//    require explicit justification.
const ignoredRelativePaths = new Set([
  "scripts/check-slot-storage-migration.mjs",
  "internal/server/slot_migration.go",
  "internal/server/slot_migration_test.go",
  "internal/server/retired_symbols_test.go",
  // cosmos-side migration: deletes the legacy embedded array. Lives in
  // its own file precisely so the guard can exempt it; this is the one
  // production code path allowed to name `native_standby_dns.slots[]`.
  "internal/store/cosmos/slot_migration.go",
  "cmd/glimmung-go/main.go",
  "docs/test-slot-lifecycle.md",
  "README.md",
]);

// Forbidden: must NOT appear in any non-excluded file. Each entry is a
// retired surface that came back, or would, with a one-line explanation
// of why it's a deletion target.
const forbidden = [
  // --- Retired embedded-array reads --------------------------------------
  //
  // Reads of `standbyDNS["slots"]` (the embedded per-project slot array)
  // post-migration always return empty because the boot sweep strips
  // them. Any reader of this path is doing nothing useful AND lying to
  // the caller about it.
  {
    name: "removed read of legacy native_standby_dns.slots[] (use SlotStore.ListSlotsByProject)",
    // Matches `standbyDNS["slots"]` and `standby["slots"]` as map indices
    // (both common locals). Does not flag the literal string in a write
    // context that deletes the field — those are caught separately.
    pattern: /\b(?:standby(?:DNS)?)\s*\[\s*["']slots["']\s*\]/,
  },
  {
    name: "removed read of project.metadata.native_standby_dns.slots[] via mapSliceFromAnySlice",
    pattern: /mapSliceFromAnySlice\(anySlice\(standby(?:DNS)?\["slots"\]\)\)/,
  },

  // --- Retired state-name constants --------------------------------------
  //
  // PR #518 renamed `warming` -> `provisioning`, `ready` -> `provisioned`,
  // `active` -> `running` on the durable wire. The legacy aliases stopped
  // matching any post-migration row; production code comparing against
  // them silently fell through. Use the canonical SlotState* constants
  // defined in `internal/server/slot.go`.
  {
    name: "removed retired state alias testSlotStateActive (use SlotStateRunning)",
    pattern: /\btestSlotStateActive\b/,
  },
  {
    name: "removed retired state alias testSlotStateReady (use SlotStateProvisioned)",
    pattern: /\btestSlotStateReady\b/,
  },
  {
    name: "removed retired state alias testSlotStateWarming (use SlotStateProvisioning)",
    pattern: /\btestSlotStateWarming\b/,
  },
  {
    name: "removed retired state alias testSlotStateActivating (use SlotStateActivating)",
    pattern: /\btestSlotStateActivating\b/,
  },
  {
    name: "removed retired state alias testSlotStateCleaning (use SlotStateCleaning)",
    pattern: /\btestSlotStateCleaning\b/,
  },

  // --- Retired legacy-shape reader functions ------------------------------
  {
    name: "removed legacy embedded-array reader testEnvironmentSlotStatuses",
    pattern: /\btestEnvironmentSlotStatuses\b/,
  },
  {
    name: "removed legacy embedded-array reader testEnvironmentSlotStatus (singular)",
    // Match only the function name, not the type name TestEnvironmentSlotStatus
    pattern: /\btestEnvironmentSlotStatus\s*\(/,
  },
  {
    name: "removed legacy embedded-array reader testEnvironmentSlotStates",
    pattern: /\btestEnvironmentSlotStates\b/,
  },
  {
    name: "removed legacy embedded-array reader testEnvironmentSlotState (singular)",
    pattern: /\btestEnvironmentSlotState\s*\(/,
  },
  {
    name: "removed legacy scale-down reader testEnvironmentSlotsAboveCount",
    pattern: /\btestEnvironmentSlotsAboveCount\b/,
  },
  {
    name: "removed legacy/new union helper mergeRemovedSlots",
    pattern: /\bmergeRemovedSlots\b/,
  },

  // --- Retired legacy-shape writer helpers --------------------------------
  {
    name: "removed legacy slot-status writer setProjectTestEnvironmentSlotStatus",
    pattern: /\bsetProjectTestEnvironmentSlotStatus\b/,
  },
  {
    name: "removed legacy slot-status doc builder projectTestEnvironmentSlotStatusMap",
    pattern: /\bprojectTestEnvironmentSlotStatusMap\b/,
  },
  {
    name: "removed legacy slot-array prune helper pruneProjectTestEnvironmentSlots",
    pattern: /\bpruneProjectTestEnvironmentSlots\b/,
  },
];

// Required: each entry names an anchor file (relative repo path) and a
// pattern that MUST appear in it. Missing an anchor is a regression —
// e.g., the canonical SlotStore-backed reader has been deleted.
const required = [
  // The canonical SlotStore-backed reader: returns a per-slot status map
  // built from the new `slots` Cosmos container.
  {
    file: "internal/server/state_api.go",
    name: "slotStatusesFromStore is the canonical SlotStore-backed status reader",
    pattern: /func\s+slotStatusesFromStore\s*\(/,
  },
  {
    file: "internal/server/state_api.go",
    name: "slotStatusFromStore is the canonical single-slot reader",
    pattern: /func\s+slotStatusFromStore\s*\(/,
  },

  // nativeReadySlots gates `/v1/test-slots/checkout`. It must read from
  // the SlotStore-backed `ListSlotsByProject`, not the legacy embedded
  // array. The 503 outage that motivated this guard was nativeReadySlots
  // reading the stripped legacy array.
  {
    file: "internal/store/cosmos/cosmos.go",
    name: "nativeReadySlots reads from the SlotStore (ListSlotsByProject)",
    // Anchor on the function name + a ListSlotsByProject call within a
    // reasonable window of it (the function body is ~30 lines).
    pattern: /func\s+\(\s*s\s+\*Store\s*\)\s+nativeReadySlots[\s\S]{0,1500}ListSlotsByProject/,
  },

  // The migration policy doc must remain present and binding.
  {
    file: "docs/migration-policy.md",
    name: "migration-policy.md still states compatibility is prohibited",
    pattern: /[Cc]ompatibility is prohibited/,
  },
];

const failures = [];

for await (const filePath of walk(repoRoot)) {
  const relativePath = toRepoPath(filePath);
  if (ignoredRelativePaths.has(relativePath)) continue;
  const bytes = await fs.readFile(filePath);
  if (bytes.includes(0)) continue;
  const text = bytes.toString("utf8");
  for (const rule of forbidden) {
    const match = rule.pattern.exec(text);
    if (!match) continue;
    const { line, column } = lineAndColumn(text, match.index);
    failures.push(
      `FORBIDDEN  ${relativePath}:${line}:${column} ${rule.name}: ${JSON.stringify(match[0])}`,
    );
  }
}

for (const rule of required) {
  const absolutePath = path.join(repoRoot, rule.file);
  let text;
  try {
    text = await fs.readFile(absolutePath, "utf8");
  } catch (err) {
    if (err && err.code === "ENOENT") {
      failures.push(
        `REQUIRED   ${rule.file}: anchor file missing (cannot verify "${rule.name}")`,
      );
      continue;
    }
    throw err;
  }
  if (!rule.pattern.test(text)) {
    failures.push(`REQUIRED   ${rule.file}: missing "${rule.name}" (pattern ${rule.pattern})`);
  }
}

if (failures.length > 0) {
  console.error("Slot-storage migration guard failed:");
  for (const failure of failures) console.error(`- ${failure}`);
  console.error("");
  console.error(
    "Each FORBIDDEN entry above is a retired surface that came back; each REQUIRED entry",
  );
  console.error(
    "is a piece of the cutover that's missing. See scripts/check-slot-storage-migration.mjs",
  );
  console.error("for the rationale per rule, and docs/migration-policy.md for the policy.");
  process.exit(1);
}

console.log("Slot-storage migration guard passed.");

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
