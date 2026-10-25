package main

import (
	"image"
	"image/color"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCropRect(t *testing.T) {
	b := image.Rect(0, 0, 100, 50)
	if r, err := cropRect(b, 10, 5, 20, 10); err != nil || r != image.Rect(10, 5, 30, 15) {
		t.Fatal(r, err)
	}
	for _, c := range [][4]int{{90, 0, 20, 10}, {0, 45, 10, 10}, {-1, 0, 5, 5}, {0, 0, 0, 5}} {
		if _, err := cropRect(b, c[0], c[1], c[2], c[3]); err == nil {
			t.Fatalf("%v should fail", c)
		}
	}
}

func TestCropKeepsSource(t *testing.T) {
	dir := t.TempDir()
	src := image.NewNRGBA(image.Rect(0, 0, 40, 30))
	for y := 0; y < 30; y++ {
		for x := 0; x < 40; x++ {
			src.SetNRGBA(x, y, color.NRGBA{uint8(x), uint8(y), 7, 255})
		}
	}
	data, _ := encodePNG(src)
	in := filepath.Join(dir, "a.png")
	os.WriteFile(in, data, 0o644)
	for i := 0; i < 2; i++ { // the second run must not overwrite the first result
		r, err := cropImage(t.Context(), nil, cropArgs{ImagePath: in, Left: 5, Top: 6, Width: 10, Height: 8})
		if err != nil {
			t.Fatal(err)
		}
		want := "a.cropped.png"
		if i == 1 {
			want = "a.cropped-2.png"
		}
		if got := filepath.Base(r.Card.Saved[0]); got != want {
			t.Fatal(got)
		}
	}
	out, _, err := loadImage(filepath.Join(dir, "a.cropped.png"))
	if err != nil || out.Bounds().Dx() != 10 || out.Bounds().Dy() != 8 {
		t.Fatal(err)
	}
	if r, g, _, _ := out.At(0, 0).RGBA(); r>>8 != 5 || g>>8 != 6 {
		t.Fatal("crop origin is wrong", r>>8, g>>8)
	}
	if got, _ := os.ReadFile(in); string(got) != string(data) {
		t.Fatal("source changed")
	}
}

func TestPaint(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 64, 64))
	for i := range src.Pix {
		src.Pix[i] = 255
	}
	half := 0.5
	out := paint(src, []stroke{
		{Points: [][]float64{{0.5, 0.5}}, Radius: 4, Color: []int{255, 0, 0}},
		{Points: [][]float64{{0.1, 0.1}, {0.9, 0.1}}, Radius: 2, Color: []int{0, 0, 0}, Opacity: &half},
	})
	if c := out.NRGBAAt(32, 32); c != (color.NRGBA{255, 0, 0, 255}) { // v=0.5 is the middle row
		t.Fatal("centre", c)
	}
	if c := out.NRGBAAt(32, 57); c.R < 120 || c.R > 135 || c.G != c.R { // v=0.1 is near the bottom
		t.Fatal("half-opacity black over white", c)
	}
	if c := out.NRGBAAt(2, 2); c != (color.NRGBA{255, 255, 255, 255}) {
		t.Fatal("untouched pixel", c)
	}
	if src.NRGBAAt(32, 32).G != 255 {
		t.Fatal("source changed")
	}
}

func TestLocalArgChecks(t *testing.T) {
	var c camArgs
	if err := c.check(false); err != nil || c.ViewportWidth != 512 || c.FovDegrees != 50 || c.View != "three_quarter" {
		t.Fatal(c, err)
	}
	for _, bad := range []camArgs{{ViewportWidth: 10}, {FovDegrees: 200}, {View: "top"}, {CameraMatrix: []float64{1, 2}}} {
		if bad.check(false) == nil {
			t.Fatalf("%+v should fail", bad)
		}
	}
	if (&camArgs{}).check(true) == nil {
		t.Fatal("bake needs a camera_matrix")
	}
	good := []partEdit{{Action: "merge", PartNames: []string{"a", "b"}, Name: "ab"}, {Action: "split", PartNames: []string{"a"}, Name: "c", FaceIndices: []int{1}}, {Action: "hide", PartNames: []string{"a"}}}
	if err := (&editArgs{Edits: good}).check(); err != nil {
		t.Fatal(err)
	}
	for _, e := range []partEdit{
		{Action: "merge", PartNames: []string{"a"}, Name: "x"},
		{Action: "merge", PartNames: []string{"a", "a"}, Name: "x"},
		{Action: "merge", PartNames: []string{"a", "b"}},
		{Action: "split", PartNames: []string{"a"}, Name: "x"},
		{Action: "delete", PartNames: []string{"a\x00"}},
		{Action: "explode", PartNames: []string{"a"}},
	} {
		if (&editArgs{Edits: []partEdit{e}}).check() == nil {
			t.Fatalf("%+v should fail", e)
		}
	}
	if (&paintArgs{Strokes: []stroke{{Points: [][]float64{{0.5, 1.5}}, Radius: 3, Color: []int{0, 0, 0}}}}).check() == nil {
		t.Fatal("u/v outside 0-1 must fail")
	}
}

func TestCheckModel(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "x.glb")
	os.WriteFile(bad, []byte("not a glb at all, just text"), 0o644)
	if _, _, err := checkModel(bad); err == nil || !strings.Contains(err.Error(), "GLB") {
		t.Fatal(err)
	}
	if _, _, err := checkModel(filepath.Join(dir, "m.obj")); err == nil {
		t.Fatal("only .glb is accepted")
	}
	if len(blenderWorker) < 1000 || !strings.Contains(string(blenderWorker), "MIT License") {
		t.Fatal("embedded worker is missing or has no attribution")
	}
}
