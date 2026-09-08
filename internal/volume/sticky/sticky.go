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

// Package sticky implements a sticky volume plugin.
//
// Unlike the mock plugin, whose backing directories are keyed by a per-create
// counter (so every actor create gets a fresh empty directory), the sticky
// plugin keys the backing directory by the volume's stable name. The control
// plane already derives that name from the actor identity
// (<atespace>-<actorName>-<volumeName>, see controlapi.actorVolumeID), so
// every create of the same actor volume resolves to the same directory.
// DeleteVolume intentionally keeps the directory on disk. Together this makes
// an ExternalVolumeTemplate-typed volume persist user data across a
// delete+recreate (image-refresh redeploy): the recreated instance is
// re-attached to the same directory and finds the previous data.
package sticky

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/agent-substrate/substrate/internal/ateompath"
	"github.com/agent-substrate/substrate/internal/volume"
)

// stickyVolumeDirectories is the base directory holding the sticky backing
// directories. Like the mock volumes it lives under ateompath.BasePath so it
// is shared between atelet and ateom but is not cleaned up by atelet.
var stickyVolumeDirectories = filepath.Join(ateompath.BasePath, "stickyvolumes")

var (
	_ volume.VolumePluginControlPlane = (*Plugin)(nil)
	_ volume.VolumePluginWorkerPlane  = (*Plugin)(nil)
)

func init() {
	// Make this plugin the process-wide default (see internal/volume/registry.go).
	volume.RegisterDefault(New())
}

// Plugin is the sticky volume plugin. It implements both the control-plane
// and the worker-plane volume plugin interfaces.
type Plugin struct {
	mu sync.Mutex
	// created tracks volumeIDs handed out by CreateVolume in this process.
	// The backing directory itself is identified purely by name.
	created map[string]bool
}

// New creates a Plugin.
func New() *Plugin {
	return &Plugin{created: make(map[string]bool)}
}

// CreateVolume is idempotent and returns name as the volumeID. Because the
// name is derived from the actor identity by the control plane, every create
// of the same actor volume maps to the same backing directory.
func (p *Plugin) CreateVolume(ctx context.Context, name string, capacity string, storageClass string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.created[name] = true
	slog.InfoContext(ctx, "StickyVolumePlugin.CreateVolume", slog.String("name", name), slog.String("capacity", capacity), slog.String("storageClass", storageClass))
	return name, nil
}

// DeleteVolume intentionally does NOT remove the backing directory: user data
// must survive actor deletion so a later redeploy of the same actor can
// re-attach it. Reclaiming the storage is left to the demo environment
// (mirroring the mock plugin, which also never cleans up its directories).
func (p *Plugin) DeleteVolume(ctx context.Context, volumeID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	slog.InfoContext(ctx, "StickyVolumePlugin.DeleteVolume (backing directory kept)", slog.String("volumeID", volumeID))
	delete(p.created, volumeID)
	return nil
}

// AttachVolume is a no-op: sticky volumes are node-local directories, so
// there is no attachable hardware (matching the single-node kind demo).
func (p *Plugin) AttachVolume(ctx context.Context, volumeID string, node string) error {
	slog.InfoContext(ctx, "StickyVolumePlugin.AttachVolume", slog.String("volumeID", volumeID), slog.String("node", node))
	return nil
}

// DetachVolume is a no-op, see AttachVolume.
func (p *Plugin) DetachVolume(ctx context.Context, volumeID string, node string) error {
	slog.InfoContext(ctx, "StickyVolumePlugin.DetachVolume", slog.String("volumeID", volumeID), slog.String("node", node))
	return nil
}

// MountVolume creates the sticky backing directory if needed and points
// targetPath at it via a symlink (no bind mount, so atelet does not require
// bidirectional mount propagation). If the directory already holds data from
// a previous instance of the same actor, that data simply becomes visible.
func (p *Plugin) MountVolume(ctx context.Context, volumeID string, targetPath string) error {
	slog.InfoContext(ctx, "StickyVolumePlugin.MountVolume", slog.String("volumeID", volumeID), slog.String("targetPath", targetPath))

	volumeDir := p.dir(volumeID)
	if err := os.MkdirAll(volumeDir, 0o755); err != nil {
		return fmt.Errorf("failed to create sticky volume directory %q: %w", volumeDir, err)
	}

	// Use symlink instead of bind mount to avoid atelet requiring bidirectional mount propagation.
	_ = os.Remove(targetPath)
	if err := os.Symlink(volumeDir, targetPath); err != nil {
		return fmt.Errorf("failed to symlink %q to %q: %w", targetPath, volumeDir, err)
	}
	return nil
}

// UnmountVolume removes the symlink only; the backing directory (and its
// data) is kept.
func (p *Plugin) UnmountVolume(ctx context.Context, volumeID string, targetPath string) error {
	slog.InfoContext(ctx, "StickyVolumePlugin.UnmountVolume", slog.String("volumeID", volumeID), slog.String("targetPath", targetPath))

	if err := os.Remove(targetPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove target path %q: %w", targetPath, err)
	}
	return nil
}

func (p *Plugin) dir(volumeID string) string {
	return filepath.Join(stickyVolumeDirectories, volumeID)
}
