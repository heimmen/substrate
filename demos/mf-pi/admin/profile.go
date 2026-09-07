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

// Per-user MinIO visibility for the mf-pi admin server.
//
// Each mf-pi user (one user == one Actor running pi-web) has a dedicated
// bucket in the MinIO object store dedicated to mf-pi user data, backing the
// actor's /data/pi-agent directory. The bucket sync itself is the platform's
// job: the ActorTemplate declares an objectStoreBucket volume (ateapi
// resolves its Secret, atelet mounts /data/pi-agent, rehydrates it from the
// bucket on an empty mount, and exports it back periodically and at
// suspend/delete time — see cmd/atelet/bucketsync.go and
// mount_minio_v2.md).
//
// This server only READS the store:
//
//   - EnsureBucket best-effort pre-creates a new user's bucket at
//     create-user time (atelet also creates it lazily on first mount).
//   - HasProfile backs the user-list "已同步" badge via HeadObject
//     LastModified on the single profile.tar.gz object per bucket — MinIO is
//     the source of truth, no mirrored Secret/RBAC needed.
//
// Deleting a user intentionally KEEPS the bucket/object — that is what makes
// a delete+recreate reset restore the profile automatically.
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	smithy "github.com/aws/smithy-go"
)

// profileObjectKey is the single object per user bucket holding the tar.gz of
// the user's /data/pi-agent profile. It must byte-match
// cmd/atelet/bucketsync.go's profileObjectKey (both the sync and this badge
// must address the same object).
const profileObjectKey = "profile.tar.gz"

// profileStore gives read-only visibility into a user's MinIO bucket.
// name is the actor/username; the bucket name is derived from it by the
// implementation (see s3ProfileStore.bucketName).
type profileStore interface {
	// EnsureBucket creates the user's bucket if it does not yet exist
	// (idempotent). Used to best-effort pre-create the bucket at create-user
	// time; atelet also creates it lazily on the actor's first mount.
	EnsureBucket(ctx context.Context, name string) error
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
// by the DNS-1123 username (itself a valid bucket name). This must match the
// platform's per-actor bucket derivation: ateapi computes
// bucketPrefix + actorName from the ActorTemplate volume, and the mf-pi
// template sets no prefix — so with a matching MINIO_BUCKET_PREFIX both
// always address the same bucket.
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
// create response message (atelet also ensures the bucket on first mount).
func (s *server) ensureProfileBucket(ctx context.Context, name string) string {
	if s.profiles == nil {
		return ""
	}
	if err := s.profiles.EnsureBucket(ctx, name); err != nil {
		return fmt.Sprintf("MinIO 数据桶创建失败（首次挂载时将自动重试）: %s", err.Error())
	}
	return ""
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
