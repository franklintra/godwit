// webboot.ts: the web half of perfbench. It measures what the web pages
// cost to start: bundle sizes (what each page loads at startup, raw and
// gzip, and what loads only later), and loads of reader.html and wit.html in
// Chromium's headless shell over the DevTools protocol, with a stand-in for
// MyGo's bridge (globalThis.mygo) that answers the pages' boot calls from
// recorded responses. No npm dependency: Bun's own server, gzip and
// WebSocket.
//
// bun scripts/perfbench/web/webboot.ts --dist web/dist --chrome <headless_shell>
//   --body body.json --runs 5 --out web.json
import { readdirSync, readFileSync, statSync, mkdtempSync, rmSync } from "node:fs";
import { join, extname } from "node:path";
import { tmpdir } from "node:os";

type Metric = { unit: string; better: string; desc: string; samples: number[] };
const metrics: Record<string, Metric> = {};
const notes: string[] = [];
const add = (name: string, unit: string, better: string, desc: string, v: number) => {
  metrics[name] ??= { unit, better, desc, samples: [] };
  metrics[name].samples.push(v);
};

const args = new Map<string, string>();
for (let i = 2; i < process.argv.length; i += 2) args.set(process.argv[i].replace(/^--/, ""), process.argv[i + 1]);
const dist = args.get("dist")!;
const chrome = args.get("chrome")!;
const runs = Number(args.get("runs") ?? "5");
const out = args.get("out")!;
const body = args.get("body") ? JSON.parse(readFileSync(args.get("body")!, "utf8")) : { html: "<p>hello</p>", plain: "hello", message: 1 };

// ---- bundle sizes ----
const gz = (b: Uint8Array) => Bun.gzipSync(b).length;
const assetsDir = join(dist, "assets");
const allAssets = readdirSync(assetsDir).filter((f) => [".js", ".css"].includes(extname(f)));
const startupOf = (html: string) => {
  const src = readFileSync(join(dist, html), "utf8");
  const refs = new Set<string>();
  for (const m of src.matchAll(/(?:src|href)="\/?(assets\/[^"]+\.(?:js|css))"/g)) refs.add(m[1]);
  return [...refs];
};
const pages = readdirSync(dist).filter((f) => f.endsWith(".html"));
const atStartup = new Set<string>();
for (const p of pages) {
  const refs = startupOf(p);
  let raw = 0, z = 0;
  for (const r of refs) {
    const b = readFileSync(join(dist, r));
    raw += b.length;
    z += gz(b);
    atStartup.add(r.replace(/^assets\//, ""));
  }
  const name = p.replace(/\.html$/, "");
  add(`bundle.${name}.startup_kb`, "KB", "lower", `JS+CSS ${p} loads before it runs (entry, modulepreload, stylesheets)`, raw / 1024);
  add(`bundle.${name}.startup_gzip_kb`, "KB", "lower", `the same, gzip`, z / 1024);
  add(`bundle.${name}.startup_files`, "files", "lower", `files ${p} loads at startup`, refs.length);
}
let total = 0, totalZ = 0, lazy = 0, lazyZ = 0;
for (const f of allAssets) {
  const b = readFileSync(join(assetsDir, f));
  total += b.length;
  totalZ += gz(b);
  if (!atStartup.has(f)) {
    lazy += b.length;
    lazyZ += gz(b);
  }
}
add("bundle.all.kb", "KB", "lower", "every JS and CSS asset of web/dist", total / 1024);
add("bundle.all.gzip_kb", "KB", "lower", "the same, gzip", totalZ / 1024);
add("bundle.lazy.kb", "KB", "lower", "JS and CSS no page loads at startup (dynamic imports)", lazy / 1024);
add("bundle.lazy.gzip_kb", "KB", "lower", "the same, gzip", lazyZ / 1024);

// ---- the pages in Chromium ----
const server = Bun.serve({
  hostname: "127.0.0.1",
  port: 0,
  fetch(req) {
    const path = new URL(req.url).pathname;
    const f = join(dist, path === "/" ? "index.html" : path);
    try {
      if (statSync(f).isFile()) return new Response(Bun.file(f));
    } catch {}
    return new Response("not found", { status: 404 });
  },
});
const origin = `http://127.0.0.1:${server.port}`;

const theme = { mode: "light", light: {}, dark: {}, fontSize: 13, bodyFontSize: 14 };
// The bridge stand-in: records calls (window.__pb.calls) and the time the
// page acknowledged painted content (ReaderPage.Painted).
const stub = (bodyJSON: string) => `(() => {
  const body = ${bodyJSON};
  const theme = ${JSON.stringify(theme)};
  const pb = (window.__pb = { calls: [], painted: 0, errors: [] });
  const answers = {
    "ReaderPage.Binding": () => ({ id: 1, active: true }),
    "ReaderPage.Load": () => ({ binding: 1, message: body.message, version: 1, html: body.html, plain: body.plain || "", remote: false, blocked: 0, cut: false, theme, locale: "en", collapseQuotes: true }),
    "ReaderPage.Painted": () => { if (!pb.painted) pb.painted = performance.now(); return null; },
    "ReaderPage.Height": () => null,
    "WitPage.Load": () => ({ conversation: 0, version: 0, model: "", locale: "en", theme }),
    "WitPage.Models": () => [],
    "WitPage.Skills": () => [],
  };
  const call = (method, ...a) => {
    pb.calls.push([method, performance.now()]);
    const f = answers[method];
    return Promise.resolve(f ? f(...a) : null);
  };
  window.mygo = {
    call,
    callWith: (method, a) => call(method, ...(a || [])),
    channel: () => ({ postMessage() {}, close() {} }),
    on: () => () => {},
    once: () => () => {},
    locale: "en",
    dir: "ltr",
    preferences: { reduceMotion: false, highContrast: false, reduceTransparency: false },
    windowActive: true,
    window: { minimize() {}, maximize() {}, unmaximize() {}, toggleMaximize() {}, isMaximized() { return false; }, toggleFullScreen() {}, close() {}, setTitle() {} },
  };
  addEventListener("error", (e) => pb.errors.push(String(e.message)));
  addEventListener("unhandledrejection", (e) => pb.errors.push(String(e.reason)));
})();`;

const profile = mkdtempSync(join(tmpdir(), "perfbench-chrome-"));
const proc = Bun.spawn([chrome, "--headless", "--no-sandbox", "--disable-gpu", "--remote-debugging-port=0", `--user-data-dir=${profile}`, "--no-first-run", "--disable-background-networking", "about:blank"], { stderr: "pipe", stdout: "ignore" });
let wsURL = "";
{
  const reader = proc.stderr.getReader();
  let buf = "";
  const deadline = Date.now() + 20000;
  while (!wsURL && Date.now() < deadline) {
    const { value, done } = await reader.read();
    if (done) break;
    buf += new TextDecoder().decode(value);
    const m = buf.match(/DevTools listening on (ws:\/\/\S+)/);
    if (m) wsURL = m[1];
  }
  reader.releaseLock();
}
if (!wsURL) throw new Error("headless shell gave no DevTools URL");

const ws = new WebSocket(wsURL);
await new Promise((r, j) => { ws.onopen = r; ws.onerror = j; });
let nextID = 1;
const waiting = new Map<number, (m: any) => void>();
const listeners: ((m: any) => void)[] = [];
ws.onmessage = (e) => {
  const m = JSON.parse(String(e.data));
  if (m.id && waiting.has(m.id)) {
    waiting.get(m.id)!(m);
    waiting.delete(m.id);
  } else for (const l of listeners) l(m);
};
const send = (method: string, params: any = {}, sessionId?: string): Promise<any> =>
  new Promise((res, rej) => {
    const id = nextID++;
    waiting.set(id, (m) => (m.error ? rej(new Error(`${method}: ${m.error.message}`)) : res(m.result)));
    ws.send(JSON.stringify({ id, method, params, sessionId }));
  });

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

async function loadPage(page: string, readyExpr: string): Promise<void> {
  const { targetId } = await send("Target.createTarget", { url: "about:blank" });
  const { sessionId } = await send("Target.attachToTarget", { targetId, flatten: true });
  const s = (m: string, p: any = {}) => send(m, p, sessionId);
  await s("Page.enable");
  await s("Runtime.enable");
  await s("Performance.enable", { timeDomain: "timeTicks" });
  await s("Network.enable");
  await s("Network.clearBrowserCache");
  await s("Network.setCacheDisabled", { cacheDisabled: true });
  await s("Emulation.setDeviceMetricsOverride", { width: 900, height: 820, deviceScaleFactor: 2, mobile: false });
  await s("Page.addScriptToEvaluateOnNewDocument", { source: stub(JSON.stringify(body)) });
  const loaded = new Promise<void>((r) => {
    const l = (m: any) => {
      if (m.sessionId === sessionId && m.method === "Page.loadEventFired") r();
    };
    listeners.push(l);
  });
  await s("Page.navigate", { url: `${origin}/${page}.html` });
  await loaded;
  const deadline = Date.now() + 15000;
  let ready = 0;
  while (Date.now() < deadline) {
    const { result } = await s("Runtime.evaluate", { expression: readyExpr, returnByValue: true });
    if (result.value) {
      ready = result.value;
      break;
    }
    await sleep(5);
  }
  // two frames after the content, then let late work finish
  await s("Runtime.evaluate", { expression: "new Promise(r => requestAnimationFrame(() => requestAnimationFrame(r)))", awaitPromise: true });
  await sleep(300);
  const { result } = await s("Runtime.evaluate", {
    returnByValue: true,
    expression: `(() => {
      const n = performance.getEntriesByType("navigation")[0];
      const fcp = performance.getEntriesByName("first-contentful-paint")[0];
      const res = performance.getEntriesByType("resource");
      return { dcl: n.domContentLoadedEventEnd, load: n.loadEventEnd, fcp: fcp ? fcp.startTime : 0,
        scripts: res.filter(r => r.initiatorType === "script" || r.name.endsWith(".js")).length,
        bytes: res.reduce((a, r) => a + (r.decodedBodySize || 0), 0),
        calls: window.__pb.calls.map(c => c[0]), errors: window.__pb.errors };
    })()`,
  });
  const v = result.value;
  const { metrics: pm } = await s("Performance.getMetrics");
  const get = (k: string) => pm.find((x: any) => x.name === k)?.value ?? 0;
  const heap = await s("Runtime.getHeapUsage");
  add(`web.${page}.dom_content_loaded_ms`, "ms", "lower", `${page}.html: navigation start to DOMContentLoaded`, v.dcl);
  add(`web.${page}.load_ms`, "ms", "lower", `${page}.html: navigation start to load`, v.load);
  add(`web.${page}.fcp_ms`, "ms", "lower", `${page}.html: first contentful paint`, v.fcp);
  add(`web.${page}.ready_ms`, "ms", "lower", `${page}.html: the page's content is up (${page === "reader" ? "it called ReaderPage.Painted" : "#root has content"})`, ready);
  add(`web.${page}.script_ms`, "ms", "lower", `${page}.html: V8 compile + run time to then (Performance ScriptDuration)`, get("ScriptDuration") * 1000);
  add(`web.${page}.task_ms`, "ms", "lower", `${page}.html: main-thread task time to then (TaskDuration)`, get("TaskDuration") * 1000);
  add(`web.${page}.layout_style_ms`, "ms", "lower", `${page}.html: layout + style recalculation`, (get("LayoutDuration") + get("RecalcStyleDuration")) * 1000);
  add(`web.${page}.js_heap_mb`, "MB", "lower", `${page}.html: JS heap used after boot`, heap.usedSize / 1048576);
  add(`web.${page}.dom_nodes`, "count", "lower", `${page}.html: DOM nodes after boot`, get("Nodes"));
  add(`web.${page}.decoded_kb`, "KB", "lower", `${page}.html: bytes of resources it loaded (decoded)`, v.bytes / 1024);
  if (v.errors.length) notes.push(`${page}: page errors with the stub bridge: ${v.errors.slice(0, 3).join(" | ")}`);
  notes.push(`${page}: boot calls ${JSON.stringify(v.calls.slice(0, 12))}`);
  await send("Target.closeTarget", { targetId });
}

for (let i = 0; i < runs; i++) {
  await loadPage("reader", "window.__pb && window.__pb.painted");
  await loadPage("wit", "(() => { const r = document.getElementById('root'); return r && r.childElementCount > 0 ? performance.now() : 0 })()");
}
ws.close();
proc.kill();
server.stop(true);
rmSync(profile, { recursive: true, force: true });
await Bun.write(out, JSON.stringify({ metrics, notes: [...new Set(notes)] }, null, 2));
