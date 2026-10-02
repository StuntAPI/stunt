package conformance

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	smithy "github.com/aws/smithy-go"
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

	// ===== Multipart lifecycle driven by the real SDK =====
	//
	// Every AWS SDK sends `x-id=<OperationName>` on every S3 call, so the
	// adapter's mutating routes allowlist that query parameter by VALUE. These
	// cases exist because hand-rolled HTTP tests never set `x-id`, so an
	// allowlist that omitted a real operation — as it once omitted
	// AbortMultipartUpload — shipped green. Driving the SDK is the only way
	// that class of gap is caught.
	mpuBucket := "sdk-mpu-bucket"
	if _, err := s3Client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: ptr(mpuBucket)}); err != nil {
		t.Fatalf("CreateBucket (multipart): %v", err)
	}

	created, err := s3Client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: ptr(mpuBucket),
		Key:    ptr("mpu/abort.bin"),
	})
	if err != nil {
		t.Fatalf("CreateMultipartUpload: %v", err)
	}
	if created.UploadId == nil {
		t.Fatal("CreateMultipartUpload returned no UploadId")
	}
	abortUploadID := *created.UploadId
	Record(t, "aws-sdk-go-v2", "aws-s3-style", "CreateMultipartUpload")

	if _, err := s3Client.UploadPart(ctx, &s3.UploadPartInput{
		Bucket:     ptr(mpuBucket),
		Key:        ptr("mpu/abort.bin"),
		UploadId:   ptr(abortUploadID),
		PartNumber: ptr[int32](1),
		Body:       strings.NewReader("payload"),
	}); err != nil {
		t.Fatalf("UploadPart: %v", err)
	}
	Record(t, "aws-sdk-go-v2", "aws-s3-style", "UploadPart")

	parts, err := s3Client.ListParts(ctx, &s3.ListPartsInput{
		Bucket:   ptr(mpuBucket),
		Key:      ptr("mpu/abort.bin"),
		UploadId: ptr(abortUploadID),
	})
	if err != nil {
		t.Fatalf("ListParts: %v", err)
	}
	if len(parts.Parts) != 1 {
		t.Fatalf("ListParts returned %d parts, want 1", len(parts.Parts))
	}
	Record(t, "aws-sdk-go-v2", "aws-s3-style", "ListParts")

	// An in-progress multipart upload blocks DeleteBucket, and abort is the
	// only route that releases it. If the x-id allowlist omits
	// AbortMultipartUpload the abort is refused and the bucket stays stuck.
	if _, err := s3Client.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: ptr(mpuBucket)}); err == nil {
		t.Fatal("DeleteBucket succeeded with a multipart upload in progress, want BucketNotEmpty")
	}
	Record(t, "aws-sdk-go-v2", "aws-s3-style", "DeleteBucket refused while a multipart upload is in progress")

	if _, err := s3Client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket:   ptr(mpuBucket),
		Key:      ptr("mpu/abort.bin"),
		UploadId: ptr(abortUploadID),
	}); err != nil {
		t.Fatalf("AbortMultipartUpload must release the bucket, got: %v", err)
	}
	Record(t, "aws-sdk-go-v2", "aws-s3-style", "AbortMultipartUpload releases the blocked bucket delete")

	if _, err := s3Client.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: ptr(mpuBucket)}); err != nil {
		t.Fatalf("DeleteBucket after abort: %v", err)
	}

	// The completing half of the lifecycle. The SDK's XML serializer escapes the
	// quotes around an ETag it echoes back, so an adapter reading the body
	// verbatim never matches the stored ETag and every Complete answers
	// 400 InvalidPart — which the abort-only half above would not have caught.
	compBucket := "sdk-mpu-complete"
	if _, err := s3Client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: ptr(compBucket)}); err != nil {
		t.Fatalf("CreateBucket (complete): %v", err)
	}
	created2, err := s3Client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: ptr(compBucket),
		Key:    ptr("mpu/complete.bin"),
	})
	if err != nil {
		t.Fatalf("CreateMultipartUpload (complete): %v", err)
	}
	completeUploadID := *created2.UploadId
	part, err := s3Client.UploadPart(ctx, &s3.UploadPartInput{
		Bucket:     ptr(compBucket),
		Key:        ptr("mpu/complete.bin"),
		UploadId:   ptr(completeUploadID),
		PartNumber: ptr[int32](1),
		Body:       strings.NewReader("payload"),
	})
	if err != nil {
		t.Fatalf("UploadPart (complete): %v", err)
	}
	if part.ETag == nil {
		t.Fatal("UploadPart returned no ETag")
	}
	if _, err := s3Client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:   ptr(compBucket),
		Key:      ptr("mpu/complete.bin"),
		UploadId: ptr(completeUploadID),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{
			{ETag: part.ETag, PartNumber: ptr[int32](1)},
		}},
	}); err != nil {
		t.Fatalf("CompleteMultipartUpload must accept the SDK's escaped ETag, got: %v", err)
	}
	Record(t, "aws-sdk-go-v2", "aws-s3-style", "CompleteMultipartUpload with the SDK's XML-escaped ETag")

	completedObj, err := s3Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: ptr(compBucket),
		Key:    ptr("mpu/complete.bin"),
	})
	if err != nil {
		t.Fatalf("GetObject after Complete: %v", err)
	}
	completedBody, err := io.ReadAll(completedObj.Body)
	completedObj.Body.Close()
	if err != nil {
		t.Fatalf("read completed body: %v", err)
	}
	if string(completedBody) != "payload" {
		t.Fatalf("completed object = %q, want %q", completedBody, "payload")
	}
	// Completing tears the upload down: the bucket is no longer blocked by an
	// in-progress upload, but the completed object still has to be removed
	// before it is empty enough to delete.
	if _, err := s3Client.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: ptr(compBucket)}); err == nil {
		t.Fatal("DeleteBucket succeeded with an object present, want BucketNotEmpty")
	}
	if _, err := s3Client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: ptr(compBucket),
		Key:    ptr("mpu/complete.bin"),
	}); err != nil {
		t.Fatalf("DeleteObject after Complete: %v", err)
	}
	if _, err := s3Client.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: ptr(compBucket)}); err != nil {
		t.Fatalf("DeleteBucket after removing the object: %v", err)
	}

	// The unimplemented subresources must fail loudly rather than fall through
	// to the object operation, for an SDK client as well as a raw HTTP one.
	if _, err := s3Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: ptr(bucket),
		Key:    ptr("keys/k1.txt"),
		Body:   strings.NewReader("original"),
	}); err != nil {
		t.Fatalf("setup PutObject: %v", err)
	}
	// Assert the error CODE, not merely that one occurred: a 403 or 500 would
	// otherwise satisfy "err != nil" while hiding a different defect.
	if _, err := s3Client.PutObjectTagging(ctx, &s3.PutObjectTaggingInput{
		Bucket:  ptr(bucket),
		Key:     ptr("keys/k1.txt"),
		Tagging: &types.Tagging{TagSet: []types.Tag{{Key: ptr("k"), Value: ptr("v")}}},
	}); !awsErrCodeIs(err, "NotImplemented") {
		t.Fatalf("PutObjectTagging = %v, want NotImplemented", err)
	}
	if _, err := s3Client.DeleteObjectTagging(ctx, &s3.DeleteObjectTaggingInput{
		Bucket: ptr(bucket),
		Key:    ptr("keys/k1.txt"),
	}); !awsErrCodeIs(err, "NotImplemented") {
		t.Fatalf("DeleteObjectTagging = %v, want NotImplemented", err)
	}
	tagged, err := s3Client.GetObject(ctx, &s3.GetObjectInput{Bucket: ptr(bucket), Key: ptr("keys/k1.txt")})
	if err != nil {
		t.Fatalf("GetObject after refused DeleteObjectTagging: %v", err)
	}
	gotBody, err := io.ReadAll(tagged.Body)
	tagged.Body.Close()
	if err != nil {
		t.Fatalf("read GetObject body: %v", err)
	}
	if string(gotBody) != "original" {
		t.Fatalf("object content = %q after refused DeleteObjectTagging, want %q", gotBody, "original")
	}
	Record(t, "aws-sdk-go-v2", "aws-s3-style", "Object tagging subresources refused without touching the object")

	// ===== Presigned URLs, from the real SDK presigner =====
	//
	// The high-level presigner is used because it adds X-Amz-Expires, which
	// SigV4 query auth requires. It also adds middleware-specific signed
	// headers — GetObject signs host;x-amz-checksum-mode — so the request has
	// to carry them. Signing a header you do not send is a SignatureDoesNotMatch
	// in real AWS too, which is the behaviour the tamper case below pins.
	presigner := s3.NewPresignClient(s3Client)

	// sendSignedHeaders applies the value the SDK would have sent for the
	// signed headers that are not derived from the body. Content-length and
	// content-type are left to Go, which sets them the same way the signer
	// computed them.
	sendSignedHeaders := func(req *http.Request, rawURL string) {
		u, err := url.Parse(rawURL)
		if err != nil {
			t.Fatalf("parse presigned URL: %v", err)
		}
		for _, name := range strings.Split(u.Query().Get("X-Amz-SignedHeaders"), ";") {
			switch name {
			case "x-amz-checksum-mode":
				req.Header.Set(name, "ENABLED")
			}
		}
	}

	presignBucket := "sdk-presign"
	if _, err := s3Client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: ptr(presignBucket)}); err != nil {
		t.Fatalf("CreateBucket (presign): %v", err)
	}
	if _, err := s3Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: ptr(presignBucket),
		Key:    ptr("presigned.txt"),
		Body:   strings.NewReader("presigned-body"),
	}); err != nil {
		t.Fatalf("PutObject (presign): %v", err)
	}

	doPresigned := func(method, rawURL string, body io.Reader) (int, string) {
		req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
		if err != nil {
			t.Fatalf("build presigned %s: %v", method, err)
		}
		sendSignedHeaders(req, rawURL)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("presigned %s: %v", method, err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(out)
	}

	presignedGET, err := presigner.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: ptr(presignBucket),
		Key:    ptr("presigned.txt"),
	})
	if err != nil {
		t.Fatalf("PresignGetObject: %v", err)
	}
	status, presignedBody := doPresigned("GET", presignedGET.URL, nil)
	if status != 200 || presignedBody != "presigned-body" {
		t.Fatalf("presigned GET -> %d %q, want 200 %q", status, presignedBody, "presigned-body")
	}
	Record(t, "aws-sdk-go-v2", "aws-s3-style", "Presigned GET URL (real presigner, signed headers sent)")

	// DELETE signs `host` alone, so it round-trips with no header juggling.
	// Presigned PUT is deliberately not driven here: it signs
	// `content-length;content-type`, which only the SDK's own request pipeline
	// reproduces faithfully, and a raw http.Request would fail for reasons that
	// have nothing to do with the adapter.
	presignedDEL, err := presigner.PresignDeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: ptr(presignBucket),
		Key:    ptr("presigned.txt"),
	})
	if err != nil {
		t.Fatalf("PresignDeleteObject: %v", err)
	}
	status, delBody := doPresigned("DELETE", presignedDEL.URL, nil)
	if status != 204 {
		t.Fatalf("presigned DELETE -> %d %q, want 204", status, delBody)
	}
	Record(t, "aws-sdk-go-v2", "aws-s3-style", "Presigned DELETE (real presigner)")

	// Tampering must still fail, so the successes above are not presigned auth
	// being skipped.
	tampered := strings.Replace(presignedGET.URL, "X-Amz-Signature=", "X-Amz-Signature=0", 1)
	status, tamperBody := doPresigned("GET", tampered, nil)
	if status != 403 || !strings.Contains(tamperBody, "SignatureDoesNotMatch") {
		t.Fatalf("presigned GET with a tampered signature -> %d %q, want 403 SignatureDoesNotMatch", status, tamperBody)
	}
	Record(t, "aws-sdk-go-v2", "aws-s3-style", "Presigned GET rejects a tampered signature")

	if _, err := s3Client.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: ptr(presignBucket)}); err != nil {
		t.Fatalf("DeleteBucket (presign cleanup): %v", err)
	}
}

func ptr[T any](v T) *T { return &v }

// awsErrCodeIs reports whether err is an S3 API error carrying the given
// <Code>, so conformance cases can assert the refusal reason and not just that
// something went wrong.
func awsErrCodeIs(err error, code string) bool {
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.ErrorCode() == code
}
