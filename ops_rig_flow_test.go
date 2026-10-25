package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeTripo answers Studio's API from memory so the paid flows run end to end
// without touching the network. Bodies of every POST are kept for assertions.
type fakeTripo struct {
	t      *testing.T
	bodies map[string]map[string]any // path -> last JSON body
	hits   map[string]int
	detail func(r *http.Request) map[string]any
}

func (f *fakeTripo) RoundTrip(r *http.Request) (*http.Response, error) {
	path := r.URL.Path
	f.hits[path]++
	if r.Method == "POST" {
		var b map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &b)
		f.bodies[path] = b
	}
	if r.URL.Host == "files.tripo3d.ai" {
		return reply200(append([]byte("glTF\x02\x00\x00\x00"), bytes.Repeat([]byte{1}, 40)...), "application/octet-stream"), nil
	}
	var data any
	switch {
	case strings.HasPrefix(path, "/v2/studio/project/detail/v3/"):
		data = f.detail(r)
	case path == "/v2/studio/user/profile/payment":
		data = map[string]any{"wallet": map[string]any{"total_credit": 1000}, "member": map[string]any{"type": "premium"}}
	case path == "/v2/studio/operation/quota":
		data = map[string]any{"quota": map[string]any{"uv_edit": map[string]any{"generate": map[string]any{"remaining": 1, "total_count": 2}}}}
	case path == "/v2/studio/operation/pre_rig_check":
		data = map[string]any{"riggable": true, "rig_type": "biped"}
	case strings.HasSuffix(path, "/rigging_model"), strings.HasSuffix(path, "/retarget_model"), strings.HasSuffix(path, "/uv_edit/generate"):
		data = []any{map[string]any{"operator_id": "op1", "project_id": "p1"}}
	case path == "/v2/studio/progress":
		data = []any{map[string]any{"operator_id": "op1", "project_id": "p1", "status": "success", "progress": 100}}
	case path == "/v2/studio/motion/generate":
		data = map[string]any{"task_id": "t1", "status": "queued"}
	case path == "/v2/studio/motion/get_task":
		data = map[string]any{"task_id": "t1", "status": "success", "asset_id": "a1"}
	case path == "/v2/studio/motion/get_asset":
		data = map[string]any{"asset_id": "a1", "motion_url": "https://files.tripo3d.ai/motion.glb", "title": "wave", "input": map[string]any{"segments": []any{map[string]any{"prompt": "wave", "duration_seconds": 3}}}}
	case path == "/v2/studio/operation/uv_edit/context":
		data = map[string]any{"project_id": "p1", "current_operator_id": "cur", "next_action": "generate", "running_task": nil, "candidates": []any{
			map[string]any{"operator_id": "op1", "created_at": 1.7e12, "model": map[string]any{"url": "https://files.tripo3d.ai/uv.glb"}, "uv_layout": map[string]any{"url": "https://files.tripo3d.ai/layout.png"}},
		}}
	case path == "/v2/studio/operation/uv_edit/apply":
		data = map[string]any{"project_id": "p1", "previous_operator_id": "cur", "current_operator_id": "op1"}
	case path == "/v2/studio/operation/export":
		data = map[string]any{"operator_id": "op1"}
	case path == "/v2/studio/operation/download_with_name":
		data = map[string]any{"model_url": "https://files.tripo3d.ai/export.glb"}
	default:
		f.t.Fatalf("unexpected request %s %s", r.Method, path)
	}
	b, _ := json.Marshal(map[string]any{"code": 0, "data": data})
	return reply200(b, "application/json"), nil
}

func reply200(b []byte, ct string) *http.Response {
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {ct}}, Body: io.NopCloser(bytes.NewReader(b))}
}

func newFakeApp(t *testing.T) (*app, *fakeTripo) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "session.json"), []byte(`{"cookie":"x","device_id":"d","source":"test"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &fakeTripo{t: t, bodies: map[string]map[string]any{}, hits: map[string]int{}}
	f.detail = func(r *http.Request) map[string]any {
		op := map[string]any{"operator_id": "cur", "is_textured": true, "is_rigged": true, "is_segmented": false,
			"rigging": map[string]any{"type": "biped"}}
		if r.URL.Query().Get("operator_id") == "op1" {
			op["operator_id"] = "op1"
			op["Retarget"] = []any{
				map[string]any{"name": "preset:biped:walk", "model_url": "https://files.tripo3d.ai/walk.glb"},
				map[string]any{"motion_asset_id": "a1", "model_url": "https://files.tripo3d.ai/applied.glb"},
			}
		}
		return map[string]any{"id": "p1", "project_name": "Robot", "status": "success", "is_owner": true,
			"model_url": "https://files.tripo3d.ai/model.glb", "operator": op}
	}
	a := &app{dir: dir,
		api: &http.Client{Transport: f, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		dl:  &http.Client{Transport: f},
	}
	return a, f
}

func TestPaidFlowsWithFakeStudio(t *testing.T) {
	ctx := context.Background()
	out := t.TempDir()
	wait := followArgs{DownloadTo: out, WaitSeconds: 30}

	t.Run("rig saves the model", func(t *testing.T) {
		a, f := newFakeApp(t)
		r, err := rigModel(ctx, a, rigArgs{ProjectID: "p1", SkeletonPreset: "mixamo", followArgs: wait})
		if err != nil || r.Card.Status != "saved" {
			t.Fatalf("%v %+v\n%s", err, r.Card, r.Text)
		}
		want := `{"model_version":"v3.0-20260909","project_id":"p1","rig_type":"biped","spec":"mixamo"}`
		if b, _ := json.Marshal(f.bodies["/v2/studio/operation/rigging_model"]); string(b) != want {
			t.Errorf("rigging body %s", b)
		}
	})
	t.Run("rig dry run submits nothing", func(t *testing.T) {
		a, f := newFakeApp(t)
		if _, err := rigModel(ctx, a, rigArgs{ProjectID: "p1", followArgs: followArgs{DryRun: true}}); err != nil {
			t.Fatal(err)
		}
		if f.hits["/v2/studio/operation/rigging_model"] != 0 || f.hits["/v2/studio/operation/pre_rig_check"] != 1 {
			t.Errorf("hits %v", f.hits)
		}
	})
	t.Run("animate saves the animation, not the model", func(t *testing.T) {
		a, f := newFakeApp(t)
		r, err := animateModel(ctx, a, animateArgs{ProjectID: "p1", Animation: "walk", followArgs: wait})
		if err != nil || len(r.Card.Saved) != 1 || !strings.Contains(r.Card.Saved[0], "Robot-walk") {
			t.Fatalf("%v %+v\n%s", err, r.Card, r.Text)
		}
		want := `{"animations":["preset:biped:walk"],"model_version":"default","project_id":"p1","rig_type":"biped"}`
		if b, _ := json.Marshal(f.bodies["/v2/studio/operation/retarget_model"]); string(b) != want {
			t.Errorf("retarget body %s", b)
		}
		if strings.Contains(r.Text, "Next: tripo_wait") {
			t.Errorf("stale next step in %q", r.Text)
		}
		if !strings.Contains(r.Text, "unknown") {
			t.Errorf("cost should be reported unknown: %q", r.Text)
		}
	})
	t.Run("animate without download_to points at the follow-ups", func(t *testing.T) {
		a, _ := newFakeApp(t)
		r, err := animateModel(ctx, a, animateArgs{ProjectID: "p1", Animation: "preset:biped:run"})
		if err != nil || !strings.Contains(r.Text, "tripo_export_model") || strings.Count(r.Text, "Next:") != 1 {
			t.Fatalf("%v\n%s", err, r.Text)
		}
	})
	t.Run("motion generate waits and saves", func(t *testing.T) {
		a, f := newFakeApp(t)
		r, err := generateMotion(ctx, a, motionArgs{Prompt: "wave", DownloadTo: out})
		if err != nil || len(r.Card.Saved) != 1 {
			t.Fatalf("%v %+v\n%s", err, r.Card, r.Text)
		}
		if b, _ := json.Marshal(f.bodies["/v2/studio/motion/generate"]); string(b) != `{"segments":[{"duration_seconds":3,"prompt":"wave"}]}` {
			t.Errorf("motion body %s", b)
		}
		if !strings.Contains(r.Text, "motion_asset_id=a1") {
			t.Errorf("no asset id in %q", r.Text)
		}
	})
	t.Run("motion generate without waiting names the task", func(t *testing.T) {
		a, f := newFakeApp(t)
		r, err := generateMotion(ctx, a, motionArgs{Prompt: "wave"})
		if err != nil || !strings.Contains(r.Text, "tripo_get_motion task_id=t1") || strings.Contains(r.Text, "tripo_wait") || f.hits["/v2/studio/motion/get_task"] != 0 {
			t.Fatalf("%v\n%s", err, r.Text)
		}
	})
	t.Run("apply motion", func(t *testing.T) {
		a, f := newFakeApp(t)
		r, err := applyMotion(ctx, a, applyMotionArgs{ProjectID: "p1", MotionAssetID: "a1", followArgs: wait})
		if err != nil || len(r.Card.Saved) != 1 || !strings.Contains(r.Card.Saved[0], "motion-a1") {
			t.Fatalf("%v %+v\n%s", err, r.Card, r.Text)
		}
		want := `{"model_version":"default","motion_asset_id":"a1","project_id":"p1","rig_type":"biped"}`
		if b, _ := json.Marshal(f.bodies["/v2/studio/operation/retarget_model"]); string(b) != want {
			t.Errorf("retarget body %s", b)
		}
	})
	t.Run("uv generate waits and saves model and layout", func(t *testing.T) {
		a, f := newFakeApp(t)
		// the model header says 1 triangle: a tiny GLB, so only the count is "unknown"
		r, err := generateUV(ctx, a, uvGenArgs{ProjectID: "p1", followArgs: wait})
		if err != nil || len(r.Card.Saved) != 2 {
			t.Fatalf("%v %+v\n%s", err, r.Card, r.Text)
		}
		want := `{"action":"generate","current_operator_id":"cur","project_id":"p1"}`
		if b, _ := json.Marshal(f.bodies["/v2/studio/operation/uv_edit/generate"]); string(b) != want {
			t.Errorf("uv body %s", b)
		}
		if !strings.Contains(r.Text, "Free quota left: 1 of 2") {
			t.Errorf("quota not mentioned: %q", r.Text)
		}
	})
	t.Run("uv apply", func(t *testing.T) {
		a, f := newFakeApp(t)
		r, err := applyUV(ctx, a, uvApplyArgs{ProjectID: "p1", CandidateOperatorID: "op1"})
		if err != nil || r.Card.Status != "done" {
			t.Fatalf("%v\n%s", err, r.Text)
		}
		want := `{"candidate_operator_id":"op1","current_operator_id":"cur","project_id":"p1"}`
		if b, _ := json.Marshal(f.bodies["/v2/studio/operation/uv_edit/apply"]); string(b) != want {
			t.Errorf("apply body %s", b)
		}
		if _, err := applyUV(ctx, a, uvApplyArgs{ProjectID: "p1", CandidateOperatorID: "zzz"}); err == nil {
			t.Error("unknown candidate applied")
		}
	})
	t.Run("export saves and measures credits", func(t *testing.T) {
		a, f := newFakeApp(t)
		r, err := exportModel(ctx, a, exportArgs{ProjectID: "p1", Format: "glb", Path: out + string(filepath.Separator)})
		if err != nil || len(r.Card.Saved) != 1 || !strings.Contains(r.Text, "the export was free") {
			t.Fatalf("%v\n%s", err, r.Text)
		}
		if f.bodies["/v2/studio/operation/export"]["format"] != "gltf" {
			t.Errorf("export body %v", f.bodies["/v2/studio/operation/export"])
		}
		if _, err := os.Stat(r.Card.Saved[0]); err != nil {
			t.Error(err)
		}
	})
}
