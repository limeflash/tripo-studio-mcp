package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"strings"
	"testing"
)

func glbBytes(t *testing.T, doc any, bin []byte) []byte {
	t.Helper()
	j, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	for len(j)%4 != 0 {
		j = append(j, ' ')
	}
	for len(bin)%4 != 0 {
		bin = append(bin, 0)
	}
	var b bytes.Buffer
	le := binary.LittleEndian
	total := 12 + 8 + len(j)
	if len(bin) > 0 {
		total += 8 + len(bin)
	}
	binary.Write(&b, le, []uint32{0x46546C67, 2, uint32(total), uint32(len(j)), 0x4E4F534A})
	b.Write(j)
	if len(bin) > 0 {
		binary.Write(&b, le, []uint32{uint32(len(bin)), 0x004E4942})
		b.Write(bin)
	}
	return b.Bytes()
}

func TestValidatePartNames(t *testing.T) {
	long := strings.Repeat("x", 257)
	var many []string
	for i := 0; i < 201; i++ {
		many = append(many, strings.Repeat("p", 1)+string(rune('a'+i%26))+strings.Repeat("0", i/26))
	}
	for _, c := range []struct {
		name  string
		in    []string
		want  []string
		fails bool
	}{
		{"trims", []string{" head ", "body"}, []string{"head", "body"}, false},
		{"empty list", nil, nil, true},
		{"blank name", []string{"  "}, nil, true},
		{"control char", []string{"a\x07b"}, nil, true},
		{"overlong", []string{long}, nil, true},
		{"duplicate after trim", []string{"a", " a"}, nil, true},
		{"too many", many, nil, true},
		{"unicode ok", []string{"крыло"}, []string{"крыло"}, false},
	} {
		got, err := validatePartNames(c.in, "part_names")
		if (err != nil) != c.fails || (!c.fails && strings.Join(got, "|") != strings.Join(c.want, "|")) {
			t.Errorf("%s: got %v, %v", c.name, got, err)
		}
	}
	if got, err := optionalPartNames(nil, "part_names"); got != nil || err != nil {
		t.Error("optional empty list must pass", got, err)
	}
}

func TestTexturePlan(t *testing.T) {
	front := viewArgs{Front: "f.png", Left: "l.png"}
	for _, c := range []struct {
		name  string
		x     textureArgs
		mode  string
		fails bool
	}{
		{"nothing", textureArgs{}, "", true},
		{"prompt infers text", textureArgs{Prompt: "rust"}, "text", false},
		{"text mode", textureArgs{Mode: "text", Prompt: "rust"}, "text", false},
		{"text with image", textureArgs{Mode: "text", Prompt: "rust", viewArgs: viewArgs{ImagePath: "a.png"}}, "", true},
		{"prompt with image", textureArgs{Prompt: "rust", viewArgs: viewArgs{ImagePath: "a.png"}}, "", true},
		{"image", textureArgs{viewArgs: viewArgs{ImagePath: "a.png"}}, "image", false},
		{"multiview inferred", textureArgs{viewArgs: front}, "multiview", false},
		{"image mode with views", textureArgs{Mode: "image", viewArgs: front}, "", true},
		{"multiview mode with one image", textureArgs{Mode: "multiview", viewArgs: viewArgs{ImagePath: "a.png"}}, "", true},
		{"bad mode", textureArgs{Mode: "video", Prompt: "x"}, "", true},
		{"style with text", textureArgs{Prompt: "rust", Style: "wood"}, "text", false},
		{"style with image", textureArgs{Style: "mecha_pop", viewArgs: viewArgs{ImagePath: "a.png"}}, "image", false},
		{"style image with image", textureArgs{StyleImagePath: "s.png", viewArgs: viewArgs{ImagePath: "a.png"}}, "image", false},
		{"style with multiview", textureArgs{Style: "heritage", viewArgs: front}, "", true},
		{"style image with multiview", textureArgs{StyleImagePath: "s.png", viewArgs: front}, "", true},
		{"two styles", textureArgs{Prompt: "x", Style: "wood", StyleImagePath: "s.png"}, "", true},
		{"unknown style", textureArgs{Prompt: "x", Style: "gold"}, "", true},
	} {
		mode, _, err := c.x.plan()
		if (err != nil) != c.fails || mode != c.mode {
			t.Errorf("%s: mode %q, err %v", c.name, mode, err)
		}
	}
	for _, s := range []string{"heritage", "mecha", "mecha_pop", "wood"} {
		if builtinStyles[s]["bucket"] != "tripo-data" || !strings.HasPrefix(builtinStyles[s]["key"].(string), "tripo-studio/style_image/"+s+".") {
			t.Errorf("style %s: %v", s, builtinStyles[s])
		}
	}
}

func TestPartCosts(t *testing.T) {
	a := &app{dir: t.TempDir()}
	prices, err := a.prices()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		keys []string
		want int
	}{
		{"texture 2K + style", []string{"TextureGeneration", "TextureStyle"}, 15},
		{"texture 8K + style", []string{"TextureGeneration", "TextureQualityExtreme", "TextureStyle"}, 35},
		{"PBR", []string{"PBR"}, 5},
		{"upscale 4K", []string{"Upscaler"}, 10},
		{"upscale 8K", []string{"UpscalerExtreme"}, 20},
		{"Magic Brush", []string{"MagicBrush"}, 5},
		{"segmentation", []string{"Segmentation"}, 40},
		{"QuickCap", []string{"QuickCap"}, 0},
		{"AI completion", []string{"AICompletion"}, 0},
	} {
		if got, why := prices.cost(c.keys); got != c.want {
			t.Errorf("%s: %d credits (%s), want %d", c.name, got, why, c.want)
		}
	}
}

func proj(flags map[string]any) *project {
	return &project{raw: map[string]any{"operator": flags}}
}

func TestAssertSupports(t *testing.T) {
	yes, no := true, false
	for _, c := range []struct {
		name      string
		kind      string
		flags     map[string]any
		smartPoly bool
		fails     bool
	}{
		{"segment quad", "segmentation", map[string]any{"is_quad": yes}, false, true},
		{"segment rigged", "segmentation", map[string]any{"is_rigged": yes}, false, true},
		{"segment plain", "segmentation", map[string]any{"is_quad": no}, false, false},
		{"remesh rigged", "remesh", map[string]any{"is_rigged": yes}, false, true},
		{"smart poly on smart mesh", "remesh", map[string]any{"is_nexus_mesh": yes}, true, true},
		{"plain remesh on smart mesh", "remesh", map[string]any{"is_nexus_mesh": yes}, false, false},
		{"complete single mesh", "ai_completion", map[string]any{"is_multiple_mesh": no}, false, true},
		{"quick cap single mesh", "mesh_fill", map[string]any{"is_multiple_mesh": no}, false, true},
		{"complete multi mesh", "ai_completion", map[string]any{"is_multiple_mesh": yes}, false, false},
		{"complete unknown", "ai_completion", map[string]any{}, false, false},
		{"magic brush untextured", "retexture", map[string]any{"is_textured": no}, false, true},
		{"magic brush 8K", "retexture", map[string]any{"is_ultra_textured": yes}, false, true},
		{"magic brush segmented", "retexture", map[string]any{"is_segmented": yes}, false, true},
		{"magic brush ok", "retexture", map[string]any{"is_textured": yes}, false, false},
		{"pbr untextured", "pbr", map[string]any{"is_textured": no}, false, true},
		{"upscale untextured", "texture_upscale", map[string]any{"is_textured": no}, false, true},
		{"upscale textured", "texture_upscale", map[string]any{"is_textured": yes}, false, false},
	} {
		if err := assertSupports(c.kind, proj(c.flags), c.smartPoly); (err != nil) != c.fails {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}

func TestNormalizeMatrix(t *testing.T) {
	id := []float64{1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1}
	move := append([]float64(nil), id...)
	move[12], move[13] = 5, -2
	bad := func(f func(m []float64)) []float64 {
		m := append([]float64(nil), id...)
		f(m)
		return m
	}
	if m, err := normalizeMatrix(nil); err != nil || len(m) != 16 || m[0] != 1 {
		t.Fatal("default must be identity", m, err)
	}
	for name, c := range map[string]struct {
		m     []float64
		fails bool
	}{
		"identity":    {id, false},
		"translation": {move, false},
		"short":       {id[:15], true},
		"huge":        {bad(func(m []float64) { m[12] = 2e6 }), true},
		"not affine":  {bad(func(m []float64) { m[3] = 0.5 }), true},
		"last cell":   {bad(func(m []float64) { m[15] = 2 }), true},
		"singular":    {bad(func(m []float64) { m[0], m[5], m[10] = 0, 0, 0 }), true},
	} {
		if _, err := normalizeMatrix(c.m); (err != nil) != c.fails {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestPartsFromAccessors(t *testing.T) {
	// A meshopt-compressed, quantized model: only the metadata is read.
	doc := map[string]any{
		"extensionsUsed": []string{"EXT_meshopt_compression", "KHR_mesh_quantization"},
		"nodes": []any{
			map[string]any{"name": "root", "children": []int{1, 2, 3}},
			map[string]any{"name": " head ", "mesh": 0},
			map[string]any{"name": "body", "mesh": 1},
			map[string]any{"mesh": 2},
		},
		"meshes": []any{
			map[string]any{"primitives": []any{map[string]any{"attributes": map[string]int{"POSITION": 0}, "indices": 1}}},
			map[string]any{"primitives": []any{
				map[string]any{"attributes": map[string]int{"POSITION": 2}},            // no indices: POSITION count / 3
				map[string]any{"attributes": map[string]int{"POSITION": 2}, "mode": 5}, // strip: count - 2
			}},
			map[string]any{"primitives": []any{map[string]any{"attributes": map[string]int{"POSITION": 0}, "indices": 1}}},
		},
		"accessors": []any{map[string]any{"count": 100}, map[string]any{"count": 300}, map[string]any{"count": 30}},
	}
	d, _, err := readGLB(bytes.NewReader(glbBytes(t, doc, nil)), false)
	if err != nil {
		t.Fatal(err)
	}
	parts, unnamed := d.parts()
	if len(parts) != 2 || unnamed != 1 || parts[0] != (part{"head", 100}) || parts[1] != (part{"body", 10 + 28}) {
		t.Errorf("parts %v, %d unnamed", parts, unnamed)
	}
	if names := d.nodeNames(); !names["root"] || !names["head"] || names["nope"] {
		t.Errorf("node names %v", names)
	}
	if _, _, err := readGLB(bytes.NewReader([]byte("Kaydara FBX Binary  \x00\x1a\x00more")), false); !errors.Is(err, errFBX) {
		t.Error("FBX must be told apart from a broken GLB:", err)
	}
	if _, _, err := readGLB(bytes.NewReader([]byte("PK\x03\x04 not a glb at all")), false); err == nil {
		t.Error("a zip is not a GLB")
	}
}

func TestTextureSizesAndVerdict(t *testing.T) {
	var img bytes.Buffer
	if err := png.Encode(&img, image.NewGray(image.Rect(0, 0, 16, 8))); err != nil {
		t.Fatal(err)
	}
	doc := map[string]any{
		"images":      []any{map[string]any{"name": "base", "bufferView": 0, "mimeType": "image/png"}},
		"bufferViews": []any{map[string]any{"buffer": 0, "byteOffset": 0, "byteLength": img.Len()}},
	}
	sizes, err := glbTextureSizes(glbBytes(t, doc, img.Bytes()))
	if err != nil || len(sizes) != 1 || sizes[0] != (texSize{"base", 16, 8}) {
		t.Fatal(sizes, err)
	}
	for _, c := range []struct {
		name      string
		sizes     []texSize
		err       error
		requested int
		partial   bool
		want      string
	}{
		{"met", []texSize{{"a", 2048, 2048}, {"b", 4096, 4096}}, nil, 2048, false, "verified"},
		{"short", []texSize{{"a", 2048, 2048}}, nil, 4096, false, "WARNING"},
		{"one short", []texSize{{"a", 4096, 4096}, {"b", 1024, 1024}}, nil, 4096, false, "WARNING"},
		{"partial is not compared", []texSize{{"a", 2048, 2048}}, nil, 4096, true, "not compared"},
		{"no images", nil, nil, 2048, false, "unverified"},
		{"unreadable", nil, errors.New("boom"), 2048, false, "unverified"},
		{"no request", []texSize{{"a", 512, 512}}, nil, 0, false, "Texture sizes"},
	} {
		if got := textureVerdict(c.sizes, c.err, c.requested, c.partial); !strings.Contains(got, c.want) {
			t.Errorf("%s: %q does not contain %q", c.name, got, c.want)
		}
	}
	if _, err := glbTextureSizes(glbBytes(t, map[string]any{"images": []any{map[string]any{"uri": "tex.png"}}}, nil)); err == nil {
		t.Error("an external image must be reported, not guessed")
	}
}

func TestInspectModel(t *testing.T) {
	stl := make([]byte, 84+50*2)
	binary.LittleEndian.PutUint32(stl[80:], 2)
	glb := glbBytes(t, map[string]any{
		"meshes":    []any{map[string]any{"primitives": []any{map[string]any{"attributes": map[string]int{"POSITION": 0}}}}},
		"accessors": []any{map[string]any{"count": 9}},
	}, nil)
	for _, c := range []struct {
		name, file string
		data       []byte
		format     string
		faces      int
		fails      bool
	}{
		{"glb", "m.GLB", glb, "glb", 3, false},
		{"glb length mismatch", "m.glb", append(append([]byte(nil), glb...), 0, 0, 0, 0), "", 0, true},
		{"obj quads are split", "m.obj", []byte("v 0 0 0\nv 1 0 0\nv 1 1 0\nv 0 1 0\nf 1 2 3 4\nf 1 2 3\n"), "obj", 3, false},
		{"obj without faces", "m.obj", []byte("v 0 0 0\nv 1 0 0\nv 1 1 0\n"), "", 0, true},
		{"stl binary", "m.stl", stl, "stl", 2, false},
		{"stl ascii", "m.stl", []byte("solid x\nfacet normal 0 0 1\nouter loop\nvertex 0 0 0\nendloop\nendfacet\nendsolid x\n"), "stl", 1, false},
		{"fbx binary", "m.fbx", []byte("Kaydara FBX Binary  \x00\x1a\x00rest"), "fbx", 0, false},
		{"fbx ascii", "m.fbx", []byte("; FBX 7.4.0 project file"), "", 0, true},
		{"wrong extension", "m.ply", []byte("ply"), "", 0, true},
	} {
		format, faces, err := inspectModel(c.file, c.data)
		if (err != nil) != c.fails || format != c.format || faces != c.faces {
			t.Errorf("%s: %q %d %v", c.name, format, faces, err)
		}
	}
}
