package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"

	"stuntapi.com/stunt/internal/contrib/lint"
	protoimport "stuntapi.com/stunt/internal/contrib/proto"
	"stuntapi.com/stunt/internal/manifest"
)

// demoProto is a sample .proto with one RPC: Echo(EchoRequest) -> EchoReply.
// It exercises the full import → serve → gRPC round-trip path.
const demoProto = `syntax = "proto3";

package stunt.demo;

service Demo {
  rpc Echo(EchoRequest) returns (EchoReply);
}

message EchoRequest {
  string name = 1;
}

message EchoReply {
  string message = 1;
}
`

// TestProtoImportEndToEnd verifies the full pipeline:
//  1. ImportProto generates adapter artifacts from a .proto source.
//  2. The engine loads and serves the adapter.
//  3. A real dynamic gRPC client call gets a synthetic response back.
//  4. lint is clean on the generated adapter.
func TestProtoImportEndToEnd(t *testing.T) {
	// --- 1. Import the proto into a temp adapter dir ---
	adapterDir := t.TempDir()
	if err := protoimport.ImportProto([]byte(demoProto), adapterDir); err != nil {
		t.Fatalf("ImportProto: %v", err)
	}

	// Sanity: descriptor and star files exist.
	descPath := filepath.Join(adapterDir, "schemas", "demo.desc")
	if _, err := os.Stat(descPath); err != nil {
		t.Fatalf("descriptor not generated: %v", err)
	}
	starPath := filepath.Join(adapterDir, "scripts", "demo.star")
	if _, err := os.Stat(starPath); err != nil {
		t.Fatalf("handler script not generated: %v", err)
	}

	// --- 2. Load the compiled descriptor for the dynamic client ---
	descData, err := os.ReadFile(descPath)
	if err != nil {
		t.Fatalf("read descriptor: %v", err)
	}
	fds := &descriptorpb.FileDescriptorSet{}
	if err := proto.Unmarshal(descData, fds); err != nil {
		t.Fatalf("unmarshal descriptor: %v", err)
	}

	// --- 3. Build the engine ---
	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"demo": {Adapter: adapterDir},
		},
	}

	e, err := New(m)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	defer e.Close()

	ctx := context.Background()
	_, cancel, err := e.ServeForTest(ctx)
	if err != nil {
		t.Fatalf("ServeForTest: %v", err)
	}
	defer cancel()
	time.Sleep(30 * time.Millisecond)

	// --- 4. Make a real dynamic gRPC call ---
	grpcTarget := e.GrpcTarget("demo")
	if grpcTarget == "" {
		t.Fatal("no gRPC target for demo service")
	}

	resp := invokeGRPC(t, fds, grpcTarget, "/stunt.demo.Demo/Echo",
		map[string]any{"name": "world"})

	// The synthetic handler returns a response with a "message" field
	// (faker placeholder for string fields). Just assert the field is
	// present and non-empty — we don't assert the exact synthetic value.
	msg, ok := resp["message"]
	if !ok {
		t.Fatalf("response missing 'message' field; got %v", resp)
	}
	msgStr, ok := msg.(string)
	if !ok || msgStr == "" {
		t.Errorf("message = %v (type %T), want non-empty string", msg, msg)
	}

	// --- 5. Assert lint is clean ---
	findings, err := lint.Lint(adapterDir)
	if err != nil {
		t.Fatalf("lint: %v", err)
	}
	if code := lint.ExitCode(findings); code != 0 {
		for _, f := range findings {
			t.Errorf("lint finding: %s:%d %s — %s", f.File, f.Line, f.Severity, f.Message)
		}
	}
}
