package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Smart UV: a candidate UV layout is generated beside the current version and
// replaces it only when applied. Port of model.uv_generate / model.uv_apply and
// the context read from tripo-studio-plugin 0.3.12 (src/ops/studio-extras.mjs).

const uvMaxTriangles = 80000 // Studio's limit: 80K triangles, i.e. 40K original quads

func init() {
	register(
		tool{
			Name:        "tripo_get_uv_context",
			Description: "Smart UV state of a project: the next action (generate, or retry once a candidate exists), a running task, and the candidates generated so far. download_to saves a candidate's model and its UV layout image (candidate defaults to the newest). Free.",
			InputSchema: object([]string{"project_id"}, props{
				"project_id":  str("Project id."),
				"candidate":   str("With download_to: the candidate's operator_id. Default the newest."),
				"download_to": str("Save the candidate's model (and <name>-layout image) to this file or folder."),
			}),
			run: handler(getUVContext),
		},
		tool{
			Name: "tripo_generate_uv",
			Description: "PAID, 20 credits (Studio key UvEditGenerate; a retry is UvEditRetry, 10), but Studio gives a free quota of Smart UV generations per account (see tripo_balance, 'uv_edit.generate'); Studio's own page charges nothing while it lasts. Generates a candidate UV layout without touching the model; look at it, then keep it with tripo_apply_uv. " +
				"Allowed for models up to 80K triangles (40K quads), not segmented ones. The action is chosen by Studio from the project's state (generate first, then retry). Submits at once; download_to waits and saves the candidate model and its layout image.",
			InputSchema: object([]string{"project_id"}, followProps, props{
				"project_id": str("Project to unwrap."),
				"action":     enum("Optional check: must equal the project's next action (see tripo_get_uv_context).", "generate", "retry"),
			}),
			UI:  true,
			run: handler(generateUV),
		},
		tool{
			Name:        "tripo_apply_uv",
			Description: "Make a Smart UV candidate the project's current version (candidate_operator_id from tripo_get_uv_context or tripo_generate_uv). No credits. dry_run=true only checks the candidate.",
			InputSchema: object([]string{"project_id", "candidate_operator_id"}, props{
				"project_id":            str("Project id."),
				"candidate_operator_id": str("The candidate to apply."),
				"dry_run":               boolean("Check that the candidate can be applied; change nothing."),
			}),
			UI:  true,
			run: handler(applyUV),
		},
	)
}

type uvCandidate struct {
	OperatorID string  `json:"operator_id"`
	CreatedAt  float64 `json:"created_at"`
	Model      struct {
		URL string `json:"url"`
	} `json:"model"`
	Layout struct {
		URL string `json:"url"`
	} `json:"uv_layout"`
}

type uvState struct {
	ProjectID         string        `json:"project_id"`
	CurrentOperatorID string        `json:"current_operator_id"`
	NextAction        string        `json:"next_action"`
	Candidates        []uvCandidate `json:"candidates"`
	RunningTask       *struct {
		OperatorID string `json:"operator_id"`
		Action     string `json:"action"`
		Status     string `json:"status"`
	} `json:"running_task"`
}

func (s uvState) candidate(op string) *uvCandidate {
	for i := range s.Candidates {
		if s.Candidates[i].OperatorID == op {
			return &s.Candidates[i]
		}
	}
	return nil
}

func (s uvState) newest() *uvCandidate {
	if len(s.Candidates) == 0 {
		return nil
	}
	c := &s.Candidates[0]
	for i := range s.Candidates {
		if s.Candidates[i].CreatedAt > c.CreatedAt {
			c = &s.Candidates[i]
		}
	}
	return c
}

// uvStamp reads created_at as seconds or milliseconds since the epoch.
func uvStamp(f float64) string {
	if f <= 0 {
		return "?"
	}
	if f > 1e11 {
		f /= 1000
	}
	return time.Unix(int64(f), 0).Local().Format("2006-01-02 15:04")
}

// uvLoad reads the Smart UV context of the project's current version.
func (a *app) uvLoad(ctx context.Context, p *project) (uvState, error) {
	var s uvState
	if p.OperatorID == "" {
		return s, errors.New("the project has no current version (operator id) yet")
	}
	body := map[string]any{"project_id": p.ID, "current_operator_id": p.OperatorID}
	if err := a.call(ctx, "POST", "/v2/studio/operation/uv_edit/context", body, &s); err != nil {
		return s, err
	}
	if s.ProjectID != p.ID || s.CurrentOperatorID != p.OperatorID {
		return s, errors.New("Tripo returned the Smart UV context of another project or version")
	}
	return s, nil
}

// uvFreeQuota reads how many free Smart UV generations are left.
func (a *app) uvFreeQuota(ctx context.Context) (left, total int, ok bool) {
	var q struct {
		Quota struct {
			UV struct {
				Generate *struct {
					Remaining int `json:"remaining"`
					Total     int `json:"total_count"`
				} `json:"generate"`
			} `json:"uv_edit"`
		} `json:"quota"`
	}
	if a.call(ctx, "POST", "/v2/studio/operation/quota", map[string]any{}, &q) != nil || q.Quota.UV.Generate == nil {
		return 0, 0, false
	}
	return q.Quota.UV.Generate.Remaining, q.Quota.UV.Generate.Total, true
}

func uvPriceKey(action string) string {
	if action == "retry" {
		return "UvEditRetry"
	}
	return "UvEditGenerate"
}

func getUVContext(ctx context.Context, a *app, x struct {
	ProjectID  string `json:"project_id"`
	Candidate  string `json:"candidate"`
	DownloadTo string `json:"download_to"`
}) (reply, error) {
	p, err := a.project(ctx, x.ProjectID, "")
	if err != nil {
		return reply{}, err
	}
	s, err := a.uvLoad(ctx, p)
	if err != nil {
		return reply{}, err
	}
	var r reply
	r.line("Project %s %q, current version %s. Next Smart UV action: %s.", p.ID, p.Name, p.OperatorID, s.NextAction)
	if left, total, ok := a.uvFreeQuota(ctx); ok {
		r.line("Free Smart UV generations left: %d of %d.", left, total)
	}
	if s.RunningTask != nil {
		r.line("Running: %s %s (operator_id %s). Wait for it with tripo_wait operator_id=%s.", s.RunningTask.Action, s.RunningTask.Status, s.RunningTask.OperatorID, s.RunningTask.OperatorID)
	}
	sort.Slice(s.Candidates, func(i, j int) bool { return s.Candidates[i].CreatedAt > s.Candidates[j].CreatedAt })
	if len(s.Candidates) == 0 {
		r.line("No candidates yet: tripo_generate_uv creates one.")
	}
	for _, c := range s.Candidates {
		r.line("- candidate %s, created %s", c.OperatorID, uvStamp(c.CreatedAt))
	}
	if len(s.Candidates) > 0 {
		r.line("Keep one with tripo_apply_uv candidate_operator_id=<id>.")
	}
	if x.DownloadTo == "" {
		return r, nil
	}
	c := s.newest()
	if x.Candidate != "" {
		if c = s.candidate(x.Candidate); c == nil {
			return r, fmt.Errorf("%s is not a candidate of this project", x.Candidate)
		}
	}
	if c == nil {
		return r, errors.New("there is no candidate to save")
	}
	files, err := a.uvSave(ctx, p, *c, x.DownloadTo, &r)
	if err == nil {
		r.line("Saved candidate %s: %s", c.OperatorID, strings.Join(files, ", "))
	}
	return r, err
}

// uvSave saves a candidate's model, and its UV layout image next to it.
func (a *app) uvSave(ctx context.Context, p *project, c uvCandidate, dest string, r *reply) ([]string, error) {
	if c.Model.URL == "" {
		return nil, errors.New("the candidate has no model file yet")
	}
	name := p.Name
	if name == "" {
		name = p.ID
	}
	base := name + "-uv-" + c.OperatorID[:min(8, len(c.OperatorID))]
	path, err := outputPath(dest, base, ".glb")
	if err != nil {
		return nil, err
	}
	final, _, _, err := a.save(ctx, c.Model.URL, path)
	if err != nil {
		return nil, err
	}
	saved := []string{final}
	if c.Layout.URL != "" {
		ext := ".png"
		if u, err := url.Parse(c.Layout.URL); err == nil && filepath.Ext(u.Path) != "" {
			ext = filepath.Ext(u.Path)
		}
		layout := strings.TrimSuffix(final, filepath.Ext(final)) + "-layout" + ext
		if final, _, _, err = a.save(ctx, c.Layout.URL, layout); err != nil {
			return saved, err
		}
		saved = append(saved, final)
	}
	if r.Card != nil {
		r.Card.Status, r.Card.Saved = "saved", saved
	}
	return saved, nil
}

// ---- eligibility ----

// glbTriangles counts the triangles a GLB's JSON chunk declares, the way the
// plugin's inspector does (accessor counts; meshopt compression keeps them).
func glbTriangles(js []byte) int {
	var doc struct {
		Accessors []struct {
			Count int `json:"count"`
		} `json:"accessors"`
		Meshes []struct {
			Primitives []struct {
				Attributes map[string]int `json:"attributes"`
				Indices    *int           `json:"indices"`
				Mode       *int           `json:"mode"`
			} `json:"primitives"`
		} `json:"meshes"`
	}
	if json.Unmarshal(bytes.TrimRight(js, " \x00"), &doc) != nil {
		return 0
	}
	total := 0
	for _, m := range doc.Meshes {
		for _, p := range m.Primitives {
			idx := -1
			if p.Indices != nil {
				idx = *p.Indices
			} else if v, ok := p.Attributes["POSITION"]; ok {
				idx = v
			}
			if idx < 0 || idx >= len(doc.Accessors) {
				continue
			}
			count := doc.Accessors[idx].Count
			mode := 4
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
	}
	return total
}

// uvTriangles reads the triangle count from the start of the project's model
// file. known is false for an FBX (quad Smart Mesh) model, which Studio checks itself.
func (a *app) uvTriangles(ctx context.Context, p *project) (n int, known bool, err error) {
	if p.ModelURL == "" {
		return 0, false, errors.New("Smart UV needs a model file; the project has none yet")
	}
	body, err := a.open(ctx, p.ModelURL)
	if err != nil {
		return 0, false, err
	}
	defer body.Close()
	head := make([]byte, 20)
	if _, err := io.ReadFull(body, head); err != nil || string(head[:4]) != "glTF" {
		return 0, false, nil
	}
	size := binary.LittleEndian.Uint32(head[12:])
	if binary.LittleEndian.Uint32(head[16:]) != 0x4E4F534A || size > 64<<20 {
		return 0, false, nil
	}
	js := make([]byte, size)
	if _, err := io.ReadFull(body, js); err != nil {
		return 0, false, nil
	}
	n = glbTriangles(js)
	return n, n > 0, nil
}

// ---- generate ----

type uvGenArgs struct {
	ProjectID string `json:"project_id"`
	Action    string `json:"action"`
	followArgs
}

func generateUV(ctx context.Context, a *app, x uvGenArgs) (reply, error) {
	if x.Action != "" && x.Action != "generate" && x.Action != "retry" {
		return reply{}, errors.New("action must be generate or retry")
	}
	p, err := a.preflight(ctx, x.ProjectID)
	if err != nil {
		return reply{}, err
	}
	if p.is("is_segmented") {
		return reply{}, errors.New("Smart UV does not support segmented projects")
	}
	s, err := a.uvLoad(ctx, p)
	if err != nil {
		return reply{}, err
	}
	if s.RunningTask != nil {
		return reply{}, fmt.Errorf("a Smart UV task is already running (operator_id %s); wait for it and use tripo_get_uv_context", s.RunningTask.OperatorID)
	}
	if s.NextAction != "generate" && s.NextAction != "retry" {
		return reply{}, fmt.Errorf("Studio offers no Smart UV action for this project (next_action %q)", s.NextAction)
	}
	if x.Action != "" && x.Action != s.NextAction {
		return reply{}, fmt.Errorf("the project's next Smart UV action is %s, not %s", s.NextAction, x.Action)
	}
	tris, known, err := a.uvTriangles(ctx, p)
	if err != nil {
		return reply{}, err
	}
	if known && tris > uvMaxTriangles {
		return reply{}, fmt.Errorf("Smart UV allows at most 80K triangles (40K original quads); this model has %s triangles", thousands(tris))
	}
	summary := fmt.Sprintf("Smart UV %s on %s", s.NextAction, x.ProjectID)
	settings := [][2]string{{"Action", s.NextAction}}
	if known {
		settings = append(settings, [2]string{"Triangles", thousands(tris)})
	}
	if left, total, ok := a.uvFreeQuota(ctx); ok {
		settings = append(settings, [2]string{"Free quota", fmt.Sprintf("%d of %d left", left, total)})
		if left > 0 {
			summary += fmt.Sprintf(". Free quota left: %d of %d; Studio's page charges nothing while it lasts, so the price below is the most it can cost.", left, total)
		}
	}
	f := x.followArgs
	f.DownloadTo = ""
	j := job{
		Tool: "tripo_generate_uv", Title: "Smart UV " + s.NextAction, Keys: []string{uvPriceKey(s.NextAction)}, ProjectID: x.ProjectID, Follow: f,
		Settings: settings, Summary: summary,
	}
	r, err := a.paid(ctx, j, func() (receipt, error) {
		return a.submit(ctx, "/v2/studio/operation/uv_edit/generate", map[string]any{
			"project_id": x.ProjectID, "current_operator_id": p.OperatorID, "action": s.NextAction,
		})
	})
	if err != nil || x.DryRun {
		return r, err
	}
	dropNext(&r)
	op := r.Card.OperatorID
	if x.DownloadTo == "" {
		r.line("Next: tripo_wait operator_id=%s, then tripo_get_uv_context project_id=%s and tripo_apply_uv. Add download_to to save the candidate here.", op, x.ProjectID)
		return r, nil
	}
	if ok, err := a.settleJob(ctx, &r, op, "", seconds(x.WaitSeconds, 900)); err != nil || !ok {
		return r, err
	}
	// The finished candidate shows up in the context, with its files.
	cur, err := a.project(ctx, x.ProjectID, "")
	if err != nil {
		return r, err
	}
	st, err := a.uvLoad(ctx, cur)
	if err != nil {
		return r, err
	}
	c := st.candidate(op)
	if c == nil {
		r.line("The job finished but its candidate is not listed yet: run tripo_get_uv_context project_id=%s download_to=%s in a moment.", x.ProjectID, x.DownloadTo)
		return r, nil
	}
	files, err := a.uvSave(ctx, cur, *c, x.DownloadTo, &r)
	if err != nil {
		return r, err
	}
	r.line("Done. Candidate %s saved: %s. Keep it with tripo_apply_uv candidate_operator_id=%s.", op, strings.Join(files, ", "), op)
	a.logJob(jobRecord{OperatorID: op, ProjectID: x.ProjectID, Status: "done", Saved: files[0]})
	return r, nil
}

// ---- apply ----

type uvApplyArgs struct {
	ProjectID           string `json:"project_id"`
	CandidateOperatorID string `json:"candidate_operator_id"`
	DryRun              bool   `json:"dry_run"`
}

func applyUV(ctx context.Context, a *app, x uvApplyArgs) (reply, error) {
	if x.CandidateOperatorID == "" {
		return reply{}, errors.New("candidate_operator_id is required")
	}
	p, err := a.preflight(ctx, x.ProjectID)
	if err != nil {
		return reply{}, err
	}
	s, err := a.uvLoad(ctx, p)
	if err != nil {
		return reply{}, err
	}
	if s.RunningTask != nil {
		return reply{}, errors.New("Smart UV is still processing; wait for it first")
	}
	if s.candidate(x.CandidateOperatorID) == nil {
		return reply{}, fmt.Errorf("%s is not a Smart UV candidate of this project; see tripo_get_uv_context", x.CandidateOperatorID)
	}
	r := reply{Card: &card{Kind: "job", Title: "Apply Smart UV", ProjectID: x.ProjectID, OperatorID: x.CandidateOperatorID,
		Summary: fmt.Sprintf("apply Smart UV candidate %s to %s", x.CandidateOperatorID, x.ProjectID)}}
	if x.DryRun {
		r.Card.Status = "quote"
		r.Text = "Candidate " + x.CandidateOperatorID + " can be applied to " + x.ProjectID + ". Dry run: nothing changed."
		return r, nil
	}
	var rc struct {
		ProjectID string `json:"project_id"`
		Previous  string `json:"previous_operator_id"`
		CurrentOp string `json:"current_operator_id"`
	}
	body := map[string]any{"project_id": x.ProjectID, "candidate_operator_id": x.CandidateOperatorID, "current_operator_id": p.OperatorID}
	if err := a.call(ctx, "POST", "/v2/studio/operation/uv_edit/apply", body, &rc); err != nil {
		r.Card.Status = "failed"
		return r, fmt.Errorf("%w (if unsure whether it applied, check tripo_project)", err)
	}
	if rc.ProjectID != x.ProjectID || rc.Previous != p.OperatorID || rc.CurrentOp != x.CandidateOperatorID {
		return r, errors.New("Tripo's apply receipt does not match the request; check tripo_project before trying again")
	}
	r.Card.Status = "done"
	r.line("Applied: candidate %s is now the current version of %s (was %s). No credits.", rc.CurrentOp, x.ProjectID, rc.Previous)
	a.logJob(jobRecord{Tool: "tripo_apply_uv", Title: "Apply Smart UV", ProjectID: x.ProjectID, OperatorID: rc.CurrentOp, Status: "done"})
	if np, err := a.project(ctx, x.ProjectID, ""); err == nil {
		a.showProject(&r, np)
	}
	return r, nil
}
