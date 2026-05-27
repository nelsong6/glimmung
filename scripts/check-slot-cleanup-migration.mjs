#!/usr/bin/env node

// Migration guard for the slot-cleanup compat-layer deletion.
//
// CONTEXT
//
// PR #518 split slot state into its own durable collection/table. PR #525 added
// the cancel-await activation contract + error→cleaning recovery. The
// embedded `project.metadata.native_standby_dns.slots[]` array became a
// dead field, stripped at boot by MigrateProjectSlotsIntoCollection.
//
// Five compat layers survived that cutover. Per tank-operator
// docs/migration-policy.md ("Treat legacy, compatibility, fallback,
// temporary, exception as deletion targets, not design options") and
// docs/quality-timeframes.md ("Migration guards prevent old paths from
// returning"), they must be deleted end-to-end:
//
//   A. Empty-array read fallback in recoveredSlotStatus and the same
//      pattern in state_api.go's slot-state rendering. The legacy
//      testEnvironmentSlotStatus/Statuses readers read from the stripped
//      array; their only excuse is unmigrated test fixtures. Tests must
//      migrate to a typed seeder; production callers must go.
//
//   B. PATCH-count's writeback to standbyDNS["slots"] in
//      the active store layer. Dual-write to a dead field that
//      the next boot strips. Pure violation.
//
//   C. Legacy state-name aliases in switch arms (e.g.,
//      `case SlotStateRunning, testSlotStateActive`). MigrateLegacyState
//      translates "active" → "running" at boot, so no slot row carries
//      the alias post-migration. The case arm catches data that cannot
//      exist.
//
//   D. (NOT deleted by this guard.) Dashboard wire vocabulary
//      ("active"/"ready") in state_api.go derived* functions is the one
//      legitimate compat — downstream SPA/mcp-glimmung consume those
//      names. Renaming on the wire is a separate frontend migration.
//
//   E. Dead writer helpers: pruneProjectTestEnvironmentSlots,
//      setProjectTestEnvironmentSlotStatus,
//      projectTestEnvironmentSlotStatusMap. Zero production callers
//      after A/B are deleted.
//
// PR #525's load-bearing additions are also guarded so a future agent
// can't revert them silently: cancel-await activation contract,
// error→cleaning transition, CAS-routing of cleanup-entry paths,
// lifecycle-doc cancel-await section.
//
// USAGE
//
//   node scripts/check-slot-cleanup-migration.mjs
//
// Exits 0 only when both FORBIDDEN (none-match) and REQUIRED (all-match)
// rules pass. On a non-migrated tree the failures form the cutover
// punch list.

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
]);

// Path-relative excludes that intentionally hold references we don't
// want to ban globally. Each entry needs an explicit reason.
const ignoredRelativePaths = new Set([
  // This script's own description names every retired symbol.
  "scripts/check-slot-cleanup-migration.mjs",
  // The state-machine source is allowed to mention SlotStateError /
  // SlotStateRunning in the validSlotTransitions table itself.
  // (Specific rules below scope themselves more narrowly than the
  // file-level exclude where needed.)
]);

// FORBIDDEN: any match anywhere outside the ignored paths is a failure.
//
// `restrictToGlob` (optional) narrows the walk. Use it to keep the
// SlotStateRunning / testSlotStateActive alias check from false-
// positiving on innocent uses of either constant in isolation — only
// the *pair* on a single line is the dead-code smell.
const forbidden = [
  // --- A. legacy embedded-array readers ---
  {
    id: "legacy-status-reader-call",
    name: "testEnvironmentSlotStatus(es) called from production Go (use SlotStore)",
    // Match the call form `testEnvironmentSlotStatus(` or
    // `testEnvironmentSlotStatuses(` but NOT the function definition
    // (`func testEnvironmentSlotStatus(`). The function bodies live in
    // state_api.go and stay deletable as a unit; this rule's purpose
    // is to surface the *call sites* that have to migrate first.
    pattern: /(?<!func\s)testEnvironmentSlotStatus(?:es)?\s*\(/,
    onlyExtensions: [".go"],
    excludeRelative: (p) => p.endsWith("_test.go"),
  },
  {
    id: "legacy-status-reader-def",
    name: "testEnvironmentSlotStatus(es) function definition (delete with callers)",
    pattern: /func\s+testEnvironmentSlotStatus(?:es)?\s*\(/,
    onlyExtensions: [".go"],
  },

  // --- B. PATCH-count writeback to the stripped legacy array ---
  {
    id: "patch-count-writes-legacy-slots",
    name: 'standbyDNS["slots"] = ... (dual-write to a stripped field)',
    // Catches the assignment on the LHS; survives whitespace and the
    // pruneProjectTestEnvironmentSlots helper being renamed.
    pattern: /standbyDNS\s*\[\s*"slots"\s*\]\s*=/,
    onlyExtensions: [".go"],
  },

  // --- C. legacy state-name alias case arms ---
  {
    id: "alias-case-arm-running-active",
    name: 'case SlotStateRunning, testSlotStateActive (dead alias for migrated state)',
    pattern: /case\s+(?:SlotStateRunning\s*,\s*testSlotStateActive|testSlotStateActive\s*,\s*SlotStateRunning)\b/,
    onlyExtensions: [".go"],
  },
  {
    id: "alias-case-arm-cleaning",
    name: 'case SlotStateCleaning, testSlotStateCleaning (same string; redundant alias)',
    pattern: /case\s+(?:SlotStateCleaning\s*,\s*testSlotStateCleaning|testSlotStateCleaning\s*,\s*SlotStateCleaning)\b/,
    onlyExtensions: [".go"],
  },
  {
    id: "alias-case-arm-activating",
    name: 'case SlotStateActivating, testSlotStateActivating (same string; redundant alias)',
    pattern: /case\s+(?:SlotStateActivating\s*,\s*testSlotStateActivating|testSlotStateActivating\s*,\s*SlotStateActivating)\b/,
    onlyExtensions: [".go"],
  },

  // --- E. dead writer helpers (zero production callers) ---
  {
    id: "dead-helper-prune",
    name: "pruneProjectTestEnvironmentSlots (legacy embedded-array writer)",
    pattern: /\bpruneProjectTestEnvironmentSlots\b/,
    onlyExtensions: [".go"],
  },
  {
    id: "dead-helper-set-status",
    name: "setProjectTestEnvironmentSlotStatus (legacy embedded-array writer)",
    pattern: /\bsetProjectTestEnvironmentSlotStatus\b/,
    onlyExtensions: [".go"],
  },
  {
    id: "dead-helper-status-map",
    name: "projectTestEnvironmentSlotStatusMap (legacy embedded-array writer)",
    pattern: /\bprojectTestEnvironmentSlotStatusMap\b/,
    onlyExtensions: [".go"],
  },

  // --- PR #525 anti-revert ---
  {
    id: "activation-token-struct-empty-marker",
    name: 'testSlotActivations.LoadOrStore(key, struct{}{}) — race-fix revert',
    // Exact reverse of the cancel-token wiring. struct{}{} as the
    // marker is what PR #525 replaced with *testSlotActivation; a
    // revert would re-introduce this literal.
    pattern: /testSlotActivations\s*\.\s*LoadOrStore\s*\([^)]*,\s*struct\s*\{\s*\}\s*\{\s*\}\s*\)/,
    onlyExtensions: [".go"],
  },
];

// REQUIRED: each entry names a file and a pattern that MUST appear in
// it. Missing file or missing pattern is a failure. Used for positive
// load-bearing assertions that a revert would silently undo.
const required = [
  // --- PR #525 race fix ---
  {
    id: "cancel-token-type",
    file: "internal/server/test_slot_api.go",
    name: "testSlotActivation struct with cancel + done (cancel-token map value)",
    pattern: /type\s+testSlotActivation\s+struct\s*\{[\s\S]{0,200}cancel\s+context\.CancelFunc[\s\S]{0,200}done\s+chan\s+struct\s*\{\s*\}/,
  },
  {
    id: "cancel-helper",
    file: "internal/server/test_slot_api.go",
    name: "cancelInflightActivation helper",
    pattern: /func\s+cancelInflightActivation\s*\(/,
  },
  {
    id: "cleanup-calls-cancel",
    file: "internal/server/test_slot_api.go",
    name: "beginTestSlotCleanup invokes cancelInflightActivation before deletes",
    // Window: the cancel call must appear inside beginTestSlotCleanup
    // (between its opening brace and the call to cleanupTestSlotRuntime).
    pattern: /func\s+beginTestSlotCleanup[\s\S]{0,4000}cancelInflightActivation\s*\([\s\S]{0,200}cleanupTestSlotRuntime/,
  },

  // --- PR #525 error→cleaning transition ---
  {
    id: "error-to-cleaning-transition",
    file: "internal/server/slot.go",
    name: "validSlotTransitions[SlotStateError] allows SlotStateCleaning",
    // SlotStateError-as-map-key, followed (within ~1500 chars to cover
    // explanatory comments) by SlotStateCleaning: true. The map-key
    // anchor (preceding comma+newline+optional whitespace) keeps this
    // from false-positiving on unrelated mentions of either constant.
    pattern: /,\s*\n\s*SlotStateError\s*:\s*\{[\s\S]{0,1500}SlotStateCleaning\s*:\s*true/,
  },

  // --- PR #525 CAS routing of cleanup-entry paths ---
  {
    id: "return-goes-through-claim",
    file: "internal/server/test_slot_api.go",
    name: "returnTestSlot routes through claimTestSlotCleanup (atomic claim)",
    pattern: /func\s+returnTestSlot[\s\S]{0,4000}claimTestSlotCleanup\s*\(/,
  },
  {
    id: "callback-goes-through-claim",
    file: "internal/server/lease_callback_api.go",
    name: "releaseTestSlotLeaseByCallback routes through claimTestSlotCleanup",
    pattern: /func\s+releaseTestSlotLeaseByCallback[\s\S]{0,4000}claimTestSlotCleanup\s*\(/,
  },

  // --- PR #525 native_launcher reorder ---
  {
    id: "return-deletes-installer-first",
    file: "internal/server/native_launcher.go",
    name: "ReturnTestSlotRuntime deletes installer Job before slot-namespace resources",
    // The installer Job is the durable K8s-side producer of slot-namespace
    // workloads (via helm-install). It must be deleted before
    // deletePlaywrightResources / deleteTestSlotRuntimeResources, otherwise
    // the racing helm tail recreates resources we just deleted and
    // waitForNoPodsInNamespaces times out (the bug PR #525 fixed).
    pattern: /func\s+\([^)]+\)\s+ReturnTestSlotRuntime\s*\([\s\S]{0,3500}deleteTestSlotInstaller\s*\(\s*ctx\s*,\s*lease\s*\)[\s\S]{0,3500}deletePlaywrightResources/,
  },
  {
    id: "wait-installer-pods-helper",
    file: "internal/server/native_launcher.go",
    name: "waitForInstallerPodsTerminated helper (installer-pod drain between Job delete and runtime delete)",
    pattern: /func\s+\([^)]+\)\s+waitForInstallerPodsTerminated\s*\(/,
  },

  // --- PR #525 observability ---
  {
    id: "activation-cancelled-metric",
    file: "internal/metrics/metrics.go",
    name: "glimmung_test_slot_activation_cancelled_total counter",
    pattern: /glimmung_test_slot_activation_cancelled_total/,
  },
  {
    id: "cleanup-claim-metric",
    file: "internal/metrics/metrics.go",
    name: "glimmung_test_slot_cleanup_claim_total counter",
    pattern: /glimmung_test_slot_cleanup_claim_total/,
  },

  // --- PR #525 doc ---
  {
    id: "lifecycle-doc-cancel-await",
    file: "docs/test-slot-lifecycle.md",
    name: 'docs describe "Cleanup interrupts activation" contract',
    // Section header + the load-bearing phrase. Tolerates rephrasing
    // by accepting either header wording but requiring the cancel-await
    // mechanic by name.
    pattern: /Cleanup\s+interrupts\s+activation[\s\S]{0,2000}(cancel[s\-]?await|cancel[ -]+and[ -]+await)/i,
  },
  {
    id: "lifecycle-doc-error-recovery",
    file: "docs/test-slot-lifecycle.md",
    name: "docs describe error→cleaning recovery transition",
    // "Error recovery" section header + the load-bearing mechanic.
    // Tolerates rephrasing of the prose but requires both pieces.
    pattern: /Error\s+recovery[\s\S]{0,3000}validSlotTransitions\s*\[\s*SlotStateError\s*\][\s\S]{0,500}SlotStateCleaning/i,
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
    if (rule.onlyExtensions && !rule.onlyExtensions.some((ext) => relativePath.endsWith(ext))) continue;
    if (rule.excludeRelative && rule.excludeRelative(relativePath)) continue;
    // Report every match so a multi-callsite cleanup gets a complete
    // punch list, not just the first hit. Build a global regex from
    // the rule's source so per-rule literal regexes don't need /g.
    const globalRe = new RegExp(rule.pattern.source, rule.pattern.flags.includes("g") ? rule.pattern.flags : rule.pattern.flags + "g");
    for (const match of text.matchAll(globalRe)) {
      const { line, column } = lineAndColumn(text, match.index);
      failures.push(`FORBIDDEN ${relativePath}:${line}:${column} ${rule.name}`);
    }
  }
}

for (const rule of required) {
  let text;
  try {
    text = await fs.readFile(path.join(repoRoot, rule.file), "utf8");
  } catch (err) {
    failures.push(`REQUIRED  ${rule.file}: MISSING FILE (${rule.name})`);
    continue;
  }
  if (!rule.pattern.test(text)) {
    failures.push(`REQUIRED  ${rule.file}: pattern not found — ${rule.name}`);
  }
}

if (failures.length > 0) {
  console.error("Slot-cleanup migration guard failed:");
  for (const failure of failures) console.error(`- ${failure}`);
  console.error(`\n${failures.length} item(s). See header of this script for the migration context.`);
  process.exit(1);
}

console.log("Slot-cleanup migration guard passed.");

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
