<div align="center">

# Tripo Studio MCP

**Tripo Studio inside Claude Code, paid with your Studio subscription credits, with live 3D result cards in the chat.**

One Go binary. 54 tools. A job is one call, and results are saved where you say.

[![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![MCP](https://img.shields.io/badge/MCP-stdio%20%2B%20Apps-6E56CF)](https://modelcontextprotocol.io)
[![Claude Code](https://img.shields.io/badge/Claude%20Code-ready-D97757)](https://claude.com/claude-code)
[![License: MIT](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)

English · [Русский](README.ru.md)

<img src="docs/card-project.jpg" width="620" alt="A project card: the textured model in an interactive 3D viewer, with its flags and ids">

</div>

> [!WARNING]
> This is an unofficial project, not affiliated with Tripo or VAST. It uses the internal web API behind studio.tripo3d.ai, which can change without notice. Automated use may conflict with Tripo's terms of service. Use it at your own risk.

## Why

Tripo has two separate wallets:
- **Studio credits** come with the web subscription (Pro, Max, …).
- **API credits** are sold separately for developers.

The official API, `tripo-cli` and the existing MCP servers can only spend API credits, so subscription credits are spent by clicking through the website.

This server sends the same requests as the Studio website, using your browser session. Claude can then generate, texture, split, retopologize, rig and download models, all charged to your subscription.

It is a full rewrite of the unofficial [tripo-studio-plugin](https://github.com/GrinZero/tripo-studio-plugin) (Node, MIT).

| | tripo-studio-plugin 0.3.12 | **tripo-studio-mcp** |
|---|---|---|
| Runtime | Node 20+, about 24 MB of bundles and a 15 MB WASM | **one 10 MB binary** |
| Paid job | staged → 60-second review card → auto-submit | **one call: submits, reports cost and credits left** |
| Result cards in Claude Code | fail with `IPC not available (-32603)`; 3D preview unavailable | **work: everything arrives in the tool result** |
| 3D preview | needs WebAssembly in the widget, or a tool call back to the server | **meshopt decoded in Go; no WASM, no network** |
| Sign-in on Windows | reading browser cookies fails (app-bound encryption) | **import the cookie once** (file, value, or the old plugin's session) |
| Price quotes | the reviewed pricing contract expired, so quotes say *unknown* | **dated price table you can override without a rebuild** |
| Wait and download | separate calls | **`download_to` on any paid tool** |
| Multiview inputs | four explicit paths | **`views_dir`**: picks `*front*`, `*left*`, `*back*`, `*right*` |
| Local Blender tools | written for Blender 3.x/4.x | **updated for Blender 5.x** (projection bake fixed; older Blender untested) |

## The cards

Every visual result renders as an MCP App card in the chat, dark-themed so the models stand out:

- 3D view: orbit, Ctrl + scroll to zoom, Textured / Clay / Parts shading, wireframe.
- Input views, settings, cost and credits left.
- Saved paths and ids with copy buttons, and a link to Studio.
- Next-step buttons, such as *Texture 2K · 10 credits*. They send the prompt to the chat, or copy it where the host does not allow that.

<table>
<tr>
<td width="50%"><img src="docs/card-quote.jpg" alt="A quote card: the four input views, settings, 35 credits, and an Approve button"></td>
<td width="50%"><img src="docs/card-gallery.jpg" alt="The project gallery with cover images and public badges"></td>
</tr>
<tr>
<td align="center"><sub><code>dry_run</code>: the price before anything is spent</sub></td>
<td align="center"><sub><code>tripo_list_projects</code>: your library</sub></td>
</tr>
</table>

The card never calls back into the server, because Claude Code does not proxy those calls. The tool result carries everything it needs:
- **`structuredContent`**: the card data.
- **`_meta`**: input thumbnails, cover URLs, and the model itself, as a GLB with meshopt decoded and textures kept at up to 2048 px (2K arrives untouched; about 2 MB).

## Quick start

**1. Get the binary** from [Releases](../../releases), or build it:

```bash
go build -o tripo-studio-mcp.exe .
```

**2. Add it to Claude Code.** Add `-s user` to make it available in every project.

```bash
claude mcp add tripo-studio -- C:/Tools/tripo-studio-mcp.exe
```

**3. Sign in once.**
1. Sign in at [studio.tripo3d.ai](https://studio.tripo3d.ai) in Chrome or Edge.
2. Open DevTools → Application → Cookies and copy the value of `ory_kratos_session` into a text file.
3. Then ask Claude:

> Import my Tripo session from `C:/Users/me/tripo-cookie.txt`

The cookie is checked against Tripo, stored in `%LOCALAPPDATA%\TripoStudioMCP\session.json`, and never shown. It lasts about 30 days. Delete the text file afterwards.

**4. Make something.**

> Make a Smart Mesh P1 from the views in `./parts/arm/inputs`, 19,500 faces, save it to `./parts/arm/mesh.glb`, then texture it at 2K from the same views and save `textured.glb`.

## Tools

Credits are Studio's own prices as of 2026-10-25 (see [Prices](#prices)). Every paid tool takes:
- `dry_run: true`: returns the price and your balance; nothing is spent.
- `download_to`: waits for the job and saves the result.

**Model generation**

| Tool | Credits | |
|---|---:|---|
| `tripo_generate_model` | **35** P1 · **100** P2 · 15+ H3.1 | Image, multiview, text, batch (up to 30), or Studio image assets. P2 variants 1/2/4. Quad, symmetry and High Detail options. |
| `tripo_texture` | **10** 2K · **20** 4K · **30** 8K, +5 style | From an image, multiview or text. Built-in styles or a style image. Whole model or `part_names`. Checks the delivered resolution. |
| `tripo_generate_pbr` | **5** | Normal, roughness and metallic maps for a textured project |
| `tripo_upscale_texture` | **10** 4K · **20** 8K | |
| `tripo_remesh` | **5/10** plain · **35/40** Smart Poly | Retopology to a face budget, whole model or per part |

**Parts**: Tripo's split-into-parts, made a first-class workflow

| Tool | Credits | |
|---|---:|---|
| `tripo_segment` | **40** | Split into semantic parts (coarse, balanced, fine) |
| `tripo_list_parts` | free | Part names and face counts, read from the GLB |
| `tripo_complete_parts` | **0** | AI completion or quick cap of open parts |

**Images**

| Tool | Credits | |
|---|---:|---|
| `tripo_generate_image` | by model; membership discount and free quota applied | Nano Banana, GPT Image, Midjourney, FLUX… with templates and references |
| `tripo_generate_multiview` | ? | The four views from one image, to feed `tripo_generate_model` |
| `tripo_regenerate_image`, `tripo_split_image` | ? | |
| `tripo_upscale_image` | **10** | 4K |
| `tripo_list_image_assets`, `tripo_get_image_asset`, `tripo_list_image_templates` | free | |

**Rigging, animation, UV, export**

| Tool | Credits | |
|---|---:|---|
| `tripo_rig_model` | **20** | Runs the free rig precheck first. Rig versions v1/v2.5/v3 and skeleton presets. |
| `tripo_animate_model`, `tripo_apply_motion` | ? | Preset animations, or a generated motion, retargeted onto the rig |
| `tripo_generate_motion` | **20** | Text-to-motion, 1–5 stages |
| `tripo_list_animation_presets`, `tripo_list_motions`, `tripo_get_motion` | free | |
| `tripo_get_uv_context`, `tripo_generate_uv`, `tripo_apply_uv` | **20** (free quota first) | Smart UV |
| `tripo_export_model` | 0 | glb, fbx, obj, usdz, stl or 3mf, with every Studio export option |

**Local and free** (Blender: `TRIPO_BLENDER_EXECUTABLE`, or `blender` on PATH)

`tripo_inspect_local_parts` · `tripo_render_model` · `tripo_edit_parts` · `tripo_bake_texture_projection` · `tripo_paint_texture` · `tripo_crop_image`

**Library and account**

`tripo_list_projects` · `tripo_project` · `tripo_wait` · `tripo_download` · `tripo_import_model` · `tripo_balance` · `tripo_list_prices` · `tripo_list_jobs` · `tripo_get_job` · asset groups (`tripo_list_asset_groups`, `tripo_create_asset_group`, `tripo_rename_asset_group`, `tripo_set_asset_group`, `tripo_list_group_assets`) · `tripo_open_in_studio` · `tripo_upload_image` · `tripo_auth_import` / `tripo_auth_status` / `tripo_auth_logout`

*?* means Studio's price config has no entry for it. The quote says *unknown*, and the server tells Claude to ask you first.

## How it works

```mermaid
sequenceDiagram
    participant C as Claude Code
    participant M as tripo-studio-mcp
    participant A as api.tripo3d.ai
    participant S as Tripo storage (S3/CDN)
    C->>M: tripo_generate_model(views_dir, face_limit, download_to)
    M->>A: storage/temporary_token
    M->>S: PUT each view (SigV4, STS token)
    M->>A: audit/image
    M->>A: operation/multiview_to_model
    A-->>M: project_id, operator_id
    loop every 5 s
        M->>A: progress
    end
    M->>A: project/detail
    M->>S: GET model (signed URL)
    Note over M: save the file; decode meshopt and shrink textures for the card
    M-->>C: text for Claude + card + inline model
```

## Safety with paid calls

- **Submitting:** paid tools submit when called. The server tells Claude to get your OK with the price first, and `dry_run` gives that price.
- **A refusal** (for example code 2010, not enough credits) reads *not submitted, nothing charged*.
- **A network failure after the request was sent** is reported as *outcome unknown*, and paid requests are **never retried automatically**. Check `tripo_list_projects` before trying again.
- **Strict arguments:** a misspelt option is an error instead of a silently ignored setting.
- **Job log:** every paid job is logged to `jobs.jsonl` (`tripo_list_jobs`).

## Prices

Prices are kept in [`prices.json`](prices.json), keyed by Studio's own price keys and dated. To change one without rebuilding, write an override to `%LOCALAPPDATA%\TripoStudioMCP\prices.json`:

```json
{ "updated": "2026-11-01", "credits": { "GenerateBaseNexus": 40 } }
```

It is read on every call. Membership discounts for image generation are in the same file.

## Development

```bash
go vet ./... && go test ./...
```

The card lives in `ui/`. After changing it, rebuild it, then rebuild the binary:

```bash
npm --prefix ui install
```

```bash
npm --prefix ui run build
```

`ui/dev/serve.mjs` is a test host: a real `AppBridge` with a strict CSP drives the real server, so you can work on the card in a browser.

| File | What it holds |
|---|---|
| `main.go` | MCP stdio JSON-RPC, tool list, the card resource |
| `tools.go` | shared paid-job flow, waiting, downloads, core tools |
| `tripo.go` | Studio API client, session, S3 upload (SigV4), export, prices |
| `card.go` | the card's data contract, thumbnails, inline model previews |
| `meshopt.go` | `EXT_meshopt_compression` decoder (port of meshoptimizer's reference decoder) |
| `ops_*.go` | operation groups: model, mesh, parts, texture, image, rig, uv, export, local, assets |
| `ui/src` | the card: `card.js`, `viewer.js` (three.js), `card.css` |

## Credits

- The request shapes and Studio contract come from [GrinZero/tripo-studio-plugin](https://github.com/GrinZero/tripo-studio-plugin) (MIT), whose authors mapped the Studio API first. Its Blender worker is included in `blender/worker.py`.
- The meshopt decoder is ported from [meshoptimizer](https://github.com/zeux/meshoptimizer) (MIT).
- The card uses [three.js](https://threejs.org) and the [MCP Apps SDK](https://github.com/modelcontextprotocol/ext-apps).

MIT © 2026 limeflash. See [LICENSE](LICENSE).
