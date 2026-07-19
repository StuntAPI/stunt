// Package grpcsim provides dynamic gRPC serving from a protobuf
// FileDescriptorSet — no compiled Go stubs required.
//
// A [Service] pairs a fully-qualified service name and a descriptor set with a
// set of Go handler functions, each receiving the decoded request as a Go
// map[string]any and returning a response map (or a gRPC status error).
// [BuildServiceDesc] turns this into a [grpc.ServiceDesc] that the gRPC
// runtime can register, and [Serve] wires it onto a listener.
package grpcsim

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"sync/atomic"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// maxRecvMsgSize is the maximum size (4 MiB) of a single inbound gRPC
// message accepted by stunt's dynamic gRPC server. This is a deliberate,
// explicit bound rather than an implicit reliance on grpc-go's default.
const maxRecvMsgSize = 4 << 20 // 4 MiB

// Handler is a Go function that processes a single gRPC request. fullMethod is
// the gRPC method path (e.g. "/pkg.Svc/Method"). The request is decoded into
// req as a JSON-like Go map; the handler returns a response map or a non-nil
// error (which becomes a gRPC status — see [Error]).
type Handler func(ctx context.Context, fullMethod string, req map[string]any) (resp map[string]any, err error)

// Stream is the interface a streaming handler uses to interact with the
// client side of a streaming RPC. Recv reads the next client message into a
// Go map; it returns (nil, io.EOF) when the client has half-closed (no more
// messages). Send writes a response map to the client.
type Stream interface {
	// Context returns the stream's context (for cancellation, deadlines, etc).
	Context() context.Context

	// Recv reads the next inbound message. The message is decoded into a
	// JSON-like Go map using proto field names (snake_case). It returns
	// (nil, io.EOF) when the client has half-closed the stream, so callers
	// can treat EOF as a sentinel for "no more messages".
	Recv() (map[string]any, error)

	// Send writes an outbound message. The map is converted to a protobuf
	// message using the method's output type descriptor.
	Send(map[string]any) error
}

// StreamHandler is a Go function that processes a streaming gRPC RPC. It reads
// from and writes to stream as needed, and returns nil on success or a
// non-nil error (which becomes a gRPC status — see [Error]).
type StreamHandler func(ctx context.Context, fullMethod string, stream Stream) error

// Service describes a dynamically-served gRPC service. Methods is keyed by the
// bare method name (e.g. "CreateCharge").
type Service struct {
	// FullName is the fully-qualified protobuf service name, e.g.
	// "stunt.test.Greeter".
	FullName string

	// Descriptor is the FileDescriptorSet containing the service definition.
	Descriptor *descriptorpb.FileDescriptorSet

	// Methods maps a bare method name (e.g. "SayHello") to its [Handler].
	Methods map[string]Handler

	// Streams maps a bare method name (e.g. "StreamMany") to its
	// [StreamHandler]. A method appears in at most one of Methods or Streams.
	Streams map[string]StreamHandler
}

// codedError carries an explicit gRPC code alongside a message so that handlers
// can return structured errors that [BuildServiceDesc] maps to a gRPC status.
type codedError struct {
	code codes.Code
	msg  string
}

func (e *codedError) Error() string { return e.msg }

// GRPCStatus lets the grpc/status package extract the code directly when the
// error is surfaced to the client.
func (e *codedError) GRPCStatus() *status.Status {
	return status.New(e.code, e.msg)
}

// Error returns an error that maps to the given gRPC status code and message.
// Handlers should return Error(codes.NotFound, "not found") to produce a
// structured gRPC error.
func Error(code codes.Code, msg string) error {
	return &codedError{code: code, msg: msg}
}

// BuildServiceDesc constructs a dynamic [grpc.ServiceDesc] from svc. Each unary
// method declared in the descriptor is routed to the matching entry in
// svc.Methods (keyed by bare method name).
func BuildServiceDesc(svc *Service) (*grpc.ServiceDesc, error) {
	files, err := protodesc.NewFiles(svc.Descriptor)
	if err != nil {
		return nil, fmt.Errorf("grpcsim: parse descriptor set: %w", err)
	}

	svcDesc, err := files.FindDescriptorByName(protoreflect.FullName(svc.FullName))
	if err != nil {
		return nil, fmt.Errorf("grpcsim: find service %q: %w", svc.FullName, err)
	}
	service, ok := svcDesc.(protoreflect.ServiceDescriptor)
	if !ok {
		return nil, fmt.Errorf("grpcsim: %q is not a service", svc.FullName)
	}

	desc := &grpc.ServiceDesc{
		ServiceName: svc.FullName,
		// HandlerType is a placeholder used internally by grpc.Server for
		// type-checking when a non-nil impl is registered. We pass nil impl
		// in Serve so the type-check is skipped, but we still generate a
		// distinct reflect.Type per ServiceDesc via newUniqueHandlerType()
		// so that multi-service-per-server registration can never collide.
		HandlerType: newUniqueHandlerType(),
		Metadata:    svc.Descriptor,
	}

	methods := service.Methods()
	for i := 0; i < methods.Len(); i++ {
		md := methods.Get(i)
		methodKey := string(md.Name())
		fullMethod := "/" + svc.FullName + "/" + methodKey

		if md.IsStreamingClient() || md.IsStreamingServer() {
			// Streaming method — route to the Streams map.
			sh, ok := svc.Streams[methodKey]
			if !ok {
				continue
			}
			desc.Streams = append(desc.Streams, grpc.StreamDesc{
				StreamName:    methodKey,
				Handler:       streamDescHandler(fullMethod, md, sh),
				ServerStreams: md.IsStreamingServer(),
				ClientStreams: md.IsStreamingClient(),
			})
			continue
		}

		// Unary method — existing path.
		handler, ok := svc.Methods[methodKey]
		if !ok {
			// Skip methods with no Go handler registered.
			continue
		}

		desc.Methods = append(desc.Methods, grpc.MethodDesc{
			MethodName: methodKey,
			Handler:    methodHandler(fullMethod, md, handler),
		})
	}

	if len(desc.Methods) == 0 && len(desc.Streams) == 0 {
		return nil, fmt.Errorf("grpcsim: no handlers matched methods on service %q", svc.FullName)
	}

	return desc, nil
}

// handlerTypeCounter is a monotonically increasing counter used to generate
// a unique reflect.Type for each ServiceDesc's HandlerType.
var handlerTypeCounter atomic.Int64

// newUniqueHandlerType returns a value whose reflect.Type is distinct from
// every other call. It uses reflect.ArrayOf with an incrementing size to
// create a genuinely unique type (*[N]int) per invocation, so that two
// dynamic ServiceDescs registered on the same grpc.Server can never share a
// HandlerType even if multi-service-per-server is used in the future.
func newUniqueHandlerType() any {
	n := int(handlerTypeCounter.Add(1))
	return reflect.New(reflect.ArrayOf(n, reflect.TypeOf(int(0)))).Interface()
}

// methodHandler builds the grpc.MethodHandler closure for a single method. It
// decodes the incoming protobuf bytes into a dynamic message, converts that to
// a Go map, calls the user handler, then converts the returned map back into a
// protobuf message for the wire.
func methodHandler(fullMethod string, md protoreflect.MethodDescriptor, h Handler) grpc.MethodHandler {
	return func(_ any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
		reqMsg := dynamicpb.NewMessage(md.Input())
		if err := dec(reqMsg); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "grpcsim: decode request: %v", err)
		}

		reqMap, err := protoToMap(reqMsg)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "grpcsim: convert request: %v", err)
		}

		respMap, err := h(ctx, fullMethod, reqMap)
		if err != nil {
			return nil, toStatusError(err)
		}

		respMsg := dynamicpb.NewMessage(md.Output())
		if err := mapToProto(respMap, respMsg); err != nil {
			return nil, status.Errorf(codes.Internal, "grpcsim: convert response: %v", err)
		}
		return respMsg, nil
	}
}

// streamDescHandler builds the grpc.StreamHandler closure for a single
// streaming method. It adapts the grpc.ServerStream into our [Stream]
// interface, converting between protobuf messages and Go maps.
func streamDescHandler(fullMethod string, md protoreflect.MethodDescriptor, h StreamHandler) grpc.StreamHandler {
	return func(_ any, ss grpc.ServerStream) error {
		adapter := &serverStreamAdapter{ss: ss, md: md}
		return h(ss.Context(), fullMethod, adapter)
	}
}

// serverStreamAdapter adapts a grpc.ServerStream into our [Stream] interface.
// RecvMsg reads a dynamic message and converts it to a Go map; SendMsg
// converts a Go map to a dynamic message and sends it. io.EOF from RecvMsg
// (client half-close) is returned as (nil, io.EOF) so handlers can distinguish
// it from a real error.
type serverStreamAdapter struct {
	ss grpc.ServerStream
	md protoreflect.MethodDescriptor
}

func (a *serverStreamAdapter) Context() context.Context {
	return a.ss.Context()
}

func (a *serverStreamAdapter) Recv() (map[string]any, error) {
	msg := dynamicpb.NewMessage(a.md.Input())
	if err := a.ss.RecvMsg(msg); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, io.EOF
		}
		return nil, err
	}
	return protoToMap(msg)
}

func (a *serverStreamAdapter) Send(m map[string]any) error {
	msg := dynamicpb.NewMessage(a.md.Output())
	if err := mapToProto(m, msg); err != nil {
		return err
	}
	return a.ss.SendMsg(msg)
}

// toStatusError maps a handler error to a gRPC status error. If the error
// already carries a gRPC status (via status.Error or a GRPCStatus method), it
// is passed through unchanged.
func toStatusError(err error) error {
	if st, ok := status.FromError(err); ok && st.Code() != codes.OK {
		return st.Err()
	}
	return status.Error(codes.Internal, err.Error())
}

// protoToMap converts a protobuf message to a Go map by marshalling via
// protojson then unmarshalling the JSON into map[string]any. UseProtoNames
// is set so that keys are the proto field names (snake_case) rather than
// the default camelCase — handlers and documentation expect snake_case.
func protoToMap(msg *dynamicpb.Message) (map[string]any, error) {
	b, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(msg)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// mapToProto converts a Go map into a protobuf message by marshalling the map
// to JSON then unmarshalling via protojson into the dynamic message.
func mapToProto(m map[string]any, msg *dynamicpb.Message) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(b, msg)
}

// Serve creates a new grpc.Server, registers the dynamic service description
// derived from svc, and serves on lis. Serving happens in a goroutine; the
// returned server can be stopped via GracefulStop (the caller should also
// cancel ctx).
//
// The returned ServeResult provides a Wait method that blocks until serving
// stops and returns the serve error (if any). This lets callers detect a
// failed bind/accept promptly rather than silently holding a dead server.
func Serve(ctx context.Context, svc *Service, lis net.Listener) (*grpc.Server, *ServeResult, error) {
	desc, err := BuildServiceDesc(svc)
	if err != nil {
		return nil, nil, err
	}

	server := grpc.NewServer(
		// Security: set an explicit receive message-size limit rather than
		// relying on the implicit grpc-go default. This makes the bound a
		// deliberate decision and prevents a malicious adapter from pushing
		// arbitrarily large messages to exhaust memory (4 MiB).
		grpc.MaxRecvMsgSize(maxRecvMsgSize),
	)
	// RegisterService with a nil impl skips grpc's reflect-based HandlerType
	// type-check (which would panic on a non-interface placeholder). The
	// method-handler closures ignore srv entirely.
	server.RegisterService(desc, nil)

	// Channel to surface the first error from Serve (e.g. a bad listener).
	// Buffered so the goroutine never blocks if the caller ignores it.
	serveErr := make(chan error, 1)

	go func() {
		err := server.Serve(lis)
		select {
		case serveErr <- err:
		default:
		}
	}()

	go func() {
		<-ctx.Done()
		server.GracefulStop()
	}()

	return server, &ServeResult{errCh: serveErr}, nil
}

// ServeResult surfaces the asynchronous serve error from a [Serve] call.
// Callers can use Wait to block until serving stops and retrieve the error.
type ServeResult struct {
	errCh chan error
}

// Wait blocks until the server stops serving and returns the error reported
// by grpc.Server.Serve (typically nil on GracefulStop, or a listener error).
// It is safe to call Wait after GracefulStop has already completed.
func (r *ServeResult) Wait() error {
	return <-r.errCh
}
