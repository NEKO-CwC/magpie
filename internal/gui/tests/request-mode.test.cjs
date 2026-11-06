// The gateway's request handling is a choice on this computer. Exercise the
// real Settings page with a local API fixture: default, save, reload, a failed
// save, keyboard input and search, in every language and both browser engines.
const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const path = require("node:path");
const { test } = require("node:test");
const { chromium, webkit } = require("playwright");
const { reach } = require("./reader.cjs");

const assets = path.resolve(process.env.MAGPIE_REQUEST_MODE_ASSETS || path.join(__dirname, "../assets"));
const names = {
  en: ["Request handling mode", "Passthrough (default)", "Original handling"],
  zh: ["请求处理模式", "透传（默认）", "原有处理"],
  "zh-TW": ["請求處理模式", "透傳（預設）", "原有處理"],
  ja: ["リクエスト処理モード", "透過（既定）", "従来の処理"],
  de: ["Anfrageverarbeitung", "Durchreichen (Standard)", "Bisherige Verarbeitung"],
};

function server(lang) {
  const f = {
    cur: { theme: "light", lang, tray: "panel", textSize: 100, currency: "usd", lanURLs: [], visionModels: [], imageGenModels: [] },
    posts: [], fail: false,
  };
  f.route = async (route) => {
    const req = route.request(), url = new URL(req.url());
    const json = (data, status = 200) => route.fulfill({ status, json: data });
    if (url.pathname === "/boot.js") return route.fulfill({ contentType: "text/javascript", body: `window.bootPrefs = ${JSON.stringify({ lang, theme: "light", web: false })};` });
    if (url.pathname === "/wails/runtime.js") return route.fulfill({ contentType: "text/javascript", body: "export const Window = {};" });
    if (url.pathname === "/api/state") return json({ agents: [], profiles: [], settings: f.cur });
    if (url.pathname === "/api/settings") {
      if (req.method() === "POST") {
        const body = req.postDataJSON();
        f.posts.push(body);
        if (f.fail) { f.fail = false; return json({ error: "save blocked" }, 400); }
        Object.assign(f.cur, body);
      }
      return json(f.cur);
    }
    if (url.pathname === "/api/providers") return json({ providers: [], presets: [], excluded: [], gateway: { running: true, window: true, url: "http://127.0.0.1:3999" } });
    if (url.pathname === "/api/groups") return json({ models: [], groups: [], pools: [] });
    if (url.pathname === "/api/plugins") return json({ plugins: [] });
    if (url.pathname === "/api/usage/quotas") return json([]);
    if (url.pathname.startsWith("/api/")) return json({});
    const file = path.join(assets, url.pathname === "/" ? "index.html" : url.pathname);
    const contentType = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml", ".png": "image/png" }[path.extname(file)];
    return route.fulfill({ body: await fs.readFile(file), contentType });
  };
  return f;
}

for (const engine of process.env.BROWSER ? [process.env.BROWSER] : ["chromium", "webkit"]) {
  for (const [lang, [title, passthrough, managed]] of Object.entries(names)) for (const width of [1000, 390]) {
    test(`${engine} ${lang} ${width}px: request mode defaults to passthrough and persists without a restart`, async (t) => {
      const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
      t.after(() => browser.close());
      const page = await (await browser.newContext({ viewport: { width, height: 760 }, reducedMotion: "reduce" })).newPage();
      page.setDefaultTimeout(5000);
      page.setDefaultNavigationTimeout(60000);
      const errors = [], navigationErrors = [], f = server(lang);
      page.on("pageerror", (e) => {
        // WebKit on Windows reports a fake-origin reload cancelling these
        // unrelated background reads as access-control errors (also covered
        // in deprecated-subscriptions.test.cjs). Keep Settings save errors.
        if (engine === "webkit" && /magpie\.test\/api\/(web-page|update\?fresh=1&lang=[\w-]+) due to access control checks\.$/.test(e.message)) navigationErrors.push(e.message);
        else errors.push(e.message);
      });
      await page.route("**/*", f.route);
      await page.goto("http://magpie.test/?view=settings&tab=network");
      const box = page.locator("#requestModeSegs"), row = page.locator("#requestModeRow");
      const choice = (name) => box.getByRole("button", { name, exact: true });
      await choice(passthrough).waitFor();
      assert.equal(await row.locator(".name").innerText(), title);
      assert.equal(await choice(passthrough).getAttribute("aria-pressed"), "true", "a missing setting is passthrough");
      assert.equal(await row.locator("select").count(), 0);
      assert.ok((await row.locator(".sub").textContent()).length > 20, "the behavior and machine scope are explained");
      const save = async (name, mode, fail = false) => {
        const count = f.posts.length;
        f.fail = fail;
        await reach(page, choice(name));
        const response = page.waitForResponse((r) => r.url().endsWith("/api/settings") && r.request().method() === "POST");
        await choice(name).focus();
        await choice(name).press("Space");
        await response;
        // A save redraws Settings and reads its other rows. Finish those
        // local reads before reload; WebKit reports an aborted read as CORS.
        await page.waitForLoadState("networkidle");
        const selected = fail ? managed : name;
        await page.waitForFunction((text) => document.querySelector("#requestModeSegs .opt.on")?.textContent === text, selected);
        assert.equal(f.posts.length, count + 1, "one settings save per choice");
        assert.equal(f.posts.at(-1).requestMode, mode);
      };

      await save(managed, "managed");
      await page.reload();
      await choice(managed).waitFor();
      assert.equal(await choice(managed).getAttribute("aria-pressed"), "true", "the saved choice survives a reload");
      await save(passthrough, "passthrough", true);
      await page.waitForFunction(() => document.querySelector("#status")?.textContent.includes("save blocked"));
      assert.equal(f.cur.requestMode, "managed", "a failed save keeps the server's choice");
      await save(passthrough, "passthrough");
      await page.reload();
      await choice(passthrough).waitFor();
      assert.equal(await choice(passthrough).getAttribute("aria-pressed"), "true");

      // The standard Settings search finds this row by its English name in
      // every language; a new setting needs no separate search index.
      await page.locator("#setQ").fill("Request handling mode");
      await page.waitForFunction(() => document.querySelector("#requestModeRow")?.getClientRects().length > 0 && document.querySelector("#setTabs")?.getClientRects().length === 0);
      assert.equal(await row.getAttribute("class").then((c) => c.includes("set-miss")), false);
      assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false, "no horizontal overflow");
      assert.deepEqual(errors, []);
      if (navigationErrors.length) t.diagnostic(`WebKit reload cancelled ${navigationErrors.length} unrelated background reads on the fake origin`);
      if (process.env.ARTIFACT_DIR) {
        await fs.mkdir(process.env.ARTIFACT_DIR, { recursive: true });
        await page.screenshot({ path: path.join(process.env.ARTIFACT_DIR, `${engine}-request-mode-${lang}-${width}.png`) });
      }
    });
  }
}
