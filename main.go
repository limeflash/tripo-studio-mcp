// tripo-studio-mcp is a stdio MCP server that drives Tripo Studio with the
// owner's web session, so Claude Code can spend Studio (Max) credits.
package main

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
)

const (
	serverName    = "tripo-studio-mcp"
	serverVersion = "0.3.0"
)

const instructions = `Tripo Studio through the owner's web session. It spends Studio (Max subscription) credits, not the Tripo API wallet.
Paid tools (their descriptions start with PAID) submit as soon as they are called: get the owner's OK with the cost first. dry_run=true returns the cost without uploading or spending anything. When the price table does not know a cost it says "unknown": ask the owner before submitting.
If a paid call reports "outcome unknown", look at tripo_list_projects before submitting again.
Model: tripo_generate_model (Smart Mesh P1, multiview) -> tripo_texture on the returned project_id -> tripo_download. A paid tool with download_to waits for the job and saves the model in the same call.
Parts: tripo_segment -> tripo_list_parts -> tripo_complete_parts -> tripo_texture / tripo_remesh with part_names.
Images: tripo_generate_image -> tripo_generate_multiview -> tripo_generate_model mode=studio_multiview.
Free and local: tripo_inspect_local_parts, tripo_render_model, tripo_edit_parts, tripo_bake_texture_projection (Blender), tripo_paint_texture, tripo_crop_image.
Results also render as a card in the chat; keep your own reply short and do not repeat what the card shows.`

// The result widget (MCP Apps). ui/build.mjs writes it; go generate rebuilds it.
//
//go:generate npm --prefix ui run build
//go:embed ui/dist/card.html
var cardHTML string

const appMIME = "text/html;profile=mcp-app"

type message struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

type server struct {
	app     *app
	cardURI string
	mu      sync.Mutex // one JSON line at a time on stdout
	cancels sync.Map   // request id -> context.CancelFunc
	running sync.WaitGroup
}

func main() {
	log.SetFlags(0)
	log.SetPrefix(serverName + ": ")
	a, err := newApp()
	if err != nil {
		log.Fatal(err)
	}
	// The URI changes with each widget build, so a host never keeps a stale copy.
	s := &server{app: a, cardURI: fmt.Sprintf("ui://tripo-studio/card-%s.html", sha256Hex([]byte(cardHTML))[:10])}
	in := bufio.NewReader(os.Stdin)
	for {
		line, err := in.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			var m message
			if json.Unmarshal(line, &m) != nil {
				s.send(map[string]any{"jsonrpc": "2.0", "id": nil, "error": map[string]any{"code": -32700, "message": "parse error"}})
			} else {
				s.running.Add(1)
				go func() { defer s.running.Done(); s.handle(m) }()
			}
		}
		if err != nil {
			s.running.Wait() // stdin closed: finish what is in flight, then exit
			return
		}
	}
}

func (s *server) handle(m message) {
	switch m.Method {
	case "":
		// a response to a request we never send
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(m.Params, &p)
		if p.ProtocolVersion == "" {
			p.ProtocolVersion = "2025-06-18"
		}
		s.result(m.ID, map[string]any{
			"protocolVersion": p.ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}, "resources": map[string]any{}},
			"serverInfo":      map[string]any{"name": serverName, "version": serverVersion},
			"instructions":    instructions,
		})
	case "ping":
		s.result(m.ID, map[string]any{})
	case "tools/list":
		s.result(m.ID, map[string]any{"tools": s.toolList()})
	case "tools/call":
		s.call(m)
	case "resources/list":
		s.result(m.ID, map[string]any{"resources": []map[string]any{{
			"uri": s.cardURI, "name": "Tripo result card", "mimeType": appMIME,
			"description": "Job, project and gallery card with a 3D preview.",
		}}})
	case "resources/read":
		var p struct {
			URI string `json:"uri"`
		}
		if json.Unmarshal(m.Params, &p) != nil || p.URI != s.cardURI {
			s.fail(m.ID, -32002, "resource not found: "+p.URI)
			return
		}
		s.result(m.ID, map[string]any{"contents": []map[string]any{{
			"uri": s.cardURI, "mimeType": appMIME, "text": cardHTML, "_meta": s.cardMeta(),
		}}})
	case "notifications/cancelled":
		var p struct {
			RequestID json.RawMessage `json:"requestId"`
		}
		if json.Unmarshal(m.Params, &p) == nil {
			if cancel, ok := s.cancels.Load(string(p.RequestID)); ok {
				cancel.(context.CancelFunc)()
			}
		}
	default:
		if len(m.ID) > 0 {
			s.fail(m.ID, -32601, "method not found: "+m.Method)
		}
	}
}

// cardMeta is the widget's sandbox policy: models and thumbnails arrive
// inline in the tool result, covers and generated images from Tripo's CDN.
func (s *server) cardMeta() map[string]any {
	return map[string]any{"ui": map[string]any{
		"prefersBorder": true,
		"csp": map[string]any{
			"connectDomains":  []string{"data:", "blob:"},
			"resourceDomains": []string{"https://*.tripo3d.com", "https://*.tripo3d.ai", "data:", "blob:"},
		},
	}}
}

func (s *server) toolList() []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		m := map[string]any{"name": t.Name, "description": t.Description, "inputSchema": t.InputSchema}
		if t.UI {
			m["_meta"] = map[string]any{
				"ui":             map[string]any{"resourceUri": s.cardURI, "visibility": []string{"model", "app"}},
				"ui/resourceUri": s.cardURI, // the pre-GA key some hosts still read
			}
		}
		out = append(out, m)
	}
	return out
}

func (s *server) call(m message) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(m.Params, &p); err != nil {
		s.fail(m.ID, -32602, "bad tools/call params")
		return
	}
	var t *tool
	for i := range tools {
		if tools[i].Name == p.Name {
			t = &tools[i]
		}
	}
	if t == nil {
		s.fail(m.ID, -32602, "unknown tool: "+p.Name)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	key := string(m.ID)
	s.cancels.Store(key, cancel)
	defer func() { s.cancels.Delete(key); cancel() }()

	r, err := func() (r reply, err error) {
		defer func() {
			if v := recover(); v != nil {
				err = fmt.Errorf("internal error: %v", v)
			}
		}()
		return t.run(ctx, s.app, p.Arguments)
	}()
	if err != nil {
		// Keep what already happened (a submitted job, its ids, its cost) above the error.
		r.Text = strings.TrimSpace(r.Text + "\nError: " + err.Error())
		if t.UI && r.Card == nil {
			r.Card = &card{Kind: "error", Title: t.Name}
		}
		if r.Card != nil {
			r.Card.Error = err.Error()
		}
	}
	res := map[string]any{"content": []map[string]any{{"type": "text", "text": r.Text}}, "isError": err != nil}
	if r.Card != nil {
		r.Card.Text = r.Text
		res["structuredContent"] = r.Card
	}
	if r.Meta != nil {
		res["_meta"] = map[string]any{"tripo": r.Meta}
	}
	s.result(m.ID, res)
}

func (s *server) result(id json.RawMessage, result any) {
	s.send(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (s *server) fail(id json.RawMessage, code int, msg string) {
	s.send(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": msg}})
}

func (s *server) send(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		log.Printf("encode: %v", err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	os.Stdout.Write(append(b, '\n'))
}
