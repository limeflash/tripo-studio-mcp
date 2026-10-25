package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

// Studio image generation. Image jobs are image assets (asset_id), polled by
// asset status; the shared paid() flow is operator/project based, so these
// tools use paidImage() below.

var imageFollowProps = props{
	"dry_run":      boolean("Check the inputs and return the cost; nothing is uploaded, submitted or charged."),
	"download_to":  str("Wait for the job, then save the output images into this folder (created if missing)."),
	"wait_seconds": integer("How long to wait for the images (default 900 with download_to, else do not wait; max 3600)."),
}

func init() {
	register(
		tool{
			Name: "tripo_generate_image",
			Description: "PAID: generate 1-4 images in Studio from a prompt, up to 10 reference images and/or a template (see tripo_list_image_templates). Submits at once; returns an asset_id (an image asset, not a project). Add download_to to wait and save the images.\n" +
				"Credits per image: Nano Banana, Nano Banana 2, FLUX Kontext Pro, GPT-4o, Midjourney 10; GPT Image 2, GPT Image 2.5, Nano Banana Pro 20; +10 per image for 4K. The membership discount applies (premium pays 0.2x) and the free image quota is deducted. Default: Nano Banana 2, 1 image, 1K = 10 base, 2 on a premium account. Models without a known price (flux.1_dev, gpt_image_1.5) are not offered.\n" +
				"Use the result with tripo_generate_model mode studio_image, or tripo_generate_multiview.",
			InputSchema: object(nil, imageFollowProps, props{
				"prompt":            str("Text prompt, up to 1000 characters; optional when references or a template are given."),
				"model_version":     enum("Default gemini_3.1_flash_image_preview (Nano Banana 2).", imageModelNames()...),
				"amount":            integer("Number of images, 1-4. Default 1."),
				"resolution":        enum("Default 1K; 4K adds ImageUpscale4K per image.", "1K", "4K"),
				"scale":             enum("Aspect ratio. Default 1:1.", imageRatios...),
				"sketch_to_render":  boolean("Treat the reference as a sketch. Default false."),
				"t_pose":            boolean("Ask for a T-pose character. Default false."),
				"template_id":       str("Template id from tripo_list_image_templates."),
				"image_paths":       strList("Local reference images (PNG/JPEG/WebP), up to 10 with studio_references. Not for Midjourney."),
				"studio_references": props{"type": "array", "description": "Completed Studio image outputs to reuse as references, without uploading.", "items": object([]string{"asset_id"}, props{"asset_id": str("Image asset id."), "output_index": integer("Which output, default 0.")})},
				"allow_sensitive":   boolean("Accept a reference that Studio's content check marks sensitive. Only after looking at it."),
			}),
			UI:  true,
			run: handler(generateImage),
		},
		tool{
			Name: "tripo_generate_multiview",
			Description: "PAID: make the four-view set (front, left, back, right) from one output of a finished tripo_generate_image asset. Submits at once; returns an asset_id. Credits: not known (Studio's price config has no entry, so dry_run says unknown; add ImageMultiview to prices.json once seen). Add download_to to wait and save the four images.\n" +
				"Use the result with tripo_generate_model mode studio_multiview.",
			InputSchema: object([]string{"source_asset_id"}, imageFollowProps, props{
				"source_asset_id": str("A successful generate_image asset (tripo_list_image_assets)."),
				"output_index":    integer("Which output of that asset to use, default 0."),
				"allow_sensitive": boolean("Accept an output that Studio marks sensitive."),
			}),
			UI:  true,
			run: handler(generateMultiview),
		},
		tool{
			Name:        "tripo_regenerate_image",
			Description: "PAID: re-run an image or multiview asset with the same settings; makes a new asset and leaves the old one. Submits at once. Credits: not known (no Studio price entry; dry_run says unknown; add ImageRegenerate to prices.json once seen).",
			InputSchema: object([]string{"asset_id"}, imageFollowProps, props{"asset_id": str("A generate_image or multiview_images asset that is not running.")}),
			UI:          true,
			run:         handler(regenerateImage),
		},
		tool{
			Name:        "tripo_upscale_image",
			Description: "PAID, 10 credits (ImageUpscale4K, no membership discount): upscale one output of an image asset to 4K; makes a new asset. Submits at once.",
			InputSchema: object([]string{"asset_id"}, imageFollowProps, props{"asset_id": str("A successful image asset."), "output_index": integer("Which output, default 0.")}),
			UI:          true,
			run:         handler(transformer("upscale", "Upscale 4K", []string{"ImageUpscale4K"})),
		},
		tool{
			Name:        "tripo_split_image",
			Description: "PAID: cut the separate subjects out of one image output (for example a sheet of characters); makes a new asset with one image per subject. Submits at once. Credits: not known (no Studio price entry; dry_run says unknown; add ImageSplit to prices.json once seen).",
			InputSchema: object([]string{"asset_id"}, imageFollowProps, props{"asset_id": str("A successful image asset."), "output_index": integer("Which output, default 0.")}),
			UI:          true,
			run:         handler(transformer("split", "Split subjects", []string{"ImageSplit"})),
		},
		tool{
			Name:        "tripo_list_image_assets",
			Description: "List your Studio image assets (generated, multiview, upscaled, split), newest first, with the first output of each. Free.",
			InputSchema: object(nil, props{"page_num": integer("Page, from 1. Default 1."), "page_size": integer("1-100. Default 20.")}),
			UI:          true,
			run:         handler(listImageAssets),
		},
		tool{
			Name:        "tripo_get_image_asset",
			Description: "Show one image asset: type, status, settings and outputs. wait_seconds waits for a running asset; download_to saves its output images into a folder. Free.",
			InputSchema: object([]string{"asset_id"}, props{
				"asset_id":     str("Image asset id."),
				"wait_seconds": integer("0 checks once (default); up to 3600."),
				"download_to":  str("Folder to save the output images into; waits up to wait_seconds (default 900) when the asset is still running."),
			}),
			UI:  true,
			run: handler(getImageAsset),
		},
		tool{
			Name:        "tripo_list_image_templates",
			Description: "List Studio's image templates (style presets) for tripo_generate_image template_id. Free.",
			InputSchema: object(nil, props{"tag": str("Keep templates with this tag, e.g. Game."), "query": str("Keep templates whose title, description or tags contain this text.")}),
			run:         handler(listImageTemplates),
		},
	)
}

// ---- models and prices ----

// imageModels maps Studio's model ids to their price keys (the plugin's
// IMAGE_KEYS). The first is the default: the cheapest, with the newest Nano Banana.
var imageModels = []struct{ id, key, label string }{
	{"gemini_3.1_flash_image_preview", "NanoBanana_2", "Nano Banana 2"},
	{"gemini_2.5_flash_image_preview", "NanoBanana", "Nano Banana"},
	{"flux.1_kontext_pro", "FLUX_1_pro", "FLUX Kontext Pro"},
	{"gpt_4o", "GPT_4o", "GPT-4o"},
	{"midjourney", "Midjourney", "Midjourney"},
	{"gpt_image_2", "GPT_image_2", "GPT Image 2"},
	{"gpt_image_2.5_sunburst", "GPT_image_2_5", "GPT Image 2.5"},
	{"gemini_3_pro_image_preview", "NanoBanana_pro", "Nano Banana Pro"},
}

var imageRatios = []string{"1:1", "16:9", "9:16", "4:3", "3:4"}

func imageModelNames() []string {
	var names []string
	for _, m := range imageModels {
		names = append(names, m.id)
	}
	return names
}

// netCost prices n requests of keys: the free image quota covers the first
// requests, the membership discount applies to the rest (Studio rounds up).
func (t priceTable) netCost(keys []string, n int, discount float64, free int) (int, string) {
	per, breakdown := t.cost(keys)
	if per < 0 {
		return per, breakdown
	}
	n = max(n, 1)
	if n > 1 {
		breakdown = fmt.Sprintf("(%s) × %d", breakdown, n)
	}
	if discount != 1 {
		breakdown += fmt.Sprintf(" × %g discount", discount)
	}
	full := int(math.Ceil(float64(per*n) * discount))
	if free = min(max(free, 0), n); free > 0 {
		breakdown += fmt.Sprintf("; the free image quota covers %d of %d (otherwise %d)", free, n, full)
	}
	return int(math.Ceil(float64(per*(n-free)) * discount)), breakdown
}

// freeImages reads how many free image generations are left; 0 if Tripo does not say.
func (a *app) freeImages(ctx context.Context) int {
	var m struct {
		Trials map[string]struct {
			Total int   `json:"total_count"`
			Used  int   `json:"used_count"`
			Left  *int  `json:"remaining"`
			Valid *bool `json:"is_valid"`
		} `json:"free_trial"`
	}
	if a.call(ctx, "GET", "/v2/studio/marketing/detail?locale=zh", nil, &m) != nil {
		return 0
	}
	t, ok := m.Trials["remaining-free-image-count"]
	if !ok {
		return 0
	}
	if t.Left != nil {
		return max(*t.Left, 0)
	}
	return max(t.Total-t.Used, 0)
}

// quoteJob prices a job like paid() does, plus the free image quota for
// image generation (j.Discount).
func (a *app) quoteJob(ctx context.Context, j job) (credits int, breakdown, line string, cost *cardCost, err error) {
	t, err := a.prices()
	if err != nil {
		return 0, "", "", nil, err
	}
	discount, free := 1.0, 0
	if j.Discount {
		if f, ok := t.Discounts[a.member(ctx)]; ok {
			discount = f
		}
		free = a.freeImages(ctx)
	}
	credits, breakdown = t.netCost(j.Keys, j.Requests, discount, free)
	cost = &cardCost{Credits: credits, Breakdown: breakdown, Updated: t.Updated}
	line = fmt.Sprintf("Cost: %s credits (%s; prices of %s).", thousands(credits), breakdown, t.Updated)
	if credits < 0 {
		line = "Cost: unknown, " + breakdown + "; add it to prices.json."
	}
	return credits, breakdown, line, cost, nil
}

// paidImage is paid() for image assets: price, dry run, submit, log, then
// optionally wait for the asset and save its images.
func (a *app) paidImage(ctx context.Context, j job, send func() (string, error)) (reply, error) {
	r := reply{Card: &card{Kind: "images", Title: j.Title, Summary: j.Summary, Settings: j.Settings}}
	if len(j.Inputs) > 0 {
		var thumbs []string
		for _, in := range j.Inputs {
			r.Card.Inputs = append(r.Card.Inputs, in.label)
			thumbs = append(thumbs, thumb(in.img))
		}
		r.meta("thumbs", thumbs)
	}
	credits, _, costLine, cost, err := a.quoteJob(ctx, j)
	if err != nil {
		return r, err
	}
	r.Card.Cost = cost
	if j.Follow.DryRun {
		r.Card.Status = "quote"
		r.Text = j.Summary + "\n" + costLine
		if n, ok := a.credits(ctx); ok {
			cost.Balance = &n
			r.Text += " Balance: " + thousands(n) + "."
		}
		r.line("Dry run: nothing uploaded, submitted or charged.")
		return r, nil
	}
	id, err := send()
	if err != nil {
		r.Card.Status = "failed"
		r.Text = j.Summary + "\n" + costLine
		return r, err
	}
	r.Card.Status = "submitted"
	r.Text = "Submitted: " + j.Summary + "\n" + costLine
	if n, ok := a.credits(ctx); ok {
		cost.Balance = &n
		r.Text += " Credits left: " + thousands(n) + "."
	}
	r.line("asset_id: %s", id)
	a.logJob(jobRecord{Tool: j.Tool, Title: j.Title, Summary: j.Summary, Credits: max(credits, 0), OperatorID: id, Status: "submitted"})
	if j.Follow.DownloadTo == "" && j.Follow.WaitSeconds <= 0 {
		r.line("Next: tripo_get_image_asset asset_id=%s wait_seconds=300 download_to=<folder>.", id)
		return r, nil
	}
	return r, a.followImage(ctx, &r, id, seconds(j.Follow.WaitSeconds, 900), j.Follow.DownloadTo)
}

// submitImage sends a paid image request and returns the new asset_id. As in
// submit(), only a refusal proves nothing was charged.
func (a *app) submitImage(ctx context.Context, path string, body any) (string, error) {
	var r struct {
		AssetID string `json:"asset_id"`
	}
	err := a.call(ctx, "POST", path, body, &r)
	var refused *apiError
	if errors.As(err, &refused) || errors.Is(err, errAuth) || errors.Is(err, errNoSession) {
		return "", fmt.Errorf("not submitted, nothing charged: %w", err)
	}
	if err != nil {
		return "", fmt.Errorf("outcome unknown: %v. The job may have been accepted and charged; check tripo_list_image_assets before submitting again", err)
	}
	if r.AssetID == "" {
		return "", fmt.Errorf("outcome unknown: Tripo accepted %s but returned no asset_id; check tripo_list_image_assets before submitting again", path)
	}
	return r.AssetID, nil
}

// ---- image assets ----

type imageOutput struct {
	Bucket string `json:"bucket"`
	Key    string `json:"key"`
	URL    string `json:"url"`
	Audit  string `json:"image_audit_result"`
}

type imageAsset struct {
	ID      string         `json:"asset_id"`
	Created any            `json:"create_time"`
	Input   map[string]any `json:"input"`
	Output  struct {
		Data []imageOutput `json:"data"`
	} `json:"output"`
	Status string `json:"status"`
	Type   string `json:"type"`
}

func (s *imageAsset) done() bool {
	switch s.Status {
	case "success", "failed", "cancelled", "expired", "banned":
		return true
	}
	return false
}

func (s *imageAsset) busy() bool {
	switch s.Status {
	case "prepare", "queued", "running":
		return true
	}
	return false
}

func (a *app) imageAsset(ctx context.Context, id string) (*imageAsset, error) {
	if id == "" {
		return nil, errors.New("asset_id is required")
	}
	var s imageAsset
	if err := a.call(ctx, "POST", "/v2/studio/image/get_image_asset", map[string]string{"asset_id": id}, &s); err != nil {
		return nil, err
	}
	if s.ID != "" && s.ID != id {
		return nil, fmt.Errorf("Tripo returned image asset %s when asked for %s", s.ID, id)
	}
	s.ID = id
	return &s, nil
}

// waitImage polls an asset until it ends or the timeout passes; 0 checks once.
func (a *app) waitImage(ctx context.Context, id string, timeout time.Duration) (*imageAsset, error) {
	deadline := time.Now().Add(timeout)
	misses := 0
	for {
		s, err := a.imageAsset(ctx, id)
		if err == nil {
			misses = 0
			if s.done() || time.Now().After(deadline) {
				return s, nil
			}
		} else if misses++; errors.Is(err, errAuth) || errors.Is(err, errNoSession) || misses >= 5 || time.Now().After(deadline) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// source picks one successful output of an asset to feed another request.
func (s *imageAsset) source(index int, wantType string) (imageOutput, error) {
	if s.Status != "success" {
		return imageOutput{}, fmt.Errorf("image asset %s is %s, not success", s.ID, s.Status)
	}
	if wantType != "" && s.Type != wantType {
		return imageOutput{}, fmt.Errorf("image asset %s is %s, need %s", s.ID, s.Type, wantType)
	}
	if index < 0 || index >= len(s.Output.Data) || s.Output.Data[index].Key == "" {
		return imageOutput{}, fmt.Errorf("image asset %s has no output %d (it has %d)", s.ID, index, len(s.Output.Data))
	}
	return s.Output.Data[index], nil
}

// imageAudit returns Studio's content verdict for an output, asking the audit
// endpoint when the asset did not carry one.
func (a *app) imageAudit(ctx context.Context, o imageOutput, allowSensitive bool) (string, error) {
	audit := o.Audit
	if audit != "pass" && audit != "sensitive" {
		var r struct {
			Result string `json:"result"`
		}
		if err := a.call(ctx, "POST", "/v2/studio/audit/image", map[string]any{"image": map[string]string{"bucket": o.Bucket, "key": o.Key}}, &r); err != nil {
			return "", err
		}
		audit = r.Result
	}
	if audit == "sensitive" && !allowSensitive {
		return "", errors.New("the selected image is marked sensitive by Studio; pass allow_sensitive=true only after looking at it")
	}
	if audit != "pass" && audit != "sensitive" {
		return "", fmt.Errorf("Studio's content check returned %q for the selected image", audit)
	}
	return audit, nil
}

func imageWire(o imageOutput, audit string) map[string]any {
	return map[string]any{"bucket": o.Bucket, "image_audit_result": audit, "image_source": "generate", "key": o.Key}
}

func outputLabel(typ string, i, n int) string {
	switch {
	case typ == "multiview_images" && i < len(viewNames):
		return viewNames[i]
	case typ == "split_image":
		return fmt.Sprintf("part %d", i+1)
	case n == 1:
		return "image"
	}
	return fmt.Sprintf("image %d", i+1)
}

// showImages puts an asset's outputs on the card: labels there, signed URLs in _meta.images.
func showImages(r *reply, s *imageAsset) {
	var urls []string
	r.Card.Images = nil
	for i, o := range s.Output.Data {
		label := outputLabel(s.Type, i, len(s.Output.Data))
		if o.Audit == "sensitive" || o.Audit == "nsfw" {
			label += " (" + o.Audit + ")"
		}
		r.Card.Images = append(r.Card.Images, cardImage{Label: label, ID: s.ID})
		urls = append(urls, o.URL)
	}
	r.meta("images", urls)
}

// followImage waits for an asset, shows its outputs and, with dest, saves them.
func (a *app) followImage(ctx context.Context, r *reply, id string, timeout time.Duration, dest string) error {
	s, err := a.waitImage(ctx, id, timeout)
	if err != nil {
		return err
	}
	r.Card.Kind = "images"
	switch {
	case s.Status == "success":
	case s.done():
		r.Card.Status = "failed"
		a.logJob(jobRecord{OperatorID: id, Status: s.Status})
		return fmt.Errorf("image asset %s ended as %s", id, s.Status)
	default:
		r.Card.Status = "running"
		next := fmt.Sprintf("tripo_get_image_asset asset_id=%s wait_seconds=600", id)
		if dest != "" {
			next += " download_to=" + dest
		}
		r.line("Still %s after %s; nothing downloaded yet. Next: %s", s.Status, timeout, next)
		return nil
	}
	r.Card.Status = "done"
	if r.Card.Title == "" {
		r.Card.Title = map[string]string{"generate_image": "Image", "multiview_images": "Four views", "upscale_image": "Upscaled image", "split_image": "Split subjects"}[s.Type]
	}
	showImages(r, s)
	r.line("Done. Asset %s (%s): %d image(s).", id, s.Type, len(s.Output.Data))
	saved := ""
	if dest != "" {
		paths, err := a.saveImages(ctx, s, dest)
		r.Card.Saved = paths
		if len(paths) > 0 {
			r.Card.Status, saved = "saved", dest
			r.line("Saved %d image(s) to %s:\n%s", len(paths), dest, strings.Join(paths, "\n"))
		}
		if err != nil {
			return err
		}
	}
	a.logJob(jobRecord{OperatorID: id, Status: "done", Saved: saved})
	return nil
}

// saveImages writes every output of an asset into the folder dest, named
// <asset id start>-<label>.<png|jpg|webp>.
func (a *app) saveImages(ctx context.Context, s *imageAsset, dest string) ([]string, error) {
	abs, err := filepath.Abs(dest)
	if err != nil {
		return nil, err
	}
	if st, err := os.Stat(abs); err == nil && !st.IsDir() {
		return nil, fmt.Errorf("download_to %s is a file; the images go into a folder", abs)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, err
	}
	short := safeName(s.ID)
	if len(short) > 8 {
		short = short[:8]
	}
	var paths []string
	for i, o := range s.Output.Data {
		if o.URL == "" {
			return paths, fmt.Errorf("output %d has no download URL", i)
		}
		body, err := a.open(ctx, o.URL)
		if err != nil {
			return paths, err
		}
		data, err := io.ReadAll(io.LimitReader(body, 128<<20))
		body.Close()
		if err != nil {
			return paths, fmt.Errorf("download failed: %w", err)
		}
		name := short + "-" + strings.ReplaceAll(safeName(outputLabel(s.Type, i, len(s.Output.Data))), " ", "-") + imageExt(data)
		path := filepath.Join(abs, name)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return paths, err
		}
		paths = append(paths, path)
	}
	return paths, nil
}

func imageExt(b []byte) string {
	switch {
	case strings.HasPrefix(string(b), "\x89PNG"):
		return ".png"
	case strings.HasPrefix(string(b), "\xff\xd8\xff"):
		return ".jpg"
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return ".webp"
	}
	return ".png" // ponytail: unknown bytes keep the PNG name; sniff more formats if Studio sends them
}

// ---- generate ----

type studioRef struct {
	AssetID     string `json:"asset_id"`
	OutputIndex int    `json:"output_index"`
}

type imageGenArgs struct {
	Prompt           string      `json:"prompt"`
	ModelVersion     string      `json:"model_version"`
	Amount           int         `json:"amount"`
	Resolution       string      `json:"resolution"`
	Scale            string      `json:"scale"`
	SketchToRender   bool        `json:"sketch_to_render"`
	TPose            bool        `json:"t_pose"`
	TemplateID       string      `json:"template_id"`
	ImagePaths       []string    `json:"image_paths"`
	StudioReferences []studioRef `json:"studio_references"`
	AllowSensitive   bool        `json:"allow_sensitive"`
	followArgs
}

// imagePlan is a checked gen_image_v2 request without its reference images.
type imagePlan struct {
	body     map[string]any
	keys     []string
	requests int
	title    string
	settings [][2]string
	summary  string
}

// cleanPrompt turns control characters into spaces and trims, as Studio's site does.
func cleanPrompt(s string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return ' '
		}
		return r
	}, s))
}

func (x imageGenArgs) plan() (imagePlan, error) {
	fail := func(format string, args ...any) (imagePlan, error) { return imagePlan{}, fmt.Errorf(format, args...) }
	model := x.ModelVersion
	if model == "" {
		model = imageModels[0].id
	}
	key, label := "", ""
	for _, m := range imageModels {
		if m.id == model {
			key, label = m.key, m.label
		}
	}
	if key == "" {
		return fail("model_version %q is not offered (no known price); use one of %s", model, strings.Join(imageModelNames(), ", "))
	}
	amount := x.Amount
	if amount == 0 {
		amount = 1
	}
	if amount < 1 || amount > 4 {
		return fail("amount must be 1 to 4")
	}
	res := x.Resolution
	if res == "" {
		res = "1K"
	}
	if res != "1K" && res != "4K" {
		return fail("resolution must be 1K or 4K")
	}
	scale := x.Scale
	if scale == "" {
		scale = "1:1"
	}
	if !contains(imageRatios, scale) {
		return fail("scale must be one of %s", strings.Join(imageRatios, ", "))
	}
	prompt := cleanPrompt(x.Prompt)
	if utf8.RuneCountInString(prompt) > 1000 {
		return fail("prompt is over 1000 characters")
	}
	refs := len(x.ImagePaths) + len(x.StudioReferences)
	if refs > 10 {
		return fail("at most 10 references in total (image_paths plus studio_references)")
	}
	if prompt == "" && refs == 0 && x.TemplateID == "" {
		return fail("give a prompt, a reference image or a template_id")
	}
	if model == "midjourney" && refs > 0 {
		return fail("Studio's Midjourney mode takes no reference images")
	}
	body := map[string]any{
		"amount": amount, "if_upload": true, "model_version": model, "prompt": prompt, "resolution": res,
		"scale": scale, "sketch_to_render": x.SketchToRender, "t_pose": x.TPose,
	}
	if x.TemplateID != "" {
		body["template_id"] = x.TemplateID
	}
	p := imagePlan{body: body, keys: []string{key}, requests: amount, title: "Image generation"}
	if res == "4K" {
		p.keys = append(p.keys, "ImageUpscale4K")
	}
	p.settings = [][2]string{{"Model", label}, {"Images", fmt.Sprint(amount)}, {"Resolution", res}, {"Ratio", scale}}
	if x.TemplateID != "" {
		p.settings = append(p.settings, [2]string{"Template", x.TemplateID})
	}
	p.summary = fmt.Sprintf("%s: %d × %s %s %s", label, amount, res, scale, "image")
	if prompt != "" {
		p.summary += fmt.Sprintf(", prompt %q", clip(prompt, 80))
	}
	if refs > 0 {
		p.summary += fmt.Sprintf(", %d reference(s)", refs)
	}
	return p, nil
}

func contains(list []string, s string) bool {
	for _, e := range list {
		if e == s {
			return true
		}
	}
	return false
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func generateImage(ctx context.Context, a *app, x imageGenArgs) (reply, error) {
	p, err := x.plan()
	if err != nil {
		return reply{}, err
	}
	var imgs []*localImage
	var in []input
	for i, path := range x.ImagePaths {
		img, err := readImage(path)
		if err != nil {
			return reply{}, err
		}
		imgs = append(imgs, img)
		in = append(in, input{fmt.Sprintf("reference %d", i+1), img})
	}
	// Studio references are checked now (free reads), so a dry run catches a bad asset.
	var refs []any
	for _, ref := range x.StudioReferences {
		s, err := a.imageAsset(ctx, ref.AssetID)
		if err != nil {
			return reply{}, err
		}
		o, err := s.source(ref.OutputIndex, "")
		if err != nil {
			return reply{}, err
		}
		audit, err := a.imageAudit(ctx, o, x.AllowSensitive)
		if err != nil {
			return reply{}, err
		}
		refs = append(refs, imageWire(o, audit))
	}
	j := job{Tool: "tripo_generate_image", Title: p.title, Summary: p.summary, Settings: p.settings, Keys: p.keys, Requests: p.requests, Discount: true, Inputs: in, Follow: x.followArgs}
	return a.paidImage(ctx, j, func() (string, error) {
		all := append([]any{}, refs...)
		uploaded, err := a.uploadAll(ctx, imgs)
		if err != nil {
			return "", err
		}
		for _, u := range uploaded {
			ref := u.(imageRef)
			if ref.Audit == "sensitive" && !x.AllowSensitive {
				return "", errors.New("not submitted, nothing charged: a reference image is marked sensitive by Studio; pass allow_sensitive=true only after looking at it")
			}
			all = append(all, ref)
		}
		switch {
		case len(all) == 1:
			p.body["image"] = all[0]
		case len(all) > 1:
			p.body["images"] = all
		}
		return a.submitImage(ctx, "/v2/studio/image/gen_image_v2", p.body)
	})
}

// ---- multiview, regenerate, upscale, split ----

type multiviewArgs struct {
	SourceAssetID  string `json:"source_asset_id"`
	OutputIndex    int    `json:"output_index"`
	AllowSensitive bool   `json:"allow_sensitive"`
	followArgs
}

func generateMultiview(ctx context.Context, a *app, x multiviewArgs) (reply, error) {
	s, err := a.imageAsset(ctx, x.SourceAssetID)
	if err != nil {
		return reply{}, err
	}
	o, err := s.source(x.OutputIndex, "generate_image")
	if err != nil {
		return reply{}, err
	}
	audit, err := a.imageAudit(ctx, o, x.AllowSensitive)
	if err != nil {
		return reply{}, err
	}
	body := map[string]any{"if_upload": true, "image": imageWire(o, audit)}
	j := job{
		Tool: "tripo_generate_multiview", Title: "Four views", Keys: []string{"ImageMultiview"}, Follow: x.followArgs,
		Settings: [][2]string{{"Source", x.SourceAssetID}, {"Output", fmt.Sprint(x.OutputIndex)}},
		Summary:  fmt.Sprintf("four views (front, left, back, right) of output %d of image asset %s", x.OutputIndex, x.SourceAssetID),
	}
	return a.paidImage(ctx, j, func() (string, error) {
		return a.submitImage(ctx, "/v2/studio/image/gen_multiview", body)
	})
}

type regenerateArgs struct {
	AssetID string `json:"asset_id"`
	followArgs
}

func regenerateImage(ctx context.Context, a *app, x regenerateArgs) (reply, error) {
	s, err := a.imageAsset(ctx, x.AssetID)
	if err != nil {
		return reply{}, err
	}
	endpoint := map[string]string{"generate_image": "re_gen_image", "multiview_images": "re_gen_multiview"}[s.Type]
	if endpoint == "" {
		return reply{}, fmt.Errorf("image asset %s is %s; only generate_image and multiview_images can be regenerated", x.AssetID, s.Type)
	}
	if s.busy() {
		return reply{}, fmt.Errorf("image asset %s is still %s; wait for it first", x.AssetID, s.Status)
	}
	j := job{
		Tool: "tripo_regenerate_image", Title: "Regenerate " + map[string]string{"generate_image": "image", "multiview_images": "four views"}[s.Type], Keys: []string{"ImageRegenerate"}, Follow: x.followArgs,
		Settings: [][2]string{{"Source", x.AssetID}, {"Type", s.Type}},
		Summary:  fmt.Sprintf("regenerate %s asset %s with the same settings", s.Type, x.AssetID),
	}
	return a.paidImage(ctx, j, func() (string, error) {
		return a.submitImage(ctx, "/v2/studio/image/"+endpoint, map[string]any{"asset_id": x.AssetID})
	})
}

type transformArgs struct {
	AssetID     string `json:"asset_id"`
	OutputIndex int    `json:"output_index"`
	followArgs
}

// transformer builds the upscale and split tools: both send the asset and the
// storage key of one output.
func transformer(endpoint, title string, keys []string) func(context.Context, *app, json.RawMessage) (reply, error) {
	return handler(func(ctx context.Context, a *app, x transformArgs) (reply, error) {
		s, err := a.imageAsset(ctx, x.AssetID)
		if err != nil {
			return reply{}, err
		}
		o, err := s.source(x.OutputIndex, "")
		if err != nil {
			return reply{}, err
		}
		j := job{
			Tool: "tripo_" + endpoint + "_image", Title: title, Keys: keys, Follow: x.followArgs,
			Settings: [][2]string{{"Source", x.AssetID}, {"Output", fmt.Sprint(x.OutputIndex)}},
			Summary:  fmt.Sprintf("%s: output %d of image asset %s", strings.ToLower(title), x.OutputIndex, x.AssetID),
		}
		return a.paidImage(ctx, j, func() (string, error) {
			return a.submitImage(ctx, "/v2/studio/image/"+endpoint, map[string]any{"asset_id": x.AssetID, "resource_key": o.Key})
		})
	})
}

// ---- free reads ----

func listImageAssets(ctx context.Context, a *app, x struct {
	PageNum  int `json:"page_num"`
	PageSize int `json:"page_size"`
}) (reply, error) {
	page, size := x.PageNum, x.PageSize
	if page == 0 {
		page = 1
	}
	if size == 0 {
		size = 20
	}
	if page < 1 || size < 1 || size > 100 {
		return reply{}, errors.New("page_num must be 1 or more and page_size 1 to 100")
	}
	var res struct {
		Assets []imageAsset `json:"assets"`
	}
	if err := a.call(ctx, "POST", "/v2/studio/image/image_assets", map[string]int{"page_num": page, "page_size": size}, &res); err != nil {
		return reply{}, err
	}
	r := reply{Card: &card{Kind: "images", Title: "Image assets"}}
	if len(res.Assets) == 0 {
		r.Text = fmt.Sprintf("No image assets on page %d.", page)
		return r, nil
	}
	r.Card.Summary = fmt.Sprintf("page %d · %d assets", page, len(res.Assets))
	r.line("Image assets, page %d (%d):", page, len(res.Assets))
	var urls []string
	for _, s := range res.Assets {
		prompt, _ := s.Input["prompt"].(string)
		first := ""
		if len(s.Output.Data) > 0 {
			first = s.Output.Data[0].URL
		}
		urls = append(urls, first)
		label := s.Type + " · " + s.Status
		if prompt != "" {
			label += " · " + clip(cleanPrompt(prompt), 60)
		}
		r.Card.Images = append(r.Card.Images, cardImage{Label: label, ID: s.ID})
		line := fmt.Sprintf("- %s  %s  %s  %d output(s)", s.ID, s.Type, s.Status, len(s.Output.Data))
		if prompt != "" {
			line += fmt.Sprintf("  %q", clip(cleanPrompt(prompt), 60))
		}
		r.line("%s", line)
	}
	r.meta("images", urls)
	if len(res.Assets) == size {
		r.Card.More = fmt.Sprintf("page_num=%d", page+1)
		r.line("More: page_num=%d", page+1)
	}
	return r, nil
}

type getAssetArgs struct {
	AssetID     string `json:"asset_id"`
	WaitSeconds int    `json:"wait_seconds"`
	DownloadTo  string `json:"download_to"`
}

func getImageAsset(ctx context.Context, a *app, x getAssetArgs) (reply, error) {
	r := reply{Card: &card{Kind: "images", Title: "Image asset"}}
	timeout := time.Duration(0)
	if x.WaitSeconds > 0 || x.DownloadTo != "" {
		timeout = seconds(x.WaitSeconds, 900)
	}
	s, err := a.waitImage(ctx, x.AssetID, timeout)
	if err != nil {
		return r, err
	}
	r.Card.Status = s.Status
	r.line("Image asset %s: %s, %s, %d output(s).", s.ID, s.Type, s.Status, len(s.Output.Data))
	for _, k := range []string{"model_version", "prompt", "amount", "resolution", "scale", "template_id"} {
		if v, ok := s.Input[k]; ok && v != nil && v != "" {
			text := fmt.Sprint(v)
			if k == "prompt" {
				text = clip(cleanPrompt(text), 200)
			}
			r.line("%s: %s", k, text)
		}
	}
	if s.Status != "success" {
		if s.done() {
			r.Card.Status = "failed"
		}
		return r, nil
	}
	r.Card.Status = "done"
	showImages(&r, s)
	if x.DownloadTo != "" {
		paths, err := a.saveImages(ctx, s, x.DownloadTo)
		r.Card.Saved = paths
		if len(paths) > 0 {
			r.Card.Status = "saved"
			r.line("Saved %d image(s):\n%s", len(paths), strings.Join(paths, "\n"))
		}
		return r, err
	}
	r.line("Next: tripo_get_image_asset with download_to=<folder> saves the images.")
	return r, nil
}

func listImageTemplates(ctx context.Context, a *app, x struct {
	Tag   string `json:"tag"`
	Query string `json:"query"`
}) (reply, error) {
	var res struct {
		Templates []struct {
			ID          string   `json:"template_id"`
			Title       string   `json:"title"`
			Description string   `json:"description"`
			Tags        []string `json:"tag"`
			Secondary   []string `json:"secondary_tag"`
		} `json:"templates"`
	}
	if err := a.call(ctx, "GET", "/v2/studio/image/available_templates", nil, &res); err != nil {
		return reply{}, err
	}
	var r reply
	n := 0
	q := strings.ToLower(x.Query)
	for _, t := range res.Templates {
		tags := append(append([]string{}, t.Tags...), t.Secondary...)
		hay := strings.ToLower(t.Title + "\n" + t.Description + "\n" + strings.Join(tags, "\n"))
		if x.Tag != "" && !containsFold(t.Tags, x.Tag) || q != "" && !strings.Contains(hay, q) {
			continue
		}
		n++
		desc, _, _ := strings.Cut(strings.ReplaceAll(t.Description, `\n`, "\n"), "\n")
		r.line("- %s  %q  [%s]  %s", t.ID, t.Title, strings.Join(tags, ", "), clip(desc, 100))
	}
	if n == 0 {
		return say(fmt.Sprintf("No templates match (of %d).", len(res.Templates))), nil
	}
	r.Text = fmt.Sprintf("%d of %d templates (pass the id as template_id):\n", n, len(res.Templates)) + r.Text
	return r, nil
}

func containsFold(list []string, s string) bool {
	for _, e := range list {
		if strings.EqualFold(e, s) {
			return true
		}
	}
	return false
}
