package grpcsim_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

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

	srv, result, err := grpcsim.Serve(ctx, svc, lis)
	if err != nil {
		t.Fatalf("grpcsim.Serve: %v", err)
	}
	defer func() {
		srv.GracefulStop()
		_ = result.Wait()
	}()

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

	srv, result, err := grpcsim.Serve(ctx, svc, lis)
	if err != nil {
		t.Fatalf("grpcsim.Serve: %v", err)
	}
	defer func() {
		srv.GracefulStop()
		_ = result.Wait()
	}()

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

// TestServeSurfacesListenerError verifies that Serve surfaces a serve error
// (from a bad/closed listener) via the ServeResult instead of silently
// discarding it.
func TestServeSurfacesListenerError(t *testing.T) {
	fds := compileDescriptor(t)

	// Create and immediately close a listener so Serve will fail.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := lis.Addr().String()
	_ = lis.Close() // close it so Serve's Accept fails immediately

	svc := &grpcsim.Service{
		FullName:   "stunt.test.Greeter",
		Descriptor: fds,
		Methods: map[string]grpcsim.Handler{
			"SayHello": func(ctx context.Context, fullMethod string, req map[string]any) (map[string]any, error) {
				return map[string]any{"message": "ok"}, nil
			},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, result, err := grpcsim.Serve(ctx, svc, lis)
	if err != nil {
		t.Fatalf("grpcsim.Serve setup error: %v", err)
	}

	// Wait should return the serve error promptly.
	done := make(chan error, 1)
	go func() { done <- result.Wait() }()
	select {
	case serveErr := <-done:
		if serveErr == nil {
			t.Fatal("expected non-nil serve error from closed listener, got nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for serve error — Serve did not surface the listener failure")
	}

	_ = addr // suppress unused var
}

// compileDescriptorFromPath is like compileDescriptor but for an arbitrary
// proto file path.
func compileDescriptorFromPath(t *testing.T, protoFile string) *descriptorpb.FileDescriptorSet {
	t.Helper()

	descPath := filepath.Join(t.TempDir(), "out.desc")

	protoc := "/opt/homebrew/bin/protoc"
	if _, err := os.Stat(protoc); err != nil {
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
		protoFile,
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

// TestProtoToMapSnakeCase verifies that a protobuf field named echo_count
// (snake_case in the proto definition) arrives in the handler's request map
// as the key "echo_count" (not camelCase "echoCount").
func TestProtoToMapSnakeCase(t *testing.T) {
	fds := compileDescriptorFromPath(t, "echo_count.proto")

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := lis.Addr().String()

	var capturedReq map[string]any

	svc := &grpcsim.Service{
		FullName:   "stunt.test.EchoCount",
		Descriptor: fds,
		Methods: map[string]grpcsim.Handler{
			"Count": func(ctx context.Context, fullMethod string, req map[string]any) (map[string]any, error) {
				capturedReq = req
				return map[string]any{"echo_count": "reply"}, nil
			},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv, result, err := grpcsim.Serve(ctx, svc, lis)
	if err != nil {
		t.Fatalf("grpcsim.Serve: %v", err)
	}
	defer func() {
		srv.GracefulStop()
		_ = result.Wait()
	}()

	client := newEchoCountClient(t, fds, addr)
	_, err = client.invoke(ctx, "/stunt.test.EchoCount/Count", map[string]any{"echo_count": "test"})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}

	// The handler should have received the snake_case key.
	if _, ok := capturedReq["echo_count"]; !ok {
		t.Errorf("handler req map missing snake_case key %q; keys = %v", "echo_count", capturedReq)
	}
	if _, ok := capturedReq["echoCount"]; ok {
		t.Errorf("handler req map should not have camelCase key %q", "echoCount")
	}

	// The response round-trip is not asserted here because the test client
	// uses default protojson (camelCase) to parse the response. The fix is
	// specifically about the server-side request map keys, not the test
	// client's response parsing.
}

// newEchoCountClient is like newDynClient but for the EchoCount service.
func newEchoCountClient(t *testing.T, fds *descriptorpb.FileDescriptorSet, target string) *echoCountClient {
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
	return &echoCountClient{files: files, conn: conn}
}

type echoCountClient struct {
	files *protoregistry.Files
	conn  *grpc.ClientConn
}

func (c *echoCountClient) invoke(ctx context.Context, fullMethod string, reqMap map[string]any) (map[string]any, error) {
	desc, err := c.files.FindDescriptorByName(protoreflect.FullName("stunt.test.EchoCount"))
	if err != nil {
		return nil, fmt.Errorf("find EchoCount service: %w", err)
	}
	svcDesc := desc.(protoreflect.ServiceDescriptor)
	md := svcDesc.Methods().Get(0) // single method "Count"

	reqProto := dynamicpb.NewMessage(md.Input())
	reqJSON, _ := json.Marshal(reqMap)
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(reqJSON, reqProto); err != nil {
		return nil, fmt.Errorf("protojson unmarshal request: %w", err)
	}

	respProto := dynamicpb.NewMessage(md.Output())
	if err := c.conn.Invoke(ctx, fullMethod, reqProto, respProto); err != nil {
		return nil, err
	}

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

// TestMaxRecvMsgSizeExceeded verifies that a gRPC message larger than the
// configured MaxRecvMsgSize (4 MiB) is rejected by the server.
func TestMaxRecvMsgSizeExceeded(t *testing.T) {
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
				return map[string]any{"message": "ok"}, nil
			},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv, result, err := grpcsim.Serve(ctx, svc, lis)
	if err != nil {
		t.Fatalf("grpcsim.Serve: %v", err)
	}
	defer func() {
		srv.GracefulStop()
		_ = result.Wait()
	}()

	client := newDynClient(t, fds, addr)

	// Build a payload larger than 4 MiB — the server must reject it.
	bigName := strings.Repeat("A", 5*1024*1024)
	_, err = client.invoke(ctx, "/stunt.test.Greeter/SayHello", map[string]any{"name": bigName})
	if err == nil {
		t.Fatal("expected error for oversized message, got nil")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected grpc status error, got: %v", err)
	}
	if st.Code() != codes.ResourceExhausted {
		t.Errorf("expected code ResourceExhausted, got %v", st.Code())
	}
}

// TestUniqueHandlerTypePerServiceDesc verifies that BuildServiceDesc produces
// distinct HandlerType reflect.Types so that two services on the same
// grpc.Server can never collide.
func TestUniqueHandlerTypePerServiceDesc(t *testing.T) {
	fds := compileDescriptor(t)

	desc1, err := grpcsim.BuildServiceDesc(&grpcsim.Service{
		FullName:   "stunt.test.Greeter",
		Descriptor: fds,
		Methods: map[string]grpcsim.Handler{
			"SayHello": func(ctx context.Context, fullMethod string, req map[string]any) (map[string]any, error) {
				return nil, nil
			},
		},
	})
	if err != nil {
		t.Fatalf("BuildServiceDesc 1: %v", err)
	}

	desc2, err := grpcsim.BuildServiceDesc(&grpcsim.Service{
		FullName:   "stunt.test.Greeter",
		Descriptor: fds,
		Methods: map[string]grpcsim.Handler{
			"SayHello": func(ctx context.Context, fullMethod string, req map[string]any) (map[string]any, error) {
				return nil, nil
			},
		},
	})
	if err != nil {
		t.Fatalf("BuildServiceDesc 2: %v", err)
	}

	t1 := reflect.TypeOf(desc1.HandlerType)
	t2 := reflect.TypeOf(desc2.HandlerType)
	if t1 == t2 {
		t.Errorf("two ServiceDescs share the same HandlerType %v; expected distinct types", t1)
	}
}
