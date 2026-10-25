package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Tripo Studio's internal web API, as the studio.tripo3d.ai site calls it.
const (
	apiBase      = "https://api.tripo3d.ai"
	studioOrigin = "https://studio.tripo3d.ai"
	whoamiURL    = "https://auth.tripo3d.ai/sessions/whoami"
	userAgent    = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36"

	nexusP1                  = "Nexus-v1.0-20260214"
	nexusP2                  = "Nexus-v2.0-20260801"
	highDetailH31            = "v3.1-20260211"
	textureModelVersion      = "v3.0-20250812"
	segmentationModelVersion = "v2.0-20260430"
)

var (
	errNoSession = errors.New("not signed in to Tripo Studio: run tripo_auth_import (cookie_file, cookie, or from_plugin=true)")
	errAuth      = errors.New("Tripo Studio rejected the session (HTTP 401/403); it expired or was signed out. Sign in at studio.tripo3d.ai and run tripo_auth_import with a fresh ory_kratos_session cookie")
	started      = time.Now()
)

// apiError is a request Tripo answered and refused: nothing was accepted or charged.
type apiError struct {
	Path string
	Code int
	Msg  string
}

func (e *apiError) Error() string {
	if e.Code == 2010 {
		return "not enough Studio credits (Tripo code 2010)"
	}
	return fmt.Sprintf("Tripo refused %s: %s (code %d)", e.Path, e.Msg, e.Code)
}

type app struct {
	dir string
	api *http.Client // Studio API: never follows redirects
	dl  *http.Client // signed artifact URLs and S3

	preview *previewServer
}

func newApp() (*app, error) {
	dir := os.Getenv("TRIPO_MCP_DATA_DIR")
	if dir == "" {
		base, err := os.UserCacheDir() // %LOCALAPPDATA% on Windows
		if err != nil {
			return nil, err
		}
		dir = filepath.Join(base, "TripoStudioMCP")
	}
	return &app{
		dir: dir,
		api: &http.Client{
			Timeout:       2 * time.Minute,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		dl: &http.Client{
			Timeout:       20 * time.Minute,
			CheckRedirect: func(r *http.Request, _ []*http.Request) error { return checkDownloadURL(r.URL) },
		},
	}, nil
}

// ---- session ----

type session struct {
	Cookie     string `json:"cookie"`
	DeviceID   string `json:"device_id"`
	ExpiresAt  string `json:"expires_at,omitempty"`
	Source     string `json:"source"`
	ImportedAt string `json:"imported_at"`
}

func (a *app) sessionPath() string { return filepath.Join(a.dir, "session.json") }

func (a *app) loadSession() (*session, error) {
	b, err := os.ReadFile(a.sessionPath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errNoSession
	}
	if err != nil {
		return nil, err
	}
	var s session
	if json.Unmarshal(b, &s) != nil || s.Cookie == "" {
		return nil, fmt.Errorf("%s is damaged; run tripo_auth_import again", a.sessionPath())
	}
	if t, err := time.Parse(time.RFC3339, s.ExpiresAt); err == nil && time.Now().After(t) {
		return nil, fmt.Errorf("the Studio session expired on %s; sign in at studio.tripo3d.ai and run tripo_auth_import again", t.Local().Format("2006-01-02 15:04"))
	}
	return &s, nil
}

// cookieValue accepts the bare ory_kratos_session value or a whole Cookie header.
func cookieValue(v string) string {
	v = strings.TrimSpace(v)
	for _, part := range strings.Split(v, ";") {
		if k, val, ok := strings.Cut(strings.TrimSpace(part), "="); ok && strings.TrimSpace(k) == "ory_kratos_session" {
			return strings.TrimSpace(val)
		}
	}
	return v
}

func (a *app) importSession(ctx context.Context, cookie, file string, fromPlugin bool) error {
	n := 0
	for _, set := range []bool{cookie != "", file != "", fromPlugin} {
		if set {
			n++
		}
	}
	if n != 1 {
		return errors.New("pass exactly one of cookie_file, cookie or from_plugin")
	}
	s := &session{DeviceID: newUUID()}
	switch {
	case file != "":
		b, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		s.Cookie, s.Source = cookieValue(string(b)), "cookie file "+filepath.Base(file)
	case cookie != "":
		s.Cookie, s.Source = cookieValue(cookie), "pasted cookie"
	default:
		base, _ := os.UserCacheDir()
		p := filepath.Join(base, "TripoStudioPlugin", "session.json")
		b, err := os.ReadFile(p)
		if err != nil {
			return fmt.Errorf("no tripo-studio-plugin session: %w", err)
		}
		var ps struct {
			Kind        string `json:"kind"`
			Cookie      string `json:"cookie"`
			DeviceID    string `json:"device_id"`
			ExpiresAtMS int64  `json:"expires_at_ms"`
		}
		if json.Unmarshal(b, &ps) != nil || ps.Kind != "cookie" || ps.Cookie == "" {
			return fmt.Errorf("%s holds no cookie session", p)
		}
		s.Cookie, s.Source = ps.Cookie, "tripo-studio-plugin session"
		if ps.DeviceID != "" {
			s.DeviceID = ps.DeviceID
		}
		if ps.ExpiresAtMS > 0 {
			s.ExpiresAt = time.UnixMilli(ps.ExpiresAtMS).UTC().Format(time.RFC3339)
		}
	}
	if s.Cookie == "" || len(s.Cookie) > 8192 || strings.ContainsAny(s.Cookie, " ;,\"\r\n\t") {
		return errors.New("that does not look like an ory_kratos_session cookie value")
	}
	if err := a.callAs(ctx, s, "GET", "/v2/studio/user/profile/payment", nil, nil); err != nil {
		return fmt.Errorf("Tripo did not accept this session: %w", err)
	}
	if exp := a.whoamiExpiry(ctx, s.Cookie); exp != "" {
		s.ExpiresAt = exp
	}
	s.ImportedAt = time.Now().UTC().Format(time.RFC3339)
	if err := os.MkdirAll(a.dir, 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(s, "", "  ")
	return os.WriteFile(a.sessionPath(), b, 0o600)
}

// whoamiExpiry asks Tripo's auth service when the session ends; "" if it won't say.
func (a *app) whoamiExpiry(ctx context.Context, cookie string) string {
	req, err := http.NewRequestWithContext(ctx, "GET", whoamiURL, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Cookie", "ory_kratos_session="+cookie)
	req.Header.Set("Accept", "application/json")
	resp, err := a.api.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var w struct {
		ExpiresAt string `json:"expires_at"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&w) != nil {
		return ""
	}
	if t, err := time.Parse(time.RFC3339, w.ExpiresAt); err == nil {
		return t.UTC().Format(time.RFC3339)
	}
	return ""
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// ---- API ----

func (a *app) call(ctx context.Context, method, path string, body, out any) error {
	s, err := a.loadSession()
	if err != nil {
		return err
	}
	err = a.callAs(ctx, s, method, path, body, out)
	var refused *apiError
	if err != nil && method == "GET" && !errors.Is(err, errAuth) && !errors.As(err, &refused) && ctx.Err() == nil {
		time.Sleep(time.Second) // reads are safe to repeat; Tripo's gateway sometimes answers 502
		err = a.callAs(ctx, s, method, path, body, out)
	}
	return err
}

// credits reads the wallet balance; ok is false when Tripo does not say.
func (a *app) credits(ctx context.Context) (n int, ok bool) {
	var pay struct {
		Wallet struct {
			Total *float64 `json:"total_credit"`
		} `json:"wallet"`
	}
	if a.call(ctx, "GET", "/v2/studio/user/profile/payment", nil, &pay) != nil || pay.Wallet.Total == nil {
		return 0, false
	}
	return int(*pay.Wallet.Total), true
}

func (a *app) callAs(ctx context.Context, s *session, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, apiBase+path, rd)
	if err != nil {
		return err
	}
	h := req.Header
	h.Set("Accept", "application/json")
	if body != nil {
		h.Set("Content-Type", "application/json")
	}
	h.Set("Cookie", "ory_kratos_session="+s.Cookie)
	h.Set("Origin", studioOrigin)
	h.Set("Referer", studioOrigin+"/")
	h.Set("User-Agent", userAgent)
	h.Set("X-Tripo-Device-Id", s.DeviceID)
	h.Set("X-Tripo-Region", "rg1")
	h.Set("X-Tripo-Start", strconv.FormatFloat(float64(time.Since(started).Microseconds())/1000, 'f', 1, 64))
	resp, err := a.api.Do(req)
	if err != nil {
		return fmt.Errorf("no answer from Tripo for %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return errAuth
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return fmt.Errorf("reading Tripo's answer for %s: %w", path, err)
	}
	var env struct {
		Code    *int            `json:"code"`
		Data    json.RawMessage `json:"data"`
		Message string          `json:"message"`
		Msg     string          `json:"msg"`
	}
	if json.Unmarshal(raw, &env) != nil || env.Code == nil {
		return fmt.Errorf("Tripo answered %s with HTTP %d and no API envelope", path, resp.StatusCode)
	}
	if *env.Code != 0 {
		msg := env.Message
		if msg == "" {
			msg = env.Msg
		}
		return &apiError{Path: path, Code: *env.Code, Msg: msg}
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Tripo answered %s with HTTP %d", path, resp.StatusCode)
	}
	if out != nil && len(env.Data) > 0 && string(env.Data) != "null" {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return fmt.Errorf("unexpected data from %s (the site API may have changed): %v", path, err)
		}
	}
	return nil
}

type receipt struct {
	OperatorID string `json:"operator_id"`
	ProjectID  string `json:"project_id"`
}

// submit sends a paid request. A refusal means nothing was charged; any other
// failure is ambiguous (the job may exist and be charged), so it is never retried.
func (a *app) submit(ctx context.Context, path string, body any) (receipt, error) {
	var raw json.RawMessage
	err := a.call(ctx, "POST", path, body, &raw)
	var refused *apiError
	if errors.As(err, &refused) || errors.Is(err, errAuth) || errors.Is(err, errNoSession) {
		return receipt{}, fmt.Errorf("not submitted, nothing charged: %w", err)
	}
	if err != nil {
		return receipt{}, fmt.Errorf("outcome unknown: %v. The job may have been accepted and charged; check tripo_list_projects before submitting again", err)
	}
	var list []receipt
	if json.Unmarshal(raw, &list) != nil {
		var one receipt
		if json.Unmarshal(raw, &one) == nil {
			list = []receipt{one}
		}
	}
	if len(list) == 0 || list[0].OperatorID == "" {
		return receipt{}, fmt.Errorf("outcome unknown: Tripo accepted %s but returned no operator_id; check tripo_list_projects before submitting again", path)
	}
	return list[0], nil
}

// ---- images ----

type localImage struct {
	path   string
	data   []byte
	format string // png, jpeg or webp, as Tripo's upload token names them
}

func readImage(path string) (*localImage, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("cannot read image: %w", err)
	}
	if len(b) > 20<<20 {
		return nil, fmt.Errorf("%s is over Tripo's 20 MB image limit", abs)
	}
	img := &localImage{path: abs, data: b}
	switch {
	case bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")):
		img.format = "png"
	case bytes.HasPrefix(b, []byte{0xFF, 0xD8, 0xFF}):
		img.format = "jpeg"
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		img.format = "webp"
	default:
		return nil, fmt.Errorf("%s is not a PNG, JPEG or WebP image", abs)
	}
	return img, nil
}

// imageRef is how Studio requests reference an uploaded image.
type imageRef struct {
	Bucket string `json:"bucket"`
	Audit  string `json:"image_audit_result"`
	Source string `json:"image_source"`
	Key    string `json:"key"`
}

var bucketName = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

// upload puts the image in Tripo's S3 storage with a short-lived STS token,
// then runs Studio's content check, as the website does before any job.
func (a *app) upload(ctx context.Context, img *localImage) (imageRef, error) {
	var tok struct {
		Bucket  string `json:"resource_bucket"`
		Key     string `json:"resource_uri"`
		Session string `json:"session_token"`
		AK      string `json:"sts_ak"`
		SK      string `json:"sts_sk"`
	}
	if err := a.call(ctx, "POST", "/v2/studio/storage/temporary_token", map[string]string{"client": "aws", "format": img.format}, &tok); err != nil {
		return imageRef{}, err
	}
	if !bucketName.MatchString(tok.Bucket) || tok.Key == "" || strings.HasPrefix(tok.Key, "/") || tok.AK == "" {
		return imageRef{}, errors.New("Tripo returned an unusable upload token (the site API may have changed)")
	}
	ct := map[string]string{"png": "image/png", "jpeg": "image/jpeg", "webp": "image/webp"}[img.format]
	if err := a.s3Put(ctx, tok.Bucket, tok.Key, tok.AK, tok.SK, tok.Session, img.data, ct); err != nil {
		return imageRef{}, fmt.Errorf("uploading %s: %w", filepath.Base(img.path), err)
	}
	var audit struct {
		Result string `json:"result"`
	}
	if err := a.call(ctx, "POST", "/v2/studio/audit/image", map[string]any{"image": map[string]string{"bucket": tok.Bucket, "key": tok.Key}}, &audit); err != nil {
		return imageRef{}, err
	}
	if audit.Result != "pass" && audit.Result != "sensitive" {
		return imageRef{}, fmt.Errorf("Tripo's content check returned %q for %s; nothing was submitted", audit.Result, filepath.Base(img.path))
	}
	return imageRef{Bucket: tok.Bucket, Audit: audit.Result, Source: "upload", Key: tok.Key}, nil
}

func (a *app) s3Put(ctx context.Context, bucket, key, ak, sk, token string, body []byte, contentType string) error {
	var last error
	// Transfer Acceleration first, like the website's SDK, then the bucket's region.
	for _, host := range []string{bucket + ".s3-accelerate.amazonaws.com", bucket + ".s3.us-west-2.amazonaws.com"} {
		uri := "/" + awsEscape(key)
		req, err := http.NewRequestWithContext(ctx, "PUT", "https://"+host+uri, bytes.NewReader(body))
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		hdr := map[string]string{
			"content-type":         contentType,
			"host":                 host,
			"x-amz-content-sha256": sha256Hex(body),
			"x-amz-date":           now.Format("20060102T150405Z"),
			"x-amz-security-token": token,
		}
		for k, v := range hdr {
			if k != "host" {
				req.Header.Set(k, v)
			}
		}
		req.Header.Set("Authorization", sigV4("PUT", uri, "", hdr, ak, sk, "us-west-2", "s3", now))
		resp, err := a.dl.Do(req)
		if err != nil {
			last = err
			continue
		}
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return nil
		}
		code := regexp.MustCompile(`<Code>([^<]*)</Code>`).FindSubmatch(msg)
		last = fmt.Errorf("storage answered HTTP %d", resp.StatusCode)
		if code != nil {
			last = fmt.Errorf("storage answered HTTP %d %s", resp.StatusCode, code[1])
		}
	}
	return last
}

// sigV4 returns the AWS Signature Version 4 Authorization header. headers must
// hold every signed header, lower-case, including host and x-amz-content-sha256.
func sigV4(method, uri, query string, headers map[string]string, ak, sk, region, service string, t time.Time) string {
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var canon strings.Builder
	for _, k := range names {
		canon.WriteString(k + ":" + strings.TrimSpace(headers[k]) + "\n")
	}
	signed := strings.Join(names, ";")
	creq := strings.Join([]string{method, uri, query, canon.String(), signed, headers["x-amz-content-sha256"]}, "\n")
	date := t.Format("20060102")
	scope := date + "/" + region + "/" + service + "/aws4_request"
	toSign := "AWS4-HMAC-SHA256\n" + t.Format("20060102T150405Z") + "\n" + scope + "\n" + sha256Hex([]byte(creq))
	key := hmacSHA256([]byte("AWS4"+sk), date)
	for _, part := range []string{region, service, "aws4_request"} {
		key = hmacSHA256(key, part)
	}
	return fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%x", ak, scope, signed, hmacSHA256(key, toSign))
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// awsEscape encodes an S3 object key for the canonical URI, keeping '/'.
func awsEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || strings.IndexByte("-_.~/", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// ---- jobs and projects ----

type progress struct {
	ID         string  `json:"id"`
	OperatorID string  `json:"operator_id"`
	ProjectID  string  `json:"project_id"`
	Status     string  `json:"status"`
	Progress   float64 `json:"progress"`
	LeftTime   any     `json:"left_time"`
	Reason     any     `json:"reason"`
}

func (p progress) done() bool {
	switch p.Status {
	case "success", "failed", "cancelled", "expired", "banned":
		return true
	}
	return false
}

func (a *app) progress(ctx context.Context, op string) (progress, error) {
	var items []progress
	if err := a.call(ctx, "POST", "/v2/studio/progress", map[string]any{"ids": []string{op}}, &items); err != nil {
		return progress{}, err
	}
	for _, it := range items {
		if it.OperatorID == op || it.ID == op {
			return it, nil
		}
	}
	if len(items) == 1 && items[0].OperatorID == "" && items[0].ID == "" {
		return items[0], nil
	}
	return progress{}, fmt.Errorf("Tripo has no job with operator_id %s", op)
}

// wait polls a job until it ends or the timeout passes; timeout 0 checks once.
func (a *app) wait(ctx context.Context, op string, timeout time.Duration) (progress, error) {
	deadline := time.Now().Add(timeout)
	misses := 0
	for {
		it, err := a.progress(ctx, op)
		if err == nil {
			misses = 0
			if it.done() || time.Now().After(deadline) {
				return it, nil
			}
		} else if misses++; errors.Is(err, errAuth) || errors.Is(err, errNoSession) || misses >= 5 || time.Now().After(deadline) {
			return it, err
		}
		select {
		case <-ctx.Done():
			return it, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

type project struct {
	raw        map[string]any
	ID         string
	Name       string
	Status     string
	ModelURL   string
	OperatorID string // the project's current version
}

func (a *app) project(ctx context.Context, id, operatorID string) (*project, error) {
	if id == "" {
		return nil, errors.New("project_id is required")
	}
	path := "/v2/studio/project/detail/v3/" + url.PathEscape(id)
	if operatorID != "" {
		path += "?operator_id=" + url.QueryEscape(operatorID)
	}
	var raw map[string]any
	if err := a.call(ctx, "GET", path, nil, &raw); err != nil {
		return nil, err
	}
	p := &project{raw: raw, ID: asString(raw["id"]), Name: asString(raw["project_name"]), Status: asString(raw["status"]), ModelURL: asString(raw["model_url"])}
	if op, ok := raw["operator"].(map[string]any); ok {
		p.OperatorID = asString(op["operator_id"])
	}
	if p.ID != "" && p.ID != id {
		return nil, fmt.Errorf("Tripo returned project %s when asked for %s", p.ID, id)
	}
	p.ID = id
	return p, nil
}

// flag reads a capability flag from the operator, its metadata, then the
// project root, as Studio's own UI does. nil means Tripo did not say.
func (p *project) flag(name string) *bool {
	op, _ := p.raw["operator"].(map[string]any)
	meta, _ := op["metadata"].(map[string]any)
	for _, m := range []map[string]any{op, meta, p.raw} {
		if v, ok := m[name].(bool); ok {
			return &v
		}
	}
	return nil
}

func (p *project) is(name string) bool { f := p.flag(name); return f != nil && *f }

// preflight loads a project that a paid post-process is about to change.
func (a *app) preflight(ctx context.Context, id string) (*project, error) {
	p, err := a.project(ctx, id, "")
	if err != nil {
		return nil, err
	}
	switch strings.ToLower(strings.ReplaceAll(p.Status, "-", "_")) {
	case "prepare", "preparing", "queued", "pending", "running", "processing", "generating", "in_progress", "uploading":
		return nil, fmt.Errorf("project %s is still busy (%s); wait for its current job first", id, p.Status)
	}
	if f := p.flag("is_owner"); f != nil && !*f {
		return nil, errors.New("this project belongs to another account")
	}
	return p, nil
}

func asString(v any) string { s, _ := v.(string); return s }

// ---- downloads ----

var downloadHosts = []string{"tripo3d.ai", "tripo3d.com", "amazonaws.com", "cloudfront.net"}

func checkDownloadURL(u *url.URL) error {
	if u.Scheme != "https" {
		return errors.New("refusing a non-HTTPS download URL")
	}
	h := strings.ToLower(u.Hostname())
	for _, s := range downloadHosts {
		if h == s || strings.HasSuffix(h, "."+s) {
			return nil
		}
	}
	return fmt.Errorf("refusing a download from unexpected host %s", h)
}

func (a *app) open(ctx context.Context, rawURL string) (io.ReadCloser, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if err := checkDownloadURL(u); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := a.dl.Do(req) // signed URL: no session cookie
	if err != nil {
		return nil, fmt.Errorf("download failed: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("download answered HTTP %d", resp.StatusCode)
	}
	return resp.Body, nil
}

var modelExt = map[string]string{"glb": ".glb", "fbx": ".fbx", "zip": ".zip"}

func sniff(head []byte) string {
	switch {
	case bytes.HasPrefix(head, []byte("glTF")):
		return "glb"
	case bytes.HasPrefix(head, []byte("Kaydara FBX Binary")):
		return "fbx"
	case bytes.HasPrefix(head, []byte("PK\x03\x04")):
		return "zip"
	}
	return ""
}

// save downloads rawURL to path. When the bytes are another format than the
// extension says (Smart Mesh quad results are FBX), the extension is fixed.
func (a *app) save(ctx context.Context, rawURL, path string) (final string, size int64, kind string, err error) {
	body, err := a.open(ctx, rawURL)
	if err != nil {
		return "", 0, "", err
	}
	defer body.Close()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", 0, "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tripo-*.part")
	if err != nil {
		return "", 0, "", err
	}
	defer os.Remove(tmp.Name())
	br := bufio.NewReader(body)
	head, _ := br.Peek(32)
	kind = sniff(head)
	size, err = io.Copy(tmp, io.LimitReader(br, 4<<30))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", 0, "", fmt.Errorf("download failed: %w", err)
	}
	final = path
	if ext := modelExt[kind]; ext != "" && !strings.EqualFold(filepath.Ext(path), ext) {
		final = strings.TrimSuffix(path, filepath.Ext(path)) + ext
	}
	return final, size, kind, os.Rename(tmp.Name(), final)
}

// outputPath turns a download target into a file path: a folder (existing,
// or ending in a slash) gets "<name><ext>" inside it.
func outputPath(dest, name, ext string) (string, error) {
	if dest == "" {
		return "", errors.New("a download path is required")
	}
	abs, err := filepath.Abs(dest)
	if err != nil {
		return "", err
	}
	if st, err := os.Stat(abs); (err == nil && st.IsDir()) || strings.HasSuffix(dest, "/") || strings.HasSuffix(dest, `\`) {
		return filepath.Join(abs, safeName(name)+ext), nil
	}
	return abs, nil
}

func safeName(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 32 || strings.ContainsRune(`<>:"/\|?*`, r) {
			return '_'
		}
		return r
	}, strings.TrimSpace(s))
	if len(s) > 100 {
		s = s[:100]
	}
	if s == "" {
		s = "model"
	}
	return s
}

// meshNames lists the mesh node names in a GLB, the part names Studio's
// remesh expects.
func meshNames(b []byte) ([]string, error) {
	if len(b) < 20 || binary.LittleEndian.Uint32(b) != 0x46546C67 || binary.LittleEndian.Uint32(b[4:]) != 2 {
		return nil, errors.New("the model is not a GLB file")
	}
	n := uint64(binary.LittleEndian.Uint32(b[12:]))
	if binary.LittleEndian.Uint32(b[16:]) != 0x4E4F534A || 20+n > uint64(len(b)) {
		return nil, errors.New("the GLB has no JSON chunk")
	}
	var doc struct {
		Nodes []struct {
			Name string `json:"name"`
			Mesh *int   `json:"mesh"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(bytes.TrimRight(b[20:20+n], " \x00"), &doc); err != nil {
		return nil, fmt.Errorf("the GLB JSON is invalid: %w", err)
	}
	var names []string
	for _, node := range doc.Nodes {
		if name := strings.TrimSpace(node.Name); name != "" && node.Mesh != nil {
			names = append(names, name)
		}
	}
	return names, nil
}

// exportURL runs Studio's export of the project's current version and returns
// the file URL. Free for the account's own projects.
func (a *app) exportURL(ctx context.Context, p *project, format string, textureSize int) (string, error) {
	wire := map[string]string{"glb": "gltf", "fbx": "fbx", "obj": "obj"}[format]
	if wire == "" {
		return "", fmt.Errorf("format must be glb, fbx or obj, not %q", format)
	}
	if textureSize == 0 {
		textureSize = 2048
		if p.is("is_ultra_textured") {
			textureSize = 8192
		} else if p.is("is_hd_textured") {
			textureSize = 4096
		}
	}
	switch textureSize {
	case 512, 1024, 2048, 4096, 8192:
	default:
		return "", errors.New("texture_size must be 512, 1024, 2048, 4096 or 8192")
	}
	name := safeName(p.Name)
	if p.Name == "" {
		name = "model-" + p.ID
	}
	body := map[string]any{
		"project_id": p.ID, "format": wire, "texture_size": textureSize, "texture_packaging": "zip",
		"fbx_preset": "blender", "with_animation": false, "animations": []string{}, "pack_uv": false,
		"animate_in_place": false, "enable_bake_animation": false, "bake_animation_frame": 0,
		"export_vertex_colors": false, "export_orientation": "-y", "name": name, "model_version": "default",
	}
	var r struct {
		ModelURL   string `json:"model_url"`
		OperatorID string `json:"operator_id"`
	}
	if err := a.call(ctx, "POST", "/v2/studio/operation/export", body, &r); err != nil {
		return "", err
	}
	if r.ModelURL != "" {
		return r.ModelURL, nil
	}
	if r.OperatorID == "" {
		return "", errors.New("Tripo started no export job")
	}
	it, err := a.wait(ctx, r.OperatorID, 10*time.Minute)
	if err != nil {
		return "", err
	}
	if it.Status != "success" {
		return "", fmt.Errorf("export job %s ended as %s", r.OperatorID, it.Status)
	}
	var d struct {
		ModelURL string `json:"model_url"`
	}
	if err := a.call(ctx, "POST", "/v2/studio/operation/download_with_name", map[string]string{"file_name": name, "operator_id": r.OperatorID}, &d); err != nil {
		return "", err
	}
	if d.ModelURL == "" {
		return "", errors.New("the export finished but Tripo gave no file URL")
	}
	return d.ModelURL, nil
}

// ---- prices ----

//go:embed prices.json
var defaultPrices []byte

type priceTable struct {
	Updated   string             `json:"updated"`
	Credits   map[string]int     `json:"credits"`
	Discounts map[string]float64 `json:"discounts"` // image generation, by membership type
}

// prices reads the built-in table, overlaid by <data dir>/prices.json when
// present, on every call so an edited file applies without a restart.
func (a *app) prices() (priceTable, error) {
	var t priceTable
	if err := json.Unmarshal(defaultPrices, &t); err != nil {
		return t, err
	}
	path := filepath.Join(a.dir, "prices.json")
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return t, nil
	}
	var o priceTable
	if err == nil {
		err = json.Unmarshal(b, &o)
	}
	if err != nil {
		return t, fmt.Errorf("%s: %w", path, err)
	}
	for k, v := range o.Credits {
		t.Credits[k] = v
	}
	for k, v := range o.Discounts {
		t.Discounts[k] = v
	}
	if o.Updated != "" {
		t.Updated = o.Updated
	}
	return t, nil
}

// cost prices an operation from its Studio price keys.
func (t priceTable) cost(keys []string) (int, string) {
	total, parts := 0, make([]string, 0, len(keys))
	for _, k := range keys {
		v, ok := t.Credits[k]
		if !ok {
			return -1, "price table has no " + k
		}
		total += v
		parts = append(parts, fmt.Sprintf("%s %d", k, v))
	}
	return total, strings.Join(parts, " + ")
}

// member returns the account's membership type (e.g. "premium"), or "".
func (a *app) member(ctx context.Context) string {
	var pay struct {
		Member struct {
			Type string `json:"type"`
		} `json:"member"`
	}
	if a.call(ctx, "GET", "/v2/studio/user/profile/payment", nil, &pay) != nil {
		return ""
	}
	return pay.Member.Type
}
