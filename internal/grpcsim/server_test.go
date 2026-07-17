package grpcsim_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/stunt-adapters/stunt/internal/grpcsim"
)

// compileDescriptor invokes protoc to produce a FileDescriptorSet for the
// greeter.proto fixture. It returns the path to the generated .desc file.
// The test fails (skips) if protoc is unavailable.
func compileDescriptor(t *testing.T) *descriptorpb.FileDescriptorSet {
	t.Helper()

	protoPath := filepath.Join("testdata", "greeter.proto")
	descPath := filepath.Join(t.TempDir(), "greeter.desc")

	protoc := "/opt/homebrew/bin/protoc"
	if _, err := os.Stat(protoc); err != nil {
		// Try PATH as a fallback.
		if p, err := exec.LookPath("protoc"); err == nil {
			protoc = p
		} else {
			t.Skipf("protoc not found: %v", err)
		}
	}

	cmd := exec.Command(protoc,
		"--proto_path=testdata",
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

// dynClient builds a dynamic gRPC client from a FileDescriptorSet, invokes the
// given method with the request map, and returns the response map. This is a
// REAL gRPC round-trip — no compiled stubs involved.
type dynClient struct {
	files *protoregistry.Files
	conn  *grpc.ClientConn
}

func newDynClient(t *testing.T, fds *descriptorpb.FileDescriptorSet, target string) *dynClient {
	t.Helper()
	files, err := protodesc.NewFiles(fds)
	if err != nil {
		t.Fatalf("protodesc.NewFiles: %v", err)
	}
	conn, err := grpc.NewClient(target,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return &dynClient{files: files, conn: conn}
}

func (c *dynClient) invoke(ctx context.Context, fullMethod string, reqMap map[string]any) (map[string]any, error) {
	desc, err := c.files.FindDescriptorByName(protoreflect.FullName("stunt.test.Greeter"))
	if err != nil {
		return nil, fmt.Errorf("find Greeter service: %w", err)
	}
	svcDesc := desc.(protoreflect.ServiceDescriptor)
	methods := svcDesc.Methods()

	// Extract the bare method name from "/pkg.Svc/Method".
	var methodName protoreflect.Name
	// fullMethod = "/stunt.test.Greeter/SayHello"
	for i := 0; i < methods.Len(); i++ {
		md := methods.Get(i)
		if "/"+string(svcDesc.FullName())+"/"+string(md.Name()) == fullMethod {
			methodName = md.Name()
			break
		}
	}
	if methodName == "" {
		return nil, fmt.Errorf("method %q not found", fullMethod)
	}
	md := svcDesc.Methods().ByName(methodName)

	reqProto := dynamicpb.NewMessage(md.Input())

	// request map -> JSON bytes -> protojson -> dynamic proto message
	reqJSON, err := json.Marshal(reqMap)
	if err != nil {
		return nil, fmt.Errorf("marshal request map: %w", err)
	}
	if err := (protojson.UnmarshalOptions{
		DiscardUnknown: true,
	}).Unmarshal(reqJSON, reqProto); err != nil {
		return nil, fmt.Errorf("protojson unmarshal request: %w", err)
	}

	respProto := dynamicpb.NewMessage(md.Output())

	err = c.conn.Invoke(ctx, fullMethod, reqProto, respProto)
	if err != nil {
		return nil, err
	}

	// response proto message -> protojson -> JSON -> response map
	respJSON, err := protojson.Marshal(respProto)
	if err != nil {
		return nil, fmt.Errorf("protojson marshal response: %w", err)
	}
	var respMap map[string]any
	if err := json.Unmarshal(respJSON, &respMap); err != nil {
		return nil, fmt.Errorf("unmarshal response map: %w", err)
	}
	return respMap, nil
}

func TestSayHelloRoundTrip(t *testing.T) {
	fds := compileDescriptor(t)

	// Pick a free TCP port (host-safe).
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := lis.Addr().String()

	svc := &grpcsim.Service{
		FullName:   "stunt.test.Greeter",
		Descriptor: fds,
		Methods: map[string]grpcsim.Handler{
			"SayHello": func(ctx context.Context, fullMethod string, req map[string]any) (map[string]any, error) {
				name, _ := req["name"].(string)
				return map[string]any{"message": "hello " + name}, nil
			},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv, err := grpcsim.Serve(ctx, svc, lis)
	if err != nil {
		t.Fatalf("grpcsim.Serve: %v", err)
	}
	defer srv.GracefulStop()

	client := newDynClient(t, fds, addr)
	resp, err := client.invoke(ctx, "/stunt.test.Greeter/SayHello", map[string]any{"name": "stunt"})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}

	got, _ := resp["message"].(string)
	if got != "hello stunt" {
		t.Errorf("message = %q, want %q", got, "hello stunt")
	}
}

func TestErrorMapping(t *testing.T) {
	fds := compileDescriptor(t)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := lis.Addr().String()

	svc := &grpcsim.Service{
		FullName:   "stunt.test.Greeter",
		Descriptor: fds,
		Methods: map[string]grpcsim.Handler{
			"SayHello": func(ctx context.Context, fullMethod string, req map[string]any) (map[string]any, error) {
				return nil, grpcsim.Error(codes.NotFound, "charge not found")
			},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv, err := grpcsim.Serve(ctx, svc, lis)
	if err != nil {
		t.Fatalf("grpcsim.Serve: %v", err)
	}
	defer srv.GracefulStop()

	client := newDynClient(t, fds, addr)
	_, err = client.invoke(ctx, "/stunt.test.Greeter/SayHello", map[string]any{"name": "x"})
	if err == nil {
		t.Fatal("expected error, got nil")
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

func TestServeRejectsUnknownService(t *testing.T) {
	fds := compileDescriptor(t)

	_, err := grpcsim.BuildServiceDesc(&grpcsim.Service{
		FullName:   "stunt.test.Nonexistent",
		Descriptor: fds,
		Methods:    map[string]grpcsim.Handler{},
	})
	if err == nil {
		t.Fatal("expected error for unknown service, got nil")
	}
}
