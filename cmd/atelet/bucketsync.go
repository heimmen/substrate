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

// objectStoreBucket volume sync for atelet.
//
// A bucket volume backs one node-local directory (ateompath.VolumeHostPath)
// with one bucket in an S3-compatible object store (e.g. the mf-pi demo's
// per-user MinIO). The guest writes plain files to the bind-mounted path;
// atelet keeps the bucket in sync:
//
//   - Mount time (Run/Restore, before the guest boots): the host dir is
//     created; when it is EMPTY it is rehydrated from the bucket's single
//     profile object. A missing object means a brand-new user — the guest
//     seeds the dir (the pi-web bootstrap materializes baked-in skills).
//     A non-empty dir is locally authoritative (same-instance resume) and is
//     never clobbered.
//   - While running: a per-actor ticker exports the dir as one tarball
//     (skipping transient *.log/*.sock/*.tmp files and empty trees).
//   - Checkpoint (suspend/pause/delete): one final, synchronous, fail-closed
//     export while the workload is quiesced and before the actor dirs are
//     reset, so a delete+recreate reset loses at most one export interval of
//     writes.
//
// Fail-closed rule: only a *confirmed* missing object leaves a fresh mount
// empty. Any other object-store error fails the Run/Restore/Checkpoint so the
// controller retries instead of silently treating it as "no data". See
// demos/mf-pi/mount_minio_v2.md.
package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/agent-substrate/substrate/cmd/atelet/internal/ategcs"
	"github.com/agent-substrate/substrate/internal/ateompath"
	"github.com/agent-substrate/substrate/internal/proto/ateletpb"
)

// profileObjectKey is the single object per bucket holding the tar.gz of the
// mounted directory. It byte-matches the object the previous in-image broker
// wrote and what the mf-pi admin "已同步" badge heads, so existing user
// buckets are read as-is and both views always address the same object.
const profileObjectKey = "profile.tar.gz"

// defaultBucketExportInterval is the periodic export cadence when the volume
// does not override exportIntervalSeconds.
const defaultBucketExportInterval = 20 * time.Second

// maxBucketExportTimeout bounds a single export (ticker or final). A profile
// is normally small; the cap keeps a pathological tree from hanging the
// suspend path forever.
const maxBucketExportTimeout = 5 * time.Minute

// bucketObjectClient is the S3 surface the bucket-volume sync needs. It is
// satisfied by *ategcs.BucketClient and by test fakes.
type bucketObjectClient interface {
	EnsureBucket(ctx context.Context, bucket string) error
	HeadObject(ctx context.Context, bucket, key string) (bool, error)
	GetObjectToFile(ctx context.Context, bucket, key, dst string) error
	PutObjectFromFile(ctx context.Context, bucket, key, src string) error
}

// bucketVolumeSpec is the resolved config of one objectStoreBucket volume on
// this node.
type bucketVolumeSpec struct {
	client   bucketObjectClient
	bucket   string
	dir      string
	interval time.Duration
}

// bucketVolumeSpecFromProto builds the spec from a resolved RPC volume. The
// dir is the per-actor host path atelet binds into the guest.
func bucketVolumeSpecFromProto(ctx context.Context, actorUID string, vol *ateletpb.Volume) (bucketVolumeSpec, error) {
	src := vol.GetObjectStoreBucket()
	if src == nil {
		return bucketVolumeSpec{}, fmt.Errorf("volume %q has no objectStoreBucket source", vol.GetName())
	}
	if src.GetEndpoint() == "" || src.GetAccessKeyId() == "" || src.GetSecretAccessKey() == "" || src.GetBucket() == "" {
		return bucketVolumeSpec{}, fmt.Errorf("volume %q has an incomplete objectStoreBucket source", vol.GetName())
	}
	client, err := ategcs.NewBucketClient(ctx, src.GetEndpoint(), src.GetAccessKeyId(), src.GetSecretAccessKey())
	if err != nil {
		return bucketVolumeSpec{}, fmt.Errorf("building object-store client for volume %q: %w", vol.GetName(), err)
	}
	interval := defaultBucketExportInterval
	if secs := src.GetExportIntervalSeconds(); secs > 0 {
		interval = time.Duration(secs) * time.Second
	}
	return bucketVolumeSpec{
		client:   client,
		bucket:   src.GetBucket(),
		dir:      ateompath.VolumeHostPath(actorUID, vol.GetName()),
		interval: interval,
	}, nil
}

// bucketVolumeSpecsFromProto resolves every bucket volume in the request.
func bucketVolumeSpecsFromProto(ctx context.Context, actorUID string, volumes []*ateletpb.Volume) ([]bucketVolumeSpec, error) {
	var specs []bucketVolumeSpec
	for _, vol := range volumes {
		if vol.GetType() != ateletpb.VolumeType_VOLUME_TYPE_OBJECT_STORE_BUCKET {
			continue
		}
		spec, err := bucketVolumeSpecFromProto(ctx, actorUID, vol)
		if err != nil {
			return nil, err
		}
		specs = append(specs, spec)
	}
	return specs, nil
}

// mountBucketVolumes prepares every bucket volume's host dir before the guest
// boots: MkdirAll, then rehydrate when the dir is empty. See the package
// comment for the mount-time decision.
func mountBucketVolumes(ctx context.Context, specs []bucketVolumeSpec) error {
	for _, spec := range specs {
		if err := os.MkdirAll(spec.dir, 0o750); err != nil {
			return fmt.Errorf("while creating bucket volume dir %q: %w", spec.dir, err)
		}
		empty, err := dirIsEmpty(spec.dir)
		if err != nil {
			return fmt.Errorf("while inspecting bucket volume dir %q: %w", spec.dir, err)
		}
		if !empty {
			// Local copy is authoritative (same-instance resume): never
			// clobber it with a possibly staler bucket copy.
			slog.InfoContext(ctx, "Bucket volume dir non-empty; local copy is authoritative",
				slog.String("dir", spec.dir), slog.String("bucket", spec.bucket))
			continue
		}
		found, err := rehydrateDirFromBucket(ctx, spec.dir, spec.client, spec.bucket)
		if err != nil {
			return err
		}
		if !found {
			slog.InfoContext(ctx, "Bucket volume empty with no stored profile; guest will seed it",
				slog.String("dir", spec.dir), slog.String("bucket", spec.bucket))
		}
	}
	return nil
}

// dirIsEmpty reports whether dir holds no entries.
func dirIsEmpty(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	return len(entries) == 0, nil
}

// rehydrateDirFromBucket restores the bucket's profile tarball into dir.
// dir must already exist and be empty. found=false (nil error) means the
// bucket has no stored profile yet. Transient object-store errors are
// returned so the caller fails closed.
func rehydrateDirFromBucket(ctx context.Context, dir string, client bucketObjectClient, bucket string) (found bool, err error) {
	if err := client.EnsureBucket(ctx, bucket); err != nil {
		return false, fmt.Errorf("while ensuring bucket %q: %w", bucket, err)
	}
	present, err := client.HeadObject(ctx, bucket, profileObjectKey)
	if err != nil {
		return false, fmt.Errorf("while checking stored profile in bucket %q: %w", bucket, err)
	}
	if !present {
		return false, nil
	}

	tmpTar, err := os.CreateTemp("", "bucket-rehydrate-*.tar.gz")
	if err != nil {
		return false, fmt.Errorf("while creating rehydrate temp file: %w", err)
	}
	tmpTarName := tmpTar.Name()
	tmpTar.Close()
	defer os.Remove(tmpTarName)

	if err := client.GetObjectToFile(ctx, bucket, profileObjectKey, tmpTarName); err != nil {
		return false, fmt.Errorf("while fetching profile from bucket %q: %w", bucket, err)
	}

	// Unpack into a sibling, then atomically swap into place so the mount
	// point never holds a partially extracted tree.
	tmpDir := dir + ".rehydrate"
	if err := os.RemoveAll(tmpDir); err != nil {
		return false, fmt.Errorf("while clearing rehydrate dir %q: %w", tmpDir, err)
	}
	if err := os.MkdirAll(tmpDir, 0o750); err != nil {
		return false, fmt.Errorf("while creating rehydrate dir %q: %w", tmpDir, err)
	}
	f, err := os.Open(tmpTarName)
	if err != nil {
		os.RemoveAll(tmpDir)
		return false, fmt.Errorf("while opening downloaded profile: %w", err)
	}
	if err := untarGzDir(tmpDir, f); err != nil {
		f.Close()
		os.RemoveAll(tmpDir)
		return false, fmt.Errorf("while unpacking profile into %q: %w", tmpDir, err)
	}
	f.Close()

	// dir exists and is empty, so Remove+Rename is an atomic-enough swap.
	if err := os.Remove(dir); err != nil {
		os.RemoveAll(tmpDir)
		return false, fmt.Errorf("while removing empty bucket volume dir %q: %w", dir, err)
	}
	if err := os.Rename(tmpDir, dir); err != nil {
		os.MkdirAll(dir, 0o750) // restore the mountpoint before failing
		os.RemoveAll(tmpDir)
		return false, fmt.Errorf("while moving rehydrated profile into %q: %w", dir, err)
	}
	slog.InfoContext(ctx, "Rehydrated bucket volume from object store",
		slog.String("dir", dir), slog.String("bucket", bucket))
	return true, nil
}

// exportDirToBucket tars dir (excluding transient files) and uploads it as
// the bucket's single profile object. exported=false means the dir held no
// exportable files (e.g. the guest has not seeded it yet) and nothing was
// written — an empty tar must never overwrite a real profile.
func exportDirToBucket(ctx context.Context, dir string, client bucketObjectClient, bucket string) (exported bool, err error) {
	tmp, err := os.CreateTemp("", "bucket-export-*.tar.gz")
	if err != nil {
		return false, fmt.Errorf("while creating export temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	files, err := tarGzDir(dir, tmp)
	closeErr := tmp.Close()
	if err != nil {
		return false, fmt.Errorf("while tarring %q: %w", dir, err)
	}
	if closeErr != nil {
		return false, fmt.Errorf("while closing export temp file: %w", closeErr)
	}
	if files == 0 {
		return false, nil
	}
	if err := client.PutObjectFromFile(ctx, bucket, profileObjectKey, tmpName); err != nil {
		return false, fmt.Errorf("while uploading profile to bucket %q: %w", bucket, err)
	}
	return true, nil
}

// excludedBaseRe matches transient files the profile must not carry (same
// exclusion set the previous in-image supervisor used).
var excludedBaseRe = regexp.MustCompile(`[.](log|sock|tmp)$`)

// tarGzDir writes a gzip-compressed tar of dir's contents into w and returns
// the number of file entries written (directories are not counted, so an
// empty-but-for-dirs tree reports 0). Only regular files, directories and
// symlinks are archived; anything else (sockets, devices, ...) is skipped.
func tarGzDir(dir string, w io.Writer) (int, error) {
	gzw := gzip.NewWriter(w)
	tw := tar.NewWriter(gzw)
	files := 0
	walkErr := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if excludedBaseRe.MatchString(d.Name()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		switch {
		case info.IsDir():
			hdr.Typeflag = tar.TypeDir
		case info.Mode().IsRegular():
			hdr.Typeflag = tar.TypeReg
		case info.Mode()&os.ModeSymlink != 0:
			link, lerr := os.Readlink(p)
			if lerr != nil {
				return lerr
			}
			hdr.Typeflag = tar.TypeSymlink
			hdr.Linkname = link
		default:
			// Sockets, devices, fifos, ... never belong in a profile.
			return nil
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			files++
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			_, err = io.Copy(tw, f)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
	if walkErr != nil {
		tw.Close()
		gzw.Close()
		return 0, walkErr
	}
	if err := tw.Close(); err != nil {
		gzw.Close()
		return 0, fmt.Errorf("while finishing tar: %w", err)
	}
	if err := gzw.Close(); err != nil {
		return 0, fmt.Errorf("while finishing gzip: %w", err)
	}
	return files, nil
}

// untarGzDir unpacks a gzip-compressed tar into dst. It rejects entries that
// would escape dst (absolute or ..-containing names, symlink targets leaving
// dst) so a corrupt or malicious bucket object cannot write outside the
// mount.
func untarGzDir(dst string, r io.Reader) error {
	cleanDst := filepath.Clean(dst)
	gzr, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("while opening gzip stream: %w", err)
	}
	defer gzr.Close()
	tr := tar.NewReader(gzr)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("while reading tar entry: %w", err)
		}
		if hdr.Name == "" {
			continue
		}
		rel, err := safeRelPath(hdr.Name)
		if err != nil {
			return err
		}
		if rel == "." {
			continue
		}
		target := filepath.Join(cleanDst, filepath.FromSlash(rel))
		if rel != "." && !strings.HasPrefix(target, cleanDst+string(os.PathSeparator)) {
			return fmt.Errorf("tar entry %q escapes destination", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o750); err != nil {
				return fmt.Errorf("while creating dir %q: %w", target, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
				return fmt.Errorf("while creating parent of %q: %w", target, err)
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return fmt.Errorf("while creating %q: %w", target, err)
			}
			_, err = io.Copy(f, tr)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return fmt.Errorf("while writing %q: %w", target, err)
			}
		case tar.TypeSymlink:
			// Creating a symlink never writes through the link, so an
			// absolute target (legitimately produced by tar for absolute
			// links, and meaningful again inside the guest after rehydrate)
			// is safe to store as-is. A *relative* target that escapes dst
			// is rejected as defense in depth.
			if !filepath.IsAbs(hdr.Linkname) {
				resolved := filepath.Join(filepath.Dir(target), filepath.FromSlash(hdr.Linkname))
				if !strings.HasPrefix(filepath.Clean(resolved), cleanDst+string(os.PathSeparator)) &&
					filepath.Clean(resolved) != cleanDst {
					return fmt.Errorf("tar entry %q has a symlink target escaping the destination", hdr.Name)
				}
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
				return fmt.Errorf("while creating parent of %q: %w", target, err)
			}
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return fmt.Errorf("while creating symlink %q: %w", target, err)
			}
		case tar.TypeLink:
			linkRel, err := safeRelPath(hdr.Linkname)
			if err != nil {
				return err
			}
			linkSrc := filepath.Join(cleanDst, filepath.FromSlash(linkRel))
			if !strings.HasPrefix(linkSrc, cleanDst+string(os.PathSeparator)) {
				return fmt.Errorf("tar entry %q has a hardlink target escaping the destination", hdr.Name)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
				return fmt.Errorf("while creating parent of %q: %w", target, err)
			}
			if err := os.Link(linkSrc, target); err != nil {
				return fmt.Errorf("while creating hardlink %q: %w", target, err)
			}
		default:
			return fmt.Errorf("unsupported tar entry type %q (%v)", hdr.Name, hdr.Typeflag)
		}
	}
}

// safeRelPath cleans a tar member name to a slash-separated relative path
// under the extraction root, rejecting absolute paths and ".." components.
func safeRelPath(name string) (string, error) {
	if strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("tar entry %q is absolute", name)
	}
	rel := path.Clean(filepath.ToSlash(name))
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("tar entry %q escapes the destination", name)
	}
	return rel, nil
}

// bucketSyncManager tracks the running actors' bucket volumes on this node
// and exports each mounted dir to its bucket on a ticker. One actor per
// worker and one bucket per volume keep exports single-writer at demo scale
// (double-export fencing deferred — mount_minio_v2.md D8).
type bucketSyncManager struct {
	mu    sync.Mutex
	items map[string]*bucketSyncItem
}

type bucketSyncItem struct {
	spec     bucketVolumeSpec
	stopCh   chan struct{}
	stopped  chan struct{}
	exportMu sync.Mutex // single-flight export per item
}

func newBucketSyncManager() *bucketSyncManager {
	return &bucketSyncManager{items: map[string]*bucketSyncItem{}}
}

func bucketSyncKey(actorUID, volName string) string {
	return actorUID + "/" + volName
}

// register starts the periodic export loop for one bucket volume. Idempotent
// per actorUID+volume: a re-Run of the same actor keeps the existing loop.
func (m *bucketSyncManager) register(actorUID string, spec bucketVolumeSpec) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := bucketSyncKey(actorUID, filepath.Base(spec.dir))
	if _, ok := m.items[key]; ok {
		return
	}
	item := &bucketSyncItem{
		spec:    spec,
		stopCh:  make(chan struct{}),
		stopped: make(chan struct{}),
	}
	m.items[key] = item
	go item.loop()
}

// unregister stops the export loops for the actor's bucket volumes without
// exporting. Used when Run/Restore fails after registration.
func (m *bucketSyncManager) unregister(actorUID string, volumes []*ateletpb.Volume) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, vol := range volumes {
		if vol.GetType() != ateletpb.VolumeType_VOLUME_TYPE_OBJECT_STORE_BUCKET {
			continue
		}
		key := bucketSyncKey(actorUID, vol.GetName())
		if item, ok := m.items[key]; ok {
			delete(m.items, key)
			close(item.stopCh)
		}
	}
}

func (item *bucketSyncItem) loop() {
	defer close(item.stopped)
	ticker := time.NewTicker(item.spec.interval)
	defer ticker.Stop()
	for {
		select {
		case <-item.stopCh:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), maxBucketExportTimeout)
			item.exportMu.Lock()
			exported, err := exportDirToBucket(ctx, item.spec.dir, item.spec.client, item.spec.bucket)
			item.exportMu.Unlock()
			cancel()
			// Periodic export is best-effort: the checkpoint-time final
			// export is fail-closed, so a missed tick only widens the
			// (bounded) loss window.
			if err != nil {
				slog.WarnContext(ctx, "Periodic bucket export failed (will retry)",
					slog.String("dir", item.spec.dir), slog.String("bucket", item.spec.bucket), slog.Any("err", err))
			} else if exported {
				slog.InfoContext(ctx, "Periodic bucket export",
					slog.String("dir", item.spec.dir), slog.String("bucket", item.spec.bucket))
			}
		}
	}
}

// finalExportAndStop synchronously exports every registered bucket volume of
// the actor, then stops its loops. Called from Checkpoint while the workload
// is quiesced and the host dirs still exist. Fail-closed: an export error
// fails the checkpoint so the actor is not silently torn down with unflushed
// writes (mount_minio_v2.md "Suspend export fails").
func (m *bucketSyncManager) finalExportAndStop(ctx context.Context, actorUID string, volumes []*ateletpb.Volume) error {
	m.mu.Lock()
	var items []*bucketSyncItem
	for _, vol := range volumes {
		if vol.GetType() != ateletpb.VolumeType_VOLUME_TYPE_OBJECT_STORE_BUCKET {
			continue
		}
		key := bucketSyncKey(actorUID, vol.GetName())
		if item, ok := m.items[key]; ok {
			items = append(items, item)
			delete(m.items, key)
			close(item.stopCh)
		}
	}
	m.mu.Unlock()
	defer func() {
		for _, item := range items {
			<-item.stopped
		}
	}()

	var errs []error
	for _, item := range items {
		item.exportMu.Lock()
		_, err := exportDirToBucket(ctx, item.spec.dir, item.spec.client, item.spec.bucket)
		item.exportMu.Unlock()
		if err != nil {
			errs = append(errs, fmt.Errorf("bucket %q: %w", item.spec.bucket, err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("while exporting bucket volumes of actor %s: %w", actorUID, err)
	}
	return nil
}
