package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// Part workflow: segment (ops_mesh.go) -> list parts -> complete parts ->
// per-part texture / remesh. Part names are the raw node names of the model's
// GLB, exactly as Studio's own post-processes expect them.

const (
	completionModelVersion = "v1.0-20250506"
	maxParts               = 200
)

func init() {
	register(
		tool{
			Name:        "tripo_list_parts",
			Description: "List a project's parts: the mesh names of its current model with the face count of each (read from the GLB's accessor metadata, so meshopt-compressed models work; triangles). Use these exact names as part_names in tripo_complete_parts, tripo_texture and tripo_remesh. A project that is not segmented has one part: tripo_segment (40 credits) splits it. Free.",
			InputSchema: object([]string{"project_id"}, props{
				"project_id":  str("Project id."),
				"operator_id": str("A specific version; default the current one."),
			}),
			UI:  true,
			run: handler(listParts),
		},
		tool{
			Name:        "tripo_complete_parts",
			Description: "PAID: fill in the hidden or open sides of selected parts of a segmented project, so each part is a closed solid. mode ai_completion (default) regenerates the missing regions; quick_cap closes open boundaries. part_names are required (exact names from tripo_list_parts). The project must be segmented / multi-mesh. Priced by Studio's table: QuickCap 0 and AICompletion 0 credits at the last check, so confirm with dry_run. Submits at once.",
			InputSchema: object([]string{"project_id", "part_names"}, followProps, props{
				"project_id": str("Segmented project."),
				"mode":       enum("Default ai_completion.", "ai_completion", "quick_cap"),
				"part_names": strList("Parts to complete: raw mesh names from tripo_list_parts (1 to 200)."),
			}),
			UI:  true,
			run: handler(completeParts),
		},
	)
}

// ---- part names ----

// validatePartNames checks names as the original plugin does: 1 to 200 raw
// hierarchy names, each trimmed, 1 to 256 characters, no control characters,
// no duplicates.
func validatePartNames(names []string, field string) ([]string, error) {
	if len(names) < 1 || len(names) > maxParts {
		return nil, fmt.Errorf("%s must contain 1 through %d raw hierarchy names", field, maxParts)
	}
	out := make([]string, 0, len(names))
	seen := map[string]bool{}
	for _, n := range names {
		n = strings.TrimSpace(n)
		if k := utf8.RuneCountInString(n); k < 1 || k > 256 || strings.ContainsFunc(n, func(r rune) bool { return r < 32 || r == 127 }) {
			return nil, fmt.Errorf("%s contains an empty, control-character, or overlong name", field)
		}
		if seen[n] {
			return nil, fmt.Errorf("%s must not contain duplicate names", field)
		}
		seen[n] = true
		out = append(out, n)
	}
	return out, nil
}

// optionalPartNames is validatePartNames where an empty list means the whole model.
func optionalPartNames(names []string, field string) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	return validatePartNames(names, field)
}

// shortList renders up to five names for a card line.
func shortList(names []string) string {
	if len(names) <= 5 {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(names[:5], ", "), len(names)-5)
}

// ---- capability rules (src/ops/capabilities.mjs) ----

func flagIs(p *project, name string, want bool) bool {
	f := p.flag(name)
	return f != nil && *f == want
}

// assertSupports ports the plugin's assertProjectSupports for the post-process
// kinds in this server. An unknown flag never blocks; only a known "no" does.
func assertSupports(kind string, p *project, smartPoly bool) error {
	switch kind {
	case "segmentation":
		if flagIs(p, "is_quad", true) {
			return errors.New("Studio cannot segment a quad project")
		}
		if flagIs(p, "is_rigged", true) {
			return errors.New("Studio cannot segment a rigged project")
		}
	case "remesh":
		if flagIs(p, "is_rigged", true) {
			return errors.New("Studio cannot remesh a rigged project")
		}
		if smartPoly && flagIs(p, "is_nexus_mesh", true) {
			return errors.New("Smart Poly retopology is not available for Smart Mesh projects; use smart_poly=false")
		}
	case "mesh_fill", "ai_completion":
		if flagIs(p, "is_multiple_mesh", false) {
			return errors.New("part completion needs a segmented / multi-mesh project: run tripo_segment first (40 credits)")
		}
	case "retexture":
		if flagIs(p, "is_textured", false) {
			return errors.New("texture editing needs an already textured project: run tripo_texture first")
		}
		if flagIs(p, "is_ultra_textured", true) {
			return errors.New("Studio's Magic Brush does not support an 8K (ultra) textured version")
		}
		if flagIs(p, "is_segmented", true) {
			return errors.New("Studio's Magic Brush does not support a segmented version")
		}
	case "texture_upscale", "pbr":
		if flagIs(p, "is_textured", false) {
			return errors.New("this needs an already textured project: run tripo_texture first")
		}
	}
	return nil
}

// ---- reading the GLB ----

var errFBX = errors.New("the model is an FBX file (a quad Smart Mesh), whose part names cannot be read here; pass part_names yourself")

type gltfDoc struct {
	Nodes []struct {
		Name string `json:"name"`
		Mesh *int   `json:"mesh"`
	} `json:"nodes"`
	Meshes []struct {
		Primitives []struct {
			Attributes map[string]int `json:"attributes"`
			Indices    *int           `json:"indices"`
			Mode       *int           `json:"mode"`
		} `json:"primitives"`
	} `json:"meshes"`
	Accessors []struct {
		Count int `json:"count"`
	} `json:"accessors"`
	Images []struct {
		Name       string `json:"name"`
		URI        string `json:"uri"`
		BufferView *int   `json:"bufferView"`
	} `json:"images"`
	BufferViews []struct {
		Buffer     int `json:"buffer"`
		ByteOffset int `json:"byteOffset"`
		ByteLength int `json:"byteLength"`
	} `json:"bufferViews"`
}

// readGLB reads a GLB's JSON chunk and, with withBin, its binary chunk. Without
// it the rest of the stream is left unread, so listing parts of a big model
// downloads only its first kilobytes.
func readGLB(r io.Reader, withBin bool) (*gltfDoc, []byte, error) {
	var h [20]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return nil, nil, errors.New("the model is not a GLB file")
	}
	if bytes.HasPrefix(h[:], []byte("Kaydara FBX Binary")) {
		return nil, nil, errFBX
	}
	le := binary.LittleEndian
	if le.Uint32(h[0:]) != 0x46546C67 || le.Uint32(h[4:]) != 2 || le.Uint32(h[16:]) != 0x4E4F534A {
		return nil, nil, errors.New("the model is not a GLB v2 file with a JSON chunk")
	}
	n := le.Uint32(h[12:])
	if n > 64<<20 {
		return nil, nil, errors.New("the GLB's JSON chunk is unreasonably large")
	}
	js := make([]byte, n)
	if _, err := io.ReadFull(r, js); err != nil {
		return nil, nil, errors.New("the GLB is truncated in its JSON chunk")
	}
	var doc gltfDoc
	if err := json.Unmarshal(bytes.TrimRight(js, " \x00"), &doc); err != nil {
		return nil, nil, fmt.Errorf("the GLB JSON is invalid: %w", err)
	}
	if !withBin {
		return &doc, nil, nil
	}
	var ch [8]byte
	if _, err := io.ReadFull(r, ch[:]); err != nil || le.Uint32(ch[4:]) != 0x004E4942 {
		return &doc, nil, nil // no binary chunk: images may still be data URIs
	}
	size := le.Uint32(ch[0:])
	if size > 1<<30 {
		return nil, nil, errors.New("the GLB's binary chunk is too large")
	}
	bin := make([]byte, size)
	if _, err := io.ReadFull(r, bin); err != nil {
		return nil, nil, errors.New("the GLB is truncated in its binary chunk")
	}
	return &doc, bin, nil
}

type part struct {
	Name  string
	Faces int
}

// faces counts triangles from accessor metadata only (the data may be meshopt
// compressed and quantized; counts are unaffected).
func (d *gltfDoc) faces(mesh int) int {
	if mesh < 0 || mesh >= len(d.Meshes) {
		return 0
	}
	total := 0
	for _, p := range d.Meshes[mesh].Primitives {
		idx := -1
		if p.Indices != nil {
			idx = *p.Indices
		} else if pos, ok := p.Attributes["POSITION"]; ok {
			idx = pos
		}
		if idx < 0 || idx >= len(d.Accessors) || d.Accessors[idx].Count < 0 {
			continue
		}
		count, mode := d.Accessors[idx].Count, 4
		if p.Mode != nil {
			mode = *p.Mode
		}
		switch mode {
		case 4:
			total += count / 3
		case 5, 6:
			total += max(0, count-2)
		}
	}
	return total
}

// parts lists the named mesh nodes, the parts Studio's post-processes target.
func (d *gltfDoc) parts() (named []part, unnamed int) {
	for _, n := range d.Nodes {
		if n.Mesh == nil {
			continue
		}
		name := strings.TrimSpace(n.Name)
		if name == "" {
			unnamed++
			continue
		}
		named = append(named, part{name, d.faces(*n.Mesh)})
	}
	return named, unnamed
}

// nodeNames is every named node, the "raw hierarchy names" the plugin accepts.
func (d *gltfDoc) nodeNames() map[string]bool {
	set := map[string]bool{}
	for _, n := range d.Nodes {
		if name := strings.TrimSpace(n.Name); name != "" {
			set[name] = true
		}
	}
	return set
}

// modelDoc reads the JSON of the project's current model (header only).
func (a *app) modelDoc(ctx context.Context, p *project) (*gltfDoc, error) {
	if p.ModelURL == "" {
		return nil, errors.New("the project has no model file yet")
	}
	body, err := a.open(ctx, p.ModelURL)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	d, _, err := readGLB(body, false)
	return d, err
}

// checkParts stops a paid call whose part_names are not in the model. A model
// whose names cannot be read (FBX) is not blocked (the returned doc is nil).
func (a *app) checkParts(ctx context.Context, p *project, names []string) (*gltfDoc, error) {
	if len(names) == 0 {
		return nil, nil
	}
	d, err := a.modelDoc(ctx, p)
	if errors.Is(err, errFBX) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("not submitted, nothing charged: cannot read the model's part names to check part_names: %w", err)
	}
	known := d.nodeNames()
	var bad []string
	for _, n := range names {
		if !known[n] {
			bad = append(bad, fmt.Sprintf("%q", n))
		}
	}
	if len(bad) == 0 {
		return d, nil
	}
	have, _ := d.parts()
	list := make([]string, 0, len(have))
	for _, h := range have {
		list = append(list, h.Name)
	}
	if len(list) > 30 {
		list = append(list[:30], "...")
	}
	return nil, fmt.Errorf("unknown part name(s) %s; this model's parts are: %s (tripo_list_parts shows them all)", strings.Join(bad, ", "), strings.Join(list, ", "))
}

// ---- tripo_list_parts ----

type listPartsArgs struct {
	ProjectID  string `json:"project_id"`
	OperatorID string `json:"operator_id"`
}

func listParts(ctx context.Context, a *app, x listPartsArgs) (reply, error) {
	p, err := a.project(ctx, x.ProjectID, x.OperatorID)
	if err != nil {
		return reply{}, err
	}
	r := reply{Card: &card{Kind: "project", Title: p.Name, Status: p.Status}}
	a.showProject(ctx, &r, p)
	d, err := a.modelDoc(ctx, p)
	if err != nil {
		return r, err
	}
	parts, unnamed := d.parts()
	faces := 0
	for _, pt := range parts {
		faces += pt.Faces
	}
	segmented := "unknown"
	if f := p.flag("is_segmented"); f != nil {
		segmented = map[bool]string{true: "yes", false: "no"}[*f]
	}
	r.Card.Summary = fmt.Sprintf("%d parts · %s faces", len(parts), thousands(faces))
	r.line("Project %s %q: %d named parts, %s faces in total (triangles); segmented: %s.", p.ID, p.Name, len(parts), thousands(faces), segmented)
	for i, pt := range parts {
		if i == maxParts {
			r.line("... and %d more", len(parts)-i)
			break
		}
		r.line("- %s: %s faces", pt.Name, thousands(pt.Faces))
	}
	if unnamed > 0 {
		r.line("%d mesh nodes have no name and cannot be targeted.", unnamed)
	}
	if segmented == "no" || len(parts) <= 1 {
		r.line("Not split into parts: tripo_segment project_id=%s (40 credits) segments it.", p.ID)
	} else {
		r.line("Use the names above exactly as part_names: tripo_complete_parts (fill open parts), tripo_texture or tripo_remesh (per part).")
		r.Card.Next = []cardAction{
			{"Complete parts", fmt.Sprintf("Dry-run tripo_complete_parts on Tripo project %s for the parts that look open, and show me the cost.", p.ID)},
			{"Texture one part", fmt.Sprintf("Texture one part of Tripo project %s with tripo_texture part_names.", p.ID)},
		}
	}
	return r, nil
}

// ---- tripo_complete_parts ----

type completeArgs struct {
	ProjectID string   `json:"project_id"`
	Mode      string   `json:"mode"`
	PartNames []string `json:"part_names"`
	followArgs
}

func completeParts(ctx context.Context, a *app, x completeArgs) (reply, error) {
	kind, endpoint, version, key, title := "ai_completion", "ai_completion", completionModelVersion, "AICompletion", "AI completion"
	switch x.Mode {
	case "", "ai_completion":
		x.Mode = "ai_completion"
	case "quick_cap":
		kind, endpoint, version, key, title = "mesh_fill", "mesh_fill", "default", "QuickCap", "QuickCap"
	default:
		return reply{}, errors.New("mode must be ai_completion or quick_cap")
	}
	names, err := validatePartNames(x.PartNames, "part_names")
	if err != nil {
		return reply{}, err
	}
	p, err := a.preflight(ctx, x.ProjectID)
	if err != nil {
		return reply{}, err
	}
	if err := assertSupports(kind, p, false); err != nil {
		return reply{}, err
	}
	d, err := a.checkParts(ctx, p, names)
	if err != nil {
		return reply{}, err
	}
	// Studio does not always say whether a project is multi-mesh: a model with
	// a single part is not segmented, whatever the flags leave open.
	if d != nil && !flagIs(p, "is_multiple_mesh", true) {
		if have, _ := d.parts(); len(have) < 2 {
			return reply{}, errors.New("part completion needs a segmented / multi-mesh project; this model is a single part: run tripo_segment first (40 credits)")
		}
	}
	j := job{
		Tool: "tripo_complete_parts", Title: title, Keys: []string{key}, ProjectID: x.ProjectID, Follow: x.followArgs,
		Settings: [][2]string{{"Mode", x.Mode}, {"Parts", shortList(names)}},
		Summary:  fmt.Sprintf("%s of %d part(s) (%s) on %s", title, len(names), shortList(names), x.ProjectID),
	}
	return a.paid(ctx, j, func() (receipt, error) {
		return a.submit(ctx, "/v2/studio/operation/"+endpoint, map[string]any{
			"model_version": version, "part_names": names, "project_id": x.ProjectID,
		})
	})
}

// given reports whether any image argument is set.
func (v viewArgs) given() bool {
	return v.ImagePath != "" || v.ViewsDir != "" || v.Front != "" || v.Left != "" || v.Back != "" || v.Right != ""
}
