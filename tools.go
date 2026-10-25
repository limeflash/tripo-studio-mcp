package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// tool is one MCP tool. Each ops_*.go file registers its own tools in init().
type tool struct {
	Name        string
	Description string
	InputSchema map[string]any
	UI          bool // results render in the card widget
	run         func(context.Context, *app, json.RawMessage) (reply, error)
}

var tools []tool

func register(ts ...tool) { tools = append(tools, ts...) }

// handler decodes the arguments strictly, so a misspelt option is an error
// instead of a silently ignored setting on a paid call.
func handler[T any](f func(context.Context, *app, T) (reply, error)) func(context.Context, *app, json.RawMessage) (reply, error) {
	return func(ctx context.Context, a *app, raw json.RawMessage) (reply, error) {
		var args T
		if len(raw) > 0 && string(raw) != "null" {
			d := json.NewDecoder(bytes.NewReader(raw))
			d.DisallowUnknownFields()
			if err := d.Decode(&args); err != nil {
				return reply{}, fmt.Errorf("bad arguments: %v", err)
			}
		}
		return f(ctx, a, args)
	}
}

// ---- schema helpers ----

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
func number(desc string) any  { return props{"type": "number", "description": desc} }
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

func init() {
	register(
		tool{
			Name:        "tripo_auth_import",
			Description: "Sign in: import the Tripo Studio web session cookie (ory_kratos_session). Pass exactly one of cookie_file (a text file with the value or a whole Cookie header; keeps it out of the chat), cookie (the value), or from_plugin=true (reuse the session saved by tripo-studio-plugin). The cookie is checked against Tripo, saved to %LOCALAPPDATA%\\TripoStudioMCP\\session.json and never shown. Free.",
			InputSchema: object(nil, props{
				"cookie_file": str("Path to a text file holding the cookie."),
				"cookie":      str("The ory_kratos_session value."),
				"from_plugin": boolean("Reuse %LOCALAPPDATA%\\TripoStudioPlugin\\session.json."),
			}),
			run: handler(authImport),
		},
		tool{
			Name:        "tripo_auth_status",
			Description: "Show the stored Studio session (source, expiry) and check it live against Tripo. Free.",
			InputSchema: object(nil),
			run:         handler(authStatus),
		},
		tool{
			Name:        "tripo_auth_logout",
			Description: "Forget the stored Studio session (deletes session.json). Free.",
			InputSchema: object(nil),
			run:         handler(authLogout),
		},
		tool{
			Name:        "tripo_balance",
			Description: "Studio credit balance, plan and free quotas. Free.",
			InputSchema: object(nil),
			run:         handler(balance),
		},
		tool{
			Name:        "tripo_upload_image",
			Description: "Upload one local image to Tripo storage and run Tripo's content check; returns its bucket/key. Free. Paid tools upload their own inputs, so this is only for testing an image.",
			InputSchema: object([]string{"path"}, props{"path": str("Image path (PNG/JPEG/WebP, up to 20 MB).")}),
			run:         handler(uploadImage),
		},
		tool{
			Name:        "tripo_wait",
			Description: "Check or wait for a job by operator_id. timeout_seconds 0 checks once; default 600. When the job is done it shows the project's state, and download_to saves the model. Free.",
			InputSchema: object([]string{"operator_id"}, props{
				"operator_id":     str("operator_id returned by a paid tool."),
				"project_id":      str("The job's project (paid tools print it); needed to show or download the result."),
				"timeout_seconds": integer("0 to 3600; default 600."),
				"download_to":     str("Save the model to this file or folder when done."),
			}),
			UI:  true,
			run: handler(waitJob),
		},
		tool{
			Name:        "tripo_download",
			Description: "Save a project's model to a file or folder. By default it is the file Studio stores (GLB for triangle meshes; quad Smart Mesh results are FBX and the extension is fixed to match). operator_id picks an earlier version. format glb/fbx/obj runs Studio's export of the current version (texture_size 512-8192; may come as a zip). The stored file is free; the export was free for own projects in the original plugin's tests but is not verified here.",
			InputSchema: object([]string{"project_id", "path"}, props{
				"project_id":   str("Project to save."),
				"path":         str("Output file, or a folder (then <project name>.glb)."),
				"operator_id":  str("A specific version (from a job); default the current one."),
				"format":       enum("Export instead of the stored file.", "glb", "fbx", "obj"),
				"texture_size": integer("Export texture size: 512, 1024, 2048, 4096 or 8192. Default the project's own."),
			}),
			UI:  true,
			run: handler(download),
		},
		tool{
			Name:        "tripo_list_projects",
			Description: "List your Studio projects with cover images, newest first, 20 per page. Free.",
			InputSchema: object(nil, props{
				"offset": integer("Skip this many projects. Default 0."),
				"filter": enum("Default all.", "all", "smart_mesh", "textured", "untextured", "rigged"),
			}),
			UI:  true,
			run: handler(listProjects),
		},
		tool{
			Name:        "tripo_project",
			Description: "Show a project with a 3D preview: name, current version, and whether it is textured, PBR, quad, Smart Mesh, segmented, rigged. Free.",
			InputSchema: object([]string{"project_id"}, props{
				"project_id":  str("Project id."),
				"operator_id": str("A specific version; default the current one."),
			}),
			UI:  true,
			run: handler(projectInfo),
		},
	)
}

type noArgs struct{}

// ---- auth and account ----

type authImportArgs struct {
	CookieFile string `json:"cookie_file"`
	Cookie     string `json:"cookie"`
	FromPlugin bool   `json:"from_plugin"`
}

func authImport(ctx context.Context, a *app, x authImportArgs) (reply, error) {
	if err := a.importSession(ctx, x.Cookie, x.CookieFile, x.FromPlugin); err != nil {
		return reply{}, err
	}
	r, err := authStatus(ctx, a, noArgs{})
	r.Text = "Imported.\n" + r.Text
	return r, err
}

func authStatus(ctx context.Context, a *app, _ noArgs) (reply, error) {
	s, err := a.loadSession()
	if err != nil {
		return reply{}, err
	}
	var r reply
	imported := s.ImportedAt
	if t, err := time.Parse(time.RFC3339, s.ImportedAt); err == nil {
		imported = t.Local().Format("2006-01-02 15:04")
	}
	r.line("Session: %s, imported %s.", s.Source, imported)
	if t, err := time.Parse(time.RFC3339, s.ExpiresAt); err == nil {
		r.line("Expires: %s (in %d days).", t.Local().Format("2006-01-02 15:04"), int(time.Until(t).Hours()/24))
	} else {
		r.line("Expires: unknown.")
	}
	if err := a.callAs(ctx, s, "GET", "/v2/studio/user/profile/payment", nil, nil); err != nil {
		return r, fmt.Errorf("live check failed: %w", err)
	}
	r.line("Live check: Tripo accepts the session.")
	return r, nil
}

func authLogout(_ context.Context, a *app, _ noArgs) (reply, error) {
	if err := os.Remove(a.sessionPath()); err != nil && !os.IsNotExist(err) {
		return reply{}, err
	}
	return say("Signed out: the stored session was deleted."), nil
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

func balance(ctx context.Context, a *app, _ noArgs) (reply, error) {
	var pay map[string]any
	if err := a.call(ctx, "GET", "/v2/studio/user/profile/payment", nil, &pay); err != nil {
		return reply{}, err
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
	return say(strings.TrimSpace(b.String())), nil
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

func uploadImage(ctx context.Context, a *app, x uploadArgs) (reply, error) {
	img, err := readImage(x.Path)
	if err != nil {
		return reply{}, err
	}
	ref, err := a.upload(ctx, img)
	if err != nil {
		return reply{}, err
	}
	return say(fmt.Sprintf("Uploaded %s (%s, %s).\nbucket: %s\nkey: %s\ncontent check: %s", img.path, img.format, size(int64(len(img.data))), ref.Bucket, ref.Key, ref.Audit)), nil
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

// inputs labels the images for the card: the view names for multiview,
// "image" for a single reference.
func inputs(imgs []*localImage, multiview bool) []input {
	var out []input
	for i, img := range imgs {
		if img == nil {
			continue
		}
		label := "image"
		if multiview {
			label = viewNames[i]
		}
		out = append(out, input{label, img})
	}
	return out
}

func describeInputs(in []input) string {
	if len(in) == 1 && in[0].label == "image" {
		return filepath.Base(in[0].img.path)
	}
	var parts []string
	for _, i := range in {
		parts = append(parts, i.label+" "+filepath.Base(i.img.path))
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

type input struct {
	label string
	img   *localImage
}

// job describes a paid operation for paid().
type job struct {
	Tool      string      // tool name, for the job log
	Title     string      // card heading, e.g. "Smart Mesh P1.0"
	Summary   string      // one line for Claude
	Settings  [][2]string // shown on the card
	Keys      []string    // Studio price keys, per request
	Requests  int         // how many requests the call makes (batch, variants); 0 means 1
	Discount  bool        // apply the membership discount (Studio does for image generation)
	Inputs    []input
	ProjectID string // the project a post-process changes
	Follow    followArgs
}

// paid is the shared flow of every paid tool: price, dry run, submit, report,
// log, then optionally wait and download.
func (a *app) paid(ctx context.Context, j job, send func() (receipt, error)) (reply, error) {
	r := reply{Card: &card{Kind: "job", Title: j.Title, Summary: j.Summary, Settings: j.Settings, ProjectID: j.ProjectID}}
	if len(j.Inputs) > 0 {
		var thumbs []string
		for _, in := range j.Inputs {
			r.Card.Inputs = append(r.Card.Inputs, in.label)
			thumbs = append(thumbs, thumb(in.img))
		}
		r.meta("thumbs", thumbs)
	}
	t, err := a.prices()
	if err != nil {
		return r, err
	}
	credits, breakdown := t.cost(j.Keys)
	if credits >= 0 && j.Requests > 1 {
		credits *= j.Requests
		breakdown = fmt.Sprintf("(%s) × %d", breakdown, j.Requests)
	}
	if credits >= 0 && j.Discount {
		m := a.member(ctx)
		if f, ok := t.Discounts[m]; ok && f != 1 {
			credits = int(math.Ceil(float64(credits) * f))
			breakdown += fmt.Sprintf(" × %g %s discount", f, m)
		}
	}
	r.Card.Cost = &cardCost{Credits: credits, Breakdown: breakdown, Updated: t.Updated}
	costLine := fmt.Sprintf("Cost: %s credits (%s; prices of %s).", thousands(credits), breakdown, t.Updated)
	if credits < 0 {
		costLine = "Cost: unknown, " + breakdown + "; add it to prices.json."
	}
	if j.Follow.DryRun {
		r.Card.Status = "quote"
		r.Text = j.Summary + "\n" + costLine
		if n, ok := a.credits(ctx); ok {
			r.Card.Cost.Balance = &n
			r.Text += " Balance: " + thousands(n) + "."
		}
		r.line("Dry run: nothing uploaded, submitted or charged.")
		return r, nil
	}
	rc, err := send()
	if err != nil {
		r.Card.Status = "failed"
		r.Text = j.Summary + "\n" + costLine
		return r, err
	}
	if rc.ProjectID == "" {
		rc.ProjectID = j.ProjectID
	}
	r.Card.Status, r.Card.ProjectID, r.Card.OperatorID = "submitted", rc.ProjectID, rc.OperatorID
	r.Text = "Submitted: " + j.Summary + "\n" + costLine
	if n, ok := a.credits(ctx); ok {
		r.Card.Cost.Balance = &n
		r.Text += " Credits left: " + thousands(n) + "."
	}
	r.line("project_id: %s\noperator_id: %s", rc.ProjectID, rc.OperatorID)
	a.logJob(jobRecord{Tool: j.Tool, Title: j.Title, Summary: j.Summary, Credits: credits, ProjectID: rc.ProjectID, OperatorID: rc.OperatorID, Status: "submitted"})
	if j.Follow.DownloadTo == "" {
		r.line("Next: tripo_wait operator_id=%s project_id=%s (add download_to to save the model).", rc.OperatorID, rc.ProjectID)
		return r, nil
	}
	return r, a.finish(ctx, &r, rc.OperatorID, rc.ProjectID, seconds(j.Follow.WaitSeconds, 900), j.Follow.DownloadTo)
}

func seconds(n, def int) time.Duration {
	if n == 0 {
		n = def
	}
	return time.Duration(min(max(n, 0), 3600)) * time.Second
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

// ---- jobs, projects, downloads ----

type waitArgs struct {
	OperatorID     string `json:"operator_id"`
	ProjectID      string `json:"project_id"`
	TimeoutSeconds *int   `json:"timeout_seconds"`
	DownloadTo     string `json:"download_to"`
}

func waitJob(ctx context.Context, a *app, x waitArgs) (reply, error) {
	if x.OperatorID == "" {
		return reply{}, errors.New("operator_id is required")
	}
	timeout := 600 * time.Second
	if x.TimeoutSeconds != nil {
		timeout = time.Duration(min(max(*x.TimeoutSeconds, 0), 3600)) * time.Second
	}
	title := "Job"
	if jobs, err := a.jobs(); err == nil {
		for _, j := range jobs {
			if j.OperatorID == x.OperatorID && j.Title != "" {
				title = j.Title
			}
		}
	}
	r := reply{Card: &card{Kind: "job", Title: title, OperatorID: x.OperatorID, ProjectID: x.ProjectID}}
	return r, a.finish(ctx, &r, x.OperatorID, x.ProjectID, timeout, x.DownloadTo)
}

// finish waits for a job and reports the project; with downloadTo it also
// saves the new version of the model.
func (a *app) finish(ctx context.Context, r *reply, op, projectID string, timeout time.Duration, downloadTo string) error {
	it, err := a.wait(ctx, op, timeout)
	if err != nil {
		return err
	}
	if it.ProjectID != "" {
		projectID = it.ProjectID
	}
	r.Card.OperatorID, r.Card.Progress = op, it.Progress
	switch it.Status {
	case "success":
	case "failed", "cancelled", "expired", "banned":
		r.Card.Status = "failed"
		a.logJob(jobRecord{OperatorID: op, Status: it.Status})
		reason := ""
		if it.Reason != nil {
			reason = fmt.Sprintf(": %v", it.Reason)
		}
		return fmt.Errorf("job %s ended as %s%s", op, it.Status, reason)
	default:
		r.Card.Status = "running"
		next := fmt.Sprintf("tripo_wait operator_id=%s project_id=%s", op, projectID)
		if downloadTo != "" {
			next += " download_to=" + downloadTo
		}
		r.line("Still %s (%.0f%%) after %s; nothing downloaded yet. Next: %s", it.Status, it.Progress, timeout, next)
		return nil
	}
	r.Card.Status = "done"
	if projectID == "" {
		if downloadTo != "" {
			return fmt.Errorf("job %s is done, but Tripo's progress does not name the project; call tripo_wait again with project_id to download", op)
		}
		r.line("Done: job %s succeeded. Pass project_id to see the project.", op)
		return nil
	}
	p, err := a.project(ctx, projectID, op)
	if err != nil {
		return err
	}
	r.line("Done. %s", describe(p))
	saved := ""
	if downloadTo != "" {
		path, line, err := a.download(ctx, p, downloadTo, "", 0)
		if err != nil {
			a.showProject(ctx, r, p)
			return err
		}
		r.line("%s", line)
		r.Card.Status, r.Card.Saved, saved = "saved", []string{path}, path
		r.meta("model", a.previewModel(ctx, path, "", filepath.Base(path)))
	}
	a.showProject(ctx, r, p) // after the download, so the preview is the saved file
	if r.Card.Title == "Job" {
		r.Card.Title = p.Name
	}
	r.Card.Next = nextSteps(p)
	a.logJob(jobRecord{OperatorID: op, ProjectID: projectID, Status: "done", Saved: saved})
	return nil
}

// nextSteps suggests the usual follow-up as a widget button.
func nextSteps(p *project) []cardAction {
	var next []cardAction
	if !p.is("is_textured") {
		next = append(next, cardAction{"Texture 2K · 10 credits", fmt.Sprintf("Texture Tripo project %s at 2K from the same views.", p.ID)})
	}
	if p.is("is_segmented") {
		next = append(next, cardAction{"List parts", fmt.Sprintf("List the parts of Tripo project %s.", p.ID)})
	}
	return next
}

func describe(p *project) string {
	var flags []string
	for _, f := range projectFlags {
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

func download(ctx context.Context, a *app, x downloadArgs) (reply, error) {
	if x.Format != "" && x.OperatorID != "" {
		return reply{}, errors.New("format exports the current version only; drop operator_id or format")
	}
	p, err := a.project(ctx, x.ProjectID, x.OperatorID)
	if err != nil {
		return reply{}, err
	}
	r := reply{Card: &card{Kind: "project", Title: p.Name, Status: "saved"}}
	path, line, err := a.download(ctx, p, x.Path, x.Format, x.TextureSize)
	if err != nil {
		r.Card.Status = "failed"
		a.showProject(ctx, &r, p)
		return r, err
	}
	r.Text, r.Card.Saved = line, []string{path}
	if k := sniff(head(path)); k == "glb" || k == "fbx" {
		r.meta("model", a.previewModel(ctx, path, "", filepath.Base(path)))
	}
	a.showProject(ctx, &r, p)
	return r, nil
}

// download saves the project's model and returns the path and a result line.
func (a *app) download(ctx context.Context, p *project, dest, format string, textureSize int) (string, string, error) {
	src, label, ext := p.ModelURL, "the stored model", ".glb"
	if format != "" {
		u, err := a.exportURL(ctx, p, format, textureSize)
		if err != nil {
			return "", "", fmt.Errorf("export failed: %w", err)
		}
		src, label, ext = u, format+" export", "."+format
	} else if src == "" {
		return "", "", errors.New("the project has no model file yet")
	}
	name := p.Name
	if name == "" {
		name = p.ID
	}
	path, err := outputPath(dest, name, ext)
	if err != nil {
		return "", "", err
	}
	final, n, kind, err := a.save(ctx, src, path)
	if err != nil {
		return "", "", err
	}
	note := ""
	if final != path {
		note = fmt.Sprintf(" (the file is %s, so the extension was changed)", strings.ToUpper(kind))
	}
	return final, fmt.Sprintf("Saved %s to %s, %s%s.", label, final, size(n), note), nil
}

type listArgs struct {
	Offset int    `json:"offset"`
	Filter string `json:"filter"`
}

func listProjects(ctx context.Context, a *app, x listArgs) (reply, error) {
	filter := x.Filter
	if filter == "" {
		filter = "all"
	}
	q := url.Values{"asset_type": {"mine"}, "offset": {strconv.Itoa(x.Offset)}, "size": {"20"}, "type": {filter}}
	var page struct {
		Total    int `json:"total"`
		Projects []struct {
			ID         string   `json:"id"`
			Name       string   `json:"project_name"`
			Type       string   `json:"type"`
			Visibility string   `json:"visibility"`
			Running    any      `json:"running_operator"`
			Covers     []string `json:"cover_image"`
		} `json:"projects"`
	}
	if err := a.call(ctx, "GET", "/v2/studio/assets/v2?"+q.Encode(), nil, &page); err != nil {
		return reply{}, err
	}
	r := reply{Card: &card{Kind: "projects", Title: "Projects"}}
	if len(page.Projects) == 0 {
		r.Text = fmt.Sprintf("No projects (filter %s, offset %d, total %d).", filter, x.Offset, page.Total)
		return r, nil
	}
	r.Card.Summary = fmt.Sprintf("%d-%d of %d · %s", x.Offset+1, x.Offset+len(page.Projects), page.Total, filter)
	r.line("Projects %d-%d of %d (filter %s):", x.Offset+1, x.Offset+len(page.Projects), page.Total, filter)
	covers := map[string]string{}
	for _, p := range page.Projects {
		m, isMap := p.Running.(map[string]any)
		running := (isMap && len(m) > 0) || (!isMap && p.Running != nil)
		r.Card.Items = append(r.Card.Items, &projectCard{ID: p.ID, Name: p.Name, Type: p.Type, Visibility: p.Visibility, Running: running, URL: studioURL(p.ID)})
		if len(p.Covers) > 0 && p.Covers[0] != "" {
			covers[p.ID] = p.Covers[0]
		}
		state := ""
		if running {
			state = "  [job running]"
		}
		r.line("- %s  %q  %s  %s%s", p.ID, p.Name, p.Type, p.Visibility, state)
	}
	r.meta("covers", covers)
	if next := x.Offset + len(page.Projects); next < page.Total {
		r.Card.More = fmt.Sprintf("offset=%d", next)
		r.line("More: offset=%d", next)
	}
	return r, nil
}

type projectArgs struct {
	ProjectID  string `json:"project_id"`
	OperatorID string `json:"operator_id"`
}

func projectInfo(ctx context.Context, a *app, x projectArgs) (reply, error) {
	p, err := a.project(ctx, x.ProjectID, x.OperatorID)
	if err != nil {
		return reply{}, err
	}
	r := reply{Card: &card{Kind: "project", Title: p.Name, Status: p.Status}}
	a.showProject(ctx, &r, p)
	r.Card.Next = nextSteps(p)
	r.line("%s", describe(p))
	return r, nil
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
