package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
)

func init() {
	register(
		tool{
			Name:        "tripo_texture",
			Description: "PAID: texture an existing project, keeping its geometry. Source: mode image (default; image_path), multiview (the views, as in tripo_generate_model) or text (prompt). quality standard = 2K, 10 credits (default); detailed = 4K, 20; ultra = 8K, 30. A style (style heritage, mecha, mecha_pop or wood, or your own style_image_path) adds 5 credits and cannot be combined with multiview. part_names textures only those parts (exact names from tripo_list_parts); the project need not be segmented for the whole model. After the job the delivered texture size is checked against quality and a warning is added if it was not met. Submits at once.",
			InputSchema: object([]string{"project_id"}, viewProps, followProps, props{
				"project_id":       str("Project to texture."),
				"mode":             enum("text needs prompt; image or multiview need the images. Left out, it follows the arguments given.", "text", "image", "multiview"),
				"prompt":           str("mode text: describe the texture (1 to 1000 characters)."),
				"style":            enum("Built-in style reference (+5 credits).", "heritage", "mecha", "mecha_pop", "wood"),
				"style_image_path": str("Your own style reference image (+5 credits); instead of style."),
				"quality":          enum("Default standard.", "standard", "detailed", "ultra"),
				"delight":          boolean("Remove baked lighting from the result. Default true."),
				"alignment":        enum("Default original_image.", "original_image", "geometry"),
				"part_names":       strList("Texture only these parts (exact names from tripo_list_parts); default the whole model."),
			}),
			UI:  true,
			run: handler(texture),
		},
		tool{
			Name:        "tripo_remesh",
			Description: "PAID: retopologize an existing project to face_limit (or only its part_names). smart_poly=true is the website's Retopology: 35 credits for triangles (up to 20,000), 40 for quads (up to 10,000); not available for Smart Mesh projects. smart_poly=false is a plain remesh: 5 credits triangles (up to 150,000), 10 quads (up to 50,000). Submits at once.",
			InputSchema: object([]string{"project_id", "face_limit"}, followProps, props{
				"project_id": str("Project to remesh."),
				"face_limit": integer("Target face count (500 or more)."),
				"quad":       boolean("Quad topology. Default false."),
				"smart_poly": boolean("Smart Poly retopology. Default false."),
				"part_names": strList("Exact part names from tripo_list_parts to remesh; default every mesh in the model."),
			}),
			UI:  true,
			run: handler(remesh),
		},
		tool{
			Name:        "tripo_segment",
			Description: "PAID, 40 credits: split a project into semantic parts (the first step of the part workflow). Not for quad or rigged models. Afterwards: tripo_list_parts shows the parts, tripo_complete_parts fills open ones, tripo_texture and tripo_remesh take part_names. Submits at once.",
			InputSchema: object([]string{"project_id"}, followProps, props{
				"project_id":  str("Project to segment."),
				"granularity": enum("Default balanced.", "coarse", "balanced", "fine"),
			}),
			UI:  true,
			run: handler(segment),
		},
	)
}

type textureArgs struct {
	ProjectID string `json:"project_id"`
	Mode      string `json:"mode"`
	Prompt    string `json:"prompt"`
	viewArgs
	Style          string   `json:"style"`
	StyleImagePath string   `json:"style_image_path"`
	Quality        string   `json:"quality"`
	Delight        *bool    `json:"delight"`
	Alignment      string   `json:"alignment"`
	PartNames      []string `json:"part_names"`
	followArgs
}

// builtinStyles are the style references Studio's texture panel offers.
var builtinStyles = map[string]map[string]any{
	"heritage":  {"bucket": "tripo-data", "key": "tripo-studio/style_image/heritage.png"},
	"mecha":     {"bucket": "tripo-data", "key": "tripo-studio/style_image/mecha.png"},
	"mecha_pop": {"bucket": "tripo-data", "key": "tripo-studio/style_image/mecha_pop.png"},
	"wood":      {"bucket": "tripo-data", "key": "tripo-studio/style_image/wood.jpg"},
}

// plan settles the texture source (text, image or multiview) and checks which
// source and style may be combined, before any file is read. paths is nil for text.
func (x textureArgs) plan() (mode string, paths []string, err error) {
	given := x.viewArgs.given()
	mode = x.Mode
	if mode == "" && x.Prompt != "" && !given {
		mode = "text"
	}
	switch mode {
	case "text":
		if given {
			return "", nil, errors.New("mode text takes a prompt, not images: drop the images or use mode image")
		}
	case "", "image", "multiview":
		if x.Prompt != "" {
			return "", nil, errors.New("prompt is only for mode text")
		}
		if mode == "" && !given {
			return "", nil, errors.New("no input: pass prompt (mode text), image_path, or the views (multiview)")
		}
		var multiview bool
		if paths, multiview, err = x.paths(); err != nil {
			return "", nil, err
		}
		got := map[bool]string{false: "image", true: "multiview"}[multiview]
		if mode != "" && mode != got {
			return "", nil, fmt.Errorf("mode %s does not fit the images given (they are %s input)", mode, got)
		}
		mode = got
	default:
		return "", nil, fmt.Errorf("mode must be text, image or multiview, not %q", mode)
	}
	if x.Style != "" && x.StyleImagePath != "" {
		return "", nil, errors.New("choose either a built-in style or style_image_path, not both")
	}
	if x.Style != "" && builtinStyles[x.Style] == nil {
		return "", nil, fmt.Errorf("style must be heritage, mecha, mecha_pop or wood, not %q", x.Style)
	}
	if mode == "multiview" && (x.Style != "" || x.StyleImagePath != "") {
		return "", nil, errors.New("Studio does not combine a style image with multiview texturing")
	}
	return mode, paths, nil
}

func texture(ctx context.Context, a *app, x textureArgs) (reply, error) {
	mode, paths, err := x.plan()
	if err != nil {
		return reply{}, err
	}
	multiview := mode == "multiview"
	wire, key, err := textureQuality(x.Quality)
	if err != nil {
		return reply{}, err
	}
	align, err := alignment(x.Alignment)
	if err != nil {
		return reply{}, err
	}
	names, err := optionalPartNames(x.PartNames, "part_names")
	if err != nil {
		return reply{}, err
	}
	var prompt string
	if mode == "text" {
		if prompt, err = validatePrompt(x.Prompt, "prompt"); err != nil {
			return reply{}, err
		}
	}
	imgs, err := readImages(paths)
	if err != nil {
		return reply{}, err
	}
	var styleImg *localImage
	if x.StyleImagePath != "" {
		if styleImg, err = readImage(x.StyleImagePath); err != nil {
			return reply{}, err
		}
	}
	p, err := a.preflight(ctx, x.ProjectID)
	if err != nil {
		return reply{}, err
	}
	if _, err := a.checkParts(ctx, p, names); err != nil {
		return reply{}, err
	}
	delight := x.Delight == nil || *x.Delight
	keys := []string{"TextureGeneration"}
	if key != "" {
		keys = append(keys, key)
	}
	styled := x.Style != "" || styleImg != nil
	if styled {
		keys = append(keys, "TextureStyle")
	}
	body := map[string]any{"model_version": textureModelVersion, "delight": delight, "project_id": x.ProjectID, "texture_alignment": align, "texture_quality": wire}
	scope := "whole model"
	if len(names) > 0 {
		body["part_names"] = names
		scope = shortList(names)
	}
	in := inputs(imgs, multiview)
	source := describeInputs(in)
	settings := [][2]string{{"Source", mode}, {"Quality", qualityLabel(wire)}, {"Delight", fmt.Sprint(delight)}, {"Alignment", align}, {"Parts", scope}}
	if mode == "text" {
		body["prompt_text"] = prompt
		source = fmt.Sprintf("prompt %q", truncate(prompt, 60))
	}
	if x.Style != "" {
		body["style_image"] = builtinStyles[x.Style]
		source += ", style " + x.Style
		settings = append(settings, [2]string{"Style", x.Style})
	}
	if styleImg != nil {
		in = append(in, input{"style", styleImg})
		source += ", style " + filepath.Base(styleImg.path)
		settings = append(settings, [2]string{"Style", filepath.Base(styleImg.path)})
	}
	j := job{
		Tool: "tripo_texture", Title: "Texture " + qualityLabel(wire), Keys: keys, Inputs: in, ProjectID: x.ProjectID, Follow: x.followArgs,
		Settings: settings,
		Summary:  fmt.Sprintf("texture %s (%s, delight %t) on %s, from %s", qualityLabel(wire), scope, delight, x.ProjectID, source),
	}
	r, err := a.paid(ctx, j, func() (receipt, error) {
		if mode != "text" {
			refs, err := a.uploadAll(ctx, imgs)
			if err != nil {
				return receipt{}, err
			}
			if multiview {
				body["images"] = refs
			} else {
				body["image"] = refs[0]
			}
		}
		if styleImg != nil {
			ref, err := a.upload(ctx, styleImg)
			if err != nil {
				return receipt{}, fmt.Errorf("not submitted, nothing charged: %w", err)
			}
			body["style_image"] = map[string]any{"bucket": ref.Bucket, "image_audit_result": ref.Audit, "key": ref.Key}
		}
		return a.submit(ctx, "/v2/studio/operation/texture_model", body)
	})
	if err == nil {
		if p.is("is_ultra_textured") {
			r.line("Note: this project is already 8K; Studio may only allow an 8K target.")
		}
		a.verifyTextures(ctx, &r, x.ProjectID, textureSize(wire), len(names) > 0)
	}
	return r, err
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "..."
	}
	return s
}

type remeshArgs struct {
	ProjectID string   `json:"project_id"`
	FaceLimit int      `json:"face_limit"`
	Quad      bool     `json:"quad"`
	SmartPoly bool     `json:"smart_poly"`
	PartNames []string `json:"part_names"`
	followArgs
}

func remesh(ctx context.Context, a *app, x remeshArgs) (reply, error) {
	limit, mode := 150000, "Remesh"
	switch {
	case x.SmartPoly && x.Quad:
		limit, mode = 10000, "Smart Poly retopology"
	case x.SmartPoly:
		limit, mode = 20000, "Smart Poly retopology"
	case x.Quad:
		limit = 50000
	}
	topo := "triangles"
	if x.Quad {
		topo = "quads"
	}
	if x.FaceLimit < 500 || x.FaceLimit > limit {
		return reply{}, fmt.Errorf("face_limit must be 500 to %s for %s with %s", thousands(limit), mode, topo)
	}
	p, err := a.preflight(ctx, x.ProjectID)
	if err != nil {
		return reply{}, err
	}
	if err := assertSupports("remesh", p, x.SmartPoly); err != nil {
		return reply{}, err
	}
	names, err := optionalPartNames(x.PartNames, "part_names")
	if err != nil {
		return reply{}, err
	}
	if _, err := a.checkParts(ctx, p, names); err != nil {
		return reply{}, err
	}
	keys := []string{"Retopology_Triangle"}
	if x.Quad {
		keys[0] = "Retopology_Quad"
	}
	if x.SmartPoly {
		keys = append(keys, "Retopology_SmartPoly")
	}
	scope := "every mesh"
	if len(names) > 0 {
		scope = shortList(names)
	}
	j := job{
		Tool: "tripo_remesh", Title: mode, Keys: keys, ProjectID: x.ProjectID, Follow: x.followArgs,
		Settings: [][2]string{{"Faces", thousands(x.FaceLimit)}, {"Topology", topo}, {"Parts", scope}},
		Summary:  fmt.Sprintf("%s to %s %s (%s) on %s", mode, thousands(x.FaceLimit), topo, scope, x.ProjectID),
	}
	return a.paid(ctx, j, func() (receipt, error) {
		list := names
		if len(list) == 0 {
			if list, err = a.partNames(ctx, p); err != nil {
				return receipt{}, fmt.Errorf("not submitted, nothing charged: %w", err)
			}
		}
		return a.submit(ctx, "/v2/studio/operation/remesh", map[string]any{
			"bake": true, "face_limit": x.FaceLimit, "model_version": "default", "part_name_list": list,
			"project_id": x.ProjectID, "quad": x.Quad, "smart_poly": x.SmartPoly,
		})
	})
}

// partNames reads the mesh names from the project's current model, the part
// names Studio's post-processes expect.
func (a *app) partNames(ctx context.Context, p *project) ([]string, error) {
	d, err := a.modelDoc(ctx, p)
	if errors.Is(err, errFBX) {
		return nil, errors.New("the model is FBX (a quad Smart Mesh); pass part_names")
	}
	if err != nil {
		return nil, err
	}
	parts, _ := d.parts()
	if len(parts) == 0 {
		return nil, errors.New("the model has no named meshes; pass part_names")
	}
	names := make([]string, len(parts))
	for i, pt := range parts {
		names[i] = pt.Name
	}
	return names, nil
}

type segmentArgs struct {
	ProjectID   string `json:"project_id"`
	Granularity string `json:"granularity"`
	followArgs
}

func segment(ctx context.Context, a *app, x segmentArgs) (reply, error) {
	g := x.Granularity
	if g == "" {
		g = "balanced"
	}
	if g != "coarse" && g != "balanced" && g != "fine" {
		return reply{}, errors.New("granularity must be coarse, balanced or fine")
	}
	p, err := a.preflight(ctx, x.ProjectID)
	if err != nil {
		return reply{}, err
	}
	if err := assertSupports("segmentation", p, false); err != nil {
		return reply{}, err
	}
	j := job{
		Tool: "tripo_segment", Title: "Segmentation", Keys: []string{"Segmentation"}, ProjectID: x.ProjectID, Follow: x.followArgs,
		Settings: [][2]string{{"Granularity", g}},
		Summary:  fmt.Sprintf("segmentation (%s) of %s", g, x.ProjectID),
	}
	r, err := a.paid(ctx, j, func() (receipt, error) {
		return a.submit(ctx, "/v2/studio/operation/ai_segmentation", map[string]any{
			"model_version": segmentationModelVersion, "project_id": x.ProjectID, "segmentation_granularity": g,
		})
	})
	if err == nil && !x.DryRun {
		r.line("Next, once it is done: tripo_list_parts project_id=%s (part names and face counts); then tripo_complete_parts to fill open parts, and tripo_texture / tripo_remesh with part_names to work on single parts.", x.ProjectID)
		if r.Card != nil {
			r.Card.Next = append(r.Card.Next, cardAction{"List parts", fmt.Sprintf("List the parts of Tripo project %s with tripo_list_parts.", x.ProjectID)})
		}
	}
	return r, err
}
