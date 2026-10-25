package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

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
	// Text repeats the reply text: Claude Code shows the model structuredContent
	// rather than content when both are present, so the prose must travel here too.
	Text       string         `json:"text,omitempty"`
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
	URL        string     `json:"url,omitempty"`    // the project in Studio's workspace
	Status     string     `json:"status,omitempty"` // for the models of a multi-model job
	Saved      string     `json:"saved,omitempty"`
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
func (a *app) showProject(ctx context.Context, r *reply, p *project) {
	if r.Card == nil {
		r.Card = &card{Kind: "project", Title: p.Name}
	}
	r.Card.Project = p.card()
	r.Card.ProjectID = p.ID
	if u := p.cover(); u != "" {
		r.meta("cover", u)
	}
	if _, done := r.Meta["model"]; !done && p.ModelURL != "" {
		r.meta("model", a.previewModel(ctx, "", p.ModelURL, p.Name))
	}
}

// ---- thumbnails ----

// thumb shrinks an input image to a small data URL for the card.
func thumb(img *localImage) string {
	b, mime := shrinkImage(img.data, 320)
	if b == nil {
		if _, _, err := image.DecodeConfig(bytes.NewReader(img.data)); err != nil {
			return ""
		}
		b, mime = img.data, "image/"+img.format // already small
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(b)
}

// shrinkImage scales an encoded image down to at most edge px and re-encodes
// it, JPEG when opaque and PNG otherwise. It returns nil when the image is
// already small enough or cannot be decoded.
func shrinkImage(data []byte, edge int) ([]byte, string) {
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, ""
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= edge && h <= edge {
		return nil, ""
	}
	if w >= h {
		w, h = edge, max(1, h*edge/w)
	} else {
		w, h = max(1, w*edge/h), edge
	}
	dst := image.NewNRGBA(image.Rect(0, 0, w, h))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, b, draw.Src, nil)
	var buf bytes.Buffer
	if dst.Opaque() {
		if jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 85}) != nil {
			return nil, ""
		}
		return buf.Bytes(), "image/jpeg"
	}
	if png.Encode(&buf, dst) != nil {
		return nil, ""
	}
	return buf.Bytes(), "image/png"
}

// ---- 3D preview ----

// previewModel returns the card's 3D preview of a model (a local path or a
// signed URL) as inline base64: a GLB with meshopt decoded and textures at
// most 2048 px (smaller ones untouched), or the FBX as is. The bytes travel in the tool result
// because hosts may stop a sandboxed widget from fetching anything, even
// 127.0.0.1 (Chrome's local network access rules).
func (a *app) previewModel(ctx context.Context, path, url, name string) map[string]any {
	b, err := a.modelBytes(ctx, path, url)
	texture := 0 // the real texture size; the preview's is capped
	if err == nil {
		switch sniff(b) {
		case "glb":
			if sizes, e := glbTextureSizes(b); e == nil {
				for _, s := range sizes {
					texture = max(texture, s.Width, s.Height)
				}
			}
			b, err = rewriteGLB(b, 2048)
		case "fbx":
		default:
			err = errors.New("not a GLB or FBX model")
		}
	}
	if err == nil && len(b) > 16<<20 {
		err = fmt.Errorf("the model is too large for an inline preview (%s)", size(int64(len(b))))
	}
	if err != nil {
		return map[string]any{"error": err.Error(), "name": name}
	}
	return map[string]any{"data": base64.StdEncoding.EncodeToString(b), "format": sniff(b), "name": name, "texture": texture}
}

// modelBytes reads a local model, or downloads a signed one into a cache
// keyed by its URL without the signature.
func (a *app) modelBytes(ctx context.Context, path, url string) ([]byte, error) {
	if path != "" {
		return os.ReadFile(path)
	}
	// ponytail: the cache is never pruned; add an age limit if it grows large.
	cache := filepath.Join(a.dir, "preview-cache", sha256Hex([]byte(strings.SplitN(url, "?", 2)[0])))
	if b, err := os.ReadFile(cache); err == nil {
		return b, nil
	}
	body, err := a.open(ctx, url)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	b, err := io.ReadAll(io.LimitReader(body, 512<<20))
	if err != nil {
		return nil, err
	}
	if os.MkdirAll(filepath.Dir(cache), 0o700) == nil {
		os.WriteFile(cache, b, 0o600)
	}
	return b, nil
}

// head returns the first bytes of a file, enough for sniff.
func head(path string) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	b := make([]byte, 32)
	n, _ := io.ReadFull(f, b)
	return b[:n]
}
