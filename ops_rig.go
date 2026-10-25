package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Rigging, animation presets and AI motion. Port of model.rig, model.animate,
// motion.generate and model.apply_motion from tripo-studio-plugin 0.3.12
// (src/ops/postops.mjs, src/ops/studio-extras.mjs, src/ops/animation-presets.mjs).

const (
	rigV1  = "v1.0-20240301"
	rigV25 = "v2.5-20260210"
	rigV3  = "v3.0-20260909"
)

var skeletonPresets = []string{"actorcore", "mixamo", "unreal", "vrm", "unity"}

// Studio's price config has no key for retargeting, so its cost is reported as
// unknown (add "Retarget" to prices.json to change that).
const retargetKey = "Retarget"

func init() {
	register(
		tool{
			Name: "tripo_rig_model",
			Description: "PAID, 20 credits (Studio price key Rigging, the same for every version): auto-rig a textured project with a skeleton. Runs Studio's free rig precheck first (is the model riggable, biped or an animal) and refuses a model Studio would reject; a biped needs a clear T-pose. " +
				"model_version: v3.0-20260909 (default; humanoid with a skeleton_preset, or rigging_type=other for the animal rig the precheck found), v2.5-20260210 or v1.0-20240301 (legacy; Studio forces a biped to v1.0). skeleton_preset (actorcore default, mixamo, unreal, vrm, unity) works only with v3 humanoid. Submits at once; download_to waits and saves the rigged model.",
			InputSchema: object([]string{"project_id"}, followProps, props{
				"project_id":      str("Project to rig (it must be textured)."),
				"model_version":   enum("Default v3.0-20260909.", rigV3, rigV25, rigV1),
				"rigging_type":    enum("v3 only: humanoid (default, needs a biped) or other (the detected animal rig).", "humanoid", "other"),
				"skeleton_preset": enum("v3 humanoid only. Default actorcore.", skeletonPresets...),
			}),
			UI:  true,
			run: handler(rigModel),
		},
		tool{
			Name:        "tripo_animate_model",
			Description: "PAID (price not in Studio's config: reported as unknown, ask the owner first): put one of Studio's preset animations on an already rigged project (retarget_model). animation is a preset id such as preset:biped:walk, or just the name (walk) with rig_type; list them with tripo_list_animation_presets. The result lives in the project as an animation: download_to waits and saves that animation file; tripo_export_model with_animation=true exports it with the skeleton. Submits at once.",
			InputSchema: object([]string{"project_id", "animation"}, followProps, props{
				"project_id": str("A rigged project."),
				"animation":  str("Preset id (preset:<rig_type>:<name>) or a bare name such as walk."),
				"rig_type":   str("The project's rig type (aquatic, avian, biped, hexapod, octopod, quadruped, serpentine). Default: taken from the preset id, else biped."),
			}),
			UI:  true,
			run: handler(animateModel),
		},
		tool{
			Name:        "tripo_list_animation_presets",
			Description: "Studio's preset animations for tripo_animate_model, as preset:<rig_type>:<name> (static list, free). Filter by rig_type and/or a name substring.",
			InputSchema: object(nil, props{
				"rig_type": str("aquatic, avian, biped, hexapod, octopod, quadruped or serpentine."),
				"query":    str("Substring of the preset id, e.g. dance."),
			}),
			run: handler(listAnimationPresets),
		},
		tool{
			Name: "tripo_generate_motion",
			Description: "PAID, 20 credits (Studio price key Motion; whether it grows with stages or seconds is not verified): generate an AI motion from text, 1 to 5 stages of 1-10 seconds. Give segments (each prompt + duration_seconds, optionally waypoints [[x,z],[x,z]] that must join the next stage's start), or just prompt + duration_seconds for one stage. " +
				"The motion is a reusable asset: apply it to a rigged biped with tripo_apply_motion. Submits at once and returns a task_id; wait_seconds / download_to wait for it here, or use tripo_get_motion task_id=... later.",
			InputSchema: object(nil, props{
				"dry_run":          boolean("Check the inputs and return the cost; nothing is submitted or charged."),
				"wait_seconds":     integer("Wait this long for the motion (max 3600). Default: do not wait, or 900 when download_to is set."),
				"download_to":      str("Wait for the motion, then save its file to this file or folder."),
				"prompt":           str("One-stage shortcut: what the character does."),
				"duration_seconds": integer("One-stage shortcut: 1-10 seconds. Default 3."),
				"segments": props{
					"type": "array", "minItems": 1, "maxItems": 5, "description": "Stages, played in order.",
					"items": object([]string{"prompt", "duration_seconds"}, props{
						"prompt":           str("What the character does in this stage (up to 1000 characters)."),
						"duration_seconds": integer("1-10 seconds."),
						"waypoints":        props{"type": "array", "minItems": 2, "maxItems": 2, "items": props{"type": "array", "minItems": 2, "maxItems": 2, "items": props{"type": "number"}}, "description": "Optional start and end position [[x,z],[x,z]] on the ground plane."},
					}),
				},
			}),
			UI:  true,
			run: handler(generateMotion),
		},
		tool{
			Name:        "tripo_apply_motion",
			Description: "PAID (price not in Studio's config: reported as unknown, ask the owner first): retarget a generated motion (motion_asset_id from tripo_generate_motion / tripo_list_motions) onto a rigged biped project. The result is stored in the project as an animation: download_to waits and saves that animation file; tripo_export_model with_animation=true exports it with the skeleton. Submits at once.",
			InputSchema: object([]string{"project_id", "motion_asset_id"}, followProps, props{
				"project_id":      str("A rigged biped project."),
				"motion_asset_id": str("A finished motion asset."),
			}),
			UI:  true,
			run: handler(applyMotion),
		},
		tool{
			Name:        "tripo_list_motions",
			Description: "List your generated motion assets and the motion tasks still running. Free.",
			InputSchema: object(nil),
			run:         handler(listMotions),
		},
		tool{
			Name:        "tripo_get_motion",
			Description: "Show one motion asset (motion_asset_id) or follow a motion task (task_id from tripo_generate_motion; wait_seconds waits for it, default checks once). download_to saves the motion file. Free.",
			InputSchema: object(nil, props{
				"motion_asset_id": str("A finished motion asset."),
				"task_id":         str("A motion task_id, to wait for its asset."),
				"wait_seconds":    integer("With task_id: how long to wait (0-3600). Default 0 (check once), or 900 when download_to is set."),
				"download_to":     str("Save the motion file to this file or folder."),
			}),
			UI:  true,
			run: handler(getMotion),
		},
	)
}

// ---- shared pieces ----

// flagFalse is true when Tripo states the flag and it is false.
func flagFalse(p *project, name string) bool { f := p.flag(name); return f != nil && !*f }

// projectRigType is the rig type Studio recorded for the project, or "".
func projectRigType(p *project) string {
	op, _ := p.raw["operator"].(map[string]any)
	meta, _ := op["metadata"].(map[string]any)
	rig, _ := op["rigging"].(map[string]any)
	for _, v := range []any{rig["type"], op["rig_type"], p.raw["rig_type"], meta["rig_type"]} {
		if s := asString(v); s != "" {
			return s
		}
	}
	return ""
}

// dropNext removes paid()'s closing "Next: tripo_wait ..." line when a tool
// follows the job itself.
func dropNext(r *reply) {
	if i := strings.LastIndex(r.Text, "\nNext: "); i >= 0 {
		r.Text = r.Text[:i]
	}
}

// settleJob waits for a job. It returns true when the job succeeded; a job
// that is still running is reported on r and is not an error.
func (a *app) settleJob(ctx context.Context, r *reply, op, projectID string, timeout time.Duration) (bool, error) {
	it, err := a.wait(ctx, op, timeout)
	if err != nil {
		return false, err
	}
	r.Card.OperatorID, r.Card.Progress = op, it.Progress
	switch it.Status {
	case "success":
		r.Card.Status = "done"
		return true, nil
	case "failed", "cancelled", "expired", "banned":
		r.Card.Status = "failed"
		a.logJob(jobRecord{OperatorID: op, Status: it.Status})
		reason := ""
		if it.Reason != nil {
			reason = fmt.Sprintf(": %v", it.Reason)
		}
		return false, fmt.Errorf("job %s ended as %s%s", op, it.Status, reason)
	}
	r.Card.Status = "running"
	next := "tripo_wait operator_id=" + op
	if projectID != "" {
		next += " project_id=" + projectID
	}
	r.line("Still %s (%.0f%%) after %s. Next: %s", it.Status, it.Progress, timeout, next)
	return false, nil
}

// ---- rigging ----

type rigArgs struct {
	ProjectID      string `json:"project_id"`
	ModelVersion   string `json:"model_version"`
	RiggingType    string `json:"rigging_type"`
	SkeletonPreset string `json:"skeleton_preset"`
	followArgs
}

type rigPlan struct {
	Version, RigType, Spec, Note string
}

// planRig applies the plugin's version and skeleton rules to the precheck
// answer (riggable, detected rig type).
func planRig(requested, riggingType, preset string, riggable bool, detected string) (rigPlan, error) {
	humanoidV3 := requested == rigV3 && riggingType == "humanoid"
	if preset != "" && !humanoidV3 {
		return rigPlan{}, errors.New("skeleton_preset needs model_version v3.0-20260909 with rigging_type humanoid")
	}
	if !riggable {
		if detected == "biped" {
			return rigPlan{}, errors.New("Studio's rig precheck rejected this biped: Auto Rig needs a clear T-pose")
		}
		return rigPlan{}, errors.New("Studio's rig precheck says this model is not riggable")
	}
	if humanoidV3 && detected != "biped" {
		return rigPlan{}, fmt.Errorf("the humanoid rig needs a biped, but the precheck found %q; use rigging_type=other for that rig", detected)
	}
	var plan rigPlan
	switch {
	case detected == "others" || detected == "":
		return rigPlan{}, errors.New("Studio's rig precheck did not identify a supported rig type")
	case requested == rigV3:
		plan.Version = rigV3
	case detected == "biped":
		plan.Version = rigV1
		if requested != rigV1 {
			plan.Note = "Studio forces a biped to rig version " + rigV1 + "."
		}
	default:
		plan.Version = requested
	}
	plan.RigType = detected
	if humanoidV3 {
		plan.RigType, plan.Spec = "biped", preset
		if plan.Spec == "" {
			plan.Spec = "actorcore"
		}
	}
	return plan, nil
}

func rigLabel(v string) string {
	return map[string]string{rigV1: "v1.0", rigV25: "v2.5", rigV3: "v3.0"}[v]
}

func rigModel(ctx context.Context, a *app, x rigArgs) (reply, error) {
	version := x.ModelVersion
	if version == "" {
		version = rigV3
	}
	if version != rigV1 && version != rigV25 && version != rigV3 {
		return reply{}, fmt.Errorf("model_version must be %s, %s or %s", rigV1, rigV25, rigV3)
	}
	kind := x.RiggingType
	if kind == "" {
		kind = "humanoid"
	}
	if kind != "humanoid" && kind != "other" {
		return reply{}, errors.New("rigging_type must be humanoid or other")
	}
	if x.SkeletonPreset != "" && !rigHas(skeletonPresets, x.SkeletonPreset) {
		return reply{}, fmt.Errorf("skeleton_preset must be one of %s", strings.Join(skeletonPresets, ", "))
	}
	if x.SkeletonPreset != "" && (version != rigV3 || kind != "humanoid") {
		return reply{}, errors.New("skeleton_preset needs model_version v3.0-20260909 with rigging_type humanoid")
	}
	p, err := a.preflight(ctx, x.ProjectID)
	if err != nil {
		return reply{}, err
	}
	if flagFalse(p, "is_textured") {
		return reply{}, errors.New("rigging needs a textured project: run tripo_texture first")
	}
	var pre struct {
		Riggable bool   `json:"riggable"`
		RigType  string `json:"rig_type"`
	}
	if err := a.call(ctx, "POST", "/v2/studio/operation/pre_rig_check", map[string]any{"model_version": version, "project_id": x.ProjectID}, &pre); err != nil {
		return reply{}, fmt.Errorf("rig precheck: %w", err)
	}
	plan, err := planRig(version, kind, x.SkeletonPreset, pre.Riggable, pre.RigType)
	if err != nil {
		return reply{}, err
	}
	body := map[string]any{"model_version": plan.Version, "project_id": x.ProjectID, "rig_type": plan.RigType}
	settings := [][2]string{{"Version", rigLabel(plan.Version)}, {"Rig", plan.RigType}}
	if plan.Spec != "" {
		body["spec"] = plan.Spec
		settings = append(settings, [2]string{"Skeleton", plan.Spec})
	}
	summary := fmt.Sprintf("auto-rig %s (%s rig) on %s", rigLabel(plan.Version), plan.RigType, x.ProjectID)
	if plan.Spec != "" {
		summary = fmt.Sprintf("auto-rig %s (biped, %s skeleton) on %s", rigLabel(plan.Version), plan.Spec, x.ProjectID)
	}
	if plan.Note != "" {
		summary += ". " + plan.Note
	}
	if p.is("is_rigged") {
		summary += " The project is already rigged; this rigs it again."
	}
	j := job{
		Tool: "tripo_rig_model", Title: "Rigging " + rigLabel(plan.Version), Keys: []string{"Rigging"}, ProjectID: x.ProjectID, Follow: x.followArgs,
		Settings: settings, Summary: summary,
	}
	return a.paid(ctx, j, func() (receipt, error) {
		return a.submit(ctx, "/v2/studio/operation/rigging_model", body)
	})
}

func rigHas(list []string, s string) bool {
	for _, e := range list {
		if e == s {
			return true
		}
	}
	return false
}

// ---- animation presets ----

// animationPresets mirrors the Studio client's catalog (src/ops/animation-presets.mjs).
var animationPresets = map[string][]string{
	"aquatic": {"march"},
	"avian":   {},
	"biped": {"afraid", "agree", "angry_01", "angry_02", "angry_03", "basketball_shot", "bow", "box_01", "box_02", "box_03",
		"cast_a_spell", "cheer", "chop", "clap", "climb", "complain_01", "complain_02", "crossover_dribble", "cry",
		"dance_01", "dance_02", "dance_03", "dance_04", "dance_05", "dance_06", "defeat_02", "defeat_03", "depressed",
		"dig", "dive", "dribble", "fall", "fire", "flee_01", "flee_02", "flip", "fold_arms", "football_catch",
		"football_save", "football_pass", "freaky", "frightened", "front_kick_01", "front_kick_02", "frustrated_01",
		"frustrated_02", "greet_01", "greet_02", "greet_03", "greet_04", "heart_pose", "hit_to_body_01", "hit_to_body_02",
		"hit_to_head", "hit_to_side", "hit_to_stomach", "hug", "idle", "jump_down", "jump", "jump_rope_01", "jump_rope_02",
		"laugh_01", "laugh_02", "lift_heavy", "look_around", "make_a_call_01", "make_a_call_02", "pitch_baseball",
		"play_mobile_game", "play_video_game", "run_upstairs", "run", "scared_01", "scared_02", "scratch", "shoot",
		"shovel", "sing_01", "sing_02", "sing_03", "sing_04", "sit", "slash", "sob", "standing_relax", "surf", "swagger",
		"swim", "turn", "volleyball", "wait", "walk", "warm_up", "wave_goodbye_01", "wave_goodbye_02"},
	"hexapod":    {"walk"},
	"octopod":    {"walk"},
	"quadruped":  {"walk"},
	"serpentine": {"march"},
}

func rigTypeNames() []string {
	names := make([]string, 0, len(animationPresets))
	for k := range animationPresets {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

var presetID = regexp.MustCompile(`^preset:([a-z]+):([a-z0-9_]+)$`)

// animationID resolves the animation argument to an exact preset id and its
// rig type: a full id, or a bare name for rigType (default biped).
func animationID(animation, rigType string) (id, rig string, err error) {
	animation = strings.TrimSpace(animation)
	if m := presetID.FindStringSubmatch(animation); m != nil {
		id, rig = animation, m[1]
		if rigType != "" && rigType != rig {
			return "", "", fmt.Errorf("rig_type %s does not match the preset %s", rigType, animation)
		}
	} else {
		rig = rigType
		if rig == "" {
			rig = "biped"
		}
		id = "preset:" + rig + ":" + animation
	}
	if !rigHas(animationPresets[rig], strings.TrimPrefix(id, "preset:"+rig+":")) {
		return "", "", fmt.Errorf("%q is not a Studio animation preset for rig type %q; see tripo_list_animation_presets", animation, rig)
	}
	return id, rig, nil
}

type presetsArgs struct {
	RigType string `json:"rig_type"`
	Query   string `json:"query"`
}

func listAnimationPresets(_ context.Context, _ *app, x presetsArgs) (reply, error) {
	if x.RigType != "" {
		if _, ok := animationPresets[x.RigType]; !ok {
			return reply{}, fmt.Errorf("rig_type must be one of %s", strings.Join(rigTypeNames(), ", "))
		}
	}
	q := strings.ToLower(strings.TrimSpace(x.Query))
	var r reply
	total := 0
	for _, rig := range rigTypeNames() {
		if x.RigType != "" && x.RigType != rig {
			continue
		}
		var ids []string
		for _, n := range animationPresets[rig] {
			if id := "preset:" + rig + ":" + n; strings.Contains(id, q) {
				ids = append(ids, id)
			}
		}
		if len(ids) > 0 {
			total += len(ids)
			r.line("%s (%d): %s", rig, len(ids), strings.Join(ids, ", "))
		}
	}
	if total == 0 {
		return say("No presets match."), nil
	}
	r.line("Use with tripo_animate_model animation=<id>. avian has no presets.")
	return r, nil
}

// ---- animate (retarget a preset) ----

type animateArgs struct {
	ProjectID string `json:"project_id"`
	Animation string `json:"animation"`
	RigType   string `json:"rig_type"`
	followArgs
}

// retargetPreflight loads a rigged project that a retarget is about to change.
func (a *app) retargetPreflight(ctx context.Context, id, rig string) (*project, error) {
	p, err := a.preflight(ctx, id)
	if err != nil {
		return nil, err
	}
	if flagFalse(p, "is_textured") {
		return nil, errors.New("animation needs a textured project")
	}
	if flagFalse(p, "is_rigged") {
		return nil, errors.New("animation needs an already rigged project: run tripo_rig_model first")
	}
	if have := projectRigType(p); have != "" && have != "others" && have != rig {
		return nil, fmt.Errorf("the project is rigged as %s, not %s", have, rig)
	}
	return p, nil
}

func animateModel(ctx context.Context, a *app, x animateArgs) (reply, error) {
	id, rig, err := animationID(x.Animation, x.RigType)
	if err != nil {
		return reply{}, err
	}
	if _, err := a.retargetPreflight(ctx, x.ProjectID, rig); err != nil {
		return reply{}, err
	}
	name := strings.TrimPrefix(id, "preset:"+rig+":")
	f := x.followArgs
	f.DownloadTo = ""
	j := job{
		Tool: "tripo_animate_model", Title: "Animation " + name, Keys: []string{retargetKey}, ProjectID: x.ProjectID, Follow: f,
		Settings: [][2]string{{"Animation", id}, {"Rig", rig}},
		Summary:  fmt.Sprintf("animation %s on %s", id, x.ProjectID),
	}
	r, err := a.paid(ctx, j, func() (receipt, error) {
		return a.submit(ctx, "/v2/studio/operation/retarget_model", map[string]any{
			"animations": []string{id}, "model_version": "default", "project_id": x.ProjectID, "rig_type": rig,
		})
	})
	if err != nil || x.DryRun {
		return r, err
	}
	return r, a.retargetFollow(ctx, &r, x.followArgs, name, "", id)
}

// retargetURL finds the animation file of a retarget in the project detail,
// by motion asset id or by preset id.
func retargetURL(p *project, motionAssetID, preset string) string {
	op, _ := p.raw["operator"].(map[string]any)
	for _, k := range []string{"Retarget", "retarget", "retarget_model"} {
		list, ok := op[k].([]any)
		if !ok {
			continue
		}
		for _, e := range list {
			m, _ := e.(map[string]any)
			hit := asString(m["name"]) == preset
			if motionAssetID != "" {
				hit = asString(m["motion_asset_id"]) == motionAssetID
			}
			if u := asString(m["model_url"]); hit && u != "" {
				return u
			}
		}
		return ""
	}
	return ""
}

// retargetFollow is the wait and download of animate / apply_motion: the
// animation file is listed in the project detail, not in its model_url.
func (a *app) retargetFollow(ctx context.Context, r *reply, f followArgs, label, motionAssetID, preset string) error {
	dropNext(r)
	if f.DownloadTo == "" {
		r.line("Next: tripo_wait operator_id=%s project_id=%s to see it finish; tripo_export_model with_animation=true exports the animation, or pass download_to next time to save the animation file.", r.Card.OperatorID, r.Card.ProjectID)
		return nil
	}
	op, pid := r.Card.OperatorID, r.Card.ProjectID
	if ok, err := a.settleJob(ctx, r, op, pid, seconds(f.WaitSeconds, 900)); err != nil || !ok {
		return err
	}
	p, err := a.project(ctx, pid, op)
	if err != nil {
		return err
	}
	src := retargetURL(p, motionAssetID, preset)
	if src == "" {
		return fmt.Errorf("the job finished but project %s lists no animation file for it yet; try tripo_export_model with_animation=true", pid)
	}
	name := p.Name
	if name == "" {
		name = p.ID
	}
	dest, err := outputPath(f.DownloadTo, name+"-"+label, ".glb")
	if err != nil {
		return err
	}
	final, n, _, err := a.save(ctx, src, dest)
	if err != nil {
		return err
	}
	r.line("Done. Saved the animation file to %s, %s.", final, size(n))
	r.Card.Status, r.Card.Saved = "saved", []string{final}
	a.logJob(jobRecord{OperatorID: op, ProjectID: pid, Status: "done", Saved: final})
	return nil
}

// ---- motion ----

type motionSegment struct {
	Prompt    string      `json:"prompt"`
	Duration  int         `json:"duration_seconds"`
	Waypoints [][]float64 `json:"waypoints,omitempty"`
}

type motionArgs struct {
	Segments    []motionSegment `json:"segments"`
	Prompt      string          `json:"prompt"`
	Duration    int             `json:"duration_seconds"`
	DryRun      bool            `json:"dry_run"`
	WaitSeconds int             `json:"wait_seconds"`
	DownloadTo  string          `json:"download_to"`
}

// motionSegments checks the stages as the plugin does: 1-5 stages of 1-10 s,
// optional waypoints that join from one stage to the next.
func motionSegments(x motionArgs) ([]motionSegment, error) {
	segs := x.Segments
	if len(segs) == 0 && x.Prompt != "" {
		d := x.Duration
		if d == 0 {
			d = 3
		}
		segs = []motionSegment{{Prompt: x.Prompt, Duration: d}}
	} else if len(segs) > 0 && (x.Prompt != "" || x.Duration != 0) {
		return nil, errors.New("pass segments, or prompt with duration_seconds, not both")
	}
	if len(segs) < 1 || len(segs) > 5 {
		return nil, errors.New("a motion needs 1 to 5 stages: pass segments, or prompt")
	}
	out := make([]motionSegment, len(segs))
	for i, s := range segs {
		s.Prompt = strings.TrimSpace(s.Prompt)
		if n := len([]rune(s.Prompt)); n < 1 || n > 1000 || strings.ContainsFunc(s.Prompt, func(r rune) bool { return r < 32 && r != '\t' && r != '\n' && r != '\r' || r == 127 }) {
			return nil, fmt.Errorf("stage %d: prompt must be 1 to 1000 plain characters", i+1)
		}
		if s.Duration < 1 || s.Duration > 10 {
			return nil, fmt.Errorf("stage %d: duration_seconds must be 1 to 10", i+1)
		}
		if s.Waypoints != nil {
			if len(s.Waypoints) != 2 || len(s.Waypoints[0]) != 2 || len(s.Waypoints[1]) != 2 {
				return nil, fmt.Errorf("stage %d: waypoints must be two [x,z] points", i+1)
			}
		}
		if i > 0 && s.Waypoints != nil && out[i-1].Waypoints != nil && (out[i-1].Waypoints[1][0] != s.Waypoints[0][0] || out[i-1].Waypoints[1][1] != s.Waypoints[0][1]) {
			return nil, fmt.Errorf("stage %d: its first waypoint must equal the previous stage's last", i+1)
		}
		out[i] = s
	}
	return out, nil
}

type motionTask struct {
	TaskID  string `json:"task_id"`
	Status  string `json:"status"`
	AssetID string `json:"asset_id"`
}

type motionAsset struct {
	AssetID   string `json:"asset_id"`
	MotionURL string `json:"motion_url"`
	Title     string `json:"title"`
	TaskID    string `json:"task_id"`
	Motion    any    `json:"motion"`
	Input     struct {
		Segments []map[string]any `json:"segments"`
	} `json:"input"`
}

func (m motionAsset) describe() string {
	var parts []string
	for _, s := range m.Input.Segments {
		parts = append(parts, fmt.Sprintf("%q %vs", s["prompt"], s["duration_seconds"]))
	}
	d := strings.Join(parts, "; ")
	if m.Title != "" && d != "" {
		return m.Title + ": " + d
	}
	return m.Title + d
}

func generateMotion(ctx context.Context, a *app, x motionArgs) (reply, error) {
	segs, err := motionSegments(x)
	if err != nil {
		return reply{}, err
	}
	total := 0
	var prompts []string
	for _, s := range segs {
		total += s.Duration
		prompts = append(prompts, fmt.Sprintf("%q (%ds)", s.Prompt, s.Duration))
	}
	j := job{
		Tool: "tripo_generate_motion", Title: "Motion", Keys: []string{"Motion"}, Follow: followArgs{DryRun: x.DryRun},
		Settings: [][2]string{{"Stages", fmt.Sprint(len(segs))}, {"Length", fmt.Sprintf("%d s", total)}},
		Summary:  fmt.Sprintf("AI motion, %d stage(s), %d s: %s", len(segs), total, strings.Join(prompts, "; ")),
	}
	r, err := a.paid(ctx, j, func() (receipt, error) { return a.submitMotion(ctx, map[string]any{"segments": segs}) })
	if err != nil || x.DryRun {
		return r, err
	}
	dropNext(&r)
	taskID := r.Card.OperatorID
	if x.DownloadTo == "" && x.WaitSeconds <= 0 {
		r.line("Next: tripo_get_motion task_id=%s wait_seconds=600 (add download_to to save the file).", taskID)
		return r, nil
	}
	return r, a.motionFollow(ctx, &r, taskID, seconds(x.WaitSeconds, 900), x.DownloadTo)
}

// submitMotion is submit() for the motion endpoint, whose answer is a task_id.
func (a *app) submitMotion(ctx context.Context, body any) (receipt, error) {
	var t motionTask
	err := a.call(ctx, "POST", "/v2/studio/motion/generate", body, &t)
	var refused *apiError
	if errors.As(err, &refused) || errors.Is(err, errAuth) || errors.Is(err, errNoSession) {
		return receipt{}, fmt.Errorf("not submitted, nothing charged: %w", err)
	}
	if err != nil {
		return receipt{}, fmt.Errorf("outcome unknown: %v. The motion may have been accepted and charged; check tripo_list_motions before submitting again", err)
	}
	if t.TaskID == "" {
		return receipt{}, errors.New("outcome unknown: Tripo accepted the motion but returned no task_id; check tripo_list_motions before submitting again")
	}
	return receipt{OperatorID: t.TaskID}, nil
}

func (a *app) motionTask(ctx context.Context, taskID string) (motionTask, error) {
	var t motionTask
	err := a.call(ctx, "POST", "/v2/studio/motion/get_task", map[string]string{"task_id": taskID}, &t)
	if err == nil && t.TaskID != "" && t.TaskID != taskID {
		err = fmt.Errorf("Tripo returned motion task %s when asked for %s", t.TaskID, taskID)
	}
	return t, err
}

func (a *app) motionAsset(ctx context.Context, assetID string) (motionAsset, error) {
	var m motionAsset
	err := a.call(ctx, "POST", "/v2/studio/motion/get_asset", map[string]string{"asset_id": assetID}, &m)
	if err == nil && m.AssetID != "" && m.AssetID != assetID {
		err = fmt.Errorf("Tripo returned motion asset %s when asked for %s", m.AssetID, assetID)
	}
	return m, err
}

// motionWait polls a motion task until it ends or the timeout passes
// (timeout 0 checks once).
func (a *app) motionWait(ctx context.Context, taskID string, timeout time.Duration) (motionTask, error) {
	deadline := time.Now().Add(timeout)
	misses := 0
	for {
		t, err := a.motionTask(ctx, taskID)
		if err == nil {
			misses = 0
			switch t.Status {
			case "success", "failed", "expired":
				return t, nil
			}
			if time.Now().After(deadline) {
				return t, nil
			}
		} else if misses++; errors.Is(err, errAuth) || errors.Is(err, errNoSession) || misses >= 5 || time.Now().After(deadline) {
			return t, err
		}
		select {
		case <-ctx.Done():
			return t, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// motionFollow waits for a motion task and reports (and optionally saves) its asset.
func (a *app) motionFollow(ctx context.Context, r *reply, taskID string, timeout time.Duration, dest string) error {
	t, err := a.motionWait(ctx, taskID, timeout)
	if err != nil {
		return err
	}
	switch t.Status {
	case "success":
	case "failed", "expired":
		r.Card.Status = "failed"
		a.logJob(jobRecord{OperatorID: taskID, Status: t.Status})
		return fmt.Errorf("motion task %s ended as %s", taskID, t.Status)
	default:
		r.Card.Status = "running"
		next := "tripo_get_motion task_id=" + taskID
		if dest != "" {
			next += " download_to=" + dest
		}
		r.line("Motion is still %s after %s. Next: %s", t.Status, timeout, next)
		return nil
	}
	if t.AssetID == "" {
		return fmt.Errorf("motion task %s succeeded but names no asset; check tripo_list_motions", taskID)
	}
	m, err := a.motionAsset(ctx, t.AssetID)
	if err != nil {
		return err
	}
	r.Card.Status = "done"
	return a.motionShow(ctx, r, m, taskID, dest)
}

func (a *app) motionShow(ctx context.Context, r *reply, m motionAsset, taskID, dest string) error {
	about := ""
	if d := m.describe(); d != "" {
		about = " (" + d + ")"
	}
	r.line("Motion asset %s%s. Apply it with tripo_apply_motion motion_asset_id=%s on a rigged biped.", m.AssetID, about, m.AssetID)
	saved := ""
	if dest != "" {
		if m.MotionURL == "" {
			return errors.New("the motion has no file URL")
		}
		ext := ".glb"
		if u, err := url.Parse(m.MotionURL); err == nil && filepath.Ext(u.Path) != "" {
			ext = filepath.Ext(u.Path)
		}
		name := m.Title
		if name == "" {
			name = "motion-" + m.AssetID
		}
		path, err := outputPath(dest, name, ext)
		if err != nil {
			return err
		}
		final, n, _, err := a.save(ctx, m.MotionURL, path)
		if err != nil {
			return err
		}
		r.line("Saved the motion file to %s, %s.", final, size(n))
		r.Card.Status, r.Card.Saved, saved = "saved", []string{final}, final
	}
	if taskID != "" {
		a.logJob(jobRecord{OperatorID: taskID, Status: "done", Saved: saved})
	}
	return nil
}

type applyMotionArgs struct {
	ProjectID     string `json:"project_id"`
	MotionAssetID string `json:"motion_asset_id"`
	followArgs
}

func applyMotion(ctx context.Context, a *app, x applyMotionArgs) (reply, error) {
	if x.MotionAssetID == "" {
		return reply{}, errors.New("motion_asset_id is required")
	}
	if _, err := a.retargetPreflight(ctx, x.ProjectID, "biped"); err != nil {
		return reply{}, err
	}
	m, err := a.motionAsset(ctx, x.MotionAssetID)
	if err != nil {
		return reply{}, err
	}
	f := x.followArgs
	f.DownloadTo = ""
	j := job{
		Tool: "tripo_apply_motion", Title: "Apply motion", Keys: []string{retargetKey}, ProjectID: x.ProjectID, Follow: f,
		Settings: [][2]string{{"Motion", x.MotionAssetID}, {"Rig", "biped"}},
		Summary:  fmt.Sprintf("apply motion %s %s to %s", x.MotionAssetID, "("+m.describe()+")", x.ProjectID),
	}
	r, err := a.paid(ctx, j, func() (receipt, error) {
		return a.submit(ctx, "/v2/studio/operation/retarget_model", map[string]any{
			"model_version": "default", "motion_asset_id": x.MotionAssetID, "project_id": x.ProjectID, "rig_type": "biped",
		})
	})
	if err != nil || x.DryRun {
		return r, err
	}
	return r, a.retargetFollow(ctx, &r, x.followArgs, "motion-"+x.MotionAssetID[:min(8, len(x.MotionAssetID))], x.MotionAssetID, "")
}

func listMotions(ctx context.Context, a *app, _ noArgs) (reply, error) {
	var out struct {
		Assets      []motionAsset `json:"assets"`
		ActiveTasks []motionTask  `json:"active_tasks"`
	}
	if err := a.call(ctx, "POST", "/v2/studio/motion/list_assets", map[string]any{}, &out); err != nil {
		return reply{}, err
	}
	if len(out.Assets) == 0 && len(out.ActiveTasks) == 0 {
		return say("No motions yet: tripo_generate_motion creates one."), nil
	}
	var r reply
	r.line("Motion assets (%d):", len(out.Assets))
	for _, m := range out.Assets {
		r.line("- %s  %s", m.AssetID, m.describe())
	}
	if len(out.ActiveTasks) > 0 {
		r.line("Running tasks (%d):", len(out.ActiveTasks))
		for _, t := range out.ActiveTasks {
			r.line("- %s  %s", t.TaskID, t.Status)
		}
	}
	return r, nil
}

type getMotionArgs struct {
	MotionAssetID string `json:"motion_asset_id"`
	TaskID        string `json:"task_id"`
	WaitSeconds   *int   `json:"wait_seconds"`
	DownloadTo    string `json:"download_to"`
}

func getMotion(ctx context.Context, a *app, x getMotionArgs) (reply, error) {
	if (x.MotionAssetID == "") == (x.TaskID == "") {
		return reply{}, errors.New("pass exactly one of motion_asset_id or task_id")
	}
	r := reply{Card: &card{Kind: "job", Title: "Motion", OperatorID: x.TaskID}}
	if x.TaskID != "" {
		timeout := time.Duration(0)
		if x.DownloadTo != "" {
			timeout = 900 * time.Second
		}
		if x.WaitSeconds != nil {
			timeout = time.Duration(min(max(*x.WaitSeconds, 0), 3600)) * time.Second
		}
		return r, a.motionFollow(ctx, &r, x.TaskID, timeout, x.DownloadTo)
	}
	m, err := a.motionAsset(ctx, x.MotionAssetID)
	if err != nil {
		return reply{}, err
	}
	r.Card.Status = "done"
	return r, a.motionShow(ctx, &r, m, "", x.DownloadTo)
}
