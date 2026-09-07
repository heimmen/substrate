//  Copyright 2026 Google LLC
//
//  Licensed under the Apache License, Version 2.0 (the "License");
//  you may not use this file except in compliance with the License.
//  You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
//  Unless required by applicable law or agreed to in writing, software
//  distributed under the License is distributed on an "AS IS" BASIS,
//  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//  See the License for the specific language governing permissions and
//  limitations under the License.

package controlapi

import (
	"context"
	"testing"

	"github.com/agent-substrate/substrate/internal/proto/ateletpb"
	atev1alpha1 "github.com/agent-substrate/substrate/pkg/api/v1alpha1"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/testing/protocmp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
)

func TestWorkloadSpecFromActorTemplate(t *testing.T) {
	tests := []struct {
		name     string
		template *atev1alpha1.ActorTemplate
		want     *ateletpb.WorkloadSpec
	}{
		{
			name: "converts DurableDir volume and mounts",
			template: &atev1alpha1.ActorTemplate{
				ObjectMeta: metav1.ObjectMeta{Name: "tmpl1", Namespace: "agent-ns"},
				Spec: atev1alpha1.ActorTemplateSpec{
					PauseImage: "pause",
					Volumes: []atev1alpha1.Volume{
						{Name: "home", VolumeSource: atev1alpha1.VolumeSource{DurableDir: &atev1alpha1.DurableDirVolumeSource{}}},
					},
					Containers: []atev1alpha1.Container{
						{
							Name:  "main",
							Image: "main",
							VolumeMounts: []atev1alpha1.VolumeMount{
								{Name: "home", MountPath: "/home/user"},
								{Name: "home", MountPath: "/workspace"},
							},
						},
					},
				},
			},
			want: &ateletpb.WorkloadSpec{
				PauseImage: "pause",
				Volumes: []*ateletpb.Volume{
					{
						Name:   "home",
						Type:   ateletpb.VolumeType_VOLUME_TYPE_DURABLE_DIR,
						Source: &ateletpb.Volume_DurableDir{DurableDir: &ateletpb.DurableDirVolume{}},
					},
				},
				Containers: []*ateletpb.Container{
					{
						Name:  "main",
						Image: "main",
						VolumeMounts: []*ateletpb.VolumeMount{
							{Name: "home", MountPath: "/home/user"},
							{Name: "home", MountPath: "/workspace"},
						},
					},
				},
			},
		},
		{
			name: "skips non-DurableDir volumes",
			template: &atev1alpha1.ActorTemplate{
				ObjectMeta: metav1.ObjectMeta{Name: "tmpl1", Namespace: "agent-ns"},
				Spec: atev1alpha1.ActorTemplateSpec{
					Volumes: []atev1alpha1.Volume{
						{Name: "unsupported", VolumeSource: atev1alpha1.VolumeSource{}},
						{Name: "home", VolumeSource: atev1alpha1.VolumeSource{DurableDir: &atev1alpha1.DurableDirVolumeSource{}}},
					},
					Containers: []atev1alpha1.Container{
						{
							Name:  "main",
							Image: "main",
							VolumeMounts: []atev1alpha1.VolumeMount{
								{Name: "home", MountPath: "/workspace"},
							},
						},
					},
				},
			},
			want: &ateletpb.WorkloadSpec{
				Volumes: []*ateletpb.Volume{
					{
						Name:   "home",
						Type:   ateletpb.VolumeType_VOLUME_TYPE_DURABLE_DIR,
						Source: &ateletpb.Volume_DurableDir{DurableDir: &ateletpb.DurableDirVolume{}},
					},
				},
				Containers: []*ateletpb.Container{
					{
						Name:  "main",
						Image: "main",
						VolumeMounts: []*ateletpb.VolumeMount{
							{Name: "home", MountPath: "/workspace"},
						},
					},
				},
			},
		},
		{
			name: "container without volume mounts has none",
			template: &atev1alpha1.ActorTemplate{
				ObjectMeta: metav1.ObjectMeta{Name: "tmpl1", Namespace: "agent-ns"},
				Spec: atev1alpha1.ActorTemplateSpec{
					Volumes: []atev1alpha1.Volume{
						{Name: "home", VolumeSource: atev1alpha1.VolumeSource{DurableDir: &atev1alpha1.DurableDirVolumeSource{}}},
					},
					Containers: []atev1alpha1.Container{
						{Name: "main", Image: "main"},
					},
				},
			},
			want: &ateletpb.WorkloadSpec{
				Volumes: []*ateletpb.Volume{
					{
						Name:   "home",
						Type:   ateletpb.VolumeType_VOLUME_TYPE_DURABLE_DIR,
						Source: &ateletpb.Volume_DurableDir{DurableDir: &ateletpb.DurableDirVolume{}},
					},
				},
				Containers: []*ateletpb.Container{{Name: "main", Image: "main"}},
			},
		},
		{
			name: "ignores container env",
			template: &atev1alpha1.ActorTemplate{
				ObjectMeta: metav1.ObjectMeta{Name: "tmpl1", Namespace: "agent-ns"},
				Spec: atev1alpha1.ActorTemplateSpec{
					Containers: []atev1alpha1.Container{
						{
							Name:  "main",
							Image: "main",
							Env: []atev1alpha1.EnvVar{
								{Name: "LITERAL", Value: ptr.To("plain")},
								{
									Name: "SECRET",
									ValueFrom: &atev1alpha1.EnvVarSource{
										SecretKeyRef: &atev1alpha1.SecretKeySelector{Name: "any", Key: "any"},
									},
								},
							},
						},
					},
				},
			},
			want: &ateletpb.WorkloadSpec{
				Containers: []*ateletpb.Container{{Name: "main", Image: "main"}},
			},
		},
		{
			name: "maps command and args",
			template: &atev1alpha1.ActorTemplate{
				ObjectMeta: metav1.ObjectMeta{Name: "tmpl1", Namespace: "agent-ns"},
				Spec: atev1alpha1.ActorTemplateSpec{
					Containers: []atev1alpha1.Container{
						{
							Name:    "main",
							Image:   "main",
							Command: []string{"/entrypoint"},
							Args:    []string{"--foo", "--bar"},
						},
					},
				},
			},
			want: &ateletpb.WorkloadSpec{
				Containers: []*ateletpb.Container{{
					Name:    "main",
					Image:   "main",
					Command: []string{"/entrypoint"},
					Args:    []string{"--foo", "--bar"},
				}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := workloadSpecFromActorTemplate(context.Background(), nil, nil, tt.template, nil)
			if err != nil {
				t.Fatalf("workloadSpecFromActorTemplate failed: %v", err)
			}
			if diff := cmp.Diff(tt.want, got, protocmp.Transform()); diff != "" {
				t.Errorf("WorkloadSpec mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestWorkloadSpecFromActorTemplateWithEnv(t *testing.T) {
	tests := []struct {
		name        string
		secrets     []runtime.Object
		template    *atev1alpha1.ActorTemplate
		want        *ateletpb.WorkloadSpec
		wantErrCode codes.Code
	}{
		{
			name: "resolves literal and secretKeyRef env",
			secrets: []runtime.Object{
				&corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{Name: "some-secret", Namespace: "agent-ns"},
					Data:       map[string][]byte{"some-key": []byte("some-value")},
				},
			},
			template: &atev1alpha1.ActorTemplate{
				ObjectMeta: metav1.ObjectMeta{Name: "tmpl1", Namespace: "agent-ns"},
				Spec: atev1alpha1.ActorTemplateSpec{
					PauseImage: "pause",
					Containers: []atev1alpha1.Container{
						{
							Name:    "main",
							Image:   "main",
							Command: []string{"/main"},
							Env: []atev1alpha1.EnvVar{
								{Name: "LITERAL", Value: ptr.To("plain")},
								{
									Name: "SOME_KEY",
									ValueFrom: &atev1alpha1.EnvVarSource{
										SecretKeyRef: &atev1alpha1.SecretKeySelector{Name: "some-secret", Key: "some-key"},
									},
								},
							},
						},
					},
				},
			},
			want: &ateletpb.WorkloadSpec{
				PauseImage: "pause",
				Containers: []*ateletpb.Container{
					{
						Name:    "main",
						Image:   "main",
						Command: []string{"/main"},
						Env: []*ateletpb.EnvEntry{
							{Name: "LITERAL", Value: "plain"},
							{Name: "SOME_KEY", Value: "some-value"},
						},
					},
				},
			},
		},
		{
			name: "skips optional missing secret",
			template: &atev1alpha1.ActorTemplate{
				ObjectMeta: metav1.ObjectMeta{Name: "tmpl1", Namespace: "agent-ns"},
				Spec: atev1alpha1.ActorTemplateSpec{
					Containers: []atev1alpha1.Container{
						{
							Name:  "main",
							Image: "main",
							Env: []atev1alpha1.EnvVar{
								{
									Name: "OPTIONAL",
									ValueFrom: &atev1alpha1.EnvVarSource{
										SecretKeyRef: &atev1alpha1.SecretKeySelector{Name: "missing", Key: "key", Optional: ptr.To(true)},
									},
								},
							},
						},
					},
				},
			},
			want: &ateletpb.WorkloadSpec{
				Containers: []*ateletpb.Container{{Name: "main", Image: "main"}},
			},
		},
		{
			name: "required missing secret fails",
			template: &atev1alpha1.ActorTemplate{
				ObjectMeta: metav1.ObjectMeta{Name: "tmpl1", Namespace: "agent-ns"},
				Spec: atev1alpha1.ActorTemplateSpec{
					Containers: []atev1alpha1.Container{
						{
							Name:  "main",
							Image: "main",
							Env: []atev1alpha1.EnvVar{
								{
									Name: "REQUIRED",
									ValueFrom: &atev1alpha1.EnvVarSource{
										SecretKeyRef: &atev1alpha1.SecretKeySelector{Name: "missing", Key: "key"},
									},
								},
							},
						},
					},
				},
			},
			wantErrCode: codes.FailedPrecondition,
		},
		{
			name: "empty valueFrom fails",
			template: &atev1alpha1.ActorTemplate{
				ObjectMeta: metav1.ObjectMeta{Name: "tmpl1", Namespace: "agent-ns"},
				Spec: atev1alpha1.ActorTemplateSpec{
					Containers: []atev1alpha1.Container{
						{
							Name:  "main",
							Image: "main",
							Env: []atev1alpha1.EnvVar{
								{Name: "EMPTY", ValueFrom: &atev1alpha1.EnvVarSource{}},
							},
						},
					},
				},
			},
			wantErrCode: codes.FailedPrecondition,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kubeClient := fake.NewSimpleClientset(tt.secrets...)
			got, err := workloadSpecFromActorTemplateWithEnv(context.Background(), kubeClient, nil, tt.template, nil)
			if tt.wantErrCode != codes.OK {
				if status.Code(err) != tt.wantErrCode {
					t.Fatalf("error code = %v, want %v: %v", status.Code(err), tt.wantErrCode, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("workloadSpecFromActorTemplateWithEnv failed: %v", err)
			}
			if diff := cmp.Diff(tt.want, got, protocmp.Transform()); diff != "" {
				t.Errorf("WorkloadSpec mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestWorkloadSpecFromActorTemplatePropagatesReadyz(t *testing.T) {
	got, err := workloadSpecFromActorTemplate(context.Background(), nil, nil, &atev1alpha1.ActorTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "tmpl-readyz", Namespace: "agent-ns"},
		Spec: atev1alpha1.ActorTemplateSpec{
			Containers: []atev1alpha1.Container{
				{
					Name:  "with-probe",
					Image: "main",
					Readyz: &atev1alpha1.ContainerReadyz{
						HTTPGet: &atev1alpha1.HTTPGetAction{Path: "/health", Port: 8080},
					},
				},
				{
					Name:  "without-probe",
					Image: "side",
				},
			},
		},
	}, nil)
	if err != nil {
		t.Fatalf("workloadSpecFromActorTemplate failed: %v", err)
	}

	want := &ateletpb.WorkloadSpec{
		Containers: []*ateletpb.Container{
			{
				Name:  "with-probe",
				Image: "main",
				Readyz: &ateletpb.Readyz{
					HttpGet: &ateletpb.HTTPGetAction{Path: "/health", Port: 8080},
				},
			},
			{
				Name:  "without-probe",
				Image: "side",
			},
		},
	}
	if diff := cmp.Diff(want, got, protocmp.Transform()); diff != "" {
		t.Errorf("WorkloadSpec mismatch (-want +got):\n%s", diff)
	}
}

func TestWorkloadSpecFromActorTemplateWithEnvCachesSecretsAcrossCalls(t *testing.T) {
	ctx := context.Background()
	secretCache := newEnvSecretCache(envSecretCacheTTL)
	kubeClient := fake.NewSimpleClientset(
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "some-secret",
				Namespace: "agent-ns",
			},
			Data: map[string][]byte{
				"some-key": []byte("some-value"),
			},
		},
	)
	actorTemplate := &atev1alpha1.ActorTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tmpl1",
			Namespace: "agent-ns",
		},
		Spec: atev1alpha1.ActorTemplateSpec{
			Containers: []atev1alpha1.Container{
				{
					Name:  "main",
					Image: "main",
					Env: []atev1alpha1.EnvVar{
						{
							Name: "SOME_KEY",
							ValueFrom: &atev1alpha1.EnvVarSource{
								SecretKeyRef: &atev1alpha1.SecretKeySelector{
									Name: "some-secret",
									Key:  "some-key",
								},
							},
						},
					},
				},
			},
		},
	}

	if _, err := workloadSpecFromActorTemplateWithEnv(ctx, kubeClient, secretCache, actorTemplate, nil); err != nil {
		t.Fatalf("first workloadSpecFromActorTemplateWithEnv failed: %v", err)
	}
	if _, err := workloadSpecFromActorTemplateWithEnv(ctx, kubeClient, secretCache, actorTemplate, nil); err != nil {
		t.Fatalf("second workloadSpecFromActorTemplateWithEnv failed: %v", err)
	}
	if got := secretGetCount(kubeClient); got != 1 {
		t.Fatalf("secret gets before TTL expiry = %d, want 1", got)
	}

	expireSecretCache(secretCache)
	if _, err := workloadSpecFromActorTemplateWithEnv(ctx, kubeClient, secretCache, actorTemplate, nil); err != nil {
		t.Fatalf("third workloadSpecFromActorTemplateWithEnv failed: %v", err)
	}
	if got := secretGetCount(kubeClient); got != 2 {
		t.Fatalf("secret gets after TTL expiry = %d, want 2", got)
	}
}

func expireSecretCache(secretCache *envSecretCache) {
	secretCache.mu.Lock()
	defer secretCache.mu.Unlock()

	for key, entry := range secretCache.entries {
		entry.expiresAt = entry.expiresAt.Add(-envSecretCacheTTL)
		secretCache.entries[key] = entry
	}
}

func secretGetCount(kubeClient *fake.Clientset) int {
	count := 0
	for _, action := range kubeClient.Actions() {
		if action.GetVerb() == "get" && action.GetResource().Resource == "secrets" {
			count++
		}
	}
	return count
}

func TestAppendExternalVolumes(t *testing.T) {
	template := &atev1alpha1.ActorTemplate{
		Spec: atev1alpha1.ActorTemplateSpec{
			Containers: []atev1alpha1.Container{
				{
					Name: "main",
					VolumeMounts: []atev1alpha1.VolumeMount{
						{Name: "vol-1", MountPath: "/mnt/vol1"},
					},
				},
			},
			Volumes: []atev1alpha1.Volume{
				{
					Name: "vol-1",
					VolumeSource: atev1alpha1.VolumeSource{
						ExternalVolumeTemplate: &atev1alpha1.ExternalVolumeTemplate{
							StorageClassName: "pd-standard",
						},
					},
				},
				{
					Name: "vol-2",
					VolumeSource: atev1alpha1.VolumeSource{
						DurableDir: &atev1alpha1.DurableDirVolumeSource{},
					},
				},
				{
					Name: "unmounted-vol",
					VolumeSource: atev1alpha1.VolumeSource{
						ExternalVolumeTemplate: &atev1alpha1.ExternalVolumeTemplate{
							StorageClassName: "pd-standard",
						},
					},
				},
			},
		},
	}

	actor := &ateapipb.Actor{
		Metadata: &ateapipb.ResourceMetadata{
			Atespace: "space-abc",
			Name:     "actor-123",
		},
		ActorVolumes: []*ateapipb.ExternalVolume{
			{
				ActorVolumeId:   "space-abc-actor-123-vol-1",
				StorageVolumeId: "vol-gce-pd-123",
				VolumeType:      "pd-standard",
			},
		},
	}

	workloadSpec := &ateletpb.WorkloadSpec{}
	if err := appendExternalVolumes(workloadSpec, template, actor); err != nil {
		t.Fatalf("appendExternalVolumes unexpected error: %v", err)
	}

	want := &ateletpb.WorkloadSpec{
		Volumes: []*ateletpb.Volume{
			{
				Name: "vol-1",
				Type: ateletpb.VolumeType_VOLUME_TYPE_EXTERNAL,
				Source: &ateletpb.Volume_External{
					External: &ateletpb.ExternalVolumeSource{
						StorageVolumeId: "vol-gce-pd-123",
						VolumeType:      "pd-standard",
					},
				},
			},
		},
	}

	if diff := cmp.Diff(want, workloadSpec, protocmp.Transform()); diff != "" {
		t.Errorf("appendExternalVolumes mismatch (-want +got):\n%s", diff)
	}

	// Test missing mounted volume returns an error
	missingActor := &ateapipb.Actor{
		Metadata: &ateapipb.ResourceMetadata{
			Atespace: "space-abc",
			Name:     "actor-123",
		},
		ActorVolumes: []*ateapipb.ExternalVolume{},
	}
	if err := appendExternalVolumes(&ateletpb.WorkloadSpec{}, template, missingActor); err == nil {
		t.Errorf("appendExternalVolumes expected error for missing volume, got nil")
	}
}

func bucketVolumeTemplate(name, secretName string, prefix string) *atev1alpha1.ActorTemplate {
	return &atev1alpha1.ActorTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "tmpl1", Namespace: "agent-ns"},
		Spec: atev1alpha1.ActorTemplateSpec{
			Volumes: []atev1alpha1.Volume{
				{
					Name: name,
					VolumeSource: atev1alpha1.VolumeSource{
						ObjectStoreBucket: &atev1alpha1.ObjectStoreBucketVolumeSource{
							SecretRef: atev1alpha1.ObjectStoreBucketSecretRef{
								Name:               secretName,
								EndpointKey:        "endpoint",
								AccessKeyIdKey:     "root-user",
								SecretAccessKeyKey: "root-password",
							},
							BucketPrefix:          prefix,
							ExportIntervalSeconds: ptr.To(int32(30)),
						},
					},
				},
			},
			Containers: []atev1alpha1.Container{
				{
					Name:  "main",
					Image: "main",
					VolumeMounts: []atev1alpha1.VolumeMount{
						{Name: name, MountPath: "/data/pi-agent"},
					},
				},
			},
		},
	}
}

func minioAdminSecret(name string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "agent-ns"},
		Data: map[string][]byte{
			"endpoint":      []byte("http://minio.agent-ns.svc:9000"),
			"root-user":     []byte("minioadmin"),
			"root-password": []byte("minioadmin-password"),
		},
	}
}

func TestAppendObjectStoreBucketVolumes(t *testing.T) {
	actor := &ateapipb.Actor{
		Metadata: &ateapipb.ResourceMetadata{Atespace: "mfpi", Name: "alice"},
	}

	t.Run("resolves secret keys and derives per-actor bucket", func(t *testing.T) {
		kubeClient := fake.NewSimpleClientset(minioAdminSecret("mfpi-minio-admin"))
		resolver := &envResolver{kubeClient: kubeClient, namespace: "agent-ns"}
		spec := &ateletpb.WorkloadSpec{}
		if err := appendObjectStoreBucketVolumes(context.Background(), spec, bucketVolumeTemplate("pi-agent-data", "mfpi-minio-admin", ""), actor, resolver); err != nil {
			t.Fatalf("appendObjectStoreBucketVolumes: %v", err)
		}
		want := &ateletpb.WorkloadSpec{
			Volumes: []*ateletpb.Volume{
				{
					Name: "pi-agent-data",
					Type: ateletpb.VolumeType_VOLUME_TYPE_OBJECT_STORE_BUCKET,
					Source: &ateletpb.Volume_ObjectStoreBucket{
						ObjectStoreBucket: &ateletpb.ObjectStoreBucketVolumeSource{
							Endpoint:              "http://minio.agent-ns.svc:9000",
							AccessKeyId:           "minioadmin",
							SecretAccessKey:       "minioadmin-password",
							Bucket:                "alice",
							ExportIntervalSeconds: 30,
						},
					},
				},
			},
		}
		if diff := cmp.Diff(want, spec, protocmp.Transform()); diff != "" {
			t.Errorf("workload spec mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("bucket is prefix plus actor name", func(t *testing.T) {
		kubeClient := fake.NewSimpleClientset(minioAdminSecret("mfpi-minio-admin"))
		resolver := &envResolver{kubeClient: kubeClient, namespace: "agent-ns"}
		spec := &ateletpb.WorkloadSpec{}
		if err := appendObjectStoreBucketVolumes(context.Background(), spec, bucketVolumeTemplate("pi-agent-data", "mfpi-minio-admin", "mfpi-"), actor, resolver); err != nil {
			t.Fatalf("appendObjectStoreBucketVolumes: %v", err)
		}
		got := spec.Volumes[0].GetObjectStoreBucket().GetBucket()
		if got != "mfpi-alice" {
			t.Errorf("bucket = %q, want mfpi-alice", got)
		}
	})

	t.Run("missing secret fails with FailedPrecondition", func(t *testing.T) {
		kubeClient := fake.NewSimpleClientset()
		resolver := &envResolver{kubeClient: kubeClient, namespace: "agent-ns"}
		err := appendObjectStoreBucketVolumes(context.Background(), &ateletpb.WorkloadSpec{}, bucketVolumeTemplate("pi-agent-data", "missing", ""), actor, resolver)
		if status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("code = %v, want FailedPrecondition: %v", status.Code(err), err)
		}
	})

	t.Run("missing key fails with FailedPrecondition", func(t *testing.T) {
		s := minioAdminSecret("mfpi-minio-admin")
		delete(s.Data, "root-password")
		kubeClient := fake.NewSimpleClientset(s)
		resolver := &envResolver{kubeClient: kubeClient, namespace: "agent-ns"}
		err := appendObjectStoreBucketVolumes(context.Background(), &ateletpb.WorkloadSpec{}, bucketVolumeTemplate("pi-agent-data", "mfpi-minio-admin", ""), actor, resolver)
		if status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("code = %v, want FailedPrecondition: %v", status.Code(err), err)
		}
	})

	t.Run("unmounted bucket volume is skipped", func(t *testing.T) {
		kubeClient := fake.NewSimpleClientset(minioAdminSecret("mfpi-minio-admin"))
		resolver := &envResolver{kubeClient: kubeClient, namespace: "agent-ns"}
		tmpl := bucketVolumeTemplate("pi-agent-data", "mfpi-minio-admin", "")
		tmpl.Spec.Containers[0].VolumeMounts = nil
		spec := &ateletpb.WorkloadSpec{}
		if err := appendObjectStoreBucketVolumes(context.Background(), spec, tmpl, actor, resolver); err != nil {
			t.Fatalf("appendObjectStoreBucketVolumes: %v", err)
		}
		if len(spec.Volumes) != 0 {
			t.Errorf("unmounted volume appended: %v", spec.Volumes)
		}
	})

	t.Run("nil actor fails", func(t *testing.T) {
		kubeClient := fake.NewSimpleClientset(minioAdminSecret("mfpi-minio-admin"))
		resolver := &envResolver{kubeClient: kubeClient, namespace: "agent-ns"}
		err := appendObjectStoreBucketVolumes(context.Background(), &ateletpb.WorkloadSpec{}, bucketVolumeTemplate("pi-agent-data", "mfpi-minio-admin", ""), nil, resolver)
		if status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("code = %v, want FailedPrecondition: %v", status.Code(err), err)
		}
	})
}

// The env-less builder (pause/suspend path) must resolve bucket volumes so the
// Checkpoint RPC carries the credentials atelet needs for the final export.
func TestWorkloadSpecFromActorTemplateResolvesBucketVolume(t *testing.T) {
	kubeClient := fake.NewSimpleClientset(minioAdminSecret("mfpi-minio-admin"))
	actor := &ateapipb.Actor{
		Metadata: &ateapipb.ResourceMetadata{Atespace: "mfpi", Name: "alice"},
	}
	spec, err := workloadSpecFromActorTemplate(context.Background(), kubeClient, nil, bucketVolumeTemplate("pi-agent-data", "mfpi-minio-admin", ""), actor)
	if err != nil {
		t.Fatalf("workloadSpecFromActorTemplate: %v", err)
	}
	if len(spec.Volumes) != 1 {
		t.Fatalf("len(volumes) = %d, want 1", len(spec.Volumes))
	}
	src := spec.Volumes[0].GetObjectStoreBucket()
	if src == nil {
		t.Fatalf("volume source = %T, want ObjectStoreBucketVolumeSource", spec.Volumes[0].GetSource())
	}
	if src.GetBucket() != "alice" || src.GetEndpoint() == "" || src.GetAccessKeyId() == "" || src.GetSecretAccessKey() == "" {
		t.Errorf("unresolved bucket volume: %+v", src)
	}
	// No env resolution happens on this path.
	for _, ctr := range spec.Containers {
		if len(ctr.GetEnv()) != 0 {
			t.Errorf("env resolved on the env-less path: %v", ctr.GetEnv())
		}
	}
}
