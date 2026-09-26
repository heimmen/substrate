// Tests for the managed shared-skills store (skills.go).
package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
)

// buildTgz returns a gzipped tar containing the given files at the archive
// root. files maps archive-relative path -> content. When top==true the whole
// tree is wrapped in a single top-level dir.
func buildTgz(t *testing.T, files map[string]string, top string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		arcName := name
		if top != "" {
			arcName = top + "/" + name
		}
		hdr := &tar.Header{Name: arcName, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func buildZip(t *testing.T, files map[string]string, top string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		arcName := name
		if top != "" {
			arcName = top + "/" + name
		}
		w, err := zw.Create(arcName)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func newTestStore(t *testing.T) *skillStore {
	t.Helper()
	dir := t.TempDir()
	s := newSkillStore(filepath.Join(dir, "skills-root"))
	s.now = func() time.Time { return fixedNow }
	if err := s.ensureDirs(); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSkillInstallAndManifest(t *testing.T) {
	s := newTestStore(t)

	// Install a tgz with a leading top-level dir.
	tgz := buildTgz(t, map[string]string{
		"SKILL.md": "# demo\nHelpers for mofang tables.\n",
		"tool.js":  "console.log('hi');\n",
	}, "demo-skill")
	entry, err := s.install("demo", bytes.NewReader(tgz))
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if entry.Name != "demo" || entry.Size == 0 || entry.SHA256 == "" {
		t.Fatalf("bad entry: %+v", entry)
	}
	if _, err := os.Stat(filepath.Join(s.skillDir("demo"), "SKILL.md")); err != nil {
		t.Fatalf("SKILL.md not extracted: %v", err)
	}

	m, etag, err := s.manifest()
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Skills) != 1 || m.Skills[0].Name != "demo" {
		t.Fatalf("manifest not rebuilt: %+v", m.Skills)
	}
	if m.Version != etag {
		t.Fatalf("etag != version: %q vs %q", etag, m.Version)
	}
	if !strings.HasPrefix(m.Version, "sha256:") {
		t.Fatalf("version should be sha256-prefixed: %q", m.Version)
	}

	// Reinstall the same bytes: version must be stable (content-addressed).
	entry2, err := s.install("demo", bytes.NewReader(tgz))
	if err != nil {
		t.Fatalf("reinstall: %v", err)
	}
	if entry2.SHA256 != entry.SHA256 {
		t.Fatalf("content-addressed hash changed: %q vs %q", entry.SHA256, entry2.SHA256)
	}
}

func TestSkillInstallZip(t *testing.T) {
	s := newTestStore(t)
	z := buildZip(t, map[string]string{
		"SKILL.md":    "# zip\n",
		"scripts/run": "#!/bin/sh\necho ok\n",
	}, "")
	if _, err := s.install("zipper", bytes.NewReader(z)); err != nil {
		t.Fatalf("install zip: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.skillDir("zipper"), "SKILL.md")); err != nil {
		t.Fatal("SKILL.md missing after zip install")
	}
}

func TestSkillInstallRejectsBadInput(t *testing.T) {
	s := newTestStore(t)
	cases := []struct {
		name string
		slug string
		data []byte
		want string
	}{
		{"bad-slug", "..", buildTgz(t, map[string]string{"SKILL.md": "x"}, "s"), "非法"},
		{"no-skillmd", "s", buildTgz(t, map[string]string{"README.md": "x"}, "s"), "SKILL.md"},
		{"not-archive", "s", []byte("hello world"), "不支持"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := s.install(c.slug, bytes.NewReader(c.data)); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("expected error containing %q, got %v", c.want, err)
			}
		})
	}

	// zip-slip via a tar entry with "../".
	slipTar := buildZipSlipTgz(t)
	if _, err := s.install("slip", bytes.NewReader(slipTar)); err == nil || !strings.Contains(err.Error(), "逃逸") {
		t.Fatalf("expected zip-slip rejection, got %v", err)
	}
}

// buildZipSlipTgz builds a gzip tar with a "../evil" payload entry.
func buildZipSlipTgz(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	hdr := &tar.Header{Name: "../evil", Mode: 0o644, Size: int64(4), Typeflag: tar.TypeReg}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("evil")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestSkillRemoveIdempotent(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.install("demo", bytes.NewReader(buildTgz(t, map[string]string{"SKILL.md": "x"}, "demo"))); err != nil {
		t.Fatal(err)
	}
	if err := s.remove("demo"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.skillDir("demo")); !os.IsNotExist(err) {
		t.Fatalf("skill dir not removed: %v", err)
	}
	// Idempotent.
	if err := s.remove("demo"); err != nil {
		t.Fatal(err)
	}
	m, _, err := s.manifest()
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Skills) != 0 {
		t.Fatalf("manifest not updated after remove: %+v", m.Skills)
	}
}

func TestSkillTgzEndpoint(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.install("demo", bytes.NewReader(buildTgz(t, map[string]string{"SKILL.md": "x", "a/b.txt": "b"}, "demo"))); err != nil {
		t.Fatal(err)
	}
	data, err := s.tgz("demo")
	if err != nil {
		t.Fatal(err)
	}
	gzr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gzr)
	seen := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		seen[hdr.Name] = true
	}
	// Top-level dir must be <name>/.
	if !seen["demo/"] && !seen["demo"] && !seen["demo/SKILL.md"] {
		t.Fatalf("tarball must be under top-level dir demo/, got %v", seen)
	}
}

func TestInternalManifestETag(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.install("demo", bytes.NewReader(buildTgz(t, map[string]string{"SKILL.md": "x"}, "demo"))); err != nil {
		t.Fatal(err)
	}
	_, etag, err := s.manifest()
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/internal/skills/manifest", nil)
	s.handleInternalManifest(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("manifest status: %d", rec.Code)
	}
	if got := rec.Header().Get("ETag"); !strings.Contains(got, etag) {
		t.Fatalf("etag mismatch: %q vs %q", got, etag)
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m["version"] != etag {
		t.Fatalf("manifest version != etag: %v", m["version"])
	}

	// If-None-Match returns 304.
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/internal/skills/manifest", nil)
	req2.Header.Set("If-None-Match", etag)
	s.handleInternalManifest(rec2, req2)
	if rec2.Code != http.StatusNotModified {
		t.Fatalf("expected 304, got %d", rec2.Code)
	}
}

// fakeApplier records reloadActor calls per host.
type fakeApplier struct {
	okByHost    map[string]int
	totalByHost map[string]int
	errByHost   map[string]error
	hosts       []string
}

func (f *fakeApplier) reloadActor(_ context.Context, host string) (int, int, error) {
	f.hosts = append(f.hosts, host)
	return f.okByHost[host], f.totalByHost[host], f.errByHost[host]
}

func TestApplySkillsFansOutToRunningActors(t *testing.T) {
	f := newFake()
	// Two RUNNING actors, one non-running.
	f.actors["mfpi/alice"] = &ateapipb.Actor{Metadata: &ateapipb.ResourceMetadata{Atespace: "mfpi", Name: "alice"}, Status: ateapipb.Actor_STATUS_RUNNING}
	f.actors["mfpi/bob"] = &ateapipb.Actor{Metadata: &ateapipb.ResourceMetadata{Atespace: "mfpi", Name: "bob"}, Status: ateapipb.Actor_STATUS_RUNNING}
	f.actors["mfpi/sleepy"] = &ateapipb.Actor{Metadata: &ateapipb.ResourceMetadata{Atespace: "mfpi", Name: "sleepy"}, Status: ateapipb.Actor_STATUS_SUSPENDED}

	applier := &fakeApplier{
		okByHost: map[string]int{
			"alice.mfpi.actors.resources.substrate.ate.dev": 2,
			"bob.mfpi.actors.resources.substrate.ate.dev":   0,
		},
		totalByHost: map[string]int{},
	}
	srv := newTestServer(f)
	srv.applier = applier

	res := srv.applySkills(context.Background())
	// Only RUNNING actors are fanned out.
	if len(applier.hosts) != 2 {
		t.Fatalf("expected fan-out to 2 running actors, got %v", applier.hosts)
	}
	if res["failed"] != 0 {
		t.Fatalf("expected no failures, got %v", res["failures"])
	}
}

func TestApplySkillsHandlesReloadErrors(t *testing.T) {
	f := newFake()
	f.actors["mfpi/alice"] = &ateapipb.Actor{Metadata: &ateapipb.ResourceMetadata{Atespace: "mfpi", Name: "alice"}, Status: ateapipb.Actor_STATUS_RUNNING}
	applier := &fakeApplier{
		okByHost:    map[string]int{"alice.mfpi.actors.resources.substrate.ate.dev": 0},
		totalByHost: map[string]int{},
		errByHost:   map[string]error{"alice.mfpi.actors.resources.substrate.ate.dev": context.DeadlineExceeded},
	}
	srv := newTestServer(f)
	srv.applier = applier

	res := srv.applySkills(context.Background())
	if res["failed"] != 1 {
		t.Fatalf("expected 1 failure recorded, got %v", res)
	}
	if len(res["failures"].([]string)) != 1 {
		t.Fatalf("expected a failure detail, got %v", res["failures"])
	}
}

// Ensure the fake control client's SendMessage-style helpers compile against
// the interface (defensive; the existing test file already covers this).
var _ controlClient = (*fakeControlClient)(nil)

func TestSkillSlugValidation(t *testing.T) {
	store := newTestStore(t)
	for _, name := range []string{"mofang-form", "a", "a1-b2"} {
		if !skillSlugRe.MatchString(name) {
			t.Fatalf("%q should be a valid slug", name)
		}
	}
	for _, name := range []string{"", "../etc", "a/b", "A", "a b", "-a", "a-"} {
		if skillSlugRe.MatchString(name) {
			t.Fatalf("%q should be rejected", name)
		}
	}
	if _, err := store.install("../etc", strings.NewReader("x")); err == nil {
		t.Fatal("expected install to reject traversal name")
	}
}

func TestHTTPApplyReloaderReloadActor(t *testing.T) {
	var gotCwd string
	var reloadedSessionID string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/projects":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"path":"/my/workspace"}]`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/sessions":
			if r.URL.Query().Get("cwd") != "/my/workspace" {
				http.Error(w, "unexpected cwd query", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"id":"sess-123","cwd":"/my/workspace"}]`))
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/sessions/") && strings.HasSuffix(r.URL.Path, "/reload"):
			reloadedSessionID = strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/sessions/"), "/reload")
			var req struct {
				Cwd string `json:"cwd"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			gotCwd = req.Cwd
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"reloaded":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	reloader := newHTTPApplyReloader(ts.URL)
	ok, total, err := reloader.reloadActor(context.Background(), "alice.mfpi.actors.resources.substrate.ate.dev")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok != 1 || total != 1 {
		t.Fatalf("expected 1/1 reloaded, got ok=%d total=%d", ok, total)
	}
	if reloadedSessionID != "sess-123" {
		t.Fatalf("expected sess-123 reloaded, got %q", reloadedSessionID)
	}
	if gotCwd != "/my/workspace" {
		t.Fatalf("expected cwd /my/workspace, got %q", gotCwd)
	}
}
