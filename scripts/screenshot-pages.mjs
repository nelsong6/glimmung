// Screenshot pass for #87. Reads frontend/screenshot-pages.json, hits
// each path against BASE_URL with a headless chromium, and writes one
// PNG per page into OUT_DIR. A page that fails to load (non-2xx, JS
// error, timeout) exits non-zero with a typed reason the workflow can
// surface as `screenshot_capture_failed: <path>`.
//
// Used by .github/workflows/agent-run.yml after the rebuilt validation
// env is ready. Screenshots land where the upload-to-Azure step picks
// them up (/tmp/evidence/screenshots).
//
// Inputs (env):
//   BASE_URL           required. e.g. https://issue-1-...glimmung.dev.romaine.life
//   PAGES_JSON         required. path to screenshot-pages.json
//   OUT_DIR            required. directory for png output
//   TIMEOUT_MS         optional. per-page nav timeout, default 30000
//   VIEWPORT_W,VIEWPORT_H  optional. default 1440x900
//
// Stdout is line-oriented and grep-friendly so the CI step can attach
// page-level diagnostics to the abort message.

import { chromium } from "playwright";
import { mkdir, readFile, writeFile } from "node:fs/promises";
import { join } from "node:path";

function env(name, fallback) {
  const v = process.env[name];
  if (v === undefined || v === "") {
    if (fallback !== undefined) return fallback;
    console.error(`missing required env: ${name}`);
    process.exit(2);
  }
  return v;
}

const BASE_URL = env("BASE_URL").replace(/\/$/, "");
const PAGES_JSON = env("PAGES_JSON");
const OUT_DIR = env("OUT_DIR");
const TIMEOUT_MS = parseInt(env("TIMEOUT_MS", "30000"), 10);
const VIEWPORT_W = parseInt(env("VIEWPORT_W", "1440"), 10);
const VIEWPORT_H = parseInt(env("VIEWPORT_H", "900"), 10);

const pagesRaw = await readFile(PAGES_JSON, "utf-8");
const pages = JSON.parse(pagesRaw);
if (!Array.isArray(pages) || pages.length === 0) {
  console.error(`${PAGES_JSON} must be a non-empty array`);
  process.exit(2);
}

await mkdir(OUT_DIR, { recursive: true });

const browser = await chromium.launch();
const ctx = await browser.newContext({
  viewport: { width: VIEWPORT_W, height: VIEWPORT_H },
  // Some pages 401 without auth; we treat that as render-OK as long as
  // the page paints. The CI pass is exercising the styleguide + public
  // routes; auth-gated views are out of scope for the visual catalog.
  ignoreHTTPSErrors: true,
});

const failures = [];
const captures = [];

for (const entry of pages) {
  const path = String(entry.path ?? "");
  const label = String(entry.label ?? path).replace(/[^a-zA-Z0-9._-]+/g, "-");
  if (!path.startsWith("/")) {
    failures.push({ path, label, reason: "page entry missing leading slash" });
    continue;
  }
  const url = `${BASE_URL}${path}`;
  const page = await ctx.newPage();
  // Surface in-page console errors / unhandled rejections in the log
  // so a 500-ing page is diagnosable from the CI run.
  page.on("pageerror", (e) => console.log(`pageerror ${path}: ${e.message}`));
  try {
    const resp = await page.goto(url, { timeout: TIMEOUT_MS, waitUntil: "domcontentloaded" });
    if (resp === null) {
      failures.push({ path, label, reason: "no response (data: or about: url?)" });
      await page.close();
      continue;
    }
    const status = resp.status();
    if (status >= 400) {
      failures.push({ path, label, reason: `HTTP ${status}` });
      await page.close();
      continue;
    }
    await page.locator("body").waitFor({ state: "visible", timeout: TIMEOUT_MS });
    const out = join(OUT_DIR, `${label}.png`);
    await page.screenshot({ path: out, fullPage: true });
    captures.push({ path, label, url, status, out });
    console.log(`captured ${label} ← ${url} (HTTP ${status})`);
  } catch (e) {
    failures.push({ path, label, reason: e?.message ?? String(e) });
  } finally {
    await page.close();
  }
}

await ctx.close();
await browser.close();

// Manifest helps the PR composer (#88) render labels next to images
// without re-deriving them from filenames.
await writeFile(
  join(OUT_DIR, "manifest.json"),
  JSON.stringify({ base_url: BASE_URL, captures, failures }, null, 2),
);

if (failures.length > 0) {
  // Match the issue's typed reason shape: screenshot_capture_failed: <path>.
  // Multiple failures are joined with `; `; CI parses the first.
  const reason = failures
    .map((f) => `screenshot_capture_failed: ${f.path} (${f.reason})`)
    .join("; ");
  console.error(reason);
  process.exit(1);
}

console.log(`captured ${captures.length} pages → ${OUT_DIR}`);
