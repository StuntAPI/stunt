package proto_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stunt-adapters/stunt/internal/adapter"
	"github.com/stunt-adapters/stunt/internal/contrib/lint"
	"github.com/stunt-adapters/stunt/internal/contrib/proto"

	protoimport "google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

// sampleProto is a proto file with two RPCs, a nested message, a repeated
// field, an enum, and a map field — exercising the synthetic-value generator.
func sampleProto() []byte {
	return []byte(`syntax = "proto3";

package stunt.example;

// ChargeService manages payment charges.
service ChargeService {
  rpc CreateCharge(CreateChargeRequest) returns (CreateChargeReply);
  rpc GetCharge(GetChargeRequest) returns (GetChargeReply);
}

message CreateChargeRequest {
  string description = 1;
  int32 amount = 2;
}

message CreateChargeReply {
  string id = 1;
  int32 amount = 2;
  string status = 3;
  Address address = 4;
  repeated string tags = 5;
  ChargeStatus charge_status = 6;
  map<string, string> metadata = 7;
}

message GetChargeRequest {
  string id = 1;
}

message GetChargeReply {
  string id = 1;
  int64 amount = 2;
  bool captured = 3;
  bytes receipt = 4;
}

enum ChargeStatus {
  CHARGE_STATUS_UNKNOWN = 0;
  CHARGE_STATUS_PENDING = 1;
  CHARGE_STATUS_SUCCEEDED = 2;
}

message Address {
  string line1 = 1;
  string city = 2;
}
`)
}

func TestImportProtoCreatesDescriptor(t *testing.T) {
	dir := t.TempDir()

	if err := proto.ImportProto(sampleProto(), dir); err != nil {
		t.Fatalf("ImportProto: %v", err)
	}

	// .desc file exists.
	descPath := filepath.Join(dir, "schemas", "chargeservice.desc")
	data, err := os.ReadFile(descPath)
	if err != nil {
		t.Fatalf("expected descriptor at %s: %v", descPath, err)
	}

	// Valid FileDescriptorSet — parse it back with protodesc.
	fds := &descriptorpb.FileDescriptorSet{}
	if err := protoimport.Unmarshal(data, fds); err != nil {
		t.Fatalf("unmarshal FileDescriptorSet: %v", err)
	}

	files, err := protodesc.NewFiles(fds)
	if err != nil {
		t.Fatalf("protodesc.NewFiles: %v", err)
	}

	// The service must be findable by its full name.
	desc, err := files.FindDescriptorByName("stunt.example.ChargeService")
	if err != nil {
		t.Fatalf("service not found in descriptor: %v", err)
	}
	svc, ok := desc.(protoreflect.ServiceDescriptor)
	if !ok {
		t.Fatalf("expected ServiceDescriptor, got %T", desc)
	}
	if svc.Methods().Len() != 2 {
		t.Errorf("methods: %d, want 2", svc.Methods().Len())
	}
}

func TestImportProtoGrpcSection(t *testing.T) {
	dir := t.TempDir()

	if err := proto.ImportProto(sampleProto(), dir); err != nil {
		t.Fatalf("ImportProto: %v", err)
	}

	a, err := adapter.Load(dir)
	if err != nil {
		t.Fatalf("adapter.Load: %v", err)
	}

	if a.Grpc == nil {
		t.Fatal("Grpc is nil")
	}
	if a.Grpc.Service != "stunt.example.ChargeService" {
		t.Errorf("Service = %q, want stunt.example.ChargeService", a.Grpc.Service)
	}
	if !strings.HasSuffix(a.Grpc.Descriptor, "schemas/chargeservice.desc") {
		t.Errorf("Descriptor = %q, want suffix schemas/chargeservice.desc", a.Grpc.Descriptor)
	}
	if len(a.Grpc.Methods) != 2 {
		t.Fatalf("Methods: %d, want 2", len(a.Grpc.Methods))
	}

	m0 := a.Grpc.Methods[0]
	if m0.Name != "CreateCharge" {
		t.Errorf("method[0] name = %q, want CreateCharge", m0.Name)
	}
	if !strings.HasSuffix(m0.Handler, "scripts/chargeservice.star#on_create_charge") {
		t.Errorf("method[0] handler = %q", m0.Handler)
	}

	m1 := a.Grpc.Methods[1]
	if m1.Name != "GetCharge" {
		t.Errorf("method[1] name = %q, want GetCharge", m1.Name)
	}
	if !strings.HasSuffix(m1.Handler, "scripts/chargeservice.star#on_get_charge") {
		t.Errorf("method[1] handler = %q", m1.Handler)
	}
}

func TestImportProtoStarHandlers(t *testing.T) {
	dir := t.TempDir()

	if err := proto.ImportProto(sampleProto(), dir); err != nil {
		t.Fatalf("ImportProto: %v", err)
	}

	starPath := filepath.Join(dir, "scripts", "chargeservice.star")
	data, err := os.ReadFile(starPath)
	if err != nil {
		t.Fatalf("expected handler script: %v", err)
	}
	src := string(data)

	// Both handlers present.
	if !strings.Contains(src, "def on_create_charge(req):") {
		t.Error("missing on_create_charge handler")
	}
	if !strings.Contains(src, "def on_get_charge(req):") {
		t.Error("missing on_get_charge handler")
	}

	// respond(200, ...) in both.
	if strings.Count(src, "respond(200,") != 2 {
		t.Errorf("expected 2 respond(200, ...) calls, got %d", strings.Count(src, "respond(200,"))
	}

	// String field -> concrete synthetic word (NOT a {{ faker.Word }} placeholder).
	if strings.Contains(src, "{{ faker.Word }}") {
		t.Error("handler should contain concrete values, not {{ faker.Word }} placeholders")
	}

	// Repeated field -> list.
	if !strings.Contains(src, "[") {
		t.Error("handler should contain a list for repeated fields")
	}

	// Enum field -> first enum value name.
	if !strings.Contains(src, "CHARGE_STATUS_UNKNOWN") {
		t.Error("handler should contain CHARGE_STATUS_UNKNOWN for enum field")
	}

	// Nested message field -> nested dict (Address has line1).
	if !strings.Contains(src, "line1") {
		t.Error("handler should contain nested Address.line1 field")
	}
}

func TestImportProtoLintClean(t *testing.T) {
	dir := t.TempDir()

	if err := proto.ImportProto(sampleProto(), dir); err != nil {
		t.Fatalf("ImportProto: %v", err)
	}

	findings, err := lint.Lint(dir)
	if err != nil {
		t.Fatalf("lint.Lint: %v", err)
	}
	if code := lint.ExitCode(findings); code != 0 {
		for _, f := range findings {
			t.Errorf("lint finding: %s:%d %s — %s", f.File, f.Line, f.Severity, f.Message)
		}
	}
}

func TestImportProtoMalformedError(t *testing.T) {
	dir := t.TempDir()

	bad := []byte("syntax = this is not valid proto {{{")
	if err := proto.ImportProto(bad, dir); err == nil {
		t.Fatal("expected error for malformed proto, got nil")
	}
}

func TestImportProtoNoServiceError(t *testing.T) {
	dir := t.TempDir()

	noSvc := []byte(`syntax = "proto3";
package stunt.example;
message Foo { string bar = 1; }
`)
	if err := proto.ImportProto(noSvc, dir); err == nil {
		t.Fatal("expected error for proto with no service, got nil")
	}
}

// TestImportProtoPreservesExistingAdapter verifies that importing into a dir
// that already has an adapter.yaml preserves existing endpoints and adds the
// grpc section.
func TestImportProtoPreservesExistingAdapter(t *testing.T) {
	dir := t.TempDir()

	// Write a minimal existing adapter.yaml with an endpoint.
	existing := `id: my-api
name: My API
version: "1.0.0"
endpoints:
  - route: /health
    method: GET
    rules:
      - name: health-ok
        match: { method: GET, path: /health }
        respond:
          status: 200
          body:
            inline:
              status: ok
`
	if err := os.WriteFile(filepath.Join(dir, "adapter.yaml"), []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := proto.ImportProto(sampleProto(), dir); err != nil {
		t.Fatalf("ImportProto: %v", err)
	}

	a, err := adapter.Load(dir)
	if err != nil {
		t.Fatalf("adapter.Load: %v", err)
	}

	// Existing endpoint preserved.
	if len(a.Endpoints) != 1 {
		t.Fatalf("Endpoints: %d, want 1", len(a.Endpoints))
	}
	if a.Endpoints[0].Route != "/health" {
		t.Errorf("endpoint route = %q, want /health", a.Endpoints[0].Route)
	}

	// grpc section added.
	if a.Grpc == nil {
		t.Fatal("Grpc is nil")
	}
	if a.Grpc.Service != "stunt.example.ChargeService" {
		t.Errorf("Service = %q", a.Grpc.Service)
	}

	// Original ID preserved.
	if a.ID != "my-api" {
		t.Errorf("ID = %q, want my-api", a.ID)
	}
}
