package grpcsim_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"

	"stuntapi.com/stunt/internal/grpcsim"
)

// streamDynClient wraps a gRPC client connection and a protobuf file registry
// for driving streaming RPCs against the dynamic server using dynamic messages.
type streamDynClient struct {
	conn  *grpc.ClientConn
	files *protoregistry.Files
}

func newStreamDynClient(t *testing.T, fdsProto string, target string) (*streamDynClient, protoreflect.ServiceDescriptor) {
	t.Helper()
	fds := compileDescriptorFromPath(t, fdsProto)
	files, err := protodesc.NewFiles(fds)
	if err != nil {
		t.Fatalf("protodesc.NewFiles: %v", err)
	}
	desc, err := files.FindDescriptorByName("stunt.test.Streamer")
	if err != nil {
		t.Fatalf("find Streamer: %v", err)
	}
	svcDesc := desc.(protoreflect.ServiceDescriptor)
	conn, err := grpc.NewClient(target,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return &streamDynClient{conn: conn, files: files}, svcDesc
}

// mapToDynamic converts a Go map to a dynamic protobuf message for the given
// message descriptor.
func mapToDynamic(t *testing.T, m map[string]any, desc protoreflect.MessageDescriptor) *dynamicpb.Message {
	t.Helper()
	msg := dynamicpb.NewMessage(desc)
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal map: %v", err)
	}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(b, msg); err != nil {
		t.Fatalf("protojson unmarshal: %v", err)
	}
	return msg
}

// dynamicToMap converts a dynamic protobuf message to a Go map.
func dynamicToMap(t *testing.T, msg *dynamicpb.Message) map[string]any {
	t.Helper()
	b, err := protojson.Marshal(msg)
	if err != nil {
		t.Fatalf("protojson marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal map: %v", err)
	}
	return m
}

// startStreamServer is a helper that starts a gRPC server with the given
// streaming handlers on a free port, returning the address and a cleanup func.
func startStreamServer(t *testing.T, streams map[string]grpcsim.StreamHandler) (addr string, cleanup func()) {
	t.Helper()
	fds := compileDescriptorFromPath(t, "streaming.proto")

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr = lis.Addr().String()

	svc := &grpcsim.Service{
		FullName:   "stunt.test.Streamer",
		Descriptor: fds,
		Streams:    streams,
	}

	ctx, cancel := context.WithCancel(context.Background())
	srv, result, err := grpcsim.Serve(ctx, svc, lis)
	if err != nil {
		cancel()
		t.Fatalf("grpcsim.Serve: %v", err)
	}

	cleanup = func() {
		cancel()
		srv.GracefulStop()
		_ = result.Wait()
	}
	return addr, cleanup
}

// ---------------------------------------------------------------------------
// Server-streaming: client sends 1 request, receives N responses.
// ---------------------------------------------------------------------------

func TestServerStreaming(t *testing.T) {
	addr, cleanup := startStreamServer(t, map[string]grpcsim.StreamHandler{
		"StreamMany": func(ctx context.Context, fullMethod string, stream grpcsim.Stream) error {
			req, err := stream.Recv()
			if err != nil {
				return fmt.Errorf("recv: %w", err)
			}
			count := 0
			if c, ok := req["count"].(float64); ok {
				count = int(c)
			}
			prefix, _ := req["prefix"].(string)
			for i := 0; i < count; i++ {
				if err := stream.Send(map[string]any{
					"index": float64(i),
					"text":  fmt.Sprintf("%s-%d", prefix, i),
				}); err != nil {
					return err
				}
			}
			return nil
		},
	})
	defer cleanup()

	client, svcDesc := newStreamDynClient(t, "streaming.proto", addr)
	md := svcDesc.Methods().ByName("StreamMany")

	stream, err := client.conn.NewStream(context.Background(), &grpc.StreamDesc{
		StreamName:    "StreamMany",
		ServerStreams: true,
	}, "/stunt.test.Streamer/StreamMany")
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}

	req := mapToDynamic(t, map[string]any{"count": float64(5), "prefix": "item"}, md.Input())
	if err := stream.SendMsg(req); err != nil {
		t.Fatalf("SendMsg: %v", err)
	}
	// Server-streaming: client sends exactly 1 request, so close the send side.
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}

	var got []map[string]any
	for {
		resp := dynamicpb.NewMessage(md.Output())
		err := stream.RecvMsg(resp)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("RecvMsg: %v", err)
		}
		got = append(got, dynamicToMap(t, resp))
	}

	if len(got) != 5 {
		t.Fatalf("received %d responses, want 5", len(got))
	}
	// Spot-check the first and last items.
	if got[0]["text"] != "item-0" {
		t.Errorf("first text = %v, want %q", got[0]["text"], "item-0")
	}
	lastIdx, _ := got[4]["index"].(float64)
	if lastIdx != 4 {
		t.Errorf("last index = %v, want 4", lastIdx)
	}
}

// ---------------------------------------------------------------------------
// Client-streaming: client sends N requests then half-closes, receives 1.
// ---------------------------------------------------------------------------

func TestClientStreaming(t *testing.T) {
	addr, cleanup := startStreamServer(t, map[string]grpcsim.StreamHandler{
		"Accumulate": func(ctx context.Context, fullMethod string, stream grpcsim.Stream) error {
			var parts []string
			var n int
			for {
				req, err := stream.Recv()
				if err == io.EOF {
					break
				}
				if err != nil {
					return err
				}
				n++
				if d, ok := req["data"].(string); ok {
					parts = append(parts, d)
				}
			}
			return stream.Send(map[string]any{
				"data":  fmt.Sprintf("%v", parts),
				"count": float64(n),
			})
		},
	})
	defer cleanup()

	client, svcDesc := newStreamDynClient(t, "streaming.proto", addr)
	md := svcDesc.Methods().ByName("Accumulate")

	stream, err := client.conn.NewStream(context.Background(), &grpc.StreamDesc{
		StreamName:    "Accumulate",
		ClientStreams: true,
	}, "/stunt.test.Streamer/Accumulate")
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}

	want := []string{"a", "b", "c"}
	for _, s := range want {
		msg := mapToDynamic(t, map[string]any{"data": s}, md.Input())
		if err := stream.SendMsg(msg); err != nil {
			t.Fatalf("SendMsg: %v", err)
		}
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}

	// Client-streaming: exactly 1 response expected.
	resp := dynamicpb.NewMessage(md.Output())
	if err := stream.RecvMsg(resp); err != nil {
		t.Fatalf("RecvMsg: %v", err)
	}
	m := dynamicToMap(t, resp)

	count, _ := m["count"].(float64)
	if int(count) != len(want) {
		t.Errorf("count = %v, want %d", count, len(want))
	}
	data, _ := m["data"].(string)
	if data != fmt.Sprintf("%v", want) {
		t.Errorf("data = %q, want %q", data, fmt.Sprintf("%v", want))
	}

	// Verify the stream is fully closed (EOF on second RecvMsg).
	if err := stream.RecvMsg(resp); err != io.EOF {
		t.Errorf("expected io.EOF on second RecvMsg, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Bidi-streaming: client sends N, receives N (echo).
// ---------------------------------------------------------------------------

func TestBidiStreaming(t *testing.T) {
	addr, cleanup := startStreamServer(t, grpcStreamHandler())
	defer cleanup()

	client, svcDesc := newStreamDynClient(t, "streaming.proto", addr)
	md := svcDesc.Methods().ByName("EchoStream")

	stream, err := client.conn.NewStream(context.Background(), &grpc.StreamDesc{
		StreamName:    "EchoStream",
		ClientStreams: true,
		ServerStreams: true,
	}, "/stunt.test.Streamer/EchoStream")
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}

	const n = 4
	for i := 0; i < n; i++ {
		msg := mapToDynamic(t, map[string]any{
			"seq":  float64(i),
			"text": fmt.Sprintf("hello-%d", i),
		}, md.Input())
		if err := stream.SendMsg(msg); err != nil {
			t.Fatalf("SendMsg: %v", err)
		}
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}

	var got []map[string]any
	for {
		resp := dynamicpb.NewMessage(md.Output())
		err := stream.RecvMsg(resp)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("RecvMsg: %v", err)
		}
		got = append(got, dynamicToMap(t, resp))
	}

	if len(got) != n {
		t.Fatalf("received %d responses, want %d", len(got), n)
	}
	for i, m := range got {
		seq, _ := m["seq"].(float64)
		if int(seq) != i {
			t.Errorf("seq[%d] = %v, want %d", i, seq, i)
		}
		text, _ := m["text"].(string)
		want := fmt.Sprintf("hello-%d", i)
		if text != want {
			t.Errorf("text[%d] = %q, want %q", i, text, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Mixed unary + streaming on the same service (backward compatibility).
// ---------------------------------------------------------------------------

func TestMixedUnaryAndStreaming(t *testing.T) {
	fds := compileDescriptorFromPath(t, "mixed.proto")

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := lis.Addr().String()

	svc := &grpcsim.Service{
		FullName:   "stunt.test.Mixed",
		Descriptor: fds,
		Methods: map[string]grpcsim.Handler{
			"Ping": func(ctx context.Context, fullMethod string, req map[string]any) (map[string]any, error) {
				msg, _ := req["msg"].(string)
				return map[string]any{"msg": "pong: " + msg, "n": float64(1)}, nil
			},
		},
		Streams: map[string]grpcsim.StreamHandler{
			"StreamPings": func(ctx context.Context, fullMethod string, stream grpcsim.Stream) error {
				req, err := stream.Recv()
				if err != nil {
					return err
				}
				msg, _ := req["msg"].(string)
				for i := 0; i < 3; i++ {
					if err := stream.Send(map[string]any{
						"msg": fmt.Sprintf("pong: %s", msg),
						"n":   float64(i),
					}); err != nil {
						return err
					}
				}
				return nil
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

	// Build a dynamic client for the Mixed service.
	files, err := protodesc.NewFiles(fds)
	if err != nil {
		t.Fatalf("protodesc.NewFiles: %v", err)
	}
	svcDesc, err := files.FindDescriptorByName("stunt.test.Mixed")
	if err != nil {
		t.Fatalf("find Mixed: %v", err)
	}
	methods := svcDesc.(protoreflect.ServiceDescriptor).Methods()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	defer conn.Close()

	// --- Unary call ---
	pingMD := methods.ByName("Ping")
	reqProto := mapToDynamic(t, map[string]any{"msg": "hi"}, pingMD.Input())
	respProto := dynamicpb.NewMessage(pingMD.Output())
	if err := conn.Invoke(ctx, "/stunt.test.Mixed/Ping", reqProto, respProto); err != nil {
		t.Fatalf("unary invoke: %v", err)
	}
	respMap := dynamicToMap(t, respProto)
	if respMap["msg"] != "pong: hi" {
		t.Errorf("unary msg = %v, want %q", respMap["msg"], "pong: hi")
	}

	// --- Streaming call ---
	streamMD := methods.ByName("StreamPings")
	stream, err := conn.NewStream(ctx, &grpc.StreamDesc{
		StreamName:    "StreamPings",
		ServerStreams: true,
	}, "/stunt.test.Mixed/StreamPings")
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}
	streamReq := mapToDynamic(t, map[string]any{"msg": "loop"}, streamMD.Input())
	if err := stream.SendMsg(streamReq); err != nil {
		t.Fatalf("SendMsg: %v", err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}
	var got []map[string]any
	for {
		m := dynamicpb.NewMessage(streamMD.Output())
		if err := stream.RecvMsg(m); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("RecvMsg: %v", err)
		}
		got = append(got, dynamicToMap(t, m))
	}
	if len(got) != 3 {
		t.Fatalf("stream responses = %d, want 3", len(got))
	}
	if got[0]["msg"] != "pong: loop" {
		t.Errorf("stream msg[0] = %v, want %q", got[0]["msg"], "pong: loop")
	}
}

// ---------------------------------------------------------------------------
// Status propagation: handler returning a wrapped gRPC status error should
// propagate the ORIGINAL code, not codes.Internal.
// ---------------------------------------------------------------------------

// TestStreamPropagatesStatusError verifies that when a StreamHandler returns an
// error that wraps a real gRPC status (e.g. a transport error from
// stream.recv()), the original status code is propagated to the client instead
// of being masked as codes.Internal.
func TestStreamPropagatesStatusError(t *testing.T) {
	addr, cleanup := startStreamServer(t, map[string]grpcsim.StreamHandler{
		"EchoStream": func(ctx context.Context, fullMethod string, stream grpcsim.Stream) error {
			// Simulate a transport error wrapping a gRPC status, exactly as
			// would surface from stream.Recv() when the client cancels.
			return fmt.Errorf("handler error: %w", status.Error(codes.Canceled, "client cancelled"))
		},
	})
	defer cleanup()

	client, svcDesc := newStreamDynClient(t, "streaming.proto", addr)
	md := svcDesc.Methods().ByName("EchoStream")

	stream, err := client.conn.NewStream(context.Background(), &grpc.StreamDesc{
		StreamName:    "EchoStream",
		ClientStreams: true,
		ServerStreams: true,
	}, "/stunt.test.Streamer/EchoStream")
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}

	req := mapToDynamic(t, map[string]any{"seq": float64(1), "text": "hi"}, md.Input())
	if err := stream.SendMsg(req); err != nil {
		t.Fatalf("SendMsg: %v", err)
	}

	resp := dynamicpb.NewMessage(md.Output())
	err = stream.RecvMsg(resp)
	if err == nil {
		t.Fatal("expected gRPC error from handler, got nil")
	}

	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("error is not a grpc status: %v", err)
	}
	if st.Code() != codes.Canceled {
		t.Errorf("code = %v, want Canceled (should not be masked as Internal)", st.Code())
	}
}

// TestStreamClientCancelPropagatesCanceled verifies that a real client cancel
// mid-stream surfaces as codes.Canceled on the client side, not codes.Internal.
// The handler calls stream.Recv() in a loop; when the client cancels, Recv
// returns a transport error that propagates through the handler.
func TestStreamClientCancelPropagatesCanceled(t *testing.T) {
	addr, cleanup := startStreamServer(t, map[string]grpcsim.StreamHandler{
		"EchoStream": func(ctx context.Context, fullMethod string, stream grpcsim.Stream) error {
			for {
				_, err := stream.Recv()
				if err == io.EOF {
					return nil
				}
				if err != nil {
					return err // propagate transport error directly
				}
			}
		},
	})
	defer cleanup()

	client, svcDesc := newStreamDynClient(t, "streaming.proto", addr)
	md := svcDesc.Methods().ByName("EchoStream")

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := client.conn.NewStream(ctx, &grpc.StreamDesc{
		StreamName:    "EchoStream",
		ClientStreams: true,
		ServerStreams: true,
	}, "/stunt.test.Streamer/EchoStream")
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}

	// Send a message, then cancel mid-stream.
	req := mapToDynamic(t, map[string]any{"seq": float64(1), "text": "hi"}, md.Input())
	if err := stream.SendMsg(req); err != nil {
		t.Fatalf("SendMsg: %v", err)
	}

	cancel()

	// After cancellation, RecvMsg should return a gRPC error.
	resp := dynamicpb.NewMessage(md.Output())
	err = stream.RecvMsg(resp)
	if err == nil {
		t.Fatal("expected error after cancel, got nil")
	}

	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("error is not a grpc status: %v", err)
	}
	if st.Code() != codes.Canceled {
		t.Errorf("code = %v, want Canceled", st.Code())
	}
}

// grpcStreamHandler returns the EchoStream bidi handler that echoes each
// inbound message back to the client with its seq and text fields.
func grpcStreamHandler() map[string]grpcsim.StreamHandler {
	return map[string]grpcsim.StreamHandler{
		"EchoStream": func(ctx context.Context, fullMethod string, stream grpcsim.Stream) error {
			for {
				req, err := stream.Recv()
				if err == io.EOF {
					return nil
				}
				if err != nil {
					return err
				}
				if err := stream.Send(map[string]any{
					"seq":  req["seq"],
					"text": req["text"],
				}); err != nil {
					return err
				}
			}
		},
	}
}
