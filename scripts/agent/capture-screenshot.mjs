import path from "node:path";
import { pathToFileURL } from "node:url";

const playwrightModule =
  process.env.PLAYWRIGHT_PACKAGE_PATH
    ? pathToFileURL(process.env.PLAYWRIGHT_PACKAGE_PATH).href
    : "playwright";
const { chromium } = await import(playwrightModule);

function readArg(flag, fallback = "") {
  const index = process.argv.indexOf(flag);
  if (index === -1 || index === process.argv.length - 1) {
    return fallback;
  }
  return process.argv[index + 1];
}

function hasFlag(flag) {
  return process.argv.includes(flag);
}

const url = readArg("--url");
const output = readArg("--output");
const waitMs = Number(readArg("--wait-ms", "5000"));
const width = Number(readArg("--width", "1600"));
const height = Number(readArg("--height", "900"));
const fullPage = hasFlag("--full-page");

if (!url) {
  throw new Error("--url is required");
}

if (!output) {
  throw new Error("--output is required");
}

const browser = await chromium.launch({ headless: true });

try {
  const context = await browser.newContext({
    viewport: { width, height },
  });
  const page = await context.newPage();
  await page.goto(url, { waitUntil: "domcontentloaded" });
  await page.waitForTimeout(waitMs);
  await page.screenshot({
    path: path.resolve(output),
    fullPage,
  });
  await context.close();
} finally {
  await browser.close();
}
