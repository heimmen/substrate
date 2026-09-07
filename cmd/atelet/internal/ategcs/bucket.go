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

package ategcs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	smithy "github.com/aws/smithy-go"
)

// BucketClient is an S3-compatible client for one objectStoreBucket volume,
// built from an explicit endpoint + static credentials rather than node env:
// a bucket volume's credentials arrive per-RPC from ateapi, which resolves
// them from the ActorTemplate namespace's Secret. It uses path-style
// addressing (required for MinIO behind a Service).
type BucketClient struct {
	client *s3.Client
}

// NewBucketClient builds a client for an S3-compatible endpoint such as
// "http://mfpi-minio.ate-demo-mf-pi.svc:9000".
func NewBucketClient(ctx context.Context, endpoint, accessKeyID, secretAccessKey string) (*BucketClient, error) {
	if endpoint == "" {
		return nil, fmt.Errorf("object-store endpoint is empty")
	}
	awsCfg, err := config.LoadDefaultConfig(ctx,
		// MinIO ignores the region but the AWS SDK requires one.
		config.WithRegion("us-east-1"),
		config.WithCredentialsProvider(aws.NewCredentialsCache(
			credentials.NewStaticCredentialsProvider(accessKeyID, secretAccessKey, ""),
		)),
	)
	if err != nil {
		return nil, fmt.Errorf("building object-store client config: %w", err)
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})
	return &BucketClient{client: client}, nil
}

// EnsureBucket creates the bucket if it does not yet exist (idempotent).
// Races (two creators) are tolerated: an already-owned bucket is success.
func (b *BucketClient) EnsureBucket(ctx context.Context, bucket string) error {
	if _, err := b.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)}); err == nil {
		return nil
	} else if !isNotFound(err) {
		return fmt.Errorf("head bucket %q: %w", bucket, err)
	}
	if _, err := b.client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		if isBucketAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("creating bucket %q: %w", bucket, err)
	}
	return nil
}

// HeadObject reports whether the object exists. A missing bucket or object is
// (false, nil); any other error is returned so callers can fail closed
// instead of mistaking a transient 5xx for "no data".
func (b *BucketClient) HeadObject(ctx context.Context, bucket, key string) (bool, error) {
	if _, err := b.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	}); err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("head object %s/%s: %w", bucket, key, err)
	}
	return true, nil
}

// GetObjectToFile downloads the object to dst (creating or truncating it).
func (b *BucketClient) GetObjectToFile(ctx context.Context, bucket, key, dst string) (err error) {
	out, err := b.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("get object %s/%s: %w", bucket, key, err)
	}
	defer func() {
		if closeErr := out.Body.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}()

	f, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("creating %q: %w", dst, err)
	}
	if _, err := io.Copy(f, out.Body); err != nil {
		f.Close()
		return fmt.Errorf("writing %q: %w", dst, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing %q: %w", dst, err)
	}
	return nil
}

// PutObjectFromFile uploads the file at src as the object. The body is a
// real file so the SDK can sign and set Content-Length without staging (the
// same file-backed PUT discipline the snapshot path uses for S3).
func (b *BucketClient) PutObjectFromFile(ctx context.Context, bucket, key, src string) (err error) {
	f, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("opening %q: %w", src, err)
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}()
	if _, err := b.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
		Body:   f,
	}); err != nil {
		return fmt.Errorf("put object %s/%s: %w", bucket, key, err)
	}
	return nil
}

// isNotFound reports whether err is an S3 404 (missing bucket or object).
// MinIO and AWS use different error codes, so match on the code string and
// fall back to the HTTP status.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NotFound", "NoSuchKey", "NoSuchBucket":
			return true
		}
	}
	var httpErr interface{ HTTPStatusCode() int }
	if errors.As(err, &httpErr) && httpErr.HTTPStatusCode() == 404 {
		return true
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
