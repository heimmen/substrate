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

package sticky

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

const testVolumeName = "ate-demo-mf-pi-alice-userdata"

// useTempBaseDir points the plugin's backing base directory at a fresh temp
// directory for the duration of the test. Tests must not run in parallel.
func useTempBaseDir(t *testing.T) {
	t.Helper()
	old := stickyVolumeDirectories
	stickyVolumeDirectories = t.TempDir()
	t.Cleanup(func() { stickyVolumeDirectories = old })
}

func TestCreateVolumeUsesStableName(t *testing.T) {
	useTempBaseDir(t)
	p := New()

	volID, err := p.CreateVolume(context.Background(), testVolumeName, "1Gi", "standard")
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if volID != testVolumeName {
		t.Fatalf("CreateVolume returned %q, want the stable name %q", volID, testVolumeName)
	}

	// Recreating the same actor volume must resolve to the same volumeID so
	// the backing directory (and its data) is reused.
	volID2, err := p.CreateVolume(context.Background(), testVolumeName, "1Gi", "standard")
	if err != nil {
		t.Fatalf("second CreateVolume: %v", err)
	}
	if volID2 != volID {
		t.Fatalf("second CreateVolume returned %q, want %q", volID2, volID)
	}
}

func TestMountUnmountRoundTrip(t *testing.T) {
	useTempBaseDir(t)
	p := New()
	ctx := context.Background()

	if _, err := p.CreateVolume(ctx, testVolumeName, "1Gi", "standard"); err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	target := filepath.Join(t.TempDir(), "mount-point")
	if err := os.MkdirAll(target, 0o750); err != nil {
		t.Fatalf("MkdirAll mount point: %v", err)
	}
	if err := p.MountVolume(ctx, testVolumeName, target); err != nil {
		t.Fatalf("MountVolume: %v", err)
	}

	// The mount point must resolve to the sticky backing directory.
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	wantDir := filepath.Join(stickyVolumeDirectories, testVolumeName)
	if resolved != wantDir {
		t.Fatalf("mount point resolves to %q, want %q", resolved, wantDir)
	}

	// Data written through the mount lands in the backing directory.
	if err := os.WriteFile(filepath.Join(target, "data.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("WriteFile through mount: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wantDir, "data.txt")); err != nil {
		t.Fatalf("data.txt missing in backing dir: %v", err)
	}

	if err := p.UnmountVolume(ctx, testVolumeName, target); err != nil {
		t.Fatalf("UnmountVolume: %v", err)
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("mount point still present after unmount: %v", err)
	}
	// Backing directory and its data survive the unmount.
	if _, err := os.Stat(filepath.Join(wantDir, "data.txt")); err != nil {
		t.Fatalf("data.txt missing in backing dir after unmount: %v", err)
	}
}

func TestDeleteVolumeKeepsBackingDir(t *testing.T) {
	useTempBaseDir(t)
	p := New()
	ctx := context.Background()

	// First instance: create, mount, write data, unmount, delete.
	if _, err := p.CreateVolume(ctx, testVolumeName, "1Gi", "standard"); err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	target := filepath.Join(t.TempDir(), "mount-point")
	if err := os.MkdirAll(target, 0o750); err != nil {
		t.Fatalf("MkdirAll mount point: %v", err)
	}
	if err := p.MountVolume(ctx, testVolumeName, target); err != nil {
		t.Fatalf("MountVolume: %v", err)
	}
	if err := os.WriteFile(filepath.Join(target, "profile.txt"), []byte("user data\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := p.UnmountVolume(ctx, testVolumeName, target); err != nil {
		t.Fatalf("UnmountVolume: %v", err)
	}
	if err := p.DeleteVolume(ctx, testVolumeName); err != nil {
		t.Fatalf("DeleteVolume: %v", err)
	}

	wantDir := filepath.Join(stickyVolumeDirectories, testVolumeName)
	if _, err := os.Stat(filepath.Join(wantDir, "profile.txt")); err != nil {
		t.Fatalf("user data lost after DeleteVolume: %v", err)
	}

	// Second instance (redeploy): create+mount with the same stable name must
	// re-attach the same backing directory with the previous data.
	if _, err := p.CreateVolume(ctx, testVolumeName, "1Gi", "standard"); err != nil {
		t.Fatalf("recreate CreateVolume: %v", err)
	}
	if err := p.MountVolume(ctx, testVolumeName, target); err != nil {
		t.Fatalf("recreate MountVolume: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(target, "profile.txt"))
	if err != nil {
		t.Fatalf("ReadFile through re-mounted volume: %v", err)
	}
	if string(got) != "user data\n" {
		t.Fatalf("redeployed instance read %q, want %q", got, "user data\n")
	}
}
