import { mkdir } from "node:fs/promises";
import path from "node:path";
import { pathToFileURL } from "node:url";

const playwrightModule =
  process.env.PLAYWRIGHT_PACKAGE_PATH
    ? pathToFileURL(process.env.PLAYWRIGHT_PACKAGE_PATH).href
    : "playwright";
const { chromium } = await import(playwrightModule);

const input = JSON.parse(await new Promise((resolve, reject) => {
  let data = "";
  process.stdin.setEncoding("utf8");
  process.stdin.on("data", (chunk) => { data += chunk; });
  process.stdin.on("end", () => resolve(data || "{}"));
  process.stdin.on("error", reject);
}));

function truncate(value, limit) {
  const text = String(value ?? "");
  if (text.length <= limit) return text;
  return `${text.slice(0, Math.max(limit - 3, 0)).trimEnd()}...`;
}

function slug(value) {
  return String(value || "inspection")
    .replace(/[^A-Za-z0-9._-]+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, 80) || "inspection";
}

function interestingElements(maxElements) {
  return () => {
    const max = maxElements;
    const selectorParts = (el) => {
      if (el.id) return `#${CSS.escape(el.id)}`;
      const testId = el.getAttribute("data-testid") || el.getAttribute("data-test");
      if (testId) return `[data-testid="${CSS.escape(testId)}"]`;
      const aria = el.getAttribute("aria-label");
      if (aria) return `${el.tagName.toLowerCase()}[aria-label="${CSS.escape(aria)}"]`;
      return el.tagName.toLowerCase();
    };
    const selectorFor = (el) => {
      const parts = [];
      let cur = el;
      while (cur && cur.nodeType === Node.ELEMENT_NODE && parts.length < 4) {
        let part = selectorParts(cur);
        if (!cur.id && cur.parentElement) {
          const siblings = Array.from(cur.parentElement.children)
            .filter((s) => s.tagName === cur.tagName);
          if (siblings.length > 1) part += `:nth-of-type(${siblings.indexOf(cur) + 1})`;
        }
        parts.unshift(part);
        if (cur.id) break;
        cur = cur.parentElement;
      }
      return parts.join(" > ");
    };
    const roleFor = (el) => {
      const explicit = el.getAttribute("role");
      if (explicit) return explicit;
      const tag = el.tagName.toLowerCase();
      if (tag === "button") return "button";
      if (tag === "a" && el.hasAttribute("href")) return "link";
      if (tag === "input") return el.getAttribute("type") || "textbox";
      if (/^h[1-6]$/.test(tag)) return "heading";
      if (tag === "select") return "combobox";
      if (tag === "textarea") return "textbox";
      if (tag === "canvas") return "canvas";
      return null;
    };
    const candidates = Array.from(document.querySelectorAll(
      "main,header,nav,section,article,h1,h2,h3,button,a,input,select,textarea,[role],canvas,[data-testid],[data-test]",
    ));
    return candidates
      .map((el) => {
        const rect = el.getBoundingClientRect();
        const style = getComputedStyle(el);
        const visible = rect.width > 0 && rect.height > 0
          && style.visibility !== "hidden"
          && style.display !== "none"
          && Number(style.opacity || "1") > 0;
        const text = (el.innerText || el.getAttribute("aria-label") || el.getAttribute("alt") || "")
          .replace(/\s+/g, " ")
          .trim();
        return {
          selector: selectorFor(el),
          tag: el.tagName.toLowerCase(),
          role: roleFor(el),
          text,
          visible,
          bounds: {
            x: Math.round(rect.x),
            y: Math.round(rect.y),
            width: Math.round(rect.width),
            height: Math.round(rect.height),
          },
          styles: {
            display: style.display,
            position: style.position,
            color: style.color,
            backgroundColor: style.backgroundColor,
            fontSize: style.fontSize,
            zIndex: style.zIndex,
          },
        };
      })
      .filter((el) => el.visible || el.text || el.role || el.tag === "canvas")
      .slice(0, max);
  };
}

function canvasStats() {
  return () => Array.from(document.querySelectorAll("canvas")).map((canvas, index) => {
    const rect = canvas.getBoundingClientRect();
    const out = {
      index,
      selector: canvas.id ? `#${CSS.escape(canvas.id)}` : `canvas:nth-of-type(${index + 1})`,
      width: canvas.width,
      height: canvas.height,
      bounds: {
        x: Math.round(rect.x),
        y: Math.round(rect.y),
        width: Math.round(rect.width),
        height: Math.round(rect.height),
      },
      readable: false,
      nonblank_pixels: null,
      sampled_pixels: null,
    };
    try {
      const ctx = canvas.getContext("2d", { willReadFrequently: true });
      if (!ctx || canvas.width === 0 || canvas.height === 0) return out;
      const sampleW = Math.min(canvas.width, 128);
      const sampleH = Math.min(canvas.height, 128);
      const image = ctx.getImageData(0, 0, sampleW, sampleH).data;
      let nonblank = 0;
      for (let i = 0; i < image.length; i += 4) {
        if (image[i] !== 0 || image[i + 1] !== 0 || image[i + 2] !== 0 || image[i + 3] !== 0) {
          nonblank += 1;
        }
      }
      out.readable = true;
      out.nonblank_pixels = nonblank;
      out.sampled_pixels = sampleW * sampleH;
    } catch (e) {
      out.error = String(e?.message ?? e);
    }
    return out;
  });
}

const startedAt = new Date();
const consoleMessages = [];
const pageErrors = [];
const failedRequests = [];
const httpErrors = [];

const browser = await chromium.launch({ headless: true });
try {
  const context = await browser.newContext({
    viewport: input.viewport || { width: 1440, height: 900 },
    ignoreHTTPSErrors: true,
  });
  const page = await context.newPage();
  if (input.captureConsole !== false) {
    page.on("console", (msg) => consoleMessages.push({
      type: msg.type(),
      text: truncate(msg.text(), 1000),
      location: msg.location(),
    }));
    page.on("pageerror", (err) => pageErrors.push(String(err?.message ?? err)));
  }
  if (input.captureNetwork !== false) {
    page.on("requestfailed", (req) => failedRequests.push({
      url: req.url(),
      method: req.method(),
      failure: req.failure(),
    }));
    page.on("response", (resp) => {
      if (resp.status() >= 400) {
        httpErrors.push({
          url: resp.url(),
          status: resp.status(),
          status_text: resp.statusText(),
        });
      }
    });
  }

  let status = null;
  let responseError = null;
  try {
    const response = await page.goto(input.url, {
      waitUntil: "domcontentloaded",
      timeout: input.timeoutMs ?? 30000,
    });
    status = response ? response.status() : null;
    await page.waitForLoadState("networkidle", { timeout: Math.min(input.timeoutMs ?? 30000, 10000) });
  } catch (e) {
    responseError = String(e?.message ?? e);
  }
  if ((input.waitMs ?? 0) > 0) {
    await page.waitForTimeout(input.waitMs);
  }

  let bodyText = "";
  try {
    bodyText = await page.locator("body").innerText({ timeout: 2000 });
  } catch {
    bodyText = "";
  }

  let screenshotPath = null;
  if (input.screenshot) {
    const artifactDir = input.artifactDir || "/tmp/glimmung-browser-inspections";
    await mkdir(artifactDir, { recursive: true });
    const stamp = startedAt.toISOString().replace(/[-:]/g, "").replace(/\.\d+Z$/, "Z");
    screenshotPath = path.join(artifactDir, `${stamp}-${slug(page.url())}.png`);
    await page.screenshot({ path: screenshotPath, fullPage: input.fullPage !== false });
  }

  let accessibility = null;
  if (input.captureAccessibility) {
    try {
      accessibility = await page.accessibility.snapshot({ interestingOnly: true });
    } catch (e) {
      accessibility = { error: String(e?.message ?? e) };
    }
  }

  const result = {
    schema_version: 1,
    url: input.url,
    final_url: page.url(),
    status,
    response_error: responseError,
    title: await page.title(),
    body_text: truncate(bodyText.replace(/\0/g, ""), input.bodyTextLimit ?? 4000),
    viewport: input.viewport || { width: 1440, height: 900 },
    elements: await page.evaluate(interestingElements(input.maxElements ?? 80)),
    console: consoleMessages,
    page_errors: pageErrors,
    failed_requests: failedRequests,
    http_errors: httpErrors,
    accessibility,
    screenshot_path: screenshotPath,
    canvas: await page.evaluate(canvasStats()),
    inspected_at: startedAt.toISOString(),
  };
  await context.close();
  process.stdout.write(`${JSON.stringify(result, null, 2)}\n`);
} finally {
  await browser.close();
}
