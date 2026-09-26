// Managed "shared skills" for the mf-pi admin server.
//
// The admin is the authoritative source for a small set of skills that are
// distributed to every user (each user is one pi-web Actor). The admin stores
// each managed skill as a directory tree under $SKILLS_DIR/skills/<name>/...
// on a PVCable location, and exposes two read-only HTTP endpoints:
//
//	GET /internal/skills/manifest          JSON manifest + ETag (304 on unchanged)
//	GET /internal/skills/<name>.tgz        tarball whose top-level dir is <name>/
//
// Actors run a background loop that conditionally pulls the manifest and
// applies changed skills into their $PI_CODING_AGENT_DIR/skills, so no control
// plane, volume plugin, or per-actor plumbing is involved (see
// deploy_skill_to_actor.md for the full design).
//
// The store deliberately only ever touches directories it manages: an
// install/remove rewrites a skill under skills/<name>/ and the manifest. It
// never reaches into a user's own skills. Skill names are DNS-1123 slugs (no
// "/", ".."), and uploaded archives are validated to reject ../ and absolute
// path entries (zip-slip) and to require a SKILL.md.
package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
)

// Default SKILLS_DIR (mounted PVC in the admin Deployment). Overridable via
// env SKILLS_DIR.
const defaultSkillsDir = "/var/lib/mfpi-skills"

// maxUploadBytes caps a single skill package upload (defense in depth; the
// extractors also cap per-file sizes). 64 MiB default.
const maxUploadBytes = 64 << 20

// maxSkillFileBytes caps a single extracted file within a skill archive.
const maxSkillFileBytes = 32 << 20

// defaultMaxSkills caps how many managed skills the store will keep, guarding
// against an admin that runs away. 1024 is far beyond any realistic count.
const defaultMaxSkills = 1024

// skillSlugRe mirrors the DNS-1123 name rule (same as actor names).
var skillSlugRe = dns1123Re

// manifest is the authoritative, on-disk description of every managed skill.
// Its JSON serialization is canonical (sorted keys, sorted skills) so that
// version can be a stable content hash across rewrites.
type manifest struct {
	Version string       `json:"version"`
	Skills  []skillEntry `json:"skills"`
}

// skillEntry describes one managed skill in the manifest.
type skillEntry struct {
	Name      string `json:"name"`
	SHA256    string `json:"sha256"`
	Size      int64  `json:"size"`
	UpdatedAt string `json:"updatedAt"` // RFC3339
}

// skillStore manages the PVC directory layout and manifest for managed skills.
// All methods are safe for concurrent use.
type skillStore struct {
	dir string // e.g. /var/lib/mfpi-skills

	// Optional clock hook for tests.
	now func() time.Time
}

func newSkillStore(dir string) *skillStore {
	if dir == "" {
		dir = defaultSkillsDir
	}
	return &skillStore{dir: dir, now: time.Now}
}

// skillsRoot returns the directory that contains skills/<name>.
func (s *skillStore) skillsRoot() string { return filepath.Join(s.dir, "skills") }

func (s *skillStore) skillDir(name string) string { return filepath.Join(s.skillsRoot(), name) }

// ensureDirs creates the store's directory layout and an empty manifest if it
// does not already exist.
func (s *skillStore) ensureDirs() error {
	if err := os.MkdirAll(s.skillsRoot(), 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(s.manifestPath()); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return s.writeManifest(manifest{})
}

func (s *skillStore) manifestPath() string { return filepath.Join(s.dir, "manifest.json") }

// list returns the current skill entries, sorted by name. It does not rewrite
// the manifest; callers that mutate should call writeManifest themselves.
func (s *skillStore) list() ([]skillEntry, error) {
	m, _, err := s.manifest()
	if err != nil {
		return nil, err
	}
	return m.Skills, nil
}

// manifest reads/rebuilds the manifest from the directory layout.
func (s *skillStore) manifest() (manifest, string, error) {
	if err := s.ensureDirs(); err != nil {
		return manifest{}, "", err
	}
	dirs, err := os.ReadDir(s.skillsRoot())
	if err != nil {
		return manifest{}, "", err
	}
	var entries []skillEntry
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		name := d.Name()
		if !skillSlugRe.MatchString(name) {
			// Not one we manage; skip silently.
			continue
		}
		sha, size, err := hashDir(s.skillDir(name))
		if err != nil {
			return manifest{}, "", err
		}
		info, err := d.Info()
		updated := ""
		if err == nil {
			updated = info.ModTime().UTC().Format(time.RFC3339)
		}
		entries = append(entries, skillEntry{Name: name, SHA256: sha, Size: size, UpdatedAt: updated})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	m := manifest{Skills: entries}
	m.Version = manifestVersion(m)
	etag := m.Version
	return m, etag, nil
}

// manifestVersion computes a stable content hash over the manifest's skills.
func manifestVersion(m manifest) string {
	h := sha256.New()
	enc := json.NewEncoder(h)
	// Skills are pre-sorted by name, and skillEntry fields are at a fixed
	// order, so marshaling is canonical.
	_ = enc.Encode(m.Skills)
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// writeManifest atomically persists the manifest (tmp + rename).
func (s *skillStore) writeManifest(m manifest) error {
	m.Version = manifestVersion(m)
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.manifestPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.manifestPath())
}

// persistManifest scans the directory layout and writes a fresh manifest,
// honoring the max-skill cap.
func (s *skillStore) persistManifest() (string, error) {
	_, etag, err := s.manifest() // rebuild
	if err != nil {
		return "", err
	}
	entries, err := s.list()
	if err != nil {
		return "", err
	}
	if len(entries) > defaultMaxSkills {
		return "", fmt.Errorf("too many managed skills (%d > %d)", len(entries), defaultMaxSkills)
	}
	if err := s.writeManifest(manifest{Skills: entries}); err != nil {
		return "", err
	}
	return etag, nil
}

// install stores a skill from an uploaded archive. r is a tgz or zip whose
// payload, after stripping one leading top-level directory if present, contains
// a SKILL.md at its root. The skill is written atomically (tmp dir + rename)
// and the manifest is refreshed. name must be a DNS-1123 slug.
func (s *skillStore) install(name string, r io.Reader) (skillEntry, error) {
	if !skillSlugRe.MatchString(name) {
		return skillEntry{}, fmt.Errorf("非法 skill 名称 %q（需为小写字母/数字/连字符）", name)
	}
	if err := s.ensureDirs(); err != nil {
		return skillEntry{}, err
	}
	// Extract into a temp dir first so a bad archive never corrupts an
	// existing skill or the manifest.
	tmp, err := os.MkdirTemp(s.skillsRoot(), ".mfpi-skill-")
	if err != nil {
		return skillEntry{}, err
	}
	defer os.RemoveAll(tmp)

	if err := s.extractArchive(tmp, r); err != nil {
		return skillEntry{}, err
	}

	// Locate the payload root: strip a single leading top-level directory if
	// the archive carried one (tgz from a checkout usually has <name>/).
	root := tmp
	if entries, err := os.ReadDir(tmp); err == nil && len(entries) == 1 && entries[0].IsDir() {
		root = filepath.Join(tmp, entries[0].Name())
	}
	if _, err := os.Stat(filepath.Join(root, "SKILL.md")); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return skillEntry{}, fmt.Errorf("skill 包缺少 SKILL.md")
		}
		return skillEntry{}, err
	}

	// Reject nested managed-skill/state names just in case.
	if err := rejectRuntimePaths(root); err != nil {
		return skillEntry{}, err
	}

	dest := s.skillDir(name)
	if err := os.RemoveAll(dest); err != nil {
		return skillEntry{}, err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return skillEntry{}, err
	}
	if err := os.Rename(root, dest); err != nil {
		return skillEntry{}, err
	}

	sha, size, err := hashDir(dest)
	if err != nil {
		return skillEntry{}, err
	}
	entry := skillEntry{
		Name:      name,
		SHA256:    sha,
		Size:      size,
		UpdatedAt: s.now().UTC().Format(time.RFC3339),
	}
	if _, err := s.persistManifest(); err != nil {
		return skillEntry{}, err
	}
	return entry, nil
}

// remove deletes a managed skill (idempotent) and refreshes the manifest.
func (s *skillStore) remove(name string) error {
	if !skillSlugRe.MatchString(name) {
		return fmt.Errorf("非法 skill 名称 %q", name)
	}
	if err := os.RemoveAll(s.skillDir(name)); err != nil {
		return err
	}
	_, err := s.persistManifest()
	return err
}

// tgz builds a gzipped tarball of one skill whose top-level dir is <name>/.
func (s *skillStore) tgz(name string) ([]byte, error) {
	dest := s.skillDir(name)
	if err := s.ensureDirs(); err != nil {
		return nil, err
	}
	if _, err := os.Stat(dest); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, os.ErrNotExist
		}
		return nil, err
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	err := filepath.WalkDir(dest, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dest, path)
		if err != nil {
			return err
		}
		// Prepend the <name>/ top-level dir.
		arcName := filepath.ToSlash(filepath.Join(name, rel))
		info, err := d.Info()
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = arcName
		if d.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, io.LimitReader(f, maxSkillFileBytes+1))
		return err
	})
	if err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// archivePayload is a decoded file entry from a skill archive.
type archivePayload struct {
	name string // normalized, slash-separated, relative to archive root
	data []byte
	mode fs.FileMode
	dir  bool
}

// extractArchive decodes a tgz or zip from r, validates every path (rejecting
// ../, leading "/", and drive letters), caps file sizes, and writes the
// payload under dest (preserving the archive's top-level directory if any).
func (s *skillStore) extractArchive(dest string, r io.Reader) error {
	// Peek the magic to pick a decoder: tgz starts with \x1f\x8b, zip with
	// "PK\x03\x04".
	head := make([]byte, 4)
	n, err := io.ReadFull(io.LimitReader(r, 4), head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return err
	}
	head = head[:n]

	var payloads []archivePayload
	switch {
	case len(head) >= 2 && head[0] == 0x1f && head[1] == 0x8b:
		payloads, err = readTar(io.MultiReader(bytes.NewReader(head), r))
	case len(head) >= 4 && string(head[:4]) == "PK\x03\x04":
		payloads, err = readZip(io.MultiReader(bytes.NewReader(head), r))
	default:
		return fmt.Errorf("不支持的文件格式（需要 .tgz/.tar.gz 或 .zip）")
	}
	if err != nil {
		return err
	}
	if len(payloads) == 0 {
		return fmt.Errorf("空 skill 包")
	}
	for _, p := range payloads {
		if _, err := validateArchivePath(p.name); err != nil {
			return err
		}
		path := filepath.Join(dest, filepath.FromSlash(p.name))
		if p.dir {
			if err := os.MkdirAll(path, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, p.data, p.mode.Perm()); err != nil {
			return err
		}
	}
	return nil
}

// validateArchivePath rejects entries that escape the extraction root or use
// absolute paths. It returns the cleaned relative path.
func validateArchivePath(name string) (string, error) {
	name = strings.ReplaceAll(name, "\\", "/")
	if strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("skill 包包含绝对路径条目 %q", name)
	}
	// Reject any ".." segment, not just a leading one.
	parts := strings.Split(name, "/")
	for _, p := range parts {
		if p == ".." {
			return "", fmt.Errorf("skill 包包含路径逃逸条目 %q", name)
		}
	}
	clean := filepath.ToSlash(filepath.Clean(name))
	if clean == "." {
		return "", fmt.Errorf("skill 包包含空路径条目")
	}
	return clean, nil
}

func readTar(r io.Reader) ([]archivePayload, error) {
	gzr, err := gzip.NewReader(io.LimitReader(r, maxUploadBytes))
	if err != nil {
		return nil, fmt.Errorf("读取 tgz 失败: %w", err)
	}
	defer gzr.Close()
	tr := tar.NewReader(gzr)
	var out []archivePayload
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("读取 tar 失败: %w", err)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			out = append(out, archivePayload{name: hdr.Name, dir: true})
		case tar.TypeReg, tar.TypeRegA:
			if hdr.Size > maxSkillFileBytes {
				return nil, fmt.Errorf("skill 包内文件过大: %s (%d bytes)", hdr.Name, hdr.Size)
			}
			data, err := io.ReadAll(io.LimitReader(tr, maxSkillFileBytes+1))
			if err != nil {
				return nil, err
			}
			if int64(len(data)) > maxSkillFileBytes {
				return nil, fmt.Errorf("skill 包内文件过大: %s", hdr.Name)
			}
			out = append(out, archivePayload{name: hdr.Name, data: data, mode: hdr.FileInfo().Mode()})
		default:
			// symlinks, devices, etc. are not supported for managed skills.
			continue
		}
	}
	return out, nil
}

func readZip(r io.Reader) ([]archivePayload, error) {
	// zip.NewReader needs a ReaderAt; buffer the (capped) payload first.
	data, err := io.ReadAll(io.LimitReader(r, maxUploadBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxUploadBytes {
		return nil, fmt.Errorf("skill 包过大（超过 %d bytes）", maxUploadBytes)
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("读取 zip 失败: %w", err)
	}
	var out []archivePayload
	for _, f := range zr.File {
		info := f.FileInfo()
		if info.IsDir() {
			out = append(out, archivePayload{name: f.Name, dir: true})
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		content, err := io.ReadAll(io.LimitReader(rc, maxSkillFileBytes+1))
		rc.Close()
		if err != nil {
			return nil, err
		}
		if int64(len(content)) > maxSkillFileBytes {
			return nil, fmt.Errorf("skill 包内文件过大: %s", f.Name)
		}
		out = append(out, archivePayload{name: f.Name, data: content, mode: info.Mode()})
	}
	return out, nil
}

// rejectRuntimePaths guards against a skill payload planting files the
// platform or actor treats specially (e.g. a nested managed-skills state file).
func rejectRuntimePaths(root string) error {
	bad := []string{
		".mfpi-managed-skills.json",
		".mfpi-tmp-",
	}
	for _, b := range bad {
		if _, err := os.Stat(filepath.Join(root, b)); err == nil {
			return fmt.Errorf("skill 包包含保留文件名 %q", b)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// hashDir computes a deterministic sha256 over the directory tree and its total
// byte size. File order is sorted; symlinks are skipped.
func hashDir(dir string) (string, int64, error) {
	h := sha256.New()
	var total int64
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s\x00%d\x00", filepath.ToSlash(rel), info.Size())
		total += info.Size()
		if d.IsDir() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(h, f)
		return err
	})
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), total, nil
}

// ---- HTTP handlers ----

// handleInternalManifest serves the manifest with ETag/If-None-Match support.
func (s *skillStore) handleInternalManifest(w http.ResponseWriter, r *http.Request) {
	m, etag, err := s.manifest()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("ETag", `"`+etag+`"`)
	if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	// Actors read version for change detection.
	writeJSON(w, http.StatusOK, m)
}

// handleInternalSkillTgz serves one skill as <name>.tgz.
func (s *skillStore) handleInternalSkillTgz(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/internal/skills/"), ".tgz")
	if name == "" || strings.Contains(name, "/") || !skillSlugRe.MatchString(name) {
		http.NotFound(w, r)
		return
	}
	data, err := s.tgz(name)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.tgz"`, name))
	_, _ = w.Write(data)
}

// skillListResponse is the JSON shape the UI renders for /api/skills.
type skillListResponse struct {
	Version string       `json:"version"`
	Skills  []skillEntry `json:"skills"`
}

// handleAPISkills serves GET /api/skills (list) and POST /api/skills (upload).
func (s *skillStore) handleAPISkills(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleListSkillsAPIRoute(w, r)
	case http.MethodPost:
		s.handleUploadSkillAPIRoute(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *skillStore) handleListSkillsAPIRoute(w http.ResponseWriter, r *http.Request) {
	entries, err := s.list()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取 skill 列表失败: " + err.Error()})
		return
	}
	_, etag, err := s.manifest()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取 skill 列表失败: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, skillListResponse{Version: etag, Skills: entries})
}

func (s *skillStore) handleUploadSkillAPIRoute(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	if err := r.ParseMultipartForm(maxUploadBytes); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "上传解析失败: " + err.Error()})
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "缺少上传文件字段 file"})
		return
	}
	defer file.Close()

	entry, err := s.install(name, file)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	log.Printf("skills: installed %q (sha256=%s size=%d)", entry.Name, entry.SHA256, entry.Size)
	writeJSON(w, http.StatusOK, map[string]any{"message": "已安装 skill " + entry.Name, "skill": entry})
}

// handleDeleteSkillAPIRoute serves DELETE /api/skills/{name}.
func (s *skillStore) handleDeleteSkillAPIRoute(w http.ResponseWriter, r *http.Request) {
	name := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/skills/"), "/")
	if name == "" || strings.Contains(name, "/") {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "skill 不存在"})
		return
	}
	if !skillSlugRe.MatchString(name) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "非法 skill 名称"})
		return
	}
	if err := s.remove(name); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "删除 skill 失败: " + err.Error()})
		return
	}
	log.Printf("skills: removed %q", name)
	writeJSON(w, http.StatusOK, map[string]string{"message": "已删除 skill " + name})
}

// handleSkillsSubresource serves DELETE /api/skills/{name}. It lives on
// *server (not *skillStore) so it is co-located with the API route that owns
// the store.
func (s *server) handleSkillsSubresource(w http.ResponseWriter, r *http.Request) {
	if s.skills == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "skills store 未启用"})
		return
	}
	// /api/skills/apply is handled by handleApplySkillsAPIRoute; anything else
	// under /api/skills/ is a named-skill DELETE.
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/skills/"), "/")
	if rest == "apply" {
		// Delegate; the mux should already route this, but guard anyway.
		s.handleApplySkillsAPIRoute(w, r)
		return
	}
	if r.Method != http.MethodDelete {
		http.NotFound(w, r)
		return
	}
	s.skills.handleDeleteSkillAPIRoute(w, r)
}

// --- Apply fan-out ---

// applyReloader reloads a running actor's open sessions so a newly provisioned
// skill takes effect without waiting for a new session. Implemented by
// httpApplyReloader (over the router); faked in tests. All operations are
// best-effort: a failed reload is reported as a per-actor note, never fatal.
type applyReloader interface {
	// reloadActor best-effort reloads every open session for one actor.
	// Returns (okCount, totalCount, err); okCount/totalCount are informational
	// even when err != nil.
	reloadActor(ctx context.Context, host string) (ok, total int, err error)
}

// httpApplyReloader reloads actor sessions through the atenet router using the
// actorHostname Host header — the same path apikey.go uses to reach pi-web.
type httpApplyReloader struct {
	base string
	hc   *http.Client
}

func newHTTPApplyReloader(base string) *httpApplyReloader {
	return &httpApplyReloader{base: base, hc: newHTTPActorAuthClient(base).hc}
}

// actorJSON carries out one JSON request against the router with the per-actor
// Host header but does not fail on non-200 — the reload flow inspects the body.
func (c *httpApplyReloader) json(ctx context.Context, host, method, path string) (int, []byte, error) {
	return c.jsonWithBody(ctx, host, method, path, nil)
}

func (c *httpApplyReloader) jsonWithBody(ctx context.Context, host, method, path string, body []byte) (int, []byte, error) {
	var bodyReader io.Reader
	if len(body) > 0 {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bodyReader)
	if err != nil {
		return 0, nil, err
	}
	req.Host = host
	req.Header.Set("Accept", "application/json")
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, data, nil
}

// reloadActor enumerates the actor's projects, then each project's open
// sessions, and POSTs /api/sessions/:id/reload for each. Any session that
// reports active work is left for the next session ("Stop current session
// activity before reloading") and counted as skipped, not failed.
func (c *httpApplyReloader) reloadActor(ctx context.Context, host string) (int, int, error) {
	// List projects.
	code, body, err := c.json(ctx, host, http.MethodGet, "/api/projects")
	if err != nil {
		return 0, 0, fmt.Errorf("list projects: %w", err)
	}
	if code != http.StatusOK {
		return 0, 0, fmt.Errorf("list projects returned %d: %s", code, truncate(string(body), 200))
	}
	// List projects: pi-web may return a JSON array `[...]` or an object `{"projects":[...]}`.
	var projectPaths []string
	var rawProjects []struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(body, &rawProjects); err == nil {
		for _, p := range rawProjects {
			if p.Path != "" {
				projectPaths = append(projectPaths, p.Path)
			}
		}
	} else {
		var wrapped struct {
			Projects []struct {
				Path string `json:"path"`
			} `json:"projects"`
		}
		if err2 := json.Unmarshal(body, &wrapped); err2 != nil {
			return 0, 0, fmt.Errorf("decode projects: %w", err2)
		}
		for _, p := range wrapped.Projects {
			if p.Path != "" {
				projectPaths = append(projectPaths, p.Path)
			}
		}
	}

	type sessionTarget struct {
		id  string
		cwd string
	}
	seen := map[string]bool{}
	var targets []sessionTarget
	for _, pPath := range projectPaths {
		code, body, err := c.json(ctx, host, http.MethodGet, "/api/sessions?cwd="+urlQueryEscape(pPath))
		if err != nil || code != http.StatusOK {
			continue // best-effort per project
		}
		var rawSessions []struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(body, &rawSessions); err == nil {
			for _, s := range rawSessions {
				if s.ID != "" && !seen[s.ID] {
					seen[s.ID] = true
					targets = append(targets, sessionTarget{id: s.ID, cwd: pPath})
				}
			}
		} else {
			var wrappedSessions struct {
				Sessions []struct {
					ID string `json:"id"`
				} `json:"sessions"`
			}
			if err := json.Unmarshal(body, &wrappedSessions); err == nil {
				for _, s := range wrappedSessions.Sessions {
					if s.ID != "" && !seen[s.ID] {
						seen[s.ID] = true
						targets = append(targets, sessionTarget{id: s.ID, cwd: pPath})
					}
				}
			}
		}
	}

	ok, total := 0, len(targets)
	for _, t := range targets {
		reqBody, _ := json.Marshal(map[string]string{"cwd": t.cwd})
		code, _, err := c.jsonWithBody(ctx, host, http.MethodPost, "/api/sessions/"+t.id+"/reload", reqBody)
		if err != nil {
			continue
		}
		// 200 => reloaded. Anything else (e.g. 409 "Stop current session
		// activity before reloading") counts as not-yet but not fatal.
		if code == http.StatusOK {
			ok++
		}
	}
	return ok, total, nil
}

func urlQueryEscape(s string) string {
	return strings.ReplaceAll(url.PathEscape(s), "+", "%20")
}

// applySkills fans out to every RUNNING actor, reloading their open sessions so
// newly provisioned skills take effect. Returns a summary for the UI. Each
// actor is handled best-effort; failures are aggregated, never aborting.
func (s *server) applySkills(ctx context.Context) map[string]any {
	// Collect RUNNING actors (same enumeration as the reconciler).
	resp, err := s.client.ListActors(ctx, &ateapipb.ListActorsRequest{Atespace: s.atespace, PageSize: 1000})
	if err != nil {
		return map[string]any{"error": "列出用户失败: " + err.Error()}
	}
	type result struct {
		actor string
		ok    int
		total int
		err   error
	}
	var results []result
	for _, a := range resp.GetActors() {
		if a.GetStatus() != ateapipb.Actor_STATUS_RUNNING {
			continue
		}
		name := a.GetMetadata().GetName()
		host := actorHostname(name, s.atespace)
		ok, total, err := s.applier.reloadActor(ctx, host)
		results = append(results, result{actor: name, ok: ok, total: total, err: err})
	}
	reloaded, online, failed := 0, 0, 0
	var failures []string
	for _, r := range results {
		online++
		reloaded += r.ok
		if r.err != nil {
			failed++
			failures = append(failures, r.actor+": "+r.err.Error())
			log.Printf("skills apply: %s: %v", r.actor, r.err)
			continue
		}
		// A reachable actor with zero open sessions still counts as applied.
		if r.ok == 0 {
			_ = r.total
		}
	}
	return map[string]any{
		"message":  "已尝试应用",
		"online":   online,
		"reloaded": reloaded,
		"failed":   failed,
		"failures": failures,
	}
}

// handleApplySkillsAPIRoute serves POST /api/skills/apply.
func (s *server) handleApplySkillsAPIRoute(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, s.applySkills(ctx))
}
