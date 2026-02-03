// csp-check.mjs
// ------------------------------------------------------------
// Fast browser-native CSP checker using Playwright.
// Accepts a list of full URLs (one per line) and reports CSP violations.
// ------------------------------------------------------------

import fs from "fs";
import path from "path";
import { createRequire } from "module";

let chromium;
let firefox;
let webkit;

try {
  const pw = await import("playwright");
  chromium = pw.chromium;
  firefox = pw.firefox;
  webkit = pw.webkit;
} catch (err) {
  const require = createRequire(import.meta.url);
  const pw = require("playwright");
  chromium = pw.chromium;
  firefox = pw.firefox;
  webkit = pw.webkit;
}

const urlsFile = process.argv[2];

if (!urlsFile) {
  console.error("Usage: node csp-check.mjs <urls_file>");
  process.exit(2);
}

const WAIT_AFTER_LOAD_MS = Number(process.env.CSP_WAIT_MS || 2000);
const NAV_TIMEOUT_MS = Number(process.env.CSP_NAV_TIMEOUT_MS || 30000);
const CONCURRENCY = Math.max(1, Number(process.env.CSP_CONCURRENCY || 1));
const WAIT_UNTIL = String(process.env.CSP_WAIT_UNTIL || "domcontentloaded");
const BETWEEN_URL_MS = Number(process.env.CSP_BETWEEN_URL_MS || 800);

const OUTPUT_JSON = String(process.env.CSP_OUTPUT_JSON || "0") === "1";
const OUTPUT_FILE = process.env.CSP_OUTPUT_FILE || "csp-report.json";

const VERBOSE = String(process.env.CSP_VERBOSE || "0") === "1";
const SHOW_POLICY = String(process.env.CSP_SHOW_POLICY || "0") === "1";
const POLICY_MAXLEN = Number(process.env.CSP_POLICY_MAXLEN || 800);
const INCLUDE_REQUEST_FAILED = String(process.env.CSP_INCLUDE_REQUEST_FAILED || "1") === "1";
const INCLUDE_CONSOLE_CSP = String(process.env.CSP_INCLUDE_CONSOLE_CSP || "1") === "1";
const HEADLESS = String(process.env.CSP_HEADLESS || "1") !== "0";
const STEALTH = String(process.env.CSP_STEALTH || "1") === "1";
const BROWSER = String(process.env.CSP_BROWSER || "chromium").toLowerCase();

const USER_AGENT =
  process.env.CSP_USER_AGENT ||
  "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36";

const ACCEPT_LANGUAGE = process.env.CSP_ACCEPT_LANGUAGE || "en-US,en;q=0.9";

function sleep(ms) {
  return new Promise((r) => setTimeout(r, ms));
}

function readUrlList(filePath) {
  const content = fs.readFileSync(filePath, "utf8");
  const lines = content.split(/\r?\n/);

  const items = [];
  for (const raw of lines) {
    let line = raw.trim();
    if (!line) continue;
    if (line.startsWith("#")) continue;
    if (line.includes("#")) {
      line = line.split("#")[0].trim();
    }
    if (!line) continue;
    if (!/^https?:\/\//i.test(line)) continue;
    items.push(normalizeUrl(line));
  }
  return items;
}

function normalizeUrl(raw) {
  try {
    const u = new URL(raw);
    if (!u.pathname) u.pathname = "/";
    return u.toString();
  } catch {
    return raw;
  }
}

function blockedOrigin(blockedURI) {
  if (!blockedURI) return null;
  try {
    const u = new URL(blockedURI);
    return u.origin;
  } catch {
    return blockedURI; // inline, eval, data:, blob:, etc.
  }
}

function truncate(s, maxLen) {
  if (!s) return s;
  if (s.length <= maxLen) return s;
  return s.slice(0, maxLen) + `…(truncated ${s.length - maxLen} chars)`;
}

function normalizeViolation(v) {
  return {
    documentURI: v.documentURI || null,
    referrer: v.referrer || null,
    blockedURI: v.blockedURI || null,
    blockedOrigin: blockedOrigin(v.blockedURI || null),
    effectiveDirective: v.effectiveDirective || null,
    violatedDirective: v.violatedDirective || null,
    originalPolicy: v.originalPolicy || null,
    disposition: v.disposition || null,
    statusCode: v.statusCode ?? null,
    sourceFile: v.sourceFile || null,
    lineNumber: v.lineNumber ?? null,
    columnNumber: v.columnNumber ?? null,
    sample: v.sample || null,
  };
}

function violationKey(v) {
  // Deduplicate within a page
  return [
    v.effectiveDirective,
    v.blockedURI,
    v.sourceFile,
    v.lineNumber,
    v.columnNumber,
  ].join("|");
}

function fmtMs(ms) {
  if (ms < 1000) return `${ms}ms`;
  return `${(ms / 1000).toFixed(2)}s`;
}

async function checkUrl(context, url) {
  const page = await context.newPage();
  const violations = [];

  await page.exposeFunction("__reportCspViolation", (v) => {
    violations.push(v);
  });

  await page.addInitScript(() => {
    window.addEventListener("securitypolicyviolation", (e) => {
      window.__reportCspViolation({
        documentURI: e.documentURI,
        referrer: e.referrer,
        blockedURI: e.blockedURI,
        effectiveDirective: e.effectiveDirective,
        violatedDirective: e.violatedDirective,
        originalPolicy: e.originalPolicy,
        disposition: e.disposition,
        statusCode: e.statusCode,
        sourceFile: e.sourceFile,
        lineNumber: e.lineNumber,
        columnNumber: e.columnNumber,
        sample: e.sample,
      });
    });
  });

  if (STEALTH) {
    await page.addInitScript(() => {
      Object.defineProperty(navigator, "webdriver", { get: () => false });
      window.chrome = window.chrome || { runtime: {} };
      const originalQuery = window.navigator.permissions.query;
      window.navigator.permissions.query = (parameters) =>
        parameters.name === "notifications"
          ? Promise.resolve({ state: Notification.permission })
          : originalQuery(parameters);
    });
  }

  if (INCLUDE_CONSOLE_CSP) {
    page.on("console", (msg) => {
      const text = msg.text();
      const parsed = parseCspConsoleMessage(text);
      if (!parsed) return;
      if (!parsed.blockedURI || !parsed.effectiveDirective) return;
      const disposition = /report[- ]only/i.test(text) ? "report-only" : "enforce";
      violations.push({
        documentURI: url,
        referrer: null,
        blockedURI: parsed.blockedURI,
        effectiveDirective: parsed.effectiveDirective,
        violatedDirective: null,
        originalPolicy: parsed.originalPolicy,
        disposition,
        statusCode: null,
        sourceFile: "console",
        lineNumber: null,
        columnNumber: null,
        sample: null,
      });
    });
  }

  if (INCLUDE_REQUEST_FAILED) {
    page.on("requestfailed", (request) => {
      const failure = request.failure();
      const msg = failure && failure.errorText ? failure.errorText : "";
      if (!msg) return;
      const isCSP =
        msg.includes("ERR_BLOCKED_BY_CSP") ||
        msg.toLowerCase().includes("blocked by csp");
      if (!isCSP) return;
      violations.push({
        documentURI: url,
        referrer: null,
        blockedURI: request.url(),
        effectiveDirective: "blocked-by-csp",
        violatedDirective: null,
        originalPolicy: null,
        disposition: "enforce",
        statusCode: null,
        sourceFile: "requestfailed",
        lineNumber: null,
        columnNumber: null,
        sample: null,
      });
    });
  }

  const start = Date.now();
  let status = null;
  let ok = false;
  let error = null;

  try {
    const resp = await page.goto(url, {
      waitUntil: WAIT_UNTIL,
      timeout: NAV_TIMEOUT_MS,
    });
    status = resp ? resp.status() : null;
    ok = true;

    if (WAIT_AFTER_LOAD_MS > 0) {
      await page.waitForTimeout(WAIT_AFTER_LOAD_MS);
    }
  } catch (e) {
    error = String(e && e.message ? e.message : e);
  } finally {
    await page.close();
  }

  const uniq = new Map();
  for (const v of violations.map(normalizeViolation)) {
    const key = violationKey(v);
    const existing = uniq.get(key);
    if (!existing) {
      uniq.set(key, v);
      continue;
    }
    const merged = mergeViolation(existing, v);
    uniq.set(key, merged);
  }

  return {
    url,
    status,
    ok,
    error,
    durationMs: Date.now() - start,
    violations: [...uniq.values()],
  };
}

function normalizeDisposition(d) {
  const val = String(d || "").toLowerCase();
  if (val.includes("report")) return "report-only";
  if (val === "enforce") return "enforce";
  return "";
}

function mergeViolation(a, b) {
  const da = normalizeDisposition(a.disposition);
  const db = normalizeDisposition(b.disposition);
  // Prefer explicit enforce, but if one is report-only and the other is unknown,
  // keep report-only to avoid misclassifying report-only as enforce.
  if (da === "enforce" || db === "enforce") {
    return da === "enforce" ? a : b;
  }
  if (da === "report-only" || db === "report-only") {
    return da === "report-only" ? a : b;
  }
  return a;
}

function parseCspConsoleMessage(text) {
  if (!text) return null;
  if (!text.includes("Content Security Policy")) return null;
  // Example:
  // Refused to connect to 'https://stats.g.doubleclick.net/...' because it violates the following Content Security Policy directive: "connect-src 'self' ...".
  const blockedMatch = text.match(/Refused to (?:load|connect|frame|execute).*?'([^']+)'/i);
  const directiveMatch = text.match(/directive: \"([^\"]+)\"/i);
  if (!blockedMatch && !directiveMatch) return null;
  const blockedURI = blockedMatch ? blockedMatch[1] : null;
  let effectiveDirective = null;
  let originalPolicy = null;
  if (directiveMatch) {
    originalPolicy = directiveMatch[1];
    effectiveDirective = originalPolicy.split(" ")[0] || null;
  }
  if (!blockedURI || !effectiveDirective) return null;
  return { blockedURI, effectiveDirective, originalPolicy };
}

async function runQueue(items, workers, fn) {
  let idx = 0;
  const results = [];

  async function worker() {
    while (true) {
      const i = idx++;
      if (i >= items.length) return;
      results[i] = await fn(items[i], i);
    }
  }

  await Promise.all(Array.from({ length: workers }, worker));
  return results;
}

function groupViolations(results) {
  const groups = new Map();

  for (const r of results) {
    for (const v of r.violations) {
      const key = `${v.effectiveDirective} -> ${v.blockedOrigin}`;
      if (!groups.has(key)) {
        groups.set(key, {
          key,
          effectiveDirective: v.effectiveDirective,
          blockedOrigin: v.blockedOrigin,
          count: 0,
          pages: new Map(),
        });
      }
      const g = groups.get(key);
      g.count += 1;
      if (!g.pages.has(r.url)) g.pages.set(r.url, []);
      g.pages.get(r.url).push(v);
    }
  }

  return [...groups.values()].sort((a, b) => b.count - a.count);
}

function printFinalReport(results) {
  const pagesChecked = results.length;
  const totalViolations = results.reduce((acc, r) => acc + r.violations.length, 0);
  const groups = groupViolations(results);

  console.error("========== CSP SUMMARY ==========");
  console.error(`Pages checked:   ${pagesChecked}`);
  console.error(`Violations:      ${totalViolations}`);
  console.error(`Unique issues:   ${groups.length}`);

  if (groups.length === 0) return;

  console.error("");
  console.error("Top issues (grouped):");
  for (const g of groups.slice(0, 50)) {
    const pageList = [...g.pages.keys()].slice(0, 3).join(" | ");
    console.error(`- ${g.count}  ${g.key}`);
    console.error(`  pages: ${pageList}${g.pages.size > 3 ? " | ..." : ""}`);
  }

  if (!VERBOSE) return;

  console.error("");
  console.error("========== CSP VIOLATIONS (detailed) ==========");
  for (const g of groups) {
    console.error("");
    console.error(`## ${g.key}  (count=${g.count})`);
    for (const [pageUrl, vs] of g.pages.entries()) {
      console.error(`- page: ${pageUrl}  (violations=${vs.length})`);
      for (const v of vs) {
        console.error("  - blockedURI:        " + (v.blockedURI || "null"));
        if (v.violatedDirective && v.violatedDirective !== v.effectiveDirective) {
          console.error("    violatedDirective:  " + v.violatedDirective);
        }
        if (v.disposition) console.error("    disposition:        " + v.disposition);
        console.error(
          "    source:             " +
            `${v.sourceFile || "unknown"}:${v.lineNumber ?? "?"}:${v.columnNumber ?? "?"}`
        );
        if (v.sample) console.error("    sample:             " + v.sample);
        if (SHOW_POLICY && v.originalPolicy) {
          console.error("    originalPolicy:     " + truncate(v.originalPolicy, POLICY_MAXLEN));
        }
      }
    }
  }
  console.error("===============================================");
}

const targets = readUrlList(urlsFile);

console.error(`[csp] Targets: ${targets.length}`);
console.error(`[csp] waitUntil: ${WAIT_UNTIL}`);
console.error(`[csp] nav timeout: ${NAV_TIMEOUT_MS}ms`);
console.error(`[csp] settle wait: ${WAIT_AFTER_LOAD_MS}ms`);
console.error(`[csp] concurrency: ${CONCURRENCY}`);
console.error(`[csp] between-url delay: ${BETWEEN_URL_MS}ms`);
console.error(`[csp] UA: ${USER_AGENT}`);
console.error(`[csp] verbose: ${VERBOSE ? "on" : "off"} (details printed at end)`);

const browserType = BROWSER === "firefox" ? firefox : BROWSER === "webkit" ? webkit : chromium;
const launchArgs = BROWSER === "chromium" ? [
  "--disable-blink-features=AutomationControlled",
  "--disable-extensions",
  "--disable-sync",
  "--disable-default-apps",
  "--disable-background-networking",
  "--disable-component-extensions-with-background-pages",
  "--no-first-run",
  "--no-default-browser-check",
] : [];
const browser = await browserType.launch({
  headless: HEADLESS,
  args: launchArgs,
});

async function createContext() {
  return await browser.newContext({
    userAgent: USER_AGENT,
    locale: "en-US",
    viewport: { width: 1280, height: 720 },
    extraHTTPHeaders: {
      "Accept-Language": ACCEPT_LANGUAGE,
    },
  });
}

const results = await runQueue(targets, CONCURRENCY, async (u, i) => {
  console.error(`[csp] (${i + 1}/${targets.length}) ${u}`);

  if (BETWEEN_URL_MS > 0) {
    await sleep(BETWEEN_URL_MS);
  }

  const ctx = await createContext();
  const r = await checkUrl(ctx, u);
  await ctx.close();
  const note = r.ok ? `HTTP ${r.status ?? "?"}` : "FAILED";
  console.error(
    `[csp]     ${note} in ${fmtMs(r.durationMs)}, violations=${r.violations.length}${
      r.error ? `, err=${r.error}` : ""
    }`
  );

  return r;
});
await browser.close();

if (OUTPUT_JSON) {
  const out = {
    baseUrl: null,
    generatedAt: new Date().toISOString(),
    config: {
      waitUntil: WAIT_UNTIL,
      navTimeoutMs: NAV_TIMEOUT_MS,
      settleWaitMs: WAIT_AFTER_LOAD_MS,
      concurrency: CONCURRENCY,
      betweenUrlMs: BETWEEN_URL_MS,
      userAgent: USER_AGENT,
      acceptLanguage: ACCEPT_LANGUAGE,
      browser: BROWSER,
      verbose: VERBOSE,
      showPolicy: SHOW_POLICY,
      policyMaxLen: POLICY_MAXLEN,
    },
    totals: {
      pages: targets.length,
      violations: results.reduce((acc, r) => acc + r.violations.length, 0),
    },
    results,
  };
  fs.writeFileSync(OUTPUT_FILE, JSON.stringify(out, null, 2), "utf8");
  console.error(`[csp] JSON written: ${path.resolve(OUTPUT_FILE)}`);
}

printFinalReport(results);

process.exit(results.some((r) => r.violations.length > 0) ? 1 : 0);
