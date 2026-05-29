#!/usr/bin/env node
// Migration guard for the retired SVG OG renderer.
//
// Per .tank/docs/migration-policy.md, the SVG renderer that originally
// shipped alongside the PNG OG image is deleted end to end. Discord
// silently dropped the SVG og:image, so SVG never reached the named
// unfurler target and was carrying no traffic — keeping it would have
// been the "fallback / compatibility" smell the policy rejects.
//
// This script fails CI if the SVG path tries to return through any of
// these surfaces:
//
//   - a `renderRunOGSVG` function (the renderer)
//   - a `runOGImage` function (the SVG HTTP handler)
//   - a `runOGImageDispatch` function (the .png-vs-.svg dispatcher)
//   - a `Content-Type: image/svg+xml` write
//   - a route or string that exposes `/og/runs/.../{slug}.svg`
//
// New PNG code is fine — only the SVG-shaped surface is forbidden.

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
  "frontend",
  "scripts",
]);

// Paths/files that should not be scanned: this script itself, the test
// that proves the surface stays deleted (it has to mention the URL to
// assert the 404), and the doc comments that explain why the surface
// is gone.
const allowedFilePatterns = [
  /\binternal\/server\/og_run_test\.go$/,
];

const forbidden = [
  { name: "renderRunOGSVG", pattern: /renderRunOGSVG\b/ },
  { name: "runOGImage handler", pattern: /\bfunc\s+runOGImage\s*\(/ },
  { name: "runOGImageDispatch", pattern: /\brunOGImageDispatch\b/ },
  { name: "image/svg+xml content type", pattern: /image\/svg\+xml/ },
  { name: ".svg OG URL", pattern: /\/og\/runs\/[^"\s]*\.svg/ },
];

async function* walk(dir) {
  for (const entry of await fs.readdir(dir, { withFileTypes: true })) {
    if (ignoredDirs.has(entry.name)) continue;
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) {
      yield* walk(full);
    } else if (entry.isFile()) {
      yield full;
    }
  }
}

const offenders = [];
for await (const file of walk(repoRoot)) {
  const rel = path.relative(repoRoot, file);
  if (allowedFilePatterns.some((p) => p.test(rel))) continue;
  // Only scan source-ish files. Keep this list narrow so binary
  // assets, vendored deps, lockfiles, etc. don't waste cycles.
  if (!/\.(go|ts|tsx|js|mjs|yaml|yml|md)$/.test(rel)) continue;
  const text = await fs.readFile(file, "utf8");
  for (const { name, pattern } of forbidden) {
    if (pattern.test(text)) {
      offenders.push({ file: rel, surface: name });
    }
  }
}

if (offenders.length > 0) {
  console.error(
    "retired SVG OG renderer surface detected:\n" +
      offenders.map((o) => `  ${o.file}: ${o.surface}`).join("\n") +
      "\n\nThe SVG OG renderer was retired because Discord silently drops\n" +
      "SVG og:images. Per .tank/docs/migration-policy.md, the path is\n" +
      "deleted end to end — no live route, no parallel format. If you\n" +
      "need to bring SVG back, write a new design doc; do not reinstate\n" +
      "the old surface.",
  );
  process.exit(1);
}

console.log("retired SVG OG renderer surface absent.");
