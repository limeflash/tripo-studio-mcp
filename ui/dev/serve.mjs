// node dev/serve.mjs [port]: serves the card test host and drives the real
// server (../tripo-studio-mcp.exe) over MCP stdio for it.
import http from "node:http";
import { spawn } from "node:child_process";
import { readFileSync, mkdirSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { build } from "esbuild";

const port = Number(process.argv[2] || 5181);
const here = (p) => fileURLToPath(new URL(p, import.meta.url));
const dl = join(tmpdir(), "tripo-card-host");
mkdirSync(dl, { recursive: true });

const srv = spawn(here("../../tripo-studio-mcp.exe"), [], { stdio: ["pipe", "pipe", "inherit"] });
let buf = "";
let next = 1;
const waiting = new Map();
srv.stdout.on("data", (d) => {
  buf += d;
  let i;
  while ((i = buf.indexOf("\n")) >= 0) {
    const m = JSON.parse(buf.slice(0, i));
    buf = buf.slice(i + 1);
    waiting.get(m.id)?.(m);
    waiting.delete(m.id);
  }
});
const rpc = (method, params) =>
  new Promise((res) => {
    const id = next++;
    waiting.set(id, res);
    srv.stdin.write(JSON.stringify({ jsonrpc: "2.0", id, method, params }) + "\n");
  });

await rpc("initialize", { protocolVersion: "2025-06-18", capabilities: {}, clientInfo: { name: "card-host", version: "0" } });
const tools = (await rpc("tools/list", {})).result.tools;
const uri = tools.find((t) => t._meta)?._meta.ui.resourceUri;
const resource = (await rpc("resources/read", { uri })).result.contents[0];
const hostJS = (await build({ entryPoints: [here("./host.js")], bundle: true, format: "iife", write: false })).outputFiles[0].text;

http
  .createServer(async (req, res) => {
    const send = (type, body) => {
      res.writeHead(200, { "content-type": type });
      res.end(body);
    };
    if (req.url === "/") return send("text/html", readFileSync(here("./host.html")));
    if (req.url === "/host.js") return send("text/javascript", hostJS);
    if (req.url === "/card.html") return send("text/html", resource.text);
    if (req.url === "/meta") return send("application/json", JSON.stringify(resource._meta.ui));
    if (req.url === "/dl") return send("text/plain", dl.replace(/\\/g, "/"));
    if (req.url === "/call") {
      let body = "";
      for await (const c of req) body += c;
      const r = await rpc("tools/call", JSON.parse(body));
      return send("application/json", JSON.stringify(r.result ?? { isError: true, content: [{ type: "text", text: JSON.stringify(r.error) }] }));
    }
    res.writeHead(404).end();
  })
  .listen(port, () => console.log(`card test host on http://localhost:${port}`));
