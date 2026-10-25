package main

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestAssetGroups(t *testing.T) {
	a := &app{dir: t.TempDir()}
	ctx := context.Background()
	must := func(_ reply, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	load := func() groupsDoc {
		t.Helper()
		doc, err := a.loadGroups()
		if err != nil {
			t.Fatal(err)
		}
		return doc
	}
	idOf := func(name string) string {
		t.Helper()
		id, ok := load().byName(name)
		if !ok {
			t.Fatalf("no group named %q", name)
		}
		return id
	}

	must(createAssetGroup(ctx, a, nameArgs{Name: "Hero"}))
	must(createAssetGroup(ctx, a, nameArgs{Name: "Villain"}))
	must(createAssetGroup(ctx, a, nameArgs{Name: "  hero "})) // same name: reuses Hero
	if n := len(load().Groups); n != 2 {
		t.Fatalf("%d groups after a repeated name, want 2", n)
	}
	hero, villain := idOf("Hero"), idOf("Villain")

	must(renameAssetGroup(ctx, a, renameArgs{Group: villain, NewName: "Boss"}))
	if _, err := renameAssetGroup(ctx, a, renameArgs{Group: "Boss", NewName: "hero"}); err == nil {
		t.Fatal("renaming onto another group's name succeeded")
	}
	if got := load().Groups[villain].Name; got != "Boss" {
		t.Fatalf("renamed to %q, want Boss", got)
	}

	must(setAssetGroup(ctx, a, setArgs{Group: hero, ProjectIDs: []string{"p1", "p2"}, OperatorIDs: []string{"op1"}}))
	must(setAssetGroup(ctx, a, setArgs{Group: "Boss", ProjectIDs: []string{"p2"}})) // p2 moves out of Hero
	doc := load()
	if h := doc.Groups[hero]; len(h.Projects) != 1 || h.Projects[0] != "p1" || len(h.Jobs) != 1 || h.Jobs[0] != "op1" {
		t.Fatalf("Hero after move: %+v", h)
	}
	if b := doc.Groups[villain]; len(b.Projects) != 1 || b.Projects[0] != "p2" || len(b.Jobs) != 0 {
		t.Fatalf("Boss after move: %+v", b)
	}

	list, err := listAssetGroups(ctx, a, noArgs{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(list.Text, `"Boss"  1 projects, 0 jobs`) {
		t.Fatalf("list text: %s", list.Text)
	}

	assets, err := listGroupAssets(ctx, a, groupArgs{Group: "boss"})
	if err != nil {
		t.Fatal(err)
	}
	if assets.Card == nil || assets.Card.Kind != "projects" || len(assets.Card.Items) != 1 || assets.Card.Items[0].ID != "p2" {
		t.Fatalf("group card: %+v", assets.Card)
	}

	// The save is a rename over groups.json, so no temp file is left behind.
	entries, err := os.ReadDir(a.dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "groups.json" {
		t.Fatalf("data dir holds %d entries, want only groups.json", len(entries))
	}
}
