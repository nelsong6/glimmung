#!/usr/bin/env node

// Completion manifest for the apply_test_slot_hot_swap migration.
//
// This script is the spec. "Done" = exit 0. Same workflow as
// tank-operator's scripts/check-stop-request-migration.mjs and
// scripts/check-session-pod-hot-swap-migration.mjs: committed as commit 1
// of the branch, before any feature code, so the contract is auditable
// independently of whatever the agent later writes.
//
// CONTEXT
//
// PR #494 in tank-operator landed the *mechanism* for session-pod
// agent-runner hot-swap (supervisor + writable target + SIGHUP re-exec).
// It surfaced that test-slot hot-swap as a whole is half-finished: glimmung
// exposes get_test_slot_hot_swap_contract and record_test_slot_hot_swap MCP
// tools, but no apply tool — every developer (or AI agent) ends up running
// kubectl-fu by hand. The /test skill literally documents the manual
// pattern. Per docs/quality-timeframes.md "prefer complete architecture
// over quick relief", this is the missing half.
//
// This PR closes the gap: a single HTTP endpoint that takes a git ref +
// a slot identifier and does end-to-end build-and-swap. The endpoint
// dispatches a one-off Kubernetes Job (init container for build, main
// container for kubectl-stream + signal), watches it to completion, and
// returns a structured result. Synchronous by default per the ArgoCD
// `app sync` pattern (researched against Google AIP-151 for async-only
// APIs; ArgoCD's developer-driven shape is the closer analog for our
// use case).
//
// THE CONTRACT — four user-named guarantees:
//
//   1. "Place new code, step back" works end-to-end. The new MCP tool
//      (apply_test_slot_hot_swap) takes a project, slot, artifact_kind,
//      and git_ref. Caller's only action is the call. Glimmung does
//      clone + build + copy + signal + verify + record-history.
//
//   2. Each app declares its own build environment. The contract's
//      builder_image field per artifact_kind tells glimmung which
//      container image to use for the build step (e.g., node:20-alpine
//      for agent_runner, golang:1.26-alpine for backend). No language
//      heuristics, no hardcoded defaults — the contract owns this.
//
//   3. Sync by default, ArgoCD pattern. Endpoint blocks until done or
//      timeout. Server-side timeout default ~120s, hard cap ~600s.
//      Structured response includes build-log tail, swap details,
//      health-poll result, timings. History recorded regardless of
//      outcome (durable state lives in the system, not in the request).
//
//   4. Existing automation paths are byte-identical. The
//      glimmung-agent test-slot-hot-swap subcommand stays unchanged.
//      The existing static + backend hot-swap paths in TestSlotHotSwap
//      stay unchanged. The /v1/test-slots/hot-swap-history endpoint stays
//      unchanged. The other /v1/test-slots/* endpoints stay unchanged.
//
// Skip slow exec gates during structural iteration with:
//   SKIP_EXEC=1 node scripts/check-apply-test-slot-hot-swap-migration.mjs

import { spawnSync } from "node:child_process";
import fs from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const skipExec = process.env.SKIP_EXEC === "1";

const CHECKS = [
  // ─────────────────────── Guarantee 1: end-to-end "place new code, step back" ───────────────────────

  {
    id: "endpoint-registered",
    from: "Guarantee 1: end-to-end apply",
    file: "internal/server/server.go",
    description: "POST /v1/test-slots/apply-hot-swap registered in the server mux",
    kind: "grep-present",
    pattern: /POST\s+\/v1\/test-slots\/apply-hot-swap/,
  },
  {
    id: "endpoint-handler-exists",
    from: "Guarantee 1: end-to-end apply",
    file: "internal/server/test_slot_apply_hot_swap_api.go",
    description: "applyTestSlotHotSwap handler function exists in its own file (mirrors test_slot_hot_swap_api.go shape)",
    kind: "grep-present",
    pattern: /func applyTestSlotHotSwap\(/,
  },
  {
    id: "endpoint-request-shape",
    from: "Guarantee 1: end-to-end apply",
    file: "internal/server/test_slot_apply_hot_swap_api.go",
    description: "Request struct names project, slot identifier (index or name), artifact_kind, git_ref, optional timeout_seconds",
    kind: "grep-present",
    pattern: /type\s+TestSlotApplyHotSwapRequest\s+struct[\s\S]{0,500}?Project[\s\S]{0,500}?ArtifactKind[\s\S]{0,500}?GitRef/,
  },
  {
    id: "endpoint-response-shape",
    from: "Guarantee 1: end-to-end apply",
    file: "internal/server/test_slot_apply_hot_swap_api.go",
    description: "Response struct names the build/copy/restart/health result fields the dev needs to diagnose a failure",
    kind: "grep-present",
    pattern: /type\s+TestSlotApplyHotSwapResult\s+struct/,
  },
  {
    id: "endpoint-resolves-lease",
    from: "Guarantee 1: end-to-end apply",
    file: "internal/server/test_slot_apply_hot_swap_api.go",
    description: "Handler resolves the lease via the existing resolveTestSlotLease helper (matches the record-history pattern)",
    kind: "grep-present",
    pattern: /resolveTestSlotLease/,
  },
  {
    id: "endpoint-reads-contract",
    from: "Guarantee 1: end-to-end apply",
    file: "internal/server/test_slot_apply_hot_swap_api.go",
    description: "Handler reads the project's contract via hotswap.FromMetadata (same path as the project-write validator)",
    kind: "grep-present",
    pattern: /hotswap\.FromMetadata/,
  },
  {
    id: "endpoint-dispatches-job",
    from: "Guarantee 1: end-to-end apply",
    file: "internal/server/test_slot_apply_hot_swap_api.go",
    description: "Handler dispatches the build-and-swap Job via the agentops performer (function-typed seam for testability; production wires to *Ops.ApplyHotSwap)",
    kind: "grep-present",
    pattern: /applyHotSwapPerformer|agentops\.ApplyHotSwapOptions/,
  },
  {
    id: "endpoint-records-history-always",
    from: "Guarantee 1: end-to-end apply",
    file: "internal/server/test_slot_apply_hot_swap_api.go",
    description: "Handler records hot-swap history on every outcome (success, build_failed, swap_failed, timeout) so durable state lives in the system",
    kind: "grep-present",
    pattern: /AppendTestSlotHotSwapHistory/,
  },
  {
    id: "ops-dispatcher-fn",
    from: "Guarantee 1: end-to-end apply",
    file: "internal/server/test_slot_apply_hot_swap_ops.go",
    description: "Dispatcher function ApplyHotSwap takes a k8sJobClient (no kubectl shell-out — glimmung pod has no kubectl; matches the native_launcher request() pattern)",
    kind: "grep-present",
    pattern: /func\s+ApplyHotSwap\([\s\S]{0,200}?k8sJobClient/,
  },
  {
    id: "ops-dispatcher-job-spec",
    from: "Guarantee 1: end-to-end apply",
    file: "internal/server/test_slot_apply_hot_swap_ops.go",
    description: "Job spec uses init container for build, main container for swap (sequential via initContainers)",
    kind: "grep-present",
    pattern: /initContainers|InitContainers/,
  },
  {
    id: "ops-dispatcher-watches-job",
    from: "Guarantee 1: end-to-end apply",
    file: "internal/server/test_slot_apply_hot_swap_ops.go",
    description: "Dispatcher polls Job's status.conditions via the k8s HTTP API (Complete/Failed types), not by kubectl-wait. Bounded by Timeout.",
    kind: "grep-present",
    pattern: /WaitForJob[\s\S]{0,2000}?conditions|Complete[\s\S]{0,200}?Failed/,
  },

  // ─────────────────────── Guarantee 2: each app declares its own builder ───────────────────────

  {
    id: "contract-agent-runner-struct",
    from: "Guarantee 2: per-app builder",
    file: "internal/domain/hotswap/hotswap.go",
    description: "AgentRunnerContract struct exists alongside StaticContract and BackendContract",
    kind: "grep-present",
    pattern: /type\s+AgentRunnerContract\s+struct/,
  },
  {
    id: "contract-agent-runner-on-contract",
    from: "Guarantee 2: per-app builder",
    file: "internal/domain/hotswap/hotswap.go",
    description: "Contract gains an AgentRunner field of type AgentRunnerContract",
    kind: "grep-present",
    pattern: /type\s+Contract\s+struct[\s\S]{0,400}?AgentRunner\s+AgentRunnerContract/,
  },
  {
    id: "contract-builder-image-backend",
    from: "Guarantee 2: per-app builder",
    file: "internal/domain/hotswap/hotswap.go",
    description: "BackendContract has BuilderImage field (per-app build environment)",
    kind: "grep-present",
    pattern: /type\s+BackendContract\s+struct[\s\S]{0,800}?BuilderImage\s+string/,
  },
  {
    id: "contract-builder-image-static",
    from: "Guarantee 2: per-app builder",
    file: "internal/domain/hotswap/hotswap.go",
    description: "StaticContract has BuilderImage field",
    kind: "grep-present",
    pattern: /type\s+StaticContract\s+struct[\s\S]{0,400}?BuilderImage\s+string/,
  },
  {
    id: "contract-builder-image-agent-runner",
    from: "Guarantee 2: per-app builder",
    file: "internal/domain/hotswap/hotswap.go",
    description: "AgentRunnerContract has BuilderImage field",
    kind: "grep-present",
    pattern: /type\s+AgentRunnerContract\s+struct[\s\S]{0,2000}?BuilderImage\s+string/,
  },
  {
    id: "contract-agent-runner-required-fields",
    from: "Guarantee 2: per-app builder",
    file: "internal/domain/hotswap/hotswap.go",
    description: "AgentRunnerContract has source/target/build_command/restart/container/pod_selector (the kubectl-orchestration inputs)",
    kind: "grep-present",
    pattern: /type\s+AgentRunnerContract\s+struct[\s\S]{0,2500}?Source[\s\S]{0,800}?Target[\s\S]{0,800}?BuildCommand[\s\S]{0,800}?PodSelector/,
  },
  {
    id: "contract-validate-agent-runner",
    from: "Guarantee 2: per-app builder",
    file: "internal/domain/hotswap/hotswap.go",
    description: "Contract.Validate enforces required AgentRunner fields when enabled (source, target, builder_image, build_command, pod_selector)",
    kind: "grep-present",
    pattern: /AgentRunner\.Enabled[\s\S]{0,2000}?BuilderImage/,
  },
  {
    id: "contract-validate-builder-image-required-agent-runner",
    from: "Guarantee 2: per-app builder",
    file: "internal/domain/hotswap/hotswap.go",
    description: "Validate rejects empty builder_image when AgentRunner is enabled (apply endpoint is the only consumer of agent_runner; no legacy CLI fallback)",
    kind: "grep-present",
    pattern: /AgentRunner\.Enabled[\s\S]{0,2500}?"builder_image"[\s\S]{0,500}?c\.AgentRunner\.BuilderImage/,
  },
  {
    id: "contract-validate-builder-image-applytime-backend",
    from: "Guarantee 2: per-app builder",
    file: "internal/server/test_slot_apply_hot_swap_api.go",
    description: "Apply endpoint rejects request when Backend artifact_kind is requested but builder_image is missing (validated at request time, not at Contract validation — keeps existing registered contracts from breaking)",
    kind: "grep-present",
    pattern: /Backend[\s\S]{0,400}?BuilderImage[\s\S]{0,400}?(?:required|missing|empty)/,
  },

  // ─────────────────────── Guarantee 3: sync UX, ArgoCD pattern, durable state ───────────────────────

  {
    id: "endpoint-default-timeout-bounded",
    from: "Guarantee 3: sync UX + durable state",
    file: "internal/server/test_slot_apply_hot_swap_api.go",
    description: "Handler enforces a server-side default timeout (~120s) when caller doesn't specify",
    kind: "grep-present",
    pattern: /(?:DefaultApplyHotSwap|defaultApplyHotSwap)?Timeout[\s\S]{0,400}?(?:120|2\*time\.Minute|2 ?\* ?Minute)/,
  },
  {
    id: "endpoint-hard-cap-timeout",
    from: "Guarantee 3: sync UX + durable state",
    file: "internal/server/test_slot_apply_hot_swap_api.go",
    description: "Handler clamps caller-provided timeout to a hard server max (~600s) to prevent dangling requests",
    kind: "grep-present",
    pattern: /(?:MaxApplyHotSwap|maxApplyHotSwap)?Timeout[\s\S]{0,400}?(?:600|10\*time\.Minute|10 ?\* ?Minute)/,
  },
  {
    id: "endpoint-blocks-on-job",
    from: "Guarantee 3: sync UX + durable state",
    file: "internal/server/test_slot_apply_hot_swap_api.go",
    description: "Handler blocks until job completion (no fire-and-forget on success path)",
    kind: "grep-present",
    pattern: /(?:DispatchApplyHotSwap|ApplyHotSwap)[\s\S]{0,400}?writeJSON\(/,
  },
  {
    id: "endpoint-history-on-failure",
    from: "Guarantee 3: sync UX + durable state",
    file: "internal/server/test_slot_apply_hot_swap_api.go",
    description: "Handler appends a hot-swap history entry with a failure-named status even when build/swap/health fails (durable failure record)",
    kind: "grep-multi-present",
    patterns: [
      /AppendTestSlotHotSwapHistory/,
      /Status:\s*status/,
      /"build_failed"|"swap_failed"|"timeout"/,
    ],
  },
  {
    id: "observability-outcome-tracked-in-result",
    from: "Guarantee 3: sync UX + durable state",
    file: "internal/server/test_slot_apply_hot_swap_ops.go",
    description: "Result struct carries a bounded Outcome field with the named failure modes (persisted | build_failed | swap_failed | timeout); these flow into the durable hot-swap history record. Prometheus counter deferred to a separate PR when glimmung gets a /metrics endpoint.",
    kind: "grep-present",
    pattern: /Outcome[\s\S]{0,400}?persisted[\s\S]{0,200}?build_failed[\s\S]{0,200}?swap_failed[\s\S]{0,200}?timeout/,
  },

  // ─────────────────────── Guarantee 4: nothing already-working is touched ───────────────────────

  {
    id: "existing-history-endpoint-unchanged",
    from: "Guarantee 4: nothing-else-touched",
    description: "internal/server/test_slot_hot_swap_api.go is byte-identical to origin/main (history endpoint preserved)",
    kind: "git-diff-empty",
    paths: ["internal/server/test_slot_hot_swap_api.go"],
    base: "origin/main",
  },
  {
    id: "existing-glimmung-agent-cli-unchanged",
    from: "Guarantee 4: nothing-else-touched",
    description: "cmd/glimmung-agent/main.go's test-slot-hot-swap subcommand is byte-identical to origin/main (existing verify-loop callers keep working)",
    kind: "git-diff-empty",
    paths: ["cmd/glimmung-agent/main.go"],
    base: "origin/main",
  },
  {
    id: "existing-hotswap-ops-static-backend-unchanged",
    from: "Guarantee 4: nothing-else-touched",
    description: "TestSlotHotSwap's static + backend handling paths in internal/ops/agentops/hotswap.go preserve their behavior (the AgentRunner path is added in a separate file, not by interleaving into the existing function)",
    kind: "git-diff-empty",
    paths: ["internal/ops/agentops/hotswap.go"],
    base: "origin/main",
  },
  {
    id: "existing-server-routes-only-add",
    from: "Guarantee 4: nothing-else-touched",
    file: "internal/server/server.go",
    description: "Existing /v1/test-slots/* routes still registered (checkout, return, hot-swap-history) — the new apply route is purely additive",
    kind: "grep-multi-present",
    patterns: [
      /POST\s+\/v1\/test-slots\/checkout/,
      /POST\s+\/v1\/test-slots\/return/,
      /POST\s+\/v1\/test-slots\/hot-swap-history/,
      /POST\s+\/v1\/test-slots\/apply-hot-swap/,
    ],
  },
  {
    id: "existing-static-block-unchanged",
    from: "Guarantee 4: nothing-else-touched",
    description: "BackendContract struct's existing field set is preserved (new BuilderImage is additive — old fields still in declaration order)",
    kind: "grep-present",
    file: "internal/domain/hotswap/hotswap.go",
    pattern: /type\s+BackendContract\s+struct[\s\S]{0,800}?Enabled\s+bool[\s\S]{0,200}?Strategy\s+string[\s\S]{0,200}?BuildCommand\s+string[\s\S]{0,200}?Artifact\s+string[\s\S]{0,200}?Target\s+string[\s\S]{0,200}?HealthPath\s+string/,
  },
  {
    id: "existing-static-contract-fields-unchanged",
    from: "Guarantee 4: nothing-else-touched",
    file: "internal/domain/hotswap/hotswap.go",
    description: "StaticContract's existing field set is preserved (Source + Target still in declaration order; BuilderImage is additive)",
    kind: "grep-present",
    pattern: /type\s+StaticContract\s+struct[\s\S]{0,400}?Enabled\s+bool[\s\S]{0,200}?Source\s+string[\s\S]{0,200}?Target\s+string/,
  },

  // ─────────────────────── Tests ───────────────────────

  {
    id: "test-contract-agent-runner-roundtrip",
    from: "Tests",
    file: "internal/domain/hotswap/hotswap_test.go",
    description: "Contract round-trip test exercises FromMetadata + Validate for the new AgentRunner sub-contract (success + missing-field cases)",
    kind: "grep-present",
    pattern: /TestContract.*AgentRunner|AgentRunner.*roundtrip|AgentRunner.*Validate/,
  },
  {
    id: "test-ops-apply-hot-swap-job-spec",
    from: "Tests",
    file: "internal/server/test_slot_apply_hot_swap_ops_test.go",
    description: "Test asserts ApplyHotSwap renders the correct Job spec for each artifact_kind (builder_image, init container, main container, volumes)",
    kind: "grep-present",
    pattern: /TestApplyHotSwap|TestDispatchApplyHotSwap/,
  },
  {
    id: "test-endpoint-happy-path",
    from: "Tests",
    file: "internal/server/test_slot_apply_hot_swap_api_test.go",
    description: "Endpoint test covers happy path (resolve lease + read contract + dispatch + record history + return result)",
    kind: "grep-present",
    pattern: /TestApplyTestSlotHotSwap.*Happy|TestApplyTestSlotHotSwap.*Resolves/,
  },
  {
    id: "test-endpoint-failure-records-history",
    from: "Tests",
    file: "internal/server/test_slot_apply_hot_swap_api_test.go",
    description: "Endpoint test covers failure paths (build_failed / swap_failed / timeout) and asserts hot-swap history is recorded with the failure status",
    kind: "grep-present",
    pattern: /TestApplyTestSlotHotSwap.*(?:Records|Failure|Timeout|Build|Swap)/,
  },
  {
    id: "test-endpoint-timeout-clamping",
    from: "Tests",
    file: "internal/server/test_slot_apply_hot_swap_api_test.go",
    description: "Endpoint test asserts caller timeout is clamped to the hard server max",
    kind: "grep-present",
    pattern: /TestApplyTestSlotHotSwap.*(?:Clamp|Bound|Cap|Max)/,
  },

  // ─────────────────────── Docs ───────────────────────

  {
    id: "docs-new-test-slot-hot-swap-doc",
    from: "Docs",
    file: "docs/test-slot-hot-swap.md",
    description: "New doc describes the workflow + the contract shape + the new MCP tool",
    kind: "grep-present",
    pattern: /apply_test_slot_hot_swap|apply-hot-swap/,
  },
  {
    id: "docs-readme-mcp-surface",
    from: "Docs",
    file: "README.md",
    description: "README MCP-surface section names the new apply tool",
    kind: "grep-present",
    pattern: /apply_test_slot_hot_swap|apply-hot-swap/,
  },
  {
    id: "docs-deprecates-manual-pattern",
    from: "Docs",
    file: "docs/test-slot-hot-swap.md",
    description: "Doc explicitly names the manual kubectl-fu pattern as deprecated (the /test skill currently documents it; this doc is the replacement)",
    kind: "grep-present",
    pattern: /deprecat|retire|replaces[\s\S]{0,200}?manual/i,
  },

  // ─────────────────────── Executable gates ───────────────────────

  {
    id: "exec-go-vet",
    from: "Executable gates",
    description: "go vet passes (catches obvious type errors)",
    kind: "exec",
    command: ["go", "vet", "./..."],
  },
  {
    id: "exec-go-test-hotswap",
    from: "Executable gates",
    description: "go test ./internal/domain/hotswap/... passes",
    kind: "exec",
    command: ["go", "test", "./internal/domain/hotswap/..."],
  },
  {
    id: "exec-go-test-agentops",
    from: "Executable gates",
    description: "go test ./internal/ops/agentops/... passes (covers existing TestSlotHotSwap regression + new ApplyHotSwap)",
    kind: "exec",
    command: ["go", "test", "./internal/ops/agentops/..."],
  },
  {
    id: "exec-go-test-server",
    from: "Executable gates",
    description: "go test ./internal/server/... passes (covers existing endpoints + new apply endpoint)",
    kind: "exec",
    command: ["go", "test", "./internal/server/..."],
  },
  {
    id: "exec-helm-template",
    from: "Executable gates",
    description: "helm template k8s renders (chart still valid)",
    kind: "exec",
    command: ["helm", "template", "glimmung", "k8s"],
  },
];

// ─────────────────────────────────────────────────────────────────────────────
// Runner
// ─────────────────────────────────────────────────────────────────────────────

printHeader();

const results = [];
for (const check of CHECKS) {
  if (check.kind === "exec" && skipExec) {
    results.push({ check, pass: true, skipped: true, evidence: "SKIP_EXEC=1" });
    printResult(results[results.length - 1]);
    continue;
  }
  const result = await runCheck(check);
  results.push(result);
  printResult(result);
}

printSummary(results);
process.exit(results.some((r) => !r.pass) ? 1 : 0);

// ─────────────────────────────────────────────────────────────────────────────
// Dispatch
// ─────────────────────────────────────────────────────────────────────────────

async function runCheck(check) {
  try {
    const result = await dispatch(check);
    return { check, ...result };
  } catch (err) {
    return { check, pass: false, evidence: `error: ${err.message}` };
  }
}

async function dispatch(check) {
  switch (check.kind) {
    case "grep-present":        return await grepPresent(check);
    case "grep-absent":         return await grepAbsent(check);
    case "grep-multi-present":  return await grepMultiPresent(check);
    case "git-diff-empty":      return gitDiffEmpty(check);
    case "exec":                return execCheck(check);
    default: return { pass: false, evidence: `unknown kind: ${check.kind}` };
  }
}

async function grepPresent({ file, pattern }) {
  if (!(await fileExists(file))) return { pass: false, evidence: `file missing: ${file}` };
  const content = await readRel(file);
  const m = pattern.exec(content);
  if (!m) return { pass: false, evidence: `pattern not found in ${file}: ${pattern}` };
  const { line } = locate(content, m.index);
  return { pass: true, evidence: `${file}:${line}` };
}

async function grepAbsent({ file, pattern }) {
  if (!(await fileExists(file))) return { pass: false, evidence: `file missing: ${file}` };
  const content = await readRel(file);
  const m = pattern.exec(content);
  if (m) {
    const { line, column } = locate(content, m.index);
    return { pass: false, evidence: `${file}:${line}:${column} present but should be absent: ${JSON.stringify(m[0].slice(0, 80))}` };
  }
  return { pass: true, evidence: `${file}: pattern absent` };
}

async function grepMultiPresent({ file, patterns }) {
  if (!(await fileExists(file))) return { pass: false, evidence: `file missing: ${file}` };
  const content = await readRel(file);
  const missing = [];
  for (const p of patterns) {
    if (!p.exec(content)) missing.push(String(p));
  }
  if (missing.length) return { pass: false, evidence: `${file}: missing ${missing.length} pattern(s): ${missing.join(", ")}` };
  return { pass: true, evidence: `${file}: all ${patterns.length} patterns present` };
}

function gitDiffEmpty({ paths, base }) {
  const result = spawnSync("git", ["diff", "--quiet", base, "--", ...paths], {
    cwd: repoRoot,
    encoding: "utf8",
    stdio: ["ignore", "pipe", "pipe"],
  });
  if (result.error) return { pass: false, evidence: `spawn error: ${result.error.message}` };
  if (result.status === 0) return { pass: true, evidence: `unchanged vs ${base}: ${paths.join(", ")}` };
  if (result.status === 1) return { pass: false, evidence: `MODIFIED vs ${base}: ${paths.join(", ")}` };
  return { pass: false, evidence: `git diff failed (status=${result.status}): ${result.stderr.trim().slice(0, 200)}` };
}

function execCheck({ command, cwd }) {
  const cwdAbs = cwd ? path.join(repoRoot, cwd) : repoRoot;
  const result = spawnSync(command[0], command.slice(1), {
    cwd: cwdAbs,
    encoding: "utf8",
    stdio: ["ignore", "pipe", "pipe"],
  });
  if (result.error) return { pass: false, evidence: `spawn error: ${result.error.message}` };
  if (result.status !== 0) {
    const stream = (result.stderr && result.stderr.trim()) || (result.stdout && result.stdout.trim()) || "";
    const tail = stream.split("\n").slice(-3).join(" ¶ ").slice(0, 240);
    return { pass: false, evidence: `exit ${result.status}: ${tail}` };
  }
  return { pass: true, evidence: `exit 0` };
}

// ─────────────────────────────────────────────────────────────────────────────
// Output + helpers
// ─────────────────────────────────────────────────────────────────────────────

function printHeader() {
  const byCategory = new Map();
  for (const check of CHECKS) {
    byCategory.set(check.from, (byCategory.get(check.from) ?? 0) + 1);
  }
  console.log(`apply_test_slot_hot_swap manifest: ${CHECKS.length} checks across ${byCategory.size} categories`);
  for (const [cat, n] of byCategory) console.log(`  ${String(n).padStart(2)} ${cat}`);
  if (skipExec) console.log("  (SKIP_EXEC=1 — exec gates marked PASS without running)");
  console.log("");
}

function printResult(r) {
  const sym = r.skipped ? "SKIP" : r.pass ? "PASS" : "FAIL";
  console.log(`${sym}  ${r.check.id.padEnd(50)}  ${r.check.description}`);
  if (!r.pass || r.skipped) {
    if (r.evidence) console.log(`      ↳ ${r.evidence}`);
  }
}

function printSummary(results) {
  const passed = results.filter((r) => r.pass && !r.skipped).length;
  const skipped = results.filter((r) => r.skipped).length;
  const failed = results.filter((r) => !r.pass);
  console.log("");
  console.log(`${passed}/${results.length} pass${skipped ? `, ${skipped} skipped` : ""}${failed.length ? `, ${failed.length} fail` : ""}`);
  if (failed.length) {
    console.log("");
    console.log("Failing checks:");
    for (const r of failed) {
      console.log(`  ${r.check.id}  [${r.check.from}]`);
      console.log(`      ${r.evidence}`);
    }
  }
}

async function fileExists(rel) {
  try {
    await fs.access(path.join(repoRoot, rel));
    return true;
  } catch {
    return false;
  }
}

async function readRel(rel) {
  return await fs.readFile(path.join(repoRoot, rel), "utf8");
}

function locate(content, index) {
  const before = content.slice(0, index);
  const lines = before.split(/\r\n|\r|\n/);
  return { line: lines.length, column: lines[lines.length - 1].length + 1 };
}
