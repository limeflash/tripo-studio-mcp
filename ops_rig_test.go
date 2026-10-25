package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPlanRig(t *testing.T) {
	cases := []struct {
		name                       string
		version, kind, preset      string
		riggable                   bool
		detected                   string
		wantErr                    string
		wantVer, wantRig, wantSpec string
	}{
		{"v3 humanoid default spec", rigV3, "humanoid", "", true, "biped", "", rigV3, "biped", "actorcore"},
		{"v3 humanoid mixamo", rigV3, "humanoid", "mixamo", true, "biped", "", rigV3, "biped", "mixamo"},
		{"v3 other quadruped", rigV3, "other", "", true, "quadruped", "", rigV3, "quadruped", ""},
		{"v3 other biped keeps biped", rigV3, "other", "", true, "biped", "", rigV3, "biped", ""},
		{"v3 humanoid on animal", rigV3, "humanoid", "", true, "quadruped", "needs a biped", "", "", ""},
		{"preset needs v3 humanoid", rigV25, "humanoid", "unreal", true, "biped", "skeleton_preset", "", "", ""},
		{"preset needs humanoid", rigV3, "other", "unreal", true, "quadruped", "skeleton_preset", "", "", ""},
		{"v2.5 biped forced to v1", rigV25, "humanoid", "", true, "biped", "", rigV1, "biped", ""},
		{"v2.5 animal stays", rigV25, "humanoid", "", true, "avian", "", rigV25, "avian", ""},
		{"v1 biped", rigV1, "humanoid", "", true, "biped", "", rigV1, "biped", ""},
		{"not riggable biped", rigV3, "humanoid", "", false, "biped", "T-pose", "", "", ""},
		{"not riggable other", rigV3, "other", "", false, "others", "not riggable", "", "", ""},
		{"unsupported rig", rigV3, "other", "", true, "others", "supported rig type", "", "", ""},
		{"unsupported rig legacy", rigV25, "humanoid", "", true, "others", "supported rig type", "", "", ""},
	}
	for _, c := range cases {
		got, err := planRig(c.version, c.kind, c.preset, c.riggable, c.detected)
		if c.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("%s: error %v, want %q", c.name, err, c.wantErr)
			}
			continue
		}
		if err != nil || got.Version != c.wantVer || got.RigType != c.wantRig || got.Spec != c.wantSpec {
			t.Errorf("%s: %+v, %v; want %s %s %s", c.name, got, err, c.wantVer, c.wantRig, c.wantSpec)
		}
	}
}

func TestAnimationID(t *testing.T) {
	cases := []struct {
		animation, rig, wantID, wantRig string
		bad                             bool
	}{
		{"preset:biped:walk", "", "preset:biped:walk", "biped", false},
		{"walk", "", "preset:biped:walk", "biped", false},
		{"walk", "quadruped", "preset:quadruped:walk", "quadruped", false},
		{"preset:biped:walk", "biped", "preset:biped:walk", "biped", false},
		{"preset:biped:walk", "quadruped", "", "", true},
		{"preset:biped:moonwalk", "", "", "", true},
		{"idle", "quadruped", "", "", true},
		{"walk", "avian", "", "", true},
	}
	for _, c := range cases {
		id, rig, err := animationID(c.animation, c.rig)
		if (err != nil) != c.bad || id != c.wantID || rig != c.wantRig {
			t.Errorf("%q/%q: %q %q %v", c.animation, c.rig, id, rig, err)
		}
	}
}

func TestMotionSegments(t *testing.T) {
	seg := func(p string, d int, w ...[]float64) motionSegment {
		return motionSegment{Prompt: p, Duration: d, Waypoints: w}
	}
	long := strings.Repeat("a", 1001)
	cases := []struct {
		name string
		args motionArgs
		want int // stages, or -1 for an error
	}{
		{"shortcut", motionArgs{Prompt: "wave"}, 1},
		{"segments", motionArgs{Segments: []motionSegment{seg("a", 1), seg("b", 10)}}, 2},
		{"nothing", motionArgs{}, -1},
		{"six stages", motionArgs{Segments: make([]motionSegment, 6)}, -1},
		{"mixed", motionArgs{Segments: []motionSegment{seg("a", 1)}, Prompt: "b"}, -1},
		{"too long", motionArgs{Segments: []motionSegment{seg("a", 11)}}, -1},
		{"zero", motionArgs{Segments: []motionSegment{seg("a", 0)}}, -1},
		{"blank prompt", motionArgs{Segments: []motionSegment{seg("  ", 2)}}, -1},
		{"long prompt", motionArgs{Segments: []motionSegment{seg(long, 2)}}, -1},
		{"joined", motionArgs{Segments: []motionSegment{seg("a", 2, []float64{0, 0}, []float64{1, 1}), seg("b", 2, []float64{1, 1}, []float64{2, 0})}}, 2},
		{"not joined", motionArgs{Segments: []motionSegment{seg("a", 2, []float64{0, 0}, []float64{1, 1}), seg("b", 2, []float64{5, 5}, []float64{2, 0})}}, -1},
		{"one waypoint", motionArgs{Segments: []motionSegment{seg("a", 2, []float64{0, 0})}}, -1},
	}
	for _, c := range cases {
		got, err := motionSegments(c.args)
		if (c.want < 0) != (err != nil) || (err == nil && len(got) != c.want) {
			t.Errorf("%s: %d stages, %v", c.name, len(got), err)
		}
	}
	if got, _ := motionSegments(motionArgs{Prompt: "x"}); len(got) == 1 && got[0].Duration != 3 {
		t.Errorf("shortcut duration %d, want 3", got[0].Duration)
	}
}

func TestGlbTriangles(t *testing.T) {
	js := `{"accessors":[{"count":300},{"count":120},{"count":6}],
	 "meshes":[{"primitives":[{"attributes":{"POSITION":1},"indices":0},{"attributes":{"POSITION":1}},{"attributes":{"POSITION":2},"mode":5}]}]}`
	// 300/3 indexed + 120/3 unindexed + a strip of 6 vertices = 4 triangles
	if got := glbTriangles([]byte(js + "  \x00")); got != 100+40+4 {
		t.Errorf("triangles %d", got)
	}
	if glbTriangles([]byte("nope")) != 0 {
		t.Error("garbage should count 0")
	}
}

func testProject(raw map[string]any) *project {
	raw["id"] = "p1"
	return &project{ID: "p1", Name: "My Model", raw: raw}
}

func TestPlanExport(t *testing.T) {
	owned := func(op map[string]any) *project { return testProject(map[string]any{"is_owner": true, "operator": op}) }
	rigged := map[string]any{"is_rigged": true}
	cases := []struct {
		name string
		p    *project
		x    exportArgs
		bad  string
	}{
		{"plain glb", owned(nil), exportArgs{Format: "glb"}, ""},
		{"3mf small texture", owned(nil), exportArgs{Format: "3mf", TextureSize: 512}, ""},
		{"bad format", owned(nil), exportArgs{Format: "dae"}, "format"},
		{"not owner", testProject(map[string]any{"is_owner": false}), exportArgs{Format: "glb"}, "own"},
		{"unknown owner", testProject(map[string]any{}), exportArgs{Format: "glb"}, "own"},
		{"animations without flag", owned(rigged), exportArgs{Format: "fbx", Animations: []string{"walk"}}, "with_animation"},
		{"animation unrigged", owned(nil), exportArgs{Format: "fbx", WithAnimation: true}, "rigged"},
		{"bake unrigged", owned(nil), exportArgs{Format: "fbx", EnableBakeAnimation: true}, "rigged"},
		{"animation rigged", owned(rigged), exportArgs{Format: "fbx", WithAnimation: true, Animations: []string{"walk"}, FbxPreset: "mixamo", EnableBakeAnimation: true, BakeAnimationFrame: 12}, ""},
		{"vertex colors", owned(nil), exportArgs{Format: "glb", ExportVertexColors: true}, "OBJ"},
		{"vertex colors obj", owned(nil), exportArgs{Format: "obj", ExportVertexColors: true}, ""},
		{"texture size", owned(nil), exportArgs{Format: "glb", TextureSize: 3000}, "texture_size"},
		{"packaging", owned(nil), exportArgs{Format: "glb", TexturePackaging: "tar"}, "packaging"},
		{"preset", owned(nil), exportArgs{Format: "fbx", FbxPreset: "maya"}, "fbx_preset"},
		{"frame", owned(nil), exportArgs{Format: "fbx", BakeAnimationFrame: -1}, "bake_animation_frame"},
		{"name slash", owned(nil), exportArgs{Format: "glb", Name: "a/b"}, "name"},
	}
	for _, c := range cases {
		body, name, err := planExport(c.p, c.x)
		if c.bad != "" {
			if err == nil || !strings.Contains(err.Error(), c.bad) {
				t.Errorf("%s: error %v, want %q", c.name, err, c.bad)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if _, err := json.Marshal(body); err != nil || name != "My Model" {
			t.Errorf("%s: name %q, %v", c.name, name, err)
		}
	}
	// The request body is the plugin's, key for key.
	body, _, _ := planExport(owned(map[string]any{"is_hd_textured": true}), exportArgs{Format: "glb"})
	b, _ := json.Marshal(body)
	want := `{"animate_in_place":false,"animations":[],"bake_animation_frame":0,"enable_bake_animation":false,"export_orientation":"-y","export_vertex_colors":false,"fbx_preset":"blender","format":"gltf","model_version":"default","name":"My Model","pack_uv":false,"project_id":"p1","texture_packaging":"zip","texture_size":4096,"with_animation":false}`
	if string(b) != want {
		t.Errorf("body %s", b)
	}
}

func TestRetargetURL(t *testing.T) {
	p := testProject(map[string]any{"operator": map[string]any{"Retarget": []any{
		map[string]any{"name": "preset:biped:walk", "model_url": "https://x/walk.glb"},
		map[string]any{"name": "", "motion_asset_id": "m1", "model_url": "https://x/m1.glb"},
		map[string]any{"name": "preset:biped:run"},
	}}})
	for _, c := range []struct{ motion, preset, want string }{
		{"", "preset:biped:walk", "https://x/walk.glb"},
		{"m1", "", "https://x/m1.glb"},
		{"", "preset:biped:run", ""},
		{"m2", "", ""},
	} {
		if got := retargetURL(p, c.motion, c.preset); got != c.want {
			t.Errorf("%+v: %q", c, got)
		}
	}
}

func TestRigPrices(t *testing.T) {
	a := &app{dir: t.TempDir()}
	prices, err := a.prices()
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]int{"Rigging": 20, "Motion": 20, "UvEditGenerate": 20, "UvEditRetry": 10} {
		if got, _ := prices.cost([]string{key}); got != want {
			t.Errorf("%s: %d, want %d", key, got, want)
		}
	}
	if got, _ := prices.cost([]string{retargetKey}); got != -1 {
		t.Errorf("%s should be unknown, got %d", retargetKey, got)
	}
	if uvPriceKey("retry") != "UvEditRetry" || uvPriceKey("generate") != "UvEditGenerate" {
		t.Error("uv price keys")
	}
}

func TestRigToolsRegistered(t *testing.T) {
	want := map[string]bool{"tripo_rig_model": false, "tripo_animate_model": false, "tripo_list_animation_presets": false,
		"tripo_generate_motion": false, "tripo_apply_motion": false, "tripo_list_motions": false, "tripo_get_motion": false,
		"tripo_get_uv_context": false, "tripo_generate_uv": false, "tripo_apply_uv": false, "tripo_export_model": false}
	seen := map[string]bool{}
	for _, tl := range tools {
		if seen[tl.Name] {
			t.Errorf("duplicate tool %s", tl.Name)
		}
		seen[tl.Name] = true
		if _, ok := want[tl.Name]; ok {
			want[tl.Name] = true
			if _, err := json.Marshal(tl.InputSchema); err != nil {
				t.Errorf("%s schema: %v", tl.Name, err)
			}
		}
	}
	for n, ok := range want {
		if !ok {
			t.Errorf("tool %s not registered", n)
		}
	}
}

func TestPresetList(t *testing.T) {
	r, err := listAnimationPresets(nil, nil, presetsArgs{Query: "dance"})
	if err != nil || !strings.Contains(r.Text, "preset:biped:dance_06") || strings.Contains(r.Text, "walk") {
		t.Errorf("%v %q", err, r.Text)
	}
	if _, err := listAnimationPresets(nil, nil, presetsArgs{RigType: "robot"}); err == nil {
		t.Error("unknown rig type accepted")
	}
}
