package main

// Local tools: they run on files on this machine, cost no credits and call no
// cloud. Blender does the 3D work; crop and paint are plain Go.
//
// Blender 5.x reads EXT_meshopt_compression (what Tripo's GLBs use) itself, so
// the plugin's meshopt decoder (glb-decode.mjs) is not ported.

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Derived from the MIT-licensed tripo-studio-plugin; see the header of the file.
//
//go:embed blender/worker.py
var blenderWorker []byte

const (
	maxLocalModel  = 150 << 20 // same limit as the plugin
	blenderTimeout = 15 * time.Minute
)

func init() {
	register(
		tool{
			Name:        "tripo_inspect_local_parts",
			Description: "Free, local (Blender): list the mesh parts of a local GLB with polygon/triangle counts, UV presence, estimated UV fill, skinning and hidden state. Part names and face indices are the ones tripo_edit_parts uses. Needs Blender (env TRIPO_BLENDER_EXECUTABLE or blender on PATH).",
			InputSchema: object([]string{"model_path"}, props{"model_path": str("Self-contained GLB file, up to 150 MiB.")}),
			run:         handler(inspectLocal),
		},
		tool{
			Name:        "tripo_render_model",
			Description: "Free, local (Blender, Cycles on CPU): render a local GLB from a preset or exact camera. Saves a PNG at exactly 2x the viewport size (soft lights, flat grey background) beside the model or in output_dir, and prints the camera matrix and FOV that tripo_bake_texture_projection needs. The model is not changed.",
			InputSchema: object([]string{"model_path"}, camProps, props{
				"model_path": str("Self-contained GLB file."),
				"output_dir": str("Folder for the render. Default: beside the model."),
			}),
			UI:  true,
			run: handler(renderLocal),
		},
		tool{
			Name:        "tripo_edit_parts",
			Description: "Free, local (Blender): merge, hide, delete or split parts of a local GLB. Writes <name>.edited.glb beside the model (or in output_dir); the original is never touched. Structural edits of skinned parts are refused. Use tripo_inspect_local_parts first for part names and face indices.",
			InputSchema: object([]string{"model_path", "edits"}, props{
				"model_path": str("Self-contained GLB file."),
				"output_dir": str("Folder for the result. Default: beside the model."),
				"edits": props{"type": "array", "minItems": 1, "maxItems": 100, "description": "Edits, applied in order.", "items": object([]string{"action", "part_names"}, props{
					"action":       enum("merge needs 2+ parts and name; split needs one part, name and face_indices; hide and delete need only part_names.", "merge", "hide", "delete", "split"),
					"part_names":   props{"type": "array", "items": props{"type": "string"}, "minItems": 1, "maxItems": 200, "description": "Mesh part names, as printed by tripo_inspect_local_parts."},
					"name":         str("Name of the merged or split-off part."),
					"face_indices": props{"type": "array", "items": props{"type": "integer", "minimum": 0}, "minItems": 1, "maxItems": 100000, "description": "Polygon indices to split off."},
				})},
			}),
			run: handler(editParts),
		},
		tool{
			Name:        "tripo_bake_texture_projection",
			Description: "Free, local (Blender): project an edited viewport image onto a local GLB through the exact camera of a tripo_render_model render and bake a full UV texture per part (PNG files beside the model or in output_dir). The image must be exactly 2x viewport_width x viewport_height. Visibility is a face-centre ray cast, so very large triangles can smear at occlusion edges. The model is not changed.",
			InputSchema: object([]string{"model_path", "image_path", "camera_matrix"}, camProps, props{
				"model_path": str("Self-contained GLB file; every projected part needs UVs."),
				"image_path": str("The edited render (PNG/JPEG/WebP), exactly 2x the viewport size."),
				"part_names": props{"type": "array", "items": props{"type": "string"}, "minItems": 1, "maxItems": 200, "description": "Bake only these parts. Default all."},
				"resolution": props{"type": "integer", "enum": []int{512, 1024, 2048, 4096}, "description": "Baked texture size. Default 2048."},
				"strength":   number("0-1, how much of the projected image replaces the old texture. Default 1."),
				"output_dir": str("Folder for the textures. Default: beside the model."),
			}),
			UI:  true,
			run: handler(bakeProjection),
		},
		tool{
			Name:        "tripo_paint_texture",
			Description: "Free, local (pure Go): paint round brush strokes onto a copy of a texture image. u and v are 0-1 with v=0 at the bottom, radius is in pixels, color is RGB 0-255. Writes <name>.painted.png beside the image (or in output_dir); the original is not touched.",
			InputSchema: object([]string{"image_path", "strokes"}, props{
				"image_path": str("Texture image (PNG/JPEG/WebP)."),
				"output_dir": str("Folder for the result. Default: beside the image."),
				"strokes": props{"type": "array", "minItems": 1, "maxItems": 1000, "description": "Strokes, painted in order.", "items": object([]string{"points", "radius", "color"}, props{
					"points":  props{"type": "array", "minItems": 1, "maxItems": 10000, "description": "Polyline of [u, v] points.", "items": props{"type": "array", "items": props{"type": "number", "minimum": 0, "maximum": 1}, "minItems": 2, "maxItems": 2}},
					"radius":  props{"type": "number", "minimum": 1, "maximum": 2048, "description": "Brush radius in pixels."},
					"color":   props{"type": "array", "items": props{"type": "integer", "minimum": 0, "maximum": 255}, "minItems": 3, "maxItems": 3, "description": "[r, g, b]."},
					"opacity": props{"type": "number", "minimum": 0, "maximum": 1, "description": "Default 1."},
				})},
			}),
			UI:  true,
			run: handler(paintTexture),
		},
		tool{
			Name:        "tripo_crop_image",
			Description: "Free, local (pure Go): crop an image to a pixel rectangle. Writes <name>.cropped.png beside the image (or in output_dir); the original is not touched. This is a plain rectangle, not Studio's subject cutout.",
			InputSchema: object([]string{"image_path", "left", "top", "width", "height"}, props{
				"image_path": str("Image (PNG/JPEG/WebP)."),
				"left":       integer("Left edge in pixels (0 or more)."),
				"top":        integer("Top edge in pixels (0 or more)."),
				"width":      integer("Width in pixels, 1-16384."),
				"height":     integer("Height in pixels, 1-16384."),
				"output_dir": str("Folder for the result. Default: beside the image."),
			}),
			UI:  true,
			run: handler(cropImage),
		},
	)
}

// ---- camera arguments shared by render and bake ----

var camProps = props{
	"viewport_width":  integer("Viewport width in pixels, 64-2048. The image is 2x this. Default 512."),
	"viewport_height": integer("Viewport height in pixels, 64-2048. Default 512."),
	"fov_degrees":     number("Vertical field of view, 10-120. Default 50."),
	"view":            enum("Camera preset used when no camera_matrix is given. Default three_quarter.", "front", "left", "back", "right", "three_quarter"),
	"camera_matrix":   props{"type": "array", "items": props{"type": "number"}, "minItems": 16, "maxItems": 16, "description": "Three.js Y-up camera world matrix, 16 numbers column-major, as printed by tripo_render_model."},
}

type camArgs struct {
	ViewportWidth  int       `json:"viewport_width"`
	ViewportHeight int       `json:"viewport_height"`
	FovDegrees     float64   `json:"fov_degrees"`
	View           string    `json:"view"`
	CameraMatrix   []float64 `json:"camera_matrix"`
}

// check fills the defaults and validates the ranges.
func (c *camArgs) check(needMatrix bool) error {
	if c.ViewportWidth == 0 {
		c.ViewportWidth = 512
	}
	if c.ViewportHeight == 0 {
		c.ViewportHeight = 512
	}
	if c.FovDegrees == 0 {
		c.FovDegrees = 50
	}
	if c.View == "" {
		c.View = "three_quarter"
	}
	switch {
	case c.ViewportWidth < 64 || c.ViewportWidth > 2048 || c.ViewportHeight < 64 || c.ViewportHeight > 2048:
		return errors.New("viewport_width and viewport_height must be 64-2048")
	case c.FovDegrees < 10 || c.FovDegrees > 120:
		return errors.New("fov_degrees must be 10-120")
	case !strings.Contains(" front left back right three_quarter ", " "+c.View+" "):
		return fmt.Errorf("unknown view %q", c.View)
	case needMatrix && len(c.CameraMatrix) != 16:
		return errors.New("camera_matrix (16 numbers) is required; render the model with tripo_render_model first and pass its camera_matrix")
	case c.CameraMatrix != nil && len(c.CameraMatrix) != 16:
		return errors.New("camera_matrix must have exactly 16 numbers")
	}
	for _, v := range c.CameraMatrix {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return errors.New("camera_matrix must be finite numbers")
		}
	}
	return nil
}

func (c *camArgs) job(job map[string]any) map[string]any {
	job["viewport_width"], job["viewport_height"] = c.ViewportWidth, c.ViewportHeight
	job["fov_degrees"], job["view"] = c.FovDegrees, c.View
	if c.CameraMatrix != nil {
		job["camera_matrix"] = c.CameraMatrix
	}
	return job
}

func matrixText(m any) string {
	b, _ := json.Marshal(m)
	return string(b)
}

// ---- files ----

// checkModel validates a local GLB: a .glb file, up to 150 MiB, with the
// resources inside the file. It returns the absolute path and whether the
// file needs EXT_meshopt_compression support from Blender.
func checkModel(path string) (abs string, meshopt bool, err error) {
	if path == "" {
		return "", false, errors.New("model_path is required")
	}
	if abs, err = filepath.Abs(path); err != nil {
		return
	}
	if !strings.EqualFold(filepath.Ext(abs), ".glb") {
		return "", false, errors.New("local Blender tools need a self-contained .glb file")
	}
	st, err := os.Stat(abs)
	if err != nil {
		return "", false, fmt.Errorf("cannot read model: %w", err)
	}
	if st.IsDir() || st.Size() > maxLocalModel {
		return "", false, fmt.Errorf("%s is not a GLB file up to 150 MiB", abs)
	}
	doc, err := glbJSON(abs)
	if err != nil {
		return "", false, err
	}
	for _, key := range []string{"buffers", "images"} {
		for _, r := range doc[key].([]any) {
			if m, _ := r.(map[string]any); m != nil {
				if uri, _ := m["uri"].(string); uri != "" && !strings.HasPrefix(uri, "data:") {
					return "", false, errors.New("the GLB references external files; embed them first")
				}
			}
		}
	}
	for _, e := range doc["extensionsUsed"].([]any) {
		meshopt = meshopt || e == "EXT_meshopt_compression"
	}
	return abs, meshopt, nil
}

// glbJSON reads the JSON chunk of a GLB; buffers, images and extensionsUsed
// are always present (maybe empty) in the result.
func glbJSON(path string) (map[string]any, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var h [20]byte
	if _, err := f.Read(h[:]); err != nil || string(h[:4]) != "glTF" || binary.LittleEndian.Uint32(h[4:]) != 2 ||
		binary.LittleEndian.Uint32(h[16:]) != 0x4e4f534a {
		return nil, fmt.Errorf("%s is not a GLB v2 file", path)
	}
	n := binary.LittleEndian.Uint32(h[12:])
	if n > 64<<20 {
		return nil, errors.New("GLB JSON chunk is too large")
	}
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, 20); err != nil {
		return nil, fmt.Errorf("%s: truncated GLB", path)
	}
	doc := map[string]any{}
	if err := json.Unmarshal(bytes.TrimRight(buf, " \x00"), &doc); err != nil {
		return nil, fmt.Errorf("%s: invalid GLB JSON", path)
	}
	for _, k := range []string{"buffers", "images", "extensionsUsed"} {
		if _, ok := doc[k].([]any); !ok {
			doc[k] = []any{}
		}
	}
	return doc, nil
}

// outDir is the folder for results: output_dir if given, else beside the input.
func outDir(input, dir string) (string, error) {
	if dir == "" {
		return filepath.Dir(input), nil
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	return abs, os.MkdirAll(abs, 0o755)
}

func stem(path string) string {
	b := filepath.Base(path)
	return strings.TrimSuffix(b, filepath.Ext(b))
}

// reserve creates an empty file dir/name+ext (name-2, name-3... if taken) so
// no output ever replaces an existing file, the source model included.
func reserve(dir, name, ext string) (string, error) {
	name = safeName(name)
	for i := 1; i < 1000; i++ {
		p := filepath.Join(dir, name+ext)
		if i > 1 {
			p = filepath.Join(dir, fmt.Sprintf("%s-%d%s", name, i, ext))
		}
		f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			return p, f.Close()
		}
		if !os.IsExist(err) {
			return "", err
		}
	}
	return "", fmt.Errorf("no free name for %s in %s", name, dir)
}

// saveAs reserves a name and fills it with data.
func saveAs(dir, name, ext string, data []byte) (string, error) {
	p, err := reserve(dir, name, ext)
	if err != nil {
		return "", err
	}
	return p, os.WriteFile(p, data, 0o644)
}

// moveAs reserves a name and moves a file into it.
func moveAs(from, dir, name, ext string) (string, error) {
	p, err := reserve(dir, name, ext)
	if err != nil {
		return "", err
	}
	return p, os.Rename(from, p)
}

func loadImage(path string) (image.Image, string, error) {
	if path == "" {
		return nil, "", errors.New("image_path is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, "", err
	}
	f, err := os.Open(abs)
	if err != nil {
		return nil, "", fmt.Errorf("cannot read image: %w", err)
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return nil, "", fmt.Errorf("%s is not a PNG, JPEG or WebP image", abs)
	}
	return img, abs, nil
}

func encodePNG(img image.Image) ([]byte, error) {
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// imagesReply is the card for saved images: thumbnails go to _meta.images in
// the order of card.Images, the full paths to the text and card.Saved.
func imagesReply(title, summary string, files, labels []string) reply {
	r := reply{Card: &card{Kind: "images", Title: title, Status: "saved", Summary: summary, Saved: files}}
	thumbs := make([]string, len(files))
	for i, f := range files {
		r.Card.Images = append(r.Card.Images, cardImage{Label: labels[i]})
		if b, err := os.ReadFile(f); err == nil {
			thumbs[i] = thumb(&localImage{path: f, data: b})
		}
		r.line("Saved %s: %s", labels[i], f)
	}
	r.meta("images", thumbs)
	return r
}

// ---- Blender ----

func blenderExe() (string, error) {
	if p := strings.TrimSpace(os.Getenv("TRIPO_BLENDER_EXECUTABLE")); p != "" {
		if st, err := os.Stat(p); err != nil || st.IsDir() {
			return "", fmt.Errorf("TRIPO_BLENDER_EXECUTABLE=%q is not a file", p)
		}
		return p, nil
	}
	if p, err := exec.LookPath("blender"); err == nil {
		return p, nil
	}
	if p := findBlender(); p != "" {
		return p, nil
	}
	return "", errors.New("Blender not found: set TRIPO_BLENDER_EXECUTABLE to blender.exe, or put blender on PATH")
}

// findBlender looks where Blender usually lands on Windows: the official
// installer's folder, and a portable copy at <drive>:\Blender. The newest
// version wins.
func findBlender() string {
	var found []string
	for d := 'C'; d <= 'Z'; d++ {
		root := string(d) + `:\`
		if _, err := os.Stat(root); err != nil {
			continue
		}
		for _, pattern := range []string{
			root + `Blender\blender.exe`,
			root + `Program Files\Blender Foundation\Blender*\blender.exe`,
		} {
			m, _ := filepath.Glob(pattern)
			found = append(found, m...)
		}
	}
	if len(found) == 0 {
		return ""
	}
	sort.Strings(found) // "Blender 4.5" < "Blender 5.0"
	return found[len(found)-1]
}

// runBlender runs the embedded worker on job in the background (no UI) and
// returns the parsed result.json. work is a fresh folder: job.json,
// worker.py, blender.log and the raw outputs live there. A failed run keeps
// it for the log; the caller removes it after a good one.
func runBlender(ctx context.Context, work string, meshopt bool, job map[string]any) (map[string]any, error) {
	exe, err := blenderExe()
	if err != nil {
		return nil, err
	}
	job["output_dir"] = work
	jobJSON, _ := json.Marshal(job)
	jobPath, script, logPath := filepath.Join(work, "job.json"), filepath.Join(work, "worker.py"), filepath.Join(work, "blender.log")
	if err := os.WriteFile(jobPath, jobJSON, 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(script, blenderWorker, 0o600); err != nil {
		return nil, err
	}
	logf, err := os.Create(logPath)
	if err != nil {
		return nil, err
	}
	defer logf.Close()
	tctx, cancel := context.WithTimeout(ctx, blenderTimeout)
	defer cancel()
	cmd := exec.CommandContext(tctx, exe, "--background", "--factory-startup", "--python-exit-code", "1", "--python", script, "--", jobPath)
	cmd.Stdout, cmd.Stderr = logf, logf
	runErr := cmd.Run()
	var out map[string]any
	if runErr == nil {
		var b []byte
		if b, runErr = os.ReadFile(filepath.Join(work, "result.json")); runErr == nil {
			runErr = json.Unmarshal(b, &out)
		}
	}
	if runErr == nil {
		return out, nil
	}
	switch {
	case ctx.Err() != nil:
		return nil, fmt.Errorf("cancelled; Blender log: %s", logPath)
	case tctx.Err() != nil:
		return nil, fmt.Errorf("Blender ran over %v and was stopped; log: %s", blenderTimeout, logPath)
	}
	msg := fmt.Sprintf("Blender failed (%v)\n%s\nLog: %s", runErr, logTail(logPath, 8), logPath)
	if lg, _ := os.ReadFile(logPath); meshopt && (bytes.Contains(bytes.ToLower(lg), []byte("meshopt")) || bytes.Contains(lg, []byte("no mesh objects"))) {
		msg += "\nThis GLB uses EXT_meshopt_compression, which older Blender versions cannot import; Blender 5.x reads it."
	}
	return nil, errors.New(msg)
}

// logTail is the last n lines of the log that say something.
func logTail(path string, n int) string {
	b, _ := os.ReadFile(path)
	var lines []string
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "TBBmalloc") {
			lines = append(lines, l)
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// blenderRun is the common path of the Blender tools: validate the model,
// make a work folder beside the results, run the worker, hand the result and
// the work folder to finish, then remove the work folder.
func blenderRun(ctx context.Context, model, outputDir string, label string, job map[string]any, finish func(res map[string]any, work, dir, name string) (reply, error)) (reply, error) {
	abs, meshopt, err := checkModel(model)
	if err != nil {
		return reply{}, err
	}
	dir, err := outDir(abs, outputDir)
	if err != nil {
		return reply{}, err
	}
	name := stem(abs)
	work, err := os.MkdirTemp(dir, "."+safeName(name)+"-"+label+"-")
	if err != nil {
		return reply{}, err
	}
	job["model_path"] = abs
	res, err := runBlender(ctx, work, meshopt, job)
	if err != nil {
		return reply{}, err
	}
	r, err := finish(res, work, dir, name)
	if err == nil {
		// Windows can hold the folder for a moment after Blender exits.
		for i := 0; i < 10 && os.RemoveAll(work) != nil; i++ {
			time.Sleep(200 * time.Millisecond)
		}
	}
	return r, err
}

// ---- tripo_inspect_local_parts ----

type modelArgs struct {
	ModelPath string `json:"model_path"`
}

type partInfo struct {
	Name          string `json:"name"`
	PolygonCount  int    `json:"polygon_count"`
	TriangleCount int    `json:"triangle_count"`
	HasUV         bool   `json:"has_uv"`
	Skinned       bool   `json:"skinned"`
	Hidden        bool   `json:"hidden"`
	UVUtilization *struct {
		Fraction float64 `json:"fraction"`
		Outside  bool    `json:"uv_outside_tile"`
	} `json:"uv_utilization"`
}

func partsText(res map[string]any) string {
	var parts []partInfo
	b, _ := json.Marshal(res["parts"])
	_ = json.Unmarshal(b, &parts)
	lines := []string{fmt.Sprintf("%d mesh part(s):", len(parts))}
	for i, p := range parts {
		uv := "no UV"
		if p.HasUV {
			uv = "UV"
			if p.UVUtilization != nil {
				uv = fmt.Sprintf("UV fill ~%.0f%%", p.UVUtilization.Fraction*100)
				if p.UVUtilization.Outside {
					uv += ", islands outside the 0-1 tile"
				}
			}
		}
		flags := ""
		if p.Skinned {
			flags += ", skinned"
		}
		if p.Hidden {
			flags += ", hidden"
		}
		lines = append(lines, fmt.Sprintf("%d. %q: %d polygons (%d triangles), %s%s", i+1, p.Name, p.PolygonCount, p.TriangleCount, uv, flags))
	}
	return strings.Join(lines, "\n")
}

func inspectLocal(ctx context.Context, a *app, x modelArgs) (reply, error) {
	return blenderRun(ctx, x.ModelPath, "", "inspect", map[string]any{"kind": "inspect"}, func(res map[string]any, work, dir, name string) (reply, error) {
		return say("Free, local. UV fill is an estimate (256x256 raster of the 0-1 tile). Face indices are Blender's polygon indices.\n" + partsText(res)), nil
	})
}

// ---- tripo_render_model ----

type renderArgs struct {
	camArgs
	ModelPath string `json:"model_path"`
	OutputDir string `json:"output_dir"`
}

func renderLocal(ctx context.Context, a *app, x renderArgs) (reply, error) {
	if err := x.check(false); err != nil {
		return reply{}, err
	}
	job := x.job(map[string]any{"kind": "render"})
	return blenderRun(ctx, x.ModelPath, x.OutputDir, "render", job, func(res map[string]any, work, dir, name string) (reply, error) {
		src, _ := res["render_path"].(string)
		raw, err := os.Open(src)
		if err != nil {
			return reply{}, fmt.Errorf("Blender produced no render: %w", err)
		}
		defer raw.Close()
		img, err := png.Decode(raw)
		if err != nil {
			return reply{}, err
		}
		// The render has a transparent background; flatten it onto light grey like the plugin does.
		flat := image.NewNRGBA(img.Bounds())
		draw.Draw(flat, flat.Bounds(), image.NewUniform(color.NRGBA{238, 238, 238, 255}), image.Point{}, draw.Src)
		draw.Draw(flat, flat.Bounds(), img, img.Bounds().Min, draw.Over)
		data, err := encodePNG(flat)
		if err != nil {
			return reply{}, err
		}
		label := x.View
		if x.CameraMatrix != nil {
			label = "camera"
		}
		path, err := saveAs(dir, name+".render-"+label, ".png", data)
		if err != nil {
			return reply{}, err
		}
		w, h := x.ViewportWidth*2, x.ViewportHeight*2
		r := imagesReply("Render "+name, fmt.Sprintf("%dx%d, FOV %g", w, h, x.FovDegrees), []string{path}, []string{fmt.Sprintf("%s %dx%d", label, w, h)})
		r.line("Free, local. The image is %dx%d (2x viewport %dx%d), FOV %g.", w, h, x.ViewportWidth, x.ViewportHeight, x.FovDegrees)
		r.line("camera_matrix: %s", matrixText(res["camera_matrix"]))
		r.line("Pass camera_matrix, fov_degrees, viewport_width and viewport_height unchanged to tripo_bake_texture_projection together with an edited copy of this image.")
		r.line("%s", partsText(res))
		return r, nil
	})
}

// ---- tripo_edit_parts ----

type editArgs struct {
	ModelPath string     `json:"model_path"`
	OutputDir string     `json:"output_dir"`
	Edits     []partEdit `json:"edits"`
}

type partEdit struct {
	Action      string   `json:"action"`
	PartNames   []string `json:"part_names"`
	Name        string   `json:"name,omitempty"`
	FaceIndices []int    `json:"face_indices,omitempty"`
}

var controlChars = regexp.MustCompile(`[\x00-\x1f\x7f]`)

func validName(s string) bool { return s != "" && len(s) <= 256 && !controlChars.MatchString(s) }

func (x *editArgs) check() error {
	if len(x.Edits) == 0 || len(x.Edits) > 100 {
		return errors.New("edits must have 1-100 items")
	}
	for i, e := range x.Edits {
		bad := func(msg string) error { return fmt.Errorf("edits[%d] (%s): %s", i, e.Action, msg) }
		if len(e.PartNames) == 0 || len(e.PartNames) > 200 {
			return bad("part_names must have 1-200 names")
		}
		seen := map[string]bool{}
		for _, n := range e.PartNames {
			if !validName(n) {
				return bad("invalid part name")
			}
			if seen[n] {
				return bad("duplicate part names")
			}
			seen[n] = true
		}
		switch e.Action {
		case "hide", "delete":
		case "merge", "split":
			if !validName(e.Name) {
				return bad("name is required")
			}
			if e.Action == "merge" && len(e.PartNames) < 2 {
				return bad("merge needs at least two parts")
			}
			if e.Action == "split" && (len(e.PartNames) != 1 || len(e.FaceIndices) == 0) {
				return bad("split needs one part and face_indices")
			}
		default:
			return bad("action must be merge, hide, delete or split")
		}
		if len(e.FaceIndices) > 100000 {
			return bad("too many face_indices")
		}
		for _, f := range e.FaceIndices {
			if f < 0 {
				return bad("face_indices must be 0 or more")
			}
		}
	}
	return nil
}

func editParts(ctx context.Context, a *app, x editArgs) (reply, error) {
	if err := x.check(); err != nil {
		return reply{}, err
	}
	job := map[string]any{"kind": "edit_parts", "edits": x.Edits}
	return blenderRun(ctx, x.ModelPath, x.OutputDir, "edit", job, func(res map[string]any, work, dir, name string) (reply, error) {
		src, _ := res["model_path"].(string)
		path, err := moveAs(src, dir, name+".edited", ".glb")
		if err != nil {
			return reply{}, err
		}
		st, _ := os.Stat(path)
		var size int64
		if st != nil {
			size = st.Size()
		}
		r := say(fmt.Sprintf("Free, local. Saved the edited copy: %s (%s). The original is unchanged.\n%s", path, sizeText(size), partsText(res)))
		r.Card = nil
		return r, nil
	})
}

func sizeText(n int64) string { return fmt.Sprintf("%.1f MB", float64(n)/1e6) }

// ---- tripo_bake_texture_projection ----

type bakeArgs struct {
	camArgs
	ModelPath  string   `json:"model_path"`
	ImagePath  string   `json:"image_path"`
	PartNames  []string `json:"part_names"`
	Resolution int      `json:"resolution"`
	Strength   *float64 `json:"strength"`
	OutputDir  string   `json:"output_dir"`
}

func (x *bakeArgs) check() (string, error) {
	if err := x.camArgs.check(true); err != nil {
		return "", err
	}
	switch x.Resolution {
	case 0:
		x.Resolution = 2048
	case 512, 1024, 2048, 4096:
	default:
		return "", errors.New("resolution must be 512, 1024, 2048 or 4096")
	}
	if x.Strength == nil {
		one := 1.0
		x.Strength = &one
	}
	if *x.Strength < 0 || *x.Strength > 1 {
		return "", errors.New("strength must be 0-1")
	}
	if len(x.PartNames) > 200 {
		return "", errors.New("part_names has more than 200 names")
	}
	for _, n := range x.PartNames {
		if !validName(n) {
			return "", errors.New("invalid part name")
		}
	}
	abs, err := filepath.Abs(x.ImagePath)
	if x.ImagePath == "" || err != nil {
		return "", errors.New("image_path is required")
	}
	f, err := os.Open(abs)
	if err != nil {
		return "", fmt.Errorf("cannot read image: %w", err)
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return "", fmt.Errorf("%s is not a PNG, JPEG or WebP image", abs)
	}
	if cfg.Width != x.ViewportWidth*2 || cfg.Height != x.ViewportHeight*2 {
		return "", fmt.Errorf("the image is %dx%d but must be exactly 2x the viewport: %dx%d (viewport_width x viewport_height = %dx%d)",
			cfg.Width, cfg.Height, x.ViewportWidth*2, x.ViewportHeight*2, x.ViewportWidth, x.ViewportHeight)
	}
	return abs, nil
}

func bakeProjection(ctx context.Context, a *app, x bakeArgs) (reply, error) {
	img, err := x.check()
	if err != nil {
		return reply{}, err
	}
	job := x.job(map[string]any{"kind": "project_texture", "image_path": img, "resolution": x.Resolution, "strength": *x.Strength})
	if len(x.PartNames) > 0 {
		job["part_names"] = x.PartNames
	}
	return blenderRun(ctx, x.ModelPath, x.OutputDir, "bake", job, func(res map[string]any, work, dir, name string) (reply, error) {
		var tex []struct {
			Part string `json:"part_name"`
			Path string `json:"image_path"`
		}
		b, _ := json.Marshal(res["textures"])
		_ = json.Unmarshal(b, &tex)
		var files, labels []string
		for _, t := range tex {
			p, err := moveAs(t.Path, dir, name+".bake-"+t.Part, ".png")
			if err != nil {
				return reply{}, err
			}
			files, labels = append(files, p), append(labels, t.Part)
		}
		if len(files) == 0 {
			return reply{}, errors.New("Blender baked no textures")
		}
		r := imagesReply("Projected texture "+name, fmt.Sprintf("%d part(s), %dx%d", len(files), x.Resolution, x.Resolution), files, labels)
		r.line("Free, local. Full UV textures at %dx%d, one per part; the model itself is unchanged. Visibility is a face-centre ray cast: very large triangles crossing an occlusion edge need subdivision.", x.Resolution, x.Resolution)
		return r, nil
	})
}

// ---- tripo_paint_texture ----

type paintArgs struct {
	ImagePath string   `json:"image_path"`
	OutputDir string   `json:"output_dir"`
	Strokes   []stroke `json:"strokes"`
}

type stroke struct {
	Points  [][]float64 `json:"points"`
	Radius  float64     `json:"radius"`
	Color   []int       `json:"color"`
	Opacity *float64    `json:"opacity"`
}

func (x *paintArgs) check() error {
	if len(x.Strokes) == 0 || len(x.Strokes) > 1000 {
		return errors.New("strokes must have 1-1000 items")
	}
	for i, s := range x.Strokes {
		bad := func(msg string) error { return fmt.Errorf("strokes[%d]: %s", i, msg) }
		switch {
		case len(s.Points) == 0 || len(s.Points) > 10000:
			return bad("points must have 1-10000 items")
		case s.Radius < 1 || s.Radius > 2048:
			return bad("radius must be 1-2048")
		case len(s.Color) != 3:
			return bad("color must be [r, g, b]")
		case s.Opacity != nil && (*s.Opacity < 0 || *s.Opacity > 1):
			return bad("opacity must be 0-1")
		}
		for _, c := range s.Color {
			if c < 0 || c > 255 {
				return bad("color values must be 0-255")
			}
		}
		for _, p := range s.Points {
			if len(p) != 2 || p[0] < 0 || p[0] > 1 || p[1] < 0 || p[1] > 1 {
				return bad("every point must be [u, v] within 0-1")
			}
		}
	}
	return nil
}

func toNRGBA(img image.Image) *image.NRGBA {
	if n, ok := img.(*image.NRGBA); ok {
		return n
	}
	n := image.NewNRGBA(img.Bounds())
	draw.Draw(n, n.Bounds(), img, img.Bounds().Min, draw.Src)
	return n
}

// paint draws the strokes on img (a fresh copy) with the plugin's brush:
// round, anti-aliased at the edge, stamped along each segment.
func paint(img *image.NRGBA, strokes []stroke) *image.NRGBA {
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	out := image.NewNRGBA(image.Rect(0, 0, w, h))
	copy(out.Pix, toNRGBA(img).Pix) // ponytail: assumes a zero-origin image, which decoders return
	mask := make([]float32, w*h)
	for _, s := range strokes {
		opacity := 1.0
		if s.Opacity != nil {
			opacity = *s.Opacity
		}
		r := s.Radius
		x0, y0, x1, y1 := w, h, -1, -1 // bounding box touched by this stroke
		stamp := func(u, v float64) {
			cx, cy := u*float64(w-1), (1-v)*float64(h-1)
			ylo, yhi := max(0, int(math.Floor(cy-r))), min(h-1, int(math.Ceil(cy+r)))
			xlo, xhi := max(0, int(math.Floor(cx-r))), min(w-1, int(math.Ceil(cx+r)))
			x0, y0, x1, y1 = min(x0, xlo), min(y0, ylo), max(x1, xhi), max(y1, yhi)
			for py := ylo; py <= yhi; py++ {
				for px := xlo; px <= xhi; px++ {
					cov := float32(math.Max(0, math.Min(1, r+0.5-math.Hypot(float64(px)-cx, float64(py)-cy))) * opacity)
					if i := py*w + px; cov > mask[i] {
						mask[i] = cov
					}
				}
			}
		}
		for i, p := range s.Points {
			u, v := p[0], p[1]
			pu, pv := u, v
			if i > 0 {
				pu, pv = s.Points[i-1][0], s.Points[i-1][1]
			}
			steps := max(1, int(math.Ceil(math.Hypot((u-pu)*float64(w), (v-pv)*float64(h))/math.Max(1, r/2))))
			for k := 0; k <= steps; k++ {
				t := float64(k) / float64(steps)
				stamp(pu+(u-pu)*t, pv+(v-pv)*t)
			}
		}
		for py := y0; py <= y1; py++ {
			for px := x0; px <= x1; px++ {
				i := py*w + px
				if m := mask[i]; m != 0 {
					for c := 0; c < 3; c++ {
						o := &out.Pix[i*4+c]
						*o = uint8(math.Round(float64(*o)*(1-float64(m)) + float64(s.Color[c])*float64(m)))
					}
					mask[i] = 0
				}
			}
		}
	}
	return out
}

func paintTexture(ctx context.Context, a *app, x paintArgs) (reply, error) {
	if err := x.check(); err != nil {
		return reply{}, err
	}
	src, abs, err := loadImage(x.ImagePath)
	if err != nil {
		return reply{}, err
	}
	dir, err := outDir(abs, x.OutputDir)
	if err != nil {
		return reply{}, err
	}
	out := paint(toNRGBA(src), x.Strokes)
	data, err := encodePNG(out)
	if err != nil {
		return reply{}, err
	}
	path, err := saveAs(dir, stem(abs)+".painted", ".png", data)
	if err != nil {
		return reply{}, err
	}
	b := out.Bounds()
	r := imagesReply("Painted "+stem(abs), fmt.Sprintf("%d stroke(s), %dx%d", len(x.Strokes), b.Dx(), b.Dy()), []string{path}, []string{fmt.Sprintf("painted %dx%d", b.Dx(), b.Dy())})
	r.line("Free, local. %d stroke(s) painted; the original is unchanged.", len(x.Strokes))
	return r, nil
}

// ---- tripo_crop_image ----

type cropArgs struct {
	ImagePath string `json:"image_path"`
	Left      int    `json:"left"`
	Top       int    `json:"top"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	OutputDir string `json:"output_dir"`
}

func cropRect(b image.Rectangle, left, top, width, height int) (image.Rectangle, error) {
	if left < 0 || top < 0 || width < 1 || height < 1 || width > 16384 || height > 16384 {
		return image.Rectangle{}, errors.New("left and top must be 0 or more; width and height 1-16384")
	}
	if left+width > b.Dx() || top+height > b.Dy() {
		return image.Rectangle{}, fmt.Errorf("the crop rectangle (left %d, top %d, %dx%d) is outside the %dx%d image", left, top, width, height, b.Dx(), b.Dy())
	}
	return image.Rect(b.Min.X+left, b.Min.Y+top, b.Min.X+left+width, b.Min.Y+top+height), nil
}

func cropImage(ctx context.Context, a *app, x cropArgs) (reply, error) {
	src, abs, err := loadImage(x.ImagePath)
	if err != nil {
		return reply{}, err
	}
	rect, err := cropRect(src.Bounds(), x.Left, x.Top, x.Width, x.Height)
	if err != nil {
		return reply{}, err
	}
	dir, err := outDir(abs, x.OutputDir)
	if err != nil {
		return reply{}, err
	}
	out := image.NewNRGBA(image.Rect(0, 0, x.Width, x.Height))
	draw.Draw(out, out.Bounds(), src, rect.Min, draw.Src)
	data, err := encodePNG(out)
	if err != nil {
		return reply{}, err
	}
	path, err := saveAs(dir, stem(abs)+".cropped", ".png", data)
	if err != nil {
		return reply{}, err
	}
	r := imagesReply("Cropped "+stem(abs), fmt.Sprintf("%dx%d from (%d, %d)", x.Width, x.Height, x.Left, x.Top), []string{path}, []string{fmt.Sprintf("crop %dx%d", x.Width, x.Height)})
	r.line("Free, local. Cropped %dx%d at left %d, top %d; the original is unchanged.", x.Width, x.Height, x.Left, x.Top)
	return r, nil
}
