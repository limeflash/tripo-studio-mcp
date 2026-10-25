package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
)

func init() {
	register(
		tool{
			Name:        "tripo_texture",
			Description: "PAID: texture an existing project, keeping its geometry. Images as in tripo_generate_model (image_path, or multiview). quality standard = 2K, 10 credits (default); detailed = 4K, 20; ultra = 8K, 30. Submits at once.",
			InputSchema: object([]string{"project_id"}, viewProps, followProps, props{
				"project_id": str("Project to texture."),
				"quality":    enum("Default standard.", "standard", "detailed", "ultra"),
				"delight":    boolean("Remove baked lighting from the result. Default true."),
				"alignment":  enum("Default original_image.", "original_image", "geometry"),
				"part_names": strList("Texture only these parts (raw mesh names); default the whole model."),
			}),
			UI:  true,
			run: handler(texture),
		},
		tool{
			Name:        "tripo_remesh",
			Description: "PAID: retopologize an existing project to face_limit. smart_poly=true is the website's Retopology: 35 credits for triangles (up to 20,000), 40 for quads (up to 10,000); not available for Smart Mesh projects. smart_poly=false is a plain remesh: 5 credits triangles (up to 150,000), 10 quads (up to 50,000). Submits at once.",
			InputSchema: object([]string{"project_id", "face_limit"}, followProps, props{
				"project_id": str("Project to remesh."),
				"face_limit": integer("Target face count (500 or more)."),
				"quad":       boolean("Quad topology. Default false."),
				"smart_poly": boolean("Smart Poly retopology. Default false."),
				"part_names": strList("Raw mesh names to remesh; default every mesh in the model."),
			}),
			UI:  true,
			run: handler(remesh),
		},
		tool{
			Name:        "tripo_segment",
			Description: "PAID, 40 credits: split a project into semantic parts. Not for quad or rigged models. Submits at once.",
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
	viewArgs
	Quality   string   `json:"quality"`
	Delight   *bool    `json:"delight"`
	Alignment string   `json:"alignment"`
	PartNames []string `json:"part_names"`
	followArgs
}

func texture(ctx context.Context, a *app, x textureArgs) (reply, error) {
	paths, multiview, err := x.paths()
	if err != nil {
		return reply{}, err
	}
	wire, key, err := textureQuality(x.Quality)
	if err != nil {
		return reply{}, err
	}
	align, err := alignment(x.Alignment)
	if err != nil {
		return reply{}, err
	}
	imgs, err := readImages(paths)
	if err != nil {
		return reply{}, err
	}
	if _, err := a.preflight(ctx, x.ProjectID); err != nil {
		return reply{}, err
	}
	delight := x.Delight == nil || *x.Delight
	keys := []string{"TextureGeneration"}
	if key != "" {
		keys = append(keys, key)
	}
	body := map[string]any{"model_version": textureModelVersion, "delight": delight, "project_id": x.ProjectID, "texture_alignment": align, "texture_quality": wire}
	scope := "whole model"
	if len(x.PartNames) > 0 {
		body["part_names"] = x.PartNames
		scope = strings.Join(x.PartNames, ", ")
	}
	in := inputs(imgs, multiview)
	j := job{
		Tool: "tripo_texture", Title: "Texture " + qualityLabel(wire), Keys: keys, Inputs: in, ProjectID: x.ProjectID, Follow: x.followArgs,
		Settings: [][2]string{{"Quality", qualityLabel(wire)}, {"Delight", fmt.Sprint(delight)}, {"Alignment", align}, {"Parts", scope}},
		Summary:  fmt.Sprintf("texture %s (%s, delight %t) on %s, from %s", qualityLabel(wire), scope, delight, x.ProjectID, describeInputs(in)),
	}
	return a.paid(ctx, j, func() (receipt, error) {
		refs, err := a.uploadAll(ctx, imgs)
		if err != nil {
			return receipt{}, err
		}
		if multiview {
			body["images"] = refs
		} else {
			body["image"] = refs[0]
		}
		return a.submit(ctx, "/v2/studio/operation/texture_model", body)
	})
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
	if p.is("is_rigged") {
		return reply{}, errors.New("Studio cannot remesh a rigged project")
	}
	if x.SmartPoly && p.is("is_nexus_mesh") {
		return reply{}, errors.New("Smart Poly retopology is not available for Smart Mesh projects; use smart_poly=false")
	}
	keys := []string{"Retopology_Triangle"}
	if x.Quad {
		keys[0] = "Retopology_Quad"
	}
	if x.SmartPoly {
		keys = append(keys, "Retopology_SmartPoly")
	}
	scope := "every mesh"
	if len(x.PartNames) > 0 {
		scope = strings.Join(x.PartNames, ", ")
	}
	j := job{
		Tool: "tripo_remesh", Title: mode, Keys: keys, ProjectID: x.ProjectID, Follow: x.followArgs,
		Settings: [][2]string{{"Faces", thousands(x.FaceLimit)}, {"Topology", topo}, {"Parts", scope}},
		Summary:  fmt.Sprintf("%s to %s %s (%s) on %s", mode, thousands(x.FaceLimit), topo, scope, x.ProjectID),
	}
	return a.paid(ctx, j, func() (receipt, error) {
		names := x.PartNames
		if len(names) == 0 {
			if names, err = a.partNames(ctx, p); err != nil {
				return receipt{}, fmt.Errorf("not submitted, nothing charged: %w", err)
			}
		}
		return a.submit(ctx, "/v2/studio/operation/remesh", map[string]any{
			"bake": true, "face_limit": x.FaceLimit, "model_version": "default", "part_name_list": names,
			"project_id": x.ProjectID, "quad": x.Quad, "smart_poly": x.SmartPoly,
		})
	})
}

// partNames reads the mesh names from the project's current model, the part
// names Studio's post-processes expect.
func (a *app) partNames(ctx context.Context, p *project) ([]string, error) {
	if p.ModelURL == "" {
		return nil, errors.New("the project has no model file to read part names from")
	}
	body, err := a.open(ctx, p.ModelURL)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	b, err := io.ReadAll(io.LimitReader(body, 1<<30))
	if err != nil {
		return nil, err
	}
	if sniff(b) == "fbx" {
		return nil, errors.New("the model is FBX (a quad Smart Mesh); pass part_names")
	}
	names, err := meshNames(b)
	if err == nil && len(names) == 0 {
		err = errors.New("the model has no named meshes; pass part_names")
	}
	return names, err
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
	if p.is("is_quad") || p.is("is_rigged") {
		return reply{}, errors.New("Studio cannot segment a quad or rigged project")
	}
	j := job{
		Tool: "tripo_segment", Title: "Segmentation", Keys: []string{"Segmentation"}, ProjectID: x.ProjectID, Follow: x.followArgs,
		Settings: [][2]string{{"Granularity", g}},
		Summary:  fmt.Sprintf("segmentation (%s) of %s", g, x.ProjectID),
	}
	return a.paid(ctx, j, func() (receipt, error) {
		return a.submit(ctx, "/v2/studio/operation/ai_segmentation", map[string]any{
			"model_version": segmentationModelVersion, "project_id": x.ProjectID, "segmentation_granularity": g,
		})
	})
}
