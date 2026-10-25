// Tripo Studio result card (an MCP App). Everything it shows arrives in the
// tool result: structuredContent is the card, _meta.tripo holds thumbnails,
// cover and image URLs, and the model itself as base64. It never calls server
// tools, so it works in hosts that do not proxy them (Claude Code).
import { App, applyHostFonts } from "@modelcontextprotocol/ext-apps/app-with-deps";
import { Viewer } from "./viewer.js";

const ICON = {
  logo: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linejoin="round"><path d="M12 2.8 20.5 7.6v8.8L12 21.2 3.5 16.4V7.6Z"/><path d="m3.5 7.6 8.5 4.9 8.5-4.9M12 12.5v8.7" opacity=".55"/></svg>',
  copy: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="9" y="9" width="12" height="12" rx="2.5"/><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"/></svg>',
  open: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M14 4h6v6M20 4l-9 9M18 14v5a1 1 0 0 1-1 1H5a1 1 0 0 1-1-1V7a1 1 0 0 1 1-1h5"/></svg>',
  reset: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M3 12a9 9 0 1 0 3-6.7L3 8"/><path d="M3 3v5h5"/></svg>',
  explode: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M12 9.3l2.6 1.5v3L12 15.3l-2.6-1.5v-3Z"/><path d="M12 2.8v3.4M10.4 4.4 12 2.8l1.6 1.6M12 21.2v-3.4M10.4 19.6l1.6 1.6 1.6-1.6M3.6 7.4l2.9 1.7M3.6 7.4l.5 2.2M3.6 7.4l2.2-.5M20.4 16.6l-2.9-1.7M20.4 16.6l-.5-2.2M20.4 16.6l-2.2.5"/></svg>',
  wire: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linejoin="round"><path d="M12 3 21 8v8l-9 5-9-5V8Z"/><path d="M3 8l18 8M21 8 3 16M12 3v18"/></svg>',
  spin: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M21 12a9 9 0 1 1-3-6.7"/><path d="M21 4v5h-5"/></svg>',
  alert: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="9"/><path d="M12 7.5v5.5M12 16.5h.01"/></svg>',
  send: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M5 12h14M13 6l6 6-6 6"/></svg>',
};

const PENDING = {
  tripo_generate_model: ["Generating model", "Smart Mesh jobs usually take 20–60 s, High Detail a few minutes."],
  tripo_texture: ["Texturing", "Usually 30–90 s."],
  tripo_remesh: ["Remeshing", "Usually under a minute."],
  tripo_segment: ["Splitting into parts", "Usually under a minute."],
  tripo_wait: ["Waiting for the job", ""],
  tripo_download: ["Downloading", ""],
  tripo_list_projects: ["Loading projects", ""],
  tripo_project: ["Loading project", ""],
};

const STATUS = {
  quote: ["Quote", "quote"],
  submitted: ["Submitted", "run"],
  running: ["Running", "run"],
  done: ["Done", "ok"],
  saved: ["Saved", "ok"],
  failed: ["Failed", "bad"],
};

const root = document.getElementById("root");
const app = new App({ name: "Tripo Studio card", version: __VERSION__ }, {}, { autoResize: true });
const state = { input: null, result: null, cancelled: null, started: Date.now(), sel: 0 };
let viewer = null;
let viewerURL = "";
let timer = 0;

// ---- tiny DOM helper ----
function h(tag, props, ...kids) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(props || {})) {
    if (v == null || v === false) continue;
    if (k === "class") el.className = v;
    else if (k === "html") el.innerHTML = v; // only our own ICON constants
    else if (k.startsWith("on")) el.addEventListener(k.slice(2), v);
    else el.setAttribute(k, v === true ? "" : v);
  }
  for (const kid of kids.flat()) if (kid != null && kid !== false) el.append(kid.nodeType ? kid : String(kid));
  return el;
}
const icon = (name, cls) => h("span", { class: cls, html: ICON[name], "aria-hidden": "true" });
const fmt = (n) => Number(n).toLocaleString("en-US");

// ---- host features, each with a fallback ----
function toast(text) {
  const t = document.getElementById("toast");
  t.textContent = text;
  t.classList.add("show");
  clearTimeout(toast.t);
  toast.t = setTimeout(() => t.classList.remove("show"), 2200);
}
async function copy(text, what = "Copied") {
  try {
    await navigator.clipboard.writeText(text);
  } catch {
    const ta = h("textarea", { style: "position:fixed;opacity:0" }, text);
    document.body.append(ta);
    ta.select();
    document.execCommand("copy");
    ta.remove();
  }
  toast(what);
}
async function ask(prompt) {
  if (app.getHostCapabilities()?.message) {
    try {
      await app.sendMessage({ role: "user", content: [{ type: "text", text: prompt }] });
      toast("Sent to the chat");
      return;
    } catch {}
  }
  copy(prompt, "Copied: paste it in the chat");
}
async function openURL(url) {
  if (app.getHostCapabilities()?.openLinks) {
    try {
      await app.openLink({ url });
      return;
    } catch {}
  }
  if (!window.open(url, "_blank", "noopener")) copy(url, "Link copied");
}

// ---- building blocks ----
function header(title, sub, status, cost) {
  const [label, cls] = STATUS[status] || [];
  return h(
    "div",
    { class: "head" },
    h("div", { class: "mark", html: ICON.logo, "aria-hidden": "true" }),
    h("div", { class: "titles" }, h("div", { class: "title" }, title || "Tripo Studio"), sub && h("div", { class: "sub" }, sub)),
    label && h("span", { class: `pill ${cls}` }, h("i"), label),
    cost
  );
}

function costBlock(c, status) {
  if (!c) return null;
  const credits = c.credits < 0 ? "?" : fmt(c.credits);
  const line = c.balance != null ? `${fmt(c.balance)} ${status === "quote" ? "available" : "left"}` : status === "quote" ? "nothing charged" : "";
  return h("div", { class: "cost", title: `${c.breakdown} · prices of ${c.updated}` }, h("b", {}, credits, h("small", {}, "credits")), line && h("span", {}, line));
}

function stage({ model, cover, thumbs, pending, progress }) {
  const el = h("div", { class: "stage" });
  const posterSrc = cover || (thumbs || []).find(Boolean);
  if (posterSrc) el.append(h("img", { class: "poster", src: posterSrc, alt: "" }));
  if (progress != null) el.append(h("div", { class: `progress${progress <= 0 ? " indet" : ""}` }, h("i", { style: `width:${Math.min(progress, 100)}%` })));
  if (pending) {
    el.append(h("div", { class: "center" }, h("div", {}, h("div", { class: "spinner" }), h("b", {}, pending[0]), pending[1])));
    return el;
  }
  if (!model) {
    if (!posterSrc) el.append(h("div", { class: "center" }, h("div", {}, h("b", {}, "No 3D preview"), "The model appears here once the job is done.")));
    return el;
  }
  el.classList.add("loading");
  const center = h("div", { class: "center" }, h("div", {}, h("div", { class: "spinner" }), "Loading 3D preview…"));
  el.append(center);
  mountViewer(el, model, center);
  return el;
}

function mountViewer(el, model, center) {
  const ready = (v) => {
    center.remove();
    el.classList.remove("loading");
    el.classList.add("ready");
    controls(el, v);
  };
  if (model.texture) el.dataset.texture = model.texture;
  const key = `${model.name}:${model.data?.length}:${model.error || ""}`;
  // One viewer (one WebGL context) for the card's lifetime: a re-render moves
  // its canvas into the new stage, a model switch reloads it in place.
  if (viewer) viewer.attach(el);
  else viewer = window.tripoViewer = new Viewer(el); // window.tripoViewer: for debugging in a test host
  const v = viewer;
  if (viewerURL === key && v.model) {
    ready(v);
    return;
  }
  viewerURL = key;
  v.load(model).then(
    () => {
      if (viewerURL !== key) return; // another model was picked meanwhile
      ready(v);
      app.sendLog?.({ level: "info", data: "3D preview loaded" }).catch(() => {});
    },
    (err) => {
      if (viewerURL !== key) return;
      v.clear();
      app.sendLog?.({ level: "error", data: `3D preview failed: ${err?.name}: ${err?.message}` }).catch(() => {});
      el.classList.remove("loading");
      center.replaceChildren(h("div", {}, h("b", {}, "3D preview unavailable"), String(err.message || err).slice(0, 180)));
    }
  );
}

// switcherBlock lets the card flip between the models of one job (batch,
// P2 variants); each has its own preview in _meta.tripo.models.
function switcherBlock(d, meta) {
  const items = d.items || [];
  if (items.length < 2) return null;
  const covers = meta.covers || {};
  return h("div", { class: "switcher", role: "tablist", "aria-label": "Models" }, ...items.map((it, i) =>
    h("button", {
      class: `pick${i === state.sel ? " on" : ""}${it.status === "failed" ? " bad" : ""}`,
      role: "tab", "aria-selected": String(i === state.sel), title: it.saved || it.id,
      onclick: () => { if (state.sel !== i) { state.sel = i; render(); } },
    },
      covers[it.id] ? h("img", { src: covers[it.id], alt: "" }) : h("span", { class: "num" }, String(i + 1)),
      h("span", {}, `Model ${i + 1}`),
      it.status === "failed" ? h("i", { class: "dot" }) : null)));
}

function controls(el, v) {
  const s = v.stats();
  if (el.dataset.texture) s.tex = Number(el.dataset.texture); // the preview's textures are shrunk
  const modes = [["textured", "Textured"], ["clay", "Clay"], ["parts", "Parts"]];
  const seg = h("div", { class: "seg glass", role: "group", "aria-label": "Shading" });
  for (const [m, label] of modes) {
    if (m === "parts" && s.parts < 2) continue;
    seg.append(
      h("button", {
        "aria-pressed": String(v.mode === m),
        onclick: (e) => {
          v.setMode(m);
          seg.querySelectorAll("button").forEach((b) => b.setAttribute("aria-pressed", String(b === e.currentTarget)));
          renderParts();
        },
      }, label)
    );
  }
  const wire = h("button", { class: "icon-btn glass", "aria-label": "Wireframe", title: "Wireframe", "aria-pressed": String(v.wire), html: ICON.wire,
    onclick: (e) => { v.setWire(!v.wire); e.currentTarget.setAttribute("aria-pressed", String(v.wire)); } });
  const reset = h("button", { class: "icon-btn glass", "aria-label": "Reset view", title: "Reset view", html: ICON.reset, onclick: () => v.reset() });
  const explode = s.parts > 1 && h("button", { class: "icon-btn glass", "aria-label": "Explode parts", title: "Explode parts", "aria-pressed": String(v.exploded), html: ICON.explode,
    onclick: (e) => { v.setExploded(!v.exploded); e.currentTarget.setAttribute("aria-pressed", String(v.exploded)); } });
  const facts = [`${fmt(s.tris)} tris`, s.parts > 1 ? `${s.parts} parts` : null, s.tex ? `${s.tex >= 1024 ? s.tex / 1024 + "K" : s.tex + "px"} tex` : null].filter(Boolean);
  el.append(
    h("div", { class: "overlay tl" }, h("div", { class: "hint glass" }, "Drag to orbit · Ctrl + scroll to zoom")),
    h("div", { class: "overlay bl" }, h("div", { class: "stats glass" }, ...facts.map((f) => h("span", {}, f)))),
    h("div", { class: "overlay br" }, seg, explode, wire, reset)
  );
  renderParts();

  function renderParts() {
    const host = root.querySelector("[data-parts]");
    if (!host) return;
    host.replaceChildren();
    const parts = v.parts();
    if (parts.length < 2) return;
    const SHOWN = 16;
    const chip = (p) =>
      h("button", { class: `chip${p.mesh.visible ? "" : " hidden"}`, title: p.name,
        onclick: (e) => e.currentTarget.classList.toggle("hidden", !v.togglePart(p.mesh)) },
        h("i", { style: v.mode === "parts" ? `background:${p.color};border-radius:3px` : "" }), p.name);
    const chips = h("div", { class: "chips" }, ...parts.slice(0, SHOWN).map(chip));
    if (parts.length > SHOWN) {
      const more = h("button", { class: "chip", onclick: () => more.replaceWith(...parts.slice(SHOWN).map(chip)) }, `+ ${parts.length - SHOWN} more`);
      chips.append(more);
    }
    host.append(h("div", { class: "label" }, "Parts", h("em", {}, `${parts.length} · click to hide or show`)), chips);
  }
}

function inputsBlock(labels, thumbs) {
  if (!labels?.length) return null;
  return h("div", {}, h("div", { class: "label" }, "Inputs"),
    h("div", { class: "thumbs" }, ...labels.map((l, i) => h("div", { class: "thumb" }, thumbs?.[i] ? h("img", { src: thumbs[i], alt: l }) : null, h("span", {}, l)))));
}

function viewsBlock(labels, thumbs) {
  return h("div", { class: "views" }, ...labels.map((l, i) =>
    h("button", { class: "view", onclick: () => thumbs?.[i] && lightbox(thumbs[i]) },
      thumbs?.[i] ? h("img", { src: thumbs[i], alt: l }) : null, h("span", { class: "badge" }, l))));
}

function settingsBlock(rows) {
  if (!rows?.length) return null;
  return h("div", { class: "kvs" }, ...rows.map(([k, v]) => h("div", { class: "kv" }, h("small", {}, k), h("b", { title: v }, v))));
}

function flagsBlock(p) {
  if (!p?.flags?.length) return null;
  return h("div", { class: "chips" }, ...p.flags.map((f) =>
    h("span", { class: `chip ${f.on === true ? "on" : f.on === false ? "off" : ""}`, title: f.on == null ? "unknown" : f.on ? "yes" : "no" }, h("i"), f.label)));
}

function factRow(label, value, extra) {
  return h("div", { class: "fact" }, h("small", {}, label), h("code", { title: value }, value),
    h("button", { class: "ghost", title: `Copy ${label.toLowerCase()}`, onclick: () => copy(value, `${label} copied`) }, icon("copy"), "Copy"), extra);
}

function factsBlock(d) {
  // A multi-model job shows the facts of the model picked in the switcher.
  const it = (d.items?.length > 1 && d.kind === "job") ? d.items[state.sel] : null;
  const rows = [];
  for (const path of it ? [it.saved].filter(Boolean) : d.saved || []) rows.push(factRow("Saved", path));
  const pid = it ? it.id : d.project?.id || d.project_id;
  if (pid) {
    const url = it ? it.url : d.project?.url;
    rows.push(factRow("Project", pid, url && h("button", { class: "ghost", onclick: () => openURL(url) }, icon("open"), "Studio")));
  }
  const op = it ? it.operator_id : d.operator_id;
  if (op) rows.push(factRow("Job", op));
  return rows.length ? h("div", { class: "facts" }, ...rows) : null;
}

function actionsBlock(d) {
  const buttons = [];
  if (d.status === "quote" && d.cost) {
    buttons.push(h("button", { class: "btn primary", onclick: () => ask(`Approved: run it (${d.cost.credits} credits).`) },
      icon("send"), "Approve", h("small", {}, `${fmt(d.cost.credits)} credits`)));
  }
  for (const n of d.next || []) buttons.push(h("button", { class: `btn${buttons.length ? "" : " primary"}`, onclick: () => ask(n.prompt) }, icon("send"), n.label));
  if (d.more) buttons.push(h("button", { class: "btn", onclick: () => ask(`Show more Tripo projects (${d.more}).`) }, "More"));
  return buttons.length ? h("div", { class: "actions" }, ...buttons) : null;
}

function alertBlock(text) {
  return text ? h("div", { class: "alert", role: "alert" }, icon("alert"), h("div", {}, text)) : null;
}

function footer(left) {
  return h("div", { class: "foot" }, h("span", {}, left || ""), h("span", {}, `tripo-studio-mcp ${__VERSION__}`));
}

// ---- views ----
function jobView(d, meta, isError) {
  const subtitle = [
    d.project?.name !== d.title ? d.project?.name : null,
    d.settings?.find((s) => s[0] === "Visibility")?.[1] || d.project?.visibility,
  ].filter(Boolean).join(" · ");
  const running = d.status === "submitted" || d.status === "running";
  const progress = running ? d.progress || 0 : null;
  // Before there is a model, the input views are the picture.
  const viewsOnly = !meta.model && !meta.cover && !running && d.inputs?.length;
  const multi = d.items?.length > 1 && Array.isArray(meta.models);
  const it = multi ? d.items[state.sel] : null;
  const model = multi ? meta.models[state.sel] || { error: it?.status === "failed" ? "this model failed" : "no preview for this model yet", name: it?.id } : meta.model;
  const cover = multi ? meta.covers?.[it?.id] : meta.cover;
  return [
    header(d.title, subtitle, isError && !d.status ? "failed" : d.status, costBlock(d.cost, d.status)),
    alertBlock(d.error),
    d.status === "quote" ? h("div", { class: "note" }, "Nothing was uploaded or charged. Approve to run it.") : null,
    viewsOnly ? viewsBlock(d.inputs, meta.thumbs) : stage({ model, cover, thumbs: meta.thumbs, progress,
      pending: running && !multi ? ["Tripo is working on it", "Ask Claude to wait for the job, or pass download_to next time."] : null }),
    switcherBlock(d, meta),
    h("div", { "data-parts": "" }),
    viewsOnly ? null : inputsBlock(d.inputs, meta.thumbs),
    settingsBlock(d.settings),
    flagsBlock(d.project),
    factsBlock(d),
    actionsBlock(d),
    footer(d.cost ? `${d.cost.breakdown} · prices of ${d.cost.updated}` : ""),
  ];
}

function galleryView(d, meta) {
  const covers = meta.covers || {};
  return [
    header(d.title || "Projects", d.summary),
    alertBlock(d.error),
    h("div", { class: "gallery" }, ...(d.items || []).map((p) =>
      h("button", { class: "tile", title: `${p.name}\n${p.id}`, onclick: () => ask(`Show Tripo project ${p.id} (${p.name}).`) },
        h("div", { class: "pic" },
          covers[p.id] ? h("img", { src: covers[p.id], alt: "", loading: "lazy" }) : null,
          p.running ? h("span", { class: "badge live" }, "running") : p.visibility === "public" ? h("span", { class: "badge public" }, "public") : null),
        h("div", { class: "meta" }, h("div", { class: "name" }, p.name || p.id), h("div", { class: "tags" }, h("span", {}, p.type), h("span", {}, p.visibility)))))),
    actionsBlock(d),
    footer("Click a model to open it in the chat"),
  ];
}

function imagesView(d, meta) {
  const urls = meta.images || [];
  return [
    header(d.title, d.summary, d.status, costBlock(d.cost, d.status)),
    alertBlock(d.error),
    h("div", { class: "shots" }, ...urls.map((u, i) =>
      h("button", { class: "shot", onclick: () => lightbox(u) }, h("img", { src: u, alt: d.images?.[i]?.label || "" }), d.images?.[i]?.label ? h("span", { class: "badge" }, d.images[i].label) : null))),
    settingsBlock(d.settings),
    factsBlock(d),
    actionsBlock(d),
    footer(),
  ];
}

function lightbox(src) {
  const box = h("div", { class: "lightbox", onclick: () => box.remove() }, h("img", { src, alt: "" }));
  document.body.append(box);
}

function pendingView() {
  const tool = app.getHostContext()?.toolInfo?.tool?.name || "";
  const [title, hint] = PENDING[tool] || ["Working", ""];
  const a = state.input || {};
  const bits = [a.model_version || a.tier, a.face_limit && `${fmt(a.face_limit)} faces`, a.quality, a.project_id && `project ${String(a.project_id).slice(0, 8)}…`].filter(Boolean);
  const secs = Math.round((Date.now() - state.started) / 1000);
  const elapsed = `${Math.floor(secs / 60)}:${String(secs % 60).padStart(2, "0")}`;
  return [
    header(title, bits.join(" · "), state.cancelled ? "failed" : "running", h("div", { class: "cost" }, h("b", { "data-elapsed": "" }, elapsed))),
    state.cancelled ? alertBlock(`Cancelled: ${state.cancelled}`) : null,
    stage({ pending: [state.cancelled ? "Cancelled" : title, hint], progress: state.cancelled ? null : 0 }),
    h("div", { class: "kvs" }, ...[0, 1, 2].map(() => h("div", { class: "kv sk", style: "height:44px" }))),
    footer(),
  ];
}

function render() {
  clearInterval(timer);
  const r = state.result;
  let view;
  if (!r) {
    view = pendingView();
    timer = setInterval(() => {
      const el = root.querySelector("[data-elapsed]");
      const secs = Math.round((Date.now() - state.started) / 1000);
      if (el) el.textContent = `${Math.floor(secs / 60)}:${String(secs % 60).padStart(2, "0")}`;
    }, 1000);
  } else {
    const d = r.structuredContent;
    const meta = r._meta?.tripo || {};
    const text = (r.content || []).filter((c) => c.type === "text").map((c) => c.text).join("\n");
    if (!d) view = [header("Tripo Studio", "", r.isError ? "failed" : ""), r.isError ? alertBlock(text) : h("div", { class: "note" }, text), footer()];
    else if (d.kind === "error") view = [header(PENDING[d.title]?.[0].replace(/^Loading /, "") || "Tripo Studio", d.title, "failed"), alertBlock(d.error), footer()];
    else if (d.kind === "projects") view = galleryView(d, meta);
    else if (d.kind === "images") view = imagesView(d, meta);
    else view = jobView(d, meta, r.isError);
  }
  root.replaceChildren(...view.filter(Boolean));
}

// The card is always dark (a 3D stage reads best on it); only the host's
// fonts are taken.
function applyContext(ctx) {
  if (ctx?.styles?.css?.fonts) applyHostFonts(ctx.styles.css.fonts);
}

app.onhostcontextchanged = (ctx) => applyContext(ctx);
app.ontoolinput = (p) => {
  state.input = p.arguments || {};
  render();
};
app.ontoolresult = (r) => {
  state.result = r;
  state.sel = 0;
  render();
};
app.ontoolcancelled = (p) => {
  state.cancelled = p?.reason || "the tool call was cancelled";
  render();
};

render();
app.connect().then(() => {
  applyContext(app.getHostContext());
  render();
});
