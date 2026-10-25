package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

func init() {
	register(tool{
		Name: "tripo_generate_model",
		Description: "PAID: generate a 3D model from one image or from multiview (front plus at least one of left/back/right). Submits at once and returns the cost, project_id and operator_id.\n" +
			"Versions: P1 = Smart Mesh P1.0 (default for tier smart_mesh; 35 credits; 500-20,000 faces; triangles only). P2 = Smart Mesh P2.0 (100 credits; up to 25,000 faces; quad allowed). H3.1 = High Detail 3.1 (default for tier high_detail; 15 base, 25 with texture; +15 detailed geometry, +10/+20 4K/8K texture, +5 PBR, +5 quad).\n" +
			"Smart Mesh returns an untextured mesh: texture it with tripo_texture on the returned project_id. Defaults are the cheapest options: triangles, private, standard quality.",
		InputSchema: object([]string{"face_limit"}, viewProps, followProps, props{
			"tier":              enum("smart_mesh or high_detail; may be left out when model_version is given.", "smart_mesh", "high_detail"),
			"model_version":     str("P1, P2, H3.1, H3.0, H2.5, or a full Studio version id."),
			"face_limit":        integer("Target face count."),
			"quad":              boolean("Quad topology (P2 and High Detail only). Default false."),
			"symmetry":          boolean("P2 only: force symmetry on/off; by default Studio's symmetry check decides."),
			"visibility":        enum("Default private.", "private", "public", "shareable"),
			"texture":           boolean("High Detail only: texture during generation. Default false."),
			"texture_quality":   enum("High Detail with texture: standard 2K (default), detailed 4K, ultra 8K.", "standard", "detailed", "ultra"),
			"pbr":               boolean("High Detail with texture: PBR maps. Default false."),
			"delight":           boolean("High Detail with texture: remove baked lighting. Default true."),
			"texture_alignment": enum("High Detail with texture. Default original_image.", "original_image", "geometry"),
			"geometry_quality":  enum("H3.1 only. Default standard.", "standard", "detailed"),
			"smart_poly":        boolean("High Detail only: low-poly Smart Poly mode. Default false."),
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
	Tier            string `json:"tier"`
	ModelVersion    string `json:"model_version"`
	FaceLimit       int    `json:"face_limit"`
	Quad            bool   `json:"quad"`
	Symmetry        *bool  `json:"symmetry"`
	Visibility      string `json:"visibility"`
	Texture         bool   `json:"texture"`
	TextureQuality  string `json:"texture_quality"`
	PBR             bool   `json:"pbr"`
	Delight         *bool  `json:"delight"`
	Alignment       string `json:"texture_alignment"`
	GeometryQuality string `json:"geometry_quality"`
	SmartPoly       bool   `json:"smart_poly"`
	followArgs
}

// genPlan is a checked generation request without its images.
type genPlan struct {
	body     map[string]any
	keys     []string
	title    string
	settings [][2]string
}

// plan checks the options the way Studio allows them.
func (g genArgs) plan(multiview bool) (genPlan, error) {
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
	if g.FaceLimit == 0 {
		return fail("face_limit is required")
	}
	topo := "triangles"
	if g.Quad {
		topo = "quads"
	}
	p := genPlan{
		body:     map[string]any{"face_limit": g.FaceLimit, "quad": g.Quad, "visibility": vis, "model_version": version},
		title:    title,
		settings: [][2]string{{"Faces", thousands(g.FaceLimit)}, {"Topology", topo}, {"Visibility", vis}},
	}
	if smart {
		limit := 25000
		if version == nexusP1 {
			limit = 20000
		}
		if g.FaceLimit < 500 || g.FaceLimit > limit {
			return fail("%s face_limit must be 500 to %s", title, thousands(limit))
		}
		if version == nexusP1 && g.Quad {
			return fail("quad topology needs Smart Mesh P2")
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
	if !multiview {
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

func generateModel(ctx context.Context, a *app, g genArgs) (reply, error) {
	paths, multiview, err := g.paths()
	if err != nil {
		return reply{}, err
	}
	p, err := g.plan(multiview)
	if err != nil {
		return reply{}, err
	}
	imgs, err := readImages(paths)
	if err != nil {
		return reply{}, err
	}
	in := inputs(imgs, multiview)
	var parts []string
	for _, s := range p.settings {
		parts = append(parts, strings.ToLower(s[0])+" "+s[1])
	}
	j := job{
		Tool: "tripo_generate_model", Title: p.title, Keys: p.keys, Inputs: in, Follow: g.followArgs,
		Settings: p.settings,
		Summary:  fmt.Sprintf("%s (%s), from %s", p.title, strings.Join(parts, ", "), describeInputs(in)),
	}
	return a.paid(ctx, j, func() (receipt, error) {
		refs, err := a.uploadAll(ctx, imgs)
		if err != nil {
			return receipt{}, err
		}
		endpoint := "/v2/studio/operation/multiview_to_model"
		p.body["image"] = refs
		if !multiview {
			endpoint, p.body["image"] = "/v2/studio/operation/image_to_model", refs[0]
		}
		if p.body["model_version"] == nexusP2 && g.Symmetry == nil {
			// The website asks Studio whether the front view is symmetric.
			first := refs[0].(imageRef)
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
		return a.submit(ctx, endpoint, p.body)
	})
}
