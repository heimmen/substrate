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

package volume

// VolumePlugin is the combined interface a volume plugin must implement to be
// usable by both the control plane (ateapi) and the worker plane (atelet).
type VolumePlugin interface {
	VolumePluginControlPlane
	VolumePluginWorkerPlane
}

// defaultPlugin is the process-wide default volume plugin. It defaults to the
// mock plugin; a plugin replaces it by calling RegisterDefault (typically from
// an init function), which keeps the wiring sites free of plugin-specific code.
var defaultPlugin VolumePlugin = NewMockVolumePlugin()

// RegisterDefault sets the process-wide default volume plugin. The last
// registration wins; it is intended to be called from plugin init functions.
func RegisterDefault(p VolumePlugin) {
	defaultPlugin = p
}

// DefaultPlugin returns the process-wide default volume plugin.
func DefaultPlugin() VolumePlugin {
	return defaultPlugin
}
