package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// Texture post-processes (PBR, upscale, Magic Brush) and the free model import.

func init() {
	register(
		tool{
			Name:        "tripo_generate_pbr",
			Description: "PAID, 5 credits: generate separate PBR maps (normal, roughness, metallic) for an already textured project, keeping its geometry and colour textures. Smart Mesh route: tripo_generate_model, then tripo_texture, then this. Submits at once.",
			InputSchema: object([]string{"project_id"}, followProps, props{"project_id": str("Textured project.")}),
			UI:          true,
			run:         handler(generatePBR),
		},
		tool{
			Name:        "tripo_upscale_texture",
			Description: "PAID: upscale a project's texture set. quality detailed = 4K, 10 credits (default); ultra = 8K, 20. A project that is already 8K cannot be reduced to 4K. After the job the delivered texture size is checked against the request. Submits at once.",
			InputSchema: object([]string{"project_id"}, followProps, props{
				"project_id": str("Textured project."),
				"quality":    enum("Default detailed.", "detailed", "ultra"),
			}),
			UI:  true,
			run: handler(upscaleTexture),
		},
		tool{
			Name:        "tripo_preview_texture_edit",
			Description: "PAID, 5 credits (Magic Brush): retexture preview for one view of a textured model (not segmented, not 8K). render_image_path is a static WebP render of the model at 2x the viewport, camera_matrix the camera's world matrix (16 numbers, Three.js activeCamera.matrix.toArray()), prompt what to paint. Waits for the preview (wait_seconds, default 300) and saves the image; the result has the preview file and the camera_matrix. With operator_id (and nothing else) it only fetches the preview of an earlier job: free.",
			InputSchema: object(nil, props{
				"project_id":        str("Textured project."),
				"prompt":            str("What to paint (1 to 1000 characters)."),
				"render_image_path": str("Static WebP render of the model at 2x the viewport."),
				"camera_matrix":     props{"type": "array", "items": props{"type": "number"}, "minItems": 16, "maxItems": 16, "description": "Camera world matrix, 16 numbers (column-major)."},
				"strength":          number("0 to 1, default 0.5."),
				"save_to":           str("File or folder for the preview image; default a folder in the server's data directory."),
				"dry_run":           boolean("Check the inputs and return the cost; nothing is uploaded, submitted or charged."),
				"wait_seconds":      integer("How long to wait for the preview (default 300, max 3600)."),
				"operator_id":       str("Fetch the preview of an earlier preview job instead of submitting one. Free."),
			}),
			UI:  true,
			run: handler(previewTextureEdit),
		},
		tool{
			Name:        "tripo_apply_texture_edits",
			Description: "PAID, billed as Magic Brush (5 credits; the original plugin could not verify this price): apply baked per-part textures to the model (Studio apply_retexture). Each entry is a part name (exact, from tripo_list_parts) and a complete texture image already baked for that part; the project must be textured, not segmented, not 8K. Submits at once.",
			InputSchema: object([]string{"project_id", "textures"}, followProps, props{
				"project_id": str("Textured project."),
				"textures": props{"type": "array", "minItems": 1, "maxItems": maxParts, "description": "Per-part baked texture images.", "items": object([]string{"part_name", "image_path"}, props{
					"part_name":  str("Exact part name."),
					"image_path": str("Texture image (PNG/JPEG/WebP)."),
				})},
			}),
			UI:  true,
			run: handler(applyTextureEdits),
		},
		tool{
			Name:        "tripo_import_model",
			Description: "Free (0 credits), but creates a new Studio project: import a local model (glb, obj, fbx or stl; up to 150 MiB and 3,000,000 faces) so it can be textured, segmented or remeshed. transform_matrix: 16 numbers, a column-major affine matrix (identity by default). use_original_uv keeps the file's UV layout. Submits at once; dry_run only checks the file.",
			InputSchema: object([]string{"file_path"}, props{
				"file_path":        str("Model file (.glb, .obj, .fbx, .stl)."),
				"name":             str("Project name; default the file name."),
				"transform_matrix": props{"type": "array", "items": props{"type": "number"}, "minItems": 16, "maxItems": 16, "description": "Column-major affine matrix, final row 0 0 0 1; identity by default."},
				"use_original_uv":  boolean("Keep the file's UVs. Default false."),
				"dry_run":          boolean("Check the file and return what would be imported; nothing is uploaded."),
			}),
			UI:  true,
			run: handler(importModel),
		},
	)
}

// ---- shared helpers ----

func validatePrompt(s, field string) (string, error) {
	s = strings.TrimSpace(s)
	if n := utf8.RuneCountInString(s); n < 1 || n > 1000 || strings.ContainsFunc(s, func(r rune) bool {
		return r == 127 || (r < 32 && r != '\t' && r != '\n' && r != '\r')
	}) {
		return "", fmt.Errorf("%s must contain 1 through 1000 safe characters", field)
	}
	return s, nil
}

// textureSize is the edge length Studio promises for a texture quality wire value.
func textureSize(wire string) int {
	return map[string]int{"standard": 2048, "detailed": 4096, "extreme": 8192}[wire]
}

var contentTypes = map[string]string{
	"png": "image/png", "jpeg": "image/jpeg", "webp": "image/webp",
	"glb": "model/gltf-binary", "obj": "model/obj", "stl": "model/stl", "fbx": "application/octet-stream",
}

// putFile uploads bytes to Tripo's temporary storage, without Studio's image
// audit (the website skips it for renders and models). format is the token's
// file format: png, jpeg, webp, glb, obj, fbx or stl.
func (a *app) putFile(ctx context.Context, format string, data []byte) (bucket, key string, err error) {
	var tok struct {
		Bucket  string `json:"resource_bucket"`
		Key     string `json:"resource_uri"`
		Session string `json:"session_token"`
		AK      string `json:"sts_ak"`
		SK      string `json:"sts_sk"`
	}
	if err := a.call(ctx, "POST", "/v2/studio/storage/temporary_token", map[string]string{"client": "aws", "format": format}, &tok); err != nil {
		return "", "", err
	}
	if !bucketName.MatchString(tok.Bucket) || tok.Key == "" || strings.HasPrefix(tok.Key, "/") || tok.AK == "" {
		return "", "", errors.New("Tripo returned an unusable upload token (the site API may have changed)")
	}
	if err := a.s3Put(ctx, tok.Bucket, tok.Key, tok.AK, tok.SK, tok.Session, data, contentTypes[format]); err != nil {
		return "", "", fmt.Errorf("upload failed: %w", err)
	}
	return tok.Bucket, tok.Key, nil
}

func (a *app) putImage(ctx context.Context, img *localImage) (map[string]string, error) {
	bucket, key, err := a.putFile(ctx, img.format, img.data)
	if err != nil {
		return nil, fmt.Errorf("not submitted, nothing charged: %s: %w", filepath.Base(img.path), err)
	}
	return map[string]string{"bucket": bucket, "key": key}, nil
}

// ---- delivered texture resolution (src/studio/export-resolution.mjs, light) ----

type texSize struct {
	Name          string
	Width, Height int
}

func sizesText(sizes []texSize) string {
	parts := make([]string, len(sizes))
	for i, s := range sizes {
		parts[i] = fmt.Sprintf("%s %dx%d", s.Name, s.Width, s.Height)
	}
	return strings.Join(parts, ", ")
}

// glbTextureSizes reads the pixel size of every image in a GLB from the image
// headers (embedded buffer views or data URIs); nothing is decoded.
func glbTextureSizes(data []byte) ([]texSize, error) {
	d, bin, err := readGLB(bytes.NewReader(data), true)
	if errors.Is(err, errFBX) {
		return nil, errors.New("the delivered model is an FBX file; its textures are not inspected here")
	}
	if err != nil {
		return nil, err
	}
	var sizes []texSize
	for i, img := range d.Images {
		name := img.Name
		if name == "" {
			name = fmt.Sprintf("image-%d", i)
		}
		var raw []byte
		switch {
		case strings.HasPrefix(img.URI, "data:"):
			_, b64, ok := strings.Cut(img.URI, ";base64,")
			if !ok {
				return nil, fmt.Errorf("image %s has an unsupported data URI", name)
			}
			if raw, err = base64.StdEncoding.DecodeString(b64); err != nil {
				return nil, fmt.Errorf("image %s: %w", name, err)
			}
		case img.URI == "" && img.BufferView != nil && *img.BufferView >= 0 && *img.BufferView < len(d.BufferViews):
			v := d.BufferViews[*img.BufferView]
			if v.Buffer != 0 || v.ByteOffset < 0 || v.ByteLength <= 0 || v.ByteOffset+v.ByteLength > len(bin) {
				return nil, fmt.Errorf("image %s has an invalid buffer view", name)
			}
			raw = bin[v.ByteOffset : v.ByteOffset+v.ByteLength]
		default:
			return nil, fmt.Errorf("image %s is stored outside the file", name)
		}
		cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
		if err != nil {
			return nil, fmt.Errorf("image %s: %w", name, err)
		}
		sizes = append(sizes, texSize{name, cfg.Width, cfg.Height})
	}
	return sizes, nil
}

// textureVerdict is the line added to a texture job's result. partial means
// only some parts were textured, so the image sizes cannot be tied to the request.
func textureVerdict(sizes []texSize, err error, requested int, partial bool) string {
	const unverified = "Texture resolution is unverified"
	switch {
	case err != nil:
		return fmt.Sprintf("%s (%v). Remote success does not prove the requested resolution; inspect the model before relying on it.", unverified, err)
	case len(sizes) == 0:
		return unverified + ": the model has no embedded images. Inspect the model before relying on it."
	case requested == 0:
		return "Texture sizes in the model: " + sizesText(sizes) + "."
	case partial:
		return fmt.Sprintf("Texture sizes in the model: %s. Only some parts were textured, so they are not compared with the requested %dx%d.", sizesText(sizes), requested, requested)
	}
	for _, s := range sizes {
		if s.Width < requested || s.Height < requested {
			return fmt.Sprintf("WARNING: requested %dx%d textures, but the model has %s. The requested resolution was not achieved. Do not repeat this paid operation automatically.", requested, requested, sizesText(sizes))
		}
	}
	return fmt.Sprintf("Texture resolution verified: %s (requested %dx%d).", sizesText(sizes), requested, requested)
}

// verifyTextures checks the model a finished texture job delivered. It does
// nothing while the job is still running or when it was not waited for.
func (a *app) verifyTextures(ctx context.Context, r *reply, projectID string, requested int, partial bool) {
	if r.Card == nil || (r.Card.Status != "done" && r.Card.Status != "saved") {
		return
	}
	sizes, err := a.deliveredTextures(ctx, r, projectID, r.Card.OperatorID)
	r.line("%s", textureVerdict(sizes, err, requested, partial))
}

func (a *app) deliveredTextures(ctx context.Context, r *reply, projectID, op string) ([]texSize, error) {
	p, err := a.project(ctx, projectID, op)
	if err != nil {
		return nil, err
	}
	if op != "" && p.OperatorID != "" && p.OperatorID != op {
		return nil, errors.New("Tripo returned another version of the project than this job's")
	}
	var data []byte
	if len(r.Card.Saved) > 0 { // download_to already saved this version
		data, _ = os.ReadFile(r.Card.Saved[0])
	}
	if data == nil {
		if p.ModelURL == "" {
			return nil, errors.New("the project has no model file yet")
		}
		body, err := a.open(ctx, p.ModelURL)
		if err != nil {
			return nil, err
		}
		defer body.Close()
		if data, err = io.ReadAll(io.LimitReader(body, 1<<30)); err != nil {
			return nil, err
		}
	}
	return glbTextureSizes(data)
}

// ---- tripo_generate_pbr, tripo_upscale_texture ----

type postArgs struct {
	ProjectID string `json:"project_id"`
	followArgs
}

func generatePBR(ctx context.Context, a *app, x postArgs) (reply, error) {
	p, err := a.preflight(ctx, x.ProjectID)
	if err != nil {
		return reply{}, err
	}
	if err := assertSupports("pbr", p, false); err != nil {
		return reply{}, err
	}
	j := job{
		Tool: "tripo_generate_pbr", Title: "PBR maps", Keys: []string{"PBR"}, ProjectID: x.ProjectID, Follow: x.followArgs,
		Summary: fmt.Sprintf("PBR maps (normal, roughness, metallic) for %s", x.ProjectID),
	}
	r, err := a.paid(ctx, j, func() (receipt, error) {
		return a.submit(ctx, "/v2/studio/operation/pbr_generate", map[string]any{"model_version": textureModelVersion, "project_id": x.ProjectID})
	})
	if err == nil && p.is("is_pbr") {
		r.line("Note: this project already reports PBR maps.")
	}
	return r, err
}

type upscaleArgs struct {
	ProjectID string `json:"project_id"`
	Quality   string `json:"quality"`
	followArgs
}

func upscaleTexture(ctx context.Context, a *app, x upscaleArgs) (reply, error) {
	wire, key := "detailed", "Upscaler"
	switch x.Quality {
	case "", "detailed":
	case "ultra":
		wire, key = "extreme", "UpscalerExtreme"
	default:
		return reply{}, fmt.Errorf("quality must be detailed (4K) or ultra (8K), not %q", x.Quality)
	}
	p, err := a.preflight(ctx, x.ProjectID)
	if err != nil {
		return reply{}, err
	}
	if err := assertSupports("texture_upscale", p, false); err != nil {
		return reply{}, err
	}
	if wire == "detailed" && p.is("is_ultra_textured") {
		return reply{}, errors.New("an 8K project cannot be reduced to the 4K target in Studio")
	}
	label := qualityLabel(wire)
	j := job{
		Tool: "tripo_upscale_texture", Title: "Upscale texture " + label, Keys: []string{key}, ProjectID: x.ProjectID, Follow: x.followArgs,
		Settings: [][2]string{{"Target", label}},
		Summary:  fmt.Sprintf("upscale textures to %s on %s", label, x.ProjectID),
	}
	r, err := a.paid(ctx, j, func() (receipt, error) {
		return a.submit(ctx, "/v2/studio/operation/texture_upscaler", map[string]any{
			"model_version": textureModelVersion, "project_id": x.ProjectID, "texture_quality": wire,
		})
	})
	if err == nil {
		if p.is("is_ultra_textured") {
			r.line("Note: this project is already 8K; Studio may only allow an 8K target.")
		}
		a.verifyTextures(ctx, &r, x.ProjectID, textureSize(wire), false)
	}
	return r, err
}

// ---- Magic Brush ----

type previewArgs struct {
	ProjectID       string    `json:"project_id"`
	Prompt          string    `json:"prompt"`
	RenderImagePath string    `json:"render_image_path"`
	CameraMatrix    []float64 `json:"camera_matrix"`
	Strength        *float64  `json:"strength"`
	SaveTo          string    `json:"save_to"`
	DryRun          bool      `json:"dry_run"`
	WaitSeconds     int       `json:"wait_seconds"`
	OperatorID      string    `json:"operator_id"`
}

func previewTextureEdit(ctx context.Context, a *app, x previewArgs) (reply, error) {
	if x.OperatorID != "" {
		if x.ProjectID != "" || x.Prompt != "" || x.RenderImagePath != "" || x.CameraMatrix != nil || x.Strength != nil || x.DryRun {
			return reply{}, errors.New("operator_id fetches an earlier preview: pass it alone (and save_to if you like)")
		}
		r := reply{Card: &card{Kind: "job", Title: "Magic Brush preview", OperatorID: x.OperatorID}}
		it, err := a.wait(ctx, x.OperatorID, seconds(x.WaitSeconds, 0))
		if err != nil {
			return r, err
		}
		return r, a.previewResult(ctx, &r, it, x.SaveTo)
	}
	if x.ProjectID == "" || x.Prompt == "" || x.RenderImagePath == "" || len(x.CameraMatrix) == 0 {
		return reply{}, errors.New("pass project_id, prompt, render_image_path and camera_matrix (or operator_id to fetch an earlier preview)")
	}
	if len(x.CameraMatrix) != 16 {
		return reply{}, errors.New("camera_matrix must hold exactly 16 numbers (the camera's world matrix)")
	}
	for _, v := range x.CameraMatrix {
		if math.IsNaN(v) || math.IsInf(v, 0) || math.Abs(v) > 1e9 {
			return reply{}, errors.New("camera_matrix must hold 16 finite numbers")
		}
	}
	prompt, err := validatePrompt(x.Prompt, "prompt")
	if err != nil {
		return reply{}, err
	}
	strength := 0.5
	if x.Strength != nil {
		strength = *x.Strength
	}
	if !(strength >= 0 && strength <= 1) {
		return reply{}, errors.New("strength must be between 0 and 1")
	}
	img, err := readImage(x.RenderImagePath)
	if err != nil {
		return reply{}, err
	}
	if img.format != "webp" {
		return reply{}, fmt.Errorf("render_image_path must be a WebP image (the 2x viewport render), not %s", img.format)
	}
	p, err := a.preflight(ctx, x.ProjectID)
	if err != nil {
		return reply{}, err
	}
	if err := assertSupports("retexture", p, false); err != nil {
		return reply{}, err
	}
	j := job{
		Tool: "tripo_preview_texture_edit", Title: "Magic Brush preview", Keys: []string{"MagicBrush"}, ProjectID: x.ProjectID,
		Follow: followArgs{DryRun: x.DryRun}, Inputs: []input{{"render", img}},
		Settings: [][2]string{{"Prompt", truncate(prompt, 80)}, {"Strength", fmt.Sprint(strength)}},
		Summary:  fmt.Sprintf("Magic Brush preview %q (strength %g) on %s", truncate(prompt, 60), strength, x.ProjectID),
	}
	r, err := a.paid(ctx, j, func() (receipt, error) {
		ref, err := a.putImage(ctx, img)
		if err != nil {
			return receipt{}, err
		}
		return a.submit(ctx, "/v2/studio/operation/retexture_generate", map[string]any{
			"camera_matrix": x.CameraMatrix, "model_version": textureModelVersion, "project_id": x.ProjectID,
			"prompt": prompt, "render_image": ref, "strength": strength,
		})
	})
	if err != nil || x.DryRun {
		return r, err
	}
	dropNext(&r)
	it, err := a.wait(ctx, r.Card.OperatorID, seconds(x.WaitSeconds, 300))
	if err != nil {
		return r, err
	}
	return r, a.previewResult(ctx, &r, it, x.SaveTo)
}

// previewResult reports a Magic Brush job: when it has succeeded, the preview
// image is saved and its camera matrix printed.
func (a *app) previewResult(ctx context.Context, r *reply, it progress, saveTo string) error {
	op := r.Card.OperatorID
	r.Card.Progress = it.Progress
	switch it.Status {
	case "success":
	case "failed", "cancelled", "expired", "banned":
		r.Card.Status = "failed"
		return fmt.Errorf("job %s ended as %s", op, it.Status)
	default:
		r.Card.Status = "running"
		r.line("Preview job %s is still %s (%.0f%%). Fetch it later, free: tripo_preview_texture_edit operator_id=%s", op, it.Status, it.Progress, op)
		return nil
	}
	var pv struct {
		CameraMatrix []float64 `json:"camera_matrix"`
		URL          string    `json:"url"`
	}
	if err := a.call(ctx, "POST", "/v2/studio/operation/get_retexture", map[string]string{"operator_id": op}, &pv); err != nil {
		return err
	}
	if pv.URL == "" {
		return fmt.Errorf("job %s succeeded but Tripo has no preview image for it yet; try again", op)
	}
	dest := saveTo
	if dest == "" {
		dest = filepath.Join(a.dir, "retexture") + string(os.PathSeparator)
	}
	ext := ".png"
	if u, err := url.Parse(pv.URL); err == nil && filepath.Ext(u.Path) != "" {
		ext = strings.ToLower(filepath.Ext(u.Path))
	}
	path, err := outputPath(dest, "retexture-"+op, ext)
	if err != nil {
		return err
	}
	final, n, _, err := a.save(ctx, pv.URL, path)
	if err != nil {
		return err
	}
	r.Card.Status, r.Card.Saved = "saved", []string{final}
	m, _ := json.Marshal(pv.CameraMatrix)
	r.line("Preview image saved to %s, %s.\ncamera_matrix: %s", final, size(n), m)
	return nil
}

type applyArgs struct {
	ProjectID string `json:"project_id"`
	Textures  []struct {
		PartName  string `json:"part_name"`
		ImagePath string `json:"image_path"`
	} `json:"textures"`
	followArgs
}

func applyTextureEdits(ctx context.Context, a *app, x applyArgs) (reply, error) {
	raw := make([]string, len(x.Textures))
	for i, t := range x.Textures {
		raw[i] = t.PartName
	}
	names, err := validatePartNames(raw, "textures[].part_name")
	if err != nil {
		return reply{}, err
	}
	imgs := make([]*localImage, len(names))
	var in []input
	for i, t := range x.Textures {
		if imgs[i], err = readImage(t.ImagePath); err != nil {
			return reply{}, err
		}
		if i < 8 { // the card shows a few thumbnails, not two hundred
			in = append(in, input{names[i], imgs[i]})
		}
	}
	p, err := a.preflight(ctx, x.ProjectID)
	if err != nil {
		return reply{}, err
	}
	if err := assertSupports("retexture", p, false); err != nil {
		return reply{}, err
	}
	if _, err := a.checkParts(ctx, p, names); err != nil {
		return reply{}, err
	}
	j := job{
		Tool: "tripo_apply_texture_edits", Title: "Apply texture edits", Keys: []string{"MagicBrush"}, Inputs: in, ProjectID: x.ProjectID, Follow: x.followArgs,
		Settings: [][2]string{{"Parts", shortList(names)}},
		Summary:  fmt.Sprintf("apply baked textures to %d part(s) (%s) on %s", len(names), shortList(names), x.ProjectID),
	}
	return a.paid(ctx, j, func() (receipt, error) {
		imageMap := make([]map[string]any, len(names))
		for i, img := range imgs {
			ref, err := a.putImage(ctx, img)
			if err != nil {
				return receipt{}, err
			}
			imageMap[i] = map[string]any{"image": ref, "part_name": names[i]}
		}
		return a.submit(ctx, "/v2/studio/operation/apply_retexture", map[string]any{
			"image_map": imageMap, "model_version": textureModelVersion, "project_id": x.ProjectID,
		})
	})
}

// ---- import ----

const (
	maxImportBytes = 150 << 20
	maxImportFaces = 3_000_000
)

type importArgs struct {
	FilePath        string    `json:"file_path"`
	Name            string    `json:"name"`
	TransformMatrix []float64 `json:"transform_matrix"`
	UseOriginalUV   bool      `json:"use_original_uv"`
	DryRun          bool      `json:"dry_run"`
}

// normalizeMatrix checks a transform_matrix as the plugin does: 16 finite
// values, an affine column-major matrix with final row 0 0 0 1, not singular.
func normalizeMatrix(in []float64) ([]float64, error) {
	if in == nil {
		return []float64{1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1}, nil
	}
	m := in
	if len(m) != 16 {
		return nil, errors.New("transform_matrix must contain exactly 16 finite values between -1,000,000 and 1,000,000")
	}
	for _, v := range m {
		if math.IsNaN(v) || math.IsInf(v, 0) || math.Abs(v) > 1e6 {
			return nil, errors.New("transform_matrix must contain exactly 16 finite values between -1,000,000 and 1,000,000")
		}
	}
	if math.Abs(m[3]) > 1e-9 || math.Abs(m[7]) > 1e-9 || math.Abs(m[11]) > 1e-9 || math.Abs(m[15]-1) > 1e-9 {
		return nil, errors.New("transform_matrix must be an affine column-major matrix with final row [0,0,0,1]")
	}
	det := m[0]*(m[5]*m[10]-m[9]*m[6]) - m[4]*(m[1]*m[10]-m[9]*m[2]) + m[8]*(m[1]*m[6]-m[5]*m[2])
	if math.IsNaN(det) || math.Abs(det) < 1e-12 {
		return nil, errors.New("transform_matrix must have a non-singular 3D scale/rotation part")
	}
	return m, nil
}

// inspectModel is a light version of the plugin's content inspection: the
// format from the extension, a container check, and a face count (GLB, OBJ,
// STL) against Studio's 3,000,000-face import limit.
func inspectModel(path string, data []byte) (format string, faces int, err error) {
	format = strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")
	bad := func(msg string) (string, int, error) { return "", 0, fmt.Errorf("%s: %s", filepath.Base(path), msg) }
	switch format {
	case "glb":
		if len(data) < 12 || binary.LittleEndian.Uint32(data[8:]) != uint32(len(data)) {
			return bad("the GLB's declared length does not match the file size")
		}
		d, _, err := readGLB(bytes.NewReader(data), false)
		if err != nil {
			return bad(err.Error())
		}
		for i := range d.Meshes {
			faces += d.faces(i)
		}
	case "obj":
		verts := 0
		for rest := data; len(rest) > 0; {
			line := rest
			if i := bytes.IndexByte(rest, '\n'); i >= 0 {
				line, rest = rest[:i], rest[i+1:]
			} else {
				rest = nil
			}
			line = bytes.TrimSpace(line)
			switch {
			case bytes.HasPrefix(line, []byte("v ")):
				verts++
			case bytes.HasPrefix(line, []byte("f ")):
				if n := len(bytes.Fields(line)) - 1; n >= 3 {
					faces += n - 2
				}
			}
		}
		if verts < 3 {
			return bad("the OBJ has fewer than three vertices")
		}
	case "stl":
		if len(data) >= 84 && 84+uint64(binary.LittleEndian.Uint32(data[80:]))*50 == uint64(len(data)) {
			faces = int(binary.LittleEndian.Uint32(data[80:]))
		} else {
			faces = bytes.Count(bytes.ToLower(data), []byte("facet normal"))
		}
	case "fbx":
		if !bytes.HasPrefix(data, []byte("Kaydara FBX Binary")) {
			return bad("only binary FBX files are accepted")
		}
		return format, 0, nil // Studio counts FBX faces itself
	default:
		return bad("the file must end in .glb, .obj, .fbx or .stl")
	}
	if faces < 1 {
		return bad("the model has no polygon faces")
	}
	if faces > maxImportFaces {
		return bad(fmt.Sprintf("%s faces is over Studio's %s-face import limit", thousands(faces), thousands(maxImportFaces)))
	}
	return format, faces, nil
}

func importModel(ctx context.Context, a *app, x importArgs) (reply, error) {
	abs, err := filepath.Abs(x.FilePath)
	if err != nil {
		return reply{}, err
	}
	st, err := os.Stat(abs)
	if err != nil {
		return reply{}, fmt.Errorf("cannot read model: %w", err)
	}
	if !st.Mode().IsRegular() || st.Size() < 1 || st.Size() > maxImportBytes {
		return reply{}, fmt.Errorf("%s must be a regular file of 1 byte to 150 MiB", abs)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return reply{}, fmt.Errorf("cannot read model: %w", err)
	}
	format, faces, err := inspectModel(abs, data)
	if err != nil {
		return reply{}, err
	}
	matrix, err := normalizeMatrix(x.TransformMatrix)
	if err != nil {
		return reply{}, err
	}
	name := x.Name
	if name == "" {
		name = filepath.Base(abs)
	}
	name = strings.TrimSpace(controlChars.ReplaceAllString(name, " "))
	if n := utf8.RuneCountInString(name); n < 1 || n > 255 {
		return reply{}, errors.New("name must be 1 to 255 characters")
	}
	faceText := "faces counted by Studio"
	if faces > 0 {
		faceText = thousands(faces) + " faces"
	}
	j := job{
		Tool: "tripo_import_model", Title: "Import model", Follow: followArgs{DryRun: x.DryRun},
		Settings: [][2]string{{"File", filepath.Base(abs)}, {"Format", format}, {"Size", size(st.Size())}, {"Faces", faceText}, {"Original UV", fmt.Sprint(x.UseOriginalUV)}},
		Summary:  fmt.Sprintf("import %s (%s, %s, %s) as a new project %q", filepath.Base(abs), format, size(st.Size()), faceText, name),
	}
	r, err := a.paid(ctx, j, func() (receipt, error) {
		bucket, key, err := a.putFile(ctx, format, data)
		if err != nil {
			return receipt{}, fmt.Errorf("not submitted, nothing charged: %w", err)
		}
		return a.submit(ctx, "/v2/studio/operation/import_user_model", map[string]any{
			"format": format, "model": map[string]string{"bucket": bucket, "key": key},
			"name": name, "transform_matrix": matrix, "use_original_uv": x.UseOriginalUV,
		})
	})
	// Import has no price key: say "free" where paid() printed an empty breakdown.
	r.Text = strings.Replace(r.Text, "credits (; prices", "credits (free; prices", 1)
	if r.Card != nil && r.Card.Cost != nil && r.Card.Cost.Breakdown == "" {
		r.Card.Cost.Breakdown = "free"
	}
	return r, err
}
