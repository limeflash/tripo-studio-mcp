package main

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestImagePlanAndPrice(t *testing.T) {
	a := &app{dir: t.TempDir()}
	prices, err := a.prices()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name     string
		args     imageGenArgs
		discount float64
		free     int
		keys     []string
		want     int
	}{
		{"default, premium", imageGenArgs{Prompt: "a fox"}, 0.2, 0, []string{"NanoBanana_2"}, 2},
		{"4 images 4K GPT Image 2, premium", imageGenArgs{Prompt: "a fox", ModelVersion: "gpt_image_2", Amount: 4, Resolution: "4K"}, 0.2, 0, []string{"GPT_image_2", "ImageUpscale4K"}, 24},
		{"4 images, all free", imageGenArgs{Prompt: "a fox", Amount: 4}, 0.2, 4, []string{"NanoBanana_2"}, 0},
		{"4 images, 1 free, no discount", imageGenArgs{Prompt: "a fox", ModelVersion: "midjourney", Amount: 4}, 1, 1, []string{"Midjourney"}, 30},
		{"template only", imageGenArgs{TemplateID: "x"}, 1, 0, []string{"NanoBanana_2"}, 10},
	}
	for _, c := range cases {
		p, err := c.args.plan()
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if !reflect.DeepEqual(p.keys, c.keys) {
			t.Errorf("%s: keys %v, want %v", c.name, p.keys, c.keys)
		}
		if got, _ := prices.netCost(p.keys, p.requests, c.discount, c.free); got != c.want {
			t.Errorf("%s: %d credits, want %d", c.name, got, c.want)
		}
	}

	p, _ := imageGenArgs{Prompt: " a\x00fox ", Scale: "16:9", TPose: true}.plan()
	want := map[string]any{"amount": 1, "if_upload": true, "model_version": "gemini_3.1_flash_image_preview", "prompt": "a fox", "resolution": "1K", "scale": "16:9", "sketch_to_render": false, "t_pose": true}
	if !reflect.DeepEqual(p.body, want) {
		t.Errorf("body %v, want %v", p.body, want)
	}

	if n, _ := prices.netCost([]string{"ImageSplit"}, 1, 1, 0); n >= 0 {
		t.Error("an unpriced operation was priced")
	}
	if n, _ := prices.netCost([]string{"ImageUpscale4K"}, 1, 1, 0); n != 10 {
		t.Errorf("upscale %d, want 10", n)
	}

	for _, bad := range []imageGenArgs{
		{},
		{Prompt: "x", Amount: 5},
		{Prompt: "x", Resolution: "8K"},
		{Prompt: "x", Scale: "2:1"},
		{Prompt: "x", ModelVersion: "flux.1_dev"},
		{Prompt: "x", ModelVersion: "midjourney", ImagePaths: []string{"a.png"}},
		{Prompt: "x", ImagePaths: make([]string, 6), StudioReferences: make([]studioRef, 5)},
	} {
		if _, err := bad.plan(); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

func TestModelModes(t *testing.T) {
	two := 0
	modes := []struct {
		name string
		g    genArgs
		mode string
		bad  bool
	}{
		{"text", genArgs{Prompt: "a fox"}, "text", false},
		{"image", genArgs{viewArgs: viewArgs{ImagePath: "a.png"}}, "image", false},
		{"batch", genArgs{ImagePaths: []string{"a.png", "b.png"}}, "batch", false},
		{"studio image", genArgs{Mode: "studio_image", StudioImageID: "x", OutputIndex: &two}, "studio_image", false},
		{"studio multiview", genArgs{StudioViewsID: "x"}, "studio_multiview", false},
		{"nothing", genArgs{}, "", true},
		{"two inputs", genArgs{Prompt: "a fox", ImagePaths: []string{"a.png"}}, "", true},
		{"text with images", genArgs{Mode: "text", Prompt: "a fox", StudioImageID: "x"}, "", true},
		{"batch without images", genArgs{Mode: "batch"}, "", true},
		{"t_pose outside text", genArgs{ImagePaths: []string{"a.png"}, TPose: true}, "", true},
		{"batch of 31", genArgs{ImagePaths: make([]string, 31)}, "", true},
	}
	for _, c := range modes {
		mode, _, err := c.g.resolve()
		if c.bad != (err != nil) || mode != c.mode {
			t.Errorf("%s: mode %q, err %v", c.name, mode, err)
		}
	}

	// variants: one budget per variant, sent as variations
	p, err := genArgs{ModelVersion: "P2", Amount: 2, FaceLimits: []int{10000, 3000}}.planFor("image")
	if err != nil {
		t.Fatal(err)
	}
	wantVar := []map[string]int{{"face_limit": 10000}, {"face_limit": 3000}}
	if _, has := p.body["face_limit"]; has || !reflect.DeepEqual(p.body["variations"], wantVar) || p.variants != 2 {
		t.Errorf("variants body %v", p.body)
	}
	if p, _ := (genArgs{ModelVersion: "P2", Amount: 4, FaceLimit: 8000}).planFor("image"); len(p.body["variations"].([]map[string]int)) != 4 {
		t.Errorf("amount 4 with one face_limit: %v", p.body)
	}
	for _, bad := range []genArgs{
		{ModelVersion: "P1", Amount: 2, FaceLimit: 8000},
		{ModelVersion: "P2", Amount: 3, FaceLimit: 8000},
		{ModelVersion: "P2", Amount: 2, FaceLimits: []int{8000}},
		{ModelVersion: "P2", FaceLimits: []int{8000, 30000}},
		{ModelVersion: "H3.1", Amount: 2, FaceLimit: 8000},
		{ModelVersion: "P2", FaceLimit: 8000, AllowSensitive: true, Visibility: "public"},
	} {
		if _, err := bad.planFor("image"); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}

	// autofix is sent for image and batch (High Detail), not for text or multiview
	for mode, want := range map[string]bool{"image": true, "batch": true, "text": false, "multiview": false} {
		p, err := genArgs{Tier: "high_detail", FaceLimit: 20000}.planFor(mode)
		if _, has := p.body["enable_image_autofix"]; err != nil || has != want {
			t.Errorf("%s: autofix sent %v, want %v (%v)", mode, has, want, err)
		}
	}
}

func TestReceiptsAndAudit(t *testing.T) {
	list, err := parseReceipts(json.RawMessage(`[{"operator_id":"o1","project_id":"p1"},{"operator_id":"o2","project_id":"p2"}]`))
	if err != nil || len(list) != 2 || list[1].ProjectID != "p2" {
		t.Fatal(list, err)
	}
	if one, err := parseReceipts(json.RawMessage(`{"operator_id":"o1","project_id":"p1"}`)); err != nil || len(one) != 1 {
		t.Fatal(one, err)
	}
	for _, bad := range []string{`[]`, `{}`, `[{"operator_id":"o1"},{"project_id":"p2"}]`, `"x"`} {
		if _, err := parseReceipts(json.RawMessage(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	for audit, allow := range map[string]bool{"sensitive": true} {
		if got, err := modelAudit(imageOutput{Audit: audit}, allow); err != nil || got != audit {
			t.Errorf("%s: %q %v", audit, got, err)
		}
	}
	for _, c := range []struct {
		audit string
		allow bool
	}{{"sensitive", false}, {"nsfw", true}, {"reject", false}} {
		if _, err := modelAudit(imageOutput{Audit: c.audit}, c.allow); err == nil {
			t.Errorf("accepted %+v", c)
		}
	}
	if got, err := modelAudit(imageOutput{}, false); err != nil || got != "pass" {
		t.Errorf("no audit: %q %v", got, err)
	}
}
