// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controlapi

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/agent-substrate/substrate/internal/volume"
	"github.com/agent-substrate/substrate/pkg/api/v1alpha1"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// fakePurgerPlugin records PurgeVolume calls so tests can assert the RPC
// purges exactly the template-derived volume IDs, without touching the
// filesystem.
type fakePurgerPlugin struct {
	purged []string
}

func (f *fakePurgerPlugin) CreateVolume(_ context.Context, name, _, _ string) (string, error) {
	return "storage-" + name, nil
}
func (f *fakePurgerPlugin) DeleteVolume(_ context.Context, _ string) error   { return nil }
func (f *fakePurgerPlugin) AttachVolume(_ context.Context, _, _ string) error { return nil }
func (f *fakePurgerPlugin) DetachVolume(_ context.Context, _, _ string) error { return nil }
func (f *fakePurgerPlugin) MountVolume(_ context.Context, _, _ string) error   { return nil }
func (f *fakePurgerPlugin) UnmountVolume(_ context.Context, _, _ string) error { return nil }
func (f *fakePurgerPlugin) PurgeVolume(_ context.Context, volumeID string) error {
	f.purged = append(f.purged, volumeID)
	return nil
}

var _ volume.VolumePlugin = (*fakePurgerPlugin)(nil)
var _ volume.VolumePurger = (*fakePurgerPlugin)(nil)

func usePurgerPlugin(t *testing.T, fake *fakePurgerPlugin) {
	t.Helper()
	old := globalVolumePlugin
	globalVolumePlugin = fake
	t.Cleanup(func() { globalVolumePlugin = old })
}

// createTemplateWithExternalVolume creates a template like createTemplate but
// with one external volume, so PurgeActorVolumes has a volume ID to derive.
func createTemplateWithExternalVolume(t *testing.T, tc *testContext, ns string) {
	t.Helper()
	ensureDefaultGvisorSandboxConfig(t, tc)
	createWorkerPool(t, tc, ns, "pool1", map[string]string{poolLabelKey: ns})
	_, err := tc.substrateClient.ApiV1alpha1().ActorTemplates(ns).Create(context.Background(), &v1alpha1.ActorTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "tmpl1", Namespace: ns},
		Spec: v1alpha1.ActorTemplateSpec{
			PauseImage: "pause@sha256:abc",
			SnapshotsConfig: v1alpha1.SnapshotsConfig{
				Location: "gs://fake-fake-fake",
			},
			Containers: []v1alpha1.Container{
				{Name: "main", Image: "main@sha256:abc", Command: []string{"/main"},
					VolumeMounts: []v1alpha1.VolumeMount{{Name: "userdata", MountPath: "/data/pi-agent"}}},
			},
			Volumes: []v1alpha1.Volume{{
				Name: "userdata",
				VolumeSource: v1alpha1.VolumeSource{
					ExternalVolumeTemplate: &v1alpha1.ExternalVolumeTemplate{
						Capacity:         resource.MustParse("5Gi"),
						StorageClassName: "standard",
					},
				},
			}},
			WorkerSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{poolLabelKey: ns},
			},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to create actor template: %v", err)
	}

	// The service reads templates through a lister (informer cache), which
	// lags behind the API server. Wait until the template is visible.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := tc.actorTemplateLister.ActorTemplates(ns).Get("tmpl1"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("template tmpl1 never became visible in the lister cache")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func createActorForPurge(t *testing.T, tc *testContext, ns string) {
	t.Helper()
	if _, err := tc.client.CreateActor(context.Background(), &ateapipb.CreateActorRequest{
		Actor: &ateapipb.Actor{
			Metadata:               &ateapipb.ResourceMetadata{Atespace: testAtespace, Name: testActorID},
			ActorTemplateNamespace: ns,
			ActorTemplateName:      "tmpl1",
		},
	}); err != nil {
		t.Fatalf("seed CreateActor: %v", err)
	}
}

func wantVolumeIDs(atespace, actor string) []string {
	return []string{atespace + "-" + actor + "-userdata"}
}

func TestPurgeActorVolumes_PurgesTemplateVolumes(t *testing.T) {
	ns := namespaceForTest("ns-purge")
	tc := setupTest(t, ns)
	defer tc.cleanup()
	fake := &fakePurgerPlugin{}
	usePurgerPlugin(t, fake)

	createTemplateWithExternalVolume(t, tc, ns)
	createActorForPurge(t, tc, ns)

	resp, err := tc.client.PurgeActorVolumes(context.Background(), &ateapipb.PurgeActorVolumesRequest{
		Actor:    &ateapipb.ObjectRef{Atespace: testAtespace, Name: testActorID},
		Template: &ateapipb.ObjectRef{Atespace: ns, Name: "tmpl1"},
	})
	if err != nil {
		t.Fatalf("PurgeActorVolumes: %v", err)
	}
	want := wantVolumeIDs(testAtespace, testActorID)
	if !reflect.DeepEqual(resp.GetPurgedVolumeIds(), want) {
		t.Fatalf("PurgedVolumeIds = %v, want %v", resp.GetPurgedVolumeIds(), want)
	}
	if !reflect.DeepEqual(fake.purged, want) {
		t.Fatalf("plugin purged = %v, want %v", fake.purged, want)
	}
}

func TestPurgeActorVolumes_RefusesRunningActor(t *testing.T) {
	ns := namespaceForTest("ns-purge-running")
	tc := setupTest(t, ns)
	defer tc.cleanup()
	fake := &fakePurgerPlugin{}
	usePurgerPlugin(t, fake)

	createTemplateWithExternalVolume(t, tc, ns)
	createActorForPurge(t, tc, ns)

	// Flip the actor to RUNNING directly in the store (a full resume workflow
	// needs worker machinery this test does not exercise).
	a, err := tc.persistence.GetActor(context.Background(), testAtespace, testActorID)
	if err != nil {
		t.Fatalf("GetActor: %v", err)
	}
	a.Status = ateapipb.Actor_STATUS_RUNNING
	if _, err := tc.persistence.UpdateActor(context.Background(), a, a.GetMetadata().GetVersion()); err != nil {
		t.Fatalf("UpdateActor: %v", err)
	}

	_, err = tc.client.PurgeActorVolumes(context.Background(), &ateapipb.PurgeActorVolumesRequest{
		Actor:    &ateapipb.ObjectRef{Atespace: testAtespace, Name: testActorID},
		Template: &ateapipb.ObjectRef{Atespace: ns, Name: "tmpl1"},
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("PurgeActorVolumes on RUNNING actor: got code %v, want FailedPrecondition (err=%v)", status.Code(err), err)
	}
	if len(fake.purged) != 0 {
		t.Fatalf("plugin purged = %v, want none", fake.purged)
	}
}

func TestPurgeActorVolumes_ActorMayBeDeleted(t *testing.T) {
	ns := namespaceForTest("ns-purge-deleted")
	tc := setupTest(t, ns)
	defer tc.cleanup()
	fake := &fakePurgerPlugin{}
	usePurgerPlugin(t, fake)

	createTemplateWithExternalVolume(t, tc, ns)

	// No actor record at all: the post-delete purge case must succeed.
	resp, err := tc.client.PurgeActorVolumes(context.Background(), &ateapipb.PurgeActorVolumesRequest{
		Actor:    &ateapipb.ObjectRef{Atespace: testAtespace, Name: "gone"},
		Template: &ateapipb.ObjectRef{Atespace: ns, Name: "tmpl1"},
	})
	if err != nil {
		t.Fatalf("PurgeActorVolumes without actor record: %v", err)
	}
	if want := wantVolumeIDs(testAtespace, "gone"); !reflect.DeepEqual(resp.GetPurgedVolumeIds(), want) {
		t.Fatalf("PurgedVolumeIds = %v, want %v", resp.GetPurgedVolumeIds(), want)
	}
}

func TestPurgeActorVolumes_UnknownTemplate(t *testing.T) {
	ns := namespaceForTest("ns-purge-notmpl")
	tc := setupTest(t, ns)
	defer tc.cleanup()
	usePurgerPlugin(t, &fakePurgerPlugin{})

	_, err := tc.client.PurgeActorVolumes(context.Background(), &ateapipb.PurgeActorVolumesRequest{
		Actor:    &ateapipb.ObjectRef{Atespace: testAtespace, Name: testActorID},
		Template: &ateapipb.ObjectRef{Atespace: ns, Name: "missing"},
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("PurgeActorVolumes with missing template: got code %v, want FailedPrecondition (err=%v)", status.Code(err), err)
	}
}
