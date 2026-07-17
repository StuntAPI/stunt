package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/stunt-adapters/stunt/internal/manifest"
	"github.com/stunt-adapters/stunt/internal/rules"
)

// greeterProto is a minimal proto for the integration test. It is compiled
// to a FileDescriptorSet via protoc at test time.
const greeterProto = `syntax = "proto3";
package stunt.test;
option go_package = "github.com/stunt-adapters/stunt/internal/engine;engine";

service Greeter {
  rpc SayHello(HelloRequest) returns (HelloReply);
}
message HelloRequest {
  string name = 1;
}
message HelloReply {
  string message = 1;
}
`

const grpcAdapterYAML = `
id: greeter
name: Greeter
endpoints:
  - route: /health
    method: GET
    handler: scripts/greeter.star#on_health
grpc:
  service: stunt.test.Greeter
  descriptor: schemas/greeter.desc
  methods:
    - name: SayHello
      handler: scripts/greeter.star#on_say_hello
`

const grpcStar = `
def on_say_hello(req):
    name = req["body"]["name"]
    return respond(200, {"message": "hello " + name})

def on_health(req):
    return respond(200, {"status": "ok"})
`

// compileDescriptor invokes protoc to produce a FileDescriptorSet for the
// greeter proto. It returns the parsed descriptor set. The test is skipped
// if protoc is unavailable.
func compileDescriptor(t *testing.T, protoSrc, protoPath string) *descriptorpb.FileDescriptorSet {
	t.Helper()

	protoc := "/opt/homebrew/bin/protoc"
	if _, err := os.Stat(protoc); err != nil {
		if p, err := exec.LookPath("protoc"); err == nil {
			protoc = p
		} else {
			t.Skipf("protoc not found: %v", err)
		}
	}

	descPath := filepath.Join(t.TempDir(), "greeter.desc")
	cmd := exec.Command(protoc,
		"--proto_path="+filepath.Dir(protoPath),
		"--include_imports",
		"--descriptor_set_out="+descPath,
		protoPath,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("protoc failed: %v\n%s", err, out)
	}

	data, err := os.ReadFile(descPath)
	if err != nil {
		t.Fatalf("read descriptor: %v", err)
	}
	fds := &descriptorpb.FileDescriptorSet{}
	if err := proto.Unmarshal(data, fds); err != nil {
		t.Fatalf("unmarshal descriptor set: %v", err)
	}
	return fds
}

// TestGRPCAdapterRoundTrip builds an engine with a gRPC-backed adapter,
// makes a real dynamic gRPC client call, and asserts the Starlark handler's
// response. Also verifies that HTTP endpoints in the same adapter still work.
func TestGRPCAdapterRoundTrip(t *testing.T) {
	// --- Lay out the adapter directory ---
	adapterDir := t.TempDir()
	writeFile(t, adapterDir, "adapter.yaml", grpcAdapterYAML)
	writeFile(t, adapterDir, "scripts/greeter.star", grpcStar)

	// Write + compile the proto to schemas/greeter.desc inside the adapter dir.
	protoPath := filepath.Join(adapterDir, "schemas", "greeter.proto")
	writeFile(t, adapterDir, "schemas/greeter.proto", greeterProto)
	fds := compileDescriptor(t, greeterProto, protoPath)

	// Write the compiled descriptor into the adapter's schemas/ dir.
	descData, err := proto.Marshal(fds)
	if err != nil {
		t.Fatalf("marshal fds: %v", err)
	}
	writeFile(t, adapterDir, "schemas/greeter.desc", string(descData))

	// --- Build the engine ---
	stateDir := t.TempDir()
	manifestPath := filepath.Join(stateDir, "stunt.yaml")
	m := &manifest.Manifest{
		Path:    manifestPath,
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"greeter": {Adapter: adapterDir},
			// Regression: an inline-rules-only service must still work.
			"hello": {Rules: []rules.Rule{
				{Match: rules.Match{Method: "GET", Path: "/ok"}, Respond: rules.Respond{Status: 200, Body: &rules.Body{Inline: map[string]any{"msg": "hi"}}}},
			}},
		},
	}

	e, err := New(m)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	defer e.Close()

	ctx := context.Background()
	addrs, cancel, err := e.ServeForTest(ctx)
	if err != nil {
		t.Fatalf("ServeForTest: %v", err)
	}
	defer cancel()
	time.Sleep(30 * time.Millisecond)

	// --- Verify HTTP still works (adapter endpoint) ---
	body, status := get2(t, addrs["greeter"]+"/health")
	if status != 200 || !strings.Contains(body, "ok") {
		t.Fatalf("GET /health -> status %d body %q, want 200 with status:ok", status, body)
	}

	// --- Verify inline-rules service (regression) ---
	body, status = get2(t, addrs["hello"]+"/ok")
	if status != 200 || body != `{"msg":"hi"}` {
		t.Fatalf("GET /ok (hello) -> status %d body %q, want 200 {\"msg\":\"hi\"}", status, body)
	}

	// --- Make a real dynamic gRPC call ---
	grpcTarget := e.GrpcTarget("greeter")
	if grpcTarget == "" {
		t.Fatal("no gRPC target for greeter service")
	}

	resp := invokeGRPC(t, fds, grpcTarget, "/stunt.test.Greeter/SayHello",
		map[string]any{"name": "stunt"})

	got, _ := resp["message"].(string)
	if got != "hello stunt" {
		t.Errorf("gRPC message = %q, want %q", got, "hello stunt")
	}
}

// TestGRPCAdapterErrorMapping verifies that a Starlark handler returning a
// non-200 status is mapped to the correct gRPC error code.
func TestGRPCAdapterErrorMapping(t *testing.T) {
	const adapterYAML = `
id: error-svc
name: Error Service
grpc:
  service: stunt.test.Greeter
  descriptor: schemas/greeter.desc
  methods:
    - name: SayHello
      handler: scripts/greeter.star#on_say_hello
`
	const starSrc = `
def on_say_hello(req):
    return respond(404, {"error": "charge not found"})
`
	adapterDir := t.TempDir()
	writeFile(t, adapterDir, "adapter.yaml", adapterYAML)
	writeFile(t, adapterDir, "scripts/greeter.star", starSrc)

	protoPath := filepath.Join(adapterDir, "schemas", "greeter.proto")
	writeFile(t, adapterDir, "schemas/greeter.proto", greeterProto)
	fds := compileDescriptor(t, greeterProto, protoPath)
	descData, _ := proto.Marshal(fds)
	writeFile(t, adapterDir, "schemas/greeter.desc", string(descData))

	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"error-svc": {Adapter: adapterDir},
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

	grpcTarget := e.GrpcTarget("error-svc")

	_, err = invokeGRPCRaw(t, fds, grpcTarget, "/stunt.test.Greeter/SayHello",
		map[string]any{"name": "x"})
	if err == nil {
		t.Fatal("expected gRPC error, got nil")
	}

	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("error is not a grpc status: %v", err)
	}
	if st.Code() != codes.NotFound {
		t.Errorf("code = %v, want NotFound", st.Code())
	}
	if st.Message() != "charge not found" {
		t.Errorf("message = %q, want %q", st.Message(), "charge not found")
	}
}

// --- dynamic gRPC client helpers (mirrors grpcsim's test client) ---

func invokeGRPC(t *testing.T, fds *descriptorpb.FileDescriptorSet, target, fullMethod string, reqMap map[string]any) map[string]any {
	t.Helper()
	resp, err := invokeGRPCRaw(t, fds, target, fullMethod, reqMap)
	if err != nil {
		t.Fatalf("invoke %s: %v", fullMethod, err)
	}
	return resp
}

func invokeGRPCRaw(t *testing.T, fds *descriptorpb.FileDescriptorSet, target, fullMethod string, reqMap map[string]any) (map[string]any, error) {
	t.Helper()

	files, err := protodesc.NewFiles(fds)
	if err != nil {
		return nil, fmt.Errorf("protodesc.NewFiles: %w", err)
	}
	conn, err := grpc.NewClient(target,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("grpc.NewClient: %w", err)
	}
	t.Cleanup(func() { conn.Close() })

	// Derive the fully-qualified service name from the full method path:
	// "/pkg.Svc/Method" → "pkg.Svc". This makes the helper generic across
	// services rather than hardcoding a single service name.
	svcFullName := protoreflect.FullName(fullMethod[1:strings.LastIndex(fullMethod, "/")])
	desc, err := files.FindDescriptorByName(svcFullName)
	if err != nil {
		return nil, fmt.Errorf("find service: %w", err)
	}
	svcDesc := desc.(protoreflect.ServiceDescriptor)

	// Find the method by matching the full path.
	var md protoreflect.MethodDescriptor
	methods := svcDesc.Methods()
	for i := 0; i < methods.Len(); i++ {
		candidate := methods.Get(i)
		if "/"+string(svcDesc.FullName())+"/"+string(candidate.Name()) == fullMethod {
			md = candidate
			break
		}
	}
	if md == nil {
		return nil, fmt.Errorf("method %q not found", fullMethod)
	}

	reqProto := dynamicpb.NewMessage(md.Input())
	reqJSON, _ := json.Marshal(reqMap)
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(reqJSON, reqProto); err != nil {
		return nil, fmt.Errorf("unmarshal request: %w", err)
	}

	respProto := dynamicpb.NewMessage(md.Output())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.Invoke(ctx, fullMethod, reqProto, respProto); err != nil {
		return nil, err
	}

	respJSON, err := protojson.Marshal(respProto)
	if err != nil {
		return nil, fmt.Errorf("marshal response: %w", err)
	}
	var respMap map[string]any
	if err := json.Unmarshal(respJSON, &respMap); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}
	return respMap, nil
}

