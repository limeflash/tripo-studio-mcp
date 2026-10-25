package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

func init() {
	register(tool{
		Name: "tripo_generate_model",
		Description: "PAID: generate 3D models. Submits at once and returns the cost, project_id and operator_id (every one when the call makes several models).\n" +
			"Input modes (mode; left out, it is guessed from the inputs): image = one reference image (image_path); multiview = front plus at least one of left/back/right; text = prompt (t_pose optional); batch = image_paths, up to 30 images, ONE MODEL PER IMAGE, cost x the number of images; studio_image = an output of a finished tripo_generate_image asset (studio_image_asset_id, output_index); studio_multiview = the four views of a tripo_generate_multiview asset (studio_multiview_asset_id).\n" +
			"Versions: P1 = Smart Mesh P1.0 (default for tier smart_mesh; 35 credits; 500-20,000 faces; triangles only). P2 = Smart Mesh P2.0 (100 credits; up to 25,000 faces; quad allowed; amount 1, 2 or 4 with face_limits makes that many variants of each input for the price of one). H3.1 = High Detail 3.1 (default for tier high_detail; 15 base, 25 with texture; +15 detailed geometry, +10/+20 4K/8K texture, +5 PBR, +5 quad). The same prices apply to every input mode.\n" +
			"Smart Mesh returns an untextured mesh: texture it with tripo_texture on the returned project_id. Defaults are the cheapest options: triangles, private, standard quality. With download_to, a call that makes several models saves each into its own numbered subfolder.",
		InputSchema: object(nil, viewProps, followProps, props{
			"mode":                      enum("Default: guessed from the inputs given.", "text", "image", "multiview", "batch", "studio_image", "studio_multiview"),
			"prompt":                    str("mode text: what to model, 1-1000 characters."),
			"t_pose":                    boolean("mode text: model a character in T-pose. Default false."),
			"image_paths":               strList("mode batch: 1-30 different local images; one model each."),
			"studio_image_asset_id":     str("mode studio_image: a successful generate_image asset (tripo_list_image_assets)."),
			"studio_multiview_asset_id": str("mode studio_multiview: a successful multiview_images asset."),
			"output_index":              integer("mode studio_image: which output of the asset, default 0."),
			"allow_sensitive":           boolean("studio modes: accept an image Studio marks sensitive (needs visibility private). Only after looking at it."),
			"tier":                      enum("smart_mesh or high_detail; may be left out when model_version is given.", "smart_mesh", "high_detail"),
			"model_version":             str("P1, P2, H3.1, H3.0, H2.5, or a full Studio version id."),
			"face_limit":                integer("Target face count (give this or face_limits)."),
			"amount":                    integer("Smart Mesh P2 only: variants per input, 1, 2 or 4. Default 1; costs the same as one."),
			"face_limits":               props{"type": "array", "items": props{"type": "integer"}, "description": "Smart Mesh P2 only: one face budget per variant; as many as amount."},
			"quad":                      boolean("Quad topology (P2 and High Detail only). Default false."),
			"symmetry":                  boolean("P2 only: force symmetry on/off; by default Studio's symmetry check decides."),
			"visibility":                enum("Default private.", "private", "public", "shareable"),
			"texture":                   boolean("High Detail only: texture during generation. Default false."),
			"texture_quality":           enum("High Detail with texture: standard 2K (default), detailed 4K, ultra 8K.", "standard", "detailed", "ultra"),
			"pbr":                       boolean("High Detail with texture: PBR maps. Default false."),
			"delight":                   boolean("High Detail with texture: remove baked lighting. Default true."),
			"texture_alignment":         enum("High Detail with texture. Default original_image.", "original_image", "geometry"),
			"geometry_quality":          enum("H3.1 only. Default standard.", "standard", "detailed"),
			"smart_poly":                boolean("High Detail only: low-poly Smart Poly mode. Default false."),
		}),
		UI:  true,
		run: handler(generateModel),
	})
}

var versionAliases = map[string]string{"p1": nexusP1, "p2": nexusP2, "h3.1": highDetailH31, "h3.0": "v3.0-20250812", "h2.5": "v2.5-20250123"}

var versionLabels = map[string]string{
	nexusP1: "Smart Mesh P1.0", nexusP2: "Smart Mesh P2.0",
	highDetailH31: "High Detail H3.1", "v3.0-20250812": "High Detail H3.0", "v2.5-20250123": "High Detail H2.5",
}

type genArgs struct {
	viewArgs
	Mode            string   `json:"mode"`
	Prompt          string   `json:"prompt"`
	TPose           bool     `json:"t_pose"`
	ImagePaths      []string `json:"image_paths"`
	StudioImageID   string   `json:"studio_image_asset_id"`
	StudioViewsID   string   `json:"studio_multiview_asset_id"`
	OutputIndex     *int     `json:"output_index"`
	AllowSensitive  bool     `json:"allow_sensitive"`
	Tier            string   `json:"tier"`
	ModelVersion    string   `json:"model_version"`
	FaceLimit       int      `json:"face_limit"`
	Amount          int      `json:"amount"`
	FaceLimits      []int    `json:"face_limits"`
	Quad            bool     `json:"quad"`
	Symmetry        *bool    `json:"symmetry"`
	Visibility      string   `json:"visibility"`
	Texture         bool     `json:"texture"`
	TextureQuality  string   `json:"texture_quality"`
	PBR             bool     `json:"pbr"`
	Delight         *bool    `json:"delight"`
	Alignment       string   `json:"texture_alignment"`
	GeometryQuality string   `json:"geometry_quality"`
	SmartPoly       bool     `json:"smart_poly"`
	followArgs
}

// textImageModel is the image model Studio uses to draw the picture behind a
// text-to-model request (the plugin's TEXT_IMAGE_MODEL_VERSION).
const textImageModel = "flux.1_dev"

// resolve settles the input mode and the local images it needs. Modes other
// than image and multiview must be the only kind of input given.
func (g genArgs) resolve() (mode string, paths []string, err error) {
	hasViews := g.ImagePath != "" || g.ViewsDir != "" || g.Front+g.Left+g.Back+g.Right != ""
	groups := []struct {
		set        bool
		name, mode string
	}{
		{hasViews, "image_path / views", "image"},
		{len(g.ImagePaths) > 0, "image_paths", "batch"},
		{g.StudioImageID != "", "studio_image_asset_id", "studio_image"},
		{g.StudioViewsID != "", "studio_multiview_asset_id", "studio_multiview"},
		{g.Prompt != "", "prompt", "text"},
	}
	mode = g.Mode
	if mode == "" {
		var set []string
		for _, gr := range groups {
			if gr.set {
				set = append(set, gr.mode)
			}
		}
		if len(set) != 1 {
			return "", nil, errors.New("pass mode (text, image, multiview, batch, studio_image, studio_multiview) with its inputs")
		}
		mode = set[0]
	}
	want := mode
	switch mode {
	case "multiview":
		want = "image"
	case "image", "text", "batch", "studio_image", "studio_multiview":
	default:
		return "", nil, fmt.Errorf("mode must be text, image, multiview, batch, studio_image or studio_multiview, not %q", mode)
	}
	for _, gr := range groups {
		if gr.set && gr.mode != want {
			return "", nil, fmt.Errorf("mode %s does not take %s", mode, gr.name)
		}
		if !gr.set && gr.mode == want {
			return "", nil, fmt.Errorf("mode %s needs %s", mode, gr.name)
		}
	}
	if g.TPose && mode != "text" {
		return "", nil, errors.New("t_pose is a text mode option")
	}
	if g.OutputIndex != nil && (mode != "studio_image" || *g.OutputIndex < 0 || *g.OutputIndex > 15) {
		return "", nil, errors.New("output_index (0 to 15) is for mode studio_image")
	}
	switch mode {
	case "image", "multiview":
		var multiview bool
		if paths, multiview, err = g.paths(); err != nil {
			return "", nil, err
		}
		if g.Mode == "" {
			mode = map[bool]string{true: "multiview", false: "image"}[multiview]
		} else if (mode == "multiview") != multiview {
			return "", nil, fmt.Errorf("mode %s needs %s", mode, map[bool]string{true: "front plus at least one other view", false: "image_path"}[mode == "multiview"])
		}
	case "batch":
		if len(g.ImagePaths) > 30 {
			return "", nil, errors.New("batch takes at most 30 images")
		}
		for _, p := range g.ImagePaths {
			if p == "" {
				return "", nil, errors.New("image_paths has an empty path")
			}
		}
		paths = g.ImagePaths
	}
	return mode, paths, nil
}

// genPlan is a checked generation request without its images.
type genPlan struct {
	body     map[string]any
	keys     []string
	title    string
	settings [][2]string
	variants int // Smart Mesh variants per input
}

// plan checks the options for an image (multiview=false) or multiview input.
func (g genArgs) plan(multiview bool) (genPlan, error) {
	if multiview {
		return g.planFor("multiview")
	}
	return g.planFor("image")
}

// planFor checks the options the way Studio allows them. mode is the wire
// shape of the request: text, image, multiview or batch (studio_image counts
// as image, studio_multiview as multiview).
func (g genArgs) planFor(mode string) (genPlan, error) {
	fail := func(format string, args ...any) (genPlan, error) { return genPlan{}, fmt.Errorf(format, args...) }
	version := g.ModelVersion
	if v, ok := versionAliases[strings.ToLower(version)]; ok {
		version = v
	}
	smart := strings.HasPrefix(version, "Nexus-")
	switch g.Tier {
	case "":
		if version == "" {
			return fail("pass tier (smart_mesh or high_detail) or model_version (P1, P2, H3.1)")
		}
	case "smart_mesh":
		if version == "" {
			version, smart = nexusP1, true
		} else if !smart {
			return fail("%s is not a Smart Mesh version", g.ModelVersion)
		}
	case "high_detail":
		if version == "" {
			version = highDetailH31
		} else if smart {
			return fail("%s is a Smart Mesh version, not High Detail", g.ModelVersion)
		}
	default:
		return fail("tier must be smart_mesh or high_detail")
	}
	title, ok := versionLabels[version]
	if !ok {
		return fail("unknown model_version %q; use P1, P2, H3.1, H3.0 or H2.5", g.ModelVersion)
	}
	vis := g.Visibility
	if vis == "" {
		vis = "private"
	}
	if vis != "private" && vis != "public" && vis != "shareable" {
		return fail("visibility must be private, public or shareable")
	}
	if g.AllowSensitive && vis != "private" {
		return fail("allow_sensitive needs visibility private")
	}
	if !smart && (g.Amount != 0 || len(g.FaceLimits) > 0) {
		return fail("amount and face_limits are Smart Mesh P2 settings")
	}
	// Smart Mesh variants: one budget per variant, as the plugin's normalizeSettings.
	variants, budgets := 1, []int{g.FaceLimit}
	if smart {
		if variants = g.Amount; variants == 0 {
			variants = max(len(g.FaceLimits), 1)
		}
		if variants != 1 && variants != 2 && variants != 4 || len(g.FaceLimits) > 0 && len(g.FaceLimits) != variants {
			return fail("amount must be 1, 2 or 4, and face_limits needs exactly that many entries")
		}
		if len(g.FaceLimits) > 0 {
			budgets = g.FaceLimits
		} else {
			budgets = make([]int, variants)
			for i := range budgets {
				budgets[i] = g.FaceLimit
			}
		}
	}
	if g.FaceLimit == 0 && len(g.FaceLimits) == 0 {
		return fail("face_limit is required")
	}
	topo := "triangles"
	if g.Quad {
		topo = "quads"
	}
	faces := make([]string, len(budgets))
	for i, b := range budgets {
		faces[i] = thousands(b)
	}
	p := genPlan{
		body:     map[string]any{"face_limit": budgets[0], "quad": g.Quad, "visibility": vis, "model_version": version},
		title:    title,
		settings: [][2]string{{"Faces", strings.Join(faces, " / ")}, {"Topology", topo}, {"Visibility", vis}},
		variants: variants,
	}
	if variants > 1 {
		var list []map[string]int
		for _, b := range budgets {
			list = append(list, map[string]int{"face_limit": b})
		}
		delete(p.body, "face_limit")
		p.body["variations"] = list
		p.settings = append(p.settings, [2]string{"Variants", fmt.Sprint(variants)})
	}
	if smart {
		limit := 25000
		if version == nexusP1 {
			limit = 20000
		}
		for _, b := range budgets {
			if b < 500 || b > limit {
				return fail("%s face_limit must be 500 to %s", title, thousands(limit))
			}
		}
		if version == nexusP1 && (g.Quad || variants > 1) {
			return fail("quad topology and variants need Smart Mesh P2")
		}
		if g.Texture || g.TextureQuality != "" || g.PBR || g.Delight != nil || g.Alignment != "" || g.GeometryQuality != "" || g.SmartPoly {
			return fail("Smart Mesh makes an untextured mesh: drop texture, texture_quality, pbr, delight, texture_alignment, geometry_quality and smart_poly, then texture the project with tripo_texture")
		}
		p.keys = []string{"GenerateBaseNexus"}
		if version == nexusP2 {
			p.keys = []string{"GenerateBaseNexusV2"}
		}
		if g.Symmetry != nil {
			if version != nexusP2 {
				return fail("symmetry is a Smart Mesh P2 option")
			}
			p.body["symmetry"] = *g.Symmetry
			p.settings = append(p.settings, [2]string{"Symmetry", fmt.Sprint(*g.Symmetry)})
		}
		return p, nil
	}
	if g.Symmetry != nil {
		return fail("symmetry is a Smart Mesh P2 option")
	}
	gq := g.GeometryQuality
	if version == highDetailH31 {
		if gq == "" {
			gq = "standard"
		}
		if gq != "standard" && gq != "detailed" {
			return fail("geometry_quality must be standard or detailed")
		}
		p.body["geometry_quality"] = gq
		p.settings = append(p.settings, [2]string{"Geometry", gq})
	} else if gq != "" {
		return fail("geometry_quality is an H3.1 option")
	}
	if g.SmartPoly && gq == "detailed" {
		return fail("smart_poly cannot be combined with detailed geometry_quality")
	}
	limit := 1000000
	switch {
	case g.SmartPoly && g.Quad:
		limit = 10000
	case g.SmartPoly:
		limit = 20000
	case g.Quad:
		limit = 50000
	case gq == "detailed":
		limit = 2000000
	}
	if g.FaceLimit < 500 || g.FaceLimit > limit {
		return fail("face_limit must be 500 to %s for these options", thousands(limit))
	}
	p.body["generate_parts"] = false
	p.body["smart_poly"] = g.SmartPoly
	p.body["texture"] = g.Texture
	if mode == "image" || mode == "batch" {
		p.body["enable_image_autofix"] = false
	}
	p.keys = []string{"GenerateBase"}
	if g.Texture {
		p.keys[0] = "GenerateWithTexture"
	}
	if g.Quad {
		p.keys = append(p.keys, "GenerateQuad")
	}
	if g.SmartPoly {
		p.keys = append(p.keys, "GenerateSmartPoly")
		p.settings = append(p.settings, [2]string{"Smart Poly", "on"})
	}
	if gq == "detailed" {
		p.keys = append(p.keys, "GenerateGeometryQualityDetailed")
	}
	if !g.Texture {
		if g.TextureQuality != "" || g.PBR || g.Delight != nil || g.Alignment != "" {
			return fail("texture_quality, pbr, delight and texture_alignment need texture=true")
		}
		return p, nil
	}
	wire, key, err := textureQuality(g.TextureQuality)
	if err != nil {
		return fail("%v", err)
	}
	align, err := alignment(g.Alignment)
	if err != nil {
		return fail("%v", err)
	}
	delight := g.Delight == nil || *g.Delight
	p.body["pbr"], p.body["delight"], p.body["texture_alignment"], p.body["texture_quality"] = g.PBR, delight, align, wire
	if key != "" {
		p.keys = append(p.keys, "Generate"+key)
	}
	if g.PBR {
		p.keys = append(p.keys, "PBR")
	}
	p.settings = append(p.settings, [2]string{"Texture", qualityLabel(wire)}, [2]string{"PBR", fmt.Sprint(g.PBR)}, [2]string{"Delight", fmt.Sprint(delight)})
	return p, nil
}

// modelAudit is the content verdict for a Studio image output used as a model
// input: sensitive needs allow_sensitive, nsfw and reject never pass.
func modelAudit(o imageOutput, allowSensitive bool) (string, error) {
	switch o.Audit {
	case "nsfw", "reject":
		return "", fmt.Errorf("the selected Studio image is marked %s and cannot be used", o.Audit)
	case "sensitive":
		if !allowSensitive {
			return "", errors.New("the selected Studio image is marked sensitive; pass allow_sensitive=true (visibility private) only after looking at it")
		}
		return "sensitive", nil
	}
	return "pass", nil
}

func generateModel(ctx context.Context, a *app, g genArgs) (reply, error) {
	mode, paths, err := g.resolve()
	if err != nil {
		return reply{}, err
	}
	wireMode := map[string]string{"studio_image": "image", "studio_multiview": "multiview"}[mode]
	if wireMode == "" {
		wireMode = mode
	}
	p, err := g.planFor(wireMode)
	if err != nil {
		return reply{}, err
	}
	var imgs []*localImage
	var in []input
	var desc string
	requests := 1
	switch mode {
	case "text":
		prompt := cleanPrompt(g.Prompt)
		if n := utf8.RuneCountInString(prompt); n < 1 || n > 1000 {
			return reply{}, errors.New("prompt must have 1 to 1000 characters")
		}
		p.body["gen_image_model_version"], p.body["prompt"], p.body["sketch_to_render"], p.body["t_pose"] = textImageModel, prompt, false, g.TPose
		desc = fmt.Sprintf("prompt %q", clip(prompt, 80))
		if g.TPose {
			desc += " (T-pose)"
			p.settings = append(p.settings, [2]string{"T-pose", "on"})
		}
	case "image", "multiview":
		if imgs, err = readImages(paths); err != nil {
			return reply{}, err
		}
		in = inputs(imgs, mode == "multiview")
		desc = describeInputs(in)
	case "batch":
		if imgs, err = readImages(paths); err != nil {
			return reply{}, err
		}
		for i, img := range imgs {
			in = append(in, input{fmt.Sprintf("image %d", i+1), img})
		}
		requests = len(imgs)
		desc = fmt.Sprintf("%d images, one model each", len(imgs))
		p.settings = append(p.settings, [2]string{"Images", fmt.Sprint(len(imgs))})
	}
	// Studio inputs already live in Studio storage; read them now (free), so a dry run checks them.
	var studio []any
	var first imageOutput
	switch mode {
	case "studio_image":
		s, err := a.imageAsset(ctx, g.StudioImageID)
		if err != nil {
			return reply{}, err
		}
		idx := 0
		if g.OutputIndex != nil {
			idx = *g.OutputIndex
		}
		o, err := s.source(idx, "generate_image")
		if err != nil {
			return reply{}, err
		}
		audit, err := modelAudit(o, g.AllowSensitive)
		if err != nil {
			return reply{}, err
		}
		studio, first = []any{imageWire(o, audit)}, o
		desc = fmt.Sprintf("output %d of image asset %s", idx, s.ID)
	case "studio_multiview":
		s, err := a.imageAsset(ctx, g.StudioViewsID)
		if err != nil {
			return reply{}, err
		}
		for i := range viewNames {
			o, err := s.source(i, "multiview_images")
			if err != nil {
				return reply{}, err
			}
			audit, err := modelAudit(o, g.AllowSensitive)
			if err != nil {
				return reply{}, err
			}
			studio = append(studio, imageWire(o, audit))
			if i == 0 {
				first = o
			}
		}
		desc = "the four views of image asset " + s.ID
	}
	var parts []string
	for _, s := range p.settings {
		parts = append(parts, strings.ToLower(s[0])+" "+s[1])
	}
	j := job{
		Tool: "tripo_generate_model", Title: p.title, Keys: p.keys, Requests: requests, Inputs: in, Follow: g.followArgs,
		Settings: p.settings,
		Summary:  fmt.Sprintf("%s (%s), from %s", p.title, strings.Join(parts, ", "), desc),
	}
	// Several models (batch, variants) are followed here: paid() follows one receipt.
	multi := mode == "batch" || p.variants > 1
	dest, wait := j.Follow.DownloadTo, seconds(j.Follow.WaitSeconds, 900)
	if multi {
		j.Follow.DownloadTo = ""
	}
	var all []receipt
	r, err := a.paid(ctx, j, func() (receipt, error) {
		refs, err := a.uploadAll(ctx, imgs)
		if err != nil {
			return receipt{}, err
		}
		endpoint := "/v2/studio/operation/image_to_model"
		switch mode {
		case "text":
			endpoint = "/v2/studio/operation/text_to_model"
		case "image":
			p.body["image"] = refs[0]
		case "multiview":
			endpoint, p.body["image"] = "/v2/studio/operation/multiview_to_model", refs
		case "batch":
			list := make([]any, len(refs))
			for i, ref := range refs {
				rf := ref.(imageRef)
				m := map[string]any{"bucket": rf.Bucket, "image_audit_result": rf.Audit, "image_source": rf.Source, "key": rf.Key}
				if g.Texture {
					m["texture_quality"] = p.body["texture_quality"]
				}
				list[i] = m
			}
			endpoint, p.body["image"] = "/v2/studio/operation/batch_image_to_model", list
		case "studio_image":
			p.body["image"] = studio[0]
		case "studio_multiview":
			endpoint, p.body["image"] = "/v2/studio/operation/multiview_to_model", studio
		}
		if p.body["model_version"] == nexusP2 && g.Symmetry == nil && mode != "text" {
			// The website asks Studio whether the (first) image is symmetric.
			for _, ref := range refs {
				if rf, ok := ref.(imageRef); ok {
					first = imageOutput{Bucket: rf.Bucket, Key: rf.Key}
					break
				}
			}
			var sym struct {
				Symmetry *bool `json:"symmetry"`
			}
			err := a.call(ctx, "POST", "/v2/studio/operation/symmetry_check", map[string]any{"image": map[string]string{"bucket": first.Bucket, "key": first.Key}}, &sym)
			if err == nil && sym.Symmetry == nil {
				err = errors.New("no answer")
			}
			if err != nil {
				return receipt{}, fmt.Errorf("not submitted, nothing charged: Studio's symmetry check failed (%v); pass symmetry explicitly", err)
			}
			p.body["symmetry"] = *sym.Symmetry
		}
		all, err = a.submitAll(ctx, endpoint, p.body)
		if err != nil {
			return receipt{}, err
		}
		return all[0], nil
	})
	if err != nil || len(all) == 0 || !multi {
		return r, err
	}
	return r, a.followAll(ctx, &r, j, all, mode == "batch", len(imgs), dest, wait)
}

// submitAll is submit() for requests that make several models: it returns
// every receipt. Same rule: only a refusal proves nothing was charged.
func (a *app) submitAll(ctx context.Context, path string, body any) ([]receipt, error) {
	var raw json.RawMessage
	err := a.call(ctx, "POST", path, body, &raw)
	var refused *apiError
	if errors.As(err, &refused) || errors.Is(err, errAuth) || errors.Is(err, errNoSession) {
		return nil, fmt.Errorf("not submitted, nothing charged: %w", err)
	}
	if err != nil {
		return nil, fmt.Errorf("outcome unknown: %v. The job may have been accepted and charged; check tripo_list_projects before submitting again", err)
	}
	list, err := parseReceipts(raw)
	if err != nil {
		return nil, fmt.Errorf("outcome unknown: Tripo accepted %s but %v; check tripo_list_projects before submitting again", path, err)
	}
	return list, nil
}

// parseReceipts reads a submit answer: one receipt or a list of them.
func parseReceipts(raw json.RawMessage) ([]receipt, error) {
	var list []receipt
	if json.Unmarshal(raw, &list) != nil {
		var one receipt
		if json.Unmarshal(raw, &one) != nil {
			return nil, errors.New("returned an unreadable answer")
		}
		list = []receipt{one}
	}
	if len(list) == 0 {
		return nil, errors.New("returned no operator_id")
	}
	for _, rc := range list {
		if rc.OperatorID == "" {
			return nil, errors.New("returned a receipt without operator_id")
		}
	}
	return list, nil
}

// followAll reports every receipt of a multi-model call and, with dest,
// waits for each in turn and saves it into its own numbered subfolder.
func (a *app) followAll(ctx context.Context, r *reply, j job, all []receipt, batch bool, inputs int, dest string, wait time.Duration) error {
	n := len(all)
	r.line("%d models submitted (every operator_id and project_id; the cost above is for all of them):", n)
	for i, rc := range all {
		r.line("%d. project_id: %s  operator_id: %s", i+1, rc.ProjectID, rc.OperatorID)
		r.Card.Items = append(r.Card.Items, &projectCard{ID: rc.ProjectID, Name: fmt.Sprintf("model %d", i+1), OperatorID: rc.OperatorID, URL: studioURL(rc.ProjectID)})
		if i > 0 {
			a.logJob(jobRecord{Tool: j.Tool, Title: j.Title, Summary: j.Summary, ProjectID: rc.ProjectID, OperatorID: rc.OperatorID, Status: "submitted"})
		}
	}
	if batch && n != inputs {
		r.line("Warning: Studio returned %d receipts for %d images.", n, inputs)
	}
	if dest == "" {
		r.line("Next: tripo_wait for each operator_id with its project_id (download_to saves the model).")
		return nil
	}
	deadline := time.Now().Add(wait)
	var errs []error
	saved, failed := 0, false
	for i, rc := range all {
		sub := reply{Card: &card{Kind: "job", Title: j.Title}}
		sub.line("Model %d of %d:", i+1, n)
		dir := filepath.Join(dest, fmt.Sprintf("model-%02d", i+1)) + string(os.PathSeparator)
		err := a.finish(ctx, &sub, rc.OperatorID, rc.ProjectID, max(time.Until(deadline), 0), dir)
		r.line("%s", sub.Text)
		r.Card.Saved = append(r.Card.Saved, sub.Card.Saved...)
		switch {
		case err != nil:
			failed = true
			errs = append(errs, fmt.Errorf("model %d: %w", i+1, err))
		case sub.Card.Status == "saved":
			saved++
		}
		if m, ok := sub.Meta["model"]; ok && r.Meta["model"] == nil {
			r.meta("model", m)
		}
	}
	switch {
	case saved == n:
		r.Card.Status = "saved"
	case failed:
		r.Card.Status = "failed"
	default:
		r.Card.Status = "running"
	}
	return errors.Join(errs...)
}
