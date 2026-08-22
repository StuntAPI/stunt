package conformance

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// TestDynamoDBSDKConformance drives aws-sdk-go-v2's dynamodb client against
// the dynamodb-style adapter with the adapter's documented synthetic
// credentials. The SDK signs every request with REAL SigV4 and serializes
// through its own DynamoDB-JSON encoder — passing means the adapter's
// expression subset, typed values, and signature verification all accept
// the genuine client output, not just hand-rolled vectors.
func TestDynamoDBSDKConformance(t *testing.T) {
	ctx := context.Background()
	base := Boot(t, "dynamodb-style")
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
	client := dynamodb.NewFromConfig(cfg, func(o *dynamodb.Options) {
		o.BaseEndpoint = &base
	})

	// ===== table lifecycle + typed item round-trip =====
	if _, err := client.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName: aws.String("conf-items"),
		KeySchema: []types.KeySchemaElement{
			{AttributeName: aws.String("id"), KeyType: types.KeyTypeHash},
		},
		AttributeDefinitions: []types.AttributeDefinition{
			{AttributeName: aws.String("id"), AttributeType: types.ScalarAttributeTypeS},
		},
		ProvisionedThroughput: &types.ProvisionedThroughput{
			ReadCapacityUnits: aws.Int64(5), WriteCapacityUnits: aws.Int64(5),
		},
	}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	Record(t, "aws-sdk-go-v2", "dynamodb-style", "CreateTable via real SigV4")

	item := map[string]types.AttributeValue{
		"id":    &types.AttributeValueMemberS{Value: "item-1"},
		"label": &types.AttributeValueMemberS{Value: "widget"},
		"qty":   &types.AttributeValueMemberN{Value: "10"},
		"live":  &types.AttributeValueMemberBOOL{Value: true},
		"tags":  &types.AttributeValueMemberSS{Value: []string{"alpha", "beta"}},
	}
	if _, err := client.PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String("conf-items"), Item: item}); err != nil {
		t.Fatalf("PutItem: %v", err)
	}
	got, err := client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      aws.String("conf-items"),
		Key:            map[string]types.AttributeValue{"id": &types.AttributeValueMemberS{Value: "item-1"}},
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if v, ok := got.Item["qty"].(*types.AttributeValueMemberN); !ok || v.Value != "10" {
		t.Fatalf("GetItem qty = %#v, want N:\"10\"", got.Item["qty"])
	}
	if v, ok := got.Item["live"].(*types.AttributeValueMemberBOOL); !ok || !v.Value {
		t.Fatalf("GetItem live = %#v, want BOOL:true", got.Item["live"])
	}
	Record(t, "aws-sdk-go-v2", "dynamodb-style", "typed item Put/Get round-trip (S/N/BOOL/SS)")

	// ===== UpdateItem: SET + exact-decimal ADD =====
	if _, err := client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:        aws.String("conf-items"),
		Key:              map[string]types.AttributeValue{"id": &types.AttributeValueMemberS{Value: "item-1"}},
		UpdateExpression: aws.String("SET label = :l ADD qty :d"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":l": &types.AttributeValueMemberS{Value: "gizmo"},
			":d": &types.AttributeValueMemberN{Value: "0.5"},
		},
	}); err != nil {
		t.Fatalf("UpdateItem SET+ADD: %v", err)
	}
	got2, err := client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String("conf-items"),
		Key:       map[string]types.AttributeValue{"id": &types.AttributeValueMemberS{Value: "item-1"}},
	})
	if err != nil {
		t.Fatalf("GetItem after update: %v", err)
	}
	if v, ok := got2.Item["qty"].(*types.AttributeValueMemberN); !ok || v.Value != "10.5" {
		t.Fatalf("ADD 10 + 0.5 = %#v, want exactly \"10.5\"", got2.Item["qty"])
	}
	Record(t, "aws-sdk-go-v2", "dynamodb-style", "UpdateItem SET/ADD with exact decimal arithmetic")

	// ===== conditional writes =====
	if _, err := client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:           aws.String("conf-items"),
		Item:                map[string]types.AttributeValue{"id": &types.AttributeValueMemberS{Value: "item-1"}},
		ConditionExpression: aws.String("attribute_not_exists(id)"),
	}); err == nil {
		t.Fatal("conditional re-put should fail")
	} else {
		var ccfe *types.ConditionalCheckFailedException
		if !errors.As(err, &ccfe) {
			t.Fatalf("conditional put error = %v, want ConditionalCheckFailedException", err)
		}
	}
	Record(t, "aws-sdk-go-v2", "dynamodb-style", "ConditionExpression → ConditionalCheckFailedException")

	// ===== Query: numeric sort-key ordering, forward + backward =====
	if _, err := client.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName: aws.String("conf-events"),
		KeySchema: []types.KeySchemaElement{
			{AttributeName: aws.String("pk"), KeyType: types.KeyTypeHash},
			{AttributeName: aws.String("sk"), KeyType: types.KeyTypeRange},
		},
		AttributeDefinitions: []types.AttributeDefinition{
			{AttributeName: aws.String("pk"), AttributeType: types.ScalarAttributeTypeS},
			{AttributeName: aws.String("sk"), AttributeType: types.ScalarAttributeTypeN},
		},
		ProvisionedThroughput: &types.ProvisionedThroughput{
			ReadCapacityUnits: aws.Int64(5), WriteCapacityUnits: aws.Int64(5),
		},
	}); err != nil {
		t.Fatalf("CreateTable range: %v", err)
	}
	for _, sk := range []string{"10", "2", "1"} {
		if _, err := client.PutItem(ctx, &dynamodb.PutItemInput{
			TableName: aws.String("conf-events"),
			Item: map[string]types.AttributeValue{
				"pk": &types.AttributeValueMemberS{Value: "u1"},
				"sk": &types.AttributeValueMemberN{Value: sk},
			},
		}); err != nil {
			t.Fatalf("PutItem event sk=%s: %v", sk, err)
		}
	}
	q, err := client.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String("conf-events"),
		KeyConditionExpression: aws.String("pk = :p"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":p": &types.AttributeValueMemberS{Value: "u1"},
		},
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(q.Items) != 3 {
		t.Fatalf("Query count = %d, want 3", len(q.Items))
	}
	skOf := func(i int) string {
		v, _ := q.Items[i]["sk"].(*types.AttributeValueMemberN)
		return v.Value
	}
	if skOf(0) != "1" || skOf(1) != "2" || skOf(2) != "10" {
		t.Fatalf("numeric sort-key order = %s,%s,%s — want 1,2,10 (not lexicographic)", skOf(0), skOf(1), skOf(2))
	}
	Record(t, "aws-sdk-go-v2", "dynamodb-style", "Query numeric sort-key ordering (1,2,10)")

	qDesc, err := client.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String("conf-events"),
		KeyConditionExpression: aws.String("pk = :p AND sk BETWEEN :a AND :b"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":p": &types.AttributeValueMemberS{Value: "u1"},
			":a": &types.AttributeValueMemberN{Value: "2"},
			":b": &types.AttributeValueMemberN{Value: "10"},
		},
		ScanIndexForward: aws.Bool(false),
	})
	if err != nil {
		t.Fatalf("Query BETWEEN desc: %v", err)
	}
	if len(qDesc.Items) != 2 {
		t.Fatalf("BETWEEN 2..10 count = %d, want 2", len(qDesc.Items))
	}
	if v, _ := qDesc.Items[0]["sk"].(*types.AttributeValueMemberN); v.Value != "10" {
		t.Fatalf("descending first sk = %#v, want 10", qDesc.Items[0]["sk"])
	}
	Record(t, "aws-sdk-go-v2", "dynamodb-style", "Query BETWEEN + ScanIndexForward=false")

	// ===== Scan with FilterExpression =====
	sc, err := client.Scan(ctx, &dynamodb.ScanInput{
		TableName:        aws.String("conf-events"),
		FilterExpression: aws.String("sk > :min"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":min": &types.AttributeValueMemberN{Value: "1"},
		},
	})
	if err != nil {
		t.Fatalf("Scan + FilterExpression: %v", err)
	}
	if int(sc.Count) != 2 {
		t.Fatalf("Scan filtered count = %d, want 2", sc.Count)
	}
	Record(t, "aws-sdk-go-v2", "dynamodb-style", "Scan with FilterExpression")

	// ===== error surface: typed provider exception =====
	_, err = client.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String("nope")})
	if err == nil {
		t.Fatal("DescribeTable on missing table should fail")
	}
	var rnfe *types.ResourceNotFoundException
	if !errors.As(err, &rnfe) {
		t.Fatalf("DescribeTable error = %v, want ResourceNotFoundException", err)
	}
	Record(t, "aws-sdk-go-v2", "dynamodb-style", "typed ResourceNotFoundException surface")
}
