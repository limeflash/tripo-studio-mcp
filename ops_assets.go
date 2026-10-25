package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// Local tools: the job log, asset groups and links. None of them calls Studio.

func init() {
	register(
		tool{
			Name:        "tripo_list_jobs",
			Description: "List paid jobs from the local job log, newest first: time, title, credits, status, project_id, operator_id and saved path. group filters by a group id or name from tripo_list_asset_groups. Free, local only.",
			InputSchema: object(nil, props{
				"group": str("Only jobs in this group (id or name)."),
				"limit": integer("How many to show, 1-200. Default 20."),
			}),
			run: handler(listJobs),
		},
		tool{
			Name:        "tripo_get_job",
			Description: "Show one logged job by operator_id: title, credits, status, project_id, saved path, tool and group. Free, local only.",
			InputSchema: object([]string{"operator_id"}, props{"operator_id": str("operator_id returned by a paid tool.")}),
			run:         handler(getJob),
		},
		tool{
			Name:        "tripo_list_asset_groups",
			Description: "List asset groups (characters, collections) with their project and job counts. Free, local only.",
			InputSchema: object(nil),
			run:         handler(listAssetGroups),
		},
		tool{
			Name:        "tripo_create_asset_group",
			Description: "Create an asset group. A name already in use returns the existing group. Free, local only.",
			InputSchema: object([]string{"name"}, props{"name": str("Group name, 1-80 characters.")}),
			run:         handler(createAssetGroup),
		},
		tool{
			Name:        "tripo_rename_asset_group",
			Description: "Rename a group; its members stay. A name used by another group is refused, and groups are never merged. Free, local only.",
			InputSchema: object([]string{"group", "new_name"}, props{
				"group":    str("Group id or current name."),
				"new_name": str("New name, 1-80 characters."),
			}),
			run: handler(renameAssetGroup),
		},
		tool{
			Name:        "tripo_set_asset_group",
			Description: "Put projects and/or jobs (by operator_id) into a group. Each project or job has one group, so members move out of their previous group. Free, local only.",
			InputSchema: object([]string{"group"}, props{
				"group":        str("Group id or name."),
				"project_ids":  strList("Studio project_ids to put in the group."),
				"operator_ids": strList("operator_ids of jobs to put in the group."),
			}),
			run: handler(setAssetGroup),
		},
		tool{
			Name:        "tripo_list_group_assets",
			Description: "List a group's projects and jobs. Free, local only; project names are not looked up.",
			InputSchema: object([]string{"group"}, props{"group": str("Group id or name.")}),
			UI:          true,
			run:         handler(listGroupAssets),
		},
		tool{
			Name:        "tripo_open_in_studio",
			Description: "Return the Studio workspace URL, or the URL of one project. It does not open a browser. Free.",
			InputSchema: object(nil, props{"project_id": str("Project to link to; default the workspace.")}),
			run:         handler(openInStudio),
		},
		tool{
			Name:        "tripo_list_prices",
			Description: "Show the credit price table (per Studio price key, with its date) and the membership discounts. Free, local only.",
			InputSchema: object(nil),
			run:         handler(listPrices),
		},
	)
}

// ---- job log ----

type listJobsArgs struct {
	Group string `json:"group"`
	Limit int    `json:"limit"`
}

func listJobs(_ context.Context, a *app, x listJobsArgs) (reply, error) {
	jobs, err := a.jobs()
	if err != nil {
		return reply{}, err
	}
	in := func(jobRecord) bool { return true }
	scope := ""
	if ref := strings.TrimSpace(x.Group); ref != "" {
		doc, err := a.loadGroups()
		if err != nil {
			return reply{}, err
		}
		name, members := ref, map[string]bool{}
		if _, g, ok := doc.resolve(ref); ok {
			name = g.Name
			for _, op := range g.Jobs {
				members[op] = true
			}
		}
		// A job is in the group when the group lists its operator_id, or the log tags it with the group's name.
		in = func(j jobRecord) bool { return members[j.OperatorID] || groupKey(j.Group) == groupKey(name) }
		scope = fmt.Sprintf(" in group %q", name)
	}
	var rows []jobRecord
	for _, j := range jobs {
		if in(j) {
			rows = append(rows, j)
		}
	}
	if len(rows) == 0 {
		return say("No logged jobs" + scope + "."), nil
	}
	limit := x.Limit
	if limit == 0 {
		limit = 20
	}
	limit = min(max(limit, 1), 200)
	shown := rows
	if len(shown) > limit {
		shown = shown[:limit]
	}
	var r reply
	r.line("Jobs 1-%d of %d%s, newest first:", len(shown), len(rows), scope)
	for _, j := range shown {
		jobLines(&r, j)
	}
	if len(rows) > len(shown) {
		r.line("Showing %d of %d; raise limit to see more.", len(shown), len(rows))
	}
	return r, nil
}

type getJobArgs struct {
	OperatorID string `json:"operator_id"`
}

func getJob(_ context.Context, a *app, x getJobArgs) (reply, error) {
	jobs, err := a.jobs()
	if err != nil {
		return reply{}, err
	}
	doc, err := a.loadGroups()
	if err != nil {
		return reply{}, err
	}
	for _, j := range jobs {
		if j.OperatorID != x.OperatorID {
			continue
		}
		group := orDash(j.Group)
		for _, g := range doc.Groups {
			if slices.Contains(g.Jobs, j.OperatorID) {
				group = g.Name
			}
		}
		var r reply
		jobLines(&r, j)
		r.line("  tool %s  group %s", orDash(j.Tool), group)
		return r, nil
	}
	return reply{}, fmt.Errorf("no logged job with operator_id %q", x.OperatorID)
}

// jobLines writes one job as two lines.
func jobLines(r *reply, j jobRecord) {
	r.line("- %s  %q  %s  %s", localTime(j.Time), j.Title, creditsText(j.Credits), orDash(j.Status))
	r.line("  project_id %s  operator_id %s", orDash(j.ProjectID), j.OperatorID)
	if j.Saved != "" {
		r.line("  saved %s", j.Saved)
	}
}

// ---- asset groups ----

// assetGroup is one entry of <data dir>/groups.json. Projects are Studio
// project_ids; Jobs are operator_ids from the job log.
type assetGroup struct {
	Name     string   `json:"name"`
	Projects []string `json:"projects"`
	Jobs     []string `json:"jobs"`
}

type groupsDoc struct {
	Groups map[string]*assetGroup `json:"groups"`
}

// ponytail: serialises writes inside this process only; two server processes
// saving at once can still lose an update. Add a lock file if that happens.
var groupsMu sync.Mutex

func (a *app) groupsPath() string { return filepath.Join(a.dir, "groups.json") }

func (a *app) loadGroups() (groupsDoc, error) {
	doc := groupsDoc{Groups: map[string]*assetGroup{}}
	b, err := os.ReadFile(a.groupsPath())
	if errors.Is(err, fs.ErrNotExist) {
		return doc, nil
	}
	if err != nil {
		return doc, err
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return doc, fmt.Errorf("%s: %w", a.groupsPath(), err)
	}
	if doc.Groups == nil {
		doc.Groups = map[string]*assetGroup{}
	}
	return doc, nil
}

// saveGroups writes a temp file and renames it over groups.json, so a crash
// leaves either the old file or the new one.
func (a *app) saveGroups(doc groupsDoc) error {
	if err := os.MkdirAll(a.dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(a.dir, ".groups-*.tmp")
	if err != nil {
		return err
	}
	_, err = tmp.Write(append(b, '\n'))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), a.groupsPath())
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
}

// editGroups loads the groups under the lock, runs f and saves only if f succeeds.
func (a *app) editGroups(f func(*groupsDoc) error) error {
	groupsMu.Lock()
	defer groupsMu.Unlock()
	doc, err := a.loadGroups()
	if err != nil {
		return err
	}
	if err := f(&doc); err != nil {
		return err
	}
	return a.saveGroups(doc)
}

// resolve finds a group by id, or else by name ignoring case and extra spaces.
func (d groupsDoc) resolve(ref string) (string, *assetGroup, bool) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", nil, false
	}
	if g, ok := d.Groups[ref]; ok {
		return ref, g, true
	}
	if id, ok := d.byName(ref); ok {
		return id, d.Groups[id], true
	}
	return "", nil, false
}

func (d groupsDoc) byName(name string) (string, bool) {
	key := groupKey(name)
	for id, g := range d.Groups {
		if groupKey(g.Name) == key {
			return id, true
		}
	}
	return "", false
}

// sortedIDs lists the group ids in name order.
func (d groupsDoc) sortedIDs() []string {
	ids := make([]string, 0, len(d.Groups))
	for id := range d.Groups {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		ni, nj := groupKey(d.Groups[ids[i]].Name), groupKey(d.Groups[ids[j]].Name)
		if ni != nj {
			return ni < nj
		}
		return ids[i] < ids[j]
	})
	return ids
}

// groupKey is the comparison form of a group name: case and extra spaces ignored.
func groupKey(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }

// cleanGroupName trims and collapses spaces. A name is 1-80 characters with no control characters.
func cleanGroupName(s string) (string, error) {
	name := strings.Join(strings.Fields(s), " ")
	switch {
	case name == "":
		return "", errors.New("the group name is empty")
	case utf8.RuneCountInString(name) > 80:
		return "", errors.New("the group name is longer than 80 characters")
	case strings.IndexFunc(name, unicode.IsControl) >= 0:
		return "", errors.New("the group name has control characters")
	}
	return name, nil
}

// cleanIDs trims the ids, drops blanks and repeats, and keeps the order.
func cleanIDs(ids []string) []string {
	out, seen := []string{}, map[string]bool{}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// dropIDs returns list without the ids in drop. It is never nil, so it stays a JSON array.
func dropIDs(list, drop []string) []string {
	skip := map[string]bool{}
	for _, id := range drop {
		skip[id] = true
	}
	out := []string{}
	for _, id := range list {
		if !skip[id] {
			out = append(out, id)
		}
	}
	return out
}

// addIDs adds add to list; an id already in list moves to the end, so no duplicates.
func addIDs(list, add []string) []string { return append(dropIDs(list, add), add...) }

// noGroup is the error for a group reference that matches nothing.
func noGroup(ref string) error {
	return fmt.Errorf("no asset group %q; list them with tripo_list_asset_groups", ref)
}

type nameArgs struct {
	Name string `json:"name"`
}

func createAssetGroup(_ context.Context, a *app, x nameArgs) (reply, error) {
	name, err := cleanGroupName(x.Name)
	if err != nil {
		return reply{}, err
	}
	var id string
	existed := false
	err = a.editGroups(func(d *groupsDoc) error {
		if id, existed = d.byName(name); existed {
			return nil
		}
		id = "group_" + newUUID()
		d.Groups[id] = &assetGroup{Name: name, Projects: []string{}, Jobs: []string{}}
		return nil
	})
	if err != nil {
		return reply{}, err
	}
	if existed {
		return say(fmt.Sprintf("Group %q already exists: %s.", name, id)), nil
	}
	return say(fmt.Sprintf("Created group %q: %s.", name, id)), nil
}

type renameArgs struct {
	Group   string `json:"group"`
	NewName string `json:"new_name"`
}

func renameAssetGroup(_ context.Context, a *app, x renameArgs) (reply, error) {
	name, err := cleanGroupName(x.NewName)
	if err != nil {
		return reply{}, err
	}
	var msg string
	err = a.editGroups(func(d *groupsDoc) error {
		id, g, ok := d.resolve(x.Group)
		if !ok {
			return noGroup(x.Group)
		}
		if other, taken := d.byName(name); taken && other != id {
			return fmt.Errorf("another asset group already uses the name %q; choose another name, or move its members with tripo_set_asset_group", name)
		}
		msg = fmt.Sprintf("Renamed group %q to %q: %s.", g.Name, name, id)
		g.Name = name
		return nil
	})
	if err != nil {
		return reply{}, err
	}
	return say(msg), nil
}

type setArgs struct {
	Group       string   `json:"group"`
	ProjectIDs  []string `json:"project_ids"`
	OperatorIDs []string `json:"operator_ids"`
}

func setAssetGroup(_ context.Context, a *app, x setArgs) (reply, error) {
	projects, jobs := cleanIDs(x.ProjectIDs), cleanIDs(x.OperatorIDs)
	if len(projects)+len(jobs) == 0 {
		return reply{}, errors.New("pass project_ids and/or operator_ids")
	}
	var msg string
	err := a.editGroups(func(d *groupsDoc) error {
		id, g, ok := d.resolve(x.Group)
		if !ok {
			return noGroup(x.Group)
		}
		// ponytail: ids are stored as given; they are not checked against Studio (no network here).
		for other, og := range d.Groups {
			if other != id {
				og.Projects = dropIDs(og.Projects, projects)
				og.Jobs = dropIDs(og.Jobs, jobs)
			}
		}
		g.Projects = addIDs(g.Projects, projects)
		g.Jobs = addIDs(g.Jobs, jobs)
		msg = fmt.Sprintf("Group %q now has %d projects and %d jobs.", g.Name, len(g.Projects), len(g.Jobs))
		return nil
	})
	if err != nil {
		return reply{}, err
	}
	return say(msg), nil
}

type groupArgs struct {
	Group string `json:"group"`
}

func listAssetGroups(_ context.Context, a *app, _ noArgs) (reply, error) {
	doc, err := a.loadGroups()
	if err != nil {
		return reply{}, err
	}
	if len(doc.Groups) == 0 {
		return say("No asset groups yet. Make one with tripo_create_asset_group."), nil
	}
	var r reply
	ids := doc.sortedIDs()
	r.line("Asset groups (%d):", len(ids))
	for _, id := range ids {
		g := doc.Groups[id]
		r.line("- %s  %q  %d projects, %d jobs", id, g.Name, len(g.Projects), len(g.Jobs))
	}
	return r, nil
}

func listGroupAssets(_ context.Context, a *app, x groupArgs) (reply, error) {
	doc, err := a.loadGroups()
	if err != nil {
		return reply{}, err
	}
	id, g, ok := doc.resolve(x.Group)
	if !ok {
		return reply{}, noGroup(x.Group)
	}
	r := reply{Card: &card{Kind: "projects", Title: g.Name, Summary: fmt.Sprintf("%d projects · %d jobs", len(g.Projects), len(g.Jobs))}}
	r.line("Group %q (%s): %d projects, %d jobs.", g.Name, id, len(g.Projects), len(g.Jobs))
	// Names are not stored, so each item's Name is its project_id; no API call per project.
	for _, p := range g.Projects {
		r.Card.Items = append(r.Card.Items, &projectCard{ID: p, Name: p, URL: studioURL(p)})
		r.line("- project %s", p)
	}
	if len(g.Jobs) > 0 {
		jobs, err := a.jobs()
		if err != nil {
			return reply{}, err
		}
		logged := map[string]jobRecord{}
		for _, j := range jobs {
			logged[j.OperatorID] = j
		}
		for _, op := range g.Jobs {
			if j, ok := logged[op]; ok {
				r.line("- job %s  %q  %s", op, j.Title, orDash(j.Status))
			} else {
				r.line("- job %s", op)
			}
		}
	}
	return r, nil
}

// ---- links and prices ----

type openArgs struct {
	ProjectID string `json:"project_id"`
}

func openInStudio(_ context.Context, _ *app, x openArgs) (reply, error) {
	if x.ProjectID == "" {
		return say("Studio workspace: " + studioOrigin + "/workspace/generate"), nil
	}
	return say("Studio project: " + studioURL(x.ProjectID)), nil
}

func listPrices(_ context.Context, a *app, _ noArgs) (reply, error) {
	t, err := a.prices()
	if err != nil {
		return reply{}, err
	}
	keys := make([]string, 0, len(t.Credits))
	for k := range t.Credits {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var r reply
	r.line("Studio prices, credits per price key (dated %s):", t.Updated)
	r.line("%-34s %8s  %s", "key", "credits", "date")
	for _, k := range keys {
		r.line("%-34s %8s  %s", k, thousands(t.Credits[k]), t.Updated)
	}
	r.line("")
	members := make([]string, 0, len(t.Discounts))
	for m := range t.Discounts {
		members = append(members, m)
	}
	sort.Strings(members)
	r.line("Membership discounts (the factor multiplies image-generation prices):")
	for _, m := range members {
		r.line("- %s  × %g", m, t.Discounts[m])
	}
	r.line("Image-generation prices are multiplied by the factor for the account's membership. This tool makes no network call, so it does not know which membership applies.")
	return r, nil
}

// ---- helpers ----

func localTime(s string) string {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.Local().Format("2006-01-02 15:04")
	}
	return s
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func creditsText(n int) string {
	if n < 0 {
		return "credits unknown"
	}
	return thousands(n) + " credits"
}
