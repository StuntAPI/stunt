package conformance

import (
	"bytes"
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// TestAWSSDKConformance drives aws-sdk-go-v2 (STS + S3) against the
// aws-iam-sts-style and aws-s3-style adapters with the adapters'
// documented synthetic credentials. The SDK signs every request with REAL
// SigV4 — passing means the adapters' signature verification accepts the
// genuine algorithm output, not just hand-rolled test vectors.
func TestAWSSDKConformance(t *testing.T) {
	ctx := context.Background()

	// The long-public example credentials from the AWS docs, which the
	// adapters' SigV4 verification is keyed to.
	cfgCreds := credentials.NewStaticCredentialsProvider(
		"AKIAIOSFODNN7EXAMPLE",
		"wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		"")

	// ===== STS: GetCallerIdentity + AssumeRole (query protocol + SigV4) =====

	stsBase := Boot(t, "aws-iam-sts-style")
	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion("us-east-1"),
		config.WithCredentialsProvider(cfgCreds),
	)
	if err != nil {
		t.Fatalf("aws config: %v", err)
	}
	stsClient := sts.NewFromConfig(cfg, func(o *sts.Options) {
		o.BaseEndpoint = &stsBase
	})

	id, err := stsClient.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		t.Fatalf("GetCallerIdentity (real SigV4): %v", err)
	}
	if id.Account == nil || *id.Account == "" {
		t.Fatal("GetCallerIdentity Account empty")
	}
	if id.Arn == nil || *id.Arn == "" {
		t.Fatal("GetCallerIdentity Arn empty")
	}
	Record(t, "aws-sdk-go-v2", "aws-iam-sts-style", "GetCallerIdentity with real SigV4 signature")

	role, err := stsClient.AssumeRole(ctx, &sts.AssumeRoleInput{
		RoleArn:         id.Arn,
		RoleSessionName: ptr("conformance-session"),
	})
	if err != nil {
		t.Fatalf("AssumeRole: %v", err)
	}
	if role.Credentials == nil || role.Credentials.AccessKeyId == nil {
		t.Fatal("AssumeRole returned no credentials")
	}
	Record(t, "aws-sdk-go-v2", "aws-iam-sts-style", "AssumeRole -> credentials")

	// ===== S3: bucket + object lifecycle (path-style, SigV4, raw bytes) =====

	s3Base := Boot(t, "aws-s3-style")
	s3Client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = &s3Base
		o.UsePathStyle = true
	})

	bucket := "conformance-bucket"
	if _, err := s3Client.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: ptr(bucket),
	}); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	Record(t, "aws-sdk-go-v2", "aws-s3-style", "CreateBucket")

	content := []byte("stunt conformance payload \x00\xff\x01 — binary round-trip")
	if _, err := s3Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: ptr(bucket),
		Key:    ptr("bin/payload.bin"),
		Body:   bytes.NewReader(content),
	}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	Record(t, "aws-sdk-go-v2", "aws-s3-style", "PutObject (binary body)")

	for i := 0; i < 3; i++ {
		if _, err := s3Client.PutObject(ctx, &s3.PutObjectInput{
			Bucket: ptr(bucket),
			Key:    ptr("keys/k" + string(rune('0'+i)) + ".txt"),
			Body:   bytes.NewReader([]byte("v")),
		}); err != nil {
			t.Fatalf("PutObject seed %d: %v", i, err)
		}
	}

	got, err := s3Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: ptr(bucket),
		Key:    ptr("bin/payload.bin"),
	})
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	defer got.Body.Close()
	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(got.Body); err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), content) {
		t.Fatalf("GetObject round-trip mismatch: got %d bytes, want %d", buf.Len(), len(content))
	}
	Record(t, "aws-sdk-go-v2", "aws-s3-style", "GetObject byte-exact round-trip (incl. non-UTF-8)")

	head, err := s3Client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: ptr(bucket),
		Key:    ptr("bin/payload.bin"),
	})
	if err != nil {
		t.Fatalf("HeadObject: %v", err)
	}
	if head.ContentLength == nil || *head.ContentLength != int64(len(content)) {
		t.Fatalf("HeadObject ContentLength = %v, want %d", head.ContentLength, len(content))
	}
	Record(t, "aws-sdk-go-v2", "aws-s3-style", "HeadObject metadata")

	// ListObjectsV2 with continuation: 4 objects, page size 2 — the SDK's
	// paginator must follow IsTruncated/NextContinuationToken.
	var listed int
	p := s3.NewListObjectsV2Paginator(s3Client, &s3.ListObjectsV2Input{
		Bucket:  ptr(bucket),
		MaxKeys: ptr[int32](2),
		Prefix:  ptr(""),
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			t.Fatalf("ListObjectsV2 page: %v", err)
		}
		if page.KeyCount != nil {
			listed += int(*page.KeyCount)
		} else {
			listed += len(page.Contents)
		}
	}
	if listed != 4 {
		t.Fatalf("ListObjectsV2 paginator listed %d objects, want 4 (continuation not followed?)", listed)
	}
	Record(t, "aws-sdk-go-v2", "aws-s3-style", "ListObjectsV2 paginator follows continuation (4 over MaxKeys=2)")

	if _, err := s3Client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: ptr(bucket),
		Key:    ptr("keys/k0.txt"),
	}); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}
	Record(t, "aws-sdk-go-v2", "aws-s3-style", "DeleteObject")
}

func ptr[T any](v T) *T { return &v }
