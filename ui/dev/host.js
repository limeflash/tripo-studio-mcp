// Test host for the card: a real AppBridge talking to dist/card.html in a
// sandboxed iframe with a strict CSP (no WebAssembly), fed with results from
// the real server through dev/serve.mjs.
import { AppBridge, PostMessageTransport } from "@modelcontextprotocol/ext-apps/app-bridge";

const $ = (s) => document.querySelector(s);
const log = (m) => {
  $("#log").textContent = `${new Date().toLocaleTimeString()}  ${m}\n` + $("#log").textContent;
};
let theme = "light";
let width = Number(new URLSearchParams(location.search).get("w")) || 720;

async function run(name, args, { pendingOnly = false } = {}) {
  const csp = await (await fetch("/meta")).json().then((m) => m.csp);
  const policy = [
    "default-src 'none'",
    "script-src 'unsafe-inline'",
    "style-src 'unsafe-inline'",
    `img-src ${csp.resourceDomains.join(" ")}`,
    `font-src ${csp.resourceDomains.join(" ")}`,
    `connect-src ${csp.connectDomains.join(" ")}`,
  ].join("; ");
  const html = (await (await fetch("/card.html")).text()).replace("<head>", `<head><meta http-equiv="Content-Security-Policy" content="${policy}">`);
  const frame = document.createElement("iframe");
  // ?same=1 renders the card in-process so screenshots can capture it; the
  // default opaque-origin sandbox is what real hosts do.
  frame.sandbox = new URLSearchParams(location.search).get("same") ? "allow-scripts allow-same-origin" : "allow-scripts";
  frame.style.width = width + "px";
  $("#stage").replaceChildren(frame);
  const bridge = new AppBridge(
    null,
    { name: "test-host", version: "0" },
    { openLinks: {}, message: { text: {} } },
    { hostContext: { theme, displayMode: "inline", platform: "desktop", toolInfo: { tool: { name, inputSchema: { type: "object" } } } } }
  );
  bridge.onsizechange = ({ height }) => {
    if (height) frame.style.height = height + "px";
  };
  bridge.onmessage = async ({ content }) => {
    log("ui/message: " + content.map((c) => c.text).join(" "));
    return {};
  };
  bridge.onopenlink = async ({ url }) => {
    log("ui/open-link: " + url);
    return {};
  };
  bridge.onloggingmessage = (p) => log("log: " + JSON.stringify(p.data));
  // Listen before the card loads: it sends ui/initialize as soon as it runs.
  const initialized = new Promise((r) => (bridge.oninitialized = r));
  await bridge.connect(new PostMessageTransport(frame.contentWindow, frame.contentWindow));
  frame.srcdoc = html;
  await initialized;
  bridge.sendToolInput({ arguments: args });
  log(`${name} ${JSON.stringify(args)}`);
  window.lastBridge = bridge;
  if (pendingOnly) return;
  const result = await (await fetch("/call", { method: "POST", body: JSON.stringify({ name, arguments: args }) })).json();
  log(`result: ${result.isError ? "ERROR " : ""}${result.content?.[0]?.text?.split("\n")[0]}`);
  bridge.sendToolResult(result);
}

const PROJECT = "a767c555-9ed9-41d5-90d4-9e44ce91d652";
const VIEWS = "D:/Pictures/build a freak/forms/dummy/remaster/wavespeed/dummy_light/inputs";
const scenarios = {
  "Project (3D)": () => run("tripo_project", { project_id: PROJECT }),
  "Saved job": () =>
    run("tripo_wait", { operator_id: "2ef61d97-9233-480b-b746-1cc9baca518f", project_id: PROJECT, timeout_seconds: 0, download_to: window.DL + "/textured.glb" }),
  "Geometry version": () =>
    run("tripo_download", { project_id: PROJECT, operator_id: "3ff7053f-24bd-4e3d-8dc3-92178b019626", path: window.DL + "/geometry.glb" }),
  Quote: () => run("tripo_generate_model", { views_dir: VIEWS, tier: "smart_mesh", face_limit: 19500, dry_run: true }),
  Gallery: () => run("tripo_list_projects", {}),
  Pending: () => run("tripo_generate_model", { views_dir: VIEWS, tier: "smart_mesh", face_limit: 19500 }, { pendingOnly: true }),
  Error: () => run("tripo_project", { project_id: "does-not-exist" }),
};
for (const [label, fn] of Object.entries(scenarios)) {
  const b = document.createElement("button");
  b.textContent = label;
  b.onclick = () => fn().catch((e) => log("host error: " + e.message));
  $("#scenarios").append(b);
}
$("#theme").onclick = () => {
  theme = theme === "light" ? "dark" : "light";
  document.body.dataset.theme = theme;
  window.lastBridge?.setHostContext({ theme });
};
$("#width").onclick = () => {
  width = width === 720 ? 400 : 720;
  const f = $("#stage iframe");
  if (f) f.style.width = width + "px";
};
fetch("/dl")
  .then((r) => r.text())
  .then((d) => (window.DL = d));
window.run = run;
window.scenarios = scenarios;
