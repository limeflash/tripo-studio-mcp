// tripo-studio-mcp is a stdio MCP server that drives Tripo Studio with the
// owner's web session, so Claude Code can spend Studio (Max) credits.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
)

const (
	serverName    = "tripo-studio-mcp"
	serverVersion = "0.1.0"
)

const instructions = `Tripo Studio through the owner's web session. It spends Studio (Max subscription) credits, not the Tripo API wallet.
Paid tools (tripo_generate_model, tripo_texture, tripo_remesh, tripo_segment) submit as soon as they are called: get the owner's OK with the cost first. dry_run=true returns the cost without uploading or spending anything.
If a paid call reports "outcome unknown", look at tripo_list_projects before submitting again.
Usual chain: tripo_generate_model (Smart Mesh P1, multiview) -> tripo_texture on the returned project_id -> tripo_download. A paid tool with download_to waits for the job and saves the model in the same call.`

type message struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

type server struct {
	app     *app
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
	s := &server{app: a}
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
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": serverName, "version": serverVersion},
			"instructions":    instructions,
		})
	case "ping":
		s.result(m.ID, map[string]any{})
	case "tools/list":
		s.result(m.ID, map[string]any{"tools": tools})
	case "tools/call":
		s.call(m)
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

	text, err := func() (text string, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("internal error: %v", r)
			}
		}()
		return t.run(ctx, s.app, p.Arguments)
	}()
	if err != nil {
		// Keep what already happened (a submitted job, its ids, its cost) above the error.
		text = strings.TrimSpace(text + "\nError: " + err.Error())
	}
	s.result(m.ID, map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": err != nil,
	})
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
