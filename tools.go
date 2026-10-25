package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	run         func(context.Context, *app, json.RawMessage) (string, error)
}

// handler decodes the arguments strictly, so a misspelt option is an error
// instead of a silently ignored setting on a paid call.
func handler[T any](f func(context.Context, *app, T) (string, error)) func(context.Context, *app, json.RawMessage) (string, error) {
	return func(ctx context.Context, a *app, raw json.RawMessage) (string, error) {
		var args T
		if len(raw) > 0 && string(raw) != "null" {
			d := json.NewDecoder(bytes.NewReader(raw))
			d.DisallowUnknownFields()
			if err := d.Decode(&args); err != nil {
				return "", fmt.Errorf("bad arguments: %v", err)
			}
		}
		return f(ctx, a, args)
	}
}

type props = map[string]any

func object(required []string, ps ...props) map[string]any {
	all := props{}
	for _, p := range ps {
		for k, v := range p {
			all[k] = v
		}
	}
	s := map[string]any{"type": "object", "properties": all, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func str(desc string) any     { return props{"type": "string", "description": desc} }
func integer(desc string) any { return props{"type": "integer", "description": desc} }
func boolean(desc string) any { return props{"type": "boolean", "description": desc} }
func strList(desc string) any {
	return props{"type": "array", "items": props{"type": "string"}, "description": desc}
}
func enum(desc string, values ...string) any {
	return props{"type": "string", "enum": values, "description": desc}
}

var viewProps = props{
	"image_path": str("One reference image (PNG/JPEG/WebP). Use instead of the views."),
	"views_dir":  str("Folder with the views; files are matched by name: *front*, *left*, *back*, *right* (e.g. 0_front.png)."),
	"front":      str("Front view image path (overrides views_dir)."),
	"left":       str("Left view image path."),
	"back":       str("Back view image path."),
	"right":      str("Right view image path."),
}

var followProps = props{
	"dry_run":      boolean("Check the inputs and return the cost; nothing is uploaded, submitted or charged."),
	"download_to":  str("Wait for the job, then save the model to this file or folder."),
	"wait_seconds": integer("How long download_to waits (default 900, max 3600)."),
}

var tools = []tool{
	{
		Name:        "tripo_auth_import",
		Description: "Sign in: import the Tripo Studio web session cookie (ory_kratos_session). Pass exactly one of cookie_file (a text file with the value or a whole Cookie header; keeps it out of the chat), cookie (the value), or from_plugin=true (reuse the session saved by tripo-studio-plugin). The cookie is checked against Tripo, saved to %LOCALAPPDATA%\\TripoStudioMCP\\session.json and never shown. Free.",
		InputSchema: object(nil, props{
			"cookie_file": str("Path to a text file holding the cookie."),
			"cookie":      str("The ory_kratos_session value."),
			"from_plugin": boolean("Reuse %LOCALAPPDATA%\\TripoStudioPlugin\\session.json."),
		}),
		run: handler(authImport),
	},
	{
		Name:        "tripo_auth_status",
		Description: "Show the stored Studio session (source, expiry) and check it live against Tripo. Free.",
		InputSchema: object(nil),
		run:         handler(authStatus),
	},
	{
		Name:        "tripo_balance",
		Description: "Studio credit balance, plan and free quotas. Free.",
		InputSchema: object(nil),
		run:         handler(balance),
	},
	{
		Name:        "tripo_upload_image",
		Description: "Upload one local image to Tripo storage and run Tripo's content check; returns its bucket/key. Free. Paid tools upload their own inputs, so this is only for testing an image.",
		InputSchema: object([]string{"path"}, props{"path": str("Image path (PNG/JPEG/WebP, up to 20 MB).")}),
		run:         handler(uploadImage),
	},
	{
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
		run: handler(generateModel),
	},
	{
		Name:        "tripo_texture",
		Description: "PAID: texture an existing project, keeping its geometry. Images as in tripo_generate_model (image_path, or multiview). quality standard = 2K, 10 credits (default); detailed = 4K, 20; ultra = 8K, 30. Submits at once.",
		InputSchema: object([]string{"project_id"}, viewProps, followProps, props{
			"project_id": str("Project to texture."),
			"quality":    enum("Default standard.", "standard", "detailed", "ultra"),
			"delight":    boolean("Remove baked lighting from the result. Default true."),
			"alignment":  enum("Default original_image.", "original_image", "geometry"),
			"part_names": strList("Texture only these parts (raw mesh names); default the whole model."),
		}),
		run: handler(texture),
	},
	{
		Name:        "tripo_remesh",
		Description: "PAID: retopologize an existing project to face_limit. smart_poly=true is the website's Retopology: 35 credits for triangles (up to 20,000), 40 for quads (up to 10,000); not available for Smart Mesh projects. smart_poly=false is a plain remesh: 5 credits triangles (up to 150,000), 10 quads (up to 50,000). Submits at once.",
		InputSchema: object([]string{"project_id", "face_limit"}, followProps, props{
			"project_id": str("Project to remesh."),
			"face_limit": integer("Target face count (500 or more)."),
			"quad":       boolean("Quad topology. Default false."),
			"smart_poly": boolean("Smart Poly retopology. Default false."),
			"part_names": strList("Raw mesh names to remesh; default every mesh in the model."),
		}),
		run: handler(remesh),
	},
	{
		Name:        "tripo_segment",
		Description: "PAID, 40 credits: split a project into semantic parts. Not for quad or rigged models. Submits at once.",
		InputSchema: object([]string{"project_id"}, followProps, props{
			"project_id":  str("Project to segment."),
			"granularity": enum("Default balanced.", "coarse", "balanced", "fine"),
		}),
		run: handler(segment),
	},
	{
		Name:        "tripo_wait",
		Description: "Check or wait for a job by operator_id. timeout_seconds 0 checks once; default 600. When the job is done it shows the project's state, and download_to saves the model. Free.",
		InputSchema: object([]string{"operator_id"}, props{
			"operator_id":     str("operator_id returned by a paid tool."),
			"project_id":      str("The job's project (paid tools print it); needed to show or download the result."),
			"timeout_seconds": integer("0 to 3600; default 600."),
			"download_to":     str("Save the model to this file or folder when done."),
		}),
		run: handler(waitJob),
	},
	{
		Name:        "tripo_download",
		Description: "Save a project's model to a file or folder. By default it is the file Studio stores (GLB for triangle meshes; quad Smart Mesh results are FBX and the extension is fixed to match). operator_id picks an earlier version. format glb/fbx/obj runs Studio's export of the current version (texture_size 512-8192; may come as a zip). The stored file is free; the export was free for own projects in the original plugin's tests but is not verified here.",
		InputSchema: object([]string{"project_id", "path"}, props{
			"project_id":   str("Project to save."),
			"path":         str("Output file, or a folder (then <project name>.glb)."),
			"operator_id":  str("A specific version (from a job); default the current one."),
			"format":       enum("Export instead of the stored file.", "glb", "fbx", "obj"),
			"texture_size": integer("Export texture size: 512, 1024, 2048, 4096 or 8192. Default the project's own."),
		}),
		run: handler(download),
	},
	{
		Name:        "tripo_list_projects",
		Description: "List your Studio projects, newest first, 20 per page. Free.",
		InputSchema: object(nil, props{
			"offset": integer("Skip this many projects. Default 0."),
			"filter": enum("Default all.", "all", "smart_mesh", "textured", "untextured", "rigged"),
		}),
		run: handler(listProjects),
	},
	{
		Name:        "tripo_project",
		Description: "Show a project's state: name, current version, and whether it is textured, PBR, quad, Smart Mesh, segmented, rigged. Free.",
		InputSchema: object([]string{"project_id"}, props{"project_id": str("Project id.")}),
		run:         handler(projectInfo),
	},
}

type noArgs struct{}

// ---- auth and account ----

type authImportArgs struct {
	CookieFile string `json:"cookie_file"`
	Cookie     string `json:"cookie"`
	FromPlugin bool   `json:"from_plugin"`
}

func authImport(ctx context.Context, a *app, x authImportArgs) (string, error) {
	if err := a.importSession(ctx, x.Cookie, x.CookieFile, x.FromPlugin); err != nil {
		return "", err
	}
	text, err := authStatus(ctx, a, noArgs{})
	return "Imported.\n" + text, err
}

func authStatus(ctx context.Context, a *app, _ noArgs) (string, error) {
	s, err := a.loadSession()
	if err != nil {
		return "", err
	}
	var b strings.Builder
	imported := s.ImportedAt
	if t, err := time.Parse(time.RFC3339, s.ImportedAt); err == nil {
		imported = t.Local().Format("2006-01-02 15:04")
	}
	fmt.Fprintf(&b, "Session: %s, imported %s.\n", s.Source, imported)
	if t, err := time.Parse(time.RFC3339, s.ExpiresAt); err == nil {
		fmt.Fprintf(&b, "Expires: %s (in %d days).\n", t.Local().Format("2006-01-02 15:04"), int(time.Until(t).Hours()/24))
	} else {
		b.WriteString("Expires: unknown.\n")
	}
	if err := a.callAs(ctx, s, "GET", "/v2/studio/user/profile/payment", nil, nil); err != nil {
		return b.String(), fmt.Errorf("live check failed: %w", err)
	}
	b.WriteString("Live check: Tripo accepts the session.")
	return b.String(), nil
}

var (
	privateKey = regexp.MustCompile(`(?i)id|email|phone|avatar|name|token|secret`)
	accountKey = regexp.MustCompile(`(?i)credit|balance|point|quota|reset|expire|member|plan|subscription|trial|used|total|remain|type|status`)
)

// summarize keeps only billing fields from an account response and drops
// anything that identifies the person.
func summarize(v any) any {
	switch t := v.(type) {
	case []any:
		out := []any{}
		for i, e := range t {
			if i == 20 {
				break
			}
			out = append(out, summarize(e))
		}
		return out
	case map[string]any:
		out := map[string]any{}
		for k, e := range t {
			if privateKey.MatchString(k) {
				continue
			}
			if accountKey.MatchString(k) {
				out[k] = summarize(e)
			} else if m, ok := e.(map[string]any); ok {
				if s := summarize(m).(map[string]any); len(s) > 0 {
					out[k] = s
				}
			}
		}
		return out
	}
	return v
}

func flatten(b *strings.Builder, prefix string, v any) {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			flatten(b, p, t[k])
		}
	case []any:
		for i, e := range t {
			flatten(b, fmt.Sprintf("%s[%d]", prefix, i), e)
		}
	default:
		fmt.Fprintf(b, "%s: %v\n", prefix, t)
	}
}

func balance(ctx context.Context, a *app, _ noArgs) (string, error) {
	var pay map[string]any
	if err := a.call(ctx, "GET", "/v2/studio/user/profile/payment", nil, &pay); err != nil {
		return "", err
	}
	var b strings.Builder
	wallet, _ := pay["wallet"].(map[string]any)
	member, _ := pay["member"].(map[string]any)
	if total, ok := wallet["total_credit"].(float64); ok {
		fmt.Fprintf(&b, "Credits: %s", thousands(int(total)))
		if exp, ok := wallet["expiring_credit"].(float64); ok && exp > 0 {
			fmt.Fprintf(&b, " (%s expiring)", thousands(int(exp)))
		}
		fmt.Fprintf(&b, ". Plan: %v.\n", member["type"])
	} else {
		// Tripo changed the shape: show every billing field instead.
		flatten(&b, "", summarize(pay))
	}
	var quota map[string]any
	if a.call(ctx, "POST", "/v2/studio/operation/quota", map[string]any{}, &quota) == nil {
		var free []string
		quotas(&free, "", quota["quota"])
		if len(free) > 0 {
			b.WriteString("Free uses left: " + strings.Join(free, ", ") + ".")
		}
	}
	return strings.TrimSpace(b.String()), nil
}

// quotas collects "name left/total" for every {remaining, total_count} entry.
func quotas(out *[]string, path string, v any) {
	m, ok := v.(map[string]any)
	if !ok {
		return
	}
	if left, ok := m["remaining"].(float64); ok {
		*out = append(*out, fmt.Sprintf("%s %v/%v", path, left, m["total_count"]))
		return
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		p := k
		if path != "" {
			p = path + "." + k
		}
		quotas(out, p, m[k])
	}
}

type uploadArgs struct {
	Path string `json:"path"`
}

func uploadImage(ctx context.Context, a *app, x uploadArgs) (string, error) {
	img, err := readImage(x.Path)
	if err != nil {
		return "", err
	}
	ref, err := a.upload(ctx, img)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Uploaded %s (%s, %s).\nbucket: %s\nkey: %s\ncontent check: %s", img.path, img.format, size(int64(len(img.data))), ref.Bucket, ref.Key, ref.Audit), nil
}

// ---- inputs ----

type viewArgs struct {
	ImagePath string `json:"image_path"`
	ViewsDir  string `json:"views_dir"`
	Front     string `json:"front"`
	Left      string `json:"left"`
	Back      string `json:"back"`
	Right     string `json:"right"`
}

var viewNames = []string{"front", "left", "back", "right"}

// paths returns the input images: one image, or the four multiview slots in
// Studio's order front, left, back, right ("" for a missing view).
func (v viewArgs) paths() (paths []string, multiview bool, err error) {
	views := []string{v.Front, v.Left, v.Back, v.Right}
	anyView := v.ViewsDir != "" || strings.Join(views, "") != ""
	if v.ImagePath != "" {
		if anyView {
			return nil, false, errors.New("pass image_path or the views (views_dir, front, left, back, right), not both")
		}
		return []string{v.ImagePath}, false, nil
	}
	if !anyView {
		return nil, false, errors.New("no input images: pass image_path, or views_dir and/or front, left, back, right")
	}
	if v.ViewsDir != "" {
		found, err := findViews(v.ViewsDir)
		if err != nil {
			return nil, false, err
		}
		for i := range views {
			if views[i] == "" {
				views[i] = found[i]
			}
		}
	}
	if views[0] == "" {
		return nil, false, errors.New("multiview needs a front view")
	}
	if strings.Join(views[1:], "") == "" {
		return nil, false, errors.New("multiview needs the front view and at least one of left, back, right")
	}
	return views, true, nil
}

func findViews(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("views_dir: %w", err)
	}
	found := make([]string, len(viewNames))
	for _, e := range entries {
		name := strings.ToLower(e.Name())
		ext := filepath.Ext(name)
		if e.IsDir() || (ext != ".png" && ext != ".jpg" && ext != ".jpeg" && ext != ".webp") {
			continue
		}
		for i, view := range viewNames {
			if !strings.Contains(strings.TrimSuffix(name, ext), view) {
				continue
			}
			if found[i] != "" {
				return nil, fmt.Errorf("views_dir has two %s images (%s, %s); pass %s explicitly", view, filepath.Base(found[i]), e.Name(), view)
			}
			found[i] = filepath.Join(dir, e.Name())
		}
	}
	return found, nil
}

func readImages(paths []string) ([]*localImage, error) {
	imgs := make([]*localImage, len(paths))
	seen := map[string]string{}
	for i, p := range paths {
		if p == "" {
			continue
		}
		img, err := readImage(p)
		if err != nil {
			return nil, err
		}
		h := sha256Hex(img.data)
		if prev, dup := seen[h]; dup {
			return nil, fmt.Errorf("%s and %s are the same image; every view must differ", prev, img.path)
		}
		seen[h] = img.path
		imgs[i] = img
	}
	return imgs, nil
}

func describeInputs(imgs []*localImage, multiview bool) string {
	if !multiview {
		return filepath.Base(imgs[0].path)
	}
	var parts []string
	for i, img := range imgs {
		if img != nil {
			parts = append(parts, viewNames[i]+" "+filepath.Base(img.path))
		}
	}
	return "multiview (" + strings.Join(parts, ", ") + ")"
}

// uploadAll uploads the present images; missing views stay nil (JSON null).
func (a *app) uploadAll(ctx context.Context, imgs []*localImage) ([]any, error) {
	refs := make([]any, len(imgs))
	for i, img := range imgs {
		if img == nil {
			continue
		}
		ref, err := a.upload(ctx, img)
		if err != nil {
			return nil, fmt.Errorf("not submitted, nothing charged: %w", err)
		}
		refs[i] = ref
	}
	return refs, nil
}

// ---- paid operations ----

type followArgs struct {
	DryRun      bool   `json:"dry_run"`
	DownloadTo  string `json:"download_to"`
	WaitSeconds int    `json:"wait_seconds"`
}

// paid is the shared end of every paid tool: price, dry run, submit, report,
// then optionally wait and download.
func (a *app) paid(ctx context.Context, what string, keys []string, f followArgs, projectID string, send func() (receipt, error)) (string, error) {
	t, err := a.prices()
	if err != nil {
		return "", err
	}
	credits, breakdown := t.cost(keys)
	costLine := fmt.Sprintf("Cost: %s credits (%s; prices of %s).", thousands(credits), breakdown, t.Updated)
	if credits < 0 {
		costLine = "Cost: unknown, " + breakdown + "; add it to prices.json."
	}
	if f.DryRun {
		text := what + "\n" + costLine
		if n, ok := a.credits(ctx); ok {
			text += " Balance: " + thousands(n) + "."
		}
		return text + "\nDry run: nothing uploaded, submitted or charged.", nil
	}
	r, err := send()
	if err != nil {
		return what + "\n" + costLine, err
	}
	if r.ProjectID == "" {
		r.ProjectID = projectID
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Submitted: %s\n%s", what, costLine)
	if n, ok := a.credits(ctx); ok {
		fmt.Fprintf(&b, " Credits left: %s.", thousands(n))
	}
	fmt.Fprintf(&b, "\nproject_id: %s\noperator_id: %s\n", r.ProjectID, r.OperatorID)
	if f.DownloadTo == "" {
		fmt.Fprintf(&b, "Next: tripo_wait operator_id=%s project_id=%s (add download_to to save the model).", r.OperatorID, r.ProjectID)
		return b.String(), nil
	}
	text, err := a.finish(ctx, r.OperatorID, r.ProjectID, seconds(f.WaitSeconds, 900), f.DownloadTo)
	b.WriteString(text)
	return b.String(), err
}

func seconds(n, def int) time.Duration {
	if n == 0 {
		n = def
	}
	return time.Duration(min(max(n, 0), 3600)) * time.Second
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

// settings checks the options the way Studio allows them and returns the
// request body without images, the price keys and a short description.
func (g genArgs) settings(multiview bool) (map[string]any, []string, string, error) {
	fail := func(format string, args ...any) (map[string]any, []string, string, error) {
		return nil, nil, "", fmt.Errorf(format, args...)
	}
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
	label, ok := versionLabels[version]
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
	body := map[string]any{"face_limit": g.FaceLimit, "quad": g.Quad, "visibility": vis, "model_version": version}
	var keys []string
	if smart {
		limit := 25000
		if version == nexusP1 {
			limit = 20000
		}
		if g.FaceLimit < 500 || g.FaceLimit > limit {
			return fail("%s face_limit must be 500 to %s", label, thousands(limit))
		}
		if version == nexusP1 && g.Quad {
			return fail("quad topology needs Smart Mesh P2")
		}
		if g.Texture || g.TextureQuality != "" || g.PBR || g.Delight != nil || g.Alignment != "" || g.GeometryQuality != "" || g.SmartPoly {
			return fail("Smart Mesh makes an untextured mesh: drop texture, texture_quality, pbr, delight, texture_alignment, geometry_quality and smart_poly, then texture the project with tripo_texture")
		}
		keys = []string{"GenerateBaseNexus"}
		if version == nexusP2 {
			keys = []string{"GenerateBaseNexusV2"}
		}
		if g.Symmetry != nil {
			if version != nexusP2 {
				return fail("symmetry is a Smart Mesh P2 option")
			}
			body["symmetry"] = *g.Symmetry
		}
	} else {
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
			body["geometry_quality"] = gq
			label += ", " + gq + " geometry"
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
		body["generate_parts"] = false
		body["smart_poly"] = g.SmartPoly
		body["texture"] = g.Texture
		if !multiview {
			body["enable_image_autofix"] = false
		}
		keys = []string{"GenerateBase"}
		if g.Texture {
			keys[0] = "GenerateWithTexture"
		}
		if g.Quad {
			keys = append(keys, "GenerateQuad")
		}
		if g.SmartPoly {
			keys = append(keys, "GenerateSmartPoly")
			label += ", Smart Poly"
		}
		if gq == "detailed" {
			keys = append(keys, "GenerateGeometryQualityDetailed")
		}
		if g.Texture {
			wire, key, err := textureQuality(g.TextureQuality)
			if err != nil {
				return fail("%v", err)
			}
			align, err := alignment(g.Alignment)
			if err != nil {
				return fail("%v", err)
			}
			body["pbr"], body["delight"], body["texture_alignment"], body["texture_quality"] = g.PBR, g.Delight == nil || *g.Delight, align, wire
			if key != "" {
				keys = append(keys, "Generate"+key)
			}
			if g.PBR {
				keys = append(keys, "PBR")
			}
			label += ", textured " + qualityLabel(wire)
		} else if g.TextureQuality != "" || g.PBR || g.Delight != nil || g.Alignment != "" {
			return fail("texture_quality, pbr, delight and texture_alignment need texture=true")
		}
	}
	topo := "triangles"
	if g.Quad {
		topo = "quads"
	}
	return body, keys, fmt.Sprintf("%s, %s faces (%s), %s", label, thousands(g.FaceLimit), topo, vis), nil
}

// textureQuality maps a quality to Studio's wire value and its price key.
func textureQuality(q string) (wire, key string, err error) {
	switch q {
	case "", "standard":
		return "standard", "", nil
	case "detailed":
		return "detailed", "TextureQualityDetailed", nil
	case "ultra":
		return "extreme", "TextureQualityExtreme", nil
	}
	return "", "", fmt.Errorf("texture quality must be standard (2K), detailed (4K) or ultra (8K), not %q", q)
}

func qualityLabel(wire string) string {
	return map[string]string{"standard": "2K", "detailed": "4K", "extreme": "8K"}[wire]
}

func alignment(v string) (string, error) {
	switch v {
	case "", "original_image":
		return "original_image", nil
	case "geometry":
		return "geometry", nil
	}
	return "", fmt.Errorf("alignment must be original_image or geometry, not %q", v)
}

func generateModel(ctx context.Context, a *app, g genArgs) (string, error) {
	paths, multiview, err := g.paths()
	if err != nil {
		return "", err
	}
	body, keys, desc, err := g.settings(multiview)
	if err != nil {
		return "", err
	}
	imgs, err := readImages(paths)
	if err != nil {
		return "", err
	}
	what := desc + ", from " + describeInputs(imgs, multiview)
	return a.paid(ctx, what, keys, g.followArgs, "", func() (receipt, error) {
		refs, err := a.uploadAll(ctx, imgs)
		if err != nil {
			return receipt{}, err
		}
		endpoint := "/v2/studio/operation/multiview_to_model"
		body["image"] = refs
		if !multiview {
			endpoint, body["image"] = "/v2/studio/operation/image_to_model", refs[0]
		}
		if body["model_version"] == nexusP2 && g.Symmetry == nil {
			// The website asks Studio whether the first view is symmetric.
			first := refs[0].(imageRef)
			var sym struct {
				Symmetry *bool `json:"symmetry"`
			}
			err := a.call(ctx, "POST", "/v2/studio/operation/symmetry_check", map[string]any{"image": map[string]string{"bucket": first.Bucket, "key": first.Key}}, &sym)
			if err != nil || sym.Symmetry == nil {
				return receipt{}, fmt.Errorf("not submitted, nothing charged: Studio's symmetry check failed (%v); pass symmetry explicitly", err)
			}
			body["symmetry"] = *sym.Symmetry
		}
		return a.submit(ctx, endpoint, body)
	})
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

func texture(ctx context.Context, a *app, x textureArgs) (string, error) {
	paths, multiview, err := x.paths()
	if err != nil {
		return "", err
	}
	wire, key, err := textureQuality(x.Quality)
	if err != nil {
		return "", err
	}
	align, err := alignment(x.Alignment)
	if err != nil {
		return "", err
	}
	imgs, err := readImages(paths)
	if err != nil {
		return "", err
	}
	if _, err := a.preflight(ctx, x.ProjectID); err != nil {
		return "", err
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
	what := fmt.Sprintf("texture %s (%s, delight %t) on %s, from %s", qualityLabel(wire), scope, delight, x.ProjectID, describeInputs(imgs, multiview))
	return a.paid(ctx, what, keys, x.followArgs, x.ProjectID, func() (receipt, error) {
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

func remesh(ctx context.Context, a *app, x remeshArgs) (string, error) {
	limit, mode := 150000, "plain remesh"
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
		return "", fmt.Errorf("face_limit must be 500 to %s for %s with %s", thousands(limit), mode, topo)
	}
	p, err := a.preflight(ctx, x.ProjectID)
	if err != nil {
		return "", err
	}
	if p.is("is_rigged") {
		return "", errors.New("Studio cannot remesh a rigged project")
	}
	if x.SmartPoly && p.is("is_nexus_mesh") {
		return "", errors.New("Smart Poly retopology is not available for Smart Mesh projects; use smart_poly=false")
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
	what := fmt.Sprintf("%s to %s %s (%s) on %s", mode, thousands(x.FaceLimit), topo, scope, x.ProjectID)
	return a.paid(ctx, what, keys, x.followArgs, x.ProjectID, func() (receipt, error) {
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

func segment(ctx context.Context, a *app, x segmentArgs) (string, error) {
	g := x.Granularity
	if g == "" {
		g = "balanced"
	}
	if g != "coarse" && g != "balanced" && g != "fine" {
		return "", errors.New("granularity must be coarse, balanced or fine")
	}
	p, err := a.preflight(ctx, x.ProjectID)
	if err != nil {
		return "", err
	}
	if p.is("is_quad") || p.is("is_rigged") {
		return "", errors.New("Studio cannot segment a quad or rigged project")
	}
	what := fmt.Sprintf("segmentation (%s) of %s", g, x.ProjectID)
	return a.paid(ctx, what, []string{"Segmentation"}, x.followArgs, x.ProjectID, func() (receipt, error) {
		return a.submit(ctx, "/v2/studio/operation/ai_segmentation", map[string]any{
			"model_version": segmentationModelVersion, "project_id": x.ProjectID, "segmentation_granularity": g,
		})
	})
}

// ---- jobs, projects, downloads ----

type waitArgs struct {
	OperatorID     string `json:"operator_id"`
	ProjectID      string `json:"project_id"`
	TimeoutSeconds *int   `json:"timeout_seconds"`
	DownloadTo     string `json:"download_to"`
}

func waitJob(ctx context.Context, a *app, x waitArgs) (string, error) {
	if x.OperatorID == "" {
		return "", errors.New("operator_id is required")
	}
	timeout := 600 * time.Second
	if x.TimeoutSeconds != nil {
		timeout = time.Duration(min(max(*x.TimeoutSeconds, 0), 3600)) * time.Second
	}
	return a.finish(ctx, x.OperatorID, x.ProjectID, timeout, x.DownloadTo)
}

// finish waits for a job and reports the project; with downloadTo it also
// saves the new version of the model.
func (a *app) finish(ctx context.Context, op, projectID string, timeout time.Duration, downloadTo string) (string, error) {
	it, err := a.wait(ctx, op, timeout)
	if err != nil {
		return "", err
	}
	if it.ProjectID != "" {
		projectID = it.ProjectID
	}
	switch it.Status {
	case "success":
	case "failed", "cancelled", "expired", "banned":
		reason := ""
		if it.Reason != nil {
			reason = fmt.Sprintf(": %v", it.Reason)
		}
		return "", fmt.Errorf("job %s ended as %s%s", op, it.Status, reason)
	default:
		next := fmt.Sprintf("tripo_wait operator_id=%s project_id=%s", op, projectID)
		if downloadTo != "" {
			next += " download_to=" + downloadTo
		}
		return fmt.Sprintf("Still %s (%.0f%%) after %s; nothing downloaded yet. Next: %s", it.Status, it.Progress, timeout, next), nil
	}
	if projectID == "" {
		if downloadTo != "" {
			return "", fmt.Errorf("job %s is done, but Tripo's progress does not name the project; call tripo_wait again with project_id to download", op)
		}
		return fmt.Sprintf("Done: job %s succeeded. Pass project_id to see the project.", op), nil
	}
	p, err := a.project(ctx, projectID, op)
	if err != nil {
		return "", err
	}
	text := "Done. " + describe(p)
	if downloadTo == "" {
		return text, nil
	}
	line, err := a.download(ctx, p, downloadTo, "", 0)
	return text + "\n" + line, err
}

func describe(p *project) string {
	var flags []string
	for _, f := range []struct{ key, label string }{
		{"is_textured", "textured"}, {"is_pbr", "PBR"}, {"is_quad", "quad"},
		{"is_nexus_mesh", "Smart Mesh"}, {"is_segmented", "segmented"}, {"is_rigged", "rigged"},
	} {
		v := "?"
		if b := p.flag(f.key); b != nil {
			v = map[bool]string{true: "yes", false: "no"}[*b]
		}
		flags = append(flags, f.label+" "+v)
	}
	file := "model file: none yet"
	if p.ModelURL != "" {
		file = "model file: ready"
	}
	return fmt.Sprintf("Project %s %q: %s; %s; current version %s.", p.ID, p.Name, strings.Join(flags, ", "), file, p.OperatorID)
}

type downloadArgs struct {
	ProjectID   string `json:"project_id"`
	Path        string `json:"path"`
	OperatorID  string `json:"operator_id"`
	Format      string `json:"format"`
	TextureSize int    `json:"texture_size"`
}

func download(ctx context.Context, a *app, x downloadArgs) (string, error) {
	if x.Format != "" && x.OperatorID != "" {
		return "", errors.New("format exports the current version only; drop operator_id or format")
	}
	p, err := a.project(ctx, x.ProjectID, x.OperatorID)
	if err != nil {
		return "", err
	}
	return a.download(ctx, p, x.Path, x.Format, x.TextureSize)
}

func (a *app) download(ctx context.Context, p *project, dest, format string, textureSize int) (string, error) {
	src, label, ext := p.ModelURL, "the stored model", ".glb"
	if format != "" {
		u, err := a.exportURL(ctx, p, format, textureSize)
		if err != nil {
			return "", fmt.Errorf("export failed: %w", err)
		}
		src, label, ext = u, format+" export", "."+format
	} else if src == "" {
		return "", errors.New("the project has no model file yet")
	}
	name := p.Name
	if name == "" {
		name = p.ID
	}
	path, err := outputPath(dest, name, ext)
	if err != nil {
		return "", err
	}
	final, n, kind, err := a.save(ctx, src, path)
	if err != nil {
		return "", err
	}
	note := ""
	if final != path {
		note = fmt.Sprintf(" (the file is %s, so the extension was changed)", strings.ToUpper(kind))
	}
	return fmt.Sprintf("Saved %s to %s, %s%s.", label, final, size(n), note), nil
}

type listArgs struct {
	Offset int    `json:"offset"`
	Filter string `json:"filter"`
}

func listProjects(ctx context.Context, a *app, x listArgs) (string, error) {
	filter := x.Filter
	if filter == "" {
		filter = "all"
	}
	q := url.Values{"asset_type": {"mine"}, "offset": {strconv.Itoa(x.Offset)}, "size": {"20"}, "type": {filter}}
	var page struct {
		Total    int `json:"total"`
		Projects []struct {
			ID         string `json:"id"`
			Name       string `json:"project_name"`
			Type       string `json:"type"`
			Visibility string `json:"visibility"`
			Running    any    `json:"running_operator"`
		} `json:"projects"`
	}
	if err := a.call(ctx, "GET", "/v2/studio/assets/v2?"+q.Encode(), nil, &page); err != nil {
		return "", err
	}
	if len(page.Projects) == 0 {
		return fmt.Sprintf("No projects (filter %s, offset %d, total %d).", filter, x.Offset, page.Total), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Projects %d-%d of %d (filter %s):\n", x.Offset+1, x.Offset+len(page.Projects), page.Total, filter)
	for _, p := range page.Projects {
		fmt.Fprintf(&b, "- %s  %q  %s  %s", p.ID, p.Name, p.Type, p.Visibility)
		if m, ok := p.Running.(map[string]any); (ok && len(m) > 0) || (!ok && p.Running != nil) {
			b.WriteString("  [job running]")
		}
		b.WriteString("\n")
	}
	if next := x.Offset + len(page.Projects); next < page.Total {
		fmt.Fprintf(&b, "More: offset=%d", next)
	}
	return strings.TrimSpace(b.String()), nil
}

type projectArgs struct {
	ProjectID string `json:"project_id"`
}

func projectInfo(ctx context.Context, a *app, x projectArgs) (string, error) {
	p, err := a.project(ctx, x.ProjectID, "")
	if err != nil {
		return "", err
	}
	return describe(p) + "\nStatus: " + p.Status, nil
}

func thousands(n int) string {
	s := strconv.Itoa(n)
	if n < 0 {
		return s
	}
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func size(n int64) string {
	if n < 1<<20 {
		return fmt.Sprintf("%d KB", (n+1023)/1024)
	}
	return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
}
