package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"testing"
)

// testdata/meshopt.json holds buffers encoded by meshoptimizer 1.3 and the
// bytes its reference decoder produces from them.
func TestMeshoptDecode(t *testing.T) {
	raw, err := os.ReadFile("testdata/meshopt.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name, Mode, Filter, Src, Want string
		Count, Stride                 int
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		src, _ := base64.StdEncoding.DecodeString(c.Src)
		want, _ := base64.StdEncoding.DecodeString(c.Want)
		got := make([]byte, c.Count*c.Stride)
		switch c.Mode {
		case "ATTRIBUTES":
			err = decodeVertexBuffer(got, c.Count, c.Stride, src, c.Filter)
		case "TRIANGLES":
			err = decodeIndexBuffer(got, c.Count, c.Stride, src)
		default:
			err = decodeIndexSequence(got, c.Count, c.Stride, src)
		}
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s: err=%v, equal=%v", c.Name, err, bytes.Equal(got, want))
		}
		if err := decodeVertexBuffer(got, c.Count, c.Stride, src[:len(src)/2], c.Filter); c.Mode == "ATTRIBUTES" && err == nil {
			t.Errorf("%s: truncated input accepted", c.Name)
		}
	}
}
