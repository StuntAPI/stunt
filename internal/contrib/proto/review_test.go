package proto_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stunt-adapters/stunt/internal/adapter"
	"github.com/stunt-adapters/stunt/internal/contrib/proto"
	"github.com/stunt-adapters/stunt/internal/starlark"
)

// ---------------------------------------------------------------------------
// I1: per-path cycle fix (diamond)
// ---------------------------------------------------------------------------

// diamondProto defines a diamond: TopReply has Left and Right, both of which
// contain a Shared field. Shared has a string field. With a shared seen-set,
// Shared is expanded once (on the Left path) then returns {} on the Right
// path. With a per-path seen-set, both paths expand it independently.
func diamondProto() []byte {
	return []byte(`syntax = "proto3";

package stunt.diamond;

service DiamondService {
  rpc GetTop(TopRequest) returns (TopReply);
}

message TopRequest {
  string query = 1;
}

message TopReply {
  Left left = 1;
  Right right = 2;
}

message Left {
  Shared shared = 1;
}

message Right {
  Shared shared = 1;
}

message Shared {
  string name = 1;
}
`)
}

func TestDiamondExpandsSharedOnBothPaths(t *testing.T) {
	dir := t.TempDir()

	if err := proto.ImportProto(diamondProto(), dir); err != nil {
		t.Fatalf("ImportProto: %v", err)
	}

	starPath := filepath.Join(dir, "scripts", "diamondservice.star")
	src, err := os.ReadFile(starPath)
	if err != nil {
		t.Fatalf("read star: %v", err)
	}

	// Load the generated handler into the Starlark VM and call it.
	vm, err := starlark.Load(string(src), nil)
	if err != nil {
		t.Fatalf("starlark load: %v", err)
	}
	resp, err := vm.Call("on_get_top", starlark.Request{})
	if err != nil {
		t.Fatalf("call handler: %v", err)
	}

	body, ok := resp.Body["left"].(map[string]any)
	if !ok {
		t.Fatalf("left is %T, want map", resp.Body["left"])
	}
	shared, ok := body["shared"].(map[string]any)
	if !ok {
		t.Fatalf("left.shared is %T, want map", body["shared"])
	}
	name, hasName := shared["name"]
	if !hasName {
		t.Fatalf("left.shared.name is missing — diamond expanded to {} on the Left path: body=%v", body)
	}
	if s, ok := name.(string); !ok || s == "" {
		t.Errorf("left.shared.name = %v, want a non-empty string", name)
	}

	// RIGHT path — this is the bug: Shared was already "seen" on the Left
	// path so it returned {} here.
	body2, ok := resp.Body["right"].(map[string]any)
	if !ok {
		t.Fatalf("right is %T, want map", resp.Body["right"])
	}
	shared2, ok := body2["shared"].(map[string]any)
	if !ok {
		t.Fatalf("right.shared is %T, want map — diamond bug: expanded to {} on second path: body=%v", body2["shared"], body2)
	}
	name2, hasName2 := shared2["name"]
	if !hasName2 {
		t.Fatalf("right.shared.name is missing — diamond bug: Shared collapsed to {} on the Right path")
	}
	if s, ok := name2.(string); !ok || s == "" {
		t.Errorf("right.shared.name = %v, want a non-empty string", name2)
	}
}

// ---------------------------------------------------------------------------
// I2: concrete synthetic values (not {{ faker.Word }} placeholders)
// ---------------------------------------------------------------------------

func TestGrpcHandlerReturnsConcreteValues(t *testing.T) {
	dir := t.TempDir()

	if err := proto.ImportProto(sampleProto(), dir); err != nil {
		t.Fatalf("ImportProto: %v", err)
	}

	starPath := filepath.Join(dir, "scripts", "chargeservice.star")
	src, err := os.ReadFile(starPath)
	if err != nil {
		t.Fatalf("read star: %v", err)
	}

	// The generated source must NOT contain literal {{ faker.Word }}.
	if strings.Contains(string(src), "{{ faker.Word }}") {
		t.Errorf("generated star contains literal {{ faker.Word }} placeholder — should be concrete value")
	}

	// Load and call the handler — the string field must be a real value,
	// not the literal placeholder string.
	vm, err := starlark.Load(string(src), nil)
	if err != nil {
		t.Fatalf("starlark load: %v", err)
	}
	resp, err := vm.Call("on_create_charge", starlark.Request{})
	if err != nil {
		t.Fatalf("call handler: %v", err)
	}

	id, ok := resp.Body["id"].(string)
	if !ok {
		t.Fatalf("id is %T, want string", resp.Body["id"])
	}
	if id == "{{ faker.Word }}" {
		t.Errorf("id = literal {{ faker.Word }} — handler must return concrete values")
	}
	if id == "" {
		t.Errorf("id is empty — should be a synthetic word")
	}
}

// ---------------------------------------------------------------------------
// I3: streaming RPCs generate stream-based stubs (not skipped)
// ---------------------------------------------------------------------------

func streamingProto() []byte {
	return []byte(`syntax = "proto3";

package stunt.stream;

service StreamService {
  rpc GetOne(GetReq) returns (GetReply);
  rpc StreamThings(GetReq) returns (stream Thing);
  rpc UploadThings(stream Thing) returns (GetReply);
}

message GetReq {
  string id = 1;
}

message GetReply {
  string msg = 1;
}

message Thing {
  string name = 1;
}
`)
}

func TestStreamingRPCsGenerated(t *testing.T) {
	dir := t.TempDir()

	if err := proto.ImportProto(streamingProto(), dir); err != nil {
		t.Fatalf("ImportProto: %v", err)
	}

	// adapter.yaml: all three methods should be present.
	a, err := adapter.Load(dir)
	if err != nil {
		t.Fatalf("adapter.Load: %v", err)
	}
	if a.Grpc == nil {
		t.Fatal("Grpc is nil")
	}

	var methodNames []string
	for _, m := range a.Grpc.Methods {
		methodNames = append(methodNames, m.Name)
	}

	if len(a.Grpc.Methods) != 3 {
		t.Fatalf("Methods: %d (%v), want 3 (GetOne + StreamThings + UploadThings)", len(a.Grpc.Methods), methodNames)
	}

	// .star: all three handlers present — unary uses on_<name>(req),
	// streaming uses on_<name>(stream).
	starPath := filepath.Join(dir, "scripts", "streamservice.star")
	src, err := os.ReadFile(starPath)
	if err != nil {
		t.Fatalf("read star: %v", err)
	}

	// Unary handler.
	if !strings.Contains(string(src), "def on_get_one(req):") {
		t.Error("missing on_get_one handler for unary method")
	}
	// Server-streaming handler.
	if !strings.Contains(string(src), "def on_stream_things(stream):") {
		t.Error("missing on_stream_things streaming handler")
	}
	// Client-streaming handler.
	if !strings.Contains(string(src), "def on_upload_things(stream):") {
		t.Error("missing on_upload_things streaming handler")
	}

	// Streaming handlers use the stream API.
	if !strings.Contains(string(src), "stream.recv()") {
		t.Error("streaming handlers should use stream.recv()")
	}
	if !strings.Contains(string(src), "stream.send(") {
		t.Error("streaming handlers should use stream.send()")
	 }

	// Type comments are present.
	if !strings.Contains(string(src), "server-streaming") {
		t.Error("missing server-streaming type comment")
	}
	if !strings.Contains(string(src), "client-streaming") {
		t.Error("missing client-streaming type comment")
	}
}

// ---------------------------------------------------------------------------
// M1: snake_case collision disambiguation
// ---------------------------------------------------------------------------

func collisionProto() []byte {
	return []byte(`syntax = "proto3";

package stunt.collide;

service CollideService {
  rpc CreateFoo(CreateReq) returns (CreateReply);
  rpc createFoo(CreateReq) returns (CreateReply);
}

message CreateReq {
  string id = 1;
}

message CreateReply {
  string id = 1;
}
`)
}

func TestSnakeCaseCollisionDisambiguated(t *testing.T) {
	dir := t.TempDir()

	if err := proto.ImportProto(collisionProto(), dir); err != nil {
		t.Fatalf("ImportProto: %v", err)
	}

	// Both methods must appear in adapter.yaml with distinct handler names.
	a, err := adapter.Load(dir)
	if err != nil {
		t.Fatalf("adapter.Load: %v", err)
	}
	if a.Grpc == nil {
		t.Fatal("Grpc is nil")
	}
	if len(a.Grpc.Methods) != 2 {
		t.Fatalf("Methods: %d, want 2", len(a.Grpc.Methods))
	}

	handlers := map[string]bool{}
	for _, m := range a.Grpc.Methods {
		if handlers[m.Handler] {
			t.Errorf("handler name collision: %q appears more than once", m.Handler)
		}
		handlers[m.Handler] = true
	}
	if len(handlers) != 2 {
		t.Errorf("expected 2 distinct handler names, got %d: %v", len(handlers), handlers)
	}

	// .star: both handlers must exist with distinct function names.
	starPath := filepath.Join(dir, "scripts", "collideservice.star")
	src, err := os.ReadFile(starPath)
	if err != nil {
		t.Fatalf("read star: %v", err)
	}
	count := strings.Count(string(src), "\ndef on_")
	if count != 2 {
		t.Errorf("expected 2 handler defs in star, got %d (collision may have silently overwritten one)", count)
	}
}

// ---------------------------------------------------------------------------
// M4: well-known types synthesize to sensible values
// ---------------------------------------------------------------------------

func wktProto() []byte {
	return []byte(`syntax = "proto3";

package stunt.wkt;

import "google/protobuf/timestamp.proto";
import "google/protobuf/wrappers.proto";

service WktService {
  rpc GetEvent(GetReq) returns (EventReply);
}

message GetReq {
  string id = 1;
}

message EventReply {
  string name = 1;
  google.protobuf.Timestamp created_at = 2;
  google.protobuf.StringValue label = 3;
}
`)
}

func TestWellKnownTypesSynthesize(t *testing.T) {
	dir := t.TempDir()

	if err := proto.ImportProto(wktProto(), dir); err != nil {
		t.Fatalf("ImportProto: %v", err)
	}

	starPath := filepath.Join(dir, "scripts", "wktservice.star")
	src, err := os.ReadFile(starPath)
	if err != nil {
		t.Fatalf("read star: %v", err)
	}

	// Load and call the handler.
	vm, err := starlark.Load(string(src), nil)
	if err != nil {
		t.Fatalf("starlark load: %v", err)
	}
	resp, err := vm.Call("on_get_event", starlark.Request{})
	if err != nil {
		t.Fatalf("call handler: %v", err)
	}

	// Timestamp -> {"seconds": <int>, "nanos": 0}
	ts, ok := resp.Body["created_at"].(map[string]any)
	if !ok {
		t.Fatalf("created_at is %T, want map — well-known type Timestamp should synthesize to a dict, not {}", resp.Body["created_at"])
	}
	if _, hasSeconds := ts["seconds"]; !hasSeconds {
		t.Errorf("created_at missing 'seconds' field: %v", ts)
	}
	if nanos, hasNanos := ts["nanos"]; !hasNanos {
		t.Errorf("created_at missing 'nanos' field: %v", ts)
	} else if nanos != int64(0) {
		t.Errorf("created_at nanos = %v, want 0", nanos)
	}

	// StringValue wrapper -> a concrete string, not {} or {{ faker.Word }}.
	label, ok := resp.Body["label"].(string)
	if !ok {
		t.Fatalf("label is %T, want string — well-known type StringValue should synthesize to its inner type", resp.Body["label"])
	}
	if label == "" || label == "{{ faker.Word }}" {
		t.Errorf("label = %q, want a concrete synthetic string", label)
	}
}
