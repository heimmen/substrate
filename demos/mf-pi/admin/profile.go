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

// Per-user MinIO profile persistence for the mf-pi admin server.
//
// Each mf-pi user (one user == one Actor running pi-web) gets a dedicated
// bucket in a real MinIO object store dedicated to mf-pi user data. The
// actor's /data/pi-agent profile (auth.json / skills / sessions / models.json
// / settings.json) is periodically pushed here by an in-actor supervisor and
// pulled back on a cold boot after a delete+recreate "reset", so the user's
// data survives an instance reset and is auto-loaded. Isolation is per-user
// bucket; this server is the only holder of the MinIO credential (a single
// central admin access key), so actors never see MinIO credentials.
//
// The admin is the trusted S3 broker: it does all S3 I/O with
// aws-sdk-go-v2/service/s3 (already a direct module dependency) and exposes
// two token-gated HTTP endpoints the supervisor talks to:
//
//	GET /internal/actor/{name}/profile   -> 200 profile.tar.gz | 204 no profile
//	PUT /internal/actor/{name}/profile   -> stores the pushed tar.gz
//
// The badge shown in the user list ("已同步") reads the S3 object itself
// (HeadObject LastModified) rather than a mirrored k8s Secret, so MinIO is
// the source of truth and no per-user metadata Secret/RBAC is needed. Deleting
// a user intentionally KEEPS the bucket/object — that is what makes a
// delete+recreate reset restore the profile on the next cold boot.
package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	smithy "github.com/aws/smithy-go"
)

const profileObjectKey = "profile.tar.gz"

// maxProfileBytes caps a supervisor push. A pi-agent profile (auth.json,
// models.json, settings.json, sessions and a few skills) is normally well
// under a few MB; this guards against a runaway tar filling admin memory.
// A var (not const) so tests can lower it.
var maxProfileBytes = 64 << 20 // 64 MiB

// profileStore persists a user's /data/pi-agent profile tar.gz in the user's
// dedicated MinIO bucket. name is the actor/username; the bucket name is
// derived from it by the implementation (see s3ProfileStore.bucketName).
type profileStore interface {
	// EnsureBucket creates the user's bucket if it does not yet exist
	// (idempotent). Buckets are created lazily on the first profile push, so
	// a user created by any path gets a bucket the first time they run.
	EnsureBucket(ctx context.Context, name string) error
	// GetProfile returns a reader over the user's stored profile tar.gz and
	// its last-modified time. ok is false when nothing is stored yet.
	GetProfile(ctx context.Context, name string) (rc io.ReadCloser, mod time.Time, ok bool, err error)
	// PutProfile stores data as the user's profile tar.gz and returns the
	// object's last-modified time.
	PutProfile(ctx context.Context, name string, data []byte) (mod time.Time, err error)
	// HasProfile reports whether the user has a stored profile and its
	// last-modified time. A missing bucket/object is not an error (ok=false).
	HasProfile(ctx context.Context, name string) (mod time.Time, ok bool, err error)
}

// s3ProfileStore implements profileStore over a real S3 endpoint (MinIO in
// the demo). It uses one central admin credential for every user bucket and
// path-style addressing (required for MinIO behind a Service).
type s3ProfileStore struct {
	client *s3.Client
	prefix string
}

// newS3ProfileStore builds a MinIO/S3 client from a static credential and a
// base endpoint. endpoint is like "http://mfpi-minio.ate-demo-mf-pi.svc:9000".
func newS3ProfileStore(ctx context.Context, endpoint, accessKey, secretKey, region, prefix string) (*s3ProfileStore, error) {
	if region == "" {
		region = "us-east-1"
	}
	awsCfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(region),
		config.WithCredentialsProvider(aws.NewCredentialsCache(
			credentials.NewStaticCredentialsProvider(accessKey, secretKey, ""),
		)),
	)
	if err != nil {
		return nil, fmt.Errorf("building MinIO client config: %w", err)
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})
	return &s3ProfileStore{client: client, prefix: prefix}, nil
}

// bucketName returns the bucket for a user: an optional static prefix followed
// by the DNS-1123 username (itself a valid bucket name).
func (s *s3ProfileStore) bucketName(name string) string {
	return s.prefix + name
}

// EnsureBucket creates the user's bucket if missing. Races (two first pushes)
// are tolerated: an already-owned bucket is success.
func (s *s3ProfileStore) EnsureBucket(ctx context.Context, name string) error {
	bucket := s.bucketName(name)
	if _, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)}); err == nil {
		return nil
	} else if !isS3NotFound(err) {
		return err
	}
	if _, err := s.client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		if isBucketAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("creating bucket %q: %w", bucket, err)
	}
	return nil
}

func (s *s3ProfileStore) GetProfile(ctx context.Context, name string) (io.ReadCloser, time.Time, bool, error) {
	bucket := s.bucketName(name)
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(profileObjectKey),
	})
	if err != nil {
		if isS3NotFound(err) {
			return nil, time.Time{}, false, nil
		}
		return nil, time.Time{}, false, fmt.Errorf("reading profile from bucket %q: %w", bucket, err)
	}
	return out.Body, aws.ToTime(out.LastModified), true, nil
}

func (s *s3ProfileStore) PutProfile(ctx context.Context, name string, data []byte) (time.Time, error) {
	bucket := s.bucketName(name)
	if _, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(profileObjectKey),
		Body:   bytes.NewReader(data),
	}); err != nil {
		return time.Time{}, fmt.Errorf("writing profile to bucket %q: %w", bucket, err)
	}
	// The S3 PutObject response carries no LastModified; MinIO stamps the real
	// object time on HEAD/GET, which is what HasProfile/GetProfile report.
	return time.Now().UTC(), nil
}

func (s *s3ProfileStore) HasProfile(ctx context.Context, name string) (time.Time, bool, error) {
	bucket := s.bucketName(name)
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(profileObjectKey),
	})
	if err != nil {
		if isS3NotFound(err) {
			return time.Time{}, false, nil
		}
		return time.Time{}, false, fmt.Errorf("stat profile in bucket %q: %w", bucket, err)
	}
	return aws.ToTime(out.LastModified), true, nil
}

// isS3NotFound reports whether err is an S3 404 (missing bucket or object).
// MinIO and AWS use different error codes, so match on the code string.
func isS3NotFound(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NotFound", "NoSuchKey", "NoSuchBucket":
			return true
		}
	}
	return false
}

// isBucketAlreadyExists reports whether a CreateBucket failed because the
// bucket already exists (a concurrent creator won the race).
func isBucketAlreadyExists(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "BucketAlreadyExists", "BucketAlreadyOwnedByYou":
			return true
		}
	}
	return false
}

// ensureProfileBucket best-effort pre-creates the user's MinIO bucket. It
// never fails the caller: on any problem it returns a warning string for the
// create response message (the bucket is also ensured lazily on first push).
func (s *server) ensureProfileBucket(ctx context.Context, name string) string {
	if s.profiles == nil {
		return ""
	}
	if err := s.profiles.EnsureBucket(ctx, name); err != nil {
		return fmt.Sprintf("MinIO 数据桶创建失败（首次同步时将自动重试）: %s", err.Error())
	}
	return ""
}

// profileGate authorizes an /internal/actor/* request. The supervisor sends
// the shared MFPI_PROFILE_TOKEN as "Authorization: Bearer <token>". A server
// without a configured token refuses everything (fail-closed misconfiguration).
func (s *server) profileGate(w http.ResponseWriter, r *http.Request) bool {
	if s.profileToken == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "profile sync 未配置（缺少 MFPI_PROFILE_TOKEN）"})
		return false
	}
	const prefix = "Bearer "
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, prefix) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing bearer token"})
		return false
	}
	got := strings.TrimPrefix(auth, prefix)
	if subtle.ConstantTimeCompare([]byte(got), []byte(s.profileToken)) != 1 {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid bearer token"})
		return false
	}
	if s.profiles == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "MinIO 未配置"})
		return false
	}
	return true
}

// handleInternalActor serves the supervisor's profile pull/push endpoints:
//
//	GET /internal/actor/{name}/profile
//	PUT /internal/actor/{name}/profile
func (s *server) handleInternalActor(w http.ResponseWriter, r *http.Request) {
	if !s.profileGate(w, r) {
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/internal/actor/"), "/")
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(rest, "/profile"):
		s.handleProfileGet(w, r, strings.TrimSuffix(rest, "/profile"))
	case r.Method == http.MethodPut && strings.HasSuffix(rest, "/profile"):
		s.handleProfilePut(w, r, strings.TrimSuffix(rest, "/profile"))
	default:
		http.NotFound(w, r)
	}
}

func (s *server) handleProfileGet(w http.ResponseWriter, r *http.Request, name string) {
	if !validUserName(name) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "非法用户名"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	rc, _, ok, err := s.profiles.GetProfile(ctx, name)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取 profile 失败: " + err.Error()})
		return
	}
	if !ok {
		// 204: the actor has no stored profile yet (fresh user). The supervisor
		// treats this as "nothing to restore" and continues without error.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Cache-Control", "no-store")
	if _, err := io.Copy(w, rc); err != nil {
		// Headers are already written; nothing more we can do but log.
		log.Printf("streaming profile for %q failed: %v", name, err)
	}
}

func (s *server) handleProfilePut(w http.ResponseWriter, r *http.Request, name string) {
	if !validUserName(name) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "非法用户名"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	// Bounded read: enforce the push cap before handing bytes to S3.
	data, err := io.ReadAll(io.LimitReader(r.Body, int64(maxProfileBytes)+1))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "读取请求体失败: " + err.Error()})
		return
	}
	if len(data) > maxProfileBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{
			"error": fmt.Sprintf("profile 超过 %d MiB 上限", maxProfileBytes>>20),
		})
		return
	}

	// Lazy bucket creation: a user created by any path (including create-user.sh,
	// which bypasses the admin REST create) gets a bucket on the first push.
	if err := s.profiles.EnsureBucket(ctx, name); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "确保 MinIO 数据桶失败: " + err.Error()})
		return
	}
	if _, err := s.profiles.PutProfile(ctx, name, data); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "写入 MinIO 失败: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"bytes": len(data)})
}

// setProfileSync fills the user summary's MinIO sync badge from the profile
// store. Failures degrade silently (the badge just shows "未同步"); a short
// per-user timeout keeps a slow MinIO from stalling the user list.
func (u *userSummary) setProfileSync(ps profileStore, name string, parent context.Context) {
	if ps == nil {
		return
	}
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	mod, ok, err := ps.HasProfile(ctx, name)
	if err != nil || !ok {
		return
	}
	u.ProfileSynced = true
	u.ProfileLastSync = mod.UTC().Format(time.RFC3339)
}

// validUserName reports whether name is a legal actor/username (DNS-1123 with
// no path separator), the same rule enforced by handleUserSubresource.
func validUserName(name string) bool {
	return name != "" && !strings.Contains(name, "/") && dns1123Re.MatchString(name)
}
