package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
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
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/stunt-adapters/stunt/internal/manifest"
)

// streamingProto defines a gRPC service with server-streaming,
// client-streaming, bidi-streaming, and unary methods for integration testing
// of the Starlark stream API.
const streamingProto = `syntax = "proto3";
package stunt.test;
option go_package = "github.com/stunt-adapters/stunt/internal/engine;engine";

service Streamer {
  rpc List(ListRequest) returns (stream Item);
  rpc Accumulate(stream Number) returns (Sum);
  rpc Echo(stream Msg) returns (stream Msg);
  rpc Fail(FailRequest) returns (stream Item);
  rpc Ping(PingRequest) returns (Pong);
}

message ListRequest {
  int32 count = 1;
}
message Item {
  int32 item = 1;
}
message Number {
  int32 x = 1;
}
message Sum {
  int32 total = 1;
}
message Msg {
  string v = 1;
}
message FailRequest {
  string code = 1;
}
message PingRequest {
  string msg = 1;
}
message Pong {
  string msg = 1;
}
`

const streamingAdapterYAML = `
id: streamer
name: Streamer
grpc:
  service: stunt.test.Streamer
  descriptor: schemas/streaming.desc
  methods:
    - name: List
      handler: scripts/streamer.star#on_list
    - name: Accumulate
      handler: scripts/streamer.star#on_accumulate
    - name: Echo
      handler: scripts/streamer.star#on_echo
    - name: Fail
      handler: scripts/streamer.star#on_fail
    - name: Ping
      handler: scripts/streamer.star#on_ping
`

const streamingStar = `
# Server-streaming: read one request, send N items.
def on_list(stream):
    req = stream.recv()
    count = 3
    if req != None and "count" in req:
        count = int(req["count"])
    for i in range(count):
        stream.send({"item": i})

# Client-streaming: sum all numbers, return single response.
def on_accumulate(stream):
    total = 0
    while True:
        m = stream.recv()
        if m == None:
            break
        total += int(m["x"])
    return respond(200, {"total": total})

# Bidi-streaming: echo each message.
def on_echo(stream):
    while True:
        m = stream.recv()
        if m == None:
            break
        stream.send({"v": m["v"]})

# Status test: return a non-OK trailing status.
def on_fail(stream):
    return respond(404, {"error": "not found"})

# Unary regression: must still work via the existing on_<method>(req) path.
def on_ping(req):
    return respond(200, {"msg": "pong: " + req["body"]["msg"]})
`

// setupStreamingEngine lays out a temp adapter directory with the streaming
// proto + starlark handlers, compiles the descriptor, and returns a started
// engine plus the parsed FileDescriptorSet for building dynamic clients.
func setupStreamingEngine(t *testing.T) (*Engine, *descriptorpb.FileDescriptorSet) {
	t.Helper()

	adapterDir := t.TempDir()
	writeFile(t, adapterDir, "adapter.yaml", streamingAdapterYAML)
	writeFile(t, adapterDir, "scripts/streamer.star", streamingStar)

	protoPath := filepath.Join(adapterDir, "schemas", "streaming.proto")
	writeFile(t, adapterDir, "schemas/streaming.proto", streamingProto)
	fds := compileDescriptor(t, streamingProto, protoPath)

	descData, err := proto.Marshal(fds)
	if err != nil {
		t.Fatalf("marshal fds: %v", err)
	}
	writeFile(t, adapterDir, "schemas/streaming.desc", string(descData))

	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"streamer": {Adapter: adapterDir},
		},
	}

	e, err := New(m)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	t.Cleanup(func() { e.Close() })

	ctx := context.Background()
	_, cancel, err := e.ServeForTest(ctx)
	if err != nil {
		t.Fatalf("ServeForTest: %v", err)
	}
	t.Cleanup(cancel)
	time.Sleep(30 * time.Millisecond)

	return e, fds
}

// --- dynamic streaming client helpers ---

// streamingClient wraps a gRPC connection and the Streamer service descriptor
// for driving streaming RPCs via dynamic protobuf messages.
type streamingClient struct {
	conn   *grpc.ClientConn
	method protoreflect.MethodDescriptor
}

func newStreamingClient(t *testing.T, fds *descriptorpb.FileDescriptorSet, target, methodName string) *streamingClient {
	t.Helper()
	files, err := protodesc.NewFiles(fds)
	if err != nil {
		t.Fatalf("protodesc.NewFiles: %v", err)
	}
	desc, err := files.FindDescriptorByName("stunt.test.Streamer")
	if err != nil {
		t.Fatalf("find Streamer: %v", err)
	}
	svcDesc := desc.(protoreflect.ServiceDescriptor)
	md := svcDesc.Methods().ByName(protoreflect.Name(methodName))
	if md == nil {
		t.Fatalf("method %q not found", methodName)
	}
	conn, err := grpc.NewClient(target,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return &streamingClient{conn: conn, method: md}
}

func (c *streamingClient) mapToMsg(m map[string]any) *dynamicpb.Message {
	msg := dynamicpb.NewMessage(c.method.Input())
	b, _ := json.Marshal(m)
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(b, msg); err != nil {
		panic(fmt.Sprintf("protojson unmarshal: %v", err))
	}
	return msg
}

func (c *streamingClient) newRespMsg() *dynamicpb.Message {
	return dynamicpb.NewMessage(c.method.Output())
}

func dynamicMsgToMap(t *testing.T, msg *dynamicpb.Message) map[string]any {
	t.Helper()
	b, err := protojson.Marshal(msg)
	if err != nil {
		t.Fatalf("protojson marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return m
}

// ---------------------------------------------------------------------------
// Server-streaming: client sends 1 request, receives N items.
// ---------------------------------------------------------------------------

func TestGRPCServerStreaming(t *testing.T) {
	e, fds := setupStreamingEngine(t)
	target := e.GrpcTarget("streamer")
	if target == "" {
		t.Fatal("no gRPC target")
	}

	client := newStreamingClient(t, fds, target, "List")

	stream, err := client.conn.NewStream(context.Background(), &grpc.StreamDesc{
		StreamName:    "List",
		ServerStreams: true,
	}, "/stunt.test.Streamer/List")
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}

	if err := stream.SendMsg(client.mapToMsg(map[string]any{"count": float64(3)})); err != nil {
		t.Fatalf("SendMsg: %v", err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}

	var got []int
	for {
		resp := client.newRespMsg()
		err := stream.RecvMsg(resp)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("RecvMsg: %v", err)
		}
		m := dynamicMsgToMap(t, resp)
		got = append(got, asInt(m["item"]))
	}

	want := []int{0, 1, 2}
	if len(got) != len(want) {
		t.Fatalf("received %d items, want %d", len(got), len(want))
	}
	for i, v := range got {
		if v != want[i] {
			t.Errorf("item[%d] = %d, want %d", i, v, want[i])
		}
	}
}

// ---------------------------------------------------------------------------
// Client-streaming: client sends N numbers, half-closes, receives 1 sum.
// ---------------------------------------------------------------------------

func TestGRPCClientStreaming(t *testing.T) {
	e, fds := setupStreamingEngine(t)
	target := e.GrpcTarget("streamer")

	client := newStreamingClient(t, fds, target, "Accumulate")

	stream, err := client.conn.NewStream(context.Background(), &grpc.StreamDesc{
		StreamName:    "Accumulate",
		ClientStreams: true,
	}, "/stunt.test.Streamer/Accumulate")
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}

	values := []int{10, 20, 5}
	for _, v := range values {
		if err := stream.SendMsg(client.mapToMsg(map[string]any{"x": float64(v)})); err != nil {
			t.Fatalf("SendMsg: %v", err)
		}
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}

	// Client-streaming: exactly 1 response.
	resp := client.newRespMsg()
	if err := stream.RecvMsg(resp); err != nil {
		t.Fatalf("RecvMsg: %v", err)
	}
	m := dynamicMsgToMap(t, resp)
	if got := asInt(m["total"]); got != 35 {
		t.Errorf("total = %d, want 35", got)
	}

	// Stream should now be closed (EOF on next RecvMsg).
	if err := stream.RecvMsg(resp); err != io.EOF {
		t.Errorf("expected io.EOF after response, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Bidi-streaming: client sends 3 messages, receives 3 echoes.
// ---------------------------------------------------------------------------

func TestGRPCBidiStreaming(t *testing.T) {
	e, fds := setupStreamingEngine(t)
	target := e.GrpcTarget("streamer")

	client := newStreamingClient(t, fds, target, "Echo")

	stream, err := client.conn.NewStream(context.Background(), &grpc.StreamDesc{
		StreamName:    "Echo",
		ClientStreams: true,
		ServerStreams: true,
	}, "/stunt.test.Streamer/Echo")
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}

	messages := []string{"alpha", "beta", "gamma"}
	for _, s := range messages {
		if err := stream.SendMsg(client.mapToMsg(map[string]any{"v": s})); err != nil {
			t.Fatalf("SendMsg: %v", err)
		}
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}

	var got []string
	for {
		resp := client.newRespMsg()
		err := stream.RecvMsg(resp)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("RecvMsg: %v", err)
		}
		m := dynamicMsgToMap(t, resp)
		got = append(got, m["v"].(string))
	}

	if len(got) != len(messages) {
		t.Fatalf("received %d echoes, want %d", len(got), len(messages))
	}
	for i, v := range got {
		if v != messages[i] {
			t.Errorf("echo[%d] = %q, want %q", i, v, messages[i])
		}
	}
}

// ---------------------------------------------------------------------------
// Status test: handler returns respond(404, ...) → codes.NotFound.
// ---------------------------------------------------------------------------

func TestGRPCStreamingStatusMapping(t *testing.T) {
	e, fds := setupStreamingEngine(t)
	target := e.GrpcTarget("streamer")

	client := newStreamingClient(t, fds, target, "Fail")

	stream, err := client.conn.NewStream(context.Background(), &grpc.StreamDesc{
		StreamName:    "Fail",
		ServerStreams: true,
	}, "/stunt.test.Streamer/Fail")
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}

	if err := stream.SendMsg(client.mapToMsg(map[string]any{"code": "x"})); err != nil {
		t.Fatalf("SendMsg: %v", err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}

	// The server returns a 404 trailing status, so RecvMsg should return a
	// gRPC error with codes.NotFound.
	resp := client.newRespMsg()
	err = stream.RecvMsg(resp)
	if err == nil {
		t.Fatal("expected gRPC error from Fail, got nil")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("error is not a grpc status: %v", err)
	}
	if st.Code() != codes.NotFound {
		t.Errorf("code = %v, want NotFound", st.Code())
	}
	if st.Message() != "not found" {
		t.Errorf("message = %q, want %q", st.Message(), "not found")
	}
}

// ---------------------------------------------------------------------------
// Unary regression: an existing unary method on the same service still works.
// ---------------------------------------------------------------------------

func TestGRPCStreamingUnaryRegression(t *testing.T) {
	e, fds := setupStreamingEngine(t)
	target := e.GrpcTarget("streamer")

	resp := invokeGRPC(t, fds, target, "/stunt.test.Streamer/Ping",
		map[string]any{"msg": "hello"})

	got, _ := resp["msg"].(string)
	if got != "pong: hello" {
		t.Errorf("Ping msg = %q, want %q", got, "pong: hello")
	}
}

// ---------------------------------------------------------------------------
// Context cancellation: cancelling the client ctx mid-stream causes the
// server handler to terminate promptly (via recv/send erroring out) rather
// than running to its step limit.
// ---------------------------------------------------------------------------

func TestGRPCStreamingClientCancelTerminates(t *testing.T) {
	e, fds := setupStreamingEngine(t)
	target := e.GrpcTarget("streamer")

	client := newStreamingClient(t, fds, target, "Echo")

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := client.conn.NewStream(ctx, &grpc.StreamDesc{
		StreamName:    "Echo",
		ClientStreams: true,
		ServerStreams: true,
	}, "/stunt.test.Streamer/Echo")
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}

	// Send one message and receive the echo so we know the handler is live.
	if err := stream.SendMsg(client.mapToMsg(map[string]any{"v": "before-cancel"})); err != nil {
		t.Fatalf("SendMsg: %v", err)
	}
	resp := client.newRespMsg()
	if err := stream.RecvMsg(resp); err != nil {
		t.Fatalf("RecvMsg (echo): %v", err)
	}

	// Cancel mid-stream.
	cancel()

	// After cancellation, RecvMsg should return promptly (codes.Canceled)
	// rather than hanging.
	done := make(chan error, 1)
	go func() {
		resp := client.newRespMsg()
		done <- stream.RecvMsg(resp)
	}()

	select {
	case err := <-done:
		if err == nil {
			// Some gRPC implementations return nil EOF on cancel; either way,
			// the important thing is that it returns promptly.
			return
		}
		st, ok := status.FromError(err)
		if !ok {
			// io.EOF is also acceptable.
			if err == io.EOF {
				return
			}
			t.Fatalf("error is not a grpc status: %v", err)
		}
		if st.Code() == codes.Internal {
			t.Errorf("code = Internal, want Canceled — transport error was masked as Internal")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RecvMsg did not return within 5s after cancel — handler did not terminate promptly")
	}
}
