package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// AWS's published SigV4 example (S3 "GET Object" with a Range header).
func TestSigV4(t *testing.T) {
	hdr := map[string]string{
		"host":                 "examplebucket.s3.amazonaws.com",
		"range":                "bytes=0-9",
		"x-amz-content-sha256": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		"x-amz-date":           "20130524T000000Z",
	}
	at := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
	got := sigV4("GET", "/test.txt", "", hdr, "AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", "us-east-1", "s3", at)
	if !strings.HasSuffix(got, "Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41") {
		t.Fatal(got)
	}
}

func TestCosts(t *testing.T) {
	a := &app{dir: t.TempDir()}
	prices, err := a.prices()
	if err != nil {
		t.Fatal(err)
	}
	cost := func(keys []string) int { n, _ := prices.cost(keys); return n }

	gen := func(g genArgs, multiview bool) int {
		_, keys, _, err := g.settings(multiview)
		if err != nil {
			t.Fatal(err)
		}
		return cost(keys)
	}
	cases := []struct {
		name string
		got  int
		want int
	}{
		{"P1 multiview", gen(genArgs{Tier: "smart_mesh", FaceLimit: 19500}, true), 35},
		{"P2", gen(genArgs{ModelVersion: "P2", FaceLimit: 19500}, true), 100},
		{"H3.1 textured 4K detailed geometry", gen(genArgs{Tier: "high_detail", FaceLimit: 50000, Texture: true, TextureQuality: "detailed", GeometryQuality: "detailed"}, false), 25 + 10 + 15},
		{"texture 2K", cost([]string{"TextureGeneration"}), 10},
		{"texture 4K", cost([]string{"TextureGeneration", "TextureQualityDetailed"}), 20},
		{"retopology triangles", cost([]string{"Retopology_Triangle", "Retopology_SmartPoly"}), 35},
		{"retopology quads", cost([]string{"Retopology_Quad", "Retopology_SmartPoly"}), 40},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: %d credits, want %d", c.name, c.got, c.want)
		}
	}

	for _, bad := range []genArgs{
		{Tier: "smart_mesh", FaceLimit: 20001},
		{Tier: "smart_mesh", FaceLimit: 19500, Quad: true},
		{Tier: "smart_mesh", FaceLimit: 19500, Texture: true},
		{ModelVersion: "P1", FaceLimit: 19500, Symmetry: new(bool)},
		{Tier: "high_detail", FaceLimit: 19500, PBR: true},
		{FaceLimit: 19500},
	} {
		if _, _, _, err := bad.settings(true); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

func TestViewsAndCookie(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"0_front.png", "1_left.png", "3_right.png", "notes.txt"} {
		os.WriteFile(filepath.Join(dir, n), nil, 0o644)
	}
	paths, multiview, err := viewArgs{ViewsDir: dir}.paths()
	if err != nil || !multiview || filepath.Base(paths[0]) != "0_front.png" || paths[2] != "" || filepath.Base(paths[3]) != "3_right.png" {
		t.Fatal(paths, err)
	}
	if _, _, err := (viewArgs{Left: "l.png"}).paths(); err == nil {
		t.Fatal("multiview without a front view was accepted")
	}

	for in, want := range map[string]string{
		"abc==":                            "abc==",
		"  ory_kratos_session=abc==\n":     "abc==",
		"a=1; ory_kratos_session=xyz; b=2": "xyz",
	} {
		if got := cookieValue(in); got != want {
			t.Errorf("cookieValue(%q) = %q, want %q", in, got, want)
		}
	}
}
