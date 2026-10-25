package main

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// tripo_export_model: Studio's export dialog with all its options, for the
// account's own projects. Port of model.export from tripo-studio-plugin 0.3.12
// (src/ops/studio-extras.mjs). tripo_download keeps its simple glb/fbx/obj export.

func init() {
	register(tool{
		Name: "tripo_export_model",
		Description: "Export your own project with Studio's export settings and save it to path: glb, fbx, obj, usdz, stl or 3mf (may arrive as a .zip with textures; the extension follows the real file). " +
			"Treated as FREE, as the original plugin did for own projects (Studio's price list shows Export 5, never checked here; the result reports any change of your credit balance). " +
			"texture_size 512-8192 (default: the project's own, 2K/4K/8K), texture_packaging zip (default) or embedded, fbx_preset blender (default), mixamo or 3dsmax. with_animation exports the skeleton and animations (rigged projects; animations lists animation names, empty = all). " +
			"pack_uv, animate_in_place, enable_bake_animation + bake_animation_frame (rigged), export_vertex_colors (obj). dry_run=true checks the options and shows the request without exporting.",
		InputSchema: object([]string{"project_id", "format"}, props{
			"project_id":            str("Project to export (must be yours)."),
			"format":                enum("Export format.", "glb", "fbx", "obj", "usdz", "stl", "3mf"),
			"path":                  str("Output file, or a folder (then <name><ext>). Required unless dry_run."),
			"name":                  str("Name inside Studio's export (default the project name)."),
			"texture_size":          integer("512, 1024, 2048, 4096 or 8192. Default the project's own texture size; more does not add detail."),
			"texture_packaging":     enum("Default zip.", "zip", "embedded"),
			"fbx_preset":            enum("FBX flavour. Default blender.", "blender", "mixamo", "3dsmax"),
			"with_animation":        boolean("Export the skeleton and animations (rigged projects). Default false."),
			"animations":            strList("With with_animation: animation names to include (empty = Studio's choice)."),
			"pack_uv":               boolean("Pack the UV islands. Default false."),
			"animate_in_place":      boolean("Remove root motion from the animation. Default false."),
			"enable_bake_animation": boolean("Bake the animation (rigged projects). Default false."),
			"bake_animation_frame":  integer("Frame to bake, 0-1000000. Default 0."),
			"export_vertex_colors":  boolean("OBJ only: write vertex colors. Default false."),
			"wait_seconds":          integer("How long to wait for the export (default 900, max 3600)."),
			"dry_run":               boolean("Check the options; export nothing."),
		}),
		UI:  true,
		run: handler(exportModel),
	})
}

type exportArgs struct {
	ProjectID           string   `json:"project_id"`
	Format              string   `json:"format"`
	Path                string   `json:"path"`
	Name                string   `json:"name"`
	TextureSize         int      `json:"texture_size"`
	TexturePackaging    string   `json:"texture_packaging"`
	FbxPreset           string   `json:"fbx_preset"`
	WithAnimation       bool     `json:"with_animation"`
	Animations          []string `json:"animations"`
	PackUV              bool     `json:"pack_uv"`
	AnimateInPlace      bool     `json:"animate_in_place"`
	EnableBakeAnimation bool     `json:"enable_bake_animation"`
	BakeAnimationFrame  int      `json:"bake_animation_frame"`
	ExportVertexColors  bool     `json:"export_vertex_colors"`
	WaitSeconds         int      `json:"wait_seconds"`
	DryRun              bool     `json:"dry_run"`
}

// exportWire maps the tool's format to Studio's, and to the file extension.
var exportWire = map[string]string{"glb": "gltf", "fbx": "fbx", "obj": "obj", "usdz": "usdz", "stl": "stl", "3mf": "3mf"}

// planExport checks the options against the project as the plugin does and
// returns Studio's request body and the export name.
func planExport(p *project, x exportArgs) (map[string]any, string, error) {
	wire := exportWire[x.Format]
	if wire == "" {
		return nil, "", errors.New("format must be glb, fbx, obj, usdz, stl or 3mf")
	}
	if f := p.flag("is_owner"); f == nil || !*f {
		return nil, "", errors.New("tripo_export_model needs one of your own projects")
	}
	if !x.WithAnimation && len(x.Animations) > 0 {
		return nil, "", errors.New("animations needs with_animation=true")
	}
	if (x.WithAnimation || x.EnableBakeAnimation) && !p.is("is_rigged") {
		return nil, "", errors.New("skeleton and animation export need a rigged project (tripo_rig_model)")
	}
	if x.ExportVertexColors && x.Format != "obj" {
		return nil, "", errors.New("export_vertex_colors is an OBJ option")
	}
	packaging, preset := x.TexturePackaging, x.FbxPreset
	if packaging == "" {
		packaging = "zip"
	}
	if packaging != "zip" && packaging != "embedded" {
		return nil, "", errors.New("texture_packaging must be zip or embedded")
	}
	if preset == "" {
		preset = "blender"
	}
	if preset != "blender" && preset != "mixamo" && preset != "3dsmax" {
		return nil, "", errors.New("fbx_preset must be blender, mixamo or 3dsmax")
	}
	if x.BakeAnimationFrame < 0 || x.BakeAnimationFrame > 1000000 {
		return nil, "", errors.New("bake_animation_frame must be 0 to 1000000")
	}
	texture := x.TextureSize
	if texture == 0 {
		texture = 2048
		if p.is("is_ultra_textured") {
			texture = 8192
		} else if p.is("is_hd_textured") {
			texture = 4096
		}
	}
	switch texture {
	case 512, 1024, 2048, 4096, 8192:
	default:
		return nil, "", errors.New("texture_size must be 512, 1024, 2048, 4096 or 8192")
	}
	name := strings.TrimSpace(x.Name)
	switch {
	case name == "" && p.Name == "":
		name = "model-" + p.ID
	case name == "":
		name = safeName(p.Name)
	case len(name) > 255 || strings.ContainsAny(name, `/\`) || strings.ContainsFunc(name, func(r rune) bool { return r < 32 || r == 127 }):
		return nil, "", errors.New("name must be up to 255 characters without slashes or control characters")
	}
	anims := x.Animations
	if anims == nil {
		anims = []string{}
	}
	return map[string]any{
		"project_id": p.ID, "format": wire, "texture_size": texture, "texture_packaging": packaging,
		"fbx_preset": preset, "with_animation": x.WithAnimation, "animations": anims, "pack_uv": x.PackUV,
		"animate_in_place": x.AnimateInPlace, "enable_bake_animation": x.EnableBakeAnimation, "bake_animation_frame": x.BakeAnimationFrame,
		"export_vertex_colors": x.ExportVertexColors, "export_orientation": "-y", "name": name, "model_version": "default",
	}, name, nil
}

// exportFileURL runs the export request and waits for the file URL.
func (a *app) exportFileURL(ctx context.Context, body map[string]any, name string, timeout time.Duration) (string, error) {
	var r struct {
		ModelURL   string `json:"model_url"`
		OperatorID string `json:"operator_id"`
	}
	if err := a.call(ctx, "POST", "/v2/studio/operation/export", body, &r); err != nil {
		return "", err
	}
	if r.ModelURL != "" {
		return r.ModelURL, nil
	}
	if r.OperatorID == "" {
		return "", errors.New("Tripo started no export job")
	}
	it, err := a.wait(ctx, r.OperatorID, timeout)
	if err != nil {
		return "", err
	}
	if it.Status != "success" {
		return "", fmt.Errorf("export job %s is %s after %s (not downloaded); it may still finish, but there is no way to fetch it later, so export again", r.OperatorID, it.Status, timeout)
	}
	var d struct {
		ModelURL string `json:"model_url"`
	}
	if err := a.call(ctx, "POST", "/v2/studio/operation/download_with_name", map[string]string{"file_name": name, "operator_id": r.OperatorID}, &d); err != nil {
		return "", err
	}
	if d.ModelURL == "" {
		return "", errors.New("the export finished but Tripo gave no file URL")
	}
	return d.ModelURL, nil
}

// zipHolds reports whether a zip file has an entry ending in suffix.
func zipHolds(path, suffix string) bool {
	z, err := zip.OpenReader(path)
	if err != nil {
		return false
	}
	defer z.Close()
	for _, f := range z.File {
		if strings.HasSuffix(strings.ToLower(f.Name), suffix) {
			return true
		}
	}
	return false
}

func exportModel(ctx context.Context, a *app, x exportArgs) (reply, error) {
	if !x.DryRun && x.Path == "" {
		return reply{}, errors.New("path is required (or dry_run=true)")
	}
	p, err := a.project(ctx, x.ProjectID, "")
	if err != nil {
		return reply{}, err
	}
	body, name, err := planExport(p, x)
	if err != nil {
		return reply{}, err
	}
	r := reply{Card: &card{Kind: "project", Title: p.Name, Status: "quote"}}
	a.showProject(ctx, &r, p)
	r.Card.Settings = [][2]string{{"Format", x.Format}, {"Texture", fmt.Sprint(body["texture_size"])}, {"Packaging", fmt.Sprint(body["texture_packaging"])}}
	summary := fmt.Sprintf("export %s of %s %q, %v px textures, animation %t", x.Format, p.ID, name, body["texture_size"], x.WithAnimation)
	if x.DryRun {
		r.Text = summary + "\nDry run: nothing exported. Treated as free (unverified)."
		return r, nil
	}
	before, haveBefore := a.credits(ctx)
	src, err := a.exportFileURL(ctx, body, name, seconds(x.WaitSeconds, 900))
	if err != nil {
		r.Card.Status = "failed"
		return r, fmt.Errorf("export failed: %w", err)
	}
	ext := "." + x.Format
	dest, err := outputPath(x.Path, name, ext)
	if err != nil {
		return r, err
	}
	final, n, kind, err := a.save(ctx, src, dest)
	if err != nil {
		return r, err
	}
	if kind == "zip" && final != dest && (ext == ".usdz" || ext == ".3mf") && !zipHolds(final, ext) {
		// A USDZ or 3MF is itself a zip: sniffing mislabelled it.
		if os.Rename(final, dest) == nil {
			final = dest
		}
	}
	r.Text = fmt.Sprintf("Exported %s. Saved %s, %s.", summary, final, size(n))
	if final != dest {
		r.line("The file is %s, so the extension was changed.", strings.ToUpper(strings.TrimPrefix(filepath.Ext(final), ".")))
	}
	r.Card.Status, r.Card.Saved = "saved", []string{final}
	if after, ok := a.credits(ctx); ok && haveBefore {
		if after != before {
			r.line("Credits: %s before, %s after: this export WAS charged %d.", thousands(before), thousands(after), before-after)
		} else {
			r.line("Credits unchanged (%s): the export was free.", thousands(after))
		}
	}
	if strings.EqualFold(filepath.Ext(final), ".glb") {
		r.meta("model", a.previewModel(ctx, final, "", filepath.Base(final)))
	}
	return r, nil
}
