// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/agent-substrate/substrate/internal/proto/ateletpb"
)

// fakeBucketClient is an in-memory bucketObjectClient.
type fakeBucketClient struct {
	mu      sync.Mutex
	objects map[string][]byte // bucket + "/" + key -> content
	ensured []string
	puts    []string // bucket + "/" + key

	ensureErr error
	headErr   error
	getErr    error
	putErr    error
}

func newFakeBucketClient() *fakeBucketClient {
	return &fakeBucketClient{objects: map[string][]byte{}}
}

func (f *fakeBucketClient) EnsureBucket(_ context.Context, bucket string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ensureErr != nil {
		return f.ensureErr
	}
	f.ensured = append(f.ensured, bucket)
	return nil
}

func (f *fakeBucketClient) HeadObject(_ context.Context, bucket, key string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.headErr != nil {
		return false, f.headErr
	}
	_, ok := f.objects[bucket+"/"+key]
	return ok, nil
}

func (f *fakeBucketClient) GetObjectToFile(_ context.Context, bucket, key, dst string) error {
	f.mu.Lock()
	data, ok := f.objects[bucket+"/"+key]
	getErr := f.getErr
	f.mu.Unlock()
	if getErr != nil {
		return getErr
	}
	if !ok {
		return fmt.Errorf("object %s/%s not found", bucket, key)
	}
	return os.WriteFile(dst, data, 0o600)
}

func (f *fakeBucketClient) PutObjectFromFile(_ context.Context, bucket, key, src string) error {
	f.mu.Lock()
	putErr := f.putErr
	f.mu.Unlock()
	if putErr != nil {
		return putErr
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[bucket+"/"+key] = data
	f.puts = append(f.puts, bucket+"/"+key)
	return nil
}

func (f *fakeBucketClient) object(bucket, key string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.objects[bucket+"/"+key]
	return data, ok
}

// seedTarGz builds a profile.tar.gz body containing the given name->content
// regular files.
func seedTarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))}); err != nil {
			t.Fatalf("WriteHeader: %v", err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gzw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatalf("mkdir for %q: %v", p, err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("write %q: %v", p, err)
		}
	}
}

func readTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			rel, _ := filepath.Rel(dir, p)
			data, rerr := os.ReadFile(p)
			if rerr != nil {
				return rerr
			}
			out[rel] = string(data)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %q: %v", dir, err)
	}
	return out
}

func TestTarGzDirRoundTrip(t *testing.T) {
	src := t.TempDir()
	writeTree(t, src, map[string]string{
		"auth.json":         `{"deepseek":"sk-1"}`,
		"skills/a/SKILL.md": "skill a",
		"sessions/s1.jsonl": "line",
		"debug.log":         "transient",
		"daemon.sock":       "socket-ish",
		"scratch.tmp":       "temp",
		"logs/excluded.log": "transient in dir",
	})
	if err := os.Symlink(filepath.Join(src, "auth.json"), filepath.Join(src, "auth-link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	var buf bytes.Buffer
	files, err := tarGzDir(src, &buf)
	if err != nil {
		t.Fatalf("tarGzDir: %v", err)
	}
	// Regular files only (dirs and symlinks are not counted).
	wantFiles := 3 // auth.json, skills/a/SKILL.md, sessions/s1.jsonl
	if files != wantFiles {
		t.Errorf("files = %d, want %d", files, wantFiles)
	}

	dst := t.TempDir()
	if err := untarGzDir(dst, &buf); err != nil {
		t.Fatalf("untarGzDir: %v", err)
	}
	got := readTree(t, dst)
	want := map[string]string{
		"auth.json":         `{"deepseek":"sk-1"}`,
		"skills/a/SKILL.md": "skill a",
		"sessions/s1.jsonl": "line",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("round-trip %q = %q, want %q", k, got[k], v)
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("unexpected round-trip entry %q (exclusions leaked?)", k)
		}
	}
	// Symlink preserved and pointing at the right relative location.
	linkTarget, err := os.Readlink(filepath.Join(dst, "auth-link"))
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if linkTarget != filepath.Join(src, "auth.json") {
		t.Errorf("symlink target = %q, want %q", linkTarget, filepath.Join(src, "auth.json"))
	}
}

func TestTarGzDirEmptyTreeReportsZero(t *testing.T) {
	src := t.TempDir()
	var buf bytes.Buffer
	files, err := tarGzDir(src, &buf)
	if err != nil {
		t.Fatalf("tarGzDir: %v", err)
	}
	if files != 0 {
		t.Errorf("files = %d, want 0 for an empty tree", files)
	}
}

func TestUntarGzDirRejectsUnsafeEntries(t *testing.T) {
	tests := []struct {
		name    string
		headers []tar.Header
	}{
		{
			name:    "dotdot path",
			headers: []tar.Header{{Name: "../escape.txt", Mode: 0o644}},
		},
		{
			name:    "absolute path",
			headers: []tar.Header{{Name: "/etc/passwd", Mode: 0o644}},
		},
		{
			name:    "escaping symlink",
			headers: []tar.Header{{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "../../outside"}},
		},
		{
			name:    "escaping hardlink",
			headers: []tar.Header{{Name: "hlink", Typeflag: tar.TypeLink, Linkname: "../outside"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			gzw := gzip.NewWriter(&buf)
			tw := tar.NewWriter(gzw)
			for i := range tc.headers {
				h := tc.headers[i]
				if h.Typeflag == tar.TypeReg || h.Typeflag == 0 {
					h.Size = 0
				}
				if err := tw.WriteHeader(&h); err != nil {
					t.Fatalf("WriteHeader: %v", err)
				}
			}
			tw.Close()
			gzw.Close()

			dst := t.TempDir()
			err := untarGzDir(dst, &buf)
			if err == nil {
				t.Fatalf("untarGzDir accepted an unsafe entry")
			}
			// Nothing escaped dst's parent.
			parent := filepath.Dir(dst)
			entries, _ := os.ReadDir(parent)
			if len(entries) != 1 {
				t.Errorf("unsafe extraction wrote outside dst: %v", entries)
			}
		})
	}
}

func TestExportDirToBucket(t *testing.T) {
	ctx := context.Background()

	t.Run("non-empty dir uploads profile", func(t *testing.T) {
		dir := t.TempDir()
		writeTree(t, dir, map[string]string{"auth.json": "x"})
		client := newFakeBucketClient()
		exported, err := exportDirToBucket(ctx, dir, client, "alice")
		if err != nil {
			t.Fatalf("exportDirToBucket: %v", err)
		}
		if !exported {
			t.Fatalf("exported = false, want true")
		}
		data, ok := client.object("alice", profileObjectKey)
		if !ok {
			t.Fatalf("profile object missing after export")
		}
		dst := t.TempDir()
		if err := untarGzDir(dst, bytes.NewReader(data)); err != nil {
			t.Fatalf("uploaded object is not a valid tar.gz: %v", err)
		}
		if got := readTree(t, dst)["auth.json"]; got != "x" {
			t.Errorf("uploaded content = %q, want x", got)
		}
	})

	t.Run("empty dir skips the PUT", func(t *testing.T) {
		dir := t.TempDir()
		client := newFakeBucketClient()
		exported, err := exportDirToBucket(ctx, dir, client, "alice")
		if err != nil {
			t.Fatalf("exportDirToBucket: %v", err)
		}
		if exported {
			t.Fatalf("exported = true, want false for an empty dir")
		}
		if len(client.puts) != 0 {
			t.Errorf("empty tree overwrote the bucket: puts=%v", client.puts)
		}
	})

	t.Run("put error surfaces", func(t *testing.T) {
		dir := t.TempDir()
		writeTree(t, dir, map[string]string{"auth.json": "x"})
		client := newFakeBucketClient()
		client.putErr = fmt.Errorf("minio down")
		if _, err := exportDirToBucket(ctx, dir, client, "alice"); err == nil {
			t.Fatalf("exportDirToBucket accepted a failed PUT")
		}
	})
}

func TestRehydrateDirFromBucket(t *testing.T) {
	ctx := context.Background()

	t.Run("present object is unpacked into the empty dir", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "vol")
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		client := newFakeBucketClient()
		client.objects["alice/"+profileObjectKey] = seedTarGz(t, map[string]string{"auth.json": "restored"})

		found, err := rehydrateDirFromBucket(ctx, dir, client, "alice")
		if err != nil {
			t.Fatalf("rehydrateDirFromBucket: %v", err)
		}
		if !found {
			t.Fatalf("found = false, want true")
		}
		if got := readTree(t, dir)["auth.json"]; got != "restored" {
			t.Errorf("rehydrated content = %q, want restored", got)
		}
		// No leftover rehydrate sibling.
		if _, err := os.Stat(dir + ".rehydrate"); !os.IsNotExist(err) {
			t.Errorf("rehydrate sibling left behind: %v", err)
		}
	})

	t.Run("absent object leaves the dir empty for the guest to seed", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "vol")
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		client := newFakeBucketClient()
		found, err := rehydrateDirFromBucket(ctx, dir, client, "alice")
		if err != nil {
			t.Fatalf("rehydrateDirFromBucket: %v", err)
		}
		if found {
			t.Fatalf("found = true, want false for an empty bucket")
		}
		empty, err := dirIsEmpty(dir)
		if err != nil || !empty {
			t.Errorf("dir not left empty (err=%v)", err)
		}
	})

	t.Run("transient get error fails closed", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "vol")
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		client := newFakeBucketClient()
		client.objects["alice/"+profileObjectKey] = seedTarGz(t, map[string]string{"auth.json": "x"})
		client.getErr = fmt.Errorf("503 from minio")
		if _, err := rehydrateDirFromBucket(ctx, dir, client, "alice"); err == nil {
			t.Fatalf("rehydrateDirFromBucket treated a transient error as no data")
		}
	})

	t.Run("corrupt tarball fails and leaves no partial tree", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "vol")
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		client := newFakeBucketClient()
		client.objects["alice/"+profileObjectKey] = []byte("not a tarball")
		if _, err := rehydrateDirFromBucket(ctx, dir, client, "alice"); err == nil {
			t.Fatalf("rehydrateDirFromBucket accepted a corrupt tarball")
		}
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("mountpoint dir not restored after failed rehydrate: %v", err)
		}
	})
}

func TestMountBucketVolumesDecision(t *testing.T) {
	ctx := context.Background()

	t.Run("empty dir with bucket content rehydrates", func(t *testing.T) {
		root := t.TempDir()
		dir := filepath.Join(root, "vol")
		client := newFakeBucketClient()
		client.objects["alice/"+profileObjectKey] = seedTarGz(t, map[string]string{"auth.json": "old"})
		spec := bucketVolumeSpec{client: client, bucket: "alice", dir: dir, interval: defaultBucketExportInterval}

		if err := mountBucketVolumes(ctx, []bucketVolumeSpec{spec}); err != nil {
			t.Fatalf("mountBucketVolumes: %v", err)
		}
		if got := readTree(t, dir)["auth.json"]; got != "old" {
			t.Errorf("content = %q, want old", got)
		}
	})

	t.Run("empty dir with empty bucket stays empty (guest seeds)", func(t *testing.T) {
		root := t.TempDir()
		dir := filepath.Join(root, "vol")
		spec := bucketVolumeSpec{client: newFakeBucketClient(), bucket: "alice", dir: dir, interval: defaultBucketExportInterval}

		if err := mountBucketVolumes(ctx, []bucketVolumeSpec{spec}); err != nil {
			t.Fatalf("mountBucketVolumes: %v", err)
		}
		if empty, err := dirIsEmpty(dir); err != nil || !empty {
			t.Errorf("dir not empty after mount (err=%v)", err)
		}
	})

	t.Run("non-empty dir is locally authoritative", func(t *testing.T) {
		root := t.TempDir()
		dir := filepath.Join(root, "vol")
		writeTree(t, dir, map[string]string{"auth.json": "local-newer"})
		client := newFakeBucketClient()
		client.objects["alice/"+profileObjectKey] = seedTarGz(t, map[string]string{"auth.json": "stale-bucket"})
		spec := bucketVolumeSpec{client: client, bucket: "alice", dir: dir, interval: defaultBucketExportInterval}

		if err := mountBucketVolumes(ctx, []bucketVolumeSpec{spec}); err != nil {
			t.Fatalf("mountBucketVolumes: %v", err)
		}
		if got := readTree(t, dir)["auth.json"]; got != "local-newer" {
			t.Errorf("local copy clobbered by bucket: %q", got)
		}
		if len(client.puts) != 0 {
			t.Errorf("mount performed a PUT: %v", client.puts)
		}
	})

	t.Run("transient error fails the mount", func(t *testing.T) {
		root := t.TempDir()
		dir := filepath.Join(root, "vol")
		client := newFakeBucketClient()
		client.ensureErr = fmt.Errorf("network unreachable")
		spec := bucketVolumeSpec{client: client, bucket: "alice", dir: dir, interval: defaultBucketExportInterval}

		if err := mountBucketVolumes(ctx, []bucketVolumeSpec{spec}); err == nil {
			t.Fatalf("mountBucketVolumes accepted a failing object store (must fail closed)")
		}
	})
}

func TestFinalExportAndStopFailClosed(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "vol")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	writeTree(t, dir, map[string]string{"sessions/s.jsonl": "latest-write"})

	client := newFakeBucketClient()
	client.putErr = fmt.Errorf("minio down")
	m := newBucketSyncManager()
	spec := bucketVolumeSpec{client: client, bucket: "alice", dir: dir, interval: defaultBucketExportInterval}
	m.register("actor-1", spec)

	vols := []*ateletpb.Volume{
		{Name: "vol", Type: ateletpb.VolumeType_VOLUME_TYPE_OBJECT_STORE_BUCKET},
	}
	if err := m.finalExportAndStop(ctx, "actor-1", vols); err == nil {
		t.Fatalf("finalExportAndStop must fail closed on an export error")
	}
	// Loop stopped: a subsequent registration starts fresh.
	if len(client.puts) != 0 {
		t.Errorf("failed export must not PUT: %v", client.puts)
	}

	// Healthy client: the final export flushes the latest writes.
	client2 := newFakeBucketClient()
	spec2 := bucketVolumeSpec{client: client2, bucket: "alice", dir: dir, interval: defaultBucketExportInterval}
	m2 := newBucketSyncManager()
	m2.register("actor-1", spec2)
	if err := m2.finalExportAndStop(ctx, "actor-1", vols); err != nil {
		t.Fatalf("finalExportAndStop: %v", err)
	}
	data, ok := client2.object("alice", profileObjectKey)
	if !ok {
		t.Fatalf("final export did not upload the profile")
	}
	dst := t.TempDir()
	if err := untarGzDir(dst, bytes.NewReader(data)); err != nil {
		t.Fatalf("final export object invalid: %v", err)
	}
	if got := readTree(t, dst)["sessions/s.jsonl"]; got != "latest-write" {
		t.Errorf("final export content = %q, want latest-write", got)
	}
}

// volumeStub was folded into the tests via ateletpb.Volume literals.

func TestBucketSyncKeyAndRegistration(t *testing.T) {
	m := newBucketSyncManager()
	spec := bucketVolumeSpec{client: newFakeBucketClient(), bucket: "alice", dir: "/tmp/whatever/vol", interval: defaultBucketExportInterval}
	m.register("actor-1", spec)
	m.register("actor-1", spec) // idempotent
	m.mu.Lock()
	n := len(m.items)
	m.mu.Unlock()
	if n != 1 {
		t.Errorf("items = %d, want 1 (register must be idempotent)", n)
	}
	if got := bucketSyncKey("a", "v"); got != "a/v" {
		t.Errorf("bucketSyncKey = %q, want a/v", got)
	}
	if !strings.Contains(fmt.Sprintf("%v", m.items), "actor-1/vol") {
		t.Errorf("registration keyed by vol name failed: %v", m.items)
	}
}
