package engine

import (
	"context"
	"io"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/stunt-adapters/stunt/internal/manifest"
)

// TestEchoStreamingRPC loads the committed echo-style adapter, calls the
// StreamEcho server-streaming RPC via a real dynamic streaming client, and
// asserts it receives the expected number of synthetic replies with the
// echoed message.
func TestEchoStreamingRPC(t *testing.T) {
	absDir, err := filepath.Abs(echoAdapterDir)
	if err != nil {
		t.Fatalf("resolve adapter dir: %v", err)
	}
	fds := loadEchoDescriptor(t)

	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"echo": {Adapter: absDir},
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

	target := e.GrpcTarget("echo")
	if target == "" {
		t.Fatal("no gRPC target for echo service")
	}

	// Build a dynamic streaming client for StreamEcho.
	files, err := protodesc.NewFiles(fds)
	if err != nil {
		t.Fatalf("protodesc.NewFiles: %v", err)
	}
	desc, err := files.FindDescriptorByName("stunt.example.Echo")
	if err != nil {
		t.Fatalf("find Echo service: %v", err)
	}
	svcDesc := desc.(protoreflect.ServiceDescriptor)
	md := svcDesc.Methods().ByName("StreamEcho")
	if md == nil {
		t.Fatal("StreamEcho method not found")
	}

	conn, err := grpc.NewClient(target,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	defer conn.Close()

	stream, err := conn.NewStream(context.Background(), &grpc.StreamDesc{
		StreamName:    "StreamEcho",
		ServerStreams: true,
	}, "/stunt.example.Echo/StreamEcho")
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}

	// Send the request.
	reqMsg := dynamicpb.NewMessage(md.Input())
	reqJSON := []byte(`{"message":"stream-test"}`)
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(reqJSON, reqMsg); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	if err := stream.SendMsg(reqMsg); err != nil {
		t.Fatalf("SendMsg: %v", err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}

	// Receive 3 synthetic replies.
	const wantCount = 3
	var gotMessages []string
	for {
		resp := dynamicpb.NewMessage(md.Output())
		err := stream.RecvMsg(resp)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("RecvMsg: %v", err)
		}
		m := dynamicMsgToMap(t, resp)
		msg, _ := m["message"].(string)
		gotMessages = append(gotMessages, msg)
	}

	if len(gotMessages) != wantCount {
		t.Fatalf("received %d replies, want %d", len(gotMessages), wantCount)
	}
	for i, msg := range gotMessages {
		if msg != "stream-test" {
			t.Errorf("reply[%d] message = %q, want %q", i, msg, "stream-test")
		}
	}
}
