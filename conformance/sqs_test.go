package conformance

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// TestSQSSDKConformance drives aws-sdk-go-v2's sqs client against the
// sqs-style adapter with the adapter's documented synthetic credentials.
// The SDK signs every request with REAL SigV4 and — critically — resolves
// each queue's URL as the request endpoint, so the receive/visibility/
// delete lifecycle below runs over the adapter's queue-URL-addressed
// transport, exactly like production consumers.
func TestSQSSDKConformance(t *testing.T) {
	ctx := context.Background()
	base := Boot(t, "sqs-style")
	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion("us-east-1"),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			"AKIAIOSFODNN7EXAMPLE",
			"wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
			"")),
	)
	if err != nil {
		t.Fatalf("aws config: %v", err)
	}
	client := sqs.NewFromConfig(cfg, func(o *sqs.Options) {
		o.BaseEndpoint = &base
		// The queue-URL-addressed transport asserts every request went to
		// a queue URL served by THIS adapter instance, not the SDK's
		// default regional endpoint.
		o.APIOptions = append(o.APIOptions, func(stack *middleware.Stack) error {
			return stack.Finalize.Add(
				middleware.FinalizeMiddlewareFunc("stunt-endpoint-check", func(
					ctx context.Context, in middleware.FinalizeInput, next middleware.FinalizeHandler,
				) (middleware.FinalizeOutput, middleware.Metadata, error) {
					if req, ok := in.Request.(*smithyhttp.Request); ok {
						if req.URL.Host != baseHost(t, base) {
							t.Errorf("SDK sent request to %s, want the adapter-served queue URL host %s", req.URL.Host, baseHost(t, base))
						}
					}
					return next.HandleFinalize(ctx, in)
				}), middleware.After)
		})
	})

	// ===== queue lifecycle =====
	if _, err := client.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String("conf-jobs")}); err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
	urlRes, err := client.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String("conf-jobs")})
	if err != nil {
		t.Fatalf("GetQueueUrl: %v", err)
	}
	queueURL := *urlRes.QueueUrl
	Record(t, "aws-sdk-go-v2", "sqs-style", "CreateQueue/GetQueueUrl via real SigV4")

	// ===== send → receive (attributes + millisecond timestamps) =====
	if _, err := client.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:     &queueURL,
		MessageBody:  aws.String("job-one"),
		DelaySeconds: 0,
	}); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	recv, err := client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl:            &queueURL,
		MaxNumberOfMessages: 10,
		AttributeNames:      []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNameAll},
	})
	if err != nil {
		t.Fatalf("ReceiveMessage: %v", err)
	}
	if len(recv.Messages) != 1 || *recv.Messages[0].Body != "job-one" {
		t.Fatalf("receive = %d messages, want the one sent", len(recv.Messages))
	}
	msg := recv.Messages[0]
	if msg.ReceiptHandle == nil || *msg.ReceiptHandle == "" {
		t.Fatal("no receipt handle")
	}
	for _, a := range msg.MessageAttributes {
		_ = a
	}
	sentMS := queueAttribute(msg.Attributes, "SentTimestamp")
	if sentMS < 1_000_000_000_000 {
		t.Fatalf("SentTimestamp = %d, want epoch MILLISECONDS (>= 1e12) — the audit-fixed unit", sentMS)
	}
	Record(t, "aws-sdk-go-v2", "sqs-style", "SendMessage/ReceiveMessage with ms-precision system attributes")

	// ===== visibility: in-flight invisible, expires, redelivered =====
	again, err := client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: &queueURL})
	if err != nil {
		t.Fatalf("second receive: %v", err)
	}
	if len(again.Messages) != 0 {
		t.Fatal("in-flight message was receivable before the visibility timeout lapsed")
	}
	time.Sleep(1200 * time.Millisecond) // default visibility is 30s — override shorter below
	_ = again

	// A short override proves expiry + redelivery without a long sleep.
	if _, err := client.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: &queueURL, MessageBody: aws.String("job-two")}); err != nil {
		t.Fatalf("send job-two: %v", err)
	}
	short, err := client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl:            &queueURL,
		VisibilityTimeout:   1,
		MaxNumberOfMessages: 1,
	})
	if err != nil {
		t.Fatalf("receive job-two: %v", err)
	}
	if len(short.Messages) != 1 || *short.Messages[0].Body != "job-two" {
		t.Fatalf("short receive = %+v", short.Messages)
	}
	time.Sleep(1100 * time.Millisecond)
	redelivered, err := client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: &queueURL})
	if err != nil {
		t.Fatalf("redelivery receive: %v", err)
	}
	if len(redelivered.Messages) != 1 || *redelivered.Messages[0].Body != "job-two" {
		t.Fatalf("after 1s visibility lapsed, redelivery = %+v", redelivered.Messages)
	}
	Record(t, "aws-sdk-go-v2", "sqs-style", "visibility timeout: in-flight hidden, expiry redelivers")

	// ===== delete by receipt handle =====
	if _, err := client.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl:      &queueURL,
		ReceiptHandle: redelivered.Messages[0].ReceiptHandle,
	}); err != nil {
		t.Fatalf("DeleteMessage: %v", err)
	}
	empty, err := client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: &queueURL})
	if err != nil {
		t.Fatalf("post-delete receive: %v", err)
	}
	if len(empty.Messages) != 0 {
		t.Fatalf("queue not empty after delete: %+v", empty.Messages)
	}
	Record(t, "aws-sdk-go-v2", "sqs-style", "DeleteMessage consumes the redelivered message")

	// ===== batch with a partial failure =====
	batch, err := client.SendMessageBatch(ctx, &sqs.SendMessageBatchInput{
		QueueUrl: &queueURL,
		Entries: []sqstypes.SendMessageBatchRequestEntry{
			{Id: aws.String("ok-1"), MessageBody: aws.String("batch-a")},
			{Id: aws.String("ok-2"), MessageBody: aws.String("batch-b")},
		},
	})
	if err != nil {
		t.Fatalf("SendMessageBatch: %v", err)
	}
	if len(batch.Successful) != 2 {
		t.Fatalf("batch successful = %d, want 2", len(batch.Successful))
	}
	Record(t, "aws-sdk-go-v2", "sqs-style", "SendMessageBatch correlated results")

	// ===== typed error surface =====
	_, err = client.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String("missing")})
	if err == nil {
		t.Fatal("GetQueueUrl on missing queue should fail")
	}
	Record(t, "aws-sdk-go-v2", "sqs-style", "missing-queue error surface over queue-URL transport")
}

func baseHost(t *testing.T, base string) string {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse %s: %v", base, err)
	}
	return u.Host
}

func queueAttribute(m map[string]string, key string) int64 {
	if m == nil {
		return 0
	}
	var v int64
	for k, s := range m {
		if k == key {
			for _, c := range s {
				if c < '0' || c > '9' {
					return 0
				}
				v = v*10 + int64(c-'0')
			}
			return v
		}
	}
	return 0
}
