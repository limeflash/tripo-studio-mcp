package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// jobRecord is one line of <data dir>/jobs.jsonl: a submitted paid job, or a
// later update to it with the same operator_id.
type jobRecord struct {
	Time       string `json:"time"`
	Tool       string `json:"tool,omitempty"`
	Title      string `json:"title,omitempty"`
	Summary    string `json:"summary,omitempty"`
	Credits    int    `json:"credits,omitempty"`
	ProjectID  string `json:"project_id,omitempty"`
	OperatorID string `json:"operator_id"`
	Status     string `json:"status,omitempty"`
	Saved      string `json:"saved,omitempty"`
	Group      string `json:"group,omitempty"` // character / asset group name
}

var jobsMu sync.Mutex

func (a *app) jobsPath() string { return filepath.Join(a.dir, "jobs.jsonl") }

// logJob appends to the job log. A failure to log never fails the job itself.
func (a *app) logJob(r jobRecord) {
	r.Time = time.Now().UTC().Format(time.RFC3339)
	b, err := json.Marshal(r)
	if err != nil {
		return
	}
	jobsMu.Lock()
	defer jobsMu.Unlock()
	if os.MkdirAll(a.dir, 0o700) != nil {
		return
	}
	f, err := os.OpenFile(a.jobsPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(append(b, '\n'))
}

// jobs returns the logged jobs, updates merged in, newest first.
func (a *app) jobs() ([]jobRecord, error) {
	f, err := os.Open(a.jobsPath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	byOp := map[string]*jobRecord{}
	var order []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var r jobRecord
		if json.Unmarshal(sc.Bytes(), &r) != nil || r.OperatorID == "" {
			continue
		}
		cur, ok := byOp[r.OperatorID]
		if !ok {
			cp := r
			byOp[r.OperatorID] = &cp
			order = append(order, r.OperatorID)
			continue
		}
		merge(cur, r)
	}
	out := make([]jobRecord, 0, len(order))
	for _, op := range order {
		out = append(out, *byOp[op])
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time > out[j].Time })
	return out, sc.Err()
}

// merge applies an update's non-empty fields; the submit time is kept.
func merge(cur *jobRecord, u jobRecord) {
	for _, f := range []struct {
		dst *string
		src string
	}{
		{&cur.Tool, u.Tool}, {&cur.Title, u.Title}, {&cur.Summary, u.Summary}, {&cur.ProjectID, u.ProjectID},
		{&cur.Status, u.Status}, {&cur.Saved, u.Saved}, {&cur.Group, u.Group},
	} {
		if f.src != "" {
			*f.dst = f.src
		}
	}
	if u.Credits != 0 {
		cur.Credits = u.Credits
	}
}
