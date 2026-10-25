package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

// reply is what a tool returns: Text for Claude, Card for the result widget
// (structuredContent, kept out of the model's context by MCP Apps hosts) and
// Meta for widget-only data such as thumbnails and preview URLs (_meta).
type reply struct {
	Text string
	Card *card
	Meta map[string]any
}

func say(s string) reply { return reply{Text: s} }

func (r *reply) line(format string, args ...any) {
	if r.Text != "" && !strings.HasSuffix(r.Text, "\n") {
		r.Text += "\n"
	}
	r.Text += fmt.Sprintf(format, args...)
}

func (r *reply) meta(key string, v any) {
	if r.Meta == nil {
		r.Meta = map[string]any{}
	}
	r.Meta[key] = v
}

// card is the result widget's data contract; ui/src/card.js renders it.
type card struct {
	Kind       string         `json:"kind"`             // job | project | projects | images
	Title      string         `json:"title"`            // e.g. "Smart Mesh P1.0"
	Status     string         `json:"status,omitempty"` // quote | submitted | running | done | failed | saved
	Summary    string         `json:"summary,omitempty"`
	Progress   float64        `json:"progress,omitempty"`
	Cost       *cardCost      `json:"cost,omitempty"`
	Settings   [][2]string    `json:"settings,omitempty"`
	Inputs     []string       `json:"inputs,omitempty"` // labels; thumbnails in _meta.thumbs, same order
	ProjectID  string         `json:"project_id,omitempty"`
	OperatorID string         `json:"operator_id,omitempty"`
	Project    *projectCard   `json:"project,omitempty"`
	Saved      []string       `json:"saved,omitempty"`
	Images     []cardImage    `json:"images,omitempty"` // generated images; URLs in _meta.images
	Items      []*projectCard `json:"items,omitempty"`  // kind=projects
	Next       []cardAction   `json:"next,omitempty"`
	More       string         `json:"more,omitempty"` // e.g. "offset=20"
	Error      string         `json:"error,omitempty"`
}

type cardCost struct {
	Credits   int    `json:"credits"` // -1 when unknown
	Breakdown string `json:"breakdown"`
	Updated   string `json:"updated"`
	Balance   *int   `json:"balance,omitempty"` // credits left after (or before, for a quote)
}

type projectCard struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	OperatorID string     `json:"operator_id,omitempty"`
	Visibility string     `json:"visibility,omitempty"`
	Type       string     `json:"type,omitempty"`
	Flags      []cardFlag `json:"flags,omitempty"`
	Running    bool       `json:"running,omitempty"`
	URL        string     `json:"url,omitempty"` // the project in Studio's workspace
}

func studioURL(projectID string) string {
	return studioOrigin + "/workspace/generate/" + url.PathEscape(projectID)
}

type cardFlag struct {
	Label string `json:"label"`
	On    *bool  `json:"on"`
}

type cardImage struct {
	Label string `json:"label"`
	ID    string `json:"id,omitempty"`
}

// cardAction is a follow-up the widget offers as a button; it sends Prompt
// to the chat when the host allows it, or copies it.
type cardAction struct {
	Label  string `json:"label"`
	Prompt string `json:"prompt"`
}

var projectFlags = []struct{ key, label string }{
	{"is_textured", "textured"}, {"is_pbr", "PBR"}, {"is_quad", "quad"},
	{"is_nexus_mesh", "Smart Mesh"}, {"is_segmented", "segmented"}, {"is_rigged", "rigged"},
}

func (p *project) card() *projectCard {
	pc := &projectCard{ID: p.ID, Name: p.Name, OperatorID: p.OperatorID, Visibility: asString(p.raw["visibility"]), URL: studioURL(p.ID)}
	for _, f := range projectFlags {
		pc.Flags = append(pc.Flags, cardFlag{f.label, p.flag(f.key)})
	}
	return pc
}

// cover returns the project's signed cover image URL, or "".
func (p *project) cover() string {
	if op, ok := p.raw["operator"].(map[string]any); ok {
		if list, ok := op["cover_image"].([]any); ok && len(list) > 0 {
			return asString(list[0])
		}
	}
	if list, ok := p.raw["cover_image"].([]any); ok && len(list) > 0 {
		return asString(list[0])
	}
	return ""
}

// showProject puts a project, its cover and a 3D preview of its model on
// the reply's card.
func (a *app) showProject(r *reply, p *project) {
	if r.Card == nil {
		r.Card = &card{Kind: "project", Title: p.Name}
	}
	r.Card.Project = p.card()
	r.Card.ProjectID = p.ID
	if u := p.cover(); u != "" {
		r.meta("cover", u)
	}
	if p.ModelURL != "" {
		r.meta("model", map[string]any{"url": a.preview.remote(p.ModelURL), "name": p.Name})
	}
}

// ---- thumbnails ----

// thumb shrinks an input image to a small data URL for the card.
func thumb(img *localImage) string {
	src, _, err := image.Decode(bytes.NewReader(img.data))
	if err != nil {
		return ""
	}
	const edge = 192
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w > edge || h > edge {
		if w >= h {
			w, h = edge, max(1, h*edge/w)
		} else {
			w, h = max(1, w*edge/h), edge
		}
	}
	dst := image.NewNRGBA(image.Rect(0, 0, w, h))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, b, draw.Src, nil)
	var buf bytes.Buffer
	if dst.Opaque() {
		if jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 82}) != nil {
			return ""
		}
		return "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
	}
	if png.Encode(&buf, dst) != nil {
		return ""
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}

// ---- local preview server ----

// previewServer serves models to the widget from 127.0.0.1. Tripo's CDN
// sends no CORS headers, so the widget cannot fetch a model URL itself.
type previewServer struct {
	origin string // http://127.0.0.1:PORT, "" if it could not start
	app    *app
	mu     sync.Mutex
	items  map[string]previewItem
}

type previewItem struct {
	path string // a local file, or
	url  string // a signed Tripo URL, fetched on demand
}

func (a *app) startPreview() {
	p := &previewServer{app: a, items: map[string]previewItem{}}
	a.preview = p
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Printf("preview server: %v", err)
		return
	}
	p.origin = "http://" + ln.Addr().String()
	go http.Serve(ln, p)
}

func (p *previewServer) add(it previewItem) string {
	if p == nil || p.origin == "" {
		return ""
	}
	token := newUUID()
	p.mu.Lock()
	p.items[token] = it
	p.mu.Unlock()
	return p.origin + "/m/" + token
}

func (p *previewServer) local(path string) string { return p.add(previewItem{path: path}) }
func (p *previewServer) remote(url string) string { return p.add(previewItem{url: url}) }

func (p *previewServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Private-Network", "true")
	h.Set("Cache-Control", "no-store")
	if r.Method == http.MethodOptions {
		h.Set("Access-Control-Allow-Methods", "GET")
		h.Set("Access-Control-Allow-Headers", "*")
		return
	}
	p.mu.Lock()
	it, ok := p.items[strings.TrimPrefix(r.URL.Path, "/m/")]
	p.mu.Unlock()
	if !ok || r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	path := it.path
	if path == "" {
		// ponytail: the cache is never pruned; add an age limit if it grows large.
		path = filepath.Join(p.app.dir, "preview-cache", sha256Hex([]byte(strings.SplitN(it.url, "?", 2)[0])))
		if _, err := os.Stat(path); err != nil {
			if err := p.fetch(r.Context(), it.url, path); err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
		}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if sniff(b) == "glb" {
		// The widget may not run WebAssembly, which three.js needs for meshopt.
		if b, err = decodeMeshoptGLB(b); err != nil {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		h.Set("Content-Type", "model/gltf-binary")
	} else {
		h.Set("Content-Type", "application/octet-stream")
		h.Set("X-Model-Format", sniff(b))
	}
	h.Set("Access-Control-Expose-Headers", "X-Model-Format")
	w.Write(b)
}

func (p *previewServer) fetch(ctx context.Context, url, path string) error {
	body, err := p.app.open(ctx, url)
	if err != nil {
		return err
	}
	defer body.Close()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".part-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = io.Copy(tmp, io.LimitReader(body, 512<<20))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
