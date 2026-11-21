package conformance

import (
	"bytes"
	"context"
	"io"
	"net/url"
	"testing"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func TestAWSS3StorageOperations(t *testing.T) {
	ctx := context.Background()
	endpoint := Boot(t, "aws-s3-style")
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion("us-east-1"), config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", "")))
	if err != nil {
		t.Fatal(err)
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) { o.BaseEndpoint = &endpoint; o.UsePathStyle = true })
	bucket := "storage-contract"
	if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: &bucket}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: &bucket}); err != nil {
		t.Fatalf("HeadBucket: %v", err)
	}
	if _, err := client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: ptr("missing-bucket")}); err == nil {
		t.Fatal("missing HeadBucket succeeded")
	}
	Record(t, "aws-sdk-go-v2", "aws-s3-style", "HeadBucket existence probe")

	source, destination := "copy/a +&é.bin", "copy/destination.bin"
	body := []byte{0, 127, 128, 255}
	if _, err := client.PutObject(ctx, &s3.PutObjectInput{Bucket: &bucket, Key: &source, Body: bytes.NewReader(body), ContentType: ptr("application/octet-stream"), Metadata: map[string]string{"owner": "synthetic"}}); err != nil {
		t.Fatal(err)
	}
	copySource := bucket + "/" + url.PathEscape(source)
	copied, err := client.CopyObject(ctx, &s3.CopyObjectInput{Bucket: &bucket, Key: &destination, CopySource: &copySource})
	if err != nil {
		t.Fatalf("CopyObject: %v", err)
	}
	if copied.CopyObjectResult == nil || copied.CopyObjectResult.ETag == nil {
		t.Fatal("CopyObject result missing ETag")
	}
	got, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: &bucket, Key: &destination})
	if err != nil {
		t.Fatal(err)
	}
	actual, err := io.ReadAll(got.Body)
	got.Body.Close()
	if err != nil || !bytes.Equal(body, actual) || got.Metadata["owner"] != "synthetic" {
		t.Fatalf("copy bytes/metadata: %x %v %v", actual, got.Metadata, err)
	}
	if _, err := client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &bucket, Key: &source}); err != nil {
		t.Fatalf("copy removed source: %v", err)
	}
	if _, err := client.CopyObject(ctx, &s3.CopyObjectInput{Bucket: &bucket, Key: ptr("missing-copy"), CopySource: ptr(bucket + "/missing")}); err == nil {
		t.Fatal("copy of missing source succeeded")
	}
	Record(t, "aws-sdk-go-v2", "aws-s3-style", "CopyObject preserves binary bytes and user metadata; missing source refused")

	result, err := client.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: &bucket, Delete: &types.Delete{Objects: []types.ObjectIdentifier{{Key: &source}, {Key: &destination}, {Key: ptr("absent")}}}})
	if err != nil {
		t.Fatalf("DeleteObjects: %v", err)
	}
	if len(result.Deleted) != 3 || len(result.Errors) != 0 {
		t.Fatalf("DeleteObjects result: %+v", result)
	}
	for _, key := range []string{source, destination} {
		if _, err := client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &bucket, Key: &key}); err == nil {
			t.Fatalf("deleted object %q remains", key)
		}
	}
	quiet, err := client.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: &bucket, Delete: &types.Delete{Quiet: ptr(true), Objects: []types.ObjectIdentifier{{Key: ptr("absent")}}}})
	if err != nil || len(quiet.Deleted) != 0 {
		t.Fatalf("quiet DeleteObjects: %+v %v", quiet, err)
	}
	Record(t, "aws-sdk-go-v2", "aws-s3-style", "DeleteObjects existing and missing keys, XML escaping and quiet mode")
}
